// Package db provides the embedded SQLite store for Origin Media Server.
// No external database process is required — the entire schema lives in a
// single file under data_dir.  We use modernc.org/sqlite which is a pure-Go
// transpilation of SQLite; the binary has zero CGO or external .so requirements.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // registers "sqlite" driver
)

// DB wraps *sql.DB with schema migration and helper methods.
type DB struct {
	*sql.DB
}

// Open opens (or creates) the SQLite database at dataDir/origin.db and
// runs all pending schema migrations.
func Open(ctx context.Context, dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	path := filepath.Join(dataDir, "origin.db")
	sqlDB, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// SQLite is single-writer; keep one write connection.
	sqlDB.SetMaxOpenConns(1)

	db := &DB{sqlDB}
	if err := db.migrate(ctx); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	slog.Info("database opened", "path", path)
	return db, nil
}

// migrate applies any not-yet-applied schema migrations in order.
func (db *DB) migrate(ctx context.Context) error {
	// Create the migrations tracking table first.
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version   INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)
	`); err != nil {
		return err
	}

	for _, m := range migrations {
		var exists int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, m.version,
		).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			continue
		}

		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("migration %d: %w", m.version, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version) VALUES (?)`, m.version,
		); err != nil {
			return err
		}
		slog.Debug("applied migration", "version", m.version)
	}
	return nil
}

type migration struct {
	version int
	sql     string
}

var migrations = []migration{
	{1, schemav1},
	{2, schemav2},
	{3, schemav3},
	{4, schemav4},
	{5, schemav5},
	{6, schemav6},
	{7, schemav7},
	{8, schemav8},
	{9, schemav9},
	{10, schemav10},
	{11, schemav11},
	{12, schemav12},
	{13, schemav13},
	{14, schemav14},
	{15, schemav15},
}

// ─── Schema v1: core tables ───────────────────────────────────────────────────

