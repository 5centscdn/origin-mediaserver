package hls

import (
	"sync"

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
}

// NewPlaylistRegistry creates an empty PlaylistRegistry.
func NewPlaylistRegistry() *PlaylistRegistry {
	return &PlaylistRegistry{byKey: make(map[media.StreamKey]*Playlist)}
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

// Get returns key's live playlist, or nil if the stream isn't currently packaging HLS.
func (r *PlaylistRegistry) Get(key media.StreamKey) *Playlist {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byKey[key]
}
