// Package node implements peer discovery and health propagation for
// multi-node deployments: each node publishes a periodic heartbeat over
// NATS (its name, URL, and live stream list) and subscribes to every
// peer's heartbeat, giving every node a real-time, auto-discovered view of
// the rest of the cluster — no central coordinator required.
//
// Heartbeats also carry the nDVR ownership state of this node's
// nDVR-enabled streams (DVRState; see internal/dvrowner, which decides
// which node serves an nDVR stream's playlists). Streams without nDVR are
// never put in that list.
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
	"sync/atomic"
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

	// Priority is [multinode] node_priority (lower wins ties).
	Priority int `json:"priority,omitempty"`
	// Gen increases with every heartbeat a node sends; a receiver ignores
	// one that isn't newer than what it already has from that node.
	Gen uint64 `json:"gen,omitempty"`
	// Leaving is set on the last heartbeat of a clean shutdown, so peers
	// drop the node at once instead of holding it as unreachable.
	Leaving bool `json:"leaving,omitempty"`
	// DVR is the ownership state of every nDVR-enabled stream this node is
	// publishing or has a recording of. Nothing else is listed.
	DVR []DVRState `json:"dvr,omitempty"`
}

// DVRState is one nDVR-enabled stream's state on the node that sent it.
// Times are UTC milliseconds; DVR times come from the recording's own
// timeline (dvr_segments start/end), not file times.
type DVRState struct {
	App            string `json:"app"`
	Stream         string `json:"stream"`
	Publishing     bool   `json:"pub,omitempty"`
	PublishSinceMS int64  `json:"pub_since,omitempty"`
	DVRStartMS     int64  `json:"dvr_start,omitempty"` // 0 = no valid recording
	DVREndMS       int64  `json:"dvr_end,omitempty"`
}

// Peer is a remote node's most recently received heartbeat, plus when it
// was received (used for staleness checks).
type Peer struct {
	Heartbeat
	LastSeen time.Time `json:"last_seen"`

	dvr map[string]DVRState // Heartbeat.DVR keyed "app/stream"
}

// PeerDVR is one peer's view of a stream, as returned by Manager.DVRView.
type PeerDVR struct {
	NodeID   string
	NodeName string
	Priority int
	// Fresh is false once the peer has missed peerStaleMultiplier
	// heartbeats: its last known state may be out of date.
	Fresh bool
	// Known is false if the peer didn't list the stream at all.
	Known bool
	State DVRState
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

	// nc is set once Run connects; read by Ready from request goroutines.
	nc atomic.Pointer[nats.Conn]

	mu    sync.RWMutex
	peers map[string]*Peer // keyed by NodeID, excludes self
	// readyAt is when this node has been connected long enough to have
	// heard every live peer (two heartbeat intervals); zero = not yet.
	readyAt time.Time

	gen         uint64
	kick        chan struct{}
	dvrReporter func() []DVRState
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
		kick:     make(chan struct{}, 1),
	}
}

// SetDVRReporter sets the source of this node's DVRState list, called for
// every heartbeat. Set before Run.
func (m *Manager) SetDVRReporter(f func() []DVRState) { m.dvrReporter = f }

// Kick sends a heartbeat now instead of at the next tick, e.g. when an
// nDVR stream starts or stops publishing here. Calls coalesce.
func (m *Manager) Kick() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// Priority is this node's configured node_priority.
func (m *Manager) Priority() int { return m.cfg.Multinode.NodePriority }

// NodeName is this node's configured name.
func (m *Manager) NodeName() string { return m.cfg.Server.NodeName }

// Ready reports whether this node's view of the cluster can be trusted:
// it's connected to NATS and has been for two heartbeat intervals, long
// enough to have heard from every live peer.
func (m *Manager) Ready() bool {
	if nc := m.nc.Load(); nc == nil || !nc.IsConnected() {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return !m.readyAt.IsZero() && time.Now().After(m.readyAt)
}

// DVRView returns every peer's view of app/stream, including peers that
// have gone quiet but aren't forgotten yet (Fresh = false). One map lookup
// per peer, no I/O.
func (m *Manager) DVRView(app, stream string) []PeerDVR {
	key := app + "/" + stream
	freshAge := m.interval() * peerStaleMultiplier
	now := time.Now()
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]PeerDVR, 0, len(m.peers))
	for _, p := range m.peers {
		st, known := p.dvr[key]
		out = append(out, PeerDVR{
			NodeID: p.NodeID, NodeName: p.NodeName, Priority: p.Priority,
			Fresh: now.Sub(p.LastSeen) <= freshAge,
			Known: known, State: st,
		})
	}
	return out
}

