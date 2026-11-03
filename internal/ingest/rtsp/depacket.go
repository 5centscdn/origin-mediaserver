package rtsp

import (
	"encoding/base64"
	"strings"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// videoTrack depacketizes an H.264 RTP stream into media.Packets, mirroring
// internal/webrtc's PublishSession.handleIncomingVideo — same
// samplebuilder + splitAnnexB + CodecInfo merge approach, since both are
// ingesting real-time H.264-over-RTP from an untrusted source with only
// in-band (or, here, SDP-signaled) parameter sets.
type videoTrack struct {
	bus *media.Bus
	sb  *samplebuilder.SampleBuilder
	clk clockUnwrapper

	sps, pps []byte

	// retimer turns RTP's single (presentation) timestamp into a valid
	// DTS/PTS pair; without it, B-frames left DTS going backwards. RTP has
	// no DTS, so it's always on. See media.H264Retimer.
	retimer *media.H264Retimer
}

// newVideoTrack creates a video depacketizer. spropParameterSets is the
// SDP a=fmtp "sprop-parameter-sets" value (base64 SPS,PPS comma-separated,
// RFC 6184) — pre-seeds CodecInfo before any RTP even arrives, since
// H.264-over-RTSP conventionally signals parameter sets this way rather
// than (or in addition to) sending them in-band.
func newVideoTrack(bus *media.Bus, spropParameterSets string) *videoTrack {
	t := &videoTrack{
		bus:     bus,
		sb:      samplebuilder.New(50, &codecs.H264Packet{}, 90000),
		retimer: media.NewH264Retimer(nil),
	}
	if spropParameterSets != "" {
		parts := strings.Split(spropParameterSets, ",")
		if len(parts) >= 1 {
			if sps, err := base64.StdEncoding.DecodeString(parts[0]); err == nil {
				t.sps = sps
			}
		}
		if len(parts) >= 2 {
			if pps, err := base64.StdEncoding.DecodeString(parts[1]); err == nil {
				t.pps = pps
			}
		}
		if t.sps != nil {
			t.retimer.SetSPS(t.sps)
			t.mergeCodecInfo()
		}
		if t.pps != nil {
			t.retimer.SetPPS(t.pps)
		}
	}
	return t
}

func (t *videoTrack) handleRTP(payload []byte) {
	var pkt rtp.Packet
	if err := pkt.Unmarshal(payload); err != nil {
		return
	}
	t.sb.Push(&pkt)

	for {
		sample := t.sb.Pop()
		if sample == nil {
			break
		}
		pts := t.clk.tick(sample.PacketTimestamp)

		for _, nal := range splitAnnexB(sample.Data) {
			if len(nal) == 0 {
				continue
			}
			switch nalType := nal[0] & 0x1F; nalType {
			case 7: // SPS
				t.sps = nal
				t.retimer.SetSPS(nal)
				t.mergeCodecInfo()
			case 8: // PPS
				t.pps = nal
				t.retimer.SetPPS(nal)
				t.mergeCodecInfo()
			case 1, 5: // non-IDR / IDR slice
				for _, p := range t.retimer.Push(&media.Packet{
					Type:       pktType(nalType == 5),
					Codec:      media.CodecH264,
					PTS:        pts, // RTP timestamp = presentation time
					IsKeyframe: nalType == 5,
					Data:       withStartCode(nal),
					WallClock:  time.Now(),
				}, nal) {
					t.bus.Publish(p)
				}
			}
		}
	}
}

func pktType(isKeyframe bool) media.PacketType {
	if isKeyframe {
		return media.PacketKeyframe
	}
	return media.PacketMedia
}

// mergeCodecInfo folds newly-seen SPS/PPS into the stream's CodecInfo,
// preserving whatever the audio side has already set — same merge
// discipline as internal/webrtc's PublishSession.mergeVideoCodecInfo and
// internal/ingest/mpegts's Demuxer.mergeVideoCodecInfo.
func (t *videoTrack) mergeCodecInfo() {
	if len(t.sps) == 0 {
		return
	}
	ci := t.bus.CodecInfo()
	merged := media.CodecInfo{}
	if ci != nil {
		merged = *ci
	}
	merged.VideoCodec = media.CodecH264
	merged.SPSNALu = t.sps
	merged.Width, merged.Height = media.ParseSPSResolution(t.sps)
	if len(t.sps) >= 4 {
		merged.Profile = t.sps[1]
		merged.ProfileCompatibility = t.sps[2]
		merged.Level = t.sps[3]
	}
	if len(t.pps) > 0 {
		merged.PPSNALu = t.pps
	}
	t.bus.SetCodecInfo(&merged)
}

// audioTrack handles G.711 (PCMU/PCMA) RTP audio: unlike H.264, a G.711
// RTP payload IS one complete, already-decodable audio frame — no
// reassembly across packets, no samplebuilder needed, just a
// timestamp-unwrap and a direct publish.
type audioTrack struct {
	bus       *media.Bus
	codec     media.CodecType
	clockRate uint32
	clk       clockUnwrapper
	infoSet   bool
}

func newAudioTrack(bus *media.Bus, codecName string, clockRate uint32) *audioTrack {
	codec := media.CodecPCMU
	if codecName == "PCMA" {
		codec = media.CodecPCMA
	}
	return &audioTrack{bus: bus, codec: codec, clockRate: clockRate}
}

func (t *audioTrack) handleRTP(payload []byte) {
	var pkt rtp.Packet
	if err := pkt.Unmarshal(payload); err != nil {
		return
	}
	if !t.infoSet {
		t.mergeCodecInfo()
		t.infoSet = true
	}

	// G.711's RTP clock rate is fixed at 8kHz; the pipeline's uniform tick
	// is 90kHz. Rescaling fresh from the cumulative source-clock tick
	// count (not by accumulating a pre-rounded per-packet delta) avoids
	// the exact class of rounding drift internal/mux.AudioClock was built
	// to eliminate for AAC — same principle, applied inline here since
	// G.711 has no fixed-samples-per-frame assumption to begin with (RTP
	// timestamp deltas already reflect the real payload size).
	srcTicks := t.clk.tick(pkt.Timestamp)
	dts := srcTicks * 90000 / int64(t.clockRate)

	t.bus.Publish(&media.Packet{
		Type:      media.PacketMedia,
		Codec:     t.codec,
		DTS:       dts,
		PTS:       dts,
		Data:      pkt.Payload,
		WallClock: time.Now(),
	})
}

func (t *audioTrack) mergeCodecInfo() {
	ci := t.bus.CodecInfo()
	merged := media.CodecInfo{}
	if ci != nil {
		merged = *ci
	}
	merged.AudioCodec = t.codec
	merged.SampleRate = int(t.clockRate)
	merged.Channels = 1 // G.711 is always mono per RFC 3551
	t.bus.SetCodecInfo(&merged)
}

// clockUnwrapper converts a wrapping 32-bit RTP timestamp (arbitrary
// starting offset, wraps every ~2^32 ticks per RFC 3550) into a monotonic
// int64 tick count in the source's own clock rate, starting at 0 — same
// approach as internal/webrtc's tsUnwrapper.
type clockUnwrapper struct {
	have    bool
	lastRTP uint32
	accum   int64
}

func (u *clockUnwrapper) tick(rtpTS uint32) int64 {
	if !u.have {
		u.have = true
		u.lastRTP = rtpTS
		return 0
	}
	delta := int32(rtpTS - u.lastRTP) // wraparound-safe signed delta
	u.accum += int64(delta)
	u.lastRTP = rtpTS
	return u.accum
}

// splitAnnexB splits a start-code-delimited Annex-B byte stream into
// individual NAL unit payloads (start codes stripped). Same algorithm as
// internal/webrtc's and internal/ingest/mpegts's splitAnnexB — duplicated
// rather than shared, consistent with how this codebase already handles
// this small helper across ingest paths.
func splitAnnexB(data []byte) [][]byte {
	var nals [][]byte
	start := -1
	i := 0
	for i+2 < len(data) {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			if start >= 0 {
				nals = append(nals, data[start:i])
			}
			i += 3
			start = i
			continue
		}
		if i+3 < len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			if start >= 0 {
				nals = append(nals, data[start:i])
			}
			i += 4
			start = i
			continue
		}
		i++
	}
	if start >= 0 && start < len(data) {
		nals = append(nals, data[start:])
	}
	return nals
}

// withStartCode re-attaches a 4-byte Annex-B start code to a bare NAL —
// this pipeline's media.Packet.Data convention for H.264 keeps the start
// code in Data itself (see internal/rtmp/connection.go's
// convertAVCCToAnnexB and internal/ingest/mpegts's identical helper).
func withStartCode(nal []byte) []byte {
	out := make([]byte, 0, len(nal)+4)
	out = append(out, 0, 0, 0, 1)
	out = append(out, nal...)
	return out
}
