// Package livestore keeps live HLS and DASH output (playlists, manifests,
// init segments, media segments and LL-HLS parts) in memory, the way
// Nimble does: a live origin serves the same few seconds of media to every
// viewer, so writing each segment to disk and reading it back per request
// only costs disk I/O. DVR recordings (internal/dvr) stay on disk.
//
// Files are addressed by the same paths the packagers used to write
// (<data_dir>/hls/<app>/<stream>/seg000123.ts, ...). In memory mode
// nothing is written to those paths.
//
// Modes ([live_cache] mode, set by StartMonitor in memory.go):
//   - hybrid (default): memory running out must not get the server
//     OOM-killed (every stream would drop), so at the high mark the store
//     switches to disk: everything in RAM is written out to its path and
//     dropped from memory, and new files go straight to disk. Below the low
//     mark new files go to RAM again, and the ones on disk are removed as
//     they expire.
//   - memory: always RAM; the high mark only raises a warning.
//   - disk: always disk.
//
// Status reports the mode and any warning, which GET /api/v1/node and the
// panel show.
//
// Retention:
//   - Playlists, manifests and init segments (pinned by name, see
//     isPinned) live until the stream's packager releases the directory
//     and a grace period passes without the directory being written again
//     (a reconnect or pull fallback switch writes them again).
//   - Media segments and parts expire TTL after they're written. Each
//     packager sets its directory's TTL from its own playlist window (see
//     SetTTL), so a segment outlives the last playlist that lists it. DASH
//     also removes segments as they leave its window.
package livestore

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultTTL applies to a directory nobody has called SetTTL for.
const DefaultTTL = 5 * time.Minute

// ReleaseGrace is how long a released directory's playlists and init
// segments stay after the packager stops: players fetching the last
// segments still get the playlist, and a source that comes back within it
// (reconnect, pull fallback) takes the directory over again. Matches
// hls.PlaylistRegistry's continueWithin.
const ReleaseGrace = 2 * time.Minute

type file struct {
	data    []byte // nil when onDisk
	onDisk  bool   // stored at its own path on disk
	size    int64
	written time.Time
}

type dir struct {
	files    map[string]*file
	ttl      time.Duration
	released time.Time // zero while a packager owns it
}

// Store is a tree of live output files, in memory or (in disk mode) on
// disk at their own paths.
type Store struct {
	mu       sync.RWMutex
	dirs     map[string]*dir
	diskMode bool
	since    time.Time // when diskMode last changed
	memPct   float64   // last memory reading, -1 = unknown
	highPct  int
	lowPct   int
	policy   string // "hybrid", "memory" or "disk" (StartMonitor)
	now      func() time.Time
}

// New returns an empty store in memory mode. Call Run to expire old
// entries and StartMonitor to switch to disk under memory pressure.
func New() *Store {
	return &Store{dirs: map[string]*dir{}, now: time.Now, memPct: -1, policy: "hybrid"}
}

var std = New()

// Default is the process-wide store the packagers write to and delivery
// reads from.
func Default() *Store { return std }

func split(path string) (string, string) {
	path = filepath.Clean(path)
	return filepath.Dir(path), filepath.Base(path)
}

// isPinned reports whether name is a playlist, manifest or init segment:
// files that are rewritten in place (or written once) and must stay for the
// stream's lifetime rather than expire like media segments.
func isPinned(name string) bool {
	return strings.HasSuffix(name, ".m3u8") || strings.HasSuffix(name, ".mpd") || strings.HasPrefix(name, "init")
}

func (s *Store) dirLocked(d string) *dir {
	dd := s.dirs[d]
	if dd == nil {
		dd = &dir{files: map[string]*file{}, ttl: DefaultTTL}
		s.dirs[d] = dd
	}
	return dd
}

// writeDisk writes data to path atomically (temp file + rename), so a
// reader never sees a partial segment or playlist.
func writeDisk(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// WriteFile stores data at path, replacing any previous content: a copy
// in memory, or the file itself in disk mode. It's atomic for readers:
// they get either the old or the new bytes.
func (s *Store) WriteFile(path string, data []byte) {
	path = filepath.Clean(path)
	d, name := split(path)
	s.mu.RLock()
	toDisk := s.diskMode
	s.mu.RUnlock()

	f := &file{size: int64(len(data)), written: s.now()}
	if toDisk {
		if err := writeDisk(path, data); err != nil {
			slog.Error("live cache: disk write failed, keeping in memory", "path", path, "err", err)
			toDisk = false
		}
	}
	if toDisk {
		f.onDisk = true
	} else {
		f.data = append([]byte(nil), data...) // callers reuse their buffers
	}

	s.mu.Lock()
	dd := s.dirLocked(d)
	old := dd.files[name]
	dd.files[name] = f
	dd.released = time.Time{}
	s.mu.Unlock()
	if old != nil && old.onDisk && !f.onDisk {
		os.Remove(path) // back in memory: the disk copy is stale
	}
}

func (s *Store) lookup(path string) *file {
	d, name := split(path)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if dd := s.dirs[d]; dd != nil {
		return dd.files[name]
	}
	return nil
}

// ReadFile returns the bytes stored at path. The slice must not be
// modified. Used for playlists and manifests; segments go through Open.
func (s *Store) ReadFile(path string) ([]byte, bool) {
	f := s.lookup(path)
	if f == nil {
		return nil, false
	}
	if !f.onDisk {
		return f.data, true
	}
	data, err := os.ReadFile(filepath.Clean(path))
	return data, err == nil
}

// Open returns a reader over the content stored at path and its size, for
// http.ServeContent. Close it when done.
func (s *Store) Open(path string) (io.ReadSeekCloser, int64, bool) {
	f := s.lookup(path)
	if f == nil {
		return nil, 0, false
	}
	if !f.onDisk {
		return nopCloser{bytes.NewReader(f.data)}, f.size, true
	}
	fh, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, 0, false // expired between lookup and open
	}
	return fh, f.size, true
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }

