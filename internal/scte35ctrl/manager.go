// Package scte35ctrl implements server-side SCTE-35 generation/insertion:
// given an API request, it builds a real splice_info_section
// (internal/scte35's generator) timed to a stream's actual tracked media
// PTS clock (never wall-clock, via streamWatcher) and injects it as an
// ordinary media.Packet{Codec: CodecSCTE35} onto the stream's bus — the
// exact same packet shape internal/ingest/mpegts publishes for a
// passed-through event. internal/hls.Packager and mux.TSMuxer already
// treat any CodecSCTE35 packet identically regardless of origin, so no new
// MPEG-TS/HLS consumption code exists anywhere else; this package only
// needs to build the bytes and publish them at the right moment.
package scte35ctrl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
	"github.com/5centscdn/origin-mediaserver/internal/scte35"
)

// TriggerType selects when an OUT (or a scheduled auto-return IN) actually
// fires, always measured against the stream's own tracked PTS clock.
type TriggerType string

const (
	TriggerImmediate    TriggerType = "immediate"     // fire now, using the stream's last-seen video PTS
	TriggerNextKeyframe TriggerType = "next_keyframe" // fire on the next observed video keyframe
	TriggerStartPTS     TriggerType = "start_pts"     // fire once the stream's tracked PTS reaches StartPTS
)

// OutRequest describes one requested ad-break start.
type OutRequest struct {
	EventID            uint32
	Command            string // "splice_insert" | "time_signal"
	DurationMS         int64  // <= 0 means no declared duration (and no auto-return)
	AutoReturn         bool
	SegmentationTypeID uint8 // used when Command == "time_signal"
	Trigger            TriggerType
	StartPTS           int64 // used when Trigger == TriggerStartPTS
}

var (
	ErrStreamNotLive       = errors.New("scte35ctrl: stream is not currently live")
	ErrModeDisallowsInsert = errors.New("scte35ctrl: stream's scte35_mode does not allow insertion")
	ErrDuplicateEvent      = errors.New("scte35ctrl: event_id is already active or completed for this stream")
	ErrEventNotFound       = errors.New("scte35ctrl: no matching event found")
	ErrCannotCancel        = errors.New("scte35ctrl: event is no longer pending, cannot cancel")
	ErrNoVideoYet          = errors.New("scte35ctrl: stream has not produced a video packet yet, cannot compute a PTS for immediate insertion")
	ErrUnknownCommand      = errors.New(`scte35ctrl: command must be "splice_insert" or "time_signal"`)
)

// segmentationEndTypeID maps a segmentation_type_id "start" value to its
// spec-defined "end" counterpart (SCTE-35 Table 22), for auto-return/manual
// IN on a time_signal-based break.
var segmentationEndTypeID = map[uint8]uint8{
	0x30: 0x31, 0x32: 0x33, 0x34: 0x35, 0x36: 0x37, 0x44: 0x45,
}

// Manager tracks and drives server-side SCTE-35 insertion for every live
// stream, mirroring internal/failover.Manager/internal/pull.Manager's
// shape: constructed once in cmd/server/main.go, exposed through
// api.Server, keyed by stream.
type Manager struct {
	database *db.DB
	registry *media.Registry
	metrics  *metrics.Metrics
	baseCtx  context.Context

	mu       sync.Mutex
	watchers map[media.StreamKey]*streamWatcher
}

// NewManager creates a Manager. baseCtx is the process's root context (not
// a request context) — per-stream watcher goroutines are tied to it, not
// to any one HTTP request.
func NewManager(baseCtx context.Context, database *db.DB, registry *media.Registry, m *metrics.Metrics) *Manager {
	return &Manager{
		database: database,
		registry: registry,
		metrics:  m,
		baseCtx:  baseCtx,
		watchers: make(map[media.StreamKey]*streamWatcher),
	}
}

