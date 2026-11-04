package hlspull

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// segments splits the repo's real x264 MPEG-TS sample (4 s, keyint 30)
// into n segments on TS packet boundaries.
func segments(t *testing.T, n int) [][]byte {
	t.Helper()
	ts, err := os.ReadFile("../mpegts/testdata/x264_bframes.ts")
	if err != nil {
		t.Fatal(err)
	}
	pkts := len(ts) / 188
	var out [][]byte
	for i := 0; i < n; i++ {
		out = append(out, ts[i*pkts/n*188:(i+1)*pkts/n*188])
	}
	return out
}

// source is a fake HLS origin: a master playlist pointing at a media
// playlist whose window (size 3) slides forward one segment per reload
// until it reaches the last one.
type source struct {
	mu      sync.Mutex
	segs    [][]byte
	visible int // segments published so far
	ended   bool
	frozen  bool
	fetched []int
}

func (s *source) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.URL.Path == "/live/master.m3u8":
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=500000\nlow/index.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=900000,CODECS=\"avc1.64001e\"\nhigh/index.m3u8?tok=abc\n")
	case r.URL.Path == "/live/high/index.m3u8":
		if r.URL.Query().Get("tok") != "abc" {
			http.Error(w, "no token", http.StatusForbidden)
			return
		}
		first := max(s.visible-3, 0)
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n", 100+first)
		for i := first; i < s.visible; i++ {
			fmt.Fprintf(w, "#EXTINF:1.0,\nseg%d.ts\n", 100+i)
		}
		if s.ended {
			fmt.Fprint(w, "#EXT-X-ENDLIST\n")
		}
		if !s.frozen && s.visible < len(s.segs) {
			s.visible++
		}
	case strings.HasPrefix(r.URL.Path, "/live/high/seg"):
		var n int
		fmt.Sscanf(r.URL.Path, "/live/high/seg%d.ts", &n)
		s.fetched = append(s.fetched, n)
		w.Write(s.segs[n-100])
	default:
		http.NotFound(w, r)
	}
}

func collect(bus *media.Bus) (func() (video int, keyframes int), func()) {
	ctx, cancel := context.WithCancel(context.Background())
	sub := bus.Subscribe(ctx)
	var mu sync.Mutex
	var v, k int
	done := make(chan struct{})
	go func() {
		defer close(done)
		for p := range sub.Chan() {
			if p.Codec == media.CodecH264 && p.Type != media.PacketConfig {
				mu.Lock()
				v++
				if p.IsKeyframe {
					k++
				}
				mu.Unlock()
			}
		}
	}()
	return func() (int, int) { mu.Lock(); defer mu.Unlock(); return v, k },
		func() { cancel(); sub.Close(); <-done }
}

// TestPull_Live follows a sliding live playlist from the best variant
// (token in its URI kept), starts near the live edge, fetches every later
// segment exactly once and in order, and gives up once the playlist stops
// advancing.
func TestPull_Live(t *testing.T) {
	src := &source{segs: segments(t, 8), visible: 4}
	srv := httptest.NewServer(src)
	defer srv.Close()

	bus := media.NewBus()
	count, stop := collect(bus)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() {
		// Freeze the source once everything is out, so the pull stalls.
		for {
			time.Sleep(100 * time.Millisecond)
			src.mu.Lock()
			if src.visible == len(src.segs) {
				src.frozen = true
				src.mu.Unlock()
				return
			}
			src.mu.Unlock()
		}
	}()
	err := Pull(ctx, srv.URL+"/live/master.m3u8", bus)
	if err == nil || !strings.Contains(err.Error(), "hasn't advanced") {
		t.Fatalf("Pull = %v, want a stall error", err)
	}

	src.mu.Lock()
	got := fmt.Sprint(src.fetched)
	src.mu.Unlock()
	// 4 visible at start → begin 2 from the edge (102), then 104..107.
	if want := "[102 103 104 105 106 107]"; got != want {
		t.Errorf("fetched segments %s, want %s", got, want)
	}
	if v, k := count(); v < 60 || k < 2 {
		t.Errorf("published %d video frames (%d keyframes), want the last 3/4 of 120 frames", v, k)
	}
	if ci := bus.CodecInfo(); ci == nil || ci.VideoCodec != media.CodecH264 || ci.Width != 320 {
		t.Errorf("codec info = %+v, want H.264 320 wide", ci)
	}
}

// TestPull_EndList: a finished playlist is played from its first segment
// and Pull returns cleanly at the end.
func TestPull_EndList(t *testing.T) {
	src := &source{segs: segments(t, 3), visible: 3, ended: true, frozen: true}
	srv := httptest.NewServer(src)
	defer srv.Close()
	bus := media.NewBus()
	count, stop := collect(bus)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Pull(ctx, srv.URL+"/live/high/index.m3u8?tok=abc", bus); err != nil {
		t.Fatalf("Pull = %v", err)
	}
	if fmt.Sprint(src.fetched) != "[100 101 102]" {
		t.Errorf("fetched %v", src.fetched)
	}
	time.Sleep(100 * time.Millisecond)
	if v, _ := count(); v < 110 {
		t.Errorf("published %d of 120 video frames", v)
	}
}

func TestParse_Refuses(t *testing.T) {
	for name, body := range map[string]string{
		"fmp4":      "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:2,\na.m4s\n",
		"aes":       "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k\"\n#EXTINF:2,\na.ts\n",
		"byterange": "#EXTM3U\n#EXTINF:2,\n#EXT-X-BYTERANGE:100@0\na.ts\n",
		"not m3u8":  "<html>",
	} {
		if _, err := parse(strings.NewReader(body)); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
	pl, err := parse(strings.NewReader("#EXTM3U\n#EXT-X-KEY:METHOD=NONE\n#EXT-X-TARGETDURATION:8\n#EXT-X-MEDIA-SEQUENCE:6887\n#EXTINF:8.3,\na.ts\n#EXT-X-DISCONTINUITY\n#EXTINF:8.3,\nb.ts\n"))
	if err != nil || pl.target != 8*time.Second || len(pl.segments) != 2 || pl.segments[1].seq != 6888 {
		t.Errorf("parse = %+v, %v", pl, err)
	}
}

func TestResolve_HTTPOnly(t *testing.T) {
	base, _ := url.Parse("https://h/a/b.m3u8")
	if _, err := resolve(base, "file:///etc/passwd"); err == nil {
		t.Error("file: URI accepted")
	}
	if u, err := resolve(base, "../c/d.ts?x=1"); err != nil || u.String() != "https://h/c/d.ts?x=1" {
		t.Errorf("resolve = %v, %v", u, err)
	}
}
