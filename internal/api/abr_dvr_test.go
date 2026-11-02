package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/dvr"
	"github.com/5centscdn/origin-mediaserver/internal/hls"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
	"github.com/5centscdn/origin-mediaserver/internal/scte35ctrl"
)

// TestABRDVRMaster covers the FMLE case from the field: test%i published
// test1..test3, the encoder has disconnected, and the player asks for the
// group's rewind playlist /vineeth/test/playlist_dvr.m3u8. Nothing is
// recorded under "test" itself, so this must be a master over the variants'
// recordings, with attributes taken from the recording.
func TestABRDVRMaster(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	registry := media.NewRegistry()
	m := metrics.New()
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret-at-least-32-bytes-long!!"
	cfg.Server.DataDir = t.TempDir()
	cfg.DVR.WindowSeconds = 3600
	srv := New(ctx, cfg, database, registry, dvr.NewEngine(cfg, database, m), nil, nil,
		hls.NewPlaylistRegistry(), nil, nil, m, scte35ctrl.NewManager(ctx, database, registry, m))

	session := time.Now().Add(-time.Minute).UnixMilli()
	record := func(stream string, w, h int, segBytes int64) {
		if _, err := database.ExecContext(ctx, `INSERT INTO dvr_sessions
			(app_name, stream_name, session_ms, dir_path, has_video, has_audio, video_codecs, audio_codecs, width, height)
			VALUES ('vineeth', ?, ?, '/x', 1, 1, 'avc1.64001f', 'mp4a.40.2', ?, ?)`, stream, session, w, h); err != nil {
			t.Fatal(err)
		}
		for i := int64(0); i < 3; i++ {
			start := session + i*6000
			if _, err := database.ExecContext(ctx, `INSERT INTO dvr_segments
				(id, app_name, stream_name, seq, msn, session_ms, start_pts_ms, end_pts_ms, duration_ms, file_path, size_bytes)
				VALUES (?, 'vineeth', ?, 0, ?, ?, ?, ?, 6000, '/x/seg', ?)`,
				fmt.Sprintf("%s-%d", stream, i), stream, i, session, start, start+6000, segBytes); err != nil {
				t.Fatal(err)
			}
		}
	}
	record("test1", 1280, 720, 750000) // 1 Mbps
	record("test2", 640, 360, 375000)  // 500 kbps
	record("test10", 320, 180, 75000)  // numeric order: after test2
	record("testing", 320, 180, 75000) // not a %i variant of "test"
	record("cam", 640, 360, 375000)    // ordinary stream

	get := func(path string) (int, string) {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code, rec.Body.String()
	}

	code, body := get("/vineeth/test/playlist_dvr.m3u8")
	if code != http.StatusOK {
		t.Fatalf("group rewind = %d %q", code, body)
	}
	for _, want := range []string{
		`BANDWIDTH=1000000,AVERAGE-BANDWIDTH=1000000,RESOLUTION=1280x720,CODECS="avc1.64001f,mp4a.40.2"`,
		"vineeth/test1/playlist_dvr.m3u8",
		"RESOLUTION=640x360",
		"vineeth/test2/playlist_dvr.m3u8",
		"vineeth/test10/playlist_dvr.m3u8",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("master missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "testing") {
		t.Errorf("non-variant stream listed:\n%s", body)
	}
	if strings.Index(body, "test2/") > strings.Index(body, "test10/") {
		t.Errorf("variants not in numeric order:\n%s", body)
	}

	// Range and timeshift names carry through to the variants.
	rng := fmt.Sprintf("playlist_dvr_range-%d-30.m3u8", session/1000)
	if code, body := get("/vineeth/test/" + rng); code != http.StatusOK || !strings.Contains(body, "\nvineeth/test1/"+rng) {
		t.Errorf("range master = %d:\n%s", code, body)
	}
	if strings.Contains(body, "..") {
		t.Errorf("master uses .. paths:\n%s", body)
	}
	// Each variant URI the master lists resolves (under the group) to the
	// variant's playlist, and its relative segment URIs resolve under it too.
	code, vbody := get("/vineeth/test/vineeth/test1/playlist_dvr.m3u8")
	if code != http.StatusOK || !strings.Contains(vbody, "#EXTINF") {
		t.Fatalf("variant playlist = %d:\n%s", code, vbody)
	}
	if !strings.Contains(vbody, fmt.Sprintf("dvr/%d/0.m4s", session)) {
		t.Errorf("variant segment URIs not relative:\n%s", vbody)
	}
	// An ordinary recorded stream still gets its media playlist.
	if code, body := get("/vineeth/cam/playlist_dvr.m3u8"); code != http.StatusOK || strings.Contains(body, "STREAM-INF") {
		t.Errorf("cam rewind = %d:\n%s", code, body)
	}
	// A range before any recording: the group is known but empty → 404.
	if code, _ := get("/vineeth/test/playlist_dvr_range-1000000000-30.m3u8"); code != http.StatusNotFound {
		t.Errorf("empty-range master = %d, want 404", code)
	}
}
