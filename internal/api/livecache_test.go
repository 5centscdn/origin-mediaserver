package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/livestore"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
	"github.com/5centscdn/origin-mediaserver/internal/scte35ctrl"
)

// The Settings page's live cache Edit: PUT applies the mode at once (disk
// writes new segments to disk), saves it for the next start, rejects bad
// values, and needs operator or above.
func TestLiveCacheConfig(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	m := metrics.New()
	registry := media.NewRegistry()
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret-at-least-32-bytes-long!!"
	srv := New(ctx, cfg, database, registry, nil, nil, nil, nil, nil, nil, m,
		scte35ctrl.NewManager(ctx, database, registry, m))
	t.Cleanup(func() { livestore.Default().Configure("hybrid", 90, 80) })

	put := func(role string, body any) *httptest.ResponseRecorder {
		t.Helper()
		token, _ := srv.generateJWT("u", role, time.Hour)
		var buf bytes.Buffer
		json.NewEncoder(&buf).Encode(body)
		req := httptest.NewRequest(http.MethodPut, "/api/v1/live-cache-config", &buf)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	for _, bad := range []LiveCacheSetting{
		{Mode: "ssd", MemoryHighPct: 90, MemoryLowPct: 80},
		{Mode: "hybrid", MemoryHighPct: 100, MemoryLowPct: 80},
		{Mode: "hybrid", MemoryHighPct: 80, MemoryLowPct: 80},
	} {
		if rec := put("operator", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%+v: %d, want 400", bad, rec.Code)
		}
	}
	if rec := put("viewer", LiveCacheSetting{Mode: "disk", MemoryHighPct: 90, MemoryLowPct: 80}); rec.Code != http.StatusForbidden {
		t.Errorf("viewer: %d, want 403", rec.Code)
	}

	rec := put("operator", LiveCacheSetting{Mode: "disk", MemoryHighPct: 85, MemoryLowPct: 70})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT disk: %d %s", rec.Code, rec.Body)
	}
	var st livestore.Status
	json.Unmarshal(rec.Body.Bytes(), &st)
	if st.Policy != "disk" || st.Mode != "disk" || st.HighPct != 85 || st.LowPct != 70 {
		t.Errorf("status after PUT: %+v", st)
	}
	path := filepath.Join(t.TempDir(), "hls", "a", "s", "seg000001.ts")
	livestore.Default().WriteFile(path, []byte("x"))
	if _, _, df, _ := livestore.Default().Stats(); df == 0 {
		t.Error("disk mode applied without a restart didn't write to disk")
	}
	livestore.Default().Remove(path)

	var saved LiveCacheSetting
	if ok, err := database.GetSetting(ctx, LiveCacheSettingKey, &saved); !ok || err != nil || saved.Mode != "disk" || saved.MemoryHighPct != 85 {
		t.Errorf("saved setting = %+v, %v, %v", saved, ok, err)
	}
}
