package icecast

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	dbpkg "github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/media"
)

func newTestDB(t *testing.T) *dbpkg.DB {
	t.Helper()
	database, err := dbpkg.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func insertTestApp(t *testing.T, database *dbpkg.DB, name, publishKey string, allowAnyStream bool) {
	t.Helper()
	allowAny := 0
	if allowAnyStream {
		allowAny = 1
	}
	_, err := database.ExecContext(context.Background(), `
		INSERT INTO applications
		  (id, name, publish_key, enabled, allow_any_stream,
		   out_hls, out_dash, out_mpegts, out_ll_hls,
		   hls_seg_duration_s, hls_playlist_window)
		VALUES (?,?,?,1,?,1,0,0,0,6,5)`,
		name, name, publishKey, allowAny,
	)
	if err != nil {
		t.Fatalf("insert test app: %v", err)
	}
}

func startTestServer(t *testing.T, database *dbpkg.DB, listen string) (*Server, chan error) {
	t.Helper()
	registry := media.NewRegistry()
	cfg := &config.Config{Icecast: config.IcecastConfig{Listen: listen}}
	srv := NewServer(cfg, registry, database)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe(ctx) }()
	time.Sleep(100 * time.Millisecond) // let the listener bind
	return srv, errCh
}

// TestServer_EndToEnd drives a real SOURCE handshake (raw TCP, hand-built
// request — there's no equivalent of gosrt.Dial for Icecast, so this is
// the same fake-client-over-real-wire-protocol approach used by
// internal/rtmp/puller_test.go and internal/ingest/rtsp/client_test.go)
// followed by a real MP3 fixture, and confirms the resulting stream's Bus
// receives correctly-decoded packets.
func TestServer_EndToEnd(t *testing.T) {
	database := newTestDB(t)
	insertTestApp(t, database, "live", "", true)
	srv, _ := startTestServer(t, database, "127.0.0.1:18900")

	conn, err := net.Dial("tcp", "127.0.0.1:18900")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "SOURCE /live/radio1 HTTP/1.0\r\n")
	fmt.Fprintf(conn, "Content-Type: audio/mpeg\r\n")
	fmt.Fprintf(conn, "\r\n")

	r := bufio.NewReader(conn)
	status, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if status != "HTTP/1.0 200 OK\r\n" {
		t.Fatalf("status line = %q, want 200 OK", status)
	}
	// consume the blank line terminating the (empty) response headers
	if _, err := r.ReadString('\n'); err != nil {
		t.Fatalf("read response terminator: %v", err)
	}

	key := media.StreamKey{App: "live", Name: "radio1"}
	var stream *media.Stream
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if stream = srv.registry.Get(key); stream != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if stream == nil {
		t.Fatal("stream never registered")
	}
	if stream.SourceType != "icecast" {
		t.Errorf("SourceType = %q, want %q", stream.SourceType, "icecast")
	}

	sub := stream.Bus.Subscribe(context.Background())
	defer sub.Close()

	data := decodeFixture(t, fixtureMP3)
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write mp3 bytes: %v", err)
	}

	select {
	case pkt := <-sub.Chan():
		if pkt.Codec != media.CodecMP3 {
			t.Errorf("Codec = %v, want CodecMP3", pkt.Codec)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("never received a decoded MP3 packet")
	}
}

// TestServer_PUTMethodAndAuth confirms the modern HTTP PUT handshake works
// (not just Icecast2-native SOURCE) and that a correct Basic-auth password
// is accepted.
func TestServer_PUTMethodAndAuth(t *testing.T) {
	database := newTestDB(t)
	insertTestApp(t, database, "live", "secret123", true)
	startTestServer(t, database, "127.0.0.1:18901")

	conn, err := net.Dial("tcp", "127.0.0.1:18901")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	auth := base64.StdEncoding.EncodeToString([]byte("source:secret123"))
	fmt.Fprintf(conn, "PUT /live/radio2 HTTP/1.1\r\n")
	fmt.Fprintf(conn, "Authorization: Basic %s\r\n", auth)
	fmt.Fprintf(conn, "Content-Type: audio/mpeg\r\n")
	fmt.Fprintf(conn, "\r\n")

	r := bufio.NewReader(conn)
	status, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if status != "HTTP/1.0 200 OK\r\n" {
		t.Fatalf("status line = %q, want 200 OK", status)
	}
}

func expectRejection(t *testing.T, addr, request string, wantStatus int) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	fmt.Fprint(conn, request)

	r := bufio.NewReader(conn)
	status, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	want := fmt.Sprintf("HTTP/1.0 %d", wantStatus)
	if len(status) < len(want) || status[:len(want)] != want {
		t.Errorf("status line = %q, want prefix %q", status, want)
	}
}

func TestServer_RejectsBadPublishKey(t *testing.T) {
	database := newTestDB(t)
	insertTestApp(t, database, "live", "secret123", true)
	startTestServer(t, database, "127.0.0.1:18902")

	auth := base64.StdEncoding.EncodeToString([]byte("source:wrongpassword"))
	expectRejection(t, "127.0.0.1:18902",
		fmt.Sprintf("SOURCE /live/radio3 HTTP/1.0\r\nAuthorization: Basic %s\r\nContent-Type: audio/mpeg\r\n\r\n", auth),
		401)
}

func TestServer_RejectsUnknownApp(t *testing.T) {
	database := newTestDB(t)
	startTestServer(t, database, "127.0.0.1:18903")

	expectRejection(t, "127.0.0.1:18903",
		"SOURCE /nosuchapp/radio HTTP/1.0\r\nContent-Type: audio/mpeg\r\n\r\n",
		404)
}

func TestServer_RejectsUnsupportedContentType(t *testing.T) {
	database := newTestDB(t)
	insertTestApp(t, database, "live", "", true)
	startTestServer(t, database, "127.0.0.1:18904")

	expectRejection(t, "127.0.0.1:18904",
		"SOURCE /live/radio HTTP/1.0\r\nContent-Type: application/ogg\r\n\r\n",
		415)
}

func TestServer_RejectsUnparseableMountpoint(t *testing.T) {
	database := newTestDB(t)
	startTestServer(t, database, "127.0.0.1:18905")

	expectRejection(t, "127.0.0.1:18905",
		"SOURCE /flatmountpoint HTTP/1.0\r\nContent-Type: audio/mpeg\r\n\r\n",
		400)
}

func TestParseIcecastPath(t *testing.T) {
	cases := []struct {
		path, wantApp, wantStream string
		wantOK                    bool
	}{
		{"/live/radio1", "live", "radio1", true},
		{"live/radio1", "live", "radio1", true},
		{"/live", "", "", false},
		{"/", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		app, stream, ok := parseIcecastPath(c.path)
		if ok != c.wantOK {
			t.Errorf("parseIcecastPath(%q) ok = %v, want %v", c.path, ok, c.wantOK)
			continue
		}
		if ok && (app != c.wantApp || stream != c.wantStream) {
			t.Errorf("parseIcecastPath(%q) = (%q, %q), want (%q, %q)", c.path, app, stream, c.wantApp, c.wantStream)
		}
	}
}