// InsertOut starts a new ad break: persists the event, resolves the
// stream's ABR group (if any — see resolveGroupMembers), and arms/fires the
// requested trigger independently on every group member using that
// member's own tracked PTS clock (each rendition's encoder timeline is
// independent, so this is what keeps every rendition's generated section
// internally PTS-consistent while still sharing one event_id/duration
// across the whole group).
func (m *Manager) InsertOut(ctx context.Context, app, name string, req OutRequest) (*db.SCTE35Event, error) {
	if req.Command != "splice_insert" && req.Command != "time_signal" {
		return nil, ErrUnknownCommand
	}

	primary := m.registry.Get(media.StreamKey{App: app, Name: name})
	if primary == nil {
		return nil, ErrStreamNotLive
	}

	mode, err := m.resolveMode(ctx, app, name)
	if err != nil {
		return nil, err
	}
	if mode != "insert" && mode != "hybrid" {
		return nil, ErrModeDisallowsInsert
	}

	existing, err := m.database.GetLatestSCTE35Event(ctx, app, name, req.EventID)
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.Status != "cancelled" {
		return nil, ErrDuplicateEvent
	}

	primaryWatcher := m.ensureWatcher(primary)
	if req.Trigger == TriggerImmediate {
		// A watcher's clock only starts once it subscribes to the bus
		// (just above, via ensureWatcher) — for the first insert attempt
		// on a given live stream that's *right now*, so give it a brief,
		// bounded chance to actually observe the next packet instead of
		// failing immediately just because none has arrived in the last
		// few microseconds (see waitForFirstVideo's doc comment).
		if _, ok := primaryWatcher.waitForFirstVideo(ctx, 2*time.Second); !ok {
			return nil, ErrNoVideoYet
		}
	}

	dbEv := db.SCTE35Event{
		ID:                 uuid.New().String(),
		AppName:            app,
		StreamName:         name,
		EventID:            req.EventID,
		Command:            req.Command,
		SegmentationTypeID: req.SegmentationTypeID,
		DurationMS:         req.DurationMS,
		AutoReturn:         req.AutoReturn,
		TriggerType:        string(req.Trigger),
		Status:             "pending", // matches InsertSCTE35Event's DB default — kept in sync explicitly below
	}
	if req.Trigger == TriggerStartPTS {
		pts := req.StartPTS
		dbEv.TriggerPTS = &pts
	}
	if err := m.database.InsertSCTE35Event(ctx, dbEv); err != nil {
		return nil, err
	}

	members := m.resolveGroupMembers(ctx, primary)
	var activated int32

	fire := func(member *media.Stream, pts int64) {
		ev, err := m.buildOutEvent(req, pts)
		if err != nil {
			m.recordInsertError(member.Key.App, member.Key.Name)
			slog.Warn("scte35ctrl: failed to build OUT event", "stream", member.Key, "event_id", req.EventID, "err", err)
			return
		}
		m.publish(member, ev, pts)
		m.metrics.SCTE35InsertedTotal.WithLabelValues(member.Key.App, member.Key.Name, "out").Inc()

		if atomic.CompareAndSwapInt32(&activated, 0, 1) {
			if err := m.database.MarkSCTE35EventActive(ctx, dbEv.ID); err != nil {
				slog.Warn("scte35ctrl: mark event active", "id", dbEv.ID, "err", err)
			}
			// Only observable by the caller when this runs synchronously
			// within InsertOut itself (the "immediate" trigger, before
			// return) — for next_keyframe/start_pts this fires later, from
			// a watcher goroutine, after InsertOut's returned snapshot has
			// already been handed to the caller; harmless to still set
			// (nothing reads dbEv concurrently with this), it just won't
			// be visible until a subsequent List/Get call re-reads the DB.
			dbEv.Status = "active"
			m.metrics.SCTE35ActiveBreaks.WithLabelValues(app, name).Set(1)
		}
		if req.DurationMS > 0 && req.AutoReturn {
			m.scheduleAutoReturn(dbEv.ID, app, name, member, req, pts)
		}
	}

	for _, member := range members {
		w := m.ensureWatcher(member)
		switch req.Trigger {
		case TriggerImmediate:
			pts, ok := w.currentPTS()
			if !ok {
				continue // this member just hasn't produced video yet — skip it, don't fail the whole group
			}
			fire(member, pts)
		case TriggerNextKeyframe:
			w.onNextKeyframe(func(pts int64) {
				if m.isCancelled(ctx, app, name, req.EventID) {
					return
				}
				fire(member, pts)
			})
		case TriggerStartPTS:
			w.onPTSReached(req.StartPTS, func(pts int64) {
				if m.isCancelled(ctx, app, name, req.EventID) {
					return
				}
				fire(member, pts)
			})
		default:
			return nil, fmt.Errorf("scte35ctrl: unknown trigger %q", req.Trigger)
		}
	}

	return &dbEv, nil
}

