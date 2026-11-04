package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/dvr"
	"github.com/5centscdn/origin-mediaserver/internal/dvrowner"
	"github.com/5centscdn/origin-mediaserver/internal/hls"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
	"github.com/5centscdn/origin-mediaserver/internal/node"
	"github.com/5centscdn/origin-mediaserver/internal/scte35ctrl"
	"github.com/5centscdn/origin-mediaserver/internal/streamoutputs"
)

// gateCluster is a fixed cluster view: this node is origin-a, peers as set.
type gateCluster struct{ peers []node.PeerDVR }

func (g *gateCluster) NodeID() string                       { return "id-a" }
func (g *gateCluster) NodeName() string                     { return "origin-a" }
func (g *gateCluster) Priority() int                        { return 100 }
func (g *gateCluster) Ready() bool                          { return true }
func (g *gateCluster) Kick()                                {}
func (g *gateCluster) DVRView(app, s string) []node.PeerDVR { return g.peers }

// newOwnershipServer builds a server whose app "live" has nDVR on (via the
// application config, resolved exactly as streamoutputs does) and app
// "plain" has it off.
func newOwnershipServer(t *testing.T) (*Server, *media.Registry, *dvrowner.Tracker, *gateCluster) {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.ExecContext(ctx, `INSERT INTO applications (id, name, out_hls, dvr_enabled)
		VALUES ('a1', 'live', 1, 1), ('a2', 'plain', 1, 0)`); err != nil {
		t.Fatal(err)
	}

	registry := media.NewRegistry()
	m := metrics.New()
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret-at-least-32-bytes-long!!"
	cfg.Server.DataDir = t.TempDir()
	cfg.DVR.WindowSeconds = 3600
	engine := dvr.NewEngine(cfg, database, m)
	srv := New(ctx, cfg, database, registry, engine, nil, nil,
		hls.NewPlaylistRegistry(), nil, nil, m, scte35ctrl.NewManager(ctx, database, registry, m))

	cl := &gateCluster{}
	tr := dvrowner.New(cl, dvrowner.NewDeps(registry, engine, func(ctx context.Context, app, stream string) bool {
		return streamoutputs.DVREnabled(ctx, cfg, database, app, stream)
	}), dvrowner.PolicyFirstPublisher)
	srv.SetDVROwner(tr)
	return srv, registry, tr, cl
}

func peerState(name string, fresh, publishing bool, dvrEnd int64) node.PeerDVR {
	p := node.PeerDVR{NodeID: "id-" + name, NodeName: name, Priority: 100, Fresh: fresh, Known: true}
	p.State = node.DVRState{Publishing: publishing}
	if publishing {
		p.State.PublishSinceMS = time.Now().Add(-time.Minute).UnixMilli()
	}
	if dvrEnd > 0 {
		p.State.DVRStartMS, p.State.DVREndMS = dvrEnd-3600_000, dvrEnd
	}
	return p
}

func gateGet(srv *Server, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

var ownedPlaylists = []string{
	"/live/cam/playlist.m3u8",
	"/live/cam/chunks.m3u8",
	"/live/cam/manifest.mpd",
	"/live/cam/audio/index.m3u8",
	"/live/cam/playlist_dvr.m3u8",
	"/live/cam/manifest_dvr.mpd",
	"/live/cam/playlist_dvr_timeshift-60.m3u8",
	"/dvr/live/cam/index.m3u8",
	"/grp/abr/live/cam/chunks.m3u8",
	"/grp/abr/live/cam/playlist_dvr.m3u8",
}

// Cases 2/4 and 16: a known non-owner answers every live-edge playlist
// with an uncacheable 404 naming the owner, so the CDN retries the next
// origin; live segments too (17, origin affinity).
func TestGateNotOwner(t *testing.T) {
	srv, _, tr, cl := newOwnershipServer(t)
	now := time.Now().UnixMilli()
	tr.SegmentIndexed(media.StreamKey{App: "live", Name: "cam"}, now-60_000, now-30_000) // stale local DVR
	cl.peers = []node.PeerDVR{peerState("origin-b", true, true, now)}

	for _, path := range append(ownedPlaylists, "/live/cam/seg000001.ts", "/live/cam/audio/seg1.m4s", "/grp/abr/live/cam/seg1.ts") {
		rec := gateGet(srv, path)
		if rec.Code != http.StatusNotFound || rec.Header().Get("Cache-Control") != "no-store" ||
			rec.Header().Get("X-Origin-Owner") != "origin-b" || rec.Header().Get("X-Origin-Ownership") != "not_owner" {
			t.Errorf("GET %s = %d cc=%q owner=%q, want 404 no-store owner origin-b",
				path, rec.Code, rec.Header().Get("Cache-Control"), rec.Header().Get("X-Origin-Owner"))
		}
	}
}

// Case 12/13: ownership that depends on an unreachable peer is 503 for
// playlists (never a guess), while existing segments aren't blocked.
func TestGateUnknown(t *testing.T) {
	srv, _, tr, cl := newOwnershipServer(t)
	now := time.Now().UnixMilli()
	tr.SegmentIndexed(media.StreamKey{App: "live", Name: "cam"}, now-60_000, now-30_000)
	cl.peers = []node.PeerDVR{peerState("origin-b", false, false, now-120_000)}

	for _, path := range ownedPlaylists {
		rec := gateGet(srv, path)
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Cache-Control") != "no-store" ||
			rec.Header().Get("Retry-After") == "" {
			t.Errorf("GET %s = %d cc=%q, want 503 no-store with Retry-After", path, rec.Code, rec.Header().Get("Cache-Control"))
		}
	}
	if rec := gateGet(srv, "/live/cam/seg000001.ts"); rec.Header().Get("X-Origin-Ownership") != "" {
		t.Errorf("segment blocked while ownership unknown: %d %q", rec.Code, rec.Body.String())
	}
}

// Cases 2/3: the publishing node serves.
func TestGateOwnerServes(t *testing.T) {
	srv, registry, _, cl := newOwnershipServer(t)
	now := time.Now().UnixMilli()
	cl.peers = []node.PeerDVR{peerState("origin-b", true, false, now)} // B has newer DVR but isn't live
	st := &media.Stream{Key: media.StreamKey{App: "live", Name: "cam"}, StartedAt: time.Now()}
	if err := registry.Register(st); err != nil {
		t.Fatal(err)
	}
	defer registry.Unregister(st)

	rec := gateGet(srv, "/live/cam/playlist.m3u8")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "chunks.m3u8") {
		t.Errorf("owner master = %d %q", rec.Code, rec.Body.String())
	}
	if rec := gateGet(srv, "/live/cam/playlist_dvr.m3u8"); rec.Header().Get("X-Origin-Ownership") != "" {
		t.Errorf("owner's DVR playlist was gated: %d", rec.Code)
	}
}

