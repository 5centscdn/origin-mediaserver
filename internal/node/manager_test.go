package node

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
)

func newTestConfig(natsURL string) *config.Config {
	cfg := &config.Config{}
	cfg.Multinode.NATSUrl = natsURL
	cfg.Multinode.HeartbeatIntervalS = 1 // fast, so the test doesn't wait long
	cfg.Multinode.PeerForgetS = 5
	cfg.Multinode.NodePriority = 100
	return cfg
}

// TestManager_DiscoversPeer drives two real Manager instances against a
// real in-process NATS server (not a mock) and verifies each one
// discovers the other via heartbeat, with the right name/URL/stream list —
// a genuine two-node cluster, just both nodes on localhost.
func TestManager_DiscoversPeer(t *testing.T) {
	srv := natstest.RunRandClientPortServer()
	defer srv.Shutdown()

	registryA := media.NewRegistry()
	streamA := &media.Stream{Key: media.StreamKey{App: "live", Name: "camera_1"}, Bus: media.NewBus()}
	if err := registryA.Register(streamA); err != nil {
		t.Fatalf("register streamA: %v", err)
	}
	defer registryA.Unregister(streamA)

	cfgA := newTestConfig(srv.ClientURL())
	cfgA.Server.NodeName = "node-a"
	mgrA := NewManager(cfgA, registryA, metrics.New(), "http://node-a.local:8080")

	registryB := media.NewRegistry()
	cfgB := newTestConfig(srv.ClientURL())
	cfgB.Server.NodeName = "node-b"
	mgrB := NewManager(cfgB, registryB, metrics.New(), "http://node-b.local:8080")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go mgrA.Run(ctx)
	go mgrB.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if len(mgrA.Peers()) > 0 && len(mgrB.Peers()) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for mutual discovery: A sees %d peers, B sees %d peers", len(mgrA.Peers()), len(mgrB.Peers()))
		}
		time.Sleep(20 * time.Millisecond)
	}

	peersOfA := mgrA.Peers()
	if len(peersOfA) != 1 {
		t.Fatalf("node A sees %d peers, want 1", len(peersOfA))
	}
	peerB := peersOfA[0]
	if peerB.NodeID != mgrB.NodeID() {
		t.Errorf("peer NodeID = %q, want %q", peerB.NodeID, mgrB.NodeID())
	}
	if peerB.NodeName != "node-b" {
		t.Errorf("peer NodeName = %q, want node-b", peerB.NodeName)
	}
	if peerB.URL != "http://node-b.local:8080" {
		t.Errorf("peer URL = %q, want http://node-b.local:8080", peerB.URL)
	}

	peersOfB := mgrB.Peers()
	if len(peersOfB) != 1 {
		t.Fatalf("node B sees %d peers, want 1", len(peersOfB))
	}
	peerA := peersOfB[0]
	if peerA.NodeID != mgrA.NodeID() {
		t.Errorf("peer NodeID = %q, want %q", peerA.NodeID, mgrA.NodeID())
	}
	if len(peerA.Streams) != 1 || peerA.Streams[0] != "live/camera_1" {
		t.Errorf("peer A streams = %v, want [live/camera_1]", peerA.Streams)
	}
}

