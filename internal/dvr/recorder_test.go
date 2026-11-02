package dvr

import (
	"context"
	"encoding/xml"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/media"
)

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	database, err := db.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	cfg := &config.Config{}
	cfg.DVR.StoragePath = t.TempDir()
	cfg.DVR.WindowSeconds = 3600
	cfg.HLS.SegmentDurationS = 2
	return NewEngine(cfg, database, nil)
}

// record runs one recorder over a fresh *media.Stream for key, publishing
// seconds of 30 fps H.264 (keyframe every second) interleaved with 48 kHz
// AAC, starting at baseDTS, and returns once everything is indexed.
func record(t *testing.T, e *Engine, key media.StreamKey, baseDTS int64, seconds int) {
	t.Helper()
	stream := &media.Stream{Key: key, Bus: media.NewBus()}
	stream.Bus.SetCodecInfo(&media.CodecInfo{
		VideoCodec: media.CodecH264,
		AudioCodec: media.CodecAAC,
		Width:      1280, Height: 720,
		Profile: 0x64, Level: 0x1f,
		SPSNALu:   []byte{0x67, 0x64, 0x00, 0x1f, 0xac},
		PPSNALu:   []byte{0x68, 0xee, 0x3c, 0x80},
		AACConfig: []byte{0x11, 0x90}, // AAC-LC, 48 kHz, stereo
	})

	ctx, cancel := context.WithCancel(context.Background())
	e.StartRecording(ctx, stream, Options{})
	e.mu.Lock()
	done := e.writers[key]
	e.mu.Unlock()
	for stream.Bus.SubscriberCount() == 0 {
		time.Sleep(time.Millisecond)
	}

	const frame, aframe = 3000, 1920 // 90 kHz ticks: 30 fps video, 1024/48000 audio
	end := baseDTS + int64(seconds)*90000
	v, a, n := baseDTS, baseDTS, 0
	for v < end || a < end {
		if v <= a && v < end {
			key := (v-baseDTS)%90000 == 0
			typ := media.PacketMedia
			if key {
				typ = media.PacketKeyframe
			}
			stream.Bus.Publish(&media.Packet{Type: typ, Codec: media.CodecH264, DTS: v, PTS: v,
				IsKeyframe: key, Data: []byte{0, 0, 0, 1, 0x65, 0x88, 0x84}})
			v += frame
		} else {
			stream.Bus.Publish(&media.Packet{Type: media.PacketMedia, Codec: media.CodecAAC, DTS: a, PTS: a,
				Data: []byte{0x21, 0x10, 0x05}})
			a += aframe
		}
		if n++; n%32 == 0 {
			time.Sleep(time.Millisecond) // stay inside the Bus's subscriber buffer
		}
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recorder did not finish")
	}
}

func boxAt(t *testing.T, data []byte, off int64) string {
	t.Helper()
	if off+8 > int64(len(data)) {
		t.Fatalf("no box at %d (len %d)", off, len(data))
	}
	return string(data[off+4 : off+8])
}

