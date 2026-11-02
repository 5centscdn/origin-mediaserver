package scte35

import (
	"sync"
	"time"
)

// State is a stream's ad-break state.
type State int

const (
	StateNormal State = iota
	StateAdBreak
)

func (s State) String() string {
	if s == StateAdBreak {
		return "ad_break"
	}
	return "normal"
}

// ActionType tells the HLS marker layer (internal/hls/admarkers.go) what to
// emit in response to a StateMachine transition.
type ActionType int

const (
	ActionCueOut ActionType = iota
	ActionCueIn
)

// Action is one marker-worthy state transition. Event is nil only for an
// IN produced by ForceAutoReturn (no fresh SCTE-35 section triggered it).
type Action struct {
	Type  ActionType
	Event *Event
}

// StateMachine tracks one stream's NORMAL/AD_BREAK state and turns a
// sequence of parsed SCTE-35 Events into OUT/IN actions, suppressing
// duplicates (retransmitted sections) and tracking auto_return breaks that
// never receive an explicit IN.
//
// This is deliberately the only place SCTE-35 events affect anything beyond
// pure pass-through: MPEG-TS output never consults a StateMachine (see
// mux.TSMuxer.WriteSCTE35's doc comment) — only HLS ad-marker generation
// does, via internal/hls/admarkers.go.
//
// Scope note: today one StateMachine is created per media.Stream (one per
// SRT/RTMP/etc. connection), so ABR renditions of the same source each run
// their own instance. Real ABR ladders duplicate identical SCTE-35 sections
// (same splice_event_id) into every rendition, so per-variant processing
// already yields consistent event-id-tagged markers across renditions in
// the common case. A future shared timeline — one StateMachine per ABR
// group instead of per Stream, mirroring hls.PlaylistRegistry's
// map[media.StreamKey]*Playlist shape — would guarantee this even when
// renditions' encoders drift; that's intentionally not built here.
type StateMachine struct {
	mu sync.Mutex

	state         State
	activeEventID uint32
	breakStart    time.Time
	expectedMS    int64
	autoReturn    bool

	dedup          *dedupTracker
	duplicatesSeen int64 // count of Apply() calls suppressed as retransmissions — see DuplicatesSuppressed
}

// NewStateMachine creates a state machine in the NORMAL state.
func NewStateMachine() *StateMachine {
	return &StateMachine{dedup: newDedupTracker()}
}

// Apply processes one parsed Event and returns the marker actions (if any)
// the HLS layer should emit. Safe for concurrent use, though in practice
// it's only ever called from one stream's packager goroutine.
func (sm *StateMachine) Apply(ev *Event) []Action {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	switch ev.CommandType {
	case CommandSpliceInsert:
		return sm.applySpliceInsertLocked(ev)
	case CommandTimeSignal:
		return sm.applyTimeSignalLocked(ev)
	default:
		return nil
	}
}

// applySpliceInsertLocked. Dedup is keyed by (event_id, command_type,
// out_of_network) rather than just (event_id, command_type): SCTE-35
// legitimately reuses the same splice_event_id for both the OUT
// (out_of_network=1) and the later IN (out_of_network=0) — those are two
// distinct actions, not a retransmission of each other.
func (sm *StateMachine) applySpliceInsertLocked(ev *Event) []Action {
	if !ev.HasEventID {
		return nil
	}
	if sm.dedup.seen(ev.EventID, ev.CommandType, ev.OutOfNetwork) {
		sm.duplicatesSeen++
		return nil // retransmission of a message already acted on
	}

	if ev.OutOfNetwork {
		sm.dedup.mark(ev.EventID, ev.CommandType, ev.OutOfNetwork)
		sm.state = StateAdBreak
		sm.activeEventID = ev.EventID
		sm.breakStart = ev.ReceivedAt
		sm.expectedMS = ev.BreakDurationMS
		// Auto-clear whenever a duration is known at all, regardless of the
		// spec's auto_return bit: real upstream sources commonly set
		// auto_return=0 yet never send a matching explicit IN either,
		// which would otherwise leave the break open forever. Confirmed
		// against a live production source: Nimble (a widely-deployed HLS
		// origin) auto-clears its own CUE-OUT-CONT at exactly the stated
		// Duration for such a source, despite auto_return=0 — this matches
		// that observed real-world behavior rather than the letter of the
		// spec, which would otherwise be useless here in practice.
		sm.autoReturn = ev.HasBreakDuration
		return []Action{{Type: ActionCueOut, Event: ev}}
	}

	// out_of_network_indicator == false is the explicit return-to-network.
	if sm.state == StateAdBreak {
		sm.dedup.mark(ev.EventID, ev.CommandType, ev.OutOfNetwork)
		sm.state = StateNormal
		return []Action{{Type: ActionCueIn, Event: ev}}
	}
	return nil
}

