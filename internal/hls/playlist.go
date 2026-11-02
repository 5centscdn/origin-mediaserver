package hls

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Part is one LL-HLS partial segment within an in-progress or recently
// finalized Segment.
type Part struct {
	URI         string
	DurationS   float64
	Independent bool
}

// Segment is one entry in an HLS media playlist.
type Segment struct {
	Seq       int
	URI       string // filename relative to playlist directory
	DurationS float64
	WallTime  time.Time // wall-clock start time; emits #EXT-X-PROGRAM-DATE-TIME when set
	IsDiscont bool      // true → emit #EXT-X-DISCONTINUITY before this segment
	Parts     []Part    // LL-HLS only: parts belonging to this segment, kept for the hold-back window

	// SCTE-35 ad markers attached to this segment (see
	// internal/hls/admarkers.go). All nil/zero unless the stream's
	// ad_marker_format resolved to something other than "none" and an
	// SCTE-35 action landed on this segment — set by the packager exactly
	// like WallTime/IsDiscont above, before the Segment is handed to
	// AddSegment; Playlist itself just renders whatever's here.
	CueOut      *CueOutMarker     // hls_ad_marker_format=cue: an ad break starts at this segment
	CueOutCont  *CueOutContMarker // hls_ad_marker_format=cue: this segment is mid-break, no fresh event
	CueIn       bool              // hls_ad_marker_format=cue: the ad break ends at this segment
	DateRanges  []DateRange       // hls_ad_marker_format=daterange
	Splicepoint string            // hls_ad_marker_format=splicepoint: base64 splice_info_section, empty = none
}

// CueOutMarker renders as #EXT-X-CUE-OUT:<DurationS> — DurationS is 0 when
// the triggering SCTE-35 event carried no known break_duration/
// segmentation_duration (the tag is still emitted per the HLS convention,
// just with a 0 duration players can't act on until CUE-IN arrives).
// SCTE35, when non-empty, appends a ",SCTE35=<base64>" attribute carrying
// the raw splice_info_section — the same convention Nimble and other real
// origins use, and matches this format's Splicepoint field's encoding.
type CueOutMarker struct {
	DurationS float64
	SCTE35    string
}

// CueOutContMarker renders as
// #EXT-X-CUE-OUT-CONT:ElapsedTime=<ElapsedS>,Duration=<DurationS>[,SCTE35=<base64>]
// — emitted on every segment cut while an ad break is in progress but no
// fresh SCTE-35 event landed on this particular segment, so players
// joining mid-break still know where they are in it (and, with SCTE35 set,
// can still read the original signal without having joined at the OUT).
type CueOutContMarker struct {
	ElapsedS  float64
	DurationS float64
	SCTE35    string
}

// DateRange renders as #EXT-X-DATERANGE. Fields left empty/nil are omitted
// from the rendered attribute list — see writeDateRange.
type DateRange struct {
	ID               string
	Class            string
	StartDate        string // RFC3339 with milliseconds, required
	EndDate          string // set only on the IN entry
	DurationS        *float64
	PlannedDurationS *float64
	SCTE35Out        string // "0x"-prefixed hex of the original splice_info_section
	SCTE35In         string
	SCTE35Cmd        string
}

// Playlist manages the live sliding-window M3U8 media playlist.
// It is safe for concurrent AddSegment and Render calls.
type Playlist struct {
	mu         sync.RWMutex
	outDir     string
	window     int // max segments in live playlist
	targetDur  int // EXT-X-TARGETDURATION
	segments   []Segment
	mediaSeq   int     // EXT-X-MEDIA-SEQUENCE
	maxSeen    float64 // highest segment duration seen (for TARGETDURATION)
	initSegURI string  // fMP4/CMAF only: emits #EXT-X-MAP when set

	// LL-HLS state — only populated when partsEnabled (see EnableParts).
	partsEnabled  bool
	partTargetS   float64
	partHoldBackS float64
	currentSegSeq int    // seq of the in-progress segment currentParts belongs to
	currentParts  []Part // parts of the in-progress (not yet finalized) segment

	// preloadHintURI is the next LL-HLS part's predicted filename — set by
	// the packager the moment it starts accumulating a new part, i.e.
	// before that part exists on disk, so clients can start requesting it
	// early and have the request held open by the same blocking-reload
	// mechanism _HLS_part uses. Empty until the stream actually starts
	// cutting parts.
	preloadHintURI string

	// updateCh is closed and replaced on every AddSegment/AddPart so
	// WaitForUpdate can select on it alongside a context deadline without
	// the goroutine-leak risk of a context-cancelable sync.Cond.Wait.
	updateCh chan struct{}
}

