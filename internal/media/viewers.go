package media

import (
	"sync"
	"time"
)

// viewerTTL is how long a client is still counted as an active viewer after
// its last playlist/segment request. HLS/DASH delivery is pull-based HTTP
// with no connect/disconnect event, so presence has to be inferred from
// recent activity — this comfortably covers a client polling every segment
// (typically every 4-6s) plus network jitter, without still counting
// someone whose player was closed a while ago.
const viewerTTL = 20 * time.Second

// ViewerTracker counts distinct concurrent real viewers of a stream's
// HLS/DASH output. Unlike Bus.SubscriberCount (which counts internal
// per-stream packet consumers — the HLS packager, DVR recorder, DASH
// packager — present for every live stream whether or not anyone is
// watching), this counts real client requests, deduplicated by client ID
// and expired by a TTL.
type ViewerTracker struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// NewViewerTracker creates an empty tracker.
func NewViewerTracker() *ViewerTracker {
	return &ViewerTracker{seen: make(map[string]time.Time)}
}

// Touch records activity from clientID (e.g. derived from remote IP +
// User-Agent) at the current time.
func (v *ViewerTracker) Touch(clientID string) {
	if clientID == "" {
		return
	}
	v.mu.Lock()
	v.seen[clientID] = time.Now()
	v.mu.Unlock()
}

// Count returns the number of distinct clients seen within the last
// viewerTTL, pruning expired entries as a side effect.
func (v *ViewerTracker) Count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	n := 0
	for id, t := range v.seen {
		if now.Sub(t) > viewerTTL {
			delete(v.seen, id)
			continue
		}
		n++
	}
	return n
}
