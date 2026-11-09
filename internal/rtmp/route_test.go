package rtmp

import (
	"bufio"
	"context"
	"net"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// TestPublish_RouteApp: OBS-style publishes to rtmp://host/live with
// stream key "{app}-{stream}" land on {app}/{stream} under that app's
// rules (publish key, push login), with the longest existing app name
// winning the split.
func TestPublish_RouteApp(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.ExecContext(ctx, `
		INSERT INTO applications (id, name, allow_any_stream, publish_key) VALUES ('a1', 'news', 1, 'k1');
		INSERT INTO applications (id, name, allow_any_stream) VALUES ('a2', 'my', 1);
		INSERT INTO applications (id, name, allow_any_stream) VALUES ('a3', 'my-app', 1);
		INSERT INTO applications (id, name, allow_any_stream, push_login, push_password) VALUES ('a4', 'locked', 1, 'u', 'p');`); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.RTMP.RouteApp, cfg.RTMP.RouteSeparator = "live", "-"
	registry := media.NewRegistry()
	srv := NewServer(cfg, registry, database, nil)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			go srv.handleConn(nc)
		}
	}()

	// dial connects to the routing app and returns a publish function.
	dial := func() (publish func(name string) string, closeFn func()) {
		nc, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_ = nc.SetDeadline(time.Now().Add(5 * time.Second))
		if err := clientHandshake(nc); err != nil {
			t.Fatal(err)
		}
		demux := newChunkDemuxer(bufio.NewReader(nc))
		if err := sendConnect(nc, demux, l.Addr().String(), "live"); err != nil {
			t.Fatal(err)
		}
		if _, err := nextCommand(demux); err != nil { // onBWDone
			t.Fatal(err)
		}
		return func(name string) string {
			t.Helper()
			msid, err := sendCreateStream(nc, demux)
			if err != nil {
				t.Fatalf("createStream %s: %v", name, err)
			}
			payload, _ := EncodeAMF0(AMF0String("publish"), AMF0Number(0), AMF0Null{}, AMF0String(name), AMF0String("live"))
			if err := writeChunk(nc, pullChunkSize, 8, 0, msgCommandAMF0, msid, payload); err != nil {
				t.Fatal(err)
			}
			vals, err := nextCommand(demux)
			if err != nil {
				return "closed"
			}
			info, _ := vals[len(vals)-1].(AMF0Object)
			code, _ := info["code"].(AMF0String)
			return string(code)
		}, func() { nc.Close() }
	}

	cases := []struct {
		name, want string
		key        *media.StreamKey
	}{
		{"news-cam1?k1", "NetStream.Publish.Start", &media.StreamKey{App: "news", Name: "cam1"}},
		{"news-cam2", "NetStream.Publish.BadName", nil}, // news needs its publish key
		{"my-app-cam-1", "NetStream.Publish.Start", &media.StreamKey{App: "my-app", Name: "cam-1"}},
		{"my-cam", "NetStream.Publish.Start", &media.StreamKey{App: "my", Name: "cam"}},
		{"nosuch-cam", "NetStream.Publish.BadName", nil},
		{"locked-cam", "NetStream.Publish.BadName", nil}, // push login can't be routed
		{"plain", "NetStream.Publish.BadName", nil},      // no separator, no real live app
	}
	for _, c := range cases {
		publish, closeFn := dial()
		if got := publish(c.name); got != c.want {
			t.Errorf("publish %s = %s, want %s", c.name, got, c.want)
		}
		if c.key != nil && registry.Get(*c.key) == nil {
			t.Errorf("publish %s: %s not live", c.name, c.key)
		}
		closeFn()
	}
	for _, k := range []media.StreamKey{{App: "live", Name: "news-cam1"}, {App: "news", Name: "cam2"}, {App: "locked", Name: "cam"}} {
		if registry.Get(k) != nil {
			t.Errorf("%s registered", k)
		}
	}

	// One connection feeds one app: a second rendition naming another
	// app is refused, one naming the same app is routed.
	publish, closeFn := dial()
	defer closeFn()
	for _, s := range []struct{ name, want string }{
		{"my-hd1", "NetStream.Publish.Start"},
		{"my-hd2", "NetStream.Publish.Start"},
		{"news-x?k1", "NetStream.Publish.BadName"},
	} {
		if got := publish(s.name); got != s.want {
			t.Errorf("same connection: publish %s = %s, want %s", s.name, got, s.want)
		}
	}
	if registry.Get(media.StreamKey{App: "my", Name: "hd2"}) == nil {
		t.Error("my/hd2 not live")
	}
}
