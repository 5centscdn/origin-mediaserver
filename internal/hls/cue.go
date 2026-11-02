package hls

import (
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/scte35"
)

// buildCueOut converts an OUT action into the #EXT-X-CUE-OUT marker's
// duration, in seconds (0 when the triggering event carried no known
// duration — see CueOutMarker's doc comment) and its base64 splice_info_
// section, via the same encoding ad_marker_format=splicepoint uses — this
// is the "SCTE35=" attribute real-world CUE-OUT/-CONT consumers (e.g.
// Nimble) expect, letting a downstream player/ad-decisioning system read
// the raw signal straight from the playlist tag instead of having to
// re-demux the TS pass-through for PID 0x86.
func buildCueOut(action scte35.Action) *CueOutMarker {
	var durationS float64
	if action.Event != nil {
		if action.Event.HasBreakDuration {
			durationS = float64(action.Event.BreakDurationMS) / 1000.0
		} else if action.Event.HasSegmentationDuration {
			durationS = float64(action.Event.SegmentationDurationMS) / 1000.0
		}
	}
	return &CueOutMarker{DurationS: durationS, SCTE35: buildSplicepoint(action.Event)}
}

// buildCueOutCont computes #EXT-X-CUE-OUT-CONT's ElapsedTime/Duration for a
// segment cut mid-break with no fresh SCTE-35 event — elapsed is measured
// from the state machine's recorded break start to now, which is accurate
// to within one segment's worth of cut latency (see admarkers.go's doc
// comment on segment-boundary handling). scte35 is the base64 section from
// the OUT that started this break (Packager.activeBreakSCTE35), carried
// forward on every CONT tag for the rest of the break — the StateMachine
// itself never retains the triggering Event beyond the Apply() call that
// produced it. Returns nil if state isn't currently AD_BREAK.
func buildCueOutCont(state scte35.State, breakStart, now time.Time, expectedMS int64, scte35Section string) *CueOutContMarker {
	if state != scte35.StateAdBreak {
		return nil
	}
	elapsedS := now.Sub(breakStart).Seconds()
	if elapsedS < 0 {
		elapsedS = 0
	}
	return &CueOutContMarker{
		ElapsedS:  elapsedS,
		DurationS: float64(expectedMS) / 1000.0,
		SCTE35:    scte35Section,
	}
}
