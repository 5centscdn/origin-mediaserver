// Package pull manages long-running outbound pull sources: it connects to a
// remote URL, registers the result as a local media.Stream, and keeps it
// running (with reconnect-on-failure) until explicitly stopped.
//
// A source can list fallback URLs (Nimble's "Fallback URLs"). When the
// primary fails, or connects but sends no media for stallTimeout, the next
// URL is tried straight away; after the last one the list starts over from
// the primary after the reconnect delay. While a fallback is playing, the
// primary is tried every PrimaryCheckS seconds in the background, and once
// it delivers media steadily (probeSteady) the stream switches back to it.
// The probe's connection becomes the live one, so the switch costs no
// reconnect. Each switch registers a new media.Stream under the same key,
// as a reconnect does, so outputs restart on the new source's timestamps.
package pull

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
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

// timing holds the fallback timing a worker uses, copied from the package
// variables below when it starts.
type timing struct{ stall, probeTimeout, probeSteady, poll time.Duration }

func currentTiming() timing {
	return timing{stallTimeout, probeTimeout, probeSteady, pollEvery}
}

// Fallback timing. Variables so tests can shorten them.
var (
	// stallTimeout: with fallbacks configured, a source that has sent no
	// audio/video for this long (from connecting, or since its last packet)
	// counts as failed. An RTMP server whose stream isn't published accepts
	// the play and then sends nothing, so an error alone never comes.
	stallTimeout = 10 * time.Second
	// probeTimeout bounds one background check of the primary.
	probeTimeout = 15 * time.Second
	// probeSteady: the primary counts as back once media has flowed for
	// this long without a gap of more than a second.
	probeSteady = 3 * time.Second
	// pollEvery is how often watchdogs look at Bus.LastMediaAt.
	pollEvery = 250 * time.Millisecond
)

// CheckFallbacks reports an error if any fallback URL needs a protocol the
// manager can't run (judged from its scheme, defaulting to primaryProtocol).
func CheckFallbacks(primaryProtocol string, urls []string) error {
	for _, u := range urls {
		if p := ProtocolForURL(u, primaryProtocol); supportedProtocols[p] == nil {
			return fmt.Errorf("fallback %q: pull protocol %q not supported", u, p)
		}
	}
	return nil
}

// ProtocolForURL picks the pull protocol for u from its scheme, or def when
// the scheme doesn't decide it. Fallback URLs may use a different protocol
// from the primary (e.g. an SRT primary with an RTMP backup).
func ProtocolForURL(u, def string) string {
	pu, err := url.Parse(u)
	if err != nil {
		return def
	}
	switch strings.ToLower(pu.Scheme) {
	case "rtmp", "rtmps":
		return "rtmp"
	case "rtsp", "rtsps":
		return "rtsp"
	case "srt":
		return "srt"
	case "udp":
		return "mpegts_udp"
	case "http", "https":
		if def == "mpegts_http" {
			return def
		}
		return "mpegts_http"
	}
	return def
}

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

	// attempts/failures count connection attempts (each URL tried, including
	// reconnects and fallback switches) and attempts that ended in an error
	// or a stall, for the dashboard (internal/sysmon).
	attempts atomic.Int64
	failures atomic.Int64
}

// Stats returns pull connection attempts and failures since start. Nil-safe.
func (m *Manager) Stats() (attempts, failures int64) {
	if m == nil {
		return 0, 0
	}
	return m.attempts.Load(), m.failures.Load()
}

// worker identifies one running pull attempt. It's a pointer (not just a bare
// CancelFunc) so run's final cleanup can tell "am I still the current worker
// for this ID" via pointer identity — otherwise a Stop() immediately followed
// by a Start() could race with the stopped worker's own cleanup deleting the
// new worker's map entry.
type worker struct {
	cancel context.CancelFunc

	mu     sync.Mutex
	active string // URL currently feeding the stream ("" while connecting)
}

