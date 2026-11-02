package webrtc

import (
	"context"
	"sync"
	"time"

	"github.com/pion/rtp/codecs"
	pionwebrtc "github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"

	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
)

// PublishSession bridges one WHIP publisher's PeerConnection to a new
// media.Stream, depacketizing incoming RTP into media.Packets on its Bus.
type PublishSession struct {
	id       string
	pc       *pionwebrtc.PeerConnection
	stream   *media.Stream
	registry *media.Registry
	metrics  *metrics.Metrics
	mgr      *Manager
	cancel   context.CancelFunc

	closeOnce sync.Once
}

// close satisfies the rtcSession interface (called by Manager.DeleteSession).
// Unregistering is what actually tears everything down — Registry.Unregister
// closes the Stream's Bus and invokes StopFunc (closePeerConnection, set in
// Manager.CreatePublishSession), which is the single place that closes the
// PeerConnection, decrements metrics, and removes this session from
// Manager.sessions. This indirection (rather than doing that here directly)
// lets an operator's "kick stream" API call — which calls Registry.Unregister
// directly, bypassing this package entirely — tear down the PeerConnection
// too, since it goes through the same StopFunc.
func (s *PublishSession) close() {
	s.registry.Unregister(s.stream)
}

// closePeerConnection is assigned to stream.StopFunc — the single place that
// actually closes the PeerConnection, guarded against every entry path
// (explicit WHIP DELETE, ICE disconnect, or an external kick-stream call)
// running it more than once.
func (s *PublishSession) closePeerConnection() {
	s.closeOnce.Do(func() {
		s.cancel()
		s.pc.Close()
		if s.metrics != nil {
			s.metrics.WHIPSessions.Dec()
		}
		s.mgr.mu.Lock()
		delete(s.mgr.sessions, s.id)
		s.mgr.mu.Unlock()
	})
}

// handleIncomingTrack depacketizes one remote RTP track (video or audio)
// into media.Packets published on the stream's Bus. Runs until ReadRTP
// errors (remote track ended / PeerConnection closed), at which point it
// tears down the whole session — a publisher losing either track ends the
// stream, mirroring how an RTMP disconnect ends a stream regardless of which
// direction failed.
func (s *PublishSession) handleIncomingTrack(track *pionwebrtc.TrackRemote) {
	switch track.Kind() {
	case pionwebrtc.RTPCodecTypeVideo:
		s.handleIncomingVideo(track)
	case pionwebrtc.RTPCodecTypeAudio:
		s.handleIncomingAudio(track)
	}
	s.close()
}

func (s *PublishSession) handleIncomingVideo(track *pionwebrtc.TrackRemote) {
	sb := samplebuilder.New(50, &codecs.H264Packet{}, 90000)
	var ts tsUnwrapper

	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		s.stream.Bus.AddBytesIn(int64(len(pkt.Payload)))
		sb.Push(pkt)

		for {
			sample := sb.Pop()
			if sample == nil {
				break
			}
			dts := ts.tick(sample.PacketTimestamp)

			// SEI (type 6) — e.g. CEA-608/708 closed captions carried as
			// ITU-T T.35 registered user data — precedes the slice NAL of
			// the same access unit in decode order; carry it into that
			// slice's own Packet.Data rather than dropping it (see
			// internal/ingest/mpegts/demux.go's flushVideo, which has the
			// identical pattern and a fuller explanation).
			var pendingSEI []byte
			for _, nal := range splitAnnexB(sample.Data) {
				if len(nal) == 0 {
					continue
				}
				switch nalType := nal[0] & 0x1F; nalType {
				case 7: // SPS
					s.mergeVideoCodecInfo(nal, nil)
				case 8: // PPS
					s.mergeVideoCodecInfo(nil, nal)
				case 6: // SEI
					pendingSEI = append(pendingSEI, nal...)
				case 1, 5: // non-IDR / IDR slice
					sliceData := nal
					if len(pendingSEI) > 0 {
						sliceData = append(pendingSEI, nal...)
						pendingSEI = nil
					}
					s.stream.Bus.Publish(&media.Packet{
						Type:       media.PacketMedia,
						Codec:      media.CodecH264,
						DTS:        dts,
						PTS:        dts,
						IsKeyframe: nalType == 5,
						Data:       sliceData,
						WallClock:  time.Now(),
					})
				}
			}
		}
	}
}

