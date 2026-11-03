package mp4file

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// testdata/clip.mp4 is 1 s of 160x96 x264 (bframes=2, keyint 15, 30 fps)
// plus mono 48 kHz AAC, made with
//
//	ffmpeg -f lavfi -i testsrc2=size=160x96:rate=30 -f lavfi -i sine=frequency=440:sample_rate=48000 \
//	  -t 1 -c:v libx264 -preset veryfast -crf 40 \
//	  -x264-params bframes=2:keyint=15:min-keyint=15:scenecut=0 -c:a aac -b:a 32k -ac 1 clip.mp4
//
// clip_faststart.mp4 is the same with -movflags +faststart (moov first) and
// clip_fragmented.mp4 with -movflags frag_keyframe+empty_moov.
//
// ffprobe reports 30 video packets with DTS starting at -1024 (an edit list
// shifting the 2-frame B-frame delay; timescale 15360) and PTS 0, 1536,
// 512, 1024, … and 48 AAC frames.

func open(t *testing.T, name string) *file {
	t.Helper()
	fh, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fh.Close() })
	st, _ := fh.Stat()
	f, err := parse(fh, st.Size())
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return f
}

func TestParse_MatchesFFprobe(t *testing.T) {
	for _, name := range []string{"clip.mp4", "clip_faststart.mp4"} {
		t.Run(name, func(t *testing.T) {
			f := open(t, name)
			if f.video == nil || f.audio == nil {
				t.Fatalf("tracks: video=%v audio=%v", f.video != nil, f.audio != nil)
			}
			if n := len(f.video.samples); n != 30 {
				t.Errorf("video samples = %d, want 30", n)
			}
			if n := len(f.audio.samples); n != 48 {
				t.Errorf("audio samples = %d, want 48", n)
			}
			v := f.video
			if v.timescale != 15360 || v.shift != 1024 {
				t.Errorf("timescale %d shift %d, want 15360/1024", v.timescale, v.shift)
			}
			// ffprobe's first packets: DTS -1024, -512, 0, 512 / PTS 0, 1536, 512, 1024.
			wantDTS := []int64{-1024, -512, 0, 512}
			wantPTS := []int64{0, 1536, 512, 1024}
			for i := range wantDTS {
				s := v.samples[i]
				if dts, pts := s.dts-v.shift, s.dts+int64(s.cto)-v.shift; dts != wantDTS[i] || pts != wantPTS[i] {
					t.Errorf("sample %d: dts %d pts %d, want %d %d", i, dts, pts, wantDTS[i], wantPTS[i])
				}
			}
			if !v.samples[0].key || !v.samples[15].key || v.samples[1].key {
				t.Errorf("sync samples wrong: 0=%v 1=%v 15=%v", v.samples[0].key, v.samples[1].key, v.samples[15].key)
			}
			ci := f.codecInfo()
			if ci.Width != 160 || ci.Height != 96 || ci.SampleRate != 48000 || ci.Channels != 1 || len(ci.SPSNALu) == 0 {
				t.Errorf("codec info %+v", ci)
			}
			// Each sample's bytes must be a sequence of whole NAL units.
			fh, _ := os.Open("testdata/" + name)
			defer fh.Close()
			for i, s := range v.samples {
				b := make([]byte, s.size)
				fh.ReadAt(b, s.off)
				ab := avccToAnnexB(b, v.lengthSize)
				if len(ab) != len(b)+(4-v.lengthSize)*strings.Count(string(ab), "\x00\x00\x00\x01") || ab[4]&0x1F == 0 {
					t.Fatalf("sample %d at %d isn't length-prefixed NALs", i, s.off)
				}
			}
		})
	}
}

func TestParse_Fragmented(t *testing.T) {
	fh, _ := os.Open("testdata/clip_fragmented.mp4")
	defer fh.Close()
	st, _ := fh.Stat()
	if _, err := parse(fh, st.Size()); err == nil || !strings.Contains(err.Error(), "fragmented") {
		t.Fatalf("err = %v, want a fragmented-MP4 error", err)
	}
}

// Play runs in real time across a loop boundary: timestamps keep increasing
// per track, DTS <= PTS, and frames arrive at roughly real-time pace.
func TestPlay_LoopsContinuously(t *testing.T) {
	bus := media.NewBus()
	sub := bus.Subscribe(context.Background())
	defer sub.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 1600*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Play(ctx, "testdata/clip.mp4", t.TempDir(), bus, nil) }()

	var pkts []*media.Packet
	start := time.Now()
collect:
	for {
		select {
		case p := <-sub.Chan():
			pkts = append(pkts, p)
		case err := <-done:
			if err != context.DeadlineExceeded {
				t.Fatalf("Play: %v", err)
			}
			break collect
		}
	}
	elapsed := time.Since(start)
	if ci := bus.CodecInfo(); ci == nil || ci.VideoCodec != media.CodecH264 || ci.AudioCodec != media.CodecAAC {
		t.Fatalf("CodecInfo not set: %+v", ci)
	}

	last := map[media.CodecType]int64{}
	var video int
	var lastVideoDTS int64
	for i, p := range pkts {
		if prev, ok := last[p.Codec]; ok && p.DTS <= prev {
			t.Fatalf("packet %d (%v): DTS %d not after %d", i, p.Codec, p.DTS, prev)
		}
		last[p.Codec] = p.DTS
		if p.DTS > p.PTS {
			t.Fatalf("packet %d: DTS %d > PTS %d", i, p.DTS, p.PTS)
		}
		if p.Codec == media.CodecH264 {
			if video == 0 && !p.IsKeyframe {
				t.Fatal("first video packet isn't a keyframe")
			}
			if video == 30 && !p.IsKeyframe {
				t.Error("first frame of the second loop isn't a keyframe")
			}
			if !strings.HasPrefix(string(p.Data), "\x00\x00\x00\x01") {
				t.Fatalf("video packet %d isn't Annex B", video)
			}
			video++
			lastVideoDTS = p.DTS
		}
	}
	if video <= 30 {
		t.Fatalf("got %d video frames in %v, want more than one loop (30)", video, elapsed)
	}
	// Real-time pacing: 1.6 s of wall clock covers ~1.6 s of media (plus
	// the frames due at t=0).
	if media := float64(lastVideoDTS-90000) / 90000; media > elapsed.Seconds()+0.1 {
		t.Errorf("played %.2f s of media in %.2f s: not paced", media, elapsed.Seconds())
	}
}

func TestLocalFile(t *testing.T) {
	for src, want := range map[string]string{
		"/srv/slate.mp4":        "/srv/slate.mp4",
		"file:///srv/slate.mp4": "/srv/slate.mp4",
	} {
		if got, err := localFile(context.Background(), src, t.TempDir(), nil); err != nil || got != want {
			t.Errorf("localFile(%q) = %q, %v", src, got, err)
		}
	}
	if _, err := localFile(context.Background(), "rtmp://x/y", t.TempDir(), nil); err == nil {
		t.Error("rtmp:// accepted")
	}
}
