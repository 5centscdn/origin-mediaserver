package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/dvr"
	"github.com/5centscdn/origin-mediaserver/internal/hls"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
	"github.com/5centscdn/origin-mediaserver/internal/scte35ctrl"
)

// TestDVRRoutes checks that the nDVR names reach the DVR handlers through
// the real router — the playlist names share /{app}/{stream}/{segment}
// with ordinary segments, so this is about chi preferring the regexp route
// without stealing anything else. Nothing is recorded, so a correctly
// routed request is the DVR handler's own 404.
func TestDVRRoutes(t *testing.T) {
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

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	for _, path := range []string{
		"/live/cam/playlist_dvr.m3u8",
		"/live/cam/playlist_dvr_range-1793597867-60.m3u8",
		"/live/cam/playlist_dvr_timeshift-60.m3u8",
		"/live/cam/playlist_dvr_timeshift-60-30.m3u8",
		"/live/cam/manifest_dvr.mpd",
		"/live/cam/manifest_dvr_range-1793597867-60.mpd",
		"/dvr/live/cam/index.m3u8",
	} {
		rec := get(path)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "nothing recorded") {
			t.Errorf("GET %s = %d %q, want the DVR handler's 404", path, rec.Code, rec.Body.String())
		}
	}

	if rec := get("/live/cam/playlist_dvr_timeshift-7200.m3u8"); rec.Code != http.StatusNotFound ||
		!strings.Contains(rec.Body.String(), "beyond") {
		t.Errorf("timeshift past the window = %d %q", rec.Code, rec.Body.String())
	}

	for _, path := range []string{
		"/live/cam/dvr/1793597867000/init.mp4",
		"/live/cam/dvr/1793597867000/12_v.m4s",
	} {
		if rec := get(path); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "segment not found") {
			t.Errorf("GET %s = %d %q, want the DVR file handler's 404", path, rec.Code, rec.Body.String())
		}
	}

	// Ordinary names must still reach their own handlers, not the DVR one.
	for _, path := range []string{"/live/cam/playlist.m3u8", "/live/cam/seg000001.ts"} {
		if rec := get(path); strings.Contains(rec.Body.String(), "nothing recorded") {
			t.Errorf("GET %s was routed to the DVR handler", path)
		}
	}
}