const schemav1 = `
-- Users
CREATE TABLE IF NOT EXISTS users (
    id          TEXT PRIMARY KEY,
    email       TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,
    role        TEXT NOT NULL DEFAULT 'viewer',  -- admin | operator | viewer
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- API tokens (hashed)
CREATE TABLE IF NOT EXISTS api_tokens (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    token_hash  TEXT UNIQUE NOT NULL,
    scopes      TEXT NOT NULL DEFAULT '[]',   -- JSON array of scope strings
    expires_at  TEXT,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- ABR groups
CREATE TABLE IF NOT EXISTS abr_groups (
    id          TEXT PRIMARY KEY,
    name        TEXT UNIQUE NOT NULL,
    app_name    TEXT NOT NULL,
    auto_detect INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- ABR variants within a group
CREATE TABLE IF NOT EXISTS abr_variants (
    id          TEXT PRIMARY KEY,
    group_id    TEXT NOT NULL REFERENCES abr_groups(id) ON DELETE CASCADE,
    stream_name TEXT NOT NULL,           -- e.g. "camera_1" (exact name, %i resolved)
    label       TEXT NOT NULL,           -- e.g. "720p"
    bandwidth   INTEGER NOT NULL DEFAULT 0,
    width       INTEGER NOT NULL DEFAULT 0,
    height      INTEGER NOT NULL DEFAULT 0,
    frame_rate  REAL NOT NULL DEFAULT 0,
    codecs      TEXT NOT NULL DEFAULT '',
    sort_order  INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Stream templates: named stream configurations (can use %i wildcard)
CREATE TABLE IF NOT EXISTS stream_templates (
    id          TEXT PRIMARY KEY,
    app_name    TEXT NOT NULL,
    pattern     TEXT NOT NULL,            -- e.g. "camera_%i" or exact name
    abr_group_id TEXT REFERENCES abr_groups(id),
    dvr_enabled  INTEGER NOT NULL DEFAULT 0,
    dvr_window_s INTEGER NOT NULL DEFAULT 3600,
    settings     TEXT NOT NULL DEFAULT '{}',  -- JSON blob for extras
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(app_name, pattern)
);

-- Live stream state (written by node, read by panel)
CREATE TABLE IF NOT EXISTS active_streams (
    id           TEXT PRIMARY KEY,
    app_name     TEXT NOT NULL,
    stream_name  TEXT NOT NULL,
    source_type  TEXT NOT NULL,  -- rtmp | srt | rtsp | mpegts | icecast
    source_addr  TEXT NOT NULL,
    abr_group_id TEXT REFERENCES abr_groups(id),
    status       TEXT NOT NULL DEFAULT 'publishing',
    codec_info   TEXT NOT NULL DEFAULT '{}',   -- JSON: video/audio codec details
    started_at   TEXT NOT NULL DEFAULT (datetime('now')),
    last_seen_at TEXT NOT NULL DEFAULT (datetime('now')),
    bytes_in     INTEGER NOT NULL DEFAULT 0,
    UNIQUE(app_name, stream_name)
);

-- Pull sources
CREATE TABLE IF NOT EXISTS pull_sources (
    id           TEXT PRIMARY KEY,
    app_name     TEXT NOT NULL,
    stream_name  TEXT NOT NULL,
    url          TEXT NOT NULL,
    protocol     TEXT NOT NULL,  -- rtmp | rtsp | srt | mpegts_http | mpegts_udp | icecast
    on_demand    INTEGER NOT NULL DEFAULT 0,
    reconnect    INTEGER NOT NULL DEFAULT 1,
    reconnect_delay_s INTEGER NOT NULL DEFAULT 5,
    status       TEXT NOT NULL DEFAULT 'inactive',
    settings     TEXT NOT NULL DEFAULT '{}',
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Failover rules
CREATE TABLE IF NOT EXISTS failover_rules (
    id               TEXT PRIMARY KEY,
    app_name         TEXT NOT NULL,
    primary_stream   TEXT NOT NULL,
    backup_url       TEXT NOT NULL,
    backup_protocol  TEXT NOT NULL,
    trigger_type     TEXT NOT NULL DEFAULT 'automatic',  -- automatic | manual
    stall_timeout_ms INTEGER NOT NULL DEFAULT 5000,
    warmup_ms        INTEGER NOT NULL DEFAULT 2000,
    enabled          INTEGER NOT NULL DEFAULT 1,
    state            TEXT NOT NULL DEFAULT 'idle',  -- idle | active_primary | active_backup
    created_at       TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at       TEXT NOT NULL DEFAULT (datetime('now'))
);
`

// ─── Schema v3: Nimble-compatible ABR delivery fields ─────────────────────────
// stream_name on abr_groups = the "ABR stream name" (UUID or slug) used in delivery URLs.
// src_app_name on abr_variants = the source RTMP application for this variant.

const schemav3 = `
ALTER TABLE abr_groups   ADD COLUMN stream_name  TEXT NOT NULL DEFAULT '';
ALTER TABLE abr_variants ADD COLUMN src_app_name TEXT NOT NULL DEFAULT '';
`

// ─── Schema v4: multi-server registry ────────────────────────────────────────

const schemav4 = `
CREATE TABLE IF NOT EXISTS servers (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    url         TEXT NOT NULL UNIQUE,
    note        TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
`

// ─── Schema v5: applications + per-stream configs ────────────────────────────

