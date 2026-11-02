// Package hls's admarkers.go turns internal/scte35 state-machine Actions
// into concrete HLS playlist tags, per the stream's configured
// ad_marker_format (see SetSCTE35Config): "cue" (cue.go, #EXT-X-CUE-OUT/
// -IN/-CONT), "splicepoint" (splicepoint.go, #EXT-X-SPLICEPOINT-SCTE35),
// "daterange" (daterange.go, #EXT-X-DATERANGE), or "none"/"" (no markers —
// SCTE-35 detection and MPEG-TS pass-through still run untouched).
//
// Scope: only the MPEG-TS ("ts") HLS container is covered — see
// handleSCTE35. fMP4/CMAF SCTE-35 signaling (an emsg box per segment) is a
// documented follow-up, not implemented here; a fMP4/CMAF stream simply
// gets no markers, the same safe no-op as ad_marker_format=none.
//
// Segment-boundary handling: a marker always attaches to the next segment
// flushed after its triggering event, never forcing an early cut — the
// existing keyframe+target-duration segmenter (handleVideo/handleVideoFMP4)
// is untouched. The tag itself still carries the event's true timestamp
// (DATERANGE's START-DATE via wallClockFor, CUE-OUT-CONT's ElapsedTime
// computed from the real break start) so no timing information is lost
// even though the segment boundary isn't splice-accurate. A future
// splice-accurate cut would hook in right where handleVideo's existing
// `if pkt.IsKeyframe { ... duration check ... }` block already decides
// whether to flush — an `if p.pendingSplice && pkt.IsKeyframe` condition
// alongside it, not built here.
//
// ABR: each Packager (one per rendition) runs its own StateMachine, keyed
// by nothing more than the SCTE-35 event_ids it happens to see — see
// scte35.StateMachine's doc comment for why that already yields consistent
// markers across a real ABR ladder's renditions in the common case, and
// what a future shared-timeline object would look like.
package hls

import (
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/scte35"
)

// handleSCTE35 processes one CodecSCTE35 packet — from ingest pass-through
// or from internal/scte35ctrl's local insertion, indistinguishable except
// for pkt.SCTE35.Source. scte35Mode gates which Source is honored at all
// ("passthrough" only forwards SourceIngest, "insert" only SourceInserted,
// "hybrid" both, "disabled" neither) before passing it through to the TS
// muxer verbatim and running it through this stream's ad-break state
// machine to decide whether a playlist marker should be queued for the
// next flushed segment.
func (p *Packager) handleSCTE35(pkt *media.Packet) error {
	p.mu.Lock()
	if p.anchorWall.IsZero() {
		p.anchorDTS = pkt.DTS
		p.anchorWall = pkt.WallClock
		if p.anchorWall.IsZero() {
			p.anchorWall = time.Now()
		}
	}
	muxer := p.segMuxer
	p.mu.Unlock()

	if pkt.SCTE35 == nil {
		return nil
	}
	if p.metrics != nil {
		p.metrics.SCTE35EventsTotal.WithLabelValues(p.stream.Key.App, p.stream.Key.Name, pkt.SCTE35.CommandType.String()).Inc()
	}

	if !scte35.SourceAllowed(p.scte35Mode, pkt.SCTE35.Source) {
		return nil
	}

	if !p.useFMP4() && muxer != nil {
		if err := muxer.WriteSCTE35(pkt.Data); err != nil {
			p.log.Warn("scte35 mpegts passthrough failed", "err", err)
		} else if p.metrics != nil && pkt.SCTE35.Source == scte35.SourceIngest {
			p.metrics.SCTE35PassthroughTotal.WithLabelValues(p.stream.Key.App, p.stream.Key.Name).Inc()
		}
	}

	if p.useFMP4() || p.adMarkerFormat == "" || p.adMarkerFormat == "none" {
		return nil
	}

	actions := p.scteState.Apply(pkt.SCTE35)
	if p.metrics != nil {
		if dup := p.scteState.DuplicatesSuppressed(); dup > p.scteDuplicatesSeen {
			p.metrics.SCTE35DuplicatesTotal.WithLabelValues(p.stream.Key.App, p.stream.Key.Name).Add(float64(dup - p.scteDuplicatesSeen))
			p.scteDuplicatesSeen = dup
		}
	}
	if len(actions) == 0 {
		return nil
	}
	atWall := p.wallClockFor(pkt.DTS)

	p.mu.Lock()
	for _, action := range actions {
		p.applyActionLocked(action, atWall)
	}
	p.mu.Unlock()
	return nil
}

// wallClockFor maps a media-timeline DTS to a real wall-clock instant using
// the anchor captured on this Packager's first packet — used only for
// SCTE-35 markers (EXT-X-DATERANGE's START-DATE/END-DATE), deliberately
// independent of segStartWall (which stays time.Now()-at-segment-start, so
// non-SCTE-35 output is unaffected — see the Packager struct doc comment).
func (p *Packager) wallClockFor(dts int64) time.Time {
	p.mu.Lock()
	anchorDTS, anchorWall := p.anchorDTS, p.anchorWall
	p.mu.Unlock()
	if anchorWall.IsZero() {
		return time.Now()
	}
	return anchorWall.Add(time.Duration(dts-anchorDTS) * time.Second / 90000)
}

