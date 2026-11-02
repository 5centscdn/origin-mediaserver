package db

import (
	"context"
	"database/sql"
	"errors"
)

// SCTE35Event is the runtime view of a scte35_events row — a durable record
// of one server-generated ad-break event, independent of any in-memory
// scheduling state (see internal/scte35ctrl), so a stream reconnect never
// has to guess whether an event already happened.
type SCTE35Event struct {
	ID                 string
	AppName            string
	StreamName         string
	EventID            uint32
	Command            string // "splice_insert" | "time_signal"
	SegmentationTypeID uint8
	DurationMS         int64
	AutoReturn         bool
	TriggerType        string // "immediate" | "next_keyframe" | "start_pts"
	TriggerPTS         *int64
	Status             string // "pending" | "active" | "completed" | "cancelled"
	OutFiredAt         string
	InFiredAt          string
	CreatedAt          string
}

// InsertSCTE35Event creates a new row, always starting at status="pending".
func (db *DB) InsertSCTE35Event(ctx context.Context, ev SCTE35Event) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO scte35_events
		  (id, app_name, stream_name, event_id, command, segmentation_type_id,
		   duration_ms, auto_return, trigger_type, trigger_pts, status)
		VALUES (?,?,?,?,?,?,?,?,?,?, 'pending')`,
		ev.ID, ev.AppName, ev.StreamName, ev.EventID, ev.Command, ev.SegmentationTypeID,
		ev.DurationMS, boolToInt(ev.AutoReturn), ev.TriggerType, ev.TriggerPTS,
	)
	return err
}

// GetLatestSCTE35Event returns the most recently created row for
// (app, stream, eventID), or nil if none exists — used to decide whether a
// new "out" request would duplicate an already active/completed event (a
// "cancelled" row is not a duplicate; its event_id is free to reuse).
func (db *DB) GetLatestSCTE35Event(ctx context.Context, app, stream string, eventID uint32) (*SCTE35Event, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, app_name, stream_name, event_id, command, segmentation_type_id,
		       duration_ms, auto_return, trigger_type, trigger_pts, status,
		       COALESCE(out_fired_at,''), COALESCE(in_fired_at,''), created_at
		FROM scte35_events
		WHERE app_name = ? AND stream_name = ? AND event_id = ?
		ORDER BY created_at DESC LIMIT 1`, app, stream, eventID)
	return scanSCTE35Event(row)
}

// GetActiveSCTE35Event returns the row for (app, stream, eventID) only if
// its status is "active" — used to validate an "in" request.
func (db *DB) GetActiveSCTE35Event(ctx context.Context, app, stream string, eventID uint32) (*SCTE35Event, error) {
	ev, err := db.GetLatestSCTE35Event(ctx, app, stream, eventID)
	if err != nil || ev == nil || ev.Status != "active" {
		return nil, err
	}
	return ev, nil
}

// ListSCTE35Events returns every row for (app, stream), most recent first.
// includeCompleted controls whether "completed"/"cancelled" rows are
// included (GET .../scte35 defaults to false — only pending/active events
// are "active/pending" in the sense the API describes).
func (db *DB) ListSCTE35Events(ctx context.Context, app, stream string, includeCompleted bool) ([]SCTE35Event, error) {
	query := `
		SELECT id, app_name, stream_name, event_id, command, segmentation_type_id,
		       duration_ms, auto_return, trigger_type, trigger_pts, status,
		       COALESCE(out_fired_at,''), COALESCE(in_fired_at,''), created_at
		FROM scte35_events
		WHERE app_name = ? AND stream_name = ?`
	if !includeCompleted {
		query += ` AND status IN ('pending','active')`
	}
	query += ` ORDER BY created_at DESC`

	rows, err := db.QueryContext(ctx, query, app, stream)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SCTE35Event
	for rows.Next() {
		ev, err := scanSCTE35EventRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ev)
	}
	return out, rows.Err()
}

// MarkSCTE35EventActive transitions a pending row to "active" (the OUT has
// actually fired).
func (db *DB) MarkSCTE35EventActive(ctx context.Context, id string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE scte35_events SET status='active', out_fired_at=datetime('now') WHERE id=?`, id)
	return err
}

// MarkSCTE35EventCompleted transitions an active row to "completed" (the IN
// has fired) — a completed row is permanent; nothing ever moves it back.
func (db *DB) MarkSCTE35EventCompleted(ctx context.Context, id string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE scte35_events SET status='completed', in_fired_at=datetime('now') WHERE id=?`, id)
	return err
}

// MarkSCTE35EventCancelled transitions a pending (or, during stream
// teardown, active) row to "cancelled".
func (db *DB) MarkSCTE35EventCancelled(ctx context.Context, id string) error {
	_, err := db.ExecContext(ctx, `UPDATE scte35_events SET status='cancelled' WHERE id=?`, id)
	return err
}

// CancelOrphanedSCTE35Events marks every still-pending/active row for
// (app, stream) as "cancelled" — called when a stream's connection ends, so
// a later reconnect always starts from a clean schedule (nothing from the
// previous connection's lifetime is ever replayed or left dangling).
func (db *DB) CancelOrphanedSCTE35Events(ctx context.Context, app, stream string) error {
	_, err := db.ExecContext(ctx, `
		UPDATE scte35_events SET status='cancelled'
		WHERE app_name=? AND stream_name=? AND status IN ('pending','active')`,
		app, stream,
	)
	return err
}

func scanSCTE35Event(row *sql.Row) (*SCTE35Event, error) {
	var ev SCTE35Event
	var autoReturn int
	var triggerPTS sql.NullInt64
	err := row.Scan(&ev.ID, &ev.AppName, &ev.StreamName, &ev.EventID, &ev.Command, &ev.SegmentationTypeID,
		&ev.DurationMS, &autoReturn, &ev.TriggerType, &triggerPTS, &ev.Status,
		&ev.OutFiredAt, &ev.InFiredAt, &ev.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ev.AutoReturn = autoReturn == 1
	if triggerPTS.Valid {
		ev.TriggerPTS = &triggerPTS.Int64
	}
	return &ev, nil
}

func scanSCTE35EventRow(rows *sql.Rows) (*SCTE35Event, error) {
	var ev SCTE35Event
	var autoReturn int
	var triggerPTS sql.NullInt64
	err := rows.Scan(&ev.ID, &ev.AppName, &ev.StreamName, &ev.EventID, &ev.Command, &ev.SegmentationTypeID,
		&ev.DurationMS, &autoReturn, &ev.TriggerType, &triggerPTS, &ev.Status,
		&ev.OutFiredAt, &ev.InFiredAt, &ev.CreatedAt)
	if err != nil {
		return nil, err
	}
	ev.AutoReturn = autoReturn == 1
	if triggerPTS.Valid {
		ev.TriggerPTS = &triggerPTS.Int64
	}
	return &ev, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
