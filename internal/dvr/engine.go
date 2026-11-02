// Package dvr is the nDVR: it records live streams to disk and serves
// rewind playback from the recording — the whole rewind window as a live
// playlist with a seek bar, a fixed time range as VOD, or a fixed-delay
// timeshift — over both HLS and DASH.
//
// Each stream is recorded once, as fMP4. A recorded segment file is the
// video moof+mdat followed by the audio moof+mdat (see recorder.cut), so:
//
//   - HLS serves the file as is, against a two-track init.mp4;
//   - DASH serves the video and audio parts of the same file as byte ranges
//     (video_bytes in the index), against single-track init_v/init_a.mp4,
//     because dash.js and Shaka don't play muxed Representations.
//
// This follows the standalone 5centsCDN DVR Server, which also records fMP4
// once for both protocols, and uses its URL names
// (playlist_dvr_range-…, playlist_dvr_timeshift-…; see names.go).
//
// Timeline: every segment's tfdt is "90 kHz ticks since dvrEpoch", anchored
// to wall-clock time when a recording session starts and advanced by the
// stream's own sample durations (PTS domain) after that, so it is
// continuous within a session and never goes backwards across sessions
// (see recorder.startSession). That one timeline gives HLS its
// PROGRAM-DATE-TIME, DASH its SegmentTimeline, and range requests their
// UTC → segment mapping. A session is one uninterrupted recorder run; a
// publisher reconnect, an output reload or a DTS jump starts a new one,
// which HLS marks with EXT-X-DISCONTINUITY and DASH with a new Period.
//
// Scope: H.264 video and AAC audio, in any combination including audio
// only. HEVC isn't recorded (internal/mux's fMP4 writer has no hvc1 sample
// entry yet) and neither are SCTE-35 markers (no emsg support; same gap as
// fMP4 HLS). Other audio codecs are dropped and the video still recorded.
//
// The segment index lives in the main SQLite DB (dvr_sessions,
// dvr_segments — see internal/db schema v11).
package dvr

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
)

// dvrEpochMS is tfdt zero: 2020-01-01T00:00:00Z. Anchoring to the Unix
// epoch instead would put 90 kHz timestamps past 2^53, which JavaScript
// players (hls.js, dash.js) can't represent exactly.
const dvrEpochMS int64 = 1577836800000

// dvrEpochXSD is dvrEpochMS as an MPD availabilityStartTime.
const dvrEpochXSD = "2020-01-01T00:00:00Z"

// Engine records live streams to disk and generates rewind playlists.
type Engine struct {
	cfg      *config.Config
	database *db.DB
	metrics  *metrics.Metrics
	log      *slog.Logger

	mu sync.Mutex
	// windows is each stream's resolved rewind window (seconds), set by
	// StartRecording, so cleanup and the live DVR playlist honour app/
	// stream overrides. Keyed by StreamKey, not *media.Stream: it's config
	// that must outlive the stream (cleanup keeps running after the
	// publisher leaves), and a reconnect resolving it again is correct.
	windows map[media.StreamKey]int
	// writers is the done channel of the most recent recorder for a key.
	// A new recorder for the same key waits on it before opening its
	// session, so the previous one's last segment is indexed first (see
	// recorder.run) — again keyed by StreamKey on purpose: it orders writes
	// to one storage directory/index key across *media.Stream instances.
	writers map[media.StreamKey]chan struct{}
}

// Options is a stream's resolved nDVR settings, from streamoutputs.
type Options struct {
	// WindowS is how far back viewers can rewind; segments older than this
	// are deleted. 0 = config.toml's [dvr] window_seconds.
	WindowS int
	// SegmentDurationS is the target segment length; 0 = [hls]
	// segment_duration_s.
	SegmentDurationS int
}

// NewEngine creates a DVR engine.
func NewEngine(cfg *config.Config, database *db.DB, m *metrics.Metrics) *Engine {
	return &Engine{
		cfg:      cfg,
		database: database,
		metrics:  m,
		log:      slog.With("component", "dvr"),
		windows:  make(map[media.StreamKey]int),
		writers:  make(map[media.StreamKey]chan struct{}),
	}
}

