package icecast

import (
	"context"
	"io"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/mux"
)

// source incrementally parses a continuous, unframed audio byte stream
// (Icecast carries no packet/PES boundaries or timestamps at all — just
// back-to-back codec frames) into media.Packets. codec is fixed for the
// lifetime of one connection, decided once at handshake time from the
// Content-Type header.
type source struct {
	bus     *media.Bus
	codec   media.CodecType // CodecMP3 or CodecAAC
	buf     []byte
	clock   *mux.AudioClock
	infoSet bool
	pts     int64 // synthesized — Icecast has no real timestamps to resync to
}

func newSource(bus *media.Bus, codec media.CodecType) *source {
	// Start at 1, not 0: internal/hls.Packager's segStartDTS uses <= 0 as
	// its "segment not open yet" sentinel (a pre-existing, documented
	// limitation shared with the video path — see project notes on
	// segStartDTS) — a literal 0 here would collide with it on every
	// single stream, not just as a rare edge case, since this is the
	// synthesized PTS's own starting value. The absolute epoch is
	// otherwise meaningless for a live stream, so this costs nothing.
	return &source{bus: bus, codec: codec, pts: 1}
}

// run reads from r until EOF, ctx is done, or a fatal read error occurs —
// mirrors the blocking, run-until-the-connection-ends contract used by
// every other ingest path (rtmp.Pull, mpegts.Demuxer.Run, rtsp.Pull).
func (s *source) run(ctx context.Context, r io.Reader) error {
	buf := make([]byte, 8192)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := r.Read(buf)
		if n > 0 {
			s.bus.AddBytesIn(int64(n))
			s.feed(buf[:n])
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func (s *source) feed(data []byte) {
	s.buf = append(s.buf, data...)
	for {
		var progressed bool
		switch s.codec {
		case media.CodecMP3:
			progressed = s.consumeMP3()
		case media.CodecAAC:
			progressed = s.consumeADTS()
		}
		if !progressed {
			return
		}
	}
}

// consumeMP3 extracts and publishes one MP3 frame from the front of s.buf
// if a complete one is buffered. Returns false when it made no progress —
// either genuinely out of data (caller should wait for more) or after
// resyncing past one bad byte (caller should retry, which the for loop in
// feed already does by calling this again — false here just means "check
// buffer state again before looping," not "give up").
func (s *source) consumeMP3() bool {
	info, ok := parseMP3Header(s.buf)
	if !ok {
		if len(s.buf) > 4 {
			s.buf = s.buf[1:] // resync: drop one byte and retry
			return true
		}
		return false
	}
	if len(s.buf) < info.length {
		return false // full frame not buffered yet
	}
	frame := s.buf[:info.length]
	s.buf = s.buf[info.length:]

	if s.clock == nil {
		s.clock = mux.NewAudioClock(info.sampleRate, info.samplesPerFrame)
	}
	if !s.infoSet {
		s.mergeCodecInfoMP3(info)
		s.infoSet = true
	}
	s.publish(frame)
	return true
}

// consumeADTS is consumeMP3's AAC counterpart.
func (s *source) consumeADTS() bool {
	info, ok := parseADTSHeader(s.buf)
	if !ok {
		if len(s.buf) > 7 {
			s.buf = s.buf[1:]
			return true
		}
		return false
	}
	if len(s.buf) < info.length {
		return false
	}
	// Raw AAC convention: ADTS header stripped, matching
	// internal/ingest/mpegts's adtsFrame.data and the FLV/RTMP AAC payload
	// shape every downstream consumer already expects.
	frame := s.buf[info.headerLen:info.length]
	s.buf = s.buf[info.length:]

	if !s.infoSet {
		s.mergeCodecInfoAAC(info)
		s.infoSet = true
	}
	if s.clock == nil {
		sampleRate := mux.ParseAACSampleRate(buildAudioSpecificConfig(info.audioObjectType, info.sfIndex, info.channelConfig))
		if sampleRate == 0 {
			sampleRate = 44100
		}
		s.clock = mux.NewAudioClock(sampleRate, 1024)
	}
	s.publish(frame)
	return true
}

// publish emits one frame with a synthesized, monotonically-advancing PTS.
// There is no real timestamp anywhere in an Icecast stream to resync
// against — mux.AudioClock's exact-rounding accumulator alone (fed back
// its own running total, never an independent "real" PTS) is the entire
// timestamp story here, unlike the MPEG-TS/SRT ingest case where a real
// per-PES PTS exists to periodically resync to.
func (s *source) publish(data []byte) {
	duration := s.clock.NextDuration(s.pts)
	s.bus.Publish(&media.Packet{
		Type:      media.PacketMedia,
		Codec:     s.codec,
		DTS:       s.pts,
		PTS:       s.pts,
		Data:      data,
		WallClock: time.Now(),
	})
	s.pts += int64(duration)
}

// mergeCodecInfoMP3/mergeCodecInfoAAC fold codec parameters into
// CodecInfo — same merge discipline as every other ingest path's
// mergeAudioCodecInfo (preserve whatever's already set; there's never a
// video side to preserve for Icecast specifically, but writing it the same
// way keeps the pattern consistent).
func (s *source) mergeCodecInfoMP3(info mp3FrameInfo) {
	ci := s.bus.CodecInfo()
	merged := media.CodecInfo{}
	if ci != nil {
		merged = *ci
	}
	merged.AudioCodec = media.CodecMP3
	merged.SampleRate = info.sampleRate
	merged.Channels = info.channels
	s.bus.SetCodecInfo(&merged)
}

func (s *source) mergeCodecInfoAAC(info adtsFrameInfo) {
	ci := s.bus.CodecInfo()
	merged := media.CodecInfo{}
	if ci != nil {
		merged = *ci
	}
	merged.AudioCodec = media.CodecAAC
	merged.AACConfig = buildAudioSpecificConfig(info.audioObjectType, info.sfIndex, info.channelConfig)
	s.bus.SetCodecInfo(&merged)
}
