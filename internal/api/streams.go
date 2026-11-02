package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/5centscdn/origin-mediaserver/internal/failover"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/streamoutputs"
)

type streamResponse struct {
	ID          string    `json:"id"`
	App         string    `json:"app"`
	Name        string    `json:"name"`
	SourceType  string    `json:"source_type"`
	SourceAddr  string    `json:"source_addr"`
	ABRGroup    string    `json:"abr_group,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	UptimeS     int64     `json:"uptime_s"`
	BytesIn     int64     `json:"bytes_in"`
	Subscribers int       `json:"subscribers"`
	Codec       string    `json:"codec,omitempty"`
}

func streamToResponse(s *media.Stream) streamResponse {
	return streamResponse{
		ID:          s.Key.String(),
		App:         s.Key.App,
		Name:        s.Key.Name,
		SourceType:  s.SourceType,
		SourceAddr:  s.SourceAddr,
		ABRGroup:    s.ABRGroup,
		StartedAt:   s.StartedAt,
		UptimeS:     int64(time.Since(s.StartedAt).Seconds()),
		BytesIn:     s.Bus.BytesIn(),
		// Subscribers reflects real concurrent HLS/DASH viewers (see
		// media.ViewerTracker), not Bus.SubscriberCount — that count also
		// includes internal per-stream packet consumers (HLS packager, DVR
		// recorder, DASH packager) that exist regardless of whether anyone
		// is actually watching, which made "viewers" wildly overcounted.
		Subscribers: s.Viewers.Count(),
		Codec:       codecSummary(s.Bus.CodecInfo()),
	}
}

// codecSummary renders a short human-readable "video/audio" codec label
// (e.g. "h264/aac") from a stream's live CodecInfo, or "" before any
// sequence header has been seen yet.
func codecSummary(ci *media.CodecInfo) string {
	if ci == nil {
		return ""
	}
	var parts []string
	if ci.VideoCodec != media.CodecUnknown {
		parts = append(parts, ci.VideoCodec.String())
	}
	if ci.AudioCodec != media.CodecUnknown {
		parts = append(parts, ci.AudioCodec.String())
	}
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += "/" + p
	}
	return out
}

func (s *Server) handleListStreams(w http.ResponseWriter, r *http.Request) {
	all := s.registry.All()
	out := make([]streamResponse, 0, len(all))
	for _, stream := range all {
		out = append(out, streamToResponse(stream))
	}
	data, _ := json.Marshal(out)
	jsonOK(w, data)
}

func (s *Server) handleGetStream(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	name := chi.URLParam(r, "name")
	stream := s.registry.Get(media.StreamKey{App: app, Name: name})
	if stream == nil {
		jsonError(w, http.StatusNotFound, "stream not found")
		return
	}
	data, _ := json.Marshal(streamToResponse(stream))
	jsonOK(w, data)
}

func (s *Server) handleKickStream(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	name := chi.URLParam(r, "name")
	stream := s.registry.Get(media.StreamKey{App: app, Name: name})
	if stream == nil {
		jsonError(w, http.StatusNotFound, "stream not found")
		return
	}
	// Unregister closes the bus, terminating all subscribers and signaling
	// the ingest handler to stop.
	s.registry.Unregister(stream)
	w.WriteHeader(http.StatusNoContent)
}

// handleReloadStream re-resolves this stream's effective application/
// stream-config from the DB (SCTE-35 mode, DASH toggle, HLS overrides, DVR)
// and restarts its output pipeline (HLS/DASH packagers, DVR recording)
// accordingly — for when an admin changed an application's settings while
// a stream using it was already live, since those settings are otherwise
// only read once, at stream registration. Unlike Kick, this never touches
// the stream's ingest connection (RTMP/SRT socket, pull-manager connection)
// or any other stream.
func (s *Server) handleReloadStream(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	name := chi.URLParam(r, "name")
	stream := s.registry.Get(media.StreamKey{App: app, Name: name})
	if stream == nil {
		jsonError(w, http.StatusNotFound, "stream not found")
		return
	}
	if err := streamoutputs.Reload(s.cfg, s.db, s.dvr, s.hlsPlaylists, stream, s.metrics); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTriggerFailover(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	name := chi.URLParam(r, "name")
	s.failover.TriggerManual(app, name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStreamDVRInfo(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	name := chi.URLParam(r, "name")
	info, err := s.dvr.StreamInfo(r.Context(), app, name)
	if err != nil {
		jsonError(w, http.StatusNotFound, "no DVR data")
		return
	}
	data, _ := json.Marshal(info)
	jsonOK(w, data)
}

func (s *Server) handleNodeInfo(w http.ResponseWriter, r *http.Request) {
	all := s.registry.All()
	resp := map[string]interface{}{
		"node_name":    s.cfg.Server.NodeName,
		"stream_count": len(all),
		"abr_groups":   s.registry.AllABRGroups(),
		"version":      "0.1.0",
		"data_dir":     s.cfg.Server.DataDir,
		"rtmp_listen":  s.cfg.RTMP.Listen,
		"http_listen":  s.cfg.HTTP.Listen,
		// srt is included even when disabled so the panel can show the
		// listen address as a preview of what enabling it would use.
		"srt": map[string]interface{}{
			"enabled":    s.cfg.SRT.Enabled,
			"listen":     s.cfg.SRT.Listen,
			"latency_ms": s.cfg.SRT.LatencyMs,
			// passphrase deliberately omitted — never sent to the panel.
		},
		"hls": map[string]interface{}{
			"segment_duration": s.cfg.HLS.SegmentDurationS,
			"playlist_size":    s.cfg.HLS.PlaylistWindow,
		},
		"dvr": map[string]interface{}{
			"enabled":        s.cfg.DVR.Enabled,
			"storage_path":   s.cfg.DVR.StoragePath,
			"window_seconds": s.cfg.DVR.WindowSeconds,
		},
	}
	data, _ := json.Marshal(resp)
	jsonOK(w, data)
}

// handleListNodes returns every peer currently visible via multi-node
// heartbeat discovery (see internal/node), plus this node's own identity —
// distinct from handleListServers' manually-curated bookmark list.
// Returns just this node (not an error) when multinode is disabled.
func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	localID := "local"
	if s.nodeMgr != nil {
		localID = s.nodeMgr.NodeID()
	}

	nodes := []map[string]interface{}{
		{
			"node_id":   localID,
			"node_name": s.cfg.Server.NodeName,
			"url":       s.cfg.Multinode.AdvertiseURL,
			"streams":   streamKeys(s.registry.All()),
			"self":      true,
		},
	}
	if s.nodeMgr != nil {
		for _, p := range s.nodeMgr.Peers() {
			nodes = append(nodes, map[string]interface{}{
				"node_id":   p.NodeID,
				"node_name": p.NodeName,
				"url":       p.URL,
				"streams":   p.Streams,
				"self":      false,
				"last_seen": p.LastSeen,
			})
		}
	}
	data, _ := json.Marshal(nodes)
	jsonOK(w, data)
}

func streamKeys(streams []*media.Stream) []string {
	out := make([]string, 0, len(streams))
	for _, st := range streams {
		out = append(out, st.Key.App+"/"+st.Key.Name)
	}
	return out
}

// ─── ABR group handlers ───────────────────────────────────────────────────────

func (s *Server) handleListABRGroups(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, name, app_name, stream_name, auto_detect, created_at FROM abr_groups ORDER BY name`)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var groups []map[string]interface{}
	for rows.Next() {
		var id, name, appName, streamName, createdAt string
		var autoDetect bool
		if err := rows.Scan(&id, &name, &appName, &streamName, &autoDetect, &createdAt); err != nil {
			continue
		}
		groups = append(groups, map[string]interface{}{
			"id": id, "name": name, "app_name": appName,
			"stream_name": streamName, "auto_detect": autoDetect, "created_at": createdAt,
		})
	}

	if groups == nil {
		groups = []map[string]interface{}{}
	}
	data, _ := json.Marshal(groups)
	jsonOK(w, data)
}