// StartRecording begins recording the stream's Bus to disk. It runs until
// ctx is cancelled or the Bus closes.
func (e *Engine) StartRecording(ctx context.Context, stream *media.Stream, opts Options) {
	if opts.WindowS <= 0 {
		opts.WindowS = e.cfg.DVRWindowSeconds()
	}
	if opts.SegmentDurationS <= 0 {
		opts.SegmentDurationS = e.cfg.HLS.SegmentDurationS
	}
	if opts.SegmentDurationS <= 0 {
		opts.SegmentDurationS = 4
	}

	done := make(chan struct{})
	e.mu.Lock()
	e.windows[stream.Key] = opts.WindowS
	prev := e.writers[stream.Key]
	e.writers[stream.Key] = done
	e.mu.Unlock()

	r := newRecorder(e, stream, opts)
	go r.run(ctx, prev, done)
}

// Window returns the rewind window (seconds) in effect for app/stream.
func (e *Engine) Window(app, stream string) int {
	e.mu.Lock()
	w, ok := e.windows[media.StreamKey{App: app, Name: stream}]
	e.mu.Unlock()
	if ok && w > 0 {
		return w
	}
	return e.cfg.DVRWindowSeconds()
}

// streamDir is where app/stream's sessions are stored.
func (e *Engine) streamDir(key media.StreamKey) string {
	return filepath.Join(e.cfg.DVR.StoragePath, key.App, key.Name)
}

// CleanupOldSegments deletes, for every recorded stream, segments that have
// fallen out of its rewind window, then any session left with no segments.
// Called periodically from main.go.
func (e *Engine) CleanupOldSegments(ctx context.Context) error {
	rows, err := e.database.QueryContext(ctx,
		`SELECT DISTINCT app_name, stream_name FROM dvr_segments`)
	if err != nil {
		return err
	}
	var keys []media.StreamKey
	for rows.Next() {
		var k media.StreamKey
		if rows.Scan(&k.App, &k.Name) == nil {
			keys = append(keys, k)
		}
	}
	rows.Close()

	now := time.Now().UnixMilli()
	total := 0
	for _, k := range keys {
		cutoff := now - int64(e.Window(k.App, k.Name))*1000
		n, err := e.deleteSegmentsBefore(ctx, k, cutoff)
		if err != nil {
			e.log.Warn("cleanup segments", "stream", k, "err", err)
			continue
		}
		total += n
	}
	if err := e.deleteEmptySessions(ctx, now); err != nil {
		e.log.Warn("cleanup sessions", "err", err)
	}
	if total > 0 {
		e.log.Info("cleaned up DVR segments", "count", total)
	}
	return nil
}

func (e *Engine) deleteSegmentsBefore(ctx context.Context, k media.StreamKey, cutoffMS int64) (int, error) {
	rows, err := e.database.QueryContext(ctx,
		`SELECT id, file_path FROM dvr_segments
		 WHERE app_name = ? AND stream_name = ? AND end_pts_ms < ?`,
		k.App, k.Name, cutoffMS)
	if err != nil {
		return 0, err
	}
	var ids []any
	var paths []string
	for rows.Next() {
		var id, path string
		if rows.Scan(&id, &path) == nil {
			ids = append(ids, id)
			paths = append(paths, path)
		}
	}
	rows.Close()
	if len(ids) == 0 {
		return 0, nil
	}

	// Rows first: a row without a file 404s one request, while a file
	// without a row would never be cleaned up.
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	if _, err := e.database.ExecContext(ctx,
		`DELETE FROM dvr_segments WHERE id IN (`+placeholders+`)`, ids...); err != nil {
		return 0, err
	}
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			e.log.Warn("cleanup: delete file", "path", p, "err", err)
		}
	}
	return len(ids), nil
}