// applyActionLocked converts one state-machine Action into this Packager's
// pending* marker fields, per the configured ad_marker_format. Called with
// p.mu held.
func (p *Packager) applyActionLocked(action scte35.Action, atWall time.Time) {
	eventID := actionEventID(action)

	switch action.Type {
	case scte35.ActionCueOut:
		switch p.adMarkerFormat {
		case "cue":
			p.pendingCueOut = buildCueOut(action)
		case "splicepoint":
			p.pendingSplicepoint = buildSplicepoint(action.Event)
		case "daterange":
			var durMS int64
			var hasDur bool
			if action.Event != nil {
				switch {
				case action.Event.HasBreakDuration:
					durMS, hasDur = action.Event.BreakDurationMS, true
				case action.Event.HasSegmentationDuration:
					durMS, hasDur = action.Event.SegmentationDurationMS, true
				}
			}
			p.pendingDateRanges = append(p.pendingDateRanges, buildDateRangeOut(action.Event, eventID, atWall, durMS, hasDur))
		}
		p.activeBreakStartWall = atWall
		p.activeBreakEventID = eventID
		p.activeBreakSCTE35 = buildSplicepoint(action.Event)
		if p.metrics != nil {
			p.metrics.SCTE35OutTotal.WithLabelValues(p.stream.Key.App, p.stream.Key.Name).Inc()
		}
		p.log.Info("hls scte35 marker", "stream", p.stream.Key.Name, "format", p.adMarkerFormat, "event_id", eventID, "action", "OUT")

	case scte35.ActionCueIn:
		if eventID == 0 {
			eventID = p.activeBreakEventID
		}
		switch p.adMarkerFormat {
		case "cue":
			p.pendingCueIn = true
		case "splicepoint":
			if action.Event != nil {
				p.pendingSplicepoint = buildSplicepoint(action.Event)
			}
		case "daterange":
			startWall := p.activeBreakStartWall
			if startWall.IsZero() {
				startWall = atWall
			}
			p.pendingDateRanges = append(p.pendingDateRanges, buildDateRangeIn(action.Event, eventID, startWall, atWall))
		}
		p.activeBreakSCTE35 = ""
		if p.metrics != nil {
			p.metrics.SCTE35InTotal.WithLabelValues(p.stream.Key.App, p.stream.Key.Name).Inc()
		}
		p.log.Info("hls scte35 marker", "stream", p.stream.Key.Name, "format", p.adMarkerFormat, "event_id", eventID, "action", "IN")
	}
}

// actionEventID extracts the driving splice_event_id/segmentation_event_id
// from an Action's Event, or 0 if the action carries no Event at all (a
// ForceAutoReturn-triggered IN).
func actionEventID(action scte35.Action) uint32 {
	if action.Event == nil {
		return 0
	}
	if action.Event.HasEventID {
		return action.Event.EventID
	}
	return action.Event.SegmentationEventID
}

// pendingMarkers is what drainPendingMarkers hands to flushSegmentAt to
// attach to the Segment about to be written.
type pendingMarkers struct {
	cueOut      *CueOutMarker
	cueOutCont  *CueOutContMarker
	cueIn       bool
	dateRanges  []DateRange
	splicepoint string
}

// drainPendingMarkers returns and clears everything handleSCTE35 has queued
// since the last flush, and — for "cue" format — synthesizes a
// CUE-OUT-CONT for a segment cut mid-break with no fresh event on it.  It
// also checks whether an auto_return break's deadline has passed with no
// explicit IN ever arriving, forcing the NORMAL transition and queuing its
// own IN marker in that case (TS-container HLS only; the state transition
// itself is not container-specific, but marker generation is).
func (p *Packager) drainPendingMarkers() pendingMarkers {
	p.mu.Lock()
	defer p.mu.Unlock()

	m := pendingMarkers{
		cueOut:      p.pendingCueOut,
		cueIn:       p.pendingCueIn,
		dateRanges:  p.pendingDateRanges,
		splicepoint: p.pendingSplicepoint,
	}
	p.pendingCueOut = nil
	p.pendingCueIn = false
	p.pendingDateRanges = nil
	p.pendingSplicepoint = ""

	if deadline, ok := p.scteState.PendingAutoReturn(); ok && !time.Now().Before(deadline) {
		now := time.Now()
		for _, action := range p.scteState.ForceAutoReturn() {
			p.applyActionLocked(action, now)
		}
		// Re-collect: applyActionLocked above may have just populated
		// pending* fields for this same flush.
		if m.cueOut == nil {
			m.cueOut = p.pendingCueOut
		}
		m.cueIn = m.cueIn || p.pendingCueIn
		m.dateRanges = append(m.dateRanges, p.pendingDateRanges...)
		if m.splicepoint == "" {
			m.splicepoint = p.pendingSplicepoint
		}
		p.pendingCueOut = nil
		p.pendingCueIn = false
		p.pendingDateRanges = nil
		p.pendingSplicepoint = ""
	}

	if p.adMarkerFormat == "cue" && m.cueOut == nil && !m.cueIn {
		state, _, breakStart, expectedMS := p.scteState.Snapshot()
		m.cueOutCont = buildCueOutCont(state, breakStart, time.Now(), expectedMS, p.activeBreakSCTE35)
	}

	return m
}