// InsertIn fires an explicit IN for an active event across every resolved
// group member immediately, using each member's own current tracked PTS.
func (m *Manager) InsertIn(ctx context.Context, app, name string, eventID uint32) (*db.SCTE35Event, error) {
	dbEv, err := m.database.GetActiveSCTE35Event(ctx, app, name, eventID)
	if err != nil {
		return nil, err
	}
	if dbEv == nil {
		return nil, ErrEventNotFound
	}

	primary := m.registry.Get(media.StreamKey{App: app, Name: name})
	if primary == nil {
		return nil, ErrStreamNotLive
	}
	members := m.resolveGroupMembers(ctx, primary)

	for _, member := range members {
		w := m.ensureWatcher(member)
		pts, ok := w.currentPTS()
		if !ok {
			continue
		}
		ev, err := m.buildInEvent(*dbEv, pts)
		if err != nil {
			m.recordInsertError(member.Key.App, member.Key.Name)
			continue
		}
		m.publish(member, ev, pts)
		m.metrics.SCTE35InsertedTotal.WithLabelValues(member.Key.App, member.Key.Name, "in").Inc()
	}

	if err := m.database.MarkSCTE35EventCompleted(ctx, dbEv.ID); err != nil {
		return nil, err
	}
	m.metrics.SCTE35ActiveBreaks.WithLabelValues(app, name).Set(0)
	dbEv.Status = "completed"
	return dbEv, nil
}

// List returns every pending/active event for (app, name).
func (m *Manager) List(ctx context.Context, app, name string) ([]db.SCTE35Event, error) {
	return m.database.ListSCTE35Events(ctx, app, name, false)
}

// Cancel marks a pending (not yet fired) event cancelled — any armed
// next_keyframe/start_pts callback for it becomes a no-op (see isCancelled)
// rather than being forcibly removed from the watcher, which keeps this
// safe under concurrent firing.
func (m *Manager) Cancel(ctx context.Context, app, name string, eventID uint32) error {
	ev, err := m.database.GetLatestSCTE35Event(ctx, app, name, eventID)
	if err != nil {
		return err
	}
	if ev == nil {
		return ErrEventNotFound
	}
	if ev.Status != "pending" {
		return ErrCannotCancel
	}
	return m.database.MarkSCTE35EventCancelled(ctx, ev.ID)
}

// isCancelled re-checks a pending event's status just before an armed
// next_keyframe/start_pts callback actually fires, so a Cancel that landed
// in the window between arming and firing is honored — a cancelled event's
// callback becomes a no-op rather than being forcibly torn out of the
// watcher's waiter list.
func (m *Manager) isCancelled(ctx context.Context, app, name string, eventID uint32) bool {
	ev, err := m.database.GetLatestSCTE35Event(ctx, app, name, eventID)
	if err != nil || ev == nil {
		return false
	}
	return ev.Status == "cancelled"
}

// resolveMode looks up the stream's effective scte35_mode, the same
// app-then-stream-override resolution cmd/server/main.go's
// startStreamOutputs already does for outHLS/adMarkerFormat/etc.
// Duplicated here (rather than shared) because cmd/server/main.go's
// resolution is a local block, not an exported helper, and this package
// can't import the main package.
func (m *Manager) resolveMode(ctx context.Context, app, name string) (string, error) {
	appCfg, err := m.database.GetApplicationByName(ctx, app)
	if err != nil {
		return "", err
	}
	mode := "disabled"
	if appCfg == nil {
		return mode, nil
	}
	mode = appCfg.SCTE35Mode
	streamCfg, err := m.database.GetStreamConfig(ctx, appCfg.ID, name)
	if err != nil {
		return "", err
	}
	if streamCfg != nil {
		mode = db.EffectiveSCTE35Mode(streamCfg.SCTE35Mode, mode)
	}
	return mode, nil
}

