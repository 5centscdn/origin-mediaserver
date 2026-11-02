package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
	"github.com/5centscdn/origin-mediaserver/internal/scte35ctrl"
)

// TestABRGroupTracks covers the DVR-server-style ABR editor end to end:
// tracks are validated against the app's stream configs, saved in order,
// and the master only lists live tracks, with auto fields filled from the
// live stream and 503 once every track is offline.
func TestABRGroupTracks(t *testing.T) {
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
	srv := New(ctx, cfg, database, registry, nil, nil, nil, nil, nil, nil, m,
		scte35ctrl.NewManager(ctx, database, registry, m))
	token, _ := srv.generateJWT("a", "admin", time.Hour)

	call := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			json.NewEncoder(&buf).Encode(body)
		}
		req := httptest.NewRequest(method, path, &buf)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		out, _ := io.ReadAll(rec.Body)
		return rec.Code, out
	}
	var created struct{ ID string }
	code, out := call("POST", "/api/v1/applications", map[string]any{"name": "vineeth", "allow_any_stream": false})
	if code != http.StatusCreated {
		t.Fatalf("create app = %d %s", code, out)
	}
	json.Unmarshal(out, &created)
	if code, out = call("POST", "/api/v1/applications/"+created.ID+"/streams",
		map[string]any{"name_pattern": "test%i", "is_wildcard": true}); code != http.StatusCreated {
		t.Fatalf("create stream config = %d %s", code, out)
	}
	code, out = call("POST", "/api/v1/abr-groups",
		map[string]any{"name": "vineeth/abr", "app_name": "vineeth", "stream_name": "abr"})
	if code != http.StatusCreated {
		t.Fatalf("create group = %d %s", code, out)
	}
	json.Unmarshal(out, &created)
	tracksURL := "/api/v1/abr-groups/" + created.ID + "/tracks"

	for _, bad := range []map[string]any{
		{"stream_name": "other"},                       // not configured in the app
		{"stream_name": "vineeth/test1"},               // app-qualified
		{"stream_name": "test1", "resolution": "720p"}, // not WxH
		{"stream_name": "test1", "bandwidth": -1},
	} {
		if code, out := call("PUT", tracksURL, map[string]any{"tracks": []any{bad}}); code != http.StatusBadRequest {
			t.Errorf("tracks %v = %d %s, want 400", bad, code, out)
		}
	}
	if code, out := call("PUT", tracksURL, map[string]any{"tracks": []any{
		map[string]any{"stream_name": "test1", "bandwidth": 2000000, "avg_bandwidth": 1500000},
		map[string]any{"stream_name": "test2", "resolution": "640x360", "codecs": "avc1.42c01e,mp4a.40.2"},
		map[string]any{"stream_name": "test3", "audio_only": true},
	}}); code != http.StatusNoContent {
		t.Fatalf("save tracks = %d %s", code, out)
	}

	master := func() (int, string) {
		req := httptest.NewRequest("GET", "/vineeth/abr/playlist.m3u8", nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, body := master(); code != http.StatusServiceUnavailable {
		t.Fatalf("master with no live tracks = %d %q, want 503", code, body)
	}

	publish := func(name string) *media.Stream {
		st := &media.Stream{Key: media.StreamKey{App: "vineeth", Name: name}, StartedAt: time.Now().Add(-10 * time.Second)}
		if err := registry.Register(st); err != nil {
			t.Fatal(err)
		}
		st.Bus.SetCodecInfo(&media.CodecInfo{
			VideoCodec: media.CodecH264, AudioCodec: media.CodecAAC,
			Width: 1280, Height: 720,
			SPSNALu: []byte{0x67, 0x64, 0x00, 0x1f},
		})
		st.Bus.AddBytesIn(1000000 / 8 * 10) // 1 Mbps for 10 s
		return st
	}
	publish("test1")
	publish("test2")

	code, body := master()
	if code != http.StatusOK {
		t.Fatalf("master = %d %q", code, body)
	}
	for _, want := range []string{
		"BANDWIDTH=2000000,AVERAGE-BANDWIDTH=1500000,RESOLUTION=1280x720,CODECS=\"avc1.",
		"vineeth/test1/chunks.m3u8",
		"RESOLUTION=640x360,CODECS=\"avc1.42c01e,mp4a.40.2\"",
		"vineeth/test2/chunks.m3u8",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("master missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "test3") {
		t.Errorf("offline track test3 listed:\n%s", body)
	}
	if strings.Index(body, "test1/") > strings.Index(body, "test2/") {
		t.Errorf("tracks out of saved order:\n%s", body)
	}
	// test2 has bandwidth auto: measured ~1 Mbps plus 20% headroom.
	var bw int
	if i := strings.Index(body, "RESOLUTION=640x360"); i < 0 {
		t.Errorf("test2 missing:\n%s", body)
	} else {
		fmt.Sscanf(body[strings.LastIndex(body[:i], "BANDWIDTH="):i], "BANDWIDTH=%d", &bw)
		if bw < 1150000 || bw > 1210000 {
			t.Errorf("auto bandwidth = %d, want ~1200000:\n%s", bw, body)
		}
	}
}
