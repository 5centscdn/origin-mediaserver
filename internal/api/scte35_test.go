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

// newTestSCTE35Server builds a real *Server with only the dependencies the
// SCTE-35 routes actually touch (registry, db, scte35ctrl.Manager) — every
// other manager is nil, which is safe here since Handler() only registers
// handler values at construction time and this test never hits any other
// route (confirmed: router.go's route registration never dereferences
// dvr/failover/pull/hlsPlaylists/webrtcMgr/nodeMgr).
func newTestSCTE35Server(t *testing.T) (*Server, *media.Registry, *db.DB, context.Context) {
	t.Helper()
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

	srv := New(ctx, cfg, database, registry, nil, nil, nil, nil, nil, nil, m, scte35Mgr)
	return srv, registry, database, ctx
}

// authedRequest builds an HTTP request carrying a validly-signed bearer
// JWT — authMiddleware's bearer path only checks the JWT's signature/expiry
// against cfg.Auth.JWTSecret, never the DB, so minting one directly via
// generateJWT (bypassing the login HTTP round trip) is sufficient and
// matches how the middleware actually validates it.
func authedRequest(t *testing.T, srv *Server, method, path string, body interface{}) *http.Request {
	t.Helper()
	token, err := srv.generateJWT("test-user", "admin", time.Hour)
	if err != nil {
		t.Fatalf("generateJWT: %v", err)
	}

	var bodyReader *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		bodyReader = bytes.NewReader(data)
	} else {
		bodyReader = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, path, bodyReader)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func setTestAppMode(t *testing.T, database *db.DB, ctx context.Context, app, mode string) {
	t.Helper()
	if _, err := database.ExecContext(ctx,
		`INSERT INTO applications (id, name, scte35_mode) VALUES (?, ?, ?)`, app, app, mode,
	); err != nil {
		t.Fatalf("insert application: %v", err)
	}
}

func registerLiveStream(t *testing.T, registry *media.Registry, app, name string) *media.Stream {
	t.Helper()
	s := &media.Stream{Key: media.StreamKey{App: app, Name: name}, Bus: media.NewBus()}
	if err := registry.Register(s); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return s
}