// applyTimeSignalLocked handles time_signal commands paired with a
// segmentation_descriptor — time_signal alone carries no event id or
// out-of-network flag, so without a segmentation_descriptor there's nothing
// to key state on.
func (sm *StateMachine) applyTimeSignalLocked(ev *Event) []Action {
	if !ev.HasSegmentation {
		return nil
	}
	isStart := isSegmentationStart(ev.SegmentationTypeID)
	if sm.dedup.seen(ev.SegmentationEventID, ev.CommandType, isStart) {
		sm.duplicatesSeen++
		return nil
	}

	switch {
	case isStart:
		sm.dedup.mark(ev.SegmentationEventID, ev.CommandType, isStart)
		sm.state = StateAdBreak
		sm.activeEventID = ev.SegmentationEventID
		sm.breakStart = ev.ReceivedAt
		sm.expectedMS = ev.SegmentationDurationMS
		// segmentation_descriptor has no auto_return field at all, but the
		// same real-world rationale as applySpliceInsertLocked applies:
		// auto-clear whenever a duration is known, rather than waiting
		// forever for an explicit segmentation-end that may never come.
		sm.autoReturn = ev.HasSegmentationDuration
		return []Action{{Type: ActionCueOut, Event: ev}}
	case isSegmentationEnd(ev.SegmentationTypeID):
		if sm.state == StateAdBreak {
			sm.dedup.mark(ev.SegmentationEventID, ev.CommandType, isStart)
			sm.state = StateNormal
			return []Action{{Type: ActionCueIn, Event: ev}}
		}
	}
	return nil
}

// isSegmentationStart reports whether a segmentation_type_id (SCTE 35
// Table 22) represents the start of a break — the Program/Provider/
// Distributor Advertisement and Placement Opportunity Start types.
func isSegmentationStart(typeID uint8) bool {
	switch typeID {
	case 0x30, 0x32, 0x34, 0x36, 0x44:
		return true
	}
	return false
}

// isSegmentationEnd reports whether a segmentation_type_id represents the
// end of a break — the *_End counterpart of each isSegmentationStart type.
func isSegmentationEnd(typeID uint8) bool {
	switch typeID {
	case 0x31, 0x33, 0x35, 0x37, 0x45:
		return true
	}
	return false
}

// PendingAutoReturn reports whether the stream is currently in an ad break
// with a known duration (regardless of the event's own auto_return bit —
// see applySpliceInsertLocked), and when it should end. The HLS packager
// polls this at segment-cut time (see admarkers.go) since such a break may
// end with no explicit IN ever arriving.
func (sm *StateMachine) PendingAutoReturn() (deadline time.Time, ok bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.state != StateAdBreak || !sm.autoReturn || sm.expectedMS <= 0 {
		return time.Time{}, false
	}
	return sm.breakStart.Add(time.Duration(sm.expectedMS) * time.Millisecond), true
}

// ForceAutoReturn transitions AD_BREAK -> NORMAL when a break's known-
// duration deadline has passed with no explicit IN, without requiring a
// new Event.
func (sm *StateMachine) ForceAutoReturn() []Action {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.state != StateAdBreak || !sm.autoReturn {
		return nil
	}
	sm.state = StateNormal
	return []Action{{Type: ActionCueIn}}
}

// Snapshot returns the current state, for CUE-OUT-CONT elapsed-time
// computation when a segment is cut mid-break without a fresh event.
func (sm *StateMachine) Snapshot() (state State, activeEventID uint32, breakStart time.Time, expectedMS int64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.state, sm.activeEventID, sm.breakStart, sm.expectedMS
}

// DuplicatesSuppressed returns the cumulative count of Apply() calls that
// were suppressed as retransmissions of an already-acted-on message — for
// callers reporting a media_scte35_duplicates_total-style metric (see
// internal/hls/admarkers.go), which compares successive calls' deltas
// rather than resetting this counter.
func (sm *StateMachine) DuplicatesSuppressed() int64 {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.duplicatesSeen
}
