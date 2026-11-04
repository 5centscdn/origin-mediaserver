package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// ─── Applications ─────────────────────────────────────────────────────────────

func (s *Server) handleListApplications(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, name, publish_key, enabled, allow_any_stream,
		       out_hls, out_dash, out_mpegts, out_ll_hls,
		       dvb_dash_enabled, dvb_subtitles_enabled,
		       hls_seg_duration_s, hls_playlist_window,
		       scte35_passthrough, ad_marker_format, scte35_mode, dvr_enabled, dvr_window_s,
		       push_login, push_password != '', created_at, updated_at
		FROM applications ORDER BY name`)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var list []map[string]interface{}
	for rows.Next() {
		var id, name, publishKey, adMarkerFormat, scte35Mode, pushLogin, createdAt, updatedAt string
		var enabled, allowAny, outHLS, outDASH, outMpegTS, outLLHLS, dvbDash, dvbSubs, scte35Passthrough int
		var hlsSeg, hlsWin, dvrEnabled, dvrWin int
		var hasPushPassword bool
		if err := rows.Scan(&id, &name, &publishKey, &enabled, &allowAny,
			&outHLS, &outDASH, &outMpegTS, &outLLHLS,
			&dvbDash, &dvbSubs,
			&hlsSeg, &hlsWin, &scte35Passthrough, &adMarkerFormat, &scte35Mode,
			&dvrEnabled, &dvrWin, &pushLogin, &hasPushPassword, &createdAt, &updatedAt); err != nil {
			continue
		}
		list = append(list, map[string]interface{}{
			"id": id, "name": name, "publish_key": publishKey,
			"enabled": enabled == 1, "allow_any_stream": allowAny == 1,
			"out_hls": outHLS == 1, "out_dash": outDASH == 1,
			"out_mpegts": outMpegTS == 1, "out_ll_hls": outLLHLS == 1,
			"dvb_dash_enabled": dvbDash == 1, "dvb_subtitles_enabled": dvbSubs == 1,
			"hls_seg_duration_s": hlsSeg, "hls_playlist_window": hlsWin,
			"scte35_passthrough": scte35Passthrough == 1, "ad_marker_format": adMarkerFormat,
			"scte35_mode": scte35Mode,
			"dvr_enabled": dvrEnabled == 1, "dvr_window_s": dvrWin,
			"push_login": pushLogin, "has_push_password": hasPushPassword,
			"created_at": createdAt, "updated_at": updatedAt,
		})
	}
	if list == nil {
		list = []map[string]interface{}{}
	}
	data, _ := json.Marshal(list)
	jsonOK(w, data)
}

func (s *Server) handleCreateApplication(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name                string `json:"name"`
		PublishKey          string `json:"publish_key"`
		Enabled             *bool  `json:"enabled"`
		AllowAnyStream      *bool  `json:"allow_any_stream"`
		OutHLS              *bool  `json:"out_hls"`
		OutDASH             *bool  `json:"out_dash"`
		OutMpegTS           *bool  `json:"out_mpegts"`
		OutLLHLS            *bool  `json:"out_ll_hls"`
		DVBDashEnabled      *bool  `json:"dvb_dash_enabled"`
		DVBSubtitlesEnabled *bool  `json:"dvb_subtitles_enabled"`
		HLSSegDurationS     int    `json:"hls_seg_duration_s"`
		HLSPlaylistWindow   int    `json:"hls_playlist_window"`
		SCTE35Passthrough   *bool  `json:"scte35_passthrough"`
		AdMarkerFormat      string `json:"ad_marker_format"`
		SCTE35Mode          string `json:"scte35_mode"`
		DVREnabled          *bool  `json:"dvr_enabled"`
		DVRWindowS          int    `json:"dvr_window_s"`
		PushLogin           string `json:"push_login"`
		PushPassword        string `json:"push_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Name == "" {
		jsonError(w, http.StatusBadRequest, "name is required")
		return
	}

	enabled := boolInt(req.Enabled, true)
	allowAny := boolInt(req.AllowAnyStream, true)
	outHLS := boolInt(req.OutHLS, true)
	outDASH := boolInt(req.OutDASH, false)
	outMpegTS := boolInt(req.OutMpegTS, false)
	outLLHLS := boolInt(req.OutLLHLS, false)
	dvbDash := boolInt(req.DVBDashEnabled, false)
	dvbSubs := boolInt(req.DVBSubtitlesEnabled, false)
	scte35Passthrough := boolInt(req.SCTE35Passthrough, false)
	dvrEnabled := boolInt(req.DVREnabled, false)
	if req.DVRWindowS < 0 {
		jsonError(w, http.StatusBadRequest, "dvr_window_s must be >= 0")
		return
	}
	adMarkerFormat := req.AdMarkerFormat
	if adMarkerFormat == "" {
		adMarkerFormat = "none"
	}
	scte35Mode := req.SCTE35Mode
	if scte35Mode == "" {
		scte35Mode = "disabled"
	}

	id := uuid.New().String()
	_, err := s.db.ExecContext(r.Context(), `
		INSERT INTO applications
		  (id, name, publish_key, enabled, allow_any_stream,
		   out_hls, out_dash, out_mpegts, out_ll_hls,
		   dvb_dash_enabled, dvb_subtitles_enabled,
		   hls_seg_duration_s, hls_playlist_window,
		   scte35_passthrough, ad_marker_format, scte35_mode,
		   dvr_enabled, dvr_window_s, push_login, push_password)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, req.Name, req.PublishKey, enabled, allowAny,
		outHLS, outDASH, outMpegTS, outLLHLS,
		dvbDash, dvbSubs,
		req.HLSSegDurationS, req.HLSPlaylistWindow,
		scte35Passthrough, adMarkerFormat, scte35Mode,
		dvrEnabled, req.DVRWindowS, req.PushLogin, req.PushPassword,
	)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var createdAt, updatedAt string
	s.db.QueryRowContext(r.Context(),
		`SELECT created_at, updated_at FROM applications WHERE id = ?`, id,
	).Scan(&createdAt, &updatedAt)

	data, _ := json.Marshal(map[string]interface{}{
		"id": id, "name": req.Name, "publish_key": req.PublishKey,
		"enabled": enabled == 1, "allow_any_stream": allowAny == 1,
		"out_hls": outHLS == 1, "out_dash": outDASH == 1,
		"out_mpegts": outMpegTS == 1, "out_ll_hls": outLLHLS == 1,
		"dvb_dash_enabled": dvbDash == 1, "dvb_subtitles_enabled": dvbSubs == 1,
		"hls_seg_duration_s": req.HLSSegDurationS, "hls_playlist_window": req.HLSPlaylistWindow,
		"scte35_passthrough": scte35Passthrough == 1, "ad_marker_format": adMarkerFormat,
		"scte35_mode": scte35Mode,
		"dvr_enabled": dvrEnabled == 1, "dvr_window_s": req.DVRWindowS,
		"push_login": req.PushLogin, "has_push_password": req.PushPassword != "",
		"created_at": createdAt, "updated_at": updatedAt,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(data)
}

func (s *Server) handleGetApplication(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var name, publishKey, adMarkerFormat, scte35Mode, pushLogin, createdAt, updatedAt string
	var enabled, allowAny, outHLS, outDASH, outMpegTS, outLLHLS, dvbDash, dvbSubs, hlsSeg, hlsWin, scte35Passthrough, dvrEnabled, dvrWin int
	var hasPushPassword bool
	err := s.db.QueryRowContext(r.Context(), `
		SELECT id, name, publish_key, enabled, allow_any_stream,
		       out_hls, out_dash, out_mpegts, out_ll_hls,
		       dvb_dash_enabled, dvb_subtitles_enabled,
		       hls_seg_duration_s, hls_playlist_window,
		       scte35_passthrough, ad_marker_format, scte35_mode, dvr_enabled, dvr_window_s,
		       push_login, push_password != '', created_at, updated_at
		FROM applications WHERE id = ?`, id,
	).Scan(&id, &name, &publishKey, &enabled, &allowAny,
		&outHLS, &outDASH, &outMpegTS, &outLLHLS,
		&dvbDash, &dvbSubs, &hlsSeg, &hlsWin,
		&scte35Passthrough, &adMarkerFormat, &scte35Mode, &dvrEnabled, &dvrWin,
		&pushLogin, &hasPushPassword, &createdAt, &updatedAt)
	if err != nil {
		jsonError(w, http.StatusNotFound, "application not found")
		return
	}
	data, _ := json.Marshal(map[string]interface{}{
		"id": id, "name": name, "publish_key": publishKey,
		"enabled": enabled == 1, "allow_any_stream": allowAny == 1,
		"out_hls": outHLS == 1, "out_dash": outDASH == 1,
		"out_mpegts": outMpegTS == 1, "out_ll_hls": outLLHLS == 1,
		"dvb_dash_enabled": dvbDash == 1, "dvb_subtitles_enabled": dvbSubs == 1,
		"hls_seg_duration_s": hlsSeg, "hls_playlist_window": hlsWin,
		"scte35_passthrough": scte35Passthrough == 1, "ad_marker_format": adMarkerFormat,
		"scte35_mode": scte35Mode,
		"dvr_enabled": dvrEnabled == 1, "dvr_window_s": dvrWin,
		"push_login": pushLogin, "has_push_password": hasPushPassword,
		"created_at": createdAt, "updated_at": updatedAt,
	})
	jsonOK(w, data)
}

func (s *Server) handleUpdateApplication(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Name                string `json:"name"`
		PublishKey          string `json:"publish_key"`
		Enabled             *bool  `json:"enabled"`
		AllowAnyStream      *bool  `json:"allow_any_stream"`
		OutHLS              *bool  `json:"out_hls"`
		OutDASH             *bool  `json:"out_dash"`
		OutMpegTS           *bool  `json:"out_mpegts"`
		OutLLHLS            *bool  `json:"out_ll_hls"`
		DVBDashEnabled      *bool  `json:"dvb_dash_enabled"`
		DVBSubtitlesEnabled *bool  `json:"dvb_subtitles_enabled"`
		HLSSegDurationS     int    `json:"hls_seg_duration_s"`
		HLSPlaylistWindow   int    `json:"hls_playlist_window"`
		SCTE35Passthrough   *bool  `json:"scte35_passthrough"`
		AdMarkerFormat      string `json:"ad_marker_format"`
		SCTE35Mode          string `json:"scte35_mode"`
		DVREnabled          *bool  `json:"dvr_enabled"`
		DVRWindowS          int    `json:"dvr_window_s"`
		// nil leaves the stored value unchanged, so a client that never
		// shows the password (it is never returned) doesn't wipe it.
		PushLogin    *string `json:"push_login"`
		PushPassword *string `json:"push_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Name == "" {
		jsonError(w, http.StatusBadRequest, "name is required")
		return
	}

	enabled := boolInt(req.Enabled, true)
	allowAny := boolInt(req.AllowAnyStream, true)
	outHLS := boolInt(req.OutHLS, true)
	outDASH := boolInt(req.OutDASH, false)
	outMpegTS := boolInt(req.OutMpegTS, false)
	outLLHLS := boolInt(req.OutLLHLS, false)
	dvbDash := boolInt(req.DVBDashEnabled, false)
	dvbSubs := boolInt(req.DVBSubtitlesEnabled, false)
	scte35Passthrough := boolInt(req.SCTE35Passthrough, false)
	dvrEnabled := boolInt(req.DVREnabled, false)
	if req.DVRWindowS < 0 {
		jsonError(w, http.StatusBadRequest, "dvr_window_s must be >= 0")
		return
	}
	adMarkerFormat := req.AdMarkerFormat
	if adMarkerFormat == "" {
		adMarkerFormat = "none"
	}
	scte35Mode := req.SCTE35Mode
	if scte35Mode == "" {
		scte35Mode = "disabled"
	}

	if req.PushLogin != nil {
		if _, err := s.db.ExecContext(r.Context(),
			`UPDATE applications SET push_login=? WHERE id=?`, *req.PushLogin, id); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if req.PushPassword != nil {
		if _, err := s.db.ExecContext(r.Context(),
			`UPDATE applications SET push_password=? WHERE id=?`, *req.PushPassword, id); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	_, err := s.db.ExecContext(r.Context(), `
		UPDATE applications SET
		  name=?, publish_key=?, enabled=?, allow_any_stream=?,
		  out_hls=?, out_dash=?, out_mpegts=?, out_ll_hls=?,
		  dvb_dash_enabled=?, dvb_subtitles_enabled=?,
		  hls_seg_duration_s=?, hls_playlist_window=?,
		  scte35_passthrough=?, ad_marker_format=?, scte35_mode=?,
		  dvr_enabled=?, dvr_window_s=?,
		  updated_at=datetime('now')
		WHERE id=?`,
		req.Name, req.PublishKey, enabled, allowAny,
		outHLS, outDASH, outMpegTS, outLLHLS,
		dvbDash, dvbSubs,
		req.HLSSegDurationS, req.HLSPlaylistWindow,
		scte35Passthrough, adMarkerFormat, scte35Mode,
		dvrEnabled, req.DVRWindowS, id,
	)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteApplication(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	s.db.ExecContext(r.Context(), `DELETE FROM applications WHERE id = ?`, id)
	w.WriteHeader(http.StatusNoContent)
}

// ─── Stream configs ───────────────────────────────────────────────────────────

func (s *Server) handleListStreamConfigs(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "id")
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, app_id, name_pattern, is_wildcard,
		       out_hls, out_dash, out_mpegts, out_ll_hls,
		       dvb_dash_enabled, dvb_subtitles_enabled,
		       dvr_enabled, dvr_window_s, COALESCE(abr_group_id,''), enabled,
		       scte35_passthrough, ad_marker_format, scte35_mode, max_streams, created_at
		FROM stream_configs WHERE app_id = ? ORDER BY name_pattern`, appID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var list []map[string]interface{}
	for rows.Next() {
		var id, appIDVal, pattern, abrGroup, adMarkerFormat, scte35Mode, createdAt string
		var isWild, outHLS, outDASH, outMpegTS, outLLHLS, dvbDash, dvbSubs, dvrEnabled, dvrWin, enabled, scte35Passthrough, maxStreams int
		if err := rows.Scan(&id, &appIDVal, &pattern, &isWild,
			&outHLS, &outDASH, &outMpegTS, &outLLHLS,
			&dvbDash, &dvbSubs,
			&dvrEnabled, &dvrWin, &abrGroup, &enabled,
			&scte35Passthrough, &adMarkerFormat, &scte35Mode, &maxStreams, &createdAt); err != nil {
			continue
		}
		list = append(list, map[string]interface{}{
			"id": id, "app_id": appIDVal, "name_pattern": pattern,
			"is_wildcard": isWild == 1,
			"out_hls":     outHLS, "out_dash": outDASH,
			"out_mpegts": outMpegTS, "out_ll_hls": outLLHLS,
			"dvb_dash_enabled": dvbDash, "dvb_subtitles_enabled": dvbSubs,
			"dvr_enabled": dvrEnabled, "dvr_window_s": dvrWin,
			"abr_group_id": abrGroup, "enabled": enabled == 1,
			"scte35_passthrough": scte35Passthrough, "ad_marker_format": adMarkerFormat,
			"scte35_mode": scte35Mode, "max_streams": maxStreams,
			"created_at":  createdAt,
		})
	}
	if list == nil {
		list = []map[string]interface{}{}
	}
	data, _ := json.Marshal(list)
	jsonOK(w, data)
}

func (s *Server) handleCreateStreamConfig(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "id")
	var req struct {
		NamePattern         string `json:"name_pattern"`
		IsWildcard          *bool  `json:"is_wildcard"`
		OutHLS              *int   `json:"out_hls"`
		OutDASH             *int   `json:"out_dash"`
		OutMpegTS           *int   `json:"out_mpegts"`
		OutLLHLS            *int   `json:"out_ll_hls"`
		DVBDashEnabled      *int   `json:"dvb_dash_enabled"`
		DVBSubtitlesEnabled *int   `json:"dvb_subtitles_enabled"`
		DVREnabled          *int   `json:"dvr_enabled"`
		DVRWindowS          int    `json:"dvr_window_s"`
		ABRGroupID          string `json:"abr_group_id"`
		Enabled             *bool  `json:"enabled"`

		SCTE35Passthrough *int   `json:"scte35_passthrough"`
		AdMarkerFormat    string `json:"ad_marker_format"`
		SCTE35Mode        string `json:"scte35_mode"`

		// MaxStreams caps concurrent publishes matching a wildcard
		// pattern (0 = no limit). Ignored for exact names.
		MaxStreams int `json:"max_streams"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.NamePattern == "" {
		jsonError(w, http.StatusBadRequest, "name_pattern is required")
		return
	}
	if req.MaxStreams < 0 {
		jsonError(w, http.StatusBadRequest, "max_streams must be 0 (no limit) or more")
		return
	}

	// One config per name: the Streams page creates several at once, and a
	// duplicate would make which one applies depend on creation order.
	var dup int
	s.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM stream_configs WHERE app_id = ? AND name_pattern = ?`,
		appID, req.NamePattern).Scan(&dup)
	if dup > 0 {
		jsonError(w, http.StatusConflict, fmt.Sprintf("stream %q already exists in this application", req.NamePattern))
		return
	}

	isWild := boolInt(req.IsWildcard, false)
	enabled := boolInt(req.Enabled, true)
	outHLS := intPtrVal(req.OutHLS, -1)
	outDASH := intPtrVal(req.OutDASH, -1)
	outMpegTS := intPtrVal(req.OutMpegTS, -1)
	outLLHLS := intPtrVal(req.OutLLHLS, -1)
	dvbDash := intPtrVal(req.DVBDashEnabled, -1)
	dvbSubs := intPtrVal(req.DVBSubtitlesEnabled, -1)
	dvrEnabled := intPtrVal(req.DVREnabled, -1)
	scte35Passthrough := intPtrVal(req.SCTE35Passthrough, -1)
	maxStreams := req.MaxStreams
	if isWild == 0 {
		maxStreams = 0
	}

	var abrGroupID interface{} = nil
	if req.ABRGroupID != "" {
		abrGroupID = req.ABRGroupID
	}

	id := uuid.New().String()
	_, err := s.db.ExecContext(r.Context(), `
		INSERT INTO stream_configs
		  (id, app_id, name_pattern, is_wildcard,
		   out_hls, out_dash, out_mpegts, out_ll_hls,
		   dvb_dash_enabled, dvb_subtitles_enabled,
		   dvr_enabled, dvr_window_s, abr_group_id, enabled,
		   scte35_passthrough, ad_marker_format, scte35_mode, max_streams)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, appID, req.NamePattern, isWild,
		outHLS, outDASH, outMpegTS, outLLHLS,
		dvbDash, dvbSubs,
		dvrEnabled, req.DVRWindowS, abrGroupID, enabled,
		scte35Passthrough, req.AdMarkerFormat, req.SCTE35Mode, maxStreams,
	)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var createdAt string
	s.db.QueryRowContext(r.Context(),
		`SELECT created_at FROM stream_configs WHERE id = ?`, id,
	).Scan(&createdAt)

	data, _ := json.Marshal(map[string]interface{}{
		"id": id, "app_id": appID, "name_pattern": req.NamePattern,
		"is_wildcard": isWild == 1,
		"out_hls":     outHLS, "out_dash": outDASH,
		"out_mpegts": outMpegTS, "out_ll_hls": outLLHLS,
		"dvb_dash_enabled": dvbDash, "dvb_subtitles_enabled": dvbSubs,
		"dvr_enabled": dvrEnabled, "dvr_window_s": req.DVRWindowS,
		"abr_group_id": req.ABRGroupID, "enabled": enabled == 1,
		"scte35_passthrough": scte35Passthrough, "ad_marker_format": req.AdMarkerFormat,
		"scte35_mode": req.SCTE35Mode, "max_streams": maxStreams,
		"created_at":  createdAt,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(data)
}

func (s *Server) handleUpdateStreamConfig(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "id")
	sid := chi.URLParam(r, "sid")
	var req struct {
		NamePattern         string `json:"name_pattern"`
		IsWildcard          *bool  `json:"is_wildcard"`
		OutHLS              *int   `json:"out_hls"`
		OutDASH             *int   `json:"out_dash"`
		OutMpegTS           *int   `json:"out_mpegts"`
		OutLLHLS            *int   `json:"out_ll_hls"`
		DVBDashEnabled      *int   `json:"dvb_dash_enabled"`
		DVBSubtitlesEnabled *int   `json:"dvb_subtitles_enabled"`
		DVREnabled          *int   `json:"dvr_enabled"`
		DVRWindowS          int    `json:"dvr_window_s"`
		ABRGroupID          string `json:"abr_group_id"`
		Enabled             *bool  `json:"enabled"`

		SCTE35Passthrough *int   `json:"scte35_passthrough"`
		AdMarkerFormat    string `json:"ad_marker_format"`
		SCTE35Mode        string `json:"scte35_mode"`

		// MaxStreams caps concurrent publishes matching a wildcard
		// pattern (0 = no limit). Ignored for exact names.
		MaxStreams int `json:"max_streams"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.NamePattern == "" {
		jsonError(w, http.StatusBadRequest, "name_pattern is required")
		return
	}
	if req.MaxStreams < 0 {
		jsonError(w, http.StatusBadRequest, "max_streams must be 0 (no limit) or more")
		return
	}

	isWild := boolInt(req.IsWildcard, false)
	enabled := boolInt(req.Enabled, true)
	outHLS := intPtrVal(req.OutHLS, -1)
	outDASH := intPtrVal(req.OutDASH, -1)
	outMpegTS := intPtrVal(req.OutMpegTS, -1)
	outLLHLS := intPtrVal(req.OutLLHLS, -1)
	dvbDash := intPtrVal(req.DVBDashEnabled, -1)
	dvbSubs := intPtrVal(req.DVBSubtitlesEnabled, -1)
	dvrEnabled := intPtrVal(req.DVREnabled, -1)
	scte35Passthrough := intPtrVal(req.SCTE35Passthrough, -1)
	maxStreams := req.MaxStreams
	if isWild == 0 {
		maxStreams = 0
	}

	var abrGroupID interface{} = nil
	if req.ABRGroupID != "" {
		abrGroupID = req.ABRGroupID
	}

	_, err := s.db.ExecContext(r.Context(), `
		UPDATE stream_configs SET
		  name_pattern=?, is_wildcard=?,
		  out_hls=?, out_dash=?, out_mpegts=?, out_ll_hls=?,
		  dvb_dash_enabled=?, dvb_subtitles_enabled=?,
		  dvr_enabled=?, dvr_window_s=?, abr_group_id=?, enabled=?,
		  scte35_passthrough=?, ad_marker_format=?, scte35_mode=?, max_streams=?
		WHERE id=? AND app_id=?`,
		req.NamePattern, isWild,
		outHLS, outDASH, outMpegTS, outLLHLS,
		dvbDash, dvbSubs,
		dvrEnabled, req.DVRWindowS, abrGroupID, enabled,
		scte35Passthrough, req.AdMarkerFormat, req.SCTE35Mode, maxStreams,
		sid, appID,
	)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteStreamConfig(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "id")
	sid := chi.URLParam(r, "sid")
	s.db.ExecContext(r.Context(),
		`DELETE FROM stream_configs WHERE id=? AND app_id=?`, sid, appID)
	w.WriteHeader(http.StatusNoContent)
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func boolInt(p *bool, defaultVal bool) int {
	if p == nil {
		if defaultVal {
			return 1
		}
		return 0
	}
	if *p {
		return 1
	}
	return 0
}

func intPtrVal(p *int, defaultVal int) int {
	if p == nil {
		return defaultVal
	}
	return *p
}
