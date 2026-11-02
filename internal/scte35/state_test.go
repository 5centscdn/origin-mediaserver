package scte35

import (
	"testing"

	"github.com/5centscdn/origin-mediaserver/internal/mux"
)

func TestStateMachine_OutIn(t *testing.T) {
	sm := NewStateMachine()

	out, err := Parse(buildSpliceInsert(t, 1, true, 30000, false))
	if err != nil {
		t.Fatalf("Parse(out): %v", err)
	}
	actions := sm.Apply(out)
	if len(actions) != 1 || actions[0].Type != ActionCueOut {
		t.Fatalf("Apply(out) = %+v, want one ActionCueOut", actions)
	}
	if state, _, _, _ := sm.Snapshot(); state != StateAdBreak {
		t.Fatalf("state = %v, want StateAdBreak", state)
	}

	in, err := Parse(buildSpliceInsert(t, 1, false, 0, false))
	if err != nil {
		t.Fatalf("Parse(in): %v", err)
	}
	actions = sm.Apply(in)
	if len(actions) != 1 || actions[0].Type != ActionCueIn {
		t.Fatalf("Apply(in) = %+v, want one ActionCueIn", actions)
	}
	if state, _, _, _ := sm.Snapshot(); state != StateNormal {
		t.Fatalf("state = %v, want StateNormal", state)
	}
}

func TestStateMachine_DuplicateOutSuppressed(t *testing.T) {
	sm := NewStateMachine()
	section := buildSpliceInsert(t, 7, true, 30000, false)

	ev1, _ := Parse(section)
	if actions := sm.Apply(ev1); len(actions) != 1 {
		t.Fatalf("first Apply = %+v, want 1 action", actions)
	}

	// Same section retransmitted (identical bytes, freshly parsed so
	// ReceivedAt differs but EventID does not) must not re-emit CUE-OUT.
	ev2, _ := Parse(section)
	if actions := sm.Apply(ev2); len(actions) != 0 {
		t.Fatalf("retransmitted Apply = %+v, want 0 actions (deduped)", actions)
	}
}

func TestStateMachine_INWithoutActiveBreakIgnored(t *testing.T) {
	sm := NewStateMachine()
	in, _ := Parse(buildSpliceInsert(t, 5, false, 0, false))
	if actions := sm.Apply(in); len(actions) != 0 {
		t.Fatalf("Apply(unexpected in) = %+v, want 0 actions", actions)
	}
}

func TestStateMachine_AutoReturnWithoutExplicitIn(t *testing.T) {
	sm := NewStateMachine()
	out, _ := Parse(buildSpliceInsert(t, 3, true, 1, true)) // 1ms duration, auto_return
	sm.Apply(out)

	if _, ok := sm.PendingAutoReturn(); !ok {
		t.Fatal("PendingAutoReturn = false, want true after an auto_return OUT")
	}

	actions := sm.ForceAutoReturn()
	if len(actions) != 1 || actions[0].Type != ActionCueIn {
		t.Fatalf("ForceAutoReturn = %+v, want one ActionCueIn", actions)
	}
	if state, _, _, _ := sm.Snapshot(); state != StateNormal {
		t.Fatalf("state after ForceAutoReturn = %v, want StateNormal", state)
	}
	// A second call once already NORMAL must be a no-op.
	if actions := sm.ForceAutoReturn(); len(actions) != 0 {
		t.Fatalf("second ForceAutoReturn = %+v, want no-op", actions)
	}
}

// TestStateMachine_AutoClearsEvenWithoutAutoReturnBit is the regression
// test for a live production finding: a real upstream source (observed via
// Nimble) sends splice_insert OUT events with auto_return=0 and a known
// break_duration, then never sends a matching explicit IN — yet a real,
// widely-deployed HLS origin (Nimble itself, pulling the very same source)
// still stops emitting CUE-OUT-CONT once that duration elapses. Strictly
// honoring auto_return=0 (waiting forever for an IN that never comes)
// matches the letter of the spec but is useless in practice, so a known
// duration alone must be enough to auto-clear — see
// applySpliceInsertLocked's comment.
func TestStateMachine_AutoClearsEvenWithoutAutoReturnBit(t *testing.T) {
	sm := NewStateMachine()
	out, _ := Parse(buildSpliceInsert(t, 11, true, 1, false)) // 1ms duration, auto_return=false
	sm.Apply(out)

	if _, ok := sm.PendingAutoReturn(); !ok {
		t.Fatal("PendingAutoReturn = false, want true for a known-duration OUT even with auto_return=0")
	}

	actions := sm.ForceAutoReturn()
	if len(actions) != 1 || actions[0].Type != ActionCueIn {
		t.Fatalf("ForceAutoReturn = %+v, want one ActionCueIn", actions)
	}
	if state, _, _, _ := sm.Snapshot(); state != StateNormal {
		t.Fatalf("state after ForceAutoReturn = %v, want StateNormal", state)
	}
}

func TestStateMachine_CancelIgnored(t *testing.T) {
	sm := NewStateMachine()
	// A cancel indicator splice_insert has HasEventID=true but the parser
	// stops before OutOfNetwork is ever set, so it must not start a break.
	section := buildSpliceInsert(t, 9, true, 0, false)
	// Flip splice_event_cancel_indicator bit in the already-built section:
	// event_id(4) + cancel byte is the 5th cmd byte, located right after
	// the 14-byte header.
	section[18] |= 0x80
	// Recompute CRC after mutating the section (cancel flag is inside the
	// command body).
	crc := mux.CRC32MPEG2(section[:len(section)-4])
	section[len(section)-4] = byte(crc >> 24)
	section[len(section)-3] = byte(crc >> 16)
	section[len(section)-2] = byte(crc >> 8)
	section[len(section)-1] = byte(crc)

	ev, err := Parse(section)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !ev.HasEventID {
		t.Fatal("HasEventID = false, want true (event_id parses before cancel check)")
	}
	if ev.OutOfNetwork {
		t.Fatal("OutOfNetwork = true, want false — parsing must stop at the cancel indicator")
	}

	actions := sm.Apply(ev)
	if len(actions) != 0 {
		t.Fatalf("Apply(cancel) = %+v, want 0 actions", actions)
	}
}
