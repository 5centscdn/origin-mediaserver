package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestClusterSingleNode: with multinode off, /cluster reports just this
// node and enabled=false (the panel shows how to turn it on).
func TestClusterSingleNode(t *testing.T) {
	srv, _, _, _ := newTestSCTE35Server(t)
	srv.cfg.Server.NodeName = "origin-a"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authedRequest(t, srv, http.MethodGet, "/api/v1/cluster", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /cluster = %d %s", rec.Code, rec.Body.String())
	}
	var resp clusterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Enabled || len(resp.Nodes) != 1 || !resp.Nodes[0].Self || resp.Nodes[0].NodeName != "origin-a" {
		t.Errorf("single-node cluster = %+v", resp)
	}
}

// TestAddServerValidatesURL: the panel links to saved servers, so only
// http(s) base URLs are accepted, and a URL can't be added twice.
func TestAddServerValidatesURL(t *testing.T) {
	srv, _, _, _ := newTestSCTE35Server(t)
	add := func(u string) int {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, authedRequest(t, srv, http.MethodPost, "/api/v1/servers",
			map[string]string{"name": "origin-b", "url": u}))
		return rec.Code
	}
	for _, bad := range []string{"javascript:alert(1)", "ftp://x", "http://", "http://u:p@host:8080", "host:8080", "http://h/?a=1"} {
		if code := add(bad); code != http.StatusBadRequest {
			t.Errorf("add %q = %d, want 400", bad, code)
		}
	}
	if code := add("http://origin-b.example.com:8080/"); code != http.StatusCreated {
		t.Fatalf("add valid = %d", code)
	}
	if code := add("http://origin-b.example.com:8080"); code != http.StatusConflict {
		t.Errorf("duplicate add = %d, want 409", code)
	}
}
