package rtmp

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// RTMP playback: rtmp://host/{app}/{stream} played by ffplay, VLC, OBS
// media sources or another origin's RTMP pull. Any live stream can be
// played, whatever protocol it was ingested over: the player subscribes to
// the stream's Bus and gets it back as FLV tags.
//
// Scope:
//   - video: H.264 (AVC sequence header from the stream's SPS/PPS, then
//     NALUs re-framed from Annex-B to 4-byte length prefixes). HEVC would
//     need Enhanced RTMP and isn't sent; such a stream plays audio only.
//   - audio: AAC (sequence header from AudioSpecificConfig), MP3, and
//     G.711 µ-law/A-law (FLV sound formats 8/7, e.g. RTSP cameras).
//   - starts at a keyframe; audio before it is skipped so A/V start
//     together. An audio-only stream (Icecast) starts at its first frame.
//   - timestamps: milliseconds from the first sent packet, on the
//     stream's own DTS clock.
//
// Playback is public, like HLS/DASH and raw MPEG-TS from this origin.
//
// Chunk streams: 4 audio, 5 data, 6 video (2 control and 3 commands are
// shared with the command path; conn.wmu serializes all writes).
const (
	csidAudio = 4
	csidData  = 5
	csidVideo = 6

	// playWriteTimeout drops a viewer whose connection stops draining.
	playWriteTimeout = 10 * time.Second
)

// handlePlay starts playback of app/vals[3] on msid.
func (c *conn) handlePlay(msid uint32, vals []AMF0Value) error {
	if len(vals) < 4 {
		return nil
	}
	nameVal, _ := vals[3].(AMF0String)
	name := stripQuery(string(nameVal))
	key := media.StreamKey{App: c.appName, Name: name}
	if c.routing && !c.routed && c.registry.Get(key) == nil {
		key = c.routePlayKey(key)
	}

	ms := c.msgStreams[msid]
	if ms == nil {
		ms = &msgStreamState{msid: msid}
		c.msgStreams[msid] = ms
	}
	c.stopPlay(ms)

	// User control: StreamBegin.
	ev := make([]byte, 6)
	binary.BigEndian.PutUint16(ev[0:2], 0)
	binary.BigEndian.PutUint32(ev[2:6], msid)
	if err := c.sendControlMessage(msgUserControl, ev); err != nil {
		return err
	}

	st := c.registry.Get(key)
	if st == nil {
		c.log.Info("rtmp play: stream not found", "stream", key)
		return c.sendStatusMessage(msid, "error", "NetStream.Play.StreamNotFound", "Stream not found: "+key.String())
	}
	if err := c.sendStatusMessage(msid, "status", "NetStream.Play.Reset", "Playing and resetting "+key.String()); err != nil {
		return err
	}
	if err := c.sendStatusMessage(msid, "status", "NetStream.Play.Start", "Started playing "+key.String()); err != nil {
		return err
	}
	if payload, err := EncodeAMF0(AMF0String("|RtmpSampleAccess"), AMF0Boolean(true), AMF0Boolean(true)); err == nil {
		if err := c.writeChunk(csidData, 0, msgDataAMF0, msid, payload); err != nil {
			return err
		}
	}

	ctx, cancel := context.WithCancel(c.ctx)
	ms.name = name
	ms.playCancel = cancel
	st.Viewers.Touch("rtmp:" + c.nc.RemoteAddr().String())
	c.log.Info("rtmp play started", "stream", key)
	go c.playLoop(ctx, msid, st)
	return nil
}

// routePlayKey maps a play of "{app}-{stream}" on the routing app onto
// the live stream it names, trying the longest app prefix first. Unlike
// routeTarget it looks at live streams, not applications, so pull-only
// streams (no publish application) play this way too. key is returned
// unchanged when nothing matches.
func (c *conn) routePlayKey(key media.StreamKey) media.StreamKey {
	sep, name := c.cfg.RTMP.RouteSeparator, key.Name
	for i := strings.LastIndex(name, sep); i > 0; i = strings.LastIndex(name[:i], sep) {
		k := media.StreamKey{App: name[:i], Name: name[i+len(sep):]}
		if k.Name != "" && c.registry.Get(k) != nil {
			return k
		}
	}
	return key
}