const schemav5 = `
CREATE TABLE IF NOT EXISTS applications (
    id                 TEXT PRIMARY KEY,
    name               TEXT UNIQUE NOT NULL,
    publish_key        TEXT NOT NULL DEFAULT '',
    enabled            INTEGER NOT NULL DEFAULT 1,
    allow_any_stream   INTEGER NOT NULL DEFAULT 1,
    out_hls            INTEGER NOT NULL DEFAULT 1,
    out_dash           INTEGER NOT NULL DEFAULT 0,
    out_mpegts         INTEGER NOT NULL DEFAULT 0,
    out_ll_hls         INTEGER NOT NULL DEFAULT 0,
    hls_seg_duration_s  INTEGER NOT NULL DEFAULT 0,
    hls_playlist_window INTEGER NOT NULL DEFAULT 0,
    created_at         TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at         TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Per-stream output/DVR config.  name_pattern supports exact names,
-- glob wildcards (*), and FMLE-style %i integer placeholders.
CREATE TABLE IF NOT EXISTS stream_configs (
    id            TEXT PRIMARY KEY,
    app_id        TEXT NOT NULL REFERENCES applications(id) ON DELETE CASCADE,
    name_pattern  TEXT NOT NULL,
    is_wildcard   INTEGER NOT NULL DEFAULT 0,
    -- Protocol overrides: -1 = inherit from app, 0 = disabled, 1 = enabled
    out_hls       INTEGER NOT NULL DEFAULT -1,
    out_dash      INTEGER NOT NULL DEFAULT -1,
    out_mpegts    INTEGER NOT NULL DEFAULT -1,
    out_ll_hls    INTEGER NOT NULL DEFAULT -1,
    dvr_enabled   INTEGER NOT NULL DEFAULT -1,
    dvr_window_s  INTEGER NOT NULL DEFAULT 0,
    abr_group_id  TEXT REFERENCES abr_groups(id),
    enabled       INTEGER NOT NULL DEFAULT 1,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(app_id, name_pattern)
);
`

// ─── Schema v2: DVR segment index ─────────────────────────────────────────────

const schemav2 = `
-- DVR segment index.  One row per stored segment.
-- Per-stream lookup is fast via the (app_name, stream_name, start_pts) index.
CREATE TABLE IF NOT EXISTS dvr_segments (
    id           TEXT PRIMARY KEY,
    app_name     TEXT NOT NULL,
    stream_name  TEXT NOT NULL,
    seq          INTEGER NOT NULL,
    start_pts_ms INTEGER NOT NULL,   -- presentation timestamp, UTC ms
    end_pts_ms   INTEGER NOT NULL,
    duration_ms  INTEGER NOT NULL,
    file_path    TEXT NOT NULL,
    size_bytes   INTEGER NOT NULL DEFAULT 0,
    is_init      INTEGER NOT NULL DEFAULT 0,  -- 1 = fMP4 init segment
    created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_dvr_stream_pts
    ON dvr_segments(app_name, stream_name, start_pts_ms);

CREATE INDEX IF NOT EXISTS idx_dvr_created
    ON dvr_segments(created_at);
`

// ─── Schema v6: failover auto-restore ─────────────────────────────────────────

const schemav6 = `
ALTER TABLE failover_rules ADD COLUMN auto_restore INTEGER NOT NULL DEFAULT 0;
`

// ─── Schema v7: SCTE-35 config ────────────────────────────────────────────────

const schemav7 = `
ALTER TABLE applications ADD COLUMN scte35_passthrough INTEGER NOT NULL DEFAULT 0;
ALTER TABLE applications ADD COLUMN ad_marker_format TEXT NOT NULL DEFAULT 'none';
-- Protocol-override convention: -1 = inherit from app, 0 = off, 1 = on.
ALTER TABLE stream_configs ADD COLUMN scte35_passthrough INTEGER NOT NULL DEFAULT -1;
-- '' = inherit from app; otherwise one of none|cue|splicepoint|daterange.
ALTER TABLE stream_configs ADD COLUMN ad_marker_format TEXT NOT NULL DEFAULT '';
`

// ─── Schema v8: SCTE-35 mode + server-side insertion ─────────────────────────