func TestRecorder_SessionsTimelineAndManifests(t *testing.T) {
	e := newTestEngine(t)
	key := media.StreamKey{App: "live", Name: "cam"}
	ctx := context.Background()

	record(t, e, key, 900000, 10)

	segs, err := e.segmentsInRange(ctx, key, 0, 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	// Keyframes every second, 2 s target: cuts at 2,4,6,8 s plus the final
	// flush → 5 segments.
	if len(segs) != 5 {
		t.Fatalf("got %d segments, want 5", len(segs))
	}
	for i, s := range segs {
		if s.MSN != int64(i) || s.DiscSeq != 0 {
			t.Errorf("seg %d: msn=%d disc=%d", i, s.MSN, s.DiscSeq)
		}
		if i < 4 && s.VideoDur != 180000 {
			t.Errorf("seg %d: video dur %d, want 180000", i, s.VideoDur)
		}
		if i > 0 {
			p := segs[i-1]
			if s.VideoTfdt != p.VideoTfdt+p.VideoDur || s.AudioTfdt != p.AudioTfdt+p.AudioDur {
				t.Errorf("seg %d: timeline not continuous", i)
			}
		}
		// The file is video moof+mdat then audio moof+mdat; DASH serves the
		// two halves as byte ranges.
		data, err := os.ReadFile(s.Path)
		if err != nil {
			t.Fatal(err)
		}
		if boxAt(t, data, 0) != "moof" || boxAt(t, data, s.VideoBytes) != "moof" {
			t.Errorf("seg %d: expected moof at 0 and at video_bytes=%d", i, s.VideoBytes)
		}
		path, off, n, err := e.ResolveFile(ctx, key.App, key.Name, s.SessionMS, File{MSN: s.MSN, Track: TrackAudio})
		if err != nil || path != s.Path || off != s.VideoBytes || off+n != int64(len(data)) {
			t.Errorf("seg %d: audio range = %s %d+%d, %v", i, path, off, n, err)
		}
	}
	first := segs[0]
	for _, name := range []string{"init.mp4", "init_v.mp4", "init_a.mp4"} {
		f, _ := ParseFile(name)
		path, _, _, err := e.ResolveFile(ctx, key.App, key.Name, first.SessionMS, f)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if data, err := os.ReadFile(path); err != nil || boxAt(t, data, 0) != "ftyp" {
			t.Errorf("%s: not an init segment (%v)", name, err)
		}
	}

	// A reconnect: new stream object, encoder timestamps restarted.
	record(t, e, key, 1000, 4)
	all, _ := e.segmentsInRange(ctx, key, 0, 1<<62)
	second := all[len(segs):]
	if len(second) == 0 {
		t.Fatal("second session recorded nothing")
	}
	last := segs[len(segs)-1]
	if second[0].SessionMS <= last.SessionMS || second[0].DiscSeq != 1 || second[0].MSN != last.MSN+1 {
		t.Errorf("second session: session=%d disc=%d msn=%d", second[0].SessionMS, second[0].DiscSeq, second[0].MSN)
	}
	if second[0].VideoTfdt < last.VideoTfdt+last.VideoDur {
		t.Errorf("second session starts at %d, before the first one ends at %d",
			second[0].VideoTfdt, last.VideoTfdt+last.VideoDur)
	}

	hls, ctype, err := e.Manifest(ctx, key.App, key.Name, Request{Kind: KindWindow}, "")
	if err != nil || ctype != "application/vnd.apple.mpegurl" {
		t.Fatalf("HLS manifest: %v %s", err, ctype)
	}
	for _, want := range []string{
		"#EXT-X-MEDIA-SEQUENCE:0\n",
		"#EXT-X-DISCONTINUITY-SEQUENCE:0\n",
		"#EXT-X-DISCONTINUITY\n",
		"dvr/" + itoa(first.SessionMS) + "/init.mp4",
		"dvr/" + itoa(first.SessionMS) + "/0.m4s\n",
	} {
		if !strings.Contains(hls, want) {
			t.Errorf("HLS playlist missing %q:\n%s", want, hls)
		}
	}
	if strings.Count(hls, "#EXTINF:") != len(all) || strings.Contains(hls, "#EXT-X-ENDLIST") {
		t.Errorf("live HLS playlist wrong:\n%s", hls)
	}

	mpd, ctype, err := e.Manifest(ctx, key.App, key.Name, Request{DASH: true, Kind: KindWindow}, "")
	if err != nil || ctype != "application/dash+xml" {
		t.Fatalf("DASH manifest: %v %s", err, ctype)
	}
	var parsed struct {
		Type    string `xml:"type,attr"`
		Periods []struct {
			Sets []struct {
				ContentType string `xml:"contentType,attr"`
			} `xml:"AdaptationSet"`
		} `xml:"Period"`
	}
	if err := xml.Unmarshal([]byte(mpd), &parsed); err != nil {
		t.Fatalf("MPD not XML: %v\n%s", err, mpd)
	}
	if parsed.Type != "dynamic" || len(parsed.Periods) != 2 || len(parsed.Periods[0].Sets) != 2 {
		t.Errorf("MPD: type=%s periods=%d\n%s", parsed.Type, len(parsed.Periods), mpd)
	}
	if !strings.Contains(mpd, `media="dvr/`+itoa(first.SessionMS)+`/$Number$_v.m4s"`) {
		t.Errorf("MPD video template wrong:\n%s", mpd)
	}

	// A range entirely in the past is VOD.
	vod := renderHLS(segs, hlsVOD, "/live/cam/", 2)
	if !strings.Contains(vod, "#EXT-X-PLAYLIST-TYPE:VOD") || !strings.HasSuffix(vod, "#EXT-X-ENDLIST\n") ||
		!strings.Contains(vod, "/live/cam/dvr/") {
		t.Errorf("VOD playlist wrong:\n%s", vod)
	}
}

func TestCleanup_DropsSegmentsOutsideWindowAndEmptySessions(t *testing.T) {
	e := newTestEngine(t)
	key := media.StreamKey{App: "live", Name: "cam"}
	ctx := context.Background()
	record(t, e, key, 900000, 4)

	segs, _ := e.segmentsInRange(ctx, key, 0, 1<<62)
	if len(segs) == 0 {
		t.Fatal("nothing recorded")
	}
	if _, err := e.deleteSegmentsBefore(ctx, key, segs[len(segs)-1].StartMS+segs[len(segs)-1].DurationMS+1); err != nil {
		t.Fatal(err)
	}
	if left, _ := e.segmentsInRange(ctx, key, 0, 1<<62); len(left) != 0 {
		t.Fatalf("%d segments left", len(left))
	}
	if _, err := os.Stat(segs[0].Path); !os.IsNotExist(err) {
		t.Errorf("segment file not deleted: %v", err)
	}
	if err := e.deleteEmptySessions(ctx, time.Now().Add(2*time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.ResolveFile(ctx, key.App, key.Name, segs[0].SessionMS, File{Init: true}); !IsNotFound(err) {
		t.Errorf("session not cleaned up: %v", err)
	}
	if _, _, err := e.Manifest(ctx, key.App, key.Name, Request{Kind: KindWindow}, ""); !IsNotFound(err) {
		t.Errorf("manifest after cleanup: %v", err)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
