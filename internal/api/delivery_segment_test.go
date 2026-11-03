package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/livestore"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
	"github.com/5centscdn/origin-mediaserver/internal/scte35ctrl"
)

// TestHandleDirectSegment_DisambiguatesHLSAndDASHByDirectory is the
// regression test for a real risk introduced by removing DASH's own /dash
// URL prefix (both now share the exact /{app}/{stream}/{segment} route
// shape): fmp4/cmaf-container HLS and DASH can both produce .m4s/init.mp4
// segment filenames, so the shared handler can't tell them apart by
// extension alone — it must actually check which on-disk directory (hls/
// vs dash/) has the file. This writes one real file into each directory
// under the same app/stream and confirms each request resolves to the
// correct one.
func TestHandleDirectSegment_DisambiguatesHLSAndDASHByDirectory(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	registry := media.NewRegistry()
	m := metrics.New()
	scte35Mgr := scte35ctrl.NewManager(ctx, database, registry, m)

	dataDir := t.TempDir()
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret-at-least-32-bytes-long!!"
	cfg.Server.DataDir = dataDir

	srv := New(ctx, cfg, database, registry, nil, nil, nil, nil, nil, nil, m, scte35Mgr)

	hlsDir := filepath.Join(dataDir, "hls", "app1", "s1")
	dashDir := filepath.Join(dataDir, "dash", "app1", "s1")
	// Same filename shape (init.mp4) in both directories, on purpose — the
	// point is the handler must not be fooled by the shared extension.
	livestore.Default().WriteFile(filepath.Join(hlsDir, "init.mp4"), []byte("HLS-INIT-BYTES"))
	livestore.Default().WriteFile(filepath.Join(dashDir, "init.mp4"), []byte("DASH-INIT-BYTES"))
	livestore.Default().WriteFile(filepath.Join(dashDir, "seg-video-1.m4s"), []byte("DASH-SEG-BYTES"))

	get := func(path string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200: %s", path, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	// Only the DASH directory has this file — must resolve there.
	if got := get("/app1/s1/seg-video-1.m4s"); got != "DASH-SEG-BYTES" {
		t.Errorf("GET .../seg-video-1.m4s = %q, want DASH-SEG-BYTES", got)
	}

	// Both directories have init.mp4 — HLS must win (it's checked first;
	// this is the same file HLS's own fmp4 init segment would be at).
	if got := get("/app1/s1/init.mp4"); got != "HLS-INIT-BYTES" {
		t.Errorf("GET .../init.mp4 = %q, want HLS-INIT-BYTES (HLS directory checked first)", got)
	}
}

// TestDASHManifest_ServedWithNoPathPrefix confirms /{app}/{stream}/
// manifest.mpd (no /dash/ prefix) resolves to the DASH manifest a
// dash.Packager wrote to disk — the actual URL shape requested per this
// session's "avoid dash prefix" change.
func TestDASHManifest_ServedWithNoPathPrefix(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	registry := media.NewRegistry()
	m := metrics.New()
	scte35Mgr := scte35ctrl.NewManager(ctx, database, registry, m)

	dataDir := t.TempDir()
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret-at-least-32-bytes-long!!"
	cfg.Server.DataDir = dataDir

	srv := New(ctx, cfg, database, registry, nil, nil, nil, nil, nil, nil, m, scte35Mgr)

	dashDir := filepath.Join(dataDir, "dash", "app1", "s1")
	livestore.Default().WriteFile(filepath.Join(dashDir, "manifest.mpd"), []byte("<MPD/>"))

	req := httptest.NewRequest(http.MethodGet, "/app1/s1/manifest.mpd", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /app1/s1/manifest.mpd = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "<MPD/>" {
		t.Errorf("body = %q, want <MPD/>", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/dash+xml" {
		t.Errorf("Content-Type = %q, want application/dash+xml", ct)
	}

	// The old /dash/-prefixed URL must no longer resolve to the manifest
	// (it now falls through to the panel catch-all, which 404s for an
	// unknown static asset path).
	req2 := httptest.NewRequest(http.MethodGet, "/dash/app1/s1/manifest.mpd", nil)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusOK && rec2.Body.String() == "<MPD/>" {
		t.Error("/dash/-prefixed URL still serves the manifest — prefix should be gone")
	}
}

// TestMpegTS_RouteReachesLiveTSHandler confirms /{app}/{stream}/mpeg.2ts
// (renamed from stream.ts to match the real-world convention other origins,
// e.g. Nimble, already use) actually routes to handleLiveTS rather than
// falling through to the panel catch-all or some other handler — checked
// via handleLiveTS's own "stream not found" response for a stream that was
// never registered, which is a distinctive enough body/status pair that no
// other handler in this router would produce for this path shape.
func TestMpegTS_RouteReachesLiveTSHandler(t *testing.T) {
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

	srv := New(ctx, cfg, database, registry, nil, nil, nil, nil, nil, nil, m, scte35Mgr)

	req := httptest.NewRequest(http.MethodGet, "/app1/nosuchstream/mpeg.2ts", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || rec.Body.String() != "stream not found\n" {
		t.Errorf("GET .../mpeg.2ts for an unregistered stream = %d %q, want 404 \"stream not found\"", rec.Code, rec.Body.String())
	}
}

// TestSegmentContentType_MatchesTrackType is a regression test for a real
// conformance gap: DASH's separately-packaged tracks (internal/dash/
// packager.go's seg-video-/seg-audio-/seg-text-/init-video/init-audio/
// init-text naming) were served with a made-up, non-registered
// "video/iso.segment" Content-Type that didn't match the MPD's own declared
// mimeType per track ("video/mp4"/"audio/mp4"/"application/mp4") — a real
// DVB-DASH validator flagged this as a conformance failure.
func TestSegmentContentType_MatchesTrackType(t *testing.T) {
	cases := map[string]string{
		"seg-video-12.m4s": "video/mp4",
		"init-video.mp4":   "video/mp4",
		"seg-audio-12.m4s": "audio/mp4",
		"init-audio.mp4":   "audio/mp4",
		"seg-text-12.m4s":  "application/mp4",
		"init-text.mp4":    "application/mp4",
		// HLS's combined-file fMP4 segments (video+audio muxed together)
		// carry no track name — video/mp4 is the conventional generic type.
		"seg000012.m4s": "video/mp4",
		"segment.ts":    "video/MP2T",
	}
	for name, want := range cases {
		if got := segmentContentType(name); got != want {
			t.Errorf("segmentContentType(%q) = %q, want %q", name, got, want)
		}
	}
}