// resolveGroupMembers returns every live rendition that should receive the
// same event as stream — stream itself, plus any ABR siblings, via
// whichever of the two ABR mechanisms applies (see the package doc and the
// plan this was designed against): RTMP %i-pattern groups expose sibling
// membership directly via stream.ABRGroup/registry.GetABRGroup; DB-backed
// abr_groups/abr_variants groups (the only mechanism SRT-ingested streams
// can participate in, since SRT never sets ABRGroup) require a reverse
// lookup this codebase doesn't have anywhere else, done here via
// abr_variants.src_app_name/stream_name. Falls back to just stream itself
// if neither applies.
func (m *Manager) resolveGroupMembers(ctx context.Context, stream *media.Stream) []*media.Stream {
	if stream.ABRGroup != "" {
		if group := m.registry.GetABRGroup(stream.Key.App, stream.ABRGroup); len(group) > 0 {
			return group
		}
		return []*media.Stream{stream}
	}

	var groupID string
	err := m.database.QueryRowContext(ctx, `
		SELECT group_id FROM abr_variants WHERE src_app_name = ? AND stream_name = ?`,
		stream.Key.App, stream.Key.Name,
	).Scan(&groupID)
	if err != nil || groupID == "" {
		return []*media.Stream{stream}
	}

	rows, err := m.database.QueryContext(ctx, `
		SELECT src_app_name, stream_name FROM abr_variants WHERE group_id = ?`, groupID)
	if err != nil {
		return []*media.Stream{stream}
	}
	defer rows.Close()

	seen := map[media.StreamKey]bool{}
	var members []*media.Stream
	for rows.Next() {
		var app, name string
		if err := rows.Scan(&app, &name); err != nil {
			continue
		}
		key := media.StreamKey{App: app, Name: name}
		if seen[key] {
			continue
		}
		seen[key] = true
		if s := m.registry.Get(key); s != nil {
			members = append(members, s)
		}
	}
	if len(members) == 0 {
		return []*media.Stream{stream}
	}
	return members
}

// ensureWatcher returns the current watcher for stream, creating one (and
// starting its watch goroutine) if none exists yet, or if the existing one
// belongs to a different (stale, since-reconnected) *media.Stream instance
// at the same key — see streamWatcher's doc comment.
func (m *Manager) ensureWatcher(stream *media.Stream) *streamWatcher {
	m.mu.Lock()
	if w, ok := m.watchers[stream.Key]; ok && w.stream == stream {
		m.mu.Unlock()
		return w
	}
	w := newStreamWatcher(stream)
	m.watchers[stream.Key] = w
	m.mu.Unlock()

	go func() {
		w.run(m.baseCtx)

		m.mu.Lock()
		if m.watchers[stream.Key] == w {
			delete(m.watchers, stream.Key)
		}
		m.mu.Unlock()

		// The bus closed (stream ended) — a later reconnect must never see
		// anything left pending/active from this connection's lifetime.
		if err := m.database.CancelOrphanedSCTE35Events(context.Background(), stream.Key.App, stream.Key.Name); err != nil {
			slog.Warn("scte35ctrl: cancel orphaned events", "stream", stream.Key, "err", err)
		}
	}()

	return w
}

// publish injects ev onto member's bus as an ordinary CodecSCTE35 packet —
// indistinguishable, downstream, from one internal/ingest/mpegts demuxed
// from the wire, except for ev.Source (already SourceInserted, set by the
// generator).
func (m *Manager) publish(member *media.Stream, ev *scte35.Event, pts int64) {
	member.Bus.Publish(&media.Packet{
		Type:      media.PacketMedia,
		Codec:     media.CodecSCTE35,
		DTS:       pts,
		PTS:       pts,
		Data:      ev.RawSection,
		SCTE35:    ev,
		WallClock: time.Now(),
	})
}

