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

// TestPublish_StreamLimit: a "cam_%i" stream config with max_streams 2
// takes FMLE's first two renditions and refuses the third with BadName,
// without dropping the connection the first two ride on.
func TestPublish_StreamLimit(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.ExecContext(ctx, `INSERT INTO applications (id, name, allow_any_stream) VALUES ('a1', 'live', 0);
		INSERT INTO stream_configs (id, app_id, name_pattern, is_wildcard, max_streams) VALUES ('s1', 'a1', 'cam_%i', 1, 2)`); err != nil {
		t.Fatal(err)
	}

	registry := media.NewRegistry()
	srv := NewServer(&config.Config{}, registry, database, nil)
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

	nc, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
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

	publish := func(name string) string {
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
			t.Fatalf("publish %s: %v", name, err)
		}
		info, _ := vals[len(vals)-1].(AMF0Object)
		code, _ := info["code"].(AMF0String)
		return string(code)
	}

	steps := []struct{ name, want string }{
		{"cam_1", "NetStream.Publish.Start"},
		{"cam_2", "NetStream.Publish.Start"},
		{"cam_3", "NetStream.Publish.BadName"},
	}
	for _, s := range steps {
		if got := publish(s.name); got != s.want {
			t.Errorf("publish %s = %s, want %s", s.name, got, s.want)
		}
	}
	for _, name := range []string{"cam_1", "cam_2"} {
		if registry.Get(media.StreamKey{App: "live", Name: name}) == nil {
			t.Errorf("%s not live after the refused third rendition", name)
		}
	}
	if registry.Get(media.StreamKey{App: "live", Name: "cam_3"}) != nil {
		t.Error("cam_3 registered past the limit")
	}
}
