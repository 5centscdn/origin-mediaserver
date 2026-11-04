package pull

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// fakeUpstreams stands in for remote servers: each URL is "down" (the
// connection fails), "silent" (connects, sends nothing, like an RTMP server
// whose stream isn't published) or "up" (sends a video frame every 20 ms).
type fakeUpstreams struct {
	mu    sync.Mutex
	state map[string]string
}

func (f *fakeUpstreams) set(u, s string) {
	f.mu.Lock()
	f.state[u] = s
	f.mu.Unlock()
}

func (f *fakeUpstreams) get(u string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state[u]
}

func (f *fakeUpstreams) pull(ctx context.Context, src db.PullSource, bus *media.Bus) error {
	if f.get(src.URL) == "down" {
		return errors.New("connection refused")
	}
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	var dts int64
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		switch f.get(src.URL) {
		case "down":
			return errors.New("connection reset")
		case "up":
			dts += 1800
			bus.Publish(&media.Packet{Codec: media.CodecH264, Type: media.PacketMedia, DTS: dts, PTS: dts})
		}
	}
}

// The Nimble scenario: primary down at start → fallback; primary comes back
// → switch to it; primary goes silent → back to the fallback.
func TestFallback_SwitchesAndReturnsToPrimary(t *testing.T) {
	// Shortened for the test and not restored: Stop doesn't wait for the
	// worker's goroutines, and no other test here depends on them.
	stallTimeout, probeTimeout, probeSteady, pollEvery = 600*time.Millisecond, 2*time.Second, 300*time.Millisecond, 20*time.Millisecond

	const primary, backup = "fake://primary/live", "fake://backup/live"
	up := &fakeUpstreams{state: map[string]string{primary: "down", backup: "up"}}
	supportedProtocols["fake"] = up.pull
	defer delete(supportedProtocols, "fake")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	database, err := db.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg := media.NewRegistry()
	m := NewManager(database, reg, nil)
	src := db.PullSource{
		ID: "p1", AppName: "live", StreamName: "cam1", URL: primary, Protocol: "fake",
		Reconnect: true, ReconnectDelayS: 1, FallbackURLs: []string{backup}, PrimaryCheckS: 1,
	}
	if err := m.Start(ctx, src); err != nil {
		t.Fatal(err)
	}
	defer m.Stop("p1")

	key := media.StreamKey{App: "live", Name: "cam1"}
	waitFor := func(want string, within time.Duration) {
		t.Helper()
		deadline := time.Now().Add(within)
		for time.Now().Before(deadline) {
			if s := reg.Get(key); s != nil && s.SourceAddr == want && m.ActiveURL("p1") == want &&
				time.Since(s.Bus.LastMediaAt()) < 200*time.Millisecond {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		got := ""
		if s := reg.Get(key); s != nil {
			got = s.SourceAddr
		}
		t.Fatalf("stream source = %q, want %q (live) within %v", got, want, within)
	}

	waitFor(backup, 2*time.Second)

	up.set(primary, "up")
	// Next check within 1 s, steady for 300 ms.
	waitFor(primary, 3*time.Second)

	up.set(primary, "silent")
	waitFor(backup, 3*time.Second)

	// A flapping primary (down again) must not pull the stream off the backup.
	up.set(primary, "down")
	time.Sleep(1500 * time.Millisecond)
	waitFor(backup, 100*time.Millisecond)
}

func TestProtocolForURL(t *testing.T) {
	for u, want := range map[string]string{
		"rtmp://103.124.108.42/static/KAUMUDY":           "rtmp",
		"srt://h:9000?streamid=x":                        "srt",
		"rtsp://cam/stream":                              "rtsp",
		"udp://0.0.0.0:5000":                             "mpegts_udp",
		"http://h/live.ts":                               "mpegts_http",
		"http://woct.oshkoshmedia.org/broadwavehigh.mp3": "icecast",
		"https://h:8000/radio.AAC":                       "icecast",
		"https://h/alba/live1/playlist.m3u8?token=1":     "hls",
		"weird":                                          "rtmp",
	} {
		if got := ProtocolForURL(u, "rtmp"); got != want {
			t.Errorf("ProtocolForURL(%q) = %q, want %q", u, got, want)
		}
	}
	// An explicit choice for an http URL is kept: Icecast mounts don't
	// always end in .mp3, and MPEG-TS URLs sometimes do odd things.
	if got := ProtocolForURL("http://h:8000/stream", "icecast"); got != "icecast" {
		t.Errorf("explicit icecast overridden: %q", got)
	}
	if got := ProtocolForURL("http://h/x.mp3", "mpegts_http"); got != "mpegts_http" {
		t.Errorf("explicit mpegts_http overridden: %q", got)
	}
}

// TestPull_ListedOnlyWithMedia: a source that refuses the connection or
// connects and sends nothing (an IPTV account over its connection limit)
// must never appear in the registry, i.e. Live Streams. Once it sends
// media it's listed, and it keeps retrying until then.
func TestPull_ListedOnlyWithMedia(t *testing.T) {
	stallTimeout, probeTimeout, probeSteady, pollEvery = 300*time.Millisecond, 2*time.Second, 300*time.Millisecond, 20*time.Millisecond

	const u = "fake://iptv/ch1"
	up := &fakeUpstreams{state: map[string]string{u: "down"}}
	supportedProtocols["fake"] = up.pull
	defer delete(supportedProtocols, "fake")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	database, err := db.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg := media.NewRegistry()
	m := NewManager(database, reg, nil)
	if err := m.Start(ctx, db.PullSource{ID: "p2", AppName: "Atinka", StreamName: "tv773", URL: u,
		Protocol: "fake", Reconnect: true, ReconnectDelayS: 1}); err != nil {
		t.Fatal(err)
	}
	defer m.Stop("p2")
	key := media.StreamKey{App: "Atinka", Name: "tv773"}

	watch := func(state string, d time.Duration) {
		t.Helper()
		up.set(u, state)
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			if reg.Get(key) != nil {
				t.Fatalf("stream listed while the source is %q", state)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	watch("down", 800*time.Millisecond)
	watch("silent", 800*time.Millisecond)

	up.set(u, "up")
	deadline := time.Now().Add(4 * time.Second)
	for reg.Get(key) == nil {
		if time.Now().After(deadline) {
			t.Fatal("stream not listed after the source started sending media")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if reg.Get(key).Bus.LastMediaAt().IsZero() {
		t.Error("listed before any media")
	}
	if a, f := m.Stats(); a < 2 || f < 2 {
		t.Errorf("attempts=%d failures=%d, want the failed tries counted", a, f)
	}
}