// NewPlaylist creates a Playlist.  outDir is the directory where index.m3u8 is written.
func NewPlaylist(outDir string, window, targetDurS int) *Playlist {
	return &Playlist{
		outDir:    outDir,
		window:    window,
		targetDur: targetDurS,
		updateCh:  make(chan struct{}),
	}
}

// EnableParts turns on LL-HLS part rendering (#EXT-X-PART-INF,
// #EXT-X-SERVER-CONTROL, #EXT-X-PART). Call once, before the first AddPart —
// a no-op for streams not using LL-HLS.
func (pl *Playlist) EnableParts(partTargetS, holdBackS float64) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	pl.partsEnabled = true
	pl.partTargetS = partTargetS
	pl.partHoldBackS = holdBackS
}

// SetInitSegment records the fMP4/CMAF init segment's filename so render()
// emits #EXT-X-MAP for it. Call once, before the first AddSegment — a no-op
// for the MPEG-TS container, which has no init segment.
func (pl *Playlist) SetInitSegment(uri string) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	pl.initSegURI = uri
	_ = pl.write()
}

// SetPreloadHint records the predicted filename of the next LL-HLS part —
// call the moment a new part starts accumulating, before it's written to
// disk or added via AddPart. A no-op (silently ignored) once that part is
// later added; callers always overwrite with the next prediction anyway.
func (pl *Playlist) SetPreloadHint(uri string) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	pl.preloadHintURI = uri
	_ = pl.write()
}

// AddPart appends a part to the in-progress segment identified by seq,
// starting a fresh part list if seq has advanced (i.e. this is the first
// part of a new segment).
func (pl *Playlist) AddPart(seq int, part Part) {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	if pl.currentSegSeq != seq {
		pl.currentSegSeq = seq
		pl.currentParts = nil
	}
	pl.currentParts = append(pl.currentParts, part)

	pl.notifyLocked()
	_ = pl.write()
}

// AddSegment appends a segment to the playlist and rewrites index.m3u8.
func (pl *Playlist) AddSegment(seg Segment) {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	if pl.currentSegSeq == seg.Seq {
		seg.Parts = pl.currentParts
		pl.currentParts = nil
		pl.currentSegSeq = 0
	}

	if seg.DurationS > pl.maxSeen {
		pl.maxSeen = seg.DurationS
	}

	pl.segments = append(pl.segments, seg)

	// Evict oldest segments to maintain window size.
	for len(pl.segments) > pl.window {
		pl.segments = pl.segments[1:]
		pl.mediaSeq++
	}

	pl.notifyLocked()
	_ = pl.write() // best-effort; errors logged by packager
}

// notifyLocked wakes every goroutine currently blocked in WaitForUpdate.
// Must be called with pl.mu held.
func (pl *Playlist) notifyLocked() {
	close(pl.updateCh)
	pl.updateCh = make(chan struct{})
}

// WaitForUpdate blocks until the playlist satisfies (msn, part) — see
// satisfiesLocked — or ctx is done, whichever happens first. part < 0 means
// "wait for segment msn to exist at all," ignoring any particular part.
func (pl *Playlist) WaitForUpdate(ctx context.Context, msn, part int) {
	for {
		pl.mu.RLock()
		if pl.satisfiesLocked(msn, part) {
			pl.mu.RUnlock()
			return
		}
		ch := pl.updateCh
		pl.mu.RUnlock()

		select {
		case <-ch:
		case <-ctx.Done():
			return
		}
	}
}

// satisfiesLocked reports whether the playlist already has what a
// blocking-reload request for (msn, part) is waiting on. Caller must hold
// pl.mu (for reading or writing).
func (pl *Playlist) satisfiesLocked(msn, part int) bool {
	lastSeq := pl.mediaSeq + len(pl.segments) - 1
	if lastSeq >= msn {
		return true // segment msn (and everything in it) is already finalized
	}
	if part < 0 {
		return false // waiting on the segment itself, which isn't finalized yet
	}
	return pl.currentSegSeq == msn && len(pl.currentParts) > part
}

// Render returns the current M3U8 playlist as a string.
func (pl *Playlist) Render() string {
	pl.mu.RLock()
	defer pl.mu.RUnlock()
	return pl.render(false)
}

