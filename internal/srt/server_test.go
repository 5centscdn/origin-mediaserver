package srt

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"
	"time"

	gosrt "github.com/datarhei/gosrt"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	dbpkg "github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/mux"
)

func TestParseStreamID(t *testing.T) {
	cases := []struct {
		id, wantApp, wantStream string
		wantPlay, wantErr       bool
	}{
		{"live/mystream", "live", "mystream", false, false},
		{"/live/mystream", "live", "mystream", false, false},
		{"live/mystream?secret-key", "live", "mystream?secret-key", false, false},
		{"play/live/mystream", "live", "mystream", true, false},
		{"#!::r=live/mystream,m=request", "live", "mystream", true, false},
		{"#!::m=request,r=live/mystream", "live", "mystream", true, false},
		{"#!::r=live/mystream,m=publish", "live", "mystream", false, false},
		{"#!::r=live/mystream", "live", "mystream", false, false},
		{"#!::u=bob,r=live/mystream?key,m=publish", "live", "mystream?key", false, false},
		{"#!::r=live/mystream,m=bidirectional", "", "", false, true},
		{"#!::m=request", "", "", false, true},
		{"live", "", "", false, true},
		{"", "", "", false, true},
		{"/live", "", "", false, true},
	}
	for _, c := range cases {
		app, stream, play, err := parseStreamID(c.id)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseStreamID(%q): expected error, got app=%q stream=%q", c.id, app, stream)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseStreamID(%q): unexpected error: %v", c.id, err)
			continue
		}
		if app != c.wantApp || stream != c.wantStream || play != c.wantPlay {
			t.Errorf("parseStreamID(%q) = (%q, %q, play=%v), want (%q, %q, play=%v)", c.id, app, stream, play, c.wantApp, c.wantStream, c.wantPlay)
		}
	}
}

func TestValidatePublishKey(t *testing.T) {
	if !validatePublishKey("secret", "secret") {
		t.Error("bare key should validate")
	}
	if !validatePublishKey("mystream?secret", "secret") {
		t.Error("name?key suffix should validate")
	}
	if validatePublishKey("mystream?wrong", "secret") {
		t.Error("wrong key should not validate")
	}
	if validatePublishKey("mystreamsecret", "secret") {
		t.Error("key without '?' separator should not validate")
	}
}

func TestStripKeyFromName(t *testing.T) {
	if got := stripKeyFromName("mystream?secret"); got != "mystream" {
		t.Errorf("stripKeyFromName = %q, want %q", got, "mystream")
	}
	if got := stripKeyFromName("mystream"); got != "mystream" {
		t.Errorf("stripKeyFromName = %q, want %q (no key present)", got, "mystream")
	}
}

// Fixture: same real 3-frame libx264 H.264 elementary stream used
// throughout this codebase's other ingest tests.
const (
	fixtureSPS   = "Z0LACtkEJsBEAAADAAQAAAMAyDxImSA="
	fixturePPS   = "aMuDyyA="
	fixtureIDR   = "ZYiECX/h8h+KAAIAPigAEuAH2lobkDHXABZWvWasHLOP10A6RkCEPw4gARSIAAIA4AxgTGbDY47YaAEeJIp1IQIQIkiGQhA5XEpSlDDUUAAhBy8or2PABv2adUIaNDDhCgAYAGCAAQABAXHHCkXUC++B0Btor51f80cK9wHTPCxLD8Q8P+GoDgnHES38GCBsiEzx4QgAECgACBCEAAiBFSAIjJYBuYphA2zz9fLAOookUwDfPP+YfWKU88AAlgBkqoXd6zxQACX8YBF78P91vwCJajL06g9By1ZEt/MX/9eEAELIQEAAIA4AAgK0DG6HJhvavW2/Ff4vh+ezvgwGVrI5i6oHZmIO0PwvACXinpN//ZX9b22PH//GnMclRE+Vf4C8SRLyzTg3+DDpZnFtlP/Sn0iw56IAGxIml0mP3zQFmi7PRLDb7fVOdqP//v9vNYfhD//BBA4EB3oMAatYlYrFAp6vencU+uXS7///swQ4DgIxrkyAL8K7WLZc99sQx4YKAcRAAuDo/wdLp4B+Qg9OBXao/KoC8Afrp+GUfgYAAyAwDwwADwAOAHApkiDVywEzTeyqEh4oYPkVOAK3TNGCUdX5H+QAoAg8Q8V6roVh0WIFoRs3NAPQh5bAEYV5HNP/9vCACAAIAKDQgBAFgCQoyEwDQbLIAyOj4Fa7DW1aCgZGeXfYQKxM8IWcv/wxC7we+Dw/g8PiT8BZPAqAWe6PB0xyWxXy1BBEAAQHnAAEBQIAA2cAB8JxkXxpASGlgDI98IE3e+EA8SA8OmcsAZF/wgj099hh2coAHti+/eAER+U3H5ruKngB4OmM0s++BoCDBAgMF+P8UDnBtrfc4YoDPwM72OeYxAIx/xEilw=="
	fixtureDelta = "QZo4FOPRPonw"
)

