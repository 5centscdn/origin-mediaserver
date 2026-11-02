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
	"github.com/google/uuid"
)

// TestCreateToken_ScopesArrayRoundTrips is the regression test for a real
// bug: the panel's "Create API token" dialog always failed with a generic
// "invalid JSON" error. The panel sends `scopes` as a real JSON array
// (`["read","write"]`, matching the APIToken.scopes: string[] type it
// renders elsewhere), but handleCreateToken's request struct declared
// `Scopes string` — decoding a JSON array into a Go string field fails,
// and that decode error was reported as the generic "invalid JSON"
// message, masking the real cause. This proves create (with a real scopes
// array) succeeds and list returns that same array back, not the
// JSON-encoded string form the DB actually stores it as.
func TestCreateToken_ScopesArrayRoundTrips(t *testing.T) {
	ctx := context.Background()

	database, err := db.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	userID := uuid.New().String()
	if _, err := database.ExecContext(ctx,
		`INSERT INTO users (id, email, password_hash, role) VALUES (?, ?, ?, ?)`,
		userID, "vineeth@5centscdn.com", "not-a-real-hash", "admin",
	); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	registry := media.NewRegistry()
	m := metrics.New()
	scte35Mgr := scte35ctrl.NewManager(ctx, database, registry, m)
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret-at-least-32-bytes-long!!"

	srv := New(ctx, cfg, database, registry, nil, nil, nil, nil, nil, nil, m, scte35Mgr)

	do := func(method, path string, body interface{}) *httptest.ResponseRecorder {
		t.Helper()
		token, err := srv.generateJWT(userID, "admin", time.Hour)
		if err != nil {
			t.Fatalf("generateJWT: %v", err)
		}
		var bodyBytes []byte
		if body != nil {
			bodyBytes, _ = json.Marshal(body)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	// Exactly what the panel's CreateTokenModal sends.
	createBody := map[string]interface{}{
		"name":   "vineeth",
		"scopes": []string{"read", "write", "stream"},
	}
	rec := do(http.MethodPost, "/api/v1/users/"+userID+"/tokens", createBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create token = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	var created struct {
		Scopes []string `json:"scopes"`
		Token  string   `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}
	if len(created.Scopes) != 3 || created.Scopes[0] != "read" {
		t.Errorf("create response scopes = %v, want [read write stream]", created.Scopes)
	}
	if created.Token == "" {
		t.Error("create response missing the raw token value")
	}

	// List must return the same real array, not the JSON-encoded string
	// the DB stores it as.
	rec = do(http.MethodGet, "/api/v1/users/"+userID+"/tokens", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list tokens = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var listed []struct {
		Scopes []string `json:"scopes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("unmarshal list response: %v", err)
	}
	if len(listed) != 1 || len(listed[0].Scopes) != 3 || listed[0].Scopes[0] != "read" {
		t.Errorf("listed token scopes = %+v, want one token with [read write stream]", listed)
	}
}
