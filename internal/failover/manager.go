// Package failover implements stream health monitoring and hot-swap failover.
package failover

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/ingest/mp4file"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
	"github.com/5centscdn/origin-mediaserver/internal/rtmp"
)

// State represents the current failover state for one stream.
type State int

const (
	StateIdle          State = iota // no primary, no backup
	StateActivePrimary              // primary source is live
	StateDetecting                  // primary stalled, waiting to confirm failure
	StateWarmingBackup              // pulling backup source, pre-buffering
	StateSwitching                  // atomic swap happening
	StateActiveBackup               // serving from backup
	StateSwitchingBack              // primary recovered, swapping back
)

func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateActivePrimary:
		return "active_primary"
	case StateDetecting:
		return "detecting_failure"
	case StateWarmingBackup:
		return "warming_backup"
	case StateSwitching:
		return "switching"
	case StateActiveBackup:
		return "active_backup"
	case StateSwitchingBack:
		return "switching_back"
	}
	return "unknown"
}

// Rule is a resolved failover rule for one stream.
type Rule struct {
	AppName        string
	PrimaryStream  string
	BackupURL      string
	BackupProtocol string
	StallTimeoutMs int
	WarmupMs       int
	AutoRestore    bool
}

// Manager watches stream health and executes failover transitions.
type Manager struct {
	cfg      *config.Config
	registry *media.Registry
	metrics  *metrics.Metrics

	// OnBackupLive is called (in a new goroutine) whenever a failover backup
	// stream is registered, so the caller can start its HLS/DVR outputs —
	// mirrors rtmp.Server.OnPublish, so a backup stream gets exactly the
	// same output wiring a normal RTMP publish would.
	OnBackupLive func(stream *media.Stream)

	mu      sync.Mutex
	streams map[streamKey]*watchedMonitor
}

type streamKey struct{ app, name string }

// watchedMonitor pairs a running streamMonitor with the cancel func for the
// context it was started with, so a single rule can be unwatched (on update
// or delete) without tearing down every other rule's monitor.
type watchedMonitor struct {
	mon    *streamMonitor
	cancel context.CancelFunc
}

// NewManager creates a failover manager.
func NewManager(cfg *config.Config, registry *media.Registry, m *metrics.Metrics) *Manager {
	return &Manager{
		cfg:      cfg,
		registry: registry,
		metrics:  m,
		streams:  make(map[streamKey]*watchedMonitor),
	}
}

// Watch starts monitoring a stream with the given failover rule.
// Cancel ctx, or call Unwatch, to stop monitoring.
func (m *Manager) Watch(ctx context.Context, rule Rule) {
	key := streamKey{rule.AppName, rule.PrimaryStream}

	m.mu.Lock()
	if _, exists := m.streams[key]; exists {
		m.mu.Unlock()
		return // already watching
	}
	monCtx, cancel := context.WithCancel(ctx)
	mon := newStreamMonitor(rule, m.registry, m.cfg, m.metrics, m.OnBackupLive)
	m.streams[key] = &watchedMonitor{mon: mon, cancel: cancel}
	m.mu.Unlock()

	go func() {
		mon.run(monCtx)
		m.mu.Lock()
		delete(m.streams, key)
		m.mu.Unlock()
	}()
}

// Unwatch stops monitoring app/stream, if currently watched — used when a
// failover rule is deleted or edited (edit = unwatch the old key, then
// Watch again with the updated rule).
func (m *Manager) Unwatch(app, stream string) {
	m.mu.Lock()
	wm := m.streams[streamKey{app, stream}]
	m.mu.Unlock()

	if wm != nil {
		wm.cancel()
	}
}

// TriggerManual forces an immediate failover for app/stream.
func (m *Manager) TriggerManual(app, stream string) {
	m.mu.Lock()
	wm := m.streams[streamKey{app, stream}]
	m.mu.Unlock()

	if wm != nil {
		wm.mon.triggerCh <- struct{}{}
	}
}

// Status returns the current failover state for a stream.
func (m *Manager) Status(app, stream string) State {
	m.mu.Lock()
	wm := m.streams[streamKey{app, stream}]
	m.mu.Unlock()

	if wm == nil {
		return StateIdle
	}
	wm.mon.mu.Lock()
	defer wm.mon.mu.Unlock()
	return wm.mon.state
}

// ─── streamMonitor ────────────────────────────────────────────────────────────

type streamMonitor struct {
	rule         Rule
	registry     *media.Registry
	cfg          *config.Config
	metrics      *metrics.Metrics
	onBackupLive func(stream *media.Stream)
	log          *slog.Logger

	mu        sync.Mutex
	state     State
	triggerCh chan struct{}

	lastKeyframeAt time.Time
}