const schemav8 = `
-- scte35_mode supersedes the schemav7 scte35_passthrough boolean: it's a
-- 4-way choice (disabled|passthrough|insert|hybrid) instead of an on/off
-- switch, since insertion needs a mode distinct from pass-through. The old
-- scte35_passthrough columns are left in place (unused by new resolution
-- logic) rather than dropped, to avoid a risky DROP COLUMN migration.
ALTER TABLE applications ADD COLUMN scte35_mode TEXT NOT NULL DEFAULT 'disabled';
UPDATE applications SET scte35_mode = CASE WHEN scte35_passthrough = 1 THEN 'passthrough' ELSE 'disabled' END;

-- '' = inherit from app; otherwise one of disabled|passthrough|insert|hybrid.
ALTER TABLE stream_configs ADD COLUMN scte35_mode TEXT NOT NULL DEFAULT '';
UPDATE stream_configs SET scte35_mode = CASE
    WHEN scte35_passthrough = 1 THEN 'passthrough'
    WHEN scte35_passthrough = 0 THEN 'disabled'
    ELSE '' -- -1 (inherit) stays inherit
END;

-- Durable record of every server-generated SCTE-35 event, so a stream
-- reconnect never replays a completed event and a cancelled/orphaned one's
-- event_id can still be legitimately reused (see internal/scte35ctrl).
-- No UNIQUE(app_name, stream_name, event_id) constraint: duplicate-vs-active
-- checking is done in application code (this codebase's existing
-- convention — see internal/db/applications.go's publish-key checks —
-- rather than relying on a DB constraint for business logic), which also
-- lets a cancelled attempt's event_id be reinserted as a fresh row.
CREATE TABLE IF NOT EXISTS scte35_events (
    id                    TEXT PRIMARY KEY,
    app_name              TEXT NOT NULL,
    stream_name           TEXT NOT NULL,
    event_id              INTEGER NOT NULL,
    command               TEXT NOT NULL,             -- splice_insert | time_signal
    segmentation_type_id  INTEGER NOT NULL DEFAULT 0,
    duration_ms           INTEGER NOT NULL DEFAULT 0,
    auto_return           INTEGER NOT NULL DEFAULT 0,
    trigger_type          TEXT NOT NULL,              -- immediate | next_keyframe | start_pts
    trigger_pts           INTEGER,
    status                TEXT NOT NULL DEFAULT 'pending', -- pending | active | completed | cancelled
    out_fired_at          TEXT,
    in_fired_at           TEXT,
    created_at            TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_scte35_events_stream
    ON scte35_events(app_name, stream_name, status);
`

// ─── Schema v9: SRT pull-source decryption ─────────────────────────────────
// Lets a pull_sources row of protocol=srt dial an upstream encrypted SRT
// source (Haivision/Nimble-style AES passphrase), mirroring the Passphrase/
// EnforcedEncryption support internal/srt.Server already has on the
// listen/inbound side. pbkeylen=0 means "use gosrt's default (16)".
const schemav9 = `
ALTER TABLE pull_sources ADD COLUMN passphrase TEXT NOT NULL DEFAULT '';
ALTER TABLE pull_sources ADD COLUMN pbkeylen INTEGER NOT NULL DEFAULT 0;
`

// ─── Schema v10: DVB-DASH profile + EBU-TT-D subtitles toggles ─────────────
// Per-app/stream control over internal/dash's DVB-DASH profile signaling and
// EBU-TT-D subtitle track (internal/captiondecode's real-time CEA-608/708
// decode — not AI-generated), mirroring out_dash's own app-default/
// stream-override shape. config.toml's [dash.dvb] flags remain the global
// master switches (see streamoutputs.apply) — these columns only narrow
// them further, same layering as every other out_* toggle.
const schemav10 = `
ALTER TABLE applications ADD COLUMN dvb_dash_enabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE applications ADD COLUMN dvb_subtitles_enabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE stream_configs ADD COLUMN dvb_dash_enabled INTEGER NOT NULL DEFAULT -1;
ALTER TABLE stream_configs ADD COLUMN dvb_subtitles_enabled INTEGER NOT NULL DEFAULT -1;
`

