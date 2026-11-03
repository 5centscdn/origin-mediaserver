package api

import (
	"bytes"
	"context"
	"encoding/json"
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

// A manual ABR group whose track is the pattern test%i lists every live
// matching stream, in numeric order, picking up renditions as they appear;
// a stream also named explicitly isn't listed twice.
func TestABRGroupPatternTrack(t *testing.T) {
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
	_, out := call("POST", "/api/v1/applications", map[string]any{"name": "vineeth", "allow_any_stream": false})
	json.Unmarshal(out, &created)
	call("POST", "/api/v1/applications/"+created.ID+"/streams", map[string]any{"name_pattern": "test%i", "is_wildcard": true})
	_, out = call("POST", "/api/v1/abr-groups", map[string]any{"name": "vineeth/test", "app_name": "vineeth", "stream_name": "test"})
	json.Unmarshal(out, &created)
	if code, out := call("PUT", "/api/v1/abr-groups/"+created.ID+"/tracks", map[string]any{"tracks": []any{
		map[string]any{"stream_name": "test%i", "bandwidth": 9000000, "codecs": "avc1.640028"},
		map[string]any{"stream_name": "test2"}, // also matched by the pattern
	}}); code != http.StatusNoContent {
		t.Fatalf("save pattern track = %d %s", code, out)
	}

	master := func() (int, string) {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/vineeth/test/playlist.m3u8", nil))
		return rec.Code, rec.Body.String()
	}
	if code, _ := master(); code != http.StatusServiceUnavailable {
		t.Fatalf("nothing live: master = %d, want 503", code)
	}
	publish := func(name string) {
		st := &media.Stream{Key: media.StreamKey{App: "vineeth", Name: name}, StartedAt: time.Now()}
		if err := registry.Register(st); err != nil {
			t.Fatal(err)
		}
	}
	publish("test10")
	publish("test1")
	publish("testx") // not test%i
	publish("test2")

	code, body := master()
	if code != http.StatusOK {
		t.Fatalf("master = %d %q", code, body)
	}
	var uris []string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "vineeth/") {
			uris = append(uris, l)
		}
	}
	want := []string{"vineeth/test1/chunks.m3u8", "vineeth/test2/chunks.m3u8", "vineeth/test10/chunks.m3u8"}
	if strings.Join(uris, " ") != strings.Join(want, " ") {
		t.Errorf("variants = %v, want %v\n%s", uris, want, body)
	}
	if strings.Contains(body, "9000000") {
		t.Errorf("pattern track's bandwidth applied to every rendition:\n%s", body)
	}
	if !strings.Contains(body, `CODECS="avc1.640028"`) || !strings.Contains(body, `NAME="test10"`) {
		t.Errorf("codecs/name not carried to matches:\n%s", body)
	}

	// A rendition added in the encoder appears without editing the group.
	publish("test3")
	if _, body := master(); !strings.Contains(body, "vineeth/test3/chunks.m3u8") {
		t.Errorf("new rendition test3 not picked up:\n%s", body)
	}
}

func TestNaturalLess(t *testing.T) {
	for _, c := range [][2]string{{"test2", "test10"}, {"test1", "test2"}, {"a", "b"}, {"cam_9", "cam_10"}} {
		if !naturalLess(c[0], c[1]) || naturalLess(c[1], c[0]) {
			t.Errorf("naturalLess(%q, %q) wrong", c[0], c[1])
		}
	}
}
