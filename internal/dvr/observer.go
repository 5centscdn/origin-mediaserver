package dvr

import (
	"context"
	"database/sql"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// Observer is told about recording lifecycle and timeline changes, so a
// consumer (internal/dvrowner, nDVR cluster ownership) can keep its view in
// memory instead of querying the index on every request. Calls come from
// recorder goroutines and must not block.
type Observer interface {
	// RecordingStarted/RecordingStopped bracket one recorder run: the
	// stream started or stopped publishing (or its outputs reloaded).
	// Stopped is called after the run's last segment is indexed.
	RecordingStarted(key media.StreamKey)
	RecordingStopped(key media.StreamKey)
	// SegmentIndexed reports a newly indexed segment's UTC timeline span.
	SegmentIndexed(key media.StreamKey, startMS, endMS int64)
	// Pruned reports that cleanup deleted segments, so timelines may have
	// shrunk or gone.
	Pruned()
}

// SetObserver installs o. Call before any recording starts.
func (e *Engine) SetObserver(o Observer) { e.observer = o }

// Timeline is a stream's recorded span on the UTC timeline. Only streams
// with at least one indexed segment have one.
type Timeline struct {
	StartMS, EndMS int64
	Segments       int
}

// Timelines returns every recorded stream's timeline in one grouped query
// over the index.
func (e *Engine) Timelines(ctx context.Context) (map[media.StreamKey]Timeline, error) {
	rows, err := e.database.QueryContext(ctx, `
		SELECT app_name, stream_name, MIN(start_pts_ms), MAX(end_pts_ms), COUNT(*)
		FROM dvr_segments
		WHERE session_ms > 0
		GROUP BY app_name, stream_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[media.StreamKey]Timeline)
	for rows.Next() {
		var k media.StreamKey
		var start, end sql.NullInt64
		var n int
		if err := rows.Scan(&k.App, &k.Name, &start, &end, &n); err != nil {
			return nil, err
		}
		if n > 0 && start.Valid && end.Valid && end.Int64 > start.Int64 {
			out[k] = Timeline{StartMS: start.Int64, EndMS: end.Int64, Segments: n}
		}
	}
	return out, rows.Err()
}