func (w *worker) setActive(u string) {
	w.mu.Lock()
	w.active = u
	w.mu.Unlock()
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
	if err := CheckFallbacks(source.Protocol, source.FallbackURLs); err != nil {
		return err
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

	go m.run(workerCtx, w, source, currentTiming())
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

// ActiveURL returns the URL currently feeding source id's stream, or "" if
// it isn't running or is between connections.
func (m *Manager) ActiveURL(id string) string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	w := m.workers[id]
	m.mu.Unlock()
	if w == nil {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.active
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
// cancelled by Stop. With fallback URLs it also moves between them; see the
// package comment.
func (m *Manager) run(ctx context.Context, w *worker, source db.PullSource, tm timing) {
	log := slog.With("component", "pull", "id", source.ID, "app", source.AppName, "stream", source.StreamName)

	reconnectDelay := time.Duration(source.ReconnectDelayS) * time.Second
	if reconnectDelay <= 0 {
		reconnectDelay = defaultReconnectDelay
	}

	key := media.StreamKey{App: source.AppName, Name: source.StreamName}
	urls := append([]string{source.URL}, source.FallbackURLs...)
	hasFallback := len(urls) > 1
	idx := 0
	var next *attempt // a connected primary handed over by a probe

	for {
		a := next
		next = nil
		if a == nil {
			a = dial(ctx, source, urls[idx])
		}

		// Not listed as live until media arrives: a source that accepts
		// the connection and then fails, or sends nothing (an IPTV account
		// over its connection limit, an RTMP path with no publisher), was
		// shown in Live Streams with a green dot and 0 B while it flapped.
		switch waitFirstMedia(ctx, a, tm) {
		case firstMediaCancelled:
			a.stop()
			return
		case firstMediaEnded:
			if a.err != nil {
				log.Error("pull source ended before sending media", "url", a.url, "err", a.err)
			}
			m.attempts.Add(1)
			m.failures.Add(1)
			if m.retryOrStop(ctx, &idx, len(urls), hasFallback, source.Reconnect, reconnectDelay) {
				continue
			}
			m.finish(w, source)
			return
		case firstMediaTimeout:
			log.Warn("pull source sent no media, giving up on it", "url", a.url, "after", tm.stall)
			a.stop()
			m.attempts.Add(1)
			m.failures.Add(1)
			if m.retryOrStop(ctx, &idx, len(urls), hasFallback, source.Reconnect, reconnectDelay) {
				continue
			}
			m.finish(w, source)
			return
		}

		// StopFunc is deliberately left unset here: Registry.Unregister calls
		// it on every unregister, including the ordinary end of one attempt
		// that's about to reconnect — wiring it to w.cancel would tear down
		// this whole retry loop after its very first attempt. Only Manager.Stop
		// should end the loop, which it does directly via w.cancel(), not
		// through the registry.
		stream := &media.Stream{
			Key:        key,
			SourceType: a.protocol + "_pull",
			SourceAddr: a.url,
			Bus:        a.bus,
		}

		registered := true
		if err := m.registry.Register(stream); err != nil {
			log.Error("register pull stream", "err", err)
			a.stop()
			registered = false
		} else {
			w.setActive(a.url)
			if m.OnLive != nil {
				go m.OnLive(stream)
			}
			log.Info("pull source connecting", "protocol", a.protocol, "url", a.url, "fallback", idx)
			m.attempts.Add(1)

			sctx, scancel := context.WithCancel(ctx)
			var stallC <-chan struct{}
			if hasFallback {
				stallC = watchStall(sctx, a.bus, time.Now(), tm)
			}
			var probeC chan *attempt
			probeDone := make(chan struct{})
			if idx > 0 && source.PrimaryCheckS > 0 {
				probeC = make(chan *attempt, 1)
				go func() {
					defer close(probeDone)
					checkPrimary(sctx, ctx, source, urls[0], time.Duration(source.PrimaryCheckS)*time.Second, probeC, tm)
				}()
			} else {
				close(probeDone)
			}

			select {
			case <-a.done:
				if a.err != nil && ctx.Err() == nil {
					log.Error("pull source ended", "url", a.url, "err", a.err)
					m.failures.Add(1)
				}
			case <-stallC:
				log.Warn("pull source sent no media, giving up on it", "url", a.url, "after", tm.stall)
				m.failures.Add(1)
				a.stop()
			case p := <-probeC:
				log.Info("primary is back, switching to it", "url", p.url)
				next = p
				a.stop()
			case <-ctx.Done():
				a.stop()
			}
			scancel()
			<-probeDone
			if probeC != nil && next == nil {
				select {
				case p := <-probeC: // a probe that finished as we left
					p.stop()
				default:
				}
			}
			w.setActive("")
			m.registry.Unregister(stream)
		}

		if ctx.Err() != nil {
			if next != nil {
				next.stop()
			}
			return // Stop() was called
		}
		if next != nil {
			idx = 0
			continue
		}

		if registered && hasFallback {
			idx++
			if idx < len(urls) {
				continue // try the next fallback straight away
			}
			idx = 0
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

	m.finish(w, source)
}

// retryOrStop decides what follows a failed attempt that was never
// listed: the next fallback URL straight away, else (with reconnect on)
// the first URL again after delay. false means the worker should end.
func (m *Manager) retryOrStop(ctx context.Context, idx *int, n int, hasFallback, reconnect bool, delay time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if hasFallback {
		*idx++
		if *idx < n {
			return true
		}
		*idx = 0
	}
	if !reconnect {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(delay):
		return true
	}
}

type firstMedia int

const (
	firstMediaArrived firstMedia = iota
	firstMediaEnded
	firstMediaTimeout
	firstMediaCancelled
)

// waitFirstMedia waits until a's bus has carried audio/video, a ends, or
// tm.stall passes with nothing. Packets published meanwhile have no
// subscriber yet and are dropped; codec info is kept on the Bus, so the
// outputs started at registration begin at the next keyframe.
func waitFirstMedia(ctx context.Context, a *attempt, tm timing) firstMedia {
	if !a.bus.LastMediaAt().IsZero() {
		return firstMediaArrived
	}
	t := time.NewTicker(tm.poll)
	defer t.Stop()
	deadline := time.NewTimer(tm.stall)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return firstMediaCancelled
		case <-a.done:
			return firstMediaEnded // gone already: never list it
		case <-deadline.C:
			return firstMediaTimeout
		case <-t.C:
			if !a.bus.LastMediaAt().IsZero() {
				return firstMediaArrived
			}
		}
	}
}

// finish clears the worker's entry and marks the source inactive.
func (m *Manager) finish(w *worker, source db.PullSource) {
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

// attempt is one running connection to one URL, publishing onto its own Bus.
type attempt struct {
	url, protocol string
	bus           *media.Bus
	cancel        context.CancelFunc
	done          chan struct{} // closed when the pull function returns
	err           error         // its result; read after done
}

// dial starts pulling u (protocol from its scheme) onto a fresh Bus.
func dial(ctx context.Context, source db.PullSource, u string) *attempt {
	src := source
	src.URL = u
	src.Protocol = ProtocolForURL(u, source.Protocol)
	actx, cancel := context.WithCancel(ctx)
	a := &attempt{url: u, protocol: src.Protocol, bus: media.NewBus(), cancel: cancel, done: make(chan struct{})}
	fn := supportedProtocols[src.Protocol]
	go func() {
		defer close(a.done)
		if fn == nil {
			a.err = fmt.Errorf("pull protocol %q not supported", src.Protocol)
			return
		}
		a.err = fn(actx, src, a.bus)
	}()
	return a
}

// stop cancels the attempt and waits for its pull function to return.
func (a *attempt) stop() {
	a.cancel()
	<-a.done
}

// watchStall returns a channel closed once bus has gone stallTimeout
// without audio/video, counting from start until the first packet.
func watchStall(ctx context.Context, bus *media.Bus, start time.Time, tm timing) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		t := time.NewTicker(tm.poll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				ref := bus.LastMediaAt()
				if ref.IsZero() {
					ref = start
				}
				if now.Sub(ref) > tm.stall {
					close(ch)
					return
				}
			}
		}
	}()
	return ch
}

// checkPrimary tries the primary every interval until a probe succeeds,
// then hands the still-running connection to out and returns. ctx ends the
// checking; connections are made under connCtx (the worker's), since a
// handed-over one must outlive the fallback period that found it.
func checkPrimary(ctx, connCtx context.Context, source db.PullSource, primary string, interval time.Duration, out chan<- *attempt, tm timing) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if a := probe(ctx, connCtx, source, primary, tm); a != nil {
			if ctx.Err() != nil {
				a.stop()
				return
			}
			out <- a // buffered; the run loop stops it if it doesn't take it
			return
		}
	}
}

// probe connects to u and reports the connection if media flows steadily
// for probeSteady within probeTimeout; otherwise it closes it and returns nil.
func probe(ctx, connCtx context.Context, source db.PullSource, u string, tm timing) *attempt {
	a := dial(connCtx, source, u)
	deadline := time.Now().Add(tm.probeTimeout)
	var first time.Time
	t := time.NewTicker(tm.poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			a.stop()
			return nil
		case <-a.done:
			return nil
		case now := <-t.C:
			last := a.bus.LastMediaAt()
			switch {
			case last.IsZero() || now.Sub(last) > time.Second:
				first = time.Time{} // nothing yet, or a gap: start over
			case first.IsZero():
				first = now
			case now.Sub(first) >= tm.probeSteady:
				return a
			}
			if now.After(deadline) {
				a.stop()
				return nil
			}
		}
	}
}