// Cases 1/18: a stream without nDVR is never gated, whatever the cluster
// says.
func TestGateBypassesNonDVR(t *testing.T) {
	srv, registry, _, cl := newOwnershipServer(t)
	cl.peers = []node.PeerDVR{peerState("origin-b", true, true, time.Now().UnixMilli())}
	st := &media.Stream{Key: media.StreamKey{App: "plain", Name: "cam"}, StartedAt: time.Now()}
	if err := registry.Register(st); err != nil {
		t.Fatal(err)
	}
	defer registry.Unregister(st)

	if rec := gateGet(srv, "/plain/cam/playlist.m3u8"); rec.Code != http.StatusOK {
		t.Errorf("non-nDVR master = %d %q, want 200", rec.Code, rec.Body.String())
	}
	for _, path := range []string{"/plain/cam/chunks.m3u8", "/plain/cam/playlist_dvr.m3u8", "/plain/cam/seg1.ts"} {
		if rec := gateGet(srv, path); rec.Header().Get("X-Origin-Ownership") != "" {
			t.Errorf("GET %s gated for a non-nDVR stream", path)
		}
	}
}

// Archive ranges: served wherever the local recording overlaps, refused
// with a 404 elsewhere, independent of live ownership.
func TestGateRange(t *testing.T) {
	srv, _, tr, cl := newOwnershipServer(t)
	now := time.Now().UnixMilli()
	tr.SegmentIndexed(media.StreamKey{App: "live", Name: "cam"}, now-3600_000, now-1800_000)
	cl.peers = []node.PeerDVR{peerState("origin-b", true, true, now)} // B owns live

	from := (now - 3000_000) / 1000
	in := "/live/cam/playlist_dvr_range-" + itoa(from) + "-60.m3u8"
	if rec := gateGet(srv, in); rec.Header().Get("X-Origin-Ownership") != "" {
		t.Errorf("range inside local recording was gated: %d %q", rec.Code, rec.Body.String())
	}
	out := "/live/cam/playlist_dvr_range-" + itoa((now-600_000)/1000) + "-60.m3u8"
	if rec := gateGet(srv, out); rec.Code != http.StatusNotFound || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("range outside local recording = %d cc=%q, want 404 no-store", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

// Case 15: an nDVR ABR master follows its variants' owner.
func TestGateABRMaster(t *testing.T) {
	srv, registry, _, cl := newOwnershipServer(t)
	for _, name := range []string{"cam1", "cam2"} {
		st := &media.Stream{Key: media.StreamKey{App: "live", Name: name}, ABRGroup: "cam", StartedAt: time.Now()}
		if err := registry.Register(st); err != nil {
			t.Fatal(err)
		}
		defer registry.Unregister(st)
	}
	if rec := gateGet(srv, "/live/cam/playlist.m3u8"); rec.Code != http.StatusOK {
		t.Errorf("ABR master on the publishing node = %d %q", rec.Code, rec.Body.String())
	}

	// Same group seen from a node whose variants are owned elsewhere.
	cl.peers = []node.PeerDVR{peerState("origin-b", true, true, time.Now().UnixMilli())}
	for _, name := range []string{"cam1", "cam2"} {
		registry.Unregister(registry.Get(media.StreamKey{App: "live", Name: name}))
	}
	if rec := gateGet(srv, "/live/cam/playlist.m3u8"); rec.Code != http.StatusNotFound {
		t.Errorf("master on a non-owner = %d, want 404", rec.Code)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
