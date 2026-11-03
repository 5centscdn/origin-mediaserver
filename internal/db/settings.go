package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// GetSetting decodes the JSON stored under key into v. It reports false,
// leaving v alone, when the key was never set.
func (d *DB) GetSetting(ctx context.Context, key string, v any) (bool, error) {
	var raw string
	err := d.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), v)
}

// SetSetting stores v as JSON under key, replacing any previous value.
func (d *DB) SetSetting(ctx context.Context, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = d.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, string(raw))
	return err
}
