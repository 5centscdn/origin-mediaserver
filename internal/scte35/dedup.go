package scte35

import (
	"sort"
	"sync"
	"time"
)

// dedupKey identifies one (event_id, command_type, direction) triple for
// retransmission suppression. direction distinguishes an OUT/start message
// from an IN/end message: SCTE-35 legitimately reuses the same event_id for
// both halves of a break, so event_id and command_type alone would
// conflate "IN for event 42" with "duplicate OUT for event 42".
type dedupKey struct {
	id  uint32
	cmd CommandType
	out bool
}

const (
	// dedupTTL bounds how long a seen event id is remembered — long enough
	// to cover typical retransmission windows (encoders commonly repeat a
	// section for several seconds around a splice point) without growing
	// unbounded for a long-lived channel.
	dedupTTL = 2 * time.Hour

	// dedupMaxEntries caps memory use regardless of TTL, satisfying the
	// "bounded queues" performance requirement even if an upstream cycles
	// through event IDs unusually fast.
	dedupMaxEntries = 4096

	dedupGCInterval = time.Minute
)

// dedupTracker is a small bounded seen-set with periodic sweeping, used by
// StateMachine to recognize retransmitted SCTE-35 sections so duplicate
// CUE-OUT/CUE-IN/DATERANGE markers are never emitted for the same event.
// Not used by MPEG-TS pass-through — every received section is forwarded
// verbatim there, since encoders legitimately retransmit for reliability.
type dedupTracker struct {
	mu     sync.Mutex
	seenAt map[dedupKey]time.Time
	lastGC time.Time
}

func newDedupTracker() *dedupTracker {
	return &dedupTracker{
		seenAt: make(map[dedupKey]time.Time),
		lastGC: time.Now(),
	}
}

func (t *dedupTracker) seen(id uint32, cmd CommandType, out bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.seenAt[dedupKey{id: id, cmd: cmd, out: out}]
	return ok
}

func (t *dedupTracker) mark(id uint32, cmd CommandType, out bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seenAt[dedupKey{id: id, cmd: cmd, out: out}] = time.Now()
	t.maybeGCLocked()
}

// maybeGCLocked evicts expired entries, and if still over the cap after
// that, evicts the oldest remaining entries first. Called under t.mu.
func (t *dedupTracker) maybeGCLocked() {
	now := time.Now()
	if now.Sub(t.lastGC) < dedupGCInterval && len(t.seenAt) < dedupMaxEntries {
		return
	}
	t.lastGC = now
	for k, at := range t.seenAt {
		if now.Sub(at) > dedupTTL {
			delete(t.seenAt, k)
		}
	}
	if len(t.seenAt) <= dedupMaxEntries {
		return
	}

	type entry struct {
		k  dedupKey
		at time.Time
	}
	entries := make([]entry, 0, len(t.seenAt))
	for k, at := range t.seenAt {
		entries = append(entries, entry{k, at})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].at.Before(entries[j].at) })
	for i := 0; i < len(entries)-dedupMaxEntries; i++ {
		delete(t.seenAt, entries[i].k)
	}
}