// RenderDelta returns a Playlist Delta Update (RFC 8216bis "Delta
// Updates"): everything older than canSkipUntilS (see render) is replaced
// by a single #EXT-X-SKIP tag instead of being listed in full. For a
// client that already has the unchanged older segments cached from a
// previous reload, this can shrink a long-running stream's playlist
// response by an order of magnitude. Served only on explicit request
// (?_HLS_skip=YES) — see handleChunks/handleVariantChunks.
func (pl *Playlist) RenderDelta() string {
	pl.mu.RLock()
	defer pl.mu.RUnlock()
	return pl.render(true)
}

// canSkipUntilS is how far back from the live edge a Delta Update
// guarantees it will still list segments in full — advertised via
// EXT-X-SERVER-CONTROL's CAN-SKIP-UNTIL. RFC 8216bis requires at least six
// times the target duration.
func (pl *Playlist) canSkipUntilS(targetDur int) float64 {
	return 6 * float64(targetDur)
}

func (pl *Playlist) render(skipDelta bool) string {
	targetDur := pl.targetDur
	if int(pl.maxSeen)+1 > targetDur {
		targetDur = int(pl.maxSeen) + 1
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:7\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", targetDur)
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", pl.mediaSeq)
	if pl.partsEnabled {
		fmt.Fprintf(&b, "#EXT-X-PART-INF:PART-TARGET=%.3f\n", pl.partTargetS)
		fmt.Fprintf(&b, "#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES,PART-HOLD-BACK=%.3f,CAN-SKIP-UNTIL=%.3f\n",
			pl.partHoldBackS, pl.canSkipUntilS(targetDur))
	}
	if pl.initSegURI != "" {
		fmt.Fprintf(&b, "#EXT-X-MAP:URI=%q\n", pl.initSegURI)
	}

	keepFrom := 0
	if skipDelta && pl.partsEnabled {
		keepFrom = pl.skipBoundaryLocked(targetDur)
		if keepFrom > 0 {
			fmt.Fprintf(&b, "#EXT-X-SKIP:SKIPPED-SEGMENTS=%d\n", keepFrom)
		}
	}

	for i := keepFrom; i < len(pl.segments); i++ {
		seg := pl.segments[i]
		if seg.IsDiscont {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if !seg.WallTime.IsZero() {
			fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n",
				seg.WallTime.UTC().Format("2006-01-02T15:04:05.000Z"))
		}
		for _, dr := range seg.DateRanges {
			writeDateRange(&b, dr)
		}
		if seg.Splicepoint != "" {
			fmt.Fprintf(&b, "#EXT-X-SPLICEPOINT-SCTE35:%s\n", seg.Splicepoint)
		}
		if seg.CueOut != nil {
			fmt.Fprintf(&b, "#EXT-X-CUE-OUT:%.3f", seg.CueOut.DurationS)
			if seg.CueOut.SCTE35 != "" {
				fmt.Fprintf(&b, ",SCTE35=%s", seg.CueOut.SCTE35)
			}
			b.WriteString("\n")
		}
		if seg.CueOutCont != nil {
			fmt.Fprintf(&b, "#EXT-X-CUE-OUT-CONT:ElapsedTime=%.3f,Duration=%.3f", seg.CueOutCont.ElapsedS, seg.CueOutCont.DurationS)
			if seg.CueOutCont.SCTE35 != "" {
				fmt.Fprintf(&b, ",SCTE35=%s", seg.CueOutCont.SCTE35)
			}
			b.WriteString("\n")
		}
		if seg.CueIn {
			b.WriteString("#EXT-X-CUE-IN\n")
		}
		// Only the most recent couple of segments need their parts listed —
		// older ones are well past the hold-back window LL-HLS clients care about.
		if pl.partsEnabled && i >= len(pl.segments)-2 {
			for _, part := range seg.Parts {
				writePart(&b, part)
			}
		}
		fmt.Fprintf(&b, "#EXTINF:%.6f,\n%s\n", seg.DurationS, seg.URI)
	}

	if pl.partsEnabled {
		for _, part := range pl.currentParts {
			writePart(&b, part)
		}
		if pl.preloadHintURI != "" {
			fmt.Fprintf(&b, "#EXT-X-PRELOAD-HINT:TYPE=PART,URI=%q\n", pl.preloadHintURI)
		}
	}

	return b.String()
}

// skipBoundaryLocked returns the index of the first segment to KEEP in a
// Delta Update: segments before it are old enough (their cumulative
// duration, counting back from the live edge, exceeds canSkipUntilS) to
// replace with a single EXT-X-SKIP tag instead of listing in full. Returns
// 0 (skip nothing) if the whole window doesn't even cover canSkipUntilS —
// a Delta Update is still valid in that case, just a no-op. Caller must
// hold pl.mu.
func (pl *Playlist) skipBoundaryLocked(targetDur int) int {
	minKeepS := pl.canSkipUntilS(targetDur)
	var kept float64
	for i := len(pl.segments) - 1; i >= 0; i-- {
		kept += pl.segments[i].DurationS
		if kept >= minKeepS {
			return i
		}
	}
	return 0
}

// writeDateRange renders one #EXT-X-DATERANGE tag. Per the RFC 8216bis
// EXT-X-DATERANGE grammar, attributes are double-quoted strings except
// DURATION/PLANNED-DURATION (decimal-floating-point, unquoted) and the
// SCTE35-* attributes (unquoted hexadecimal-sequence, "0x..." per the
// AWS Elemental convention this codebase follows for daterange markers).
func writeDateRange(b *strings.Builder, dr DateRange) {
	fmt.Fprintf(b, "#EXT-X-DATERANGE:ID=%q,START-DATE=%q", dr.ID, dr.StartDate)
	if dr.Class != "" {
		fmt.Fprintf(b, ",CLASS=%q", dr.Class)
	}
	if dr.EndDate != "" {
		fmt.Fprintf(b, ",END-DATE=%q", dr.EndDate)
	}
	if dr.DurationS != nil {
		fmt.Fprintf(b, ",DURATION=%.3f", *dr.DurationS)
	}
	if dr.PlannedDurationS != nil {
		fmt.Fprintf(b, ",PLANNED-DURATION=%.3f", *dr.PlannedDurationS)
	}
	if dr.SCTE35Out != "" {
		fmt.Fprintf(b, ",SCTE35-OUT=%s", dr.SCTE35Out)
	}
	if dr.SCTE35In != "" {
		fmt.Fprintf(b, ",SCTE35-IN=%s", dr.SCTE35In)
	}
	if dr.SCTE35Cmd != "" {
		fmt.Fprintf(b, ",SCTE35-CMD=%s", dr.SCTE35Cmd)
	}
	b.WriteString("\n")
}

func writePart(b *strings.Builder, part Part) {
	attrs := fmt.Sprintf("DURATION=%.3f,URI=%q", part.DurationS, part.URI)
	if part.Independent {
		attrs += ",INDEPENDENT=YES"
	}
	fmt.Fprintf(b, "#EXT-X-PART:%s\n", attrs)
}

func (pl *Playlist) write() error {
	content := pl.render(false)
	tmpPath := filepath.Join(pl.outDir, ".index.m3u8.tmp")
	finalPath := filepath.Join(pl.outDir, "index.m3u8")

	if err := os.WriteFile(tmpPath, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, finalPath)
}

// ─── Master Playlist ──────────────────────────────────────────────────────────

// Variant describes one quality tier for the master playlist.
type Variant struct {
	Bandwidth  int
	Resolution string // e.g. "1280x720", empty if unknown
	Codecs     string // e.g. "avc1.4d401f,mp4a.40.2"
	FrameRate  float64
	URI        string // relative path to variant playlist, e.g. "../camera_3/index.m3u8"
	Label      string
}

// RenderMasterPlaylist generates an HLS master playlist (#EXTM3U with
// #EXT-X-STREAM-INF entries) for a set of ABR variants.
// groupDir is the directory where master.m3u8 will be written.
func RenderMasterPlaylist(groupDir string, variants []Variant) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:6\n\n")

	for _, v := range variants {
		attrs := fmt.Sprintf("BANDWIDTH=%d", v.Bandwidth)
		if v.Resolution != "" {
			attrs += fmt.Sprintf(",RESOLUTION=%s", v.Resolution)
		}
		if v.Codecs != "" {
			attrs += fmt.Sprintf(",CODECS=%q", v.Codecs)
		}
		if v.FrameRate > 0 {
			attrs += fmt.Sprintf(",FRAME-RATE=%.3f", v.FrameRate)
		}
		attrs += fmt.Sprintf(",NAME=%q", v.Label)

		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:%s\n%s\n\n", attrs, v.URI)
	}

	return b.String()
}

// WriteMasterPlaylist writes master.m3u8 to groupDir.
func WriteMasterPlaylist(groupDir string, variants []Variant) error {
	content := RenderMasterPlaylist(groupDir, variants)
	tmpPath := filepath.Join(groupDir, ".master.m3u8.tmp")
	finalPath := filepath.Join(groupDir, "master.m3u8")
	if err := os.WriteFile(tmpPath, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, finalPath)
}