func newStreamMonitor(rule Rule, registry *media.Registry, cfg *config.Config, m *metrics.Metrics, onBackupLive func(*media.Stream)) *streamMonitor {
	return &streamMonitor{
		rule:         rule,
		registry:     registry,
		cfg:          cfg,
		metrics:      m,
		onBackupLive: onBackupLive,
		log:          slog.With("app", rule.AppName, "stream", rule.PrimaryStream),
		state:        StateIdle,
		triggerCh:    make(chan struct{}, 1),
	}
}

// run monitors the primary stream and, on failover, waits for it to recover
// (if rule.AutoRestore) before re-monitoring it again. It returns when ctx
// is cancelled, or immediately after a failover if AutoRestore is false.
func (m *streamMonitor) run(ctx context.Context) {
	for {
		m.monitorPrimary(ctx)

		if ctx.Err() != nil {
			return
		}
		if m.getState() != StateActiveBackup {
			// monitorPrimary only returns early (without failing over) when
			// ctx is cancelled, which is handled above — nothing left to do.
			return
		}
		if !m.rule.AutoRestore {
			return // stay on backup until manually intervened
		}
		if !m.waitForPrimaryRestore(ctx) {
			return
		}

		m.setState(StateSwitchingBack)
		m.log.Info("primary reconnected, switching back")
		// Loop back into monitorPrimary, which immediately sets
		// StateActivePrimary and resumes normal watchdog monitoring.
	}
}

// monitorPrimary watches the primary stream and executes failover if it
// disappears or stalls. It returns once a failover succeeds (state becomes
// StateActiveBackup) or ctx is cancelled.
func (m *streamMonitor) monitorPrimary(ctx context.Context) {
	watchInterval := time.Duration(m.cfg.Failover.WatchdogIntervalMs) * time.Millisecond
	stallTimeout := time.Duration(m.rule.StallTimeoutMs) * time.Millisecond
	if stallTimeout == 0 {
		stallTimeout = time.Duration(m.cfg.Failover.StallTimeoutMs) * time.Millisecond
	}

	m.setState(StateActivePrimary)
	m.mu.Lock()
	m.lastKeyframeAt = time.Now()
	m.mu.Unlock()

	// Keep lastKeyframeAt fresh by subscribing to the primary stream's bus.
	// Scoped to this monitorPrimary call — once we fail over there's no
	// primary to track keyframes from anymore.
	keyframeCtx, stopKeyframeWatch := context.WithCancel(ctx)
	defer stopKeyframeWatch()
	go m.watchKeyframes(keyframeCtx)

	watchdog := time.NewTicker(watchInterval)
	defer watchdog.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-m.triggerCh:
			m.log.Info("manual failover triggered")
			if m.executeFailover(ctx, "manual") {
				return
			}

		case <-watchdog.C:
			if m.getState() != StateActivePrimary {
				continue
			}
			stream := m.registry.Get(media.StreamKey{App: m.rule.AppName, Name: m.rule.PrimaryStream})
			if stream == nil {
				m.log.Warn("primary stream disappeared")
				if m.executeFailover(ctx, "stream_missing") {
					return
				}
				continue
			}

			m.mu.Lock()
			last := m.lastKeyframeAt
			m.mu.Unlock()

			if time.Since(last) > stallTimeout {
				m.log.Warn("primary stream stalled", "since", time.Since(last).Round(time.Millisecond))
				m.setState(StateDetecting)
				if m.executeFailover(ctx, "stall") {
					return
				}
			}
		}
	}
}

// waitForPrimaryRestore polls until a real primary stream reappears at the
// primary key (SourceType == "rtmp") — which happens automatically once an
// encoder reconnects and handlePublish's eviction rule preempts the backup
// registration. Returns false if ctx is cancelled first.
func (m *streamMonitor) waitForPrimaryRestore(ctx context.Context) bool {
	watchInterval := time.Duration(m.cfg.Failover.WatchdogIntervalMs) * time.Millisecond
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()

	primaryKey := media.StreamKey{App: m.rule.AppName, Name: m.rule.PrimaryStream}

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if s := m.registry.Get(primaryKey); s != nil && s.SourceType == "rtmp" {
				return true
			}
		}
	}
}

// onKeyframe is called by a packet subscriber when a keyframe arrives.
func (m *streamMonitor) onKeyframe() {
	m.mu.Lock()
	m.lastKeyframeAt = time.Now()
	m.mu.Unlock()
}

