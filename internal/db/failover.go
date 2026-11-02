package db

import "context"

// FailoverRule is the runtime view of a failover_rules row.
type FailoverRule struct {
	ID             string
	AppName        string
	PrimaryStream  string
	BackupURL      string
	BackupProtocol string
	StallTimeoutMs int
	WarmupMs       int
	Enabled        bool
	AutoRestore    bool
}

// ListEnabledFailoverRules returns every failover rule with enabled=1, for
// loading at startup so existing rules are monitored without waiting for a
// create/update via the API to trigger Watch.
func (db *DB) ListEnabledFailoverRules(ctx context.Context) ([]FailoverRule, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, app_name, primary_stream, backup_url, backup_protocol,
		       stall_timeout_ms, warmup_ms, enabled, auto_restore
		FROM failover_rules WHERE enabled = 1`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FailoverRule
	for rows.Next() {
		var r FailoverRule
		var enabled, autoRestore int
		if err := rows.Scan(
			&r.ID, &r.AppName, &r.PrimaryStream, &r.BackupURL, &r.BackupProtocol,
			&r.StallTimeoutMs, &r.WarmupMs, &enabled, &autoRestore,
		); err != nil {
			return nil, err
		}
		r.Enabled = enabled == 1
		r.AutoRestore = autoRestore == 1
		out = append(out, r)
	}
	return out, rows.Err()
}
