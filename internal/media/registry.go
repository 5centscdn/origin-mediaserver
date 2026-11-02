package media

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// StreamKey is the unique identity of a live stream.
type StreamKey struct {
	App  string
	Name string
}

func (k StreamKey) String() string { return k.App + "/" + k.Name }

// Stream holds the runtime state of one active stream.
type Stream struct {
	Key        StreamKey
	Bus        *Bus
	CodecInfo  *CodecInfo
	SourceType string // rtmp | srt | rtsp | mpegts | icecast | failover_backup
	SourceAddr string
	StartedAt  time.Time

	// StopFunc, if set, is called by Registry.Unregister in addition to
	// closing the Bus. RTMP-ingest streams don't need it (teardown is
	// TCP-disconnect-driven); streams with an active background goroutine
	// that isn't otherwise tied to a connection — e.g. a failover backup
	// pull — use it to cancel that goroutine when evicted or torn down.
	StopFunc func()

	// ABR group this stream belongs to, or "" for standalone streams.
	ABRGroup string
	// ABR variant index within the group, or 0 for standalone streams.
	ABRVariantIndex int
	// ABRLabel is the human-readable variant name (e.g. "720p"), used in
	// master playlist NAME attributes before/without live CodecInfo.
	ABRLabel string
	// ABRBandwidthHint and ABRHeightHint come from the config-defined ABR
	// pattern (config.ABRVariantHint) and are used as master playlist
	// BANDWIDTH/RESOLUTION fallbacks until the stream's own CodecInfo/
	// bitrate become known.
	ABRBandwidthHint int
	ABRHeightHint    int

	// Viewers tracks real concurrent HLS/DASH viewers of this stream (see
	// ViewerTracker) — distinct from Bus's subscriber count, which counts
	// internal packet consumers (HLS packager, DVR recorder, etc.) rather
	// than actual player connections. Initialized by Register.
	Viewers *ViewerTracker

	// outputsMu guards lifetimeCtx and outputsCancel — both are written
	// once at stream-registration time and potentially again later by a
	// "reload this stream's outputs" request (see internal/streamoutputs),
	// which runs on a different goroutine than the one that started the
	// stream.
	outputsMu sync.Mutex
	// lifetimeCtx is the stream's overall output-pipeline lifetime context
	// (cancelled when the stream's Bus closes, i.e. its ingest connection
	// ends) — set once by streamoutputs.Start. A later Reload re-parents
	// its fresh output generation on this same context, never a new one,
	// so a reload can never outlive the stream itself.
	lifetimeCtx context.Context
	// outputsCancel cancels the *current* generation of output goroutines
	// (HLS/DASH packagers, DVR recording) — NOT the stream's ingest
	// connection or its registry entry. Replaced on every Start/Reload.
	outputsCancel context.CancelFunc
}

// SetLifetimeCtx records ctx as this stream's overall output-pipeline
// lifetime context. Called once by streamoutputs.Start.
func (s *Stream) SetLifetimeCtx(ctx context.Context) {
	s.outputsMu.Lock()
	s.lifetimeCtx = ctx
	s.outputsMu.Unlock()
}

// LifetimeCtx returns the context set by SetLifetimeCtx, or nil if the
// stream's output pipeline was never started (shouldn't happen for any
// stream returned by Registry.Get/All).
func (s *Stream) LifetimeCtx() context.Context {
	s.outputsMu.Lock()
	defer s.outputsMu.Unlock()
	return s.lifetimeCtx
}

// SetOutputsCancel records cancel as the function that stops the current
// generation of output goroutines. Called by streamoutputs.Start/Reload.
func (s *Stream) SetOutputsCancel(cancel context.CancelFunc) {
	s.outputsMu.Lock()
	s.outputsCancel = cancel
	s.outputsMu.Unlock()
}

// CancelOutputs stops whatever output generation is currently running for
// this stream (a no-op if none has ever been started). It does not touch
// the stream's ingest connection or its registry entry.
func (s *Stream) CancelOutputs() {
	s.outputsMu.Lock()
	cancel := s.outputsCancel
	s.outputsMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Registry is the in-process store of all currently active streams.
// It is the source of truth for what is live on this node.
type Registry struct {
	mu      sync.RWMutex
	streams map[StreamKey]*Stream
	// ABR groups: group name → list of variant stream keys (ordered by index)
	abrGroups map[StreamKey][]*Stream // keyed by {app, group name}
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		streams:   make(map[StreamKey]*Stream),
		abrGroups: make(map[StreamKey][]*Stream),
	}
}