// buildOutEvent generates the OUT section for req at pts, dispatching on
// req.Command.
func (m *Manager) buildOutEvent(req OutRequest, pts int64) (*scte35.Event, error) {
	if req.Command == "time_signal" {
		return scte35.BuildTimeSignal(scte35.TimeSignalOptions{
			PTS: pts, HasPTS: true,
			SegmentationEventID:    req.EventID,
			SegmentationTypeID:     req.SegmentationTypeID,
			SegmentationDurationMS: req.DurationMS,
		})
	}
	return scte35.BuildSpliceInsert(scte35.SpliceInsertOptions{
		EventID:      req.EventID,
		OutOfNetwork: true,
		PTS:          pts,
		HasPTS:       true,
		DurationMS:   req.DurationMS,
		AutoReturn:   req.AutoReturn,
	})
}

// buildInEvent generates the matching IN section for a completed/completing
// event at pts.
func (m *Manager) buildInEvent(dbEv db.SCTE35Event, pts int64) (*scte35.Event, error) {
	if dbEv.Command == "time_signal" {
		endType := segmentationEndTypeID[dbEv.SegmentationTypeID]
		if endType == 0 {
			return nil, fmt.Errorf("scte35ctrl: no known segmentation end type for start type %#x", dbEv.SegmentationTypeID)
		}
		return scte35.BuildTimeSignal(scte35.TimeSignalOptions{
			PTS: pts, HasPTS: true,
			SegmentationEventID: dbEv.EventID,
			SegmentationTypeID:  endType,
		})
	}
	return scte35.BuildSpliceInsert(scte35.SpliceInsertOptions{
		EventID:      dbEv.EventID,
		OutOfNetwork: false,
		PTS:          pts,
		HasPTS:       true,
	})
}

// scheduleAutoReturn arms a real timer that builds and injects an actual IN
// section after DurationMS — this is what lets the existing HLS
// StateMachine/MPEG-TS pass-through handle an auto-return break exactly
// like a real downstream splice_insert IN, with no separate "synthetic IN"
// concept needed anywhere else in the pipeline. The wall-clock timer only
// decides *when* to build the section; the section's own embedded PTS is
// still the member's live tracked PTS at fire time, never the timer's
// wall-clock instant.
func (m *Manager) scheduleAutoReturn(dbID, app, name string, member *media.Stream, req OutRequest, outPTS int64) {
	time.AfterFunc(time.Duration(req.DurationMS)*time.Millisecond, func() {
		ctx := context.Background()
		if m.isCancelled(ctx, app, name, req.EventID) {
			return
		}
		// Already completed by a manual IN in the meantime — don't fire a
		// second one.
		ev, err := m.database.GetLatestSCTE35Event(ctx, app, name, req.EventID)
		if err != nil || ev == nil || ev.Status == "completed" {
			return
		}

		w := m.ensureWatcher(member)
		pts, ok := w.currentPTS()
		if !ok {
			pts = outPTS // stream produced no further video — fall back to the OUT's own PTS rather than dropping the auto-return entirely
		}

		inEv, err := m.buildInEvent(*ev, pts)
		if err != nil {
			m.recordInsertError(member.Key.App, member.Key.Name)
			slog.Warn("scte35ctrl: failed to build auto-return IN", "stream", member.Key, "event_id", req.EventID, "err", err)
			return
		}
		m.publish(member, inEv, pts)
		m.metrics.SCTE35InsertedTotal.WithLabelValues(member.Key.App, member.Key.Name, "in").Inc()

		if err := m.database.MarkSCTE35EventCompleted(ctx, dbID); err != nil {
			slog.Warn("scte35ctrl: mark event completed", "id", dbID, "err", err)
		}
		m.metrics.SCTE35ActiveBreaks.WithLabelValues(app, name).Set(0)
	})
}

func (m *Manager) recordInsertError(app, name string) {
	if m.metrics != nil {
		m.metrics.SCTE35InsertErrorsTotal.WithLabelValues(app, name).Inc()
	}
}