// stopPlay ends ms's playback, if any.
func (c *conn) stopPlay(ms *msgStreamState) {
	if ms != nil && ms.playCancel != nil {
		ms.playCancel()
		ms.playCancel = nil
	}
}

// playLoop sends st's media to the player until ctx ends, the stream
// stops, or a write fails (the connection is then closed).
func (c *conn) playLoop(ctx context.Context, msid uint32, st *media.Stream) {
	sub := st.Bus.Subscribe(ctx)
	defer sub.Close()

	var (
		started      bool
		base         int64
		lastVideoCfg []byte
		lastAudioCfg []byte
		warnedHEVC   bool
	)
	write := func(csid uint32, typeID uint8, ts int64, data []byte) bool {
		_ = c.nc.SetWriteDeadline(time.Now().Add(playWriteTimeout))
		if err := c.writeChunk(csid, uint32(ts), typeID, msid, data); err != nil {
			c.log.Info("rtmp play ended", "stream", st.Key, "err", err)
			c.nc.Close()
			return false
		}
		return true
	}
	// sendConfig (re)sends sequence headers when the stream's codec config
	// is new or has changed.
	sendConfig := func(ts int64) bool {
		ci := st.Bus.CodecInfo()
		if ci == nil {
			return true
		}
		if ci.VideoCodec == media.CodecH264 && len(ci.SPSNALu) >= 4 && len(ci.PPSNALu) > 0 {
			cfg := avcDecoderConfig(ci.SPSNALu, ci.PPSNALu)
			if !bytes.Equal(cfg, lastVideoCfg) {
				if !write(csidVideo, msgVideo, ts, append([]byte{0x17, 0, 0, 0, 0}, cfg...)) {
					return false
				}
				lastVideoCfg = cfg
			}
		}
		if ci.AudioCodec == media.CodecAAC && len(ci.AACConfig) > 0 && !bytes.Equal(ci.AACConfig, lastAudioCfg) {
			if !write(csidAudio, msgAudio, ts, append([]byte{0xAF, 0}, ci.AACConfig...)) {
				return false
			}
			lastAudioCfg = append([]byte(nil), ci.AACConfig...)
		}
		return true
	}
	begin := func(dts int64) bool {
		started, base = true, dts
		if payload, err := EncodeAMF0(AMF0String("onMetaData"), metaData(st.Bus.CodecInfo())); err == nil {
			if !write(csidData, msgDataAMF0, 0, payload) {
				return false
			}
		}
		return sendConfig(0)
	}

	for {
		var pkt *media.Packet
		var ok bool
		select {
		case <-ctx.Done():
			return
		case pkt, ok = <-sub.Chan():
		}
		if !ok {
			c.sendStatusMessage(msid, "status", "NetStream.Play.UnpublishNotify", st.Key.String()+" is now unpublished")
			c.sendStatusMessage(msid, "status", "NetStream.Play.Stop", "Stopped playing "+st.Key.String())
			return
		}
		if pkt.Type == media.PacketConfig {
			continue
		}

		switch {
		case pkt.Codec == media.CodecH264:
			if !started {
				if !pkt.IsKeyframe {
					continue
				}
				if !begin(pkt.DTS) {
					return
				}
			}
			ts := pkt.DTS - base
			if ts < 0 {
				continue
			}
			ts /= 90
			if pkt.IsKeyframe && !sendConfig(ts) {
				return
			}
			nalus := annexBToAVCC(pkt.Data)
			if len(nalus) == 0 {
				continue
			}
			cts := (pkt.PTS - pkt.DTS) / 90
			hdr := []byte{0x27, 1, byte(cts >> 16), byte(cts >> 8), byte(cts)}
			if pkt.IsKeyframe {
				hdr[0] = 0x17
			}
			if !write(csidVideo, msgVideo, ts, append(hdr, nalus...)) {
				return
			}

		case pkt.Codec.IsVideo():
			if !warnedHEVC {
				c.log.Info("rtmp play: video codec not sent over RTMP, audio only", "stream", st.Key, "codec", pkt.Codec)
				warnedHEVC = true
			}

		case pkt.Codec.IsAudio():
			var hdr []byte
			switch pkt.Codec {
			case media.CodecAAC:
				hdr = []byte{0xAF, 1}
			case media.CodecMP3:
				hdr = []byte{0x2F}
			case media.CodecPCMU:
				hdr = []byte{0x82}
			case media.CodecPCMA:
				hdr = []byte{0x72}
			default:
				continue
			}
			if !started {
				// Wait for a video keyframe, unless the stream has no
				// H.264 to wait for.
				if ci := st.Bus.CodecInfo(); ci != nil && ci.VideoCodec == media.CodecH264 && len(ci.SPSNALu) > 0 {
					continue
				}
				if !begin(pkt.DTS) {
					return
				}
			}
			ts := pkt.DTS - base
			if ts < 0 {
				continue
			}
			if !write(csidAudio, msgAudio, ts/90, append(hdr, pkt.Data...)) {
				return
			}
		}
	}
}