func decodeFixture(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return b
}

func withStartCode(nal []byte) []byte {
	return append([]byte{0, 0, 0, 1}, nal...)
}

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

// TestServer_EndToEnd drives a real SRT connection (a genuine gosrt.Dial
// caller, not a mock) against a real Server.ListenAndServe listener,
// carrying real MPEG-TS bytes produced by this codebase's own
// mux.TSMuxer, and confirms the resulting media.Stream's Bus receives a
// correctly-decoded H.264 keyframe — the same "real bytes through both
// real ends" standard used by internal/ingest/rtsp's client_test.go.
func TestServer_EndToEnd(t *testing.T) {
	database := newTestDB(t)
	insertTestApp(t, database, "live", "", true)

	registry := media.NewRegistry()
	cfg := &config.Config{SRT: config.SRTConfig{Listen: "127.0.0.1:0", LatencyMs: 20}}

	srv := NewServer(cfg, registry, database)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Bind on an ephemeral port ourselves first so we know the real address
	// (ListenAndServe binds internally) — instead, run ListenAndServe in the
	// background and poll the registry, since gosrt.Listen chooses the OS
	// there's no separate "bound address" callback; use a fixed high port
	// instead of :0 to keep this deterministic.
	cfg.SRT.Listen = "127.0.0.1:18855"

	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- srv.ListenAndServe(ctx) }()
	time.Sleep(200 * time.Millisecond) // let the listener bind

	sps := decodeFixture(t, fixtureSPS)
	pps := decodeFixture(t, fixturePPS)
	idr := decodeFixture(t, fixtureIDR)
	delta := decodeFixture(t, fixtureDelta)

	var tsBuf bytes.Buffer
	tsMuxer := mux.NewTSMuxer(&tsBuf, mux.StreamTypeH264, mux.StreamTypeAAC)
	tsMuxer.SetSPSPPS(sps, pps)
	if err := tsMuxer.WriteVideo(withStartCode(idr), 0, 0, true); err != nil {
		t.Fatalf("WriteVideo idr: %v", err)
	}
	// A second video frame is required to flush the demuxer's PES
	// reassembly buffer: an MPEG-TS PES payload's length is implicit,
	// terminated only by the next payload_unit_start on the same PID (or
	// stream EOF) — the same "needs a follow-up to release" pattern as
	// pion/webrtc/samplebuilder, just at the PES layer instead of RTP.
	if err := tsMuxer.WriteVideo(withStartCode(delta), 3000, 3000, false); err != nil {
		t.Fatalf("WriteVideo delta: %v", err)
	}

	dialCfg := gosrt.DefaultConfig()
	dialCfg.StreamId = "live/mystream"
	conn, err := gosrt.Dial("srt", cfg.SRT.Listen, dialCfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write(tsBuf.Bytes()); err != nil {
		t.Fatalf("write ts bytes: %v", err)
	}

	key := media.StreamKey{App: "live", Name: "mystream"}
	var stream *media.Stream
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if stream = registry.Get(key); stream != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if stream == nil {
		t.Fatal("stream never registered")
	}

	sub := stream.Bus.Subscribe(ctx)
	defer sub.Close()

	var gotKeyframe bool
	waitDeadline := time.After(5 * time.Second)
	for !gotKeyframe {
		select {
		case pkt := <-sub.Chan():
			if pkt.Codec == media.CodecH264 && pkt.IsKeyframe {
				gotKeyframe = true
			}
		case <-waitDeadline:
			t.Fatal("never received a decoded H.264 keyframe over SRT")
		}
	}

	conn.Close()
	cancel()
	<-srvErrCh
}

// TestServer_RejectsUnknownApp confirms a connection whose streamid names
// an application that was never created in the DB is rejected outright
// (not silently accepted and then dropped).
func TestServer_RejectsUnknownApp(t *testing.T) {
	database := newTestDB(t)
	registry := media.NewRegistry()
	cfg := &config.Config{SRT: config.SRTConfig{Listen: "127.0.0.1:18856", LatencyMs: 20}}
	srv := NewServer(cfg, registry, database)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- srv.ListenAndServe(ctx) }()
	time.Sleep(200 * time.Millisecond)

	dialCfg := gosrt.DefaultConfig()
	dialCfg.StreamId = "nosuchapp/mystream"
	_, err := gosrt.Dial("srt", cfg.SRT.Listen, dialCfg)
	if err == nil {
		t.Fatal("expected Dial to fail for an unknown application")
	}

	cancel()
	<-srvErrCh
}