// Register adds a new stream to the registry and creates its MediaBus.
// Returns an error if a stream with the same key is already active.
func (r *Registry) Register(s *Stream) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.streams[s.Key]; exists {
		return fmt.Errorf("stream %s is already publishing", s.Key)
	}

	if s.Bus == nil {
		s.Bus = NewBus()
	}
	if s.Viewers == nil {
		s.Viewers = NewViewerTracker()
	}
	if s.StartedAt.IsZero() {
		s.StartedAt = time.Now()
	}

	r.streams[s.Key] = s

	if s.ABRGroup != "" {
		gk := StreamKey{App: s.Key.App, Name: s.ABRGroup}
		r.abrGroups[gk] = append(r.abrGroups[gk], s)
	}

	return nil
}

// Unregister removes stream from the registry, closes its bus, and — if
// set — calls its StopFunc to tear down any background goroutine (e.g. a
// failover backup pull) that isn't otherwise tied to a connection.
//
// It only acts if stream is still the currently-registered stream at its
// key — identity, not just key equality. This matters when a stream has
// already been evicted and replaced (e.g. a stalled primary force-evicted
// by failover, or a failover backup preempted by a reconnecting primary):
// the original owner's own cleanup path may still call Unregister later
// with its now-stale *Stream, and that must be a safe no-op rather than
// tearing down whatever new stream has since taken the key.
func (r *Registry) Unregister(stream *Stream) {
	r.mu.Lock()

	current, ok := r.streams[stream.Key]
	if !ok || current != stream {
		r.mu.Unlock()
		return
	}

	current.Bus.Close()
	delete(r.streams, stream.Key)

	if current.ABRGroup != "" {
		gk := StreamKey{App: current.Key.App, Name: current.ABRGroup}
		group := r.abrGroups[gk]
		for i, gs := range group {
			if gs == current {
				r.abrGroups[gk] = append(group[:i], group[i+1:]...)
				break
			}
		}
		if len(r.abrGroups[gk]) == 0 {
			delete(r.abrGroups, gk)
		}
	}

	r.mu.Unlock()

	if current.StopFunc != nil {
		current.StopFunc()
	}
}

// TouchViewer records viewer activity for the stream at key, if it is
// currently registered. A no-op otherwise (e.g. a request racing the
// stream's teardown) — there's nothing to attribute the view to.
func (r *Registry) TouchViewer(key StreamKey, clientID string) {
	r.mu.RLock()
	s := r.streams[key]
	r.mu.RUnlock()
	if s != nil {
		s.Viewers.Touch(clientID)
	}
}

// Get returns the stream for the given key, or nil if not found.
func (r *Registry) Get(key StreamKey) *Stream {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.streams[key]
}

// GetABRGroup returns all variant streams for app's ABR group, ordered by
// variant index ascending.  Returns nil if the group does not exist. Groups
// are per app: live/camera and events/camera are different groups.
func (r *Registry) GetABRGroup(app, groupName string) []*Stream {
	r.mu.RLock()
	defer r.mu.RUnlock()
	// Return a copy so callers don't race with modifications.
	src := r.abrGroups[StreamKey{App: app, Name: groupName}]
	if src == nil {
		return nil
	}
	out := make([]*Stream, len(src))
	copy(out, src)
	sort.Slice(out, func(i, j int) bool { return out[i].ABRVariantIndex < out[j].ABRVariantIndex })
	return out
}

// All returns a snapshot of all active streams.
func (r *Registry) All() []*Stream {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Stream, 0, len(r.streams))
	for _, s := range r.streams {
		out = append(out, s)
	}
	return out
}

// AllABRGroups returns a snapshot of all active ABR groups as "app/group".
func (r *Registry) AllABRGroups() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.abrGroups))
	for gk := range r.abrGroups {
		names = append(names, gk.String())
	}
	return names
}