// metaData is the onMetaData object for ci.
func metaData(ci *media.CodecInfo) AMF0Object {
	m := AMF0Object{"server": AMF0String("origin-mediaserver")}
	if ci == nil {
		return m
	}
	if ci.VideoCodec == media.CodecH264 {
		m["videocodecid"] = AMF0Number(7)
		if ci.Width > 0 {
			m["width"] = AMF0Number(ci.Width)
			m["height"] = AMF0Number(ci.Height)
		}
		if ci.FrameRate > 0 {
			m["framerate"] = AMF0Number(ci.FrameRate)
		}
	}
	switch ci.AudioCodec {
	case media.CodecAAC:
		m["audiocodecid"] = AMF0Number(10)
	case media.CodecMP3:
		m["audiocodecid"] = AMF0Number(2)
	case media.CodecPCMA:
		m["audiocodecid"] = AMF0Number(7)
	case media.CodecPCMU:
		m["audiocodecid"] = AMF0Number(8)
	}
	return m
}

// avcDecoderConfig builds an AVCDecoderConfigurationRecord (4-byte NAL
// lengths) from one SPS and PPS.
func avcDecoderConfig(sps, pps []byte) []byte {
	b := []byte{1, sps[1], sps[2], sps[3], 0xFF, 0xE1, byte(len(sps) >> 8), byte(len(sps))}
	b = append(b, sps...)
	b = append(b, 1, byte(len(pps)>>8), byte(len(pps)))
	return append(b, pps...)
}

// annexBToAVCC re-frames an Annex-B access unit as 4-byte length-prefixed
// NALUs, leaving out SPS/PPS (sent in the sequence header) and access unit
// delimiters.
func annexBToAVCC(data []byte) []byte {
	var out []byte
	for _, nal := range splitAnnexB(data) {
		if len(nal) == 0 {
			continue
		}
		switch nal[0] & 0x1F {
		case 7, 8, 9:
			continue
		}
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(nal)))
		out = append(out, l[:]...)
		out = append(out, nal...)
	}
	return out
}

// splitAnnexB splits data at 3- and 4-byte start codes. Data with no
// start code is returned as one NAL (some ingest paths pass bare NALUs).
func splitAnnexB(data []byte) [][]byte {
	var nals [][]byte
	start := -1
	i := 0
	for i+3 <= len(data) {
		if data[i] == 0 && data[i+1] == 0 && (data[i+2] == 1 || (i+4 <= len(data) && data[i+2] == 0 && data[i+3] == 1)) {
			sc := 3
			if data[i+2] == 0 {
				sc = 4
			}
			if start >= 0 {
				nals = append(nals, trimTrailingZeros(data[start:i]))
			}
			start = i + sc
			i += sc
			continue
		}
		i++
	}
	if start >= 0 {
		nals = append(nals, data[start:])
	} else if len(data) > 0 {
		nals = append(nals, data)
	}
	return nals
}

func trimTrailingZeros(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
}