// TestHandleInsertSCTE35_Out_Success exercises the full black-box path:
// POST an OUT armed for start:"next_keyframe" (avoids needing the
// stream's PTS watcher to have already observed video — see
// scte35ctrl's own tests for "immediate" trigger coverage), then confirms
// it actually fires — a real CodecSCTE35 packet appears on the stream's
// bus — once a keyframe arrives.
func TestHandleInsertSCTE35_Out_Success(t *testing.T) {
	srv, registry, database, ctx := newTestSCTE35Server(t)
	setTestAppMode(t, database, ctx, "app1", "insert")
	stream := registerLiveStream(t, registry, "app1", "s1")

	// No "duration" here deliberately: a nonzero duration would arm a real
	// auto-return timer (see scte35ctrl.Manager.scheduleAutoReturn) that
	// would still be pending — and would then log a (harmless in
	// production, noisy in a test run) error trying to reach this test's
	// already-closed temp DB — well after this test function returns.
	// Auto-return timing itself is covered by internal/scte35ctrl's own
	// tests with a short duration.
	req := authedRequest(t, srv, http.MethodPost, "/api/v1/streams/app1/s1/scte35", map[string]interface{}{
		"action": "out", "event_id": 1, "command": "splice_insert", "start": "next_keyframe",
	})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["status"] != "pending" {
		t.Errorf("response status = %v, want pending (armed for next_keyframe)", resp["status"])
	}

	sub := stream.Bus.Subscribe(ctx)
	defer sub.Close()

	// Retry-publish the keyframe: the Manager's own internal watcher
	// subscribed asynchronously (in a goroutine spawned during the POST
	// above), so the first keyframe published here might arrive before
	// that subscription completes and be silently dropped, per
	// media.Bus.Publish's documented non-blocking/no-late-subscriber
	// semantics — retrying is the same pattern internal/scte35ctrl's own
	// tests use for this exact race.
	deadline := time.Now().Add(2 * time.Second)
	var gotSCTE35 *media.Packet
	for gotSCTE35 == nil && time.Now().Before(deadline) {
		stream.Bus.Publish(&media.Packet{Type: media.PacketMedia, Codec: media.CodecH264, PTS: 5000, DTS: 5000, IsKeyframe: true})
		select {
		case pkt := <-sub.Chan():
			if pkt.Codec == media.CodecSCTE35 {
				gotSCTE35 = pkt
			}
		case <-time.After(10 * time.Millisecond):
		}
	}
	if gotSCTE35 == nil {
		t.Fatal("never received the armed SCTE-35 OUT packet after publishing keyframes")
	}
	if !gotSCTE35.SCTE35.OutOfNetwork {
		t.Error("OutOfNetwork = false, want true")
	}

	// Poll until the DB row reflects "active" — the packet above is
	// published before the Manager's own status-update write, so without
	// this the test could return (closing the DB via t.Cleanup) while
	// that write is still in flight, logging a harmless but confusing
	// "database is closed" warning from the watcher's goroutine.
	deadline = time.Now().Add(2 * time.Second)
	for {
		ev, err := database.GetLatestSCTE35Event(ctx, "app1", "s1", 1)
		if err != nil {
			t.Fatalf("GetLatestSCTE35Event: %v", err)
		}
		if ev != nil && ev.Status == "active" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("event status never reached active (got %+v)", ev)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHandleInsertSCTE35_RequiresAuth(t *testing.T) {
	srv, _, database, ctx := newTestSCTE35Server(t)
	setTestAppMode(t, database, ctx, "app1", "insert")

	body, _ := json.Marshal(map[string]interface{}{"action": "out", "event_id": 1, "command": "splice_insert"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/streams/app1/s1/scte35", bytes.NewReader(body))
	// No Authorization header.
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandleInsertSCTE35_InvalidJSON(t *testing.T) {
	srv, _, _, _ := newTestSCTE35Server(t)
	token, _ := srv.generateJWT("u", "admin", time.Hour)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/streams/app1/s1/scte35", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleInsertSCTE35_UnknownAction(t *testing.T) {
	srv, registry, database, ctx := newTestSCTE35Server(t)
	setTestAppMode(t, database, ctx, "app1", "insert")
	registerLiveStream(t, registry, "app1", "s1")

	req := authedRequest(t, srv, http.MethodPost, "/api/v1/streams/app1/s1/scte35", map[string]interface{}{"action": "sideways"})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleInsertSCTE35_StreamNotLive(t *testing.T) {
	srv, _, database, ctx := newTestSCTE35Server(t)
	setTestAppMode(t, database, ctx, "app1", "insert")
	// No stream registered.

	req := authedRequest(t, srv, http.MethodPost, "/api/v1/streams/app1/ghost/scte35", map[string]interface{}{
		"action": "out", "event_id": 1, "command": "splice_insert",
	})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleInsertSCTE35_ModeDisallows(t *testing.T) {
	srv, registry, database, ctx := newTestSCTE35Server(t)
	setTestAppMode(t, database, ctx, "app1", "passthrough")
	registerLiveStream(t, registry, "app1", "s1")

	req := authedRequest(t, srv, http.MethodPost, "/api/v1/streams/app1/s1/scte35", map[string]interface{}{
		"action": "out", "event_id": 1, "command": "splice_insert",
	})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleInsertSCTE35_Duplicate(t *testing.T) {
	srv, registry, database, ctx := newTestSCTE35Server(t)
	setTestAppMode(t, database, ctx, "app1", "insert")
	registerLiveStream(t, registry, "app1", "s1")

	body := map[string]interface{}{"action": "out", "event_id": 1, "command": "splice_insert", "start": "next_keyframe"}
	rec1 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec1, authedRequest(t, srv, http.MethodPost, "/api/v1/streams/app1/s1/scte35", body))
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first insert status = %d, want 201, body: %s", rec1.Code, rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, authedRequest(t, srv, http.MethodPost, "/api/v1/streams/app1/s1/scte35", body))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("duplicate insert status = %d, want 409, body: %s", rec2.Code, rec2.Body.String())
	}
}

func TestHandleListAndCancelSCTE35(t *testing.T) {
	srv, registry, database, ctx := newTestSCTE35Server(t)
	setTestAppMode(t, database, ctx, "app1", "insert")
	registerLiveStream(t, registry, "app1", "s1")

	// A next_keyframe-triggered event stays "pending" (no keyframe follows
	// in this test), so it's both listable and cancellable.
	insertReq := authedRequest(t, srv, http.MethodPost, "/api/v1/streams/app1/s1/scte35", map[string]interface{}{
		"action": "out", "event_id": 42, "command": "splice_insert", "start": "next_keyframe",
	})
	insertRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(insertRec, insertReq)
	if insertRec.Code != http.StatusCreated {
		t.Fatalf("insert status = %d, want 201, body: %s", insertRec.Code, insertRec.Body.String())
	}

	listRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(listRec, authedRequest(t, srv, http.MethodGet, "/api/v1/streams/app1/s1/scte35", nil))
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200, body: %s", listRec.Code, listRec.Body.String())
	}
	var list []map[string]interface{}
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list) != 1 || list[0]["status"] != "pending" {
		t.Fatalf("list = %+v, want one pending event", list)
	}

	cancelRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(cancelRec, authedRequest(t, srv, http.MethodDelete, "/api/v1/streams/app1/s1/scte35/42", nil))
	if cancelRec.Code != http.StatusNoContent {
		t.Fatalf("cancel status = %d, want 204, body: %s", cancelRec.Code, cancelRec.Body.String())
	}

	// Cancelling again (already cancelled, not pending) must 409.
	cancelAgainRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(cancelAgainRec, authedRequest(t, srv, http.MethodDelete, "/api/v1/streams/app1/s1/scte35/42", nil))
	if cancelAgainRec.Code != http.StatusConflict {
		t.Fatalf("re-cancel status = %d, want 409, body: %s", cancelAgainRec.Code, cancelAgainRec.Body.String())
	}
}