// watchKeyframes subscribes to the primary stream's bus and calls onKeyframe
// for every keyframe packet, refreshing lastKeyframeAt for the watchdog in
// monitorPrimary. If the stream isn't live yet, it polls the registry until
// it appears.
func (m *streamMonitor) watchKeyframes(ctx context.Context) {
	const retryDelay = 500 * time.Millisecond

	for {
		if ctx.Err() != nil {
			return
		}

		stream := m.registry.Get(media.StreamKey{App: m.rule.AppName, Name: m.rule.PrimaryStream})
		if stream == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryDelay):
				continue
			}
		}

		sub := stream.Bus.Subscribe(ctx)
		for pkt := range sub.Chan() {
			if pkt.IsKeyframe {
				m.onKeyframe()
			}
		}
		sub.Close()

		select {
		case <-ctx.Done():
			return
		case <-time.After(retryDelay):
		}
	}
}

// executeFailover attempts to fail over from primary to backup: it evicts a
// stalled-but-still-registered primary (a no-op if the primary already
// disappeared on its own), pulls the backup source (an RTMP pull, or an MP4
// file looped in real time by internal/ingest/mp4file), registers it at the
// primary's own StreamKey so existing playback URLs keep working, and starts
// its HLS/DVR outputs via onBackupLive — the same path a normal RTMP publish
// goes through.
//
// Returns true if it succeeded (state is now StateActiveBackup) and the
// caller should stop primary monitoring; false if it was skipped or aborted
// (state unchanged or reverted) and primary monitoring should continue.
func (m *streamMonitor) executeFailover(ctx context.Context, trigger string) bool {
	if s := m.getState(); s == StateActiveBackup || s == StateWarmingBackup {
		return false // already failed over / failing over
	}

	switch m.rule.BackupProtocol {
	case "rtmp", "mp4":
	default:
		m.log.Error("failover backup protocol not supported (rtmp or mp4)",
			"protocol", m.rule.BackupProtocol)
		return false
	}

	if m.metrics != nil {
		m.metrics.FailoverEvents.WithLabelValues(m.rule.AppName, m.rule.PrimaryStream, trigger).Inc()
	}

	m.setState(StateWarmingBackup)
	m.log.Info("warming backup", "url", m.rule.BackupURL)

	primaryKey := media.StreamKey{App: m.rule.AppName, Name: m.rule.PrimaryStream}

	// Evict a stalled-but-still-registered primary so the backup can take
	// its key. Unregister is identity-checked, so if the primary already
	// disappeared (registry.Get would already be nil) this is skipped.
	if stalePrimary := m.registry.Get(primaryKey); stalePrimary != nil {
		m.registry.Unregister(stalePrimary)
	}

	backupCtx, cancel := context.WithCancel(ctx)
	backupStream := &media.Stream{
		Key:        primaryKey,
		SourceType: "failover_backup",
		SourceAddr: m.rule.BackupURL,
		StopFunc:   cancel,
	}
	if err := m.registry.Register(backupStream); err != nil {
		m.log.Error("register backup stream", "err", err)
		cancel()
		m.setState(StateActivePrimary)
		return false
	}

	go func() {
		var err error
		switch m.rule.BackupProtocol {
		case "mp4":
			// A slate/filler file looped in real time; a remote file is
			// downloaded once into the data dir and reused next time.
			err = mp4file.Play(backupCtx, m.rule.BackupURL,
				filepath.Join(m.cfg.Server.DataDir, "failover-cache"), backupStream.Bus, m.log)
		default:
			err = rtmp.Pull(backupCtx, m.rule.BackupURL, m.rule.PrimaryStream, backupStream.Bus)
		}
		if err != nil && backupCtx.Err() == nil {
			m.log.Error("backup source ended", "protocol", m.rule.BackupProtocol, "err", err)
		}
	}()

	if m.onBackupLive != nil {
		go m.onBackupLive(backupStream)
	}

	warmup := time.Duration(m.rule.WarmupMs) * time.Millisecond
	if warmup == 0 {
		warmup = time.Duration(m.cfg.Failover.WarmupBufferMs) * time.Millisecond
	}

	select {
	case <-ctx.Done():
		cancel()
		return false
	case <-time.After(warmup):
	}

	m.setState(StateSwitching)
	m.log.Info("switching to backup")

	m.setState(StateActiveBackup)
	m.log.Info("now serving from backup", "url", m.rule.BackupURL)
	return true
}

func (m *streamMonitor) setState(s State) {
	m.mu.Lock()
	old := m.state
	m.state = s
	m.mu.Unlock()
	if old != s {
		m.log.Info("failover state changed", "from", old, "to", s)
	}
}

func (m *streamMonitor) getState() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}