// Exists reports whether path is stored.
func (s *Store) Exists(path string) bool {
	return s.lookup(path) != nil
}

// HasDir reports whether directory d is known: a packager has set it up
// (SetTTL) or written to it, and it hasn't been expired.
func (s *Store) HasDir(d string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dirs[filepath.Clean(d)] != nil
}

// List returns the names of the files stored in directory d, unsorted.
func (s *Store) List(d string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var names []string
	if dd := s.dirs[filepath.Clean(d)]; dd != nil {
		for n := range dd.files {
			names = append(names, n)
		}
	}
	return names
}

// Remove deletes path; a missing path is not an error.
func (s *Store) Remove(path string) {
	path = filepath.Clean(path)
	d, name := split(path)
	s.mu.Lock()
	var f *file
	if dd := s.dirs[d]; dd != nil {
		f = dd.files[name]
		delete(dd.files, name)
	}
	s.mu.Unlock()
	if f != nil && f.onDisk {
		os.Remove(path)
	}
}

// SetTTL sets how long media segments in directory d live after they're
// written.
func (s *Store) SetTTL(d string, ttl time.Duration) {
	s.mu.Lock()
	s.dirLocked(filepath.Clean(d)).ttl = ttl
	s.mu.Unlock()
}

// Release marks directory d (and its subdirectories) as no longer written
// by a packager. Its pinned files go after ReleaseGrace unless something
// writes to the directory again first.
func (s *Store) Release(d string) {
	d = filepath.Clean(d)
	s.mu.Lock()
	now := s.now()
	for name, dd := range s.dirs {
		if name == d || strings.HasPrefix(name, d+string(filepath.Separator)) {
			dd.released = now
		}
	}
	s.mu.Unlock()
}

// Expire drops expired segments, and released directories' files once the
// grace period has passed. Run calls it periodically.
func (s *Store) Expire() {
	var unlink []string
	s.mu.Lock()
	now := s.now()
	for name, dd := range s.dirs {
		gone := !dd.released.IsZero() && now.Sub(dd.released) >= ReleaseGrace
		for fn, f := range dd.files {
			if gone || (!isPinned(fn) && now.Sub(f.written) >= dd.ttl) {
				delete(dd.files, fn)
				if f.onDisk {
					unlink = append(unlink, filepath.Join(name, fn))
				}
			}
		}
		if len(dd.files) == 0 && (gone || !dd.released.IsZero()) {
			delete(s.dirs, name)
		}
	}
	s.mu.Unlock()
	for _, p := range unlink {
		os.Remove(p)
	}
}

// Stats returns the number of stored files and their total size, in
// memory and on disk.
func (s *Store) Stats() (memFiles int, memBytes int64, diskFiles int, diskBytes int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, dd := range s.dirs {
		for _, f := range dd.files {
			if f.onDisk {
				diskFiles++
				diskBytes += f.size
			} else {
				memFiles++
				memBytes += f.size
			}
		}
	}
	return
}

// setDiskMode switches modes. Turning disk mode on also moves everything
// already in memory to disk, which is what frees the memory.
func (s *Store) setDiskMode(on bool) {
	s.mu.Lock()
	if s.diskMode == on {
		s.mu.Unlock()
		return
	}
	s.diskMode = on
	s.since = s.now()
	type item struct {
		path string
		f    *file
	}
	var spill []item
	if on {
		for name, dd := range s.dirs {
			for fn, f := range dd.files {
				if !f.onDisk {
					spill = append(spill, item{filepath.Join(name, fn), f})
				}
			}
		}
	}
	s.mu.Unlock()

	moved := 0
	for _, it := range spill {
		if err := writeDisk(it.path, it.f.data); err != nil {
			slog.Error("live cache: moving to disk failed", "path", it.path, "err", err)
			continue
		}
		d, name := split(it.path)
		s.mu.Lock()
		if dd := s.dirs[d]; dd != nil && dd.files[name] == it.f {
			// Swap in a new entry rather than mutate: readers may hold it.
			dd.files[name] = &file{onDisk: true, size: it.f.size, written: it.f.written}
			moved++
		} else {
			os.Remove(it.path) // replaced or expired meanwhile
		}
		s.mu.Unlock()
	}
	if on {
		slog.Info("live cache: moved to disk", "files", moved)
	}
}

// Run calls Expire every interval until stop is closed.
func (s *Store) Run(stop <-chan struct{}, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.Expire()
		}
	}
}

func init() {
	go std.Run(make(chan struct{}), 5*time.Second)
}
