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

// TestPublish_MaxConnsPerApp: with max_connections_per_app = 2, a
// connection publishing two renditions takes one slot, a second connection
// takes the other, and a third is refused with BadName until one of the
// first two disconnects.
func TestPublish_MaxConnsPerApp(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.ExecContext(ctx, `INSERT INTO applications (id, name, allow_any_stream) VALUES ('a1', 'live', 1)`); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.RTMP.MaxConnsPerApp = 2
	srv := NewServer(cfg, media.NewRegistry(), database, nil)
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

	// dial connects to app "live" and returns the connection and a
	// publish function that reports the server's status code.
	dial := func() (net.Conn, func(string) string) {
		t.Helper()
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
		return nc, func(name string) string {
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
	}

	const start, refused = "NetStream.Publish.Start", "NetStream.Publish.BadName"
	ncA, pubA := dial()
	if got := pubA("a1"); got != start {
		t.Fatalf("a1 = %s", got)
	}
	if got := pubA("a2"); got != start { // same connection, same slot
		t.Fatalf("a2 = %s", got)
	}
	ncB, pubB := dial()
	defer ncB.Close()
	if got := pubB("b1"); got != start {
		t.Fatalf("b1 = %s", got)
	}
	ncC, pubC := dial()
	defer ncC.Close()
	if got := pubC("c1"); got != refused {
		t.Fatalf("c1 = %s, want %s", got, refused)
	}

	// A disconnects: its slot comes back once the server has cleaned up.
	ncA.Close()
	deadline := time.Now().Add(3 * time.Second)
	for i := 2; ; i++ {
		got := pubC("c" + string(rune('0'+i)))
		if got == start {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot not released after A disconnected (last: %s)", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestHandshakeTimeout: a client that connects and never sends C0/C1 is
// disconnected once the handshake deadline passes.
func TestHandshakeTimeout(t *testing.T) {
	cfg := &config.Config{}
	cfg.RTMP.IdleTimeoutMs = 200
	srv := NewServer(cfg, media.NewRegistry(), nil, nil)
	client, server := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() { srv.handleConn(server); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("connection still open after the handshake deadline")
	}
}