func (s *Server) handleCreateABRGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		AppName    string `json:"app_name"`
		StreamName string `json:"stream_name"`
		AutoDetect bool   `json:"auto_detect"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Name == "" || req.AppName == "" {
		jsonError(w, http.StatusBadRequest, "name and app_name are required")
		return
	}

	id := uuid.New().String()
	autoDetectInt := 0
	if req.AutoDetect {
		autoDetectInt = 1
	}
	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO abr_groups (id, name, app_name, stream_name, auto_detect) VALUES (?, ?, ?, ?, ?)`,
		id, req.Name, req.AppName, req.StreamName, autoDetectInt,
	)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var createdAt string
	s.db.QueryRowContext(r.Context(),
		`SELECT created_at FROM abr_groups WHERE id = ?`, id,
	).Scan(&createdAt)

	data, _ := json.Marshal(map[string]interface{}{
		"id": id, "name": req.Name, "app_name": req.AppName,
		"stream_name": req.StreamName, "auto_detect": req.AutoDetect, "created_at": createdAt,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(data)
}

func (s *Server) handleGetABRGroup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var name, appName, streamName, createdAt string
	var autoDetect bool
	err := s.db.QueryRowContext(r.Context(),
		`SELECT id, name, app_name, stream_name, auto_detect, created_at FROM abr_groups WHERE id = ?`, id,
	).Scan(&id, &name, &appName, &streamName, &autoDetect, &createdAt)
	if err != nil {
		jsonError(w, http.StatusNotFound, "abr group not found")
		return
	}
	data, _ := json.Marshal(map[string]interface{}{
		"id": id, "name": name, "app_name": appName,
		"stream_name": streamName, "auto_detect": autoDetect, "created_at": createdAt,
	})
	jsonOK(w, data)
}