func (s *PublishSession) handleIncomingAudio(track *pionwebrtc.TrackRemote) {
	sb := samplebuilder.New(50, &codecs.OpusPacket{}, 48000)
	var ts tsUnwrapper
	var infoSet bool

	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		s.stream.Bus.AddBytesIn(int64(len(pkt.Payload)))
		sb.Push(pkt)

		for {
			sample := sb.Pop()
			if sample == nil {
				break
			}
			if !infoSet {
				s.mergeAudioCodecInfo()
				infoSet = true
			}
			// Opus RTP clock rate is fixed at 48kHz; the pipeline's uniform
			// tick is 90kHz — rescale.
			dts := ts.tick(sample.PacketTimestamp) * 90000 / 48000
			s.stream.Bus.Publish(&media.Packet{
				Type:      media.PacketMedia,
				Codec:     media.CodecOpus,
				DTS:       dts,
				PTS:       dts,
				Data:      sample.Data,
				WallClock: time.Now(),
			})
		}
	}
}

// mergeVideoCodecInfo folds newly-seen SPS/PPS into the stream's CodecInfo,
// preserving whatever the audio track has already set — the same merge
// discipline that fixed a real bug in RTMP's decodeAudioTag (that merge
// branch set AACConfig but forgot AudioCodec, leaving codec detection
// broken); this must set every field the video branch owns, every time.
func (s *PublishSession) mergeVideoCodecInfo(sps, pps []byte) {
	ci := s.stream.Bus.CodecInfo()
	merged := media.CodecInfo{}
	if ci != nil {
		merged = *ci
	}
	merged.VideoCodec = media.CodecH264
	if sps != nil {
		merged.SPSNALu = sps
		merged.Width, merged.Height = media.ParseSPSResolution(sps)
		if len(sps) >= 4 {
			merged.Profile = sps[1]
			merged.ProfileCompatibility = sps[2]
			merged.Level = sps[3]
		}
	}
	if pps != nil {
		merged.PPSNALu = pps
	}
	s.stream.Bus.SetCodecInfo(&merged)
}

// mergeAudioCodecInfo folds Opus's fixed parameters into CodecInfo,
// preserving whatever the video track has already set.
func (s *PublishSession) mergeAudioCodecInfo() {
	ci := s.stream.Bus.CodecInfo()
	merged := media.CodecInfo{}
	if ci != nil {
		merged = *ci
	}
	merged.AudioCodec = media.CodecOpus
	merged.SampleRate = 48000
	merged.Channels = 2
	s.stream.Bus.SetCodecInfo(&merged)
}

// splitAnnexB splits a start-code-delimited byte stream into individual NAL
// unit payloads (start codes stripped) — a reassembled RTP sample can
// contain more than one NAL (e.g. SPS+PPS+IDR concatenated for a keyframe,
// common from WebRTC senders).
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

// tsUnwrapper converts a track's raw 32-bit RTP timestamps (arbitrary
// starting offset, wraps every ~2^32 ticks per RFC 3550) into a monotonic
// int64 tick count starting at 0 — WHEP never needed this (it only emits RTP
// from already-normalized pipeline ticks), but WHIP must interpret incoming
// ones.
type tsUnwrapper struct {
	have    bool
	lastRTP uint32
	accum   int64
}

func (u *tsUnwrapper) tick(rtpTS uint32) int64 {
	if !u.have {
		u.have = true
		u.lastRTP = rtpTS
		u.accum = 0
		return 0
	}
	delta := int32(rtpTS - u.lastRTP) // wraparound-safe signed delta
	u.accum += int64(delta)
	u.lastRTP = rtpTS
	return u.accum
}
