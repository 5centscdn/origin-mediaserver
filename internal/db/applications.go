package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// AppConfig is the runtime view of an applications row.
type AppConfig struct {
	ID                  string
	Name                string
	PublishKey          string
	PushLogin           string // RTMP Adobe-auth login; "" = not required
	PushPassword        string
	Enabled             bool
	AllowAnyStream      bool
	OutHLS              bool
	OutDASH             bool
	OutMpegTS           bool
	OutLLHLS            bool
	DVBDashEnabled      bool
	DVBSubtitlesEnabled bool
	HLSSegDurationS     int
	HLSPlaylistWindow   int
	SCTE35Passthrough   bool   // superseded by SCTE35Mode — kept for read compatibility, no longer authoritative
	AdMarkerFormat      string // "none"|"cue"|"splicepoint"|"daterange"
	SCTE35Mode          string // "disabled"|"passthrough"|"insert"|"hybrid"
	DVREnabled          bool   // nDVR recording + rewind playback (internal/dvr)
	DVRWindowS          int    // rewind window; 0 = config.toml's [dvr] window_seconds
}

// StreamConfig is the runtime view of a stream_configs row.
// Protocol fields use -1 = inherit from app, 0 = disabled, 1 = enabled.
type StreamConfig struct {
	ID                  string
	AppID               string
	NamePattern         string
	IsWildcard          bool
	OutHLS              int
	OutDASH             int
	OutMpegTS           int
	OutLLHLS            int
	DVBDashEnabled      int
	DVBSubtitlesEnabled int
	DVREnabled          int
	DVRWindowS          int
	ABRGroupID          string
	Enabled             bool
	SCTE35Passthrough   int    // superseded by SCTE35Mode — kept for read compatibility, no longer authoritative
	AdMarkerFormat      string // "" = inherit from app; else none|cue|splicepoint|daterange
	SCTE35Mode          string // "" = inherit from app; else disabled|passthrough|insert|hybrid
}

// GetApplicationByName returns the app config for the given name, or nil if not found.
func (db *DB) GetApplicationByName(ctx context.Context, name string) (*AppConfig, error) {
	var a AppConfig
	var enabled, allowAny, outHLS, outDASH, outMpegTS, outLLHLS, dvbDash, dvbSubs, scte35Passthrough, dvrEnabled int
	err := db.QueryRowContext(ctx, `
		SELECT id, name, publish_key, enabled, allow_any_stream,
		       out_hls, out_dash, out_mpegts, out_ll_hls,
		       dvb_dash_enabled, dvb_subtitles_enabled,
		       hls_seg_duration_s, hls_playlist_window,
		       scte35_passthrough, ad_marker_format, scte35_mode,
		       dvr_enabled, dvr_window_s, push_login, push_password
		FROM applications WHERE name = ?`, name,
	).Scan(
		&a.ID, &a.Name, &a.PublishKey,
		&enabled, &allowAny,
		&outHLS, &outDASH, &outMpegTS, &outLLHLS,
		&dvbDash, &dvbSubs,
		&a.HLSSegDurationS, &a.HLSPlaylistWindow,
		&scte35Passthrough, &a.AdMarkerFormat, &a.SCTE35Mode,
		&dvrEnabled, &a.DVRWindowS, &a.PushLogin, &a.PushPassword,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.Enabled = enabled == 1
	a.AllowAnyStream = allowAny == 1
	a.OutHLS = outHLS == 1
	a.OutDASH = outDASH == 1
	a.OutMpegTS = outMpegTS == 1
	a.OutLLHLS = outLLHLS == 1
	a.DVBDashEnabled = dvbDash == 1
	a.DVBSubtitlesEnabled = dvbSubs == 1
	a.SCTE35Passthrough = scte35Passthrough == 1
	a.DVREnabled = dvrEnabled == 1
	return &a, nil
}

// GetStreamConfig finds the best matching stream_configs row for the given
// app and stream name.  Exact matches beat wildcard matches.
func (db *DB) GetStreamConfig(ctx context.Context, appID, streamName string) (*StreamConfig, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, app_id, name_pattern, is_wildcard,
		       out_hls, out_dash, out_mpegts, out_ll_hls,
		       dvb_dash_enabled, dvb_subtitles_enabled,
		       dvr_enabled, dvr_window_s, COALESCE(abr_group_id,''), enabled,
		       scte35_passthrough, ad_marker_format, scte35_mode
		FROM stream_configs
		WHERE app_id = ? AND enabled = 1
		ORDER BY is_wildcard ASC, created_at ASC`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var best *StreamConfig
	for rows.Next() {
		var sc StreamConfig
		var isWild, enabled int
		if err := rows.Scan(
			&sc.ID, &sc.AppID, &sc.NamePattern, &isWild,
			&sc.OutHLS, &sc.OutDASH, &sc.OutMpegTS, &sc.OutLLHLS,
			&sc.DVBDashEnabled, &sc.DVBSubtitlesEnabled,
			&sc.DVREnabled, &sc.DVRWindowS, &sc.ABRGroupID, &enabled,
			&sc.SCTE35Passthrough, &sc.AdMarkerFormat, &sc.SCTE35Mode,
		); err != nil {
			continue
		}
		sc.IsWildcard = isWild == 1
		sc.Enabled = enabled == 1

		if sc.NamePattern == streamName {
			return &sc, nil // exact match — highest priority
		}
		if best == nil && sc.IsWildcard && matchStreamPattern(sc.NamePattern, streamName) {
			best = &sc
		}
	}
	return best, nil
}

// GetABRPatternConfig returns the first enabled %i stream config in app that
// streamName matches, or nil. FMLE ABR grouping is decided by the %i pattern
// even when an exact config for the same name exists and wins in
// GetStreamConfig: a panel user may create test1..test5 as streams (to pick
// them in an ABR group before they go live) alongside "test%i", and that
// mustn't pull them out of the automatic "test" group.
func (db *DB) GetABRPatternConfig(ctx context.Context, appID, streamName string) (*StreamConfig, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name_pattern FROM stream_configs
		WHERE app_id = ? AND enabled = 1 AND is_wildcard = 1 AND name_pattern LIKE '%\%i%' ESCAPE '\'
		ORDER BY created_at ASC`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sc StreamConfig
		if rows.Scan(&sc.ID, &sc.NamePattern) != nil {
			continue
		}
		if matchStreamPattern(sc.NamePattern, streamName) {
			sc.AppID, sc.IsWildcard, sc.Enabled = appID, true, true
			return &sc, nil
		}
	}
	return nil, rows.Err()
}

