package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/dvr"
	"github.com/5centscdn/origin-mediaserver/internal/hls"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
	"github.com/5centscdn/origin-mediaserver/internal/scte35ctrl"
	"github.com/5centscdn/origin-mediaserver/internal/streamoutputs"
)

// TestStreamRoutes_TwoSegmentAppName is the regression test for a real
// production bug: /streams/{id}/kick|reload|dvr used to take a single
// "app/name" composite id, but chi v5 matches on RawPath and never
// unescapes the "%2F" a browser's encodeURIComponent("app/name") produces —
// so the handler always received the literal, un-decoded escape sequence
// and could never correctly split it back into app+name, making every one
// of these routes silently 404 ("stream not found") for any real stream.
// The fix uses two literal path segments instead; this proves each route
// resolves the right stream through the real router with no special
// encoding trick needed at all.
func TestStreamRoutes_TwoSegmentAppName(t *testing.T) {
	ctx := context.Background()

	database, err := db.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	registry := media.NewRegistry()
	m := metrics.New()
	scte35Mgr := scte35ctrl.NewManager(ctx, database, registry, m)

	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret-at-least-32-bytes-long!!"
	cfg.Server.DataDir = t.TempDir()

	dvrEngine := dvr.NewEngine(cfg, database, m)
	hlsPlaylists := hls.NewPlaylistRegistry()

	srv := New(ctx, cfg, database, registry, dvrEngine, nil, nil, hlsPlaylists, nil, nil, m, scte35Mgr)

	const app, name = "srt", "test"
	stream := &media.Stream{Key: media.StreamKey{App: app, Name: name}, Bus: media.NewBus()}
	if err := registry.Register(stream); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Reload needs the stream's output pipeline to have actually been
	// started (as it always is in production, via each ingest protocol's
	// OnPublish/OnLive callback) — Registry.Register alone doesn't do this.
	streamoutputs.Start(ctx, cfg, database, dvrEngine, hlsPlaylists, stream, m)
	t.Cleanup(stream.CancelOutputs)

	get := func(method, path string) *httptest.ResponseRecorder {
		t.Helper()
		token, err := srv.generateJWT("test-user", "admin", time.Hour)
		if err != nil {
			t.Fatalf("generateJWT: %v", err)
		}
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	if rec := get(http.MethodGet, "/api/v1/streams/"+app+"/"+name); rec.Code != http.StatusOK {
		t.Errorf("GET /streams/%s/%s = %d, want 200: %s", app, name, rec.Code, rec.Body.String())
	}

	// This stream has no DVR segments recorded, so the real, correctly-
	// routed response is a zero-valued info object for app/name "srt"/
	// "test" (from s.dvr.StreamInfo actually running with the right
	// app/name) — not the generic route-level failure a broken app/name
	// split would produce.
	if rec := get(http.MethodGet, "/api/v1/streams/"+app+"/"+name+"/dvr"); rec.Code != http.StatusOK {
		t.Errorf("GET .../dvr = %d %s, want 200", rec.Code, rec.Body.String())
	} else {
		var info dvr.StreamDVRInfo
		if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil ||
			info.App != app || info.Stream != name || info.SegmentCount != 0 ||
			info.URLs["hls"] != "/srt/test/playlist_dvr.m3u8" {
			t.Errorf("GET .../dvr = %s, want app=srt,stream=test with no segments", rec.Body.String())
		}
	}

	if rec := get(http.MethodPost, "/api/v1/streams/"+app+"/"+name+"/reload"); rec.Code != http.StatusNoContent {
		t.Errorf("POST .../reload = %d, want 204: %s", rec.Code, rec.Body.String())
	}

	// Re-register (Reload doesn't unregister) before testing Kick, which does.
	if got := registry.Get(media.StreamKey{App: app, Name: name}); got == nil {
		t.Fatal("stream vanished after Reload — Reload must not unregister it")
	}

	if rec := get(http.MethodPost, "/api/v1/streams/"+app+"/"+name+"/kick"); rec.Code != http.StatusNoContent {
		t.Errorf("POST .../kick = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if got := registry.Get(media.StreamKey{App: app, Name: name}); got != nil {
		t.Error("stream still registered after Kick, want it gone")
	}
}
