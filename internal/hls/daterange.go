package hls

import (
	"encoding/hex"
	"fmt"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/scte35"
)

// dateRangeTimeFormat is EXT-X-DATERANGE's required START-DATE/END-DATE
// format: an ISO-8601 date-time (RFC 8216bis references RFC 3339).
const dateRangeTimeFormat = "2006-01-02T15:04:05.000Z"

// buildDateRangeOut builds the #EXT-X-DATERANGE entry for an ad break's
// start, anchored to atWall — the SCTE-35 event's actual PTS mapped to
// wall-clock time via Packager.wallClockFor, never time.Now(), so this
// stays accurate to the real splice point even though the segment it's
// attached to may start slightly earlier or later (see admarkers.go).
func buildDateRangeOut(ev *scte35.Event, eventID uint32, atWall time.Time, durationMS int64, hasDuration bool) DateRange {
	dr := DateRange{
		ID:        fmt.Sprintf("%d", eventID),
		StartDate: atWall.UTC().Format(dateRangeTimeFormat),
		SCTE35Out: scte35HexPayload(ev),
	}
	if hasDuration {
		d := float64(durationMS) / 1000.0
		dr.PlannedDurationS = &d
	}
	return dr
}

// buildDateRangeIn builds the matching #EXT-X-DATERANGE entry for an ad
// break's end. Per RFC 8216bis, START-DATE is a required attribute on every
// DATERANGE tag instance, so this carries the same START-DATE as the OUT
// entry (startWall) alongside its own END-DATE/DURATION/SCTE35-IN — a
// client matches the two by ID.
func buildDateRangeIn(ev *scte35.Event, eventID uint32, startWall, endWall time.Time) DateRange {
	durationS := endWall.Sub(startWall).Seconds()
	if durationS < 0 {
		durationS = 0
	}
	return DateRange{
		ID:        fmt.Sprintf("%d", eventID),
		StartDate: startWall.UTC().Format(dateRangeTimeFormat),
		EndDate:   endWall.UTC().Format(dateRangeTimeFormat),
		DurationS: &durationS,
		SCTE35In:  scte35HexPayload(ev),
	}
}

// scte35HexPayload renders ev's raw section as the "0x"-prefixed
// hexadecimal-sequence EXT-X-DATERANGE's SCTE35-* attributes use — distinct
// from #EXT-X-SPLICEPOINT-SCTE35's base64 encoding (see splicepoint.go);
// this is the convention verified against real-world AWS Elemental output.
func scte35HexPayload(ev *scte35.Event) string {
	if ev == nil || len(ev.RawSection) == 0 {
		return ""
	}
	return "0x" + hex.EncodeToString(ev.RawSection)
}