// MatchStreamPattern reports whether name matches a stream config pattern
// ("*", "cam_*", "test%i", or an exact name). Exported for ABR pattern
// tracks (api.expandPatternTracks).
func MatchStreamPattern(pattern, name string) bool {
	if !IsStreamPattern(pattern) {
		return pattern == name
	}
	return matchStreamPattern(pattern, name)
}

// IsStreamPattern reports whether a stream name is a pattern rather than
// one exact name.
func IsStreamPattern(s string) bool {
	return strings.Contains(s, "%i") || strings.Contains(s, "*")
}

// matchStreamPattern returns true if streamName matches pattern.
// Supported wildcards:
//   - *            → any name
//   - prefix*      → prefix match
//   - prefix%i     → prefix + integer suffix (FMLE style)
//   - prefix%isuffix → prefix + integer + suffix
func matchStreamPattern(pattern, name string) bool {
	if pattern == "*" {
		return true
	}
	if idx := strings.Index(pattern, "%i"); idx >= 0 {
		prefix := pattern[:idx]
		suffix := pattern[idx+2:]
		if !strings.HasPrefix(name, prefix) {
			return false
		}
		rest := name[len(prefix):]
		if suffix != "" {
			if !strings.HasSuffix(rest, suffix) {
				return false
			}
			rest = rest[:len(rest)-len(suffix)]
		}
		if len(rest) == 0 {
			return false
		}
		for _, ch := range rest {
			if ch < '0' || ch > '9' {
				return false
			}
		}
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(name, pattern[:len(pattern)-1])
	}
	if strings.HasPrefix(pattern, "*") {
		return strings.HasSuffix(name, pattern[1:])
	}
	return false
}

// EffectiveOut resolves a stream-level protocol override (-1/0/1) against
// the app-level default.
func EffectiveOut(streamVal int, appDefault bool) bool {
	switch streamVal {
	case 1:
		return true
	case 0:
		return false
	default:
		return appDefault
	}
}

// EffectiveAdMarkerFormat resolves a stream-level ad_marker_format override
// ("" = inherit) against the app-level default.
func EffectiveAdMarkerFormat(streamVal, appDefault string) string {
	if streamVal == "" {
		return appDefault
	}
	return streamVal
}

// EffectiveSCTE35Mode resolves a stream-level scte35_mode override
// ("" = inherit) against the app-level default. Same inherit convention as
// EffectiveAdMarkerFormat.
func EffectiveSCTE35Mode(streamVal, appDefault string) string {
	if streamVal == "" {
		return appDefault
	}
	return streamVal
}