// TestManager_ExpiresStalePeer verifies a peer that stops heartbeating is
// eventually dropped from Peers() rather than lingering forever.
func TestManager_ExpiresStalePeer(t *testing.T) {
	srv := natstest.RunRandClientPortServer()
	defer srv.Shutdown()

	registryA := media.NewRegistry()
	cfgA := newTestConfig(srv.ClientURL())
	cfgA.Server.NodeName = "node-a"
	mgrA := NewManager(cfgA, registryA, metrics.New(), "http://node-a.local:8080")

	registryB := media.NewRegistry()
	cfgB := newTestConfig(srv.ClientURL())
	cfgB.Server.NodeName = "node-b"
	mgrB := NewManager(cfgB, registryB, metrics.New(), "http://node-b.local:8080")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgrA.Run(ctx)

	ctxB, cancelB := context.WithCancel(ctx)
	go mgrB.Run(ctxB)

	deadline := time.Now().Add(5 * time.Second)
	for len(mgrA.Peers()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for initial discovery")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancelB() // node B stops heartbeating entirely, without a graceful bye

	// peerStaleMultiplier=3 * 1s heartbeat interval = 3s max before A's own
	// stale-check ticker (also every 1s) notices and drops B.
	deadline = time.Now().Add(6 * time.Second)
	for len(mgrA.Peers()) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("peer was never expired: still %d peers after node B stopped", len(mgrA.Peers()))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestUnreachablePeerKeptUntilForgotten: a peer that stops heartbeating
// without saying goodbye is hidden from Peers but stays in DVRView as not
// fresh, so nDVR ownership answers "unknown" instead of assuming it's
// offline, until peer_forget_s passes.
func TestUnreachablePeerKeptUntilForgotten(t *testing.T) {
	cfg := newTestConfig("")
	m := NewManager(cfg, media.NewRegistry(), metrics.New(), "http://a:8080")
	hb := Heartbeat{NodeID: "b1", NodeName: "node-b", URL: "http://b:8080", Gen: 1, Priority: 5,
		DVR: []DVRState{{App: "live", Stream: "cam", Publishing: true, PublishSinceMS: 1}}}
	m.handleHeartbeat(&nats.Msg{Data: mustJSON(t, hb)})

	v := m.DVRView("live", "cam")
	if len(v) != 1 || !v[0].Fresh || !v[0].Known || !v[0].State.Publishing || v[0].Priority != 5 {
		t.Fatalf("fresh view = %+v", v)
	}
	if v := m.DVRView("live", "other"); len(v) != 1 || v[0].Known {
		t.Fatalf("unlisted stream should be known=false: %+v", v)
	}

	// Silent for longer than 3 heartbeats.
	m.mu.Lock()
	m.peers["b1"].LastSeen = time.Now().Add(-4 * time.Second)
	m.mu.Unlock()
	m.expireStalePeers(time.Duration(cfg.Multinode.PeerForgetS) * time.Second)
	if len(m.Peers()) != 0 {
		t.Error("silent peer should be hidden from Peers")
	}
	if v := m.DVRView("live", "cam"); len(v) != 1 || v[0].Fresh {
		t.Errorf("silent peer should stay in DVRView as not fresh: %+v", v)
	}

	// Past peer_forget_s it's gone.
	m.mu.Lock()
	m.peers["b1"].LastSeen = time.Now().Add(-time.Duration(cfg.Multinode.PeerForgetS+1) * time.Second)
	m.mu.Unlock()
	m.expireStalePeers(time.Duration(cfg.Multinode.PeerForgetS) * time.Second)
	if v := m.DVRView("live", "cam"); len(v) != 0 {
		t.Errorf("forgotten peer still in DVRView: %+v", v)
	}
}

// TestHeartbeatOrderingAndRestart: an older heartbeat never overwrites a
// newer one, and a restarted peer (same name and URL, new ID) replaces its
// old incarnation instead of leaving it to look unreachable.
func TestHeartbeatOrderingAndRestart(t *testing.T) {
	m := NewManager(newTestConfig(""), media.NewRegistry(), metrics.New(), "http://a:8080")
	send := func(hb Heartbeat) { m.handleHeartbeat(&nats.Msg{Data: mustJSON(t, hb)}) }

	send(Heartbeat{NodeID: "b1", NodeName: "node-b", URL: "http://b:8080", Gen: 5,
		DVR: []DVRState{{App: "live", Stream: "cam", Publishing: true}}})
	send(Heartbeat{NodeID: "b1", NodeName: "node-b", URL: "http://b:8080", Gen: 4})
	if v := m.DVRView("live", "cam"); len(v) != 1 || !v[0].Known {
		t.Fatalf("older heartbeat overwrote newer: %+v", v)
	}

	send(Heartbeat{NodeID: "b2", NodeName: "node-b", URL: "http://b:8080", Gen: 1})
	if v := m.DVRView("live", "cam"); len(v) != 1 || v[0].NodeID != "b2" {
		t.Fatalf("restart: want only b2, got %+v", v)
	}

	send(Heartbeat{NodeID: "b2", NodeName: "node-b", Gen: 2, Leaving: true})
	if v := m.DVRView("live", "cam"); len(v) != 0 {
		t.Fatalf("leaving peer still known: %+v", v)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
