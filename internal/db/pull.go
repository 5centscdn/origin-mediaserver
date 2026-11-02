package db

import (
	"context"
	"database/sql"
	"errors"
)

// PullSource is the runtime view of a pull_sources row.
type PullSource struct {
	ID              string
	AppName         string
	StreamName      string
	URL             string
	Protocol        string
	OnDemand        bool
	Reconnect       bool
	ReconnectDelayS int
	Status          string
	// Passphrase decrypts an upstream SRT source encrypted with the AES
	// passphrase scheme (Haivision SRT / Nimble's "Passphrase" field).
	// Only meaningful when Protocol == "srt"; empty means unencrypted.
	Passphrase string
	// PBKeylen is the AES key length in bytes (16/24/32); 0 = gosrt default.
	PBKeylen int
}

// GetPullSourceByID returns the pull source for id, or nil if not found.
func (db *DB) GetPullSourceByID(ctx context.Context, id string) (*PullSource, error) {
	var s PullSource
	var onDemand, reconnect int
	err := db.QueryRowContext(ctx, `
		SELECT id, app_name, stream_name, url, protocol, on_demand, reconnect, reconnect_delay_s, status, passphrase, pbkeylen
		FROM pull_sources WHERE id = ?`, id,
	).Scan(&s.ID, &s.AppName, &s.StreamName, &s.URL, &s.Protocol, &onDemand, &reconnect, &s.ReconnectDelayS, &s.Status, &s.Passphrase, &s.PBKeylen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.OnDemand = onDemand == 1
	s.Reconnect = reconnect == 1
	return &s, nil
}

// ListAutoStartPullSources returns every pull source with on_demand=0, for
// starting automatically at boot.
func (db *DB) ListAutoStartPullSources(ctx context.Context) ([]PullSource, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, app_name, stream_name, url, protocol, on_demand, reconnect, reconnect_delay_s, status, passphrase, pbkeylen
		FROM pull_sources WHERE on_demand = 0`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PullSource
	for rows.Next() {
		var s PullSource
		var onDemand, reconnect int
		if err := rows.Scan(&s.ID, &s.AppName, &s.StreamName, &s.URL, &s.Protocol, &onDemand, &reconnect, &s.ReconnectDelayS, &s.Status, &s.Passphrase, &s.PBKeylen); err != nil {
			return nil, err
		}
		s.OnDemand = onDemand == 1
		s.Reconnect = reconnect == 1
		out = append(out, s)
	}
	return out, rows.Err()
}

// SetPullSourceStatus updates a single pull source's status column, reflecting
// the pull manager's actual runtime state (not just an API-requested intent).
func (db *DB) SetPullSourceStatus(ctx context.Context, id, status string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE pull_sources SET status=?, updated_at=datetime('now') WHERE id=?`, status, id)
	return err
}

// ResetAllPullSourceStatus sets every pull source's status column to status —
// used once at boot to clear any 'active' left over from a previous crash,
// before the auto-start loop sets the real ones back to active.
func (db *DB) ResetAllPullSourceStatus(ctx context.Context, status string) error {
	_, err := db.ExecContext(ctx, `UPDATE pull_sources SET status=?`, status)
	return err
}
