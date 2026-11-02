// Package webvtt renders WebVTT-in-ISOBMFF ('wvtt', ISO/IEC 14496-30 §7)
// samples — the non-DVB-profile caption format (see internal/ebuttd for the
// DVB-DASH/EBU-TT-D counterpart).
//
// Why this exists: live playback testing found EBU-TT-D/fragmented-TTML
// rendering unreliable across web DASH clients specifically for *live*
// streams (confirmed independently in both dash.js and Shaka Player — cues
// parse and buffer correctly, matching a long-standing, still-open class of
// upstream player issues, e.g. github.com/Dash-Industry-Forum/dash.js#640
// — while our own EBU-TT-D output was independently verified spec-correct
// at the box/sample/XML level). WebVTT is dash.js/browsers' most mature
// text-track path: it populates the native <video> TextTrack directly,
// letting the browser render captions itself with no custom TTML renderer
// in the loop. DVB-DASH mandates EBU-TT-D, so this package is only used
// when the DVB profile itself is off (see dash.Packager.SetDVBConfig).
package webvtt

import (
	"bytes"
	"encoding/binary"
	"sort"

	"github.com/5centscdn/origin-mediaserver/internal/captiondecode"
)

// Sample is one ISOBMFF sample for a wvtt track, DTS/Duration relative to
// the segment's own start (90kHz ticks — the same clock as the fMP4 track).
// Data is the fully-encoded vttc/vtte box payload, ready to use as an
// mux.Sample's Data.
type Sample struct {
	DTS      int64
	Duration int64
	Data     []byte
}

// RenderSamples converts cues overlapping [segStartPTS, segStartPTS+segDurationTicks)
// into a gapless sequence of WebVTT ISOBMFF samples spanning the whole
// segment.
//
// Unlike EBU-TT-D's one-XML-document-per-segment model (where each <p>
// carries its own begin/end), a WebVTT-in-ISOBMFF sample carries no timing
// of its own — the sample's *own* trun duration/CTO IS its cue's entire
// active window. So each cue becomes its own sample, with empty "vtte"
// filler samples covering silent gaps — required to keep the segment's
// total sample duration matching its declared span (the DASH SegmentTimeline
// entry other tracks in the same segment share).
func RenderSamples(cues []captiondecode.Cue, segStartPTS int64, segDurationTicks int64) []Sample {
	segEndPTS := segStartPTS + segDurationTicks

	type clipped struct {
		start, end int64
		text       string
	}
	var overlapping []clipped
	for _, c := range cues {
		start, end := c.StartPTS, c.EndPTS
		if end <= segStartPTS || start >= segEndPTS {
			continue // no overlap with this segment at all
		}
		if start < segStartPTS {
			start = segStartPTS
		}
		if end > segEndPTS {
			end = segEndPTS
		}
		overlapping = append(overlapping, clipped{start - segStartPTS, end - segStartPTS, c.Text})
	}
	sort.Slice(overlapping, func(i, j int) bool { return overlapping[i].start < overlapping[j].start })

	var samples []Sample
	cursor := int64(0)
	for _, c := range overlapping {
		start := c.start
		if start < cursor {
			// CEA-608/708 (Service 1, single active caption line) never
			// produces genuinely overlapping cues in practice — clip
			// defensively rather than emit a negative-duration filler.
			start = cursor
		}
		if start > cursor {
			samples = append(samples, Sample{DTS: cursor, Duration: start - cursor, Data: encodeEmpty()})
		}
		if c.end > start {
			samples = append(samples, Sample{DTS: start, Duration: c.end - start, Data: encodeCue(c.text)})
			cursor = c.end
		}
	}
	if cursor < segDurationTicks {
		samples = append(samples, Sample{DTS: cursor, Duration: segDurationTicks - cursor, Data: encodeEmpty()})
	}
	if len(samples) == 0 {
		// Always produce at least one sample per segment, same convention
		// ebuttd.Render follows for an always-parseable per-segment sample.
		samples = append(samples, Sample{DTS: 0, Duration: segDurationTicks, Data: encodeEmpty()})
	}
	return samples
}

// encodeCue builds one VTTCueBox ('vttc') wrapping a single VTTCuePayloadBox
// ('payl') — the minimal valid non-empty WebVTT ISOBMFF sample (ISO/IEC
// 14496-30 §7.3.1/7.3.5). No settings/identifier/timing sub-boxes: this
// project's captions carry no styling or per-cue metadata to express.
func encodeCue(text string) []byte {
	var payl bytes.Buffer
	writeBox(&payl, "payl", []byte(text))

	var vttc bytes.Buffer
	writeBox(&vttc, "vttc", payl.Bytes())
	return vttc.Bytes()
}

// encodeEmpty builds a VTTEmptyCueBox ('vtte') — the standard way to signal
// "no cue active" for a sample's whole duration (ISO/IEC 14496-30 §7.3.6).
func encodeEmpty() []byte {
	var vtte bytes.Buffer
	writeBox(&vtte, "vtte", nil)
	return vtte.Bytes()
}

// writeBox mirrors internal/mux's own raw box-writing convention —
// duplicated rather than exported/shared, consistent with this codebase's
// established per-package duplication for small leaf helpers (see e.g.
// internal/dash's own splitAnnexB).
func writeBox(w *bytes.Buffer, boxType string, body []byte) {
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(8+len(body)))
	w.Write(size[:])
	w.WriteString(boxType)
	w.Write(body)
}