func (s *Server) handleUpdateABRGroup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Name       string `json:"name"`
		AppName    string `json:"app_name"`
		StreamName string `json:"stream_name"`
		AutoDetect bool   `json:"auto_detect"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	autoDetectInt := 0
	if req.AutoDetect {
		autoDetectInt = 1
	}
	res, err := s.db.ExecContext(r.Context(),
		`UPDATE abr_groups SET name=?, app_name=?, stream_name=?, auto_detect=?, updated_at=datetime('now') WHERE id=?`,
		req.Name, req.AppName, req.StreamName, autoDetectInt, id,
	)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		jsonError(w, http.StatusNotFound, "abr group not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteABRGroup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	_, err := s.db.ExecContext(r.Context(), `DELETE FROM abr_groups WHERE id=?`, id)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListABRVariants(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, group_id, stream_name, src_app_name, label, bandwidth, avg_bandwidth, width, height, frame_rate, codecs, audio_only, sort_order, created_at
		 FROM abr_variants WHERE group_id = ? ORDER BY sort_order`, id)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var variants []map[string]interface{}
	for rows.Next() {
		var vid, groupID, streamName, srcAppName, label, codecs, createdAt string
		var bandwidth, avgBandwidth, width, height, sortOrder int
		var audioOnly bool
		var frameRate float64
		if err := rows.Scan(&vid, &groupID, &streamName, &srcAppName, &label,
			&bandwidth, &avgBandwidth, &width, &height, &frameRate, &codecs, &audioOnly, &sortOrder, &createdAt); err != nil {
			continue
		}
		variants = append(variants, map[string]interface{}{
			"id": vid, "group_id": groupID, "stream_name": streamName,
			"src_app_name": srcAppName, "label": label, "bandwidth": bandwidth,
			"avg_bandwidth": avgBandwidth, "width": width, "height": height,
			"frame_rate": frameRate, "codecs": codecs, "audio_only": audioOnly,
			"sort_order": sortOrder, "created_at": createdAt,
		})
	}

	if variants == nil {
		variants = []map[string]interface{}{}
	}
	data, _ := json.Marshal(variants)
	jsonOK(w, data)
}

