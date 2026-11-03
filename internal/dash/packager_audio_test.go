package dash

import (
	"log/slog"
	"testing"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// A source that loses audio (a pull over the internet) must not pull the
// audio timeline behind the video: segment N's audio and video have to
// start at the same time, or players stall waiting for one of them. Before
// tsOrigin/audioPos the audio timeline was the sum of nominal frame
// durations, so every frame missing from the source moved all later audio
// segments earlier.
func TestPackager_AudioTimelineFollowsSourceAcrossGaps(t *testing.T) {
	cfg := &config.Config{}
	cfg.DASH.SegmentDurationS = 4
	stream := &media.Stream{Key: media.StreamKey{App: "test", Name: "s1"}, Bus: media.NewBus()}
	stream.Bus.SetCodecInfo(&media.CodecInfo{
		VideoCodec: media.CodecH264, Width: 640, Height: 360,
		SPSNALu: []byte{0x67, 0x42, 0x00, 0x1e, 0x00}, PPSNALu: []byte{0x68, 0xce},
		AudioCodec: media.CodecAAC, AACConfig: []byte{0x11, 0x90}, SampleRate: 48000, Channels: 2,
	})
	p := &Packager{cfg: cfg, stream: stream, outDir: t.TempDir(), log: slog.Default(), segSeq: 1}

	// t0 = 0 like an RTMP pull's first frame: that keyframe doesn't open a
	// segment (see tsOrigin), the next one 4 s later does.
	const t0 = 0
	const frame = 1920 // AAC at 48 kHz
	nextAudio := int64(t0)
	slice := []byte{0, 0, 0, 1, 0x41, 0xAA}
	// 25 fps video with a keyframe every 4 s for 20 s. Audio has a 1 s hole
	// at 9-10 s and a 0.5 s hole across the 12 s segment boundary, and
	// arrives 2 s ahead of the video it goes with (the kaumudy RTMP pull
	// does), so segments must be cut by timestamp, not arrival.
	const lead = 2 * 90000
	for f := 0; f <= 20*25; f++ {
		dts := int64(t0 + f*3600)
		for nextAudio <= dts+lead {
			if rel := nextAudio - t0; (rel < 9*90000 || rel >= 10*90000) && (rel < 11.8*90000 || rel >= 12.3*90000) {
				if err := p.handleAudio(&media.Packet{DTS: nextAudio, PTS: nextAudio, Codec: media.CodecAAC, Type: media.PacketMedia, Data: []byte{1}}); err != nil {
					t.Fatal(err)
				}
			}
			nextAudio += frame
		}
		if err := p.handleVideo(&media.Packet{DTS: dts, PTS: dts, Codec: media.CodecH264, IsKeyframe: f%100 == 0, Data: slice}); err != nil {
			t.Fatal(err)
		}
	}

	if len(p.videoTimeline) != 4 || len(p.audioTimeline) != 4 {
		t.Fatalf("timelines: %d video, %d audio segments, want 4 each", len(p.videoTimeline), len(p.audioTimeline))
	}
	for i, v := range p.videoTimeline {
		a := p.audioTimeline[i]
		late := int64(frame)
		if v.Number == 1 {
			late = lead + frame // audio before the first keyframe was dropped
		}
		if v.Number == 3 {
			late = 0.3*90000 + frame // its audio really starts after the 12-12.3 s hole
		}
		if d := a.StartTicks - v.StartTicks; d < -frame || d > late {
			t.Errorf("segment %d: audio starts at %d, video at %d (%.2f s apart)", v.Number, a.StartTicks, v.StartTicks, float64(d)/90000)
		}
		if aEnd, vEnd := a.StartTicks+a.DurationTicks, v.StartTicks+v.DurationTicks; aEnd-vEnd > frame || vEnd-aEnd > frame {
			t.Errorf("segment %d: audio ends at %d, video at %d (%.2f s apart)", v.Number, aEnd, vEnd, float64(aEnd-vEnd)/90000)
		}
		if i > 0 {
			prev := p.audioTimeline[i-1]
			if end := prev.StartTicks + prev.DurationTicks; end > a.StartTicks {
				t.Errorf("segment %d: audio overlaps the previous segment (%d > %d)", a.Number, end, a.StartTicks)
			}
		}
	}
}
