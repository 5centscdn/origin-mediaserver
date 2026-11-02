package webrtc

import (
	"bytes"
	"context"
	"sync"
	"time"

	pionwebrtc "github.com/pion/webrtc/v4"
	pionmedia "github.com/pion/webrtc/v4/pkg/media"

	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
	"github.com/5centscdn/origin-mediaserver/internal/mux"
)

var h264StartCode = []byte{0x00, 0x00, 0x00, 0x01}

// Session bridges one live media.Stream to one WHEP viewer's PeerConnection.
type Session struct {
	id         string
	pc         *pionwebrtc.PeerConnection
	stream     *media.Stream
	videoTrack *pionwebrtc.TrackLocalStaticSample
	audioTrack *pionwebrtc.TrackLocalStaticSample // nil unless the source is Opus-sourced (see Manager.CreateSession)

	// sps/pps are captured once at session creation from the stream's
	// CodecInfo, exactly like every other packager in this codebase — a
	// mid-stream codec change is not supported.
	sps, pps []byte

	metrics   *metrics.Metrics
	cancel    context.CancelFunc
	closeOnce sync.Once
}

// close tears down this viewer session. Satisfies the rtcSession interface.
func (s *Session) close() {
	s.closeOnce.Do(func() {
		s.cancel()
		s.pc.Close()
		if s.metrics != nil {
			s.metrics.WHEPSessions.Dec()
		}
	})
}

// ticksToDuration converts a duration in 90kHz ticks (this pipeline's
// uniform timestamp unit) to a time.Duration, as required by pion's
// media.Sample.Duration field.
func ticksToDuration(ticks uint32) time.Duration {
	return time.Duration(ticks) * time.Second / 90000
}

// run subscribes to the stream's Bus and forwards video (and, once WHIP
// exists, Opus audio) packets to the browser as RTP via WriteSample. Mirrors
// hls.Packager.Run's subscribe/dispatch/cleanup shape.
func (s *Session) run(ctx context.Context, mgr *Manager) {
	sub := s.stream.Bus.Subscribe(ctx)
	defer sub.Close()

	var pendingVideo *mux.Sample

	for {
		select {
		case <-ctx.Done():
			return

		case pkt, ok := <-sub.Chan():
			if !ok {
				mgr.DeleteSession(s.id)
				return
			}
			if pkt.Type == media.PacketEnd {
				mgr.DeleteSession(s.id)
				return
			}

			switch {
			case pkt.Codec.IsVideo():
				if pkt.Type == media.PacketConfig {
					continue // SPS/PPS already captured from CodecInfo at session creation
				}
				var completed []mux.Sample
				completed, pendingVideo = mux.AppendPendingVideoSample(
					completed, pendingVideo, pkt.DTS, pkt.PTS, pkt.IsKeyframe, pkt.Data)
				for _, sample := range completed {
					s.writeVideoSample(sample)
				}

			case pkt.Codec.IsAudio() && s.audioTrack != nil:
				// Opus frames arrive whole — no reassembly needed, unlike video.
				if err := s.audioTrack.WriteSample(pionmedia.Sample{Data: pkt.Data, Duration: 20 * time.Millisecond}); err != nil {
					mgr.DeleteSession(s.id)
					return
				}
			}
		}
	}
}

// writeVideoSample prepends SPS/PPS before keyframes — exactly what
// mux.TSMuxer.WriteVideo already does for the TS output path — since
// Packet.Data for a keyframe contains only the IDR slice, never the
// parameter sets, and a WebRTC decoder needs them in-band to decode.
func (s *Session) writeVideoSample(sample mux.Sample) {
	data := sample.Data
	if sample.IsKey && (len(s.sps) > 0 || len(s.pps) > 0) {
		var buf bytes.Buffer
		if len(s.sps) > 0 {
			buf.Write(h264StartCode)
			buf.Write(s.sps)
		}
		if len(s.pps) > 0 {
			buf.Write(h264StartCode)
			buf.Write(s.pps)
		}
		buf.Write(data)
		data = buf.Bytes()
	}
	// Best-effort: WriteSample errors (e.g. a mid-write disconnect) are
	// harmless here — OnConnectionStateChange handles session teardown.
	_ = s.videoTrack.WriteSample(pionmedia.Sample{Data: data, Duration: ticksToDuration(sample.Duration)})
}
