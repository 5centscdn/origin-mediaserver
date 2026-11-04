// Package dvrowner decides which cluster node serves an nDVR stream's
// playlists, so a CDN in front of several origins (NGINX trying them in
// turn, moving on at a 404) always lands on the right one.
//
// Only streams with nDVR enabled take part. For every other stream, and
// whenever multinode is off, Check answers Bypass after one in-memory
// lookup and nothing is synced, polled or checked.
//
// For an nDVR stream the owner is (see decide/beats):
//
//  1. the node receiving the encoder; if several are, the configured
//     [multinode] ownership_policy, then node_priority;
//  2. with no encoder anywhere, the node whose recording ends latest on
//     the recording's own timeline (dvr_segments), node_priority on a tie;
//     an empty archive doesn't count;
//  3. nobody, if no node publishes or has a recording (404 everywhere);
//  4. undecidable (503) while it depends on a peer that has stopped
//     heartbeating but isn't forgotten yet, or before this node's cluster
//     view is ready. An unreachable peer is never assumed offline.
//
// State travels in the existing NATS heartbeat (node.Heartbeat.DVR): each
// node lists its nDVR-enabled streams that are publishing or recorded,
// once per heartbeat and immediately when one starts or stops publishing
// (dvr.Observer → node.Manager.Kick). Recording timelines are loaded once
// with one grouped query, then kept current from the recorder's own
// "segment indexed" events; the query is re-run only after cleanup pruned
// segments and every reconcileInterval as a fallback. A playlist request
// costs map lookups only: no DB, file or network access.
//
// Time-range rewind (playlist_dvr_range-*) isn't owned: any node whose own
// recording overlaps the range serves it (CoversRange), so archives on a
// node that lost ownership stay reachable. Recorded segment files are
// immutable and named by session, so they're served wherever they exist.
package dvrowner

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/dvr"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/node"
)

const (
	// eligibilityTTL bounds how long a cached "is nDVR enabled" answer is
	// used. Config writes through the API also clear the cache
	// (InvalidateEligibility), so this only matters for config.toml edits
	// picked up some other way.
	eligibilityTTL = 60 * time.Second
	// eligibilityMax caps the cache, so requests for endless made-up
	// stream names can't grow it without bound.
	eligibilityMax = 100_000
	// reconcileInterval is the fallback re-read of recording timelines.
	reconcileInterval = 2 * time.Minute
)

// Cluster is the part of node.Manager the tracker needs.
type Cluster interface {
	NodeID() string
	NodeName() string
	Priority() int
	Ready() bool
	DVRView(app, stream string) []node.PeerDVR
	Kick()
}

// Deps are the tracker's local sources, injected so tests don't need a DB.
type Deps struct {
	// Eligible resolves whether app/stream has nDVR enabled (DB-backed;
	// the tracker caches it).
	Eligible func(ctx context.Context, app, stream string) bool
	// Live returns the live stream for key, or nil.
	Live func(key media.StreamKey) *media.Stream
	// LiveKeys lists every live stream.
	LiveKeys func() []media.StreamKey
	// Timelines loads every recorded stream's timeline.
	Timelines func(ctx context.Context) (map[media.StreamKey]dvr.Timeline, error)
}

// NewDeps wires Deps to the real registry and DVR engine.
func NewDeps(registry *media.Registry, engine *dvr.Engine, eligible func(ctx context.Context, app, stream string) bool) Deps {
	return Deps{
		Eligible: eligible,
		Live:     registry.Get,
		LiveKeys: func() []media.StreamKey {
			all := registry.All()
			keys := make([]media.StreamKey, len(all))
			for i, s := range all {
				keys[i] = s.Key
			}
			return keys
		},
		Timelines: engine.Timelines,
	}
}

type eligEntry struct {
	on      bool
	expires time.Time
}

// Tracker holds this node's nDVR ownership view. It implements
// dvr.Observer. A nil *Tracker is valid and always answers Bypass, which
// is what single-node mode uses.
type Tracker struct {
	cluster Cluster
	deps    Deps
	policy  string
	log     *slog.Logger

	eligMu sync.RWMutex
	elig   map[media.StreamKey]eligEntry

	tlMu sync.RWMutex
	tl   map[media.StreamKey]dvr.Timeline

	reconcile chan struct{}
}

// New creates a tracker. policy is [multinode] ownership_policy.
func New(cluster Cluster, deps Deps, policy string) *Tracker {
	if policy != PolicyPriority {
		policy = PolicyFirstPublisher
	}
	return &Tracker{
		cluster:   cluster,
		deps:      deps,
		policy:    policy,
		log:       slog.With("component", "dvrowner"),
		elig:      make(map[media.StreamKey]eligEntry),
		tl:        make(map[media.StreamKey]dvr.Timeline),
		reconcile: make(chan struct{}, 1),
	}
}

// Run loads recording timelines, then keeps them reconciled until ctx is
// done.
func (t *Tracker) Run(ctx context.Context) {
	t.reload(ctx)
	tick := time.NewTicker(reconcileInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-t.reconcile:
		}
		t.reload(ctx)
	}
}