func (s *Server) handleAddABRVariant(w http.ResponseWriter, r *http.Request) {
	groupID := chi.URLParam(r, "id")
	var req struct {
		StreamName string  `json:"stream_name"`
		SrcAppName string  `json:"src_app_name"`
		Label      string  `json:"label"`
		Bandwidth  int     `json:"bandwidth"`
		Width      int     `json:"width"`
		Height     int     `json:"height"`
		FrameRate  float64 `json:"frame_rate"`
		Codecs     string  `json:"codecs"`
		SortOrder  int     `json:"sort_order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	vid := uuid.New().String()
	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO abr_variants (id, group_id, stream_name, src_app_name, label, bandwidth, width, height, frame_rate, codecs, sort_order)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		vid, groupID, req.StreamName, req.SrcAppName, req.Label,
		req.Bandwidth, req.Width, req.Height, req.FrameRate, req.Codecs, req.SortOrder,
	)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var createdAt string
	s.db.QueryRowContext(r.Context(),
		`SELECT created_at FROM abr_variants WHERE id = ?`, vid,
	).Scan(&createdAt)

	data, _ := json.Marshal(map[string]interface{}{
		"id": vid, "group_id": groupID, "stream_name": req.StreamName,
		"src_app_name": req.SrcAppName, "label": req.Label, "bandwidth": req.Bandwidth,
		"width": req.Width, "height": req.Height, "frame_rate": req.FrameRate,
		"codecs": req.Codecs, "sort_order": req.SortOrder, "created_at": createdAt,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(data)
}

func (s *Server) handleUpdateABRVariant(w http.ResponseWriter, r *http.Request) {
	vid := chi.URLParam(r, "vid")
	var req struct {
		StreamName string  `json:"stream_name"`
		SrcAppName string  `json:"src_app_name"`
		Label      string  `json:"label"`
		Bandwidth  int     `json:"bandwidth"`
		Width      int     `json:"width"`
		Height     int     `json:"height"`
		FrameRate  float64 `json:"frame_rate"`
		Codecs     string  `json:"codecs"`
		SortOrder  int     `json:"sort_order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	res, err := s.db.ExecContext(r.Context(),
		`UPDATE abr_variants SET stream_name=?, src_app_name=?, label=?, bandwidth=?, width=?, height=?, frame_rate=?, codecs=?, sort_order=?
		 WHERE id=?`,
		req.StreamName, req.SrcAppName, req.Label, req.Bandwidth,
		req.Width, req.Height, req.FrameRate, req.Codecs, req.SortOrder, vid,
	)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		jsonError(w, http.StatusNotFound, "variant not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteABRVariant(w http.ResponseWriter, r *http.Request) {
	vid := chi.URLParam(r, "vid")
	_, err := s.db.ExecContext(r.Context(), `DELETE FROM abr_variants WHERE id=?`, vid)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// abrTrackRequest is one row of the panel's ABR group editor. Zero/empty
// bandwidth, resolution and codecs mean auto-detect (see streamInf).
type abrTrackRequest struct {
	SrcAppName   string `json:"src_app_name"`
	StreamName   string `json:"stream_name"`
	Bandwidth    int    `json:"bandwidth"`
	AvgBandwidth int    `json:"avg_bandwidth"`
	Resolution   string `json:"resolution"` // "WIDTHxHEIGHT" or ""
	Codecs       string `json:"codecs"`
	AudioOnly    bool   `json:"audio_only"`
}

// handleReplaceABRTracks handles PUT /abr-groups/{id}/tracks: it replaces a
// group's whole track list in one transaction, in the order given, so the
// editor never leaves a half-saved group behind.
//
// Each track's stream must be one its app would accept a publish for (a
// matching stream config, or allow_any_stream). Otherwise a typo saves fine
// and the track silently never appears in the master (the DVR server's
// 2026-09-29 incident). It need not be live yet.
func (s *Server) handleReplaceABRTracks(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Tracks []abrTrackRequest `json:"tracks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	var groupApp string
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT app_name FROM abr_groups WHERE id = ?`, id).Scan(&groupApp); err != nil {
		jsonError(w, http.StatusNotFound, "abr group not found")
		return
	}
	if len(req.Tracks) == 0 {
		jsonError(w, http.StatusBadRequest, "at least one track is required")
		return
	}

	type row struct {
		abrTrackRequest
		width, height int
	}
	rows := make([]row, 0, len(req.Tracks))
	for _, t := range req.Tracks {
		if t.SrcAppName == "" {
			t.SrcAppName = groupApp
		}
		t.Resolution = strings.TrimSpace(t.Resolution)
		t.Codecs = strings.TrimSpace(t.Codecs)
		if t.StreamName == "" || strings.Contains(t.StreamName, "/") {
			jsonError(w, http.StatusBadRequest, fmt.Sprintf("track stream %q must be a bare stream name", t.StreamName))
			return
		}
		if t.Bandwidth < 0 || t.AvgBandwidth < 0 {
			jsonError(w, http.StatusBadRequest, "bandwidth must not be negative")
			return
		}
		rw := row{abrTrackRequest: t}
		if t.Resolution != "" {
			if n, _ := fmt.Sscanf(strings.ToLower(t.Resolution), "%dx%d", &rw.width, &rw.height); n != 2 || rw.width <= 0 || rw.height <= 0 {
				jsonError(w, http.StatusBadRequest, fmt.Sprintf("resolution %q must be WIDTHxHEIGHT, e.g. 1280x720, or empty for auto", t.Resolution))
				return
			}
		}
		if err := s.checkPublishable(r.Context(), t.SrcAppName, t.StreamName); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		rows = append(rows, rw)
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM abr_variants WHERE group_id = ?`, id); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for i, t := range rows {
		audioOnly := 0
		if t.AudioOnly {
			audioOnly = 1
		}
		if _, err := tx.ExecContext(r.Context(),
			`INSERT INTO abr_variants (id, group_id, stream_name, src_app_name, label, bandwidth, avg_bandwidth,
			                           width, height, codecs, audio_only, sort_order)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			uuid.New().String(), id, t.StreamName, t.SrcAppName, t.StreamName, t.Bandwidth, t.AvgBandwidth,
			t.width, t.height, t.Codecs, audioOnly, i,
		); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE abr_groups SET updated_at = datetime('now') WHERE id = ?`, id); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// checkPublishable reports whether app would accept a publish of stream, by
// the same rules RTMP/SRT/WHIP ingest apply: a panel app needs a matching
// stream config unless allow_any_stream is on; a config.toml app accepts
// any name.
func (s *Server) checkPublishable(ctx context.Context, app, stream string) error {
	dbApp, err := s.db.GetApplicationByName(ctx, app)
	if err != nil {
		return err
	}
	if dbApp == nil {
		if s.cfg.RTMP.AppByName(app) != nil {
			return nil
		}
		return fmt.Errorf("application %q does not exist", app)
	}
	if dbApp.AllowAnyStream {
		return nil
	}
	sc, err := s.db.GetStreamConfig(ctx, dbApp.ID, stream)
	if err != nil {
		return err
	}
	if sc == nil {
		return fmt.Errorf("stream %q is not configured in application %q", stream, app)
	}
	return nil
}

// ─── Pull source handlers ─────────────────────────────────────────────────────

func (s *Server) handleListPullSources(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, app_name, stream_name, url, protocol, on_demand, reconnect, reconnect_delay_s, status, created_at, passphrase, pbkeylen
		 FROM pull_sources ORDER BY created_at`)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var sources []map[string]interface{}
	for rows.Next() {
		var id, appName, streamName, url, protocol, status, createdAt, passphrase string
		var onDemand, reconnect bool
		var reconnectDelayS, pbkeylen int
		if err := rows.Scan(&id, &appName, &streamName, &url, &protocol,
			&onDemand, &reconnect, &reconnectDelayS, &status, &createdAt, &passphrase, &pbkeylen); err != nil {
			continue
		}
		sources = append(sources, map[string]interface{}{
			"id": id, "app_name": appName, "stream_name": streamName,
			"url": url, "protocol": protocol, "on_demand": onDemand,
			"reconnect": reconnect, "reconnect_delay_s": reconnectDelayS,
			"status": status, "created_at": createdAt,
			// The passphrase itself is never echoed back — only whether one
			// is set, so the panel can show "configured" without exposing
			// the secret to anyone who can read the list.
			"has_passphrase": passphrase != "",
			"pbkeylen":       pbkeylen,
		})
	}

	if sources == nil {
		sources = []map[string]interface{}{}
	}
	data, _ := json.Marshal(sources)
	jsonOK(w, data)
}