// ─── Schema v11: nDVR (fMP4 recording, rewind playback) ─────────────────────
// internal/dvr records each stream once as fMP4 and serves both HLS and DASH
// rewind from the same files (see internal/dvr's package doc). A recording
// session is one uninterrupted run of the recorder — a publisher reconnect
// or an output reload starts a new one — and owns the init segments its
// media segments decode against.
//
// dvr_segments gains the per-segment fMP4 layout: the file is the video
// moof+mdat (video_bytes long) followed by the audio moof+mdat, so DASH can
// serve each track as a byte range of the same file. The *_tfdt/*_dur
// columns are the exact track timeline (90 kHz) the MPD's SegmentTimeline
// is built from. msn and disc_seq are per-stream counters that stay stable
// while old segments are pruned, as HLS media/discontinuity sequence numbers
// must. Rows written by the old MPEG-TS recorder have session_ms = 0 and are
// ignored by playback until cleanup ages them out.
//
// applications.dvr_enabled/dvr_window_s are the per-app nDVR switch and
// rewind window (0 = config.toml's [dvr] window_seconds); stream_configs
// already had the per-stream override pair since v5.
const schemav11 = `
CREATE TABLE IF NOT EXISTS dvr_sessions (
    app_name      TEXT NOT NULL,
    stream_name   TEXT NOT NULL,
    session_ms    INTEGER NOT NULL,
    dir_path      TEXT NOT NULL,
    has_video     INTEGER NOT NULL DEFAULT 0,
    has_audio     INTEGER NOT NULL DEFAULT 0,
    video_codecs  TEXT NOT NULL DEFAULT '',
    audio_codecs  TEXT NOT NULL DEFAULT '',
    width         INTEGER NOT NULL DEFAULT 0,
    height        INTEGER NOT NULL DEFAULT 0,
    sample_rate   INTEGER NOT NULL DEFAULT 0,
    channels      INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (app_name, stream_name, session_ms)
);

ALTER TABLE dvr_segments ADD COLUMN session_ms  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE dvr_segments ADD COLUMN msn         INTEGER NOT NULL DEFAULT 0;
ALTER TABLE dvr_segments ADD COLUMN disc_seq    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE dvr_segments ADD COLUMN video_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE dvr_segments ADD COLUMN video_tfdt  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE dvr_segments ADD COLUMN video_dur   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE dvr_segments ADD COLUMN audio_tfdt  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE dvr_segments ADD COLUMN audio_dur   INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_dvr_msn
    ON dvr_segments(app_name, stream_name, msn);

ALTER TABLE applications ADD COLUMN dvr_enabled  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE applications ADD COLUMN dvr_window_s INTEGER NOT NULL DEFAULT 0;
`

// ─── Schema v12: ABR track options ───────────────────────────────────────────
// abr_variants.avg_bandwidth is the optional AVERAGE-BANDWIDTH and
// audio_only marks an audio rendition (no RESOLUTION, audio CODECS). As
// with bandwidth/width/height/codecs, 0/empty means auto-detect from the
// live stream (see api.streamInf).
const schemav12 = `
ALTER TABLE abr_variants ADD COLUMN avg_bandwidth INTEGER NOT NULL DEFAULT 0;
ALTER TABLE abr_variants ADD COLUMN audio_only    INTEGER NOT NULL DEFAULT 0;
`

// ─── Schema v13: RTMP push login ─────────────────────────────────────────────
// Per-application username/password for RTMP publishers (Adobe RTMP
// authentication, see internal/rtmp/adobeauth.go). An empty login means
// no login is required. The password is stored as given: the Adobe scheme
// has the server recompute md5(user+salt+password), so it can't be a
// one-way hash.
const schemav13 = `
ALTER TABLE applications ADD COLUMN push_login    TEXT NOT NULL DEFAULT '';
ALTER TABLE applications ADD COLUMN push_password TEXT NOT NULL DEFAULT '';
`

// ─── Schema v14: pull-source fallback URLs ───────────────────────────────────
// Backup URLs a pull source switches to when its primary fails, and how
// often it checks whether the primary is back (Nimble's "Fallback URLs" and
// "Primary check interval"). fallback_urls is newline-separated, in order.
// primary_check_s = 0 never switches back on its own.
// schemav15: server-wide settings changed from the panel, which override
// config.toml (e.g. "live_cache"). Values are JSON.
const schemav15 = `
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`

const schemav14 = `
ALTER TABLE pull_sources ADD COLUMN fallback_urls   TEXT NOT NULL DEFAULT '';
ALTER TABLE pull_sources ADD COLUMN primary_check_s INTEGER NOT NULL DEFAULT 30;
`
