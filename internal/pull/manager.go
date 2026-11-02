// Package pull manages long-running outbound pull sources: it connects to a
// remote URL, registers the result as a local media.Stream, and keeps it
// running (with reconnect-on-failure) until explicitly stopped.
package pull

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/ingest/mpegts"
	"github.com/5centscdn/origin-mediaserver/internal/ingest/rtsp"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
	"github.com/5centscdn/origin-mediaserver/internal/rtmp"
	"github.com/5centscdn/origin-mediaserver/internal/srt"
)

// supportedProtocols lists every db.PullSource.Protocol value this Manager
// can actually run. mpegts_udp treats source.URL as a listen address (the
// "connection" is a passive bind, not an outbound dial) — reconnect just
// means re-binding the socket; see mpegts.ListenUDP.
var supportedProtocols = map[string]func(ctx context.Context, source db.PullSource, bus *media.Bus) error{
	"rtmp": func(ctx context.Context, source db.PullSource, bus *media.Bus) error {
		return rtmp.Pull(ctx, source.URL, source.StreamName, bus)
	},
	"mpegts_http": func(ctx context.Context, source db.PullSource, bus *media.Bus) error {
		return mpegts.PullHTTP(ctx, source.URL, bus)
	},
	"mpegts_udp": func(ctx context.Context, source db.PullSource, bus *media.Bus) error {
		return mpegts.ListenUDP(ctx, source.URL, bus)
	},
	"rtsp": func(ctx context.Context, source db.PullSource, bus *media.Bus) error {
		return rtsp.Pull(ctx, source.URL, bus)
	},
	"srt": func(ctx context.Context, source db.PullSource, bus *media.Bus) error {
		return srt.Pull(ctx, source.URL, bus, source.Passphrase, source.PBKeylen)
	},
}

// defaultReconnectDelay is used when a source's ReconnectDelayS is unset (0).
const defaultReconnectDelay = 5 * time.Second

// Manager runs and supervises pull sources.
type Manager struct {
	database *db.DB
	registry *media.Registry
	metrics  *metrics.Metrics

	// OnLive is called (in a new goroutine) whenever a pull source's stream
	// is registered, so the caller can start its HLS/DVR outputs — mirrors
	// rtmp.Server.OnPublish / failover.Manager.OnBackupLive, so a pulled
	// stream gets exactly the same output wiring a normal RTMP publish would.
	OnLive func(stream *media.Stream)

	mu      sync.Mutex
	workers map[string]*worker // keyed by pull_sources.id
}

// worker identifies one running pull attempt. It's a pointer (not just a bare
// CancelFunc) so run's final cleanup can tell "am I still the current worker
// for this ID" via pointer identity — otherwise a Stop() immediately followed
// by a Start() could race with the stopped worker's own cleanup deleting the
// new worker's map entry.
type worker struct {
	cancel context.CancelFunc
}

// NewManager creates a pull manager.
func NewManager(database *db.DB, registry *media.Registry, m *metrics.Metrics) *Manager {
	return &Manager{
		database: database,
		registry: registry,
		metrics:  m,
		workers:  make(map[string]*worker),
	}
}

// Start begins running source, if not already running. Only protocols in
// supportedProtocols are implemented — anything else returns an error
// immediately rather than silently doing nothing.
func (m *Manager) Start(ctx context.Context, source db.PullSource) error {
	if _, ok := supportedProtocols[source.Protocol]; !ok {
		return fmt.Errorf("pull protocol %q not supported", source.Protocol)
	}

	m.mu.Lock()
	if _, exists := m.workers[source.ID]; exists {
		m.mu.Unlock()
		return nil // already running
	}
	workerCtx, cancel := context.WithCancel(ctx)
	w := &worker{cancel: cancel}
	m.workers[source.ID] = w
	m.mu.Unlock()

	if err := m.database.SetPullSourceStatus(ctx, source.ID, "active"); err != nil {
		slog.Warn("set pull source status", "id", source.ID, "err", err)
	}

	go m.run(workerCtx, w, source)
	return nil
}

// Stop cancels the running worker for id, if any, and unconditionally marks
// the source inactive in the DB — self-healing a stale 'active' row left
// over from a previous crash even if nothing is actually running.
func (m *Manager) Stop(id string) {
	m.mu.Lock()
	w, exists := m.workers[id]
	if exists {
		delete(m.workers, id)
	}
	m.mu.Unlock()

	if exists {
		w.cancel()
	}

	if err := m.database.SetPullSourceStatus(context.Background(), id, "inactive"); err != nil {
		slog.Warn("set pull source status", "id", id, "err", err)
	}
}

// IsRunning reports whether a worker is currently running for id.
func (m *Manager) IsRunning(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, exists := m.workers[id]
	return exists
}

// run is the supervisor loop: connect, register, pull until it ends, and
// reconnect (if source.Reconnect) after source.ReconnectDelayS, until ctx is
// cancelled by Stop.
func (m *Manager) run(ctx context.Context, w *worker, source db.PullSource) {
	log := slog.With("component", "pull", "id", source.ID, "app", source.AppName, "stream", source.StreamName)

	reconnectDelay := time.Duration(source.ReconnectDelayS) * time.Second
	if reconnectDelay <= 0 {
		reconnectDelay = defaultReconnectDelay
	}

	key := media.StreamKey{App: source.AppName, Name: source.StreamName}

	for {
		// StopFunc is deliberately left unset here: Registry.Unregister calls
		// it on every unregister, including the ordinary end of one attempt
		// that's about to reconnect — wiring it to w.cancel would tear down
		// this whole retry loop after its very first attempt. Only Manager.Stop
		// should end the loop, which it does directly via w.cancel(), not
		// through the registry.
		stream := &media.Stream{
			Key:        key,
			SourceType: source.Protocol + "_pull",
			SourceAddr: source.URL,
		}

		if err := m.registry.Register(stream); err != nil {
			log.Error("register pull stream", "err", err)
		} else {
			if m.OnLive != nil {
				go m.OnLive(stream)
			}
			log.Info("pull source connecting", "protocol", source.Protocol, "url", source.URL)
			err := supportedProtocols[source.Protocol](ctx, source, stream.Bus)
			m.registry.Unregister(stream)
			if err != nil && ctx.Err() == nil {
				log.Error("pull source ended", "err", err)
			}
		}

		if ctx.Err() != nil {
			return // Stop() was called
		}
		if !source.Reconnect {
			break
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectDelay):
		}
	}

	// Only clean up if we're still the current worker for this ID — a
	// Stop() immediately followed by a Start() would already have replaced
	// us in the map, and we must not delete the new worker's entry.
	m.mu.Lock()
	stillCurrent := m.workers[source.ID] == w
	if stillCurrent {
		delete(m.workers, source.ID)
	}
	m.mu.Unlock()

	if stillCurrent {
		if err := m.database.SetPullSourceStatus(context.Background(), source.ID, "inactive"); err != nil {
			slog.Warn("set pull source status", "id", source.ID, "err", err)
		}
	}
}