func (s *Server) handleCreatePullSource(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AppName        string `json:"app_name"`
		StreamName     string `json:"stream_name"`
		URL            string `json:"url"`
		Protocol       string `json:"protocol"`
		OnDemand       bool   `json:"on_demand"`
		Reconnect      bool   `json:"reconnect"`
		ReconnectDelay int    `json:"reconnect_delay_s"`
		Passphrase     string `json:"passphrase"`
		PBKeylen       int    `json:"pbkeylen"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.AppName == "" || req.StreamName == "" || req.URL == "" {
		jsonError(w, http.StatusBadRequest, "app_name, stream_name, and url are required")
		return
	}
	if req.Passphrase != "" && (len(req.Passphrase) < 10 || len(req.Passphrase) > 79) {
		jsonError(w, http.StatusBadRequest, "passphrase must be 10-79 characters")
		return
	}

	id := uuid.New().String()
	onDemandInt := 0
	if req.OnDemand {
		onDemandInt = 1
	}
	reconnectInt := 0
	if req.Reconnect {
		reconnectInt = 1
	}
	if req.ReconnectDelay == 0 {
		req.ReconnectDelay = 5
	}
	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO pull_sources (id, app_name, stream_name, url, protocol, on_demand, reconnect, reconnect_delay_s, passphrase, pbkeylen)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, req.AppName, req.StreamName, req.URL, req.Protocol,
		onDemandInt, reconnectInt, req.ReconnectDelay, req.Passphrase, req.PBKeylen,
	)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var createdAt string
	s.db.QueryRowContext(r.Context(),
		`SELECT created_at FROM pull_sources WHERE id = ?`, id,
	).Scan(&createdAt)

	data, _ := json.Marshal(map[string]interface{}{
		"id": id, "app_name": req.AppName, "stream_name": req.StreamName,
		"url": req.URL, "protocol": req.Protocol, "on_demand": req.OnDemand,
		"reconnect": req.Reconnect, "reconnect_delay_s": req.ReconnectDelay,
		"status": "inactive", "created_at": createdAt,
		"has_passphrase": req.Passphrase != "", "pbkeylen": req.PBKeylen,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(data)
}

func (s *Server) handleUpdatePullSource(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		AppName        string `json:"app_name"`
		StreamName     string `json:"stream_name"`
		URL            string `json:"url"`
		Protocol       string `json:"protocol"`
		OnDemand       bool   `json:"on_demand"`
		Reconnect      bool   `json:"reconnect"`
		ReconnectDelay int    `json:"reconnect_delay_s"`
		// Passphrase is only written if non-empty, or ClearPassphrase is set —
		// the panel never has the real secret to redisplay/resend (see
		// handleListPullSources's has_passphrase redaction), so "field left
		// blank" must mean "leave whatever's already stored" rather than
		// silently wiping it on every unrelated edit.
		Passphrase      string `json:"passphrase"`
		ClearPassphrase bool   `json:"clear_passphrase"`
		PBKeylen        int    `json:"pbkeylen"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Passphrase != "" && (len(req.Passphrase) < 10 || len(req.Passphrase) > 79) {
		jsonError(w, http.StatusBadRequest, "passphrase must be 10-79 characters")
		return
	}
	onDemandInt := 0
	if req.OnDemand {
		onDemandInt = 1
	}
	reconnectInt := 0
	if req.Reconnect {
		reconnectInt = 1
	}

	var res sql.Result
	var err error
	if req.Passphrase != "" || req.ClearPassphrase {
		res, err = s.db.ExecContext(r.Context(),
			`UPDATE pull_sources SET app_name=?, stream_name=?, url=?, protocol=?, on_demand=?, reconnect=?, reconnect_delay_s=?, passphrase=?, pbkeylen=?, updated_at=datetime('now')
			 WHERE id=?`,
			req.AppName, req.StreamName, req.URL, req.Protocol,
			onDemandInt, reconnectInt, req.ReconnectDelay, req.Passphrase, req.PBKeylen, id,
		)
	} else {
		res, err = s.db.ExecContext(r.Context(),
			`UPDATE pull_sources SET app_name=?, stream_name=?, url=?, protocol=?, on_demand=?, reconnect=?, reconnect_delay_s=?, updated_at=datetime('now')
			 WHERE id=?`,
			req.AppName, req.StreamName, req.URL, req.Protocol,
			onDemandInt, reconnectInt, req.ReconnectDelay, id,
		)
	}
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		jsonError(w, http.StatusNotFound, "pull source not found")
		return
	}

	// If this source is currently running, restart it so the edit (new URL,
	// app/stream name, protocol, etc.) takes effect immediately.
	if s.pull.IsRunning(id) {
		s.pull.Stop(id)
		if updated, err := s.db.GetPullSourceByID(r.Context(), id); err == nil && updated != nil {
			if err := s.pull.Start(s.baseCtx, *updated); err != nil {
				slog.Warn("restart pull source after update", "id", id, "err", err)
			}
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeletePullSource(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	s.pull.Stop(id) // no-op if not running; avoids an orphaned goroutine
	_, err := s.db.ExecContext(r.Context(), `DELETE FROM pull_sources WHERE id=?`, id)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStartPull(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	source, err := s.db.GetPullSourceByID(r.Context(), id)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if source == nil {
		jsonError(w, http.StatusNotFound, "pull source not found")
		return
	}
	if err := s.pull.Start(s.baseCtx, *source); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStopPull(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	source, err := s.db.GetPullSourceByID(r.Context(), id)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if source == nil {
		jsonError(w, http.StatusNotFound, "pull source not found")
		return
	}
	s.pull.Stop(id)
	w.WriteHeader(http.StatusNoContent)
}

// ─── Failover rule handlers ───────────────────────────────────────────────────

func (s *Server) handleListFailoverRules(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, app_name, primary_stream, backup_url, backup_protocol, trigger_type,
		        stall_timeout_ms, warmup_ms, enabled, auto_restore, state, created_at
		 FROM failover_rules ORDER BY created_at`)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var rules []map[string]interface{}
	for rows.Next() {
		var id, appName, primaryStream, backupURL, backupProtocol, triggerType, state, createdAt string
		var stallTimeoutMS, warmupMS int
		var enabled, autoRestore bool
		if err := rows.Scan(&id, &appName, &primaryStream, &backupURL, &backupProtocol,
			&triggerType, &stallTimeoutMS, &warmupMS, &enabled, &autoRestore, &state, &createdAt); err != nil {
			continue
		}
		rules = append(rules, map[string]interface{}{
			"id": id, "app_name": appName, "primary_stream": primaryStream,
			"backup_url": backupURL, "backup_protocol": backupProtocol,
			"trigger_type": triggerType, "stall_timeout_ms": stallTimeoutMS,
			"warmup_ms": warmupMS, "enabled": enabled, "auto_restore": autoRestore, "state": state,
			"created_at": createdAt,
		})
	}

	if rules == nil {
		rules = []map[string]interface{}{}
	}
	data, _ := json.Marshal(rules)
	jsonOK(w, data)
}

func (s *Server) handleCreateFailoverRule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AppName        string `json:"app_name"`
		PrimaryStream  string `json:"primary_stream"`
		BackupURL      string `json:"backup_url"`
		BackupProtocol string `json:"backup_protocol"`
		TriggerType    string `json:"trigger_type"`
		StallTimeoutMS int    `json:"stall_timeout_ms"`
		WarmupMS       int    `json:"warmup_ms"`
		Enabled        bool   `json:"enabled"`
		AutoRestore    bool   `json:"auto_restore"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.AppName == "" || req.PrimaryStream == "" {
		jsonError(w, http.StatusBadRequest, "app_name and primary_stream are required")
		return
	}
	if req.TriggerType == "" {
		req.TriggerType = "automatic"
	}
	if req.StallTimeoutMS == 0 {
		req.StallTimeoutMS = 5000
	}
	if req.WarmupMS == 0 {
		req.WarmupMS = 2000
	}

	id := uuid.New().String()
	enabledInt := 0
	if req.Enabled {
		enabledInt = 1
	}
	autoRestoreInt := 0
	if req.AutoRestore {
		autoRestoreInt = 1
	}
	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO failover_rules (id, app_name, primary_stream, backup_url, backup_protocol, trigger_type, stall_timeout_ms, warmup_ms, enabled, auto_restore)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, req.AppName, req.PrimaryStream, req.BackupURL, req.BackupProtocol,
		req.TriggerType, req.StallTimeoutMS, req.WarmupMS, enabledInt, autoRestoreInt,
	)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if req.Enabled {
		s.failover.Watch(s.baseCtx, failover.Rule{
			AppName:        req.AppName,
			PrimaryStream:  req.PrimaryStream,
			BackupURL:      req.BackupURL,
			BackupProtocol: req.BackupProtocol,
			StallTimeoutMs: req.StallTimeoutMS,
			WarmupMs:       req.WarmupMS,
			AutoRestore:    req.AutoRestore,
		})
	}

	var createdAt string
	s.db.QueryRowContext(r.Context(),
		`SELECT created_at FROM failover_rules WHERE id = ?`, id,
	).Scan(&createdAt)

	data, _ := json.Marshal(map[string]interface{}{
		"id": id, "app_name": req.AppName, "primary_stream": req.PrimaryStream,
		"backup_url": req.BackupURL, "backup_protocol": req.BackupProtocol,
		"trigger_type": req.TriggerType, "stall_timeout_ms": req.StallTimeoutMS,
		"warmup_ms": req.WarmupMS, "enabled": req.Enabled, "auto_restore": req.AutoRestore, "state": "idle",
		"created_at": createdAt,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(data)
}

func (s *Server) handleUpdateFailoverRule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var oldAppName, oldPrimaryStream string
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT app_name, primary_stream FROM failover_rules WHERE id=?`, id,
	).Scan(&oldAppName, &oldPrimaryStream); err != nil {
		jsonError(w, http.StatusNotFound, "failover rule not found")
		return
	}

	var req struct {
		AppName        string `json:"app_name"`
		PrimaryStream  string `json:"primary_stream"`
		BackupURL      string `json:"backup_url"`
		BackupProtocol string `json:"backup_protocol"`
		TriggerType    string `json:"trigger_type"`
		StallTimeoutMS int    `json:"stall_timeout_ms"`
		WarmupMS       int    `json:"warmup_ms"`
		Enabled        bool   `json:"enabled"`
		AutoRestore    bool   `json:"auto_restore"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	enabledInt := 0
	if req.Enabled {
		enabledInt = 1
	}
	autoRestoreInt := 0
	if req.AutoRestore {
		autoRestoreInt = 1
	}
	res, err := s.db.ExecContext(r.Context(),
		`UPDATE failover_rules SET app_name=?, primary_stream=?, backup_url=?, backup_protocol=?,
		        trigger_type=?, stall_timeout_ms=?, warmup_ms=?, enabled=?, auto_restore=?, updated_at=datetime('now')
		 WHERE id=?`,
		req.AppName, req.PrimaryStream, req.BackupURL, req.BackupProtocol,
		req.TriggerType, req.StallTimeoutMS, req.WarmupMS, enabledInt, autoRestoreInt, id,
	)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		jsonError(w, http.StatusNotFound, "failover rule not found")
		return
	}

	// Stop the old monitor (if any) and start fresh with the updated rule,
	// so edits take effect immediately instead of only on next restart.
	s.failover.Unwatch(oldAppName, oldPrimaryStream)
	if req.Enabled {
		s.failover.Watch(s.baseCtx, failover.Rule{
			AppName:        req.AppName,
			PrimaryStream:  req.PrimaryStream,
			BackupURL:      req.BackupURL,
			BackupProtocol: req.BackupProtocol,
			StallTimeoutMs: req.StallTimeoutMS,
			WarmupMs:       req.WarmupMS,
			AutoRestore:    req.AutoRestore,
		})
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteFailoverRule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var appName, primaryStream string
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT app_name, primary_stream FROM failover_rules WHERE id=?`, id,
	).Scan(&appName, &primaryStream); err == nil {
		s.failover.Unwatch(appName, primaryStream)
	}

	_, err := s.db.ExecContext(r.Context(), `DELETE FROM failover_rules WHERE id=?`, id)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTriggerFailoverRule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var rule struct{ AppName, PrimaryStream string }
	err := s.db.QueryRowContext(r.Context(),
		`SELECT app_name, primary_stream FROM failover_rules WHERE id = ?`, id,
	).Scan(&rule.AppName, &rule.PrimaryStream)
	if err != nil {
		jsonError(w, http.StatusNotFound, "rule not found")
		return
	}
	s.failover.TriggerManual(rule.AppName, rule.PrimaryStream)
	w.WriteHeader(http.StatusNoContent)
}

// ─── DVR config ───────────────────────────────────────────────────────────────

func (s *Server) handleGetDVRConfig(w http.ResponseWriter, r *http.Request) {
	data, _ := json.Marshal(struct {
		Enabled       bool   `json:"enabled"`
		StoragePath   string `json:"storage_path"`
		WindowSeconds int    `json:"window_seconds"`
	}{
		Enabled:       s.cfg.DVREnabled(),
		StoragePath:   s.cfg.DVR.StoragePath,
		WindowSeconds: s.cfg.DVRWindowSeconds(),
	})
	jsonOK(w, data)
}

func (s *Server) handleUpdateDVRConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled       *bool  `json:"enabled"`
		StoragePath   string `json:"storage_path"`
		WindowSeconds *int   `json:"window_seconds"` // nil = unchanged
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.StoragePath != "" && req.StoragePath != s.cfg.DVR.StoragePath {
		jsonError(w, http.StatusBadRequest, "storage_path cannot be changed at runtime; edit config.toml and restart")
		return
	}
	window := 0 // UpdateDVRRuntime: <= 0 leaves the window unchanged
	if req.WindowSeconds != nil {
		if *req.WindowSeconds <= 0 {
			jsonError(w, http.StatusBadRequest, "window_seconds must be positive")
			return
		}
		window = *req.WindowSeconds
	}
	s.cfg.UpdateDVRRuntime(window, req.Enabled)
	w.WriteHeader(http.StatusNoContent)
}

// ─── Server management (multi-server) ────────────────────────────────────────

func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, name, url, note, created_at FROM servers ORDER BY created_at`)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	// Prepend synthetic "this server" entry.
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	localURL := scheme + "://" + r.Host

	servers := []map[string]interface{}{
		{
			"id":         "local",
			"name":       s.cfg.Server.NodeName,
			"url":        localURL,
			"note":       "this server",
			"created_at": "",
		},
	}

	for rows.Next() {
		var id, name, url, note, createdAt string
		if err := rows.Scan(&id, &name, &url, &note, &createdAt); err != nil {
			continue
		}
		servers = append(servers, map[string]interface{}{
			"id": id, "name": name, "url": url, "note": note, "created_at": createdAt,
		})
	}

	data, _ := json.Marshal(servers)
	jsonOK(w, data)
}

func (s *Server) handleAddServer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		URL  string `json:"url"`
		Note string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Name == "" || req.URL == "" {
		jsonError(w, http.StatusBadRequest, "name and url are required")
		return
	}

	id := uuid.New().String()
	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO servers (id, name, url, note) VALUES (?, ?, ?, ?)`,
		id, req.Name, req.URL, req.Note,
	)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var createdAt string
	s.db.QueryRowContext(r.Context(),
		`SELECT created_at FROM servers WHERE id = ?`, id,
	).Scan(&createdAt)

	data, _ := json.Marshal(map[string]interface{}{
		"id": id, "name": req.Name, "url": req.URL,
		"note": req.Note, "created_at": createdAt,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(data)
}

func (s *Server) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "local" {
		jsonError(w, http.StatusBadRequest, "cannot delete local server")
		return
	}
	_, err := s.db.ExecContext(r.Context(), `DELETE FROM servers WHERE id=?`, id)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