func (t *Tracker) reload(ctx context.Context) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tl, err := t.deps.Timelines(qctx)
	if err != nil {
		if ctx.Err() == nil {
			t.log.Warn("load DVR timelines", "err", err)
		}
		return
	}
	t.tlMu.Lock()
	t.tl = tl
	t.tlMu.Unlock()
}

// ─── dvr.Observer ─────────────────────────────────────────────────────────────

// RecordingStarted announces the new publisher to peers at once.
func (t *Tracker) RecordingStarted(media.StreamKey) { t.cluster.Kick() }

// RecordingStopped announces the end of publishing (with the final
// recording end time) to peers at once.
func (t *Tracker) RecordingStopped(media.StreamKey) { t.cluster.Kick() }

// SegmentIndexed extends key's timeline.
func (t *Tracker) SegmentIndexed(key media.StreamKey, startMS, endMS int64) {
	if endMS <= startMS {
		return
	}
	t.tlMu.Lock()
	cur, ok := t.tl[key]
	if !ok {
		cur = dvr.Timeline{StartMS: startMS, EndMS: endMS}
	}
	cur.StartMS = min(cur.StartMS, startMS)
	cur.EndMS = max(cur.EndMS, endMS)
	cur.Segments++
	t.tl[key] = cur
	t.tlMu.Unlock()
}

// Pruned re-reads timelines, since cleanup may have shortened or removed
// some.
func (t *Tracker) Pruned() {
	select {
	case t.reconcile <- struct{}{}:
	default:
	}
}

// ─── eligibility ──────────────────────────────────────────────────────────────

// Eligible reports whether key has nDVR enabled, from the cache when
// possible.
func (t *Tracker) Eligible(ctx context.Context, key media.StreamKey) bool {
	now := time.Now()
	t.eligMu.RLock()
	e, ok := t.elig[key]
	t.eligMu.RUnlock()
	if ok && now.Before(e.expires) {
		return e.on
	}
	on := t.deps.Eligible(ctx, key.App, key.Name)
	t.eligMu.Lock()
	if len(t.elig) >= eligibilityMax {
		t.elig = make(map[media.StreamKey]eligEntry)
	}
	t.elig[key] = eligEntry{on: on, expires: now.Add(eligibilityTTL)}
	t.eligMu.Unlock()
	return on
}

// InvalidateEligibility drops cached answers after an application or
// stream config change.
func (t *Tracker) InvalidateEligibility() {
	if t == nil {
		return
	}
	t.eligMu.Lock()
	t.elig = make(map[media.StreamKey]eligEntry)
	t.eligMu.Unlock()
	t.cluster.Kick()
}

// ─── decisions ────────────────────────────────────────────────────────────────

// Check is the per-request entry point: Bypass for streams without nDVR
// (and on a nil tracker), else this node's ownership verdict.
func (t *Tracker) Check(ctx context.Context, app, stream string) Decision {
	if t == nil {
		return Decision{Verdict: Bypass}
	}
	key := media.StreamKey{App: app, Name: stream}
	if !t.Eligible(ctx, key) {
		return Decision{Verdict: Bypass}
	}
	return decide(t.self(key), t.cluster.DVRView(app, stream), t.policy, t.cluster.Ready())
}

// CoversRange reports whether this node's recording of app/stream overlaps
// [fromMS, toMS): the archive-range policy for explicit time ranges.
func (t *Tracker) CoversRange(app, stream string, fromMS, toMS int64) bool {
	t.tlMu.RLock()
	tl, ok := t.tl[media.StreamKey{App: app, Name: stream}]
	t.tlMu.RUnlock()
	return ok && tl.StartMS < toMS && tl.EndMS > fromMS
}

func (t *Tracker) self(key media.StreamKey) candidate {
	c := candidate{id: t.cluster.NodeID(), name: t.cluster.NodeName(), priority: t.cluster.Priority(), self: true}
	if st := t.deps.Live(key); st != nil {
		c.publishing = true
		c.sinceMS = st.StartedAt.UnixMilli()
	}
	t.tlMu.RLock()
	tl, ok := t.tl[key]
	t.tlMu.RUnlock()
	if ok {
		c.dvrEndMS = tl.EndMS
	}
	return c
}

// Report lists this node's nDVR-enabled streams that are publishing or
// have a recording, for the heartbeat. Streams without nDVR are left out.
func (t *Tracker) Report() []node.DVRState {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.tlMu.RLock()
	keys := make(map[media.StreamKey]dvr.Timeline, len(t.tl))
	for k, v := range t.tl {
		keys[k] = v
	}
	t.tlMu.RUnlock()
	for _, k := range t.deps.LiveKeys() {
		if _, ok := keys[k]; !ok {
			keys[k] = dvr.Timeline{}
		}
	}

	out := make([]node.DVRState, 0, len(keys))
	for k, tl := range keys {
		if !t.Eligible(ctx, k) {
			continue
		}
		st := node.DVRState{App: k.App, Stream: k.Name}
		if live := t.deps.Live(k); live != nil {
			st.Publishing = true
			st.PublishSinceMS = live.StartedAt.UnixMilli()
		}
		if tl.Segments > 0 {
			st.DVRStartMS, st.DVREndMS = tl.StartMS, tl.EndMS
		}
		if st.Publishing || st.DVREndMS > 0 {
			out = append(out, st)
		}
	}
	return out
}