func (m *Manager) interval() time.Duration {
	return time.Duration(m.cfg.Multinode.HeartbeatIntervalS) * time.Second
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
	interval := m.interval()
	opts := []nats.Option{
		nats.Name("origin-mediaserver:" + m.cfg.Server.NodeName),
		// Keep trying instead of giving up: without NATS this node can't
		// see its peers, and nDVR ownership stays "unknown" (503) rather
		// than guessing, so it must come back by itself.
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ConnectHandler(func(*nats.Conn) { m.markConnected(interval) }),
		nats.ReconnectHandler(func(*nats.Conn) { m.markConnected(interval) }),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			m.mu.Lock()
			m.readyAt = time.Time{}
			m.mu.Unlock()
			slog.Warn("multinode: disconnected from nats", "err", err)
		}),
	}
	if m.cfg.Multinode.NATSCreds != "" {
		opts = append(opts, nats.UserCredentials(m.cfg.Multinode.NATSCreds))
	}

	nc, err := nats.Connect(m.cfg.Multinode.NATSUrl, opts...)
	if err != nil {
		return fmt.Errorf("connect to nats %s: %w", m.cfg.Multinode.NATSUrl, err)
	}
	defer nc.Close()
	m.nc.Store(nc)
	if nc.IsConnected() {
		m.markConnected(interval)
	}

	sub, err := nc.Subscribe(heartbeatWildcardSubject, m.handleHeartbeat)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	defer sub.Unsubscribe()

	heartbeatTicker := time.NewTicker(interval)
	defer heartbeatTicker.Stop()

	staleTicker := time.NewTicker(interval)
	defer staleTicker.Stop()

	m.publishHeartbeat()
	slog.Info("multinode: cluster manager started", "node_id", m.nodeID, "node_name", m.cfg.Server.NodeName, "nats_url", m.cfg.Multinode.NATSUrl)

	for {
		select {
		case <-ctx.Done():
			m.publishLeaving()
			return nil
		case <-heartbeatTicker.C:
			m.publishHeartbeat()
		case <-m.kick:
			m.publishHeartbeat()
		case <-staleTicker.C:
			m.expireStalePeers(max(time.Duration(m.cfg.Multinode.PeerForgetS)*time.Second, interval*peerStaleMultiplier))
		}
	}
}

// markConnected starts the readiness clock on (re)connect: peers'
// heartbeats need a couple of intervals to arrive.
func (m *Manager) markConnected(interval time.Duration) {
	m.mu.Lock()
	m.readyAt = time.Now().Add(2 * interval)
	m.mu.Unlock()
}

func (m *Manager) publishHeartbeat() {
	m.gen++
	hb := Heartbeat{
		NodeID:   m.nodeID,
		NodeName: m.cfg.Server.NodeName,
		URL:      m.selfURL,
		SentAt:   time.Now(),
		Priority: m.cfg.Multinode.NodePriority,
		Gen:      m.gen,
	}
	for _, s := range m.registry.All() {
		hb.Streams = append(hb.Streams, s.Key.App+"/"+s.Key.Name)
	}
	if m.dvrReporter != nil {
		hb.DVR = m.dvrReporter()
	}
	m.publish(hb)
}

// publishLeaving tells peers this node is shutting down cleanly, so they
// stop counting on it at once.
func (m *Manager) publishLeaving() {
	m.gen++
	m.publish(Heartbeat{NodeID: m.nodeID, NodeName: m.cfg.Server.NodeName, URL: m.selfURL,
		SentAt: time.Now(), Gen: m.gen, Leaving: true})
	m.nc.Load().FlushTimeout(time.Second)
}

func (m *Manager) publish(hb Heartbeat) {
	data, err := json.Marshal(hb)
	if err != nil {
		slog.Warn("multinode: marshal heartbeat", "err", err)
		return
	}
	subject := heartbeatSubjectPrefix + m.nodeID + heartbeatSubjectSuffix
	if err := m.nc.Load().Publish(subject, data); err != nil {
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
	old, known := m.peers[hb.NodeID]
	if known && hb.Gen != 0 && hb.Gen <= old.Gen {
		m.mu.Unlock()
		return // reordered or duplicate
	}
	if hb.Leaving {
		delete(m.peers, hb.NodeID)
		count := len(m.peers)
		m.mu.Unlock()
		slog.Info("multinode: peer left", "node_id", hb.NodeID, "node_name", hb.NodeName)
		m.setPeerMetric(count)
		return
	}
	// A restarted node comes back with a new random ID; drop its previous
	// incarnation now rather than holding it as unreachable.
	if !known && hb.NodeName != "" {
		for id, p := range m.peers {
			if p.NodeName == hb.NodeName && p.URL == hb.URL {
				delete(m.peers, id)
			}
		}
	}
	p := &Peer{Heartbeat: hb, LastSeen: time.Now()}
	if len(hb.DVR) > 0 {
		p.dvr = make(map[string]DVRState, len(hb.DVR))
		for _, st := range hb.DVR {
			p.dvr[st.App+"/"+st.Stream] = st
		}
	}
	m.peers[hb.NodeID] = p
	count := len(m.peers)
	m.mu.Unlock()

	if !known {
		slog.Info("multinode: discovered new peer", "node_id", hb.NodeID, "node_name", hb.NodeName, "url", hb.URL)
	}
	m.setPeerMetric(count)
}

func (m *Manager) setPeerMetric(count int) {
	if m.metrics != nil {
		m.metrics.ClusterPeers.Set(float64(count))
	}
}

// expireStalePeers forgets peers silent for longer than maxAge. Peers
// silent for less, but more than peerStaleMultiplier intervals, are kept
// as unreachable: hidden from Peers, still in DVRView with Fresh=false.
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
	m.setPeerMetric(len(m.peers))
}

// Peers returns a snapshot of every peer that has heartbeated recently
// (excluding this node itself).
func (m *Manager) Peers() []Peer {
	freshAge := m.interval() * peerStaleMultiplier
	now := time.Now()
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Peer, 0, len(m.peers))
	for _, p := range m.peers {
		if now.Sub(p.LastSeen) > freshAge {
			continue
		}
		out = append(out, *p)
	}
	return out
}
