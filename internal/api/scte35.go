package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/scte35ctrl"
)

// handleInsertSCTE35 implements POST /streams/{app}/{name}/scte35 — both
// "out" (start an ad break) and "in" (end one) actions, per the request.
func (s *Server) handleInsertSCTE35(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	name := chi.URLParam(r, "name")

	var req struct {
		Action             string  `json:"action"`
		EventID            uint32  `json:"event_id"`
		Duration           float64 `json:"duration"` // seconds
		Command            string  `json:"command"`
		SegmentationTypeID *uint8  `json:"segmentation_type_id"`
		Start              string  `json:"start"`     // "immediate" (default) | "next_keyframe"
		StartPTS           *int64  `json:"start_pts"` // mutually exclusive with Start
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	switch req.Action {
	case "out":
		s.handleSCTE35Out(w, r, app, name, req.EventID, req.Duration, req.Command, req.SegmentationTypeID, req.Start, req.StartPTS)
	case "in":
		s.handleSCTE35In(w, r, app, name, req.EventID)
	default:
		jsonError(w, http.StatusBadRequest, `action must be "out" or "in"`)
	}
}

func (s *Server) handleSCTE35Out(
	w http.ResponseWriter, r *http.Request,
	app, name string,
	eventID uint32, durationS float64, command string, segTypeID *uint8, start string, startPTS *int64,
) {
	if start != "" && startPTS != nil {
		jsonError(w, http.StatusBadRequest, `"start" and "start_pts" are mutually exclusive`)
		return
	}

	trigger := scte35ctrl.TriggerImmediate
	var resolvedStartPTS int64
	switch {
	case startPTS != nil:
		trigger = scte35ctrl.TriggerStartPTS
		resolvedStartPTS = *startPTS
	case start == "" || start == "immediate":
		trigger = scte35ctrl.TriggerImmediate
	case start == "next_keyframe":
		trigger = scte35ctrl.TriggerNextKeyframe
	default:
		jsonError(w, http.StatusBadRequest, `"start" must be "immediate" or "next_keyframe"`)
		return
	}

	if command == "" {
		command = "splice_insert"
	}
	var segType uint8
	if segTypeID != nil {
		segType = *segTypeID
	}

	ev, err := s.scte35.InsertOut(r.Context(), app, name, scte35ctrl.OutRequest{
		EventID:    eventID,
		Command:    command,
		DurationMS: int64(durationS * 1000),
		// A declared duration always implies auto-return — there's no
		// separate API field for it (matching the task's own examples,
		// which never pass one), since an OUT with a duration and no
		// automatic return has no real meaning for this API.
		AutoReturn:         durationS > 0,
		SegmentationTypeID: segType,
		Trigger:            trigger,
		StartPTS:           resolvedStartPTS,
	})
	if err != nil {
		writeSCTE35Error(w, err)
		return
	}

	data, _ := json.Marshal(scte35EventResponse(ev))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(data)
}

func (s *Server) handleSCTE35In(w http.ResponseWriter, r *http.Request, app, name string, eventID uint32) {
	ev, err := s.scte35.InsertIn(r.Context(), app, name, eventID)
	if err != nil {
		writeSCTE35Error(w, err)
		return
	}
	data, _ := json.Marshal(scte35EventResponse(ev))
	jsonOK(w, data)
}

// handleListSCTE35 implements GET /streams/{app}/{name}/scte35 — every
// pending/active event for the stream.
func (s *Server) handleListSCTE35(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	name := chi.URLParam(r, "name")

	events, err := s.scte35.List(r.Context(), app, name)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	list := make([]map[string]interface{}, 0, len(events))
	for i := range events {
		list = append(list, scte35EventResponse(&events[i]))
	}
	data, _ := json.Marshal(list)
	jsonOK(w, data)
}

// handleCancelSCTE35 implements DELETE
// /streams/{app}/{name}/scte35/{event_id} — cancels a pending (not yet
// fired) event; anything already active/completed returns 409.
func (s *Server) handleCancelSCTE35(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	name := chi.URLParam(r, "name")

	eventID, err := strconv.ParseUint(chi.URLParam(r, "event_id"), 10, 32)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "event_id must be an integer")
		return
	}

	if err := s.scte35.Cancel(r.Context(), app, name, uint32(eventID)); err != nil {
		writeSCTE35Error(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeSCTE35Error maps a scte35ctrl error to the appropriate HTTP status.
func writeSCTE35Error(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, scte35ctrl.ErrStreamNotLive), errors.Is(err, scte35ctrl.ErrEventNotFound):
		jsonError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, scte35ctrl.ErrDuplicateEvent), errors.Is(err, scte35ctrl.ErrCannotCancel):
		jsonError(w, http.StatusConflict, err.Error())
	case errors.Is(err, scte35ctrl.ErrModeDisallowsInsert),
		errors.Is(err, scte35ctrl.ErrUnknownCommand),
		errors.Is(err, scte35ctrl.ErrNoVideoYet):
		jsonError(w, http.StatusBadRequest, err.Error())
	default:
		jsonError(w, http.StatusInternalServerError, err.Error())
	}
}

func scte35EventResponse(ev *db.SCTE35Event) map[string]interface{} {
	m := map[string]interface{}{
		"id": ev.ID, "app": ev.AppName, "stream": ev.StreamName,
		"event_id": ev.EventID, "command": ev.Command,
		"duration_ms": ev.DurationMS, "auto_return": ev.AutoReturn,
		"trigger_type": ev.TriggerType, "status": ev.Status,
		"out_fired_at": ev.OutFiredAt, "in_fired_at": ev.InFiredAt,
		"created_at": ev.CreatedAt,
	}
	if ev.SegmentationTypeID != 0 {
		m["segmentation_type_id"] = ev.SegmentationTypeID
	}
	if ev.TriggerPTS != nil {
		m["start_pts"] = *ev.TriggerPTS
	}
	return m
}
