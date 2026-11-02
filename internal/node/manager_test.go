package node

import (
	"context"
	"testing"
	"time"

	natstest "github.com/nats-io/nats-server/v2/test"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
)

func newTestConfig(natsURL string) *config.Config {
	cfg := &config.Config{}
	cfg.Multinode.NATSUrl = natsURL
	cfg.Multinode.HeartbeatIntervalS = 1 // fast, so the test doesn't wait long
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