// deleteEmptySessions removes sessions with no segments left, with their
// init segments and directory. A session that has only just started
// legitimately has no segments yet, so only sessions older than a minute
// are considered.
func (e *Engine) deleteEmptySessions(ctx context.Context, nowMS int64) error {
	rows, err := e.database.QueryContext(ctx, `
		SELECT s.app_name, s.stream_name, s.session_ms, s.dir_path
		FROM dvr_sessions s
		WHERE s.session_ms < ? AND NOT EXISTS (
			SELECT 1 FROM dvr_segments g
			WHERE g.app_name = s.app_name AND g.stream_name = s.stream_name
			  AND g.session_ms = s.session_ms)`, nowMS-60_000)
	if err != nil {
		return err
	}
	type sess struct {
		key       media.StreamKey
		sessionMS int64
		dir       string
	}
	var list []sess
	for rows.Next() {
		var s sess
		if rows.Scan(&s.key.App, &s.key.Name, &s.sessionMS, &s.dir) == nil {
			list = append(list, s)
		}
	}
	rows.Close()

	root, _ := filepath.Abs(e.cfg.DVR.StoragePath)
	for _, s := range list {
		if _, err := e.database.ExecContext(ctx,
			`DELETE FROM dvr_sessions WHERE app_name = ? AND stream_name = ? AND session_ms = ?`,
			s.key.App, s.key.Name, s.sessionMS); err != nil {
			return err
		}
		// dir_path is always one we built under the storage root, but it
		// comes back from the DB — never RemoveAll anything outside it.
		if abs, err := filepath.Abs(s.dir); err == nil && root != "" &&
			strings.HasPrefix(abs, root+string(filepath.Separator)) {
			if err := os.RemoveAll(abs); err != nil {
				e.log.Warn("cleanup: delete session dir", "dir", abs, "err", err)
			}
		}
	}
	return nil
}

// StreamInfo returns info for DVR API responses.
func (e *Engine) StreamInfo(ctx context.Context, app, streamName string) (*StreamDVRInfo, error) {
	info := StreamDVRInfo{
		App:     app,
		Stream:  streamName,
		WindowS: e.Window(app, streamName),
	}

	row := e.database.QueryRowContext(ctx,
		`SELECT MIN(start_pts_ms), MAX(end_pts_ms), COUNT(*), SUM(size_bytes)
		 FROM dvr_segments
		 WHERE app_name = ? AND stream_name = ? AND session_ms > 0`,
		app, streamName,
	)

	var minMS, maxMS, count, sizeBytes sql.NullInt64
	if err := row.Scan(&minMS, &maxMS, &count, &sizeBytes); err != nil {
		return nil, err
	}

	if minMS.Valid {
		info.OldestMS = minMS.Int64
		info.NewestMS = maxMS.Int64
		info.SegmentCount = int(count.Int64)
		info.TotalBytes = sizeBytes.Int64
	}

	base := "/" + app + "/" + streamName + "/"
	info.URLs = map[string]string{
		"hls":           base + "playlist_dvr.m3u8",
		"dash":          base + "manifest_dvr.mpd",
		"hls_range":     base + "playlist_dvr_range-{from_unix}-{length_s}.m3u8",
		"dash_range":    base + "manifest_dvr_range-{from_unix}-{length_s}.mpd",
		"hls_timeshift": base + "playlist_dvr_timeshift-{shift_s}[-{depth_s}].m3u8",
	}
	return &info, nil
}

// StreamDVRInfo is returned by the DVR API.
type StreamDVRInfo struct {
	App          string            `json:"app"`
	Stream       string            `json:"stream"`
	OldestMS     int64             `json:"oldest_ms"`
	NewestMS     int64             `json:"newest_ms"`
	SegmentCount int               `json:"segment_count"`
	TotalBytes   int64             `json:"total_bytes"`
	WindowS      int               `json:"window_s"`
	URLs         map[string]string `json:"urls"`
}

func (e *Engine) updateMetrics(ctx context.Context, key media.StreamKey) {
	if e.metrics == nil {
		return
	}
	info, err := e.StreamInfo(ctx, key.App, key.Name)
	if err != nil {
		return
	}
	e.metrics.DVRSegmentsStored.WithLabelValues(key.App, key.Name).Set(float64(info.SegmentCount))
	e.metrics.DVRStorageBytes.WithLabelValues(key.App, key.Name).Set(float64(info.TotalBytes))
}

// msToTicks/ticksToMS convert between UTC milliseconds and the tfdt
// timeline (90 kHz since dvrEpoch).
func msToTicks(ms int64) int64 { return (ms - dvrEpochMS) * 90 }
func ticksToMS(t int64) int64  { return dvrEpochMS + t/90 }

var errNotFound = fmt.Errorf("dvr: not found")
