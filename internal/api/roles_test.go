package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
	"github.com/5centscdn/origin-mediaserver/internal/scte35ctrl"
)

// TestRoles is the regression test for requireRole existing but never being
// wired into the router: any authenticated user — a viewer included — could
// create users, promote itself to admin and mint tokens for other users.
func TestRoles(t *testing.T) {
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
	cfg.DVR.WindowSeconds = 3600
	srv := New(ctx, cfg, database, registry, nil, nil, nil, nil, nil, nil, m,
		scte35ctrl.NewManager(ctx, database, registry, m))

	do := func(userID, role, method, path string, body any) int {
		t.Helper()
		token, err := srv.generateJWT(userID, role, time.Hour)
		if err != nil {
			t.Fatalf("generateJWT: %v", err)
		}
		var buf bytes.Buffer
		if body != nil {
			json.NewEncoder(&buf).Encode(body)
		}
		req := httptest.NewRequest(method, path, &buf)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	newApp := func(name string) map[string]any { return map[string]any{"name": name} }
	newUser := func(email string) map[string]any {
		return map[string]any{"email": email, "password": "pw-123456789", "role": "admin"}
	}

	cases := []struct {
		user, role, method, path string
		body                     any
		want                     int
	}{
		// Reads: any role.
		{"v", "viewer", "GET", "/api/v1/applications", nil, 200},
		{"v", "viewer", "GET", "/api/v1/dvr-config", nil, 200},
		// Changes: operator and up.
		{"v", "viewer", "POST", "/api/v1/applications", newApp("a1"), 403},
		{"o", "operator", "POST", "/api/v1/applications", newApp("a2"), 201},
		{"v", "viewer", "PUT", "/api/v1/dvr-config", map[string]any{"enabled": true}, 403},
		// Saved server list: admin only (no panel page; kept for clustering).
		{"v", "viewer", "GET", "/api/v1/servers", nil, 403},
		{"o", "operator", "POST", "/api/v1/servers", map[string]any{"name": "x", "url": "http://x"}, 403},
		{"a", "admin", "GET", "/api/v1/servers", nil, 200},
		// Users: admin only — including a viewer editing its own record.
		{"v", "viewer", "GET", "/api/v1/users", nil, 403},
		{"v", "viewer", "POST", "/api/v1/users", newUser("x@example.com"), 403},
		{"v", "viewer", "PUT", "/api/v1/users/v", map[string]any{"role": "admin"}, 403},
		{"o", "operator", "POST", "/api/v1/users", newUser("y@example.com"), 403},
		{"a", "admin", "POST", "/api/v1/users", newUser("z@example.com"), 201},
		// Tokens: own, or admin.
		{"v", "viewer", "GET", "/api/v1/users/v/tokens", nil, 200},
		{"v", "viewer", "GET", "/api/v1/users/o/tokens", nil, 403},
		{"o", "operator", "POST", "/api/v1/users/a/tokens", map[string]any{"name": "t"}, 403},
		{"a", "admin", "GET", "/api/v1/users/v/tokens", nil, 200},
		// Unknown/missing role gets nothing.
		{"n", "", "GET", "/api/v1/applications", nil, 403},
	}
	for _, c := range cases {
		if got := do(c.user, c.role, c.method, c.path, c.body); got != c.want {
			t.Errorf("%s (%s) %s %s = %d, want %d", c.user, c.role, c.method, c.path, got, c.want)
		}
	}
}

// TestUpdateDVRConfig_Window checks window_seconds: a missing field leaves
// the window alone, and an explicit non-positive value is rejected rather
// than reported as saved.
func TestUpdateDVRConfig_Window(t *testing.T) {
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
	cfg.DVR.WindowSeconds = 3600
	srv := New(ctx, cfg, database, registry, nil, nil, nil, nil, nil, nil, m,
		scte35ctrl.NewManager(ctx, database, registry, m))
	token, _ := srv.generateJWT("a", "admin", time.Hour)

	put := func(body string) int {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/dvr-config", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	if got := put(`{"window_seconds":0}`); got != http.StatusBadRequest {
		t.Errorf("window_seconds 0 = %d, want 400", got)
	}
	if got := put(`{"enabled":true}`); got != http.StatusNoContent || cfg.DVRWindowSeconds() != 3600 || !cfg.DVREnabled() {
		t.Errorf("enabled only = %d, window %d enabled %v; want 204, 3600, true", got, cfg.DVRWindowSeconds(), cfg.DVREnabled())
	}
	if got := put(`{"window_seconds":120}`); got != http.StatusNoContent || cfg.DVRWindowSeconds() != 120 {
		t.Errorf("window_seconds 120 = %d, window %d; want 204, 120", got, cfg.DVRWindowSeconds())
	}
}
