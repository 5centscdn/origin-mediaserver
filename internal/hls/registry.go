package hls

import (
	"sync"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// PlaylistRegistry maps a live stream to its HLS Playlist, so the HTTP
// delivery layer (internal/api, a different package with no other reference
// to a running Packager) can look up the live Playlist object a blocking
// LL-HLS playlist reload needs to wait on.
//
// Deliberately separate from media.Registry rather than merged into it:
// internal/media must not import internal/hls, since internal/hls already
// imports internal/media.
type PlaylistRegistry struct {
	mu    sync.RWMutex
	byKey map[media.StreamKey]*Playlist

	// retired keeps the last playlist of a stream that stopped, so a new
	// source on the same key (a reconnect, or a pull source switching to
	// or from a fallback) can continue it; see Previous.
	retired map[media.StreamKey]retiredPlaylist
}

type retiredPlaylist struct {
	pl *Playlist
	at time.Time
}

// continueWithin is how long after a stream stops its playlist can still
// be continued by the next source on that key. Longer than that, players
// have given up anyway and a fresh playlist is clearer.
const continueWithin = 2 * time.Minute

// NewPlaylistRegistry creates an empty PlaylistRegistry.
func NewPlaylistRegistry() *PlaylistRegistry {
	return &PlaylistRegistry{byKey: make(map[media.StreamKey]*Playlist), retired: make(map[media.StreamKey]retiredPlaylist)}
}

// Register records that key's live playlist is pl.
func (r *PlaylistRegistry) Register(key media.StreamKey, pl *Playlist) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byKey[key] = pl
}

// Unregister removes key's entry, if any.
func (r *PlaylistRegistry) Unregister(key media.StreamKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byKey, key)
}

// Retire removes key's entry if it is still pl, and remembers pl for
// Previous. Identity-checked: when the next source's packager has already
// registered, the old packager finishing late must not remove it.
func (r *PlaylistRegistry) Retire(key media.StreamKey, pl *Playlist) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byKey[key] == pl {
		delete(r.byKey, key)
	}
	r.retired[key] = retiredPlaylist{pl: pl, at: time.Now()}
	for k, v := range r.retired { // drop stale entries
		if time.Since(v.at) > continueWithin {
			delete(r.retired, k)
		}
	}
}

// Previous returns the playlist a new packager for key should continue:
// the one still registered (its packager is finishing), else one retired
// within continueWithin, else nil.
func (r *PlaylistRegistry) Previous(key media.StreamKey) *Playlist {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if pl := r.byKey[key]; pl != nil {
		return pl
	}
	if v, ok := r.retired[key]; ok && time.Since(v.at) <= continueWithin {
		return v.pl
	}
	return nil
}

// Get returns key's live playlist, or nil if the stream isn't currently packaging HLS.
func (r *PlaylistRegistry) Get(key media.StreamKey) *Playlist {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byKey[key]
}
