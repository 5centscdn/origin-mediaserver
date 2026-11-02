// Package node implements peer discovery and health propagation for
// multi-node deployments: each node publishes a periodic heartbeat over
// NATS (its name, URL, and live stream list) and subscribes to every
// peer's heartbeat, giving every node a real-time, auto-discovered view of
// the rest of the cluster — no central coordinator required.
//
// This is peer discovery/health only. A central management console
// (cross-node config push, orchestration) remains a separate, optional
// service per this project's architecture — see project memory/the
// original spec doc. It replaces nothing about the pre-existing "servers"
// DB table (a manually-curated bookmark list the panel uses to link
// between node dashboards) — the two are complementary and exposed via
// separate API endpoints.
package node

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
)

const (
	heartbeatSubjectPrefix   = "mediaserver.nodes."
	heartbeatSubjectSuffix   = ".heartbeat"
	heartbeatWildcardSubject = heartbeatSubjectPrefix + "*" + heartbeatSubjectSuffix

	// peerStaleMultiplier is how many heartbeat intervals a peer can miss
	// before it's considered offline and dropped from Peers() — tolerates
	// one or two dropped/delayed heartbeats without flapping.
	peerStaleMultiplier = 3
)

// Heartbeat is the payload each node publishes about itself.
type Heartbeat struct {
	NodeID   string    `json:"node_id"`
	NodeName string    `json:"node_name"`
	URL      string    `json:"url"`
	Streams  []string  `json:"streams"` // "app/name" for every currently live stream
	SentAt   time.Time `json:"sent_at"`
}

// Peer is a remote node's most recently received heartbeat, plus when it
// was received (used for staleness checks).
type Peer struct {
	Heartbeat
	LastSeen time.Time `json:"last_seen"`
}

// Manager connects to NATS, publishes this node's own heartbeat on a
// timer, and maintains an in-memory view of every peer's last heartbeat.
// Not safe for concurrent Run calls — one Manager per process.
type Manager struct {
	cfg      *config.Config
	registry *media.Registry
	metrics  *metrics.Metrics
	selfURL  string
	nodeID   string

	nc *nats.Conn

	mu    sync.RWMutex
	peers map[string]*Peer // keyed by NodeID, excludes self
}

// NewManager creates a cluster Manager. selfURL is this node's own
// externally-reachable base URL (e.g. "http://host:8080"), advertised in
// its heartbeat so peers/panels can link directly to it.
func NewManager(cfg *config.Config, registry *media.Registry, m *metrics.Metrics, selfURL string) *Manager {
	return &Manager{
		cfg:      cfg,
		registry: registry,
		metrics:  m,
		selfURL:  selfURL,
		nodeID:   uuid.NewString(),
		peers:    make(map[string]*Peer),
	}
}

// NodeID returns this node's randomly-generated cluster identity — stable
// for the lifetime of the process, not persisted across restarts (a
// restarted node is indistinguishable from a new one to its peers, which
// is fine: heartbeats are stateless and peers self-expire on their own).
func (m *Manager) NodeID() string { return m.nodeID }

// Run connects to NATS and runs the heartbeat publish loop + peer
// subscription until ctx is done or a fatal connection error occurs.
// Blocking, matching every other supervised loop in this codebase
// (rtmp.Pull, mpegts.ListenUDP, ...).
func (m *Manager) Run(ctx context.Context) error {
	opts := []nats.Option{nats.Name("origin-mediaserver:" + m.cfg.Server.NodeName)}
	if m.cfg.Multinode.NATSCreds != "" {
		opts = append(opts, nats.UserCredentials(m.cfg.Multinode.NATSCreds))
	}

	nc, err := nats.Connect(m.cfg.Multinode.NATSUrl, opts...)
	if err != nil {
		return fmt.Errorf("connect to nats %s: %w", m.cfg.Multinode.NATSUrl, err)
	}
	defer nc.Close()
	m.nc = nc

	sub, err := nc.Subscribe(heartbeatWildcardSubject, m.handleHeartbeat)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	defer sub.Unsubscribe()

	interval := time.Duration(m.cfg.Multinode.HeartbeatIntervalS) * time.Second
	heartbeatTicker := time.NewTicker(interval)
	defer heartbeatTicker.Stop()

	staleTicker := time.NewTicker(interval)
	defer staleTicker.Stop()

	m.publishHeartbeat()
	slog.Info("multinode: cluster manager started", "node_id", m.nodeID, "node_name", m.cfg.Server.NodeName, "nats_url", m.cfg.Multinode.NATSUrl)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-heartbeatTicker.C:
			m.publishHeartbeat()
		case <-staleTicker.C:
			m.expireStalePeers(interval * peerStaleMultiplier)
		}
	}
}

func (m *Manager) publishHeartbeat() {
	hb := Heartbeat{
		NodeID:   m.nodeID,
		NodeName: m.cfg.Server.NodeName,
		URL:      m.selfURL,
		SentAt:   time.Now(),
	}
	for _, s := range m.registry.All() {
		hb.Streams = append(hb.Streams, s.Key.App+"/"+s.Key.Name)
	}

	data, err := json.Marshal(hb)
	if err != nil {
		slog.Warn("multinode: marshal heartbeat", "err", err)
		return
	}
	subject := heartbeatSubjectPrefix + m.nodeID + heartbeatSubjectSuffix
	if err := m.nc.Publish(subject, data); err != nil {
		slog.Warn("multinode: publish heartbeat", "err", err)
	}
}

// handleHeartbeat is the NATS subscription callback — invoked from a
// nats.go-managed goroutine, so peers map access must go through m.mu (it
// isn't otherwise touched concurrently, since publishHeartbeat/
// expireStalePeers both run on Run's single goroutine, but handleHeartbeat
// runs on nats.go's own).
func (m *Manager) handleHeartbeat(msg *nats.Msg) {
	var hb Heartbeat
	if err := json.Unmarshal(msg.Data, &hb); err != nil {
		slog.Warn("multinode: bad heartbeat payload", "err", err)
		return
	}
	if hb.NodeID == m.nodeID {
		return // our own heartbeat, echoed back by NATS — ignore
	}

	m.mu.Lock()
	_, isNew := m.peers[hb.NodeID]
	m.peers[hb.NodeID] = &Peer{Heartbeat: hb, LastSeen: time.Now()}
	count := len(m.peers)
	m.mu.Unlock()

	if !isNew {
		slog.Info("multinode: discovered new peer", "node_id", hb.NodeID, "node_name", hb.NodeName, "url", hb.URL)
	}
	if m.metrics != nil {
		m.metrics.ClusterPeers.Set(float64(count))
	}
}

func (m *Manager) expireStalePeers(maxAge time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for id, p := range m.peers {
		if now.Sub(p.LastSeen) > maxAge {
			delete(m.peers, id)
			slog.Info("multinode: peer went stale, removed", "node_id", id, "node_name", p.NodeName)
		}
	}
	if m.metrics != nil {
		m.metrics.ClusterPeers.Set(float64(len(m.peers)))
	}
}

// Peers returns a snapshot of every currently-known peer (excluding this
// node itself).
func (m *Manager) Peers() []Peer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Peer, 0, len(m.peers))
	for _, p := range m.peers {
		out = append(out, *p)
	}
	return out
}
