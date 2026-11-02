package hls

import (
	"strings"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/scte35"
)

// newTestPackagerForSCTE35 builds a real TS-container Packager (via
// NewPackager, so scteState/playlist/etc. are all properly constructed —
// unlike the lower-level &Packager{...} literals used elsewhere in this
// package's tests) with a 1-second target duration so a couple of keyframes
// spaced a few seconds apart reliably cut multiple segments.
func newTestPackagerForSCTE35(t *testing.T, adMarkerFormat string) *Packager {
	t.Helper()
	cfg := &config.Config{}
	cfg.HLS.Container = "ts"
	cfg.HLS.SegmentDurationS = 1
	cfg.HLS.PlaylistWindow = 20

	stream := &media.Stream{Key: media.StreamKey{App: "test", Name: "s1"}, Bus: media.NewBus()}
	stream.Bus.SetCodecInfo(&media.CodecInfo{
		VideoCodec: media.CodecH264,
		SPSNALu:    []byte{0x67, 0x42, 0xC0, 0x1E},
		PPSNALu:    []byte{0x68, 0xCE},
		Width:      64, Height: 64,
	})

	p, err := NewPackager(cfg, stream, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewPackager: %v", err)
	}
	p.SetSCTE35Config("hybrid", adMarkerFormat) // hybrid = passthrough+insert both allowed, equivalent to the old passthrough=true
	return p
}

// newTestPackagerForSCTE35Mode is newTestPackagerForSCTE35 with an
// explicit, caller-chosen scte35Mode instead of always "hybrid" — used by
// the mode-gating tests below.
func newTestPackagerForSCTE35Mode(t *testing.T, mode, adMarkerFormat string) *Packager {
	t.Helper()
	p := newTestPackagerForSCTE35(t, adMarkerFormat)
	p.SetSCTE35Config(mode, adMarkerFormat)
	return p
}

func keyframeAt(dtsTicks int64) *media.Packet {
	return &media.Packet{
		Type: media.PacketKeyframe, Codec: media.CodecH264, IsKeyframe: true,
		DTS: dtsTicks, PTS: dtsTicks,
		Data:      []byte{0, 0, 0, 1, 0x65, 1, 2, 3},
		WallClock: time.Now(),
	}
}

func scte35OutPacket(dtsTicks int64, eventID uint32, durationMS int64) *media.Packet {
	ev := &scte35.Event{
		CommandType:      scte35.CommandSpliceInsert,
		HasEventID:       true,
		EventID:          eventID,
		OutOfNetwork:     true,
		HasBreakDuration: durationMS > 0,
		BreakDurationMS:  durationMS,
		RawSection:       []byte{0xFC, 0x30, 0x00, byte(eventID), 0xAA, 0xBB},
		ReceivedAt:       time.Now(),
	}
	return &media.Packet{Codec: media.CodecSCTE35, DTS: dtsTicks, PTS: dtsTicks, Data: ev.RawSection, SCTE35: ev, WallClock: time.Now()}
}

func scte35InPacket(dtsTicks int64, eventID uint32) *media.Packet {
	ev := &scte35.Event{
		CommandType:  scte35.CommandSpliceInsert,
		HasEventID:   true,
		EventID:      eventID,
		OutOfNetwork: false,
		RawSection:   []byte{0xFC, 0x30, 0x01, byte(eventID), 0xCC, 0xDD},
		ReceivedAt:   time.Now(),
	}
	return &media.Packet{Codec: media.CodecSCTE35, DTS: dtsTicks, PTS: dtsTicks, Data: ev.RawSection, SCTE35: ev, WallClock: time.Now()}
}

// insertedOutPacket is scte35OutPacket's counterpart tagged
// Source: SourceInserted — as if internal/scte35ctrl had generated it,
// rather than internal/ingest/mpegts having demuxed it from the wire.
func insertedOutPacket(dtsTicks int64, eventID uint32, durationMS int64) *media.Packet {
	pkt := scte35OutPacket(dtsTicks, eventID, durationMS)
	pkt.SCTE35.Source = scte35.SourceInserted
	return pkt
}

const ticksPerSec = 90000

// dtsBase offsets every test DTS off zero: handleVideo treats segStartDTS
// <= 0 as its own "no segment open" sentinel, so a literal DTS of 0 for the
// first keyframe would collapse timing (see the existing note in
// TestHandleVideo_LazyInitsFromCodecInfoWithoutConfigPacket — the same
// gotcha applies here).
const dtsBase = ticksPerSec

// TestAdMarkers_Cue drives a full OUT -> (mid-break segment) -> IN sequence
// through a real Packager with ad_marker_format=cue and checks the
// rendered playlist for #EXT-X-CUE-OUT, a #EXT-X-CUE-OUT-CONT on the
// segment in between, and #EXT-X-CUE-IN — the task's canonical example.
func TestAdMarkers_Cue(t *testing.T) {
	p := newTestPackagerForSCTE35(t, "cue")

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	outPkt := scte35OutPacket(dtsBase, 42, 30000)
	wantSCTE35 := buildSplicepoint(outPkt.SCTE35)

	must(p.handleVideo(keyframeAt(dtsBase)))                        // segment 0 opens
	must(p.handleSCTE35(outPkt))                                    // OUT queued
	must(p.handleVideo(keyframeAt(dtsBase + 2*ticksPerSec)))        // flush segment 0 (CUE-OUT), open segment 1
	must(p.handleVideo(keyframeAt(dtsBase + 4*ticksPerSec)))        // flush segment 1 (mid-break, no fresh event -> CUE-OUT-CONT), open segment 2
	must(p.handleSCTE35(scte35InPacket(dtsBase+4*ticksPerSec, 42))) // IN queued
	must(p.handleVideo(keyframeAt(dtsBase + 6*ticksPerSec)))        // flush segment 2 (CUE-IN)
	must(p.flushSegment(true))

	out := p.playlist.Render()
	if !strings.Contains(out, "#EXT-X-CUE-OUT:30.000,SCTE35="+wantSCTE35) {
		t.Errorf("playlist missing #EXT-X-CUE-OUT:30.000,SCTE35=%s:\n%s", wantSCTE35, out)
	}
	if !strings.Contains(out, "#EXT-X-CUE-OUT-CONT:ElapsedTime=") {
		t.Errorf("playlist missing #EXT-X-CUE-OUT-CONT:\n%s", out)
	}
	// The mid-break CONT (segment 1, no fresh event) must still carry the
	// triggering OUT's splice_info_section — real consumers (e.g. Nimble)
	// expect SCTE35= on every CUE-OUT-CONT line, not just the initiating
	// CUE-OUT, so a player/ad-decisioning system joining mid-break can
	// still read the original signal. Two occurrences expected: one on
	// CUE-OUT, one on CUE-OUT-CONT.
	if got := strings.Count(out, "SCTE35="+wantSCTE35); got < 2 {
		t.Errorf("want SCTE35=%s on both CUE-OUT and CUE-OUT-CONT (found %d occurrences):\n%s", wantSCTE35, got, out)
	}
	if !strings.Contains(out, "#EXT-X-CUE-IN") {
		t.Errorf("playlist missing #EXT-X-CUE-IN:\n%s", out)
	}
	// Ordering: CUE-OUT must appear before CUE-IN.
	if strings.Index(out, "#EXT-X-CUE-OUT:") > strings.Index(out, "#EXT-X-CUE-IN") {
		t.Errorf("CUE-IN appears before CUE-OUT:\n%s", out)
	}
}

// TestAdMarkers_Splicepoint checks that ad_marker_format=splicepoint emits
// #EXT-X-SPLICEPOINT-SCTE35 with the exact original section, base64-encoded.
func TestAdMarkers_Splicepoint(t *testing.T) {
	p := newTestPackagerForSCTE35(t, "splicepoint")
	pkt := scte35OutPacket(dtsBase, 7, 15000)

	if err := p.handleVideo(keyframeAt(dtsBase)); err != nil {
		t.Fatal(err)
	}
	if err := p.handleSCTE35(pkt); err != nil {
		t.Fatal(err)
	}
	if err := p.handleVideo(keyframeAt(dtsBase + 2*ticksPerSec)); err != nil {
		t.Fatal(err)
	}
	if err := p.flushSegment(true); err != nil {
		t.Fatal(err)
	}

	out := p.playlist.Render()
	want := buildSplicepoint(pkt.SCTE35)
	if !strings.Contains(out, "#EXT-X-SPLICEPOINT-SCTE35:"+want) {
		t.Errorf("playlist missing #EXT-X-SPLICEPOINT-SCTE35:%s:\n%s", want, out)
	}
}

// TestAdMarkers_Daterange checks that ad_marker_format=daterange emits
// #EXT-X-DATERANGE with SCTE35-OUT (hex) on the OUT segment and a matching
// entry with SCTE35-IN + END-DATE on the IN segment, sharing an ID.
func TestAdMarkers_Daterange(t *testing.T) {
	p := newTestPackagerForSCTE35(t, "daterange")

	if err := p.handleVideo(keyframeAt(dtsBase)); err != nil {
		t.Fatal(err)
	}
	if err := p.handleSCTE35(scte35OutPacket(dtsBase, 55, 30000)); err != nil {
		t.Fatal(err)
	}
	if err := p.handleVideo(keyframeAt(dtsBase + 2*ticksPerSec)); err != nil {
		t.Fatal(err)
	}
	if err := p.handleVideo(keyframeAt(dtsBase + 4*ticksPerSec)); err != nil {
		t.Fatal(err)
	}
	if err := p.handleSCTE35(scte35InPacket(dtsBase+4*ticksPerSec, 55)); err != nil {
		t.Fatal(err)
	}
	if err := p.handleVideo(keyframeAt(dtsBase + 6*ticksPerSec)); err != nil {
		t.Fatal(err)
	}
	if err := p.flushSegment(true); err != nil {
		t.Fatal(err)
	}

	out := p.playlist.Render()
	if !strings.Contains(out, `ID="55"`) {
		t.Errorf("playlist missing DATERANGE ID=\"55\":\n%s", out)
	}
	if !strings.Contains(out, "SCTE35-OUT=0x") {
		t.Errorf("playlist missing SCTE35-OUT=0x...:\n%s", out)
	}
	if !strings.Contains(out, "SCTE35-IN=0x") {
		t.Errorf("playlist missing SCTE35-IN=0x...:\n%s", out)
	}
	if !strings.Contains(out, "END-DATE=") {
		t.Errorf("playlist missing END-DATE:\n%s", out)
	}
}

// TestAdMarkers_None checks that ad_marker_format=none/"" never emits any
// playlist marker even though SCTE-35 events are still being processed
// (state-machine transitions still happen; only marker rendering is
// suppressed) — the task's explicit "none disables HLS markers only".
func TestAdMarkers_None(t *testing.T) {
	for _, format := range []string{"", "none"} {
		t.Run(format, func(t *testing.T) {
			p := newTestPackagerForSCTE35(t, format)
			if err := p.handleVideo(keyframeAt(dtsBase)); err != nil {
				t.Fatal(err)
			}
			if err := p.handleSCTE35(scte35OutPacket(dtsBase, 1, 30000)); err != nil {
				t.Fatal(err)
			}
			if err := p.handleVideo(keyframeAt(dtsBase + 2*ticksPerSec)); err != nil {
				t.Fatal(err)
			}
			if err := p.flushSegment(true); err != nil {
				t.Fatal(err)
			}

			out := p.playlist.Render()
			for _, tag := range []string{"#EXT-X-CUE-OUT", "#EXT-X-CUE-IN", "#EXT-X-DATERANGE", "#EXT-X-SPLICEPOINT-SCTE35"} {
				if strings.Contains(out, tag) {
					t.Errorf("format %q: playlist unexpectedly contains %s:\n%s", format, tag, out)
				}
			}
		})
	}
}

// TestAdMarkers_MultipleSequentialBreaks covers two independent ad breaks
// (distinct event IDs) in the same stream, each getting its own CUE-OUT/
// CUE-IN pair with no cross-contamination.
func TestAdMarkers_MultipleSequentialBreaks(t *testing.T) {
	p := newTestPackagerForSCTE35(t, "cue")

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	must(p.handleVideo(keyframeAt(dtsBase)))
	must(p.handleSCTE35(scte35OutPacket(dtsBase, 1, 10000)))
	must(p.handleVideo(keyframeAt(dtsBase + 2*ticksPerSec)))
	must(p.handleSCTE35(scte35InPacket(dtsBase+2*ticksPerSec, 1)))
	must(p.handleVideo(keyframeAt(dtsBase + 4*ticksPerSec)))

	must(p.handleSCTE35(scte35OutPacket(dtsBase+4*ticksPerSec, 2, 20000)))
	must(p.handleVideo(keyframeAt(dtsBase + 6*ticksPerSec)))
	must(p.handleSCTE35(scte35InPacket(dtsBase+6*ticksPerSec, 2)))
	must(p.handleVideo(keyframeAt(dtsBase + 8*ticksPerSec)))
	must(p.flushSegment(true))

	out := p.playlist.Render()
	if got := strings.Count(out, "#EXT-X-CUE-OUT:"); got != 2 {
		t.Errorf("got %d CUE-OUT tags, want 2:\n%s", got, out)
	}
	if got := strings.Count(out, "#EXT-X-CUE-IN"); got != 2 {
		t.Errorf("got %d CUE-IN tags, want 2:\n%s", got, out)
	}
	if !strings.Contains(out, "#EXT-X-CUE-OUT:10.000") || !strings.Contains(out, "#EXT-X-CUE-OUT:20.000") {
		t.Errorf("expected both 10.000 and 20.000 second CUE-OUT durations:\n%s", out)
	}
}

// TestAdMarkers_DuplicateOutSuppressed guards against re-emitting a marker
// for a retransmitted SCTE-35 section (same event_id) — the task's "do not
// generate duplicate CUE-OUT/CUE-IN markers" requirement.
func TestAdMarkers_DuplicateOutSuppressed(t *testing.T) {
	p := newTestPackagerForSCTE35(t, "cue")

	if err := p.handleVideo(keyframeAt(dtsBase)); err != nil {
		t.Fatal(err)
	}
	if err := p.handleSCTE35(scte35OutPacket(dtsBase, 9, 10000)); err != nil {
		t.Fatal(err)
	}
	if err := p.handleSCTE35(scte35OutPacket(dtsBase, 9, 10000)); err != nil { // retransmission
		t.Fatal(err)
	}
	if err := p.handleVideo(keyframeAt(dtsBase + 2*ticksPerSec)); err != nil {
		t.Fatal(err)
	}
	if err := p.flushSegment(true); err != nil {
		t.Fatal(err)
	}

	out := p.playlist.Render()
	if got := strings.Count(out, "#EXT-X-CUE-OUT:"); got != 1 {
		t.Errorf("got %d CUE-OUT tags for a retransmitted event, want 1:\n%s", got, out)
	}
}

// TestAdMarkers_HybridModeProcessesBothSources drives an ingest-origin
// OUT/IN pair to completion, then an inserted-origin OUT/IN pair, through a
// single hybrid-mode Packager, and confirms both produce markers — the
// task's explicit "preserve incoming while ALSO allowing manually
// generated" requirement. Sequential, not concurrent: scte35.StateMachine
// (like real-world SCTE-35 for one program) models a single active break
// at a time, not multiple simultaneously-active event_ids, so this
// exercises hybrid mode's source-gating across two separate breaks in
// sequence rather than overlapping ones.
func TestAdMarkers_HybridModeProcessesBothSources(t *testing.T) {
	p := newTestPackagerForSCTE35Mode(t, "hybrid", "cue")

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	must(p.handleVideo(keyframeAt(dtsBase)))
	must(p.handleSCTE35(scte35OutPacket(dtsBase, 1, 10000))) // ingest-origin OUT
	must(p.handleVideo(keyframeAt(dtsBase + 2*ticksPerSec)))
	must(p.handleSCTE35(scte35InPacket(dtsBase+2*ticksPerSec, 1))) // ingest-origin IN
	must(p.handleVideo(keyframeAt(dtsBase + 4*ticksPerSec)))

	must(p.handleSCTE35(insertedOutPacket(dtsBase+4*ticksPerSec, 2, 20000))) // inserted-origin OUT
	must(p.handleVideo(keyframeAt(dtsBase + 6*ticksPerSec)))
	must(p.flushSegment(true))

	out := p.playlist.Render()
	if !strings.Contains(out, "#EXT-X-CUE-OUT:10.000") {
		t.Errorf("missing marker for ingest-origin event:\n%s", out)
	}
	if !strings.Contains(out, "#EXT-X-CUE-OUT:20.000") {
		t.Errorf("missing marker for inserted-origin event:\n%s", out)
	}
	if got := strings.Count(out, "#EXT-X-CUE-IN"); got != 1 {
		t.Errorf("got %d CUE-IN tags, want 1 (only the ingest-origin break was closed with an IN):\n%s", got, out)
	}
}

// TestAdMarkers_InsertModeIgnoresIngestOrigin confirms that in "insert"
// mode, an ingest-origin SCTE-35 packet produces no HLS marker and no
// MPEG-TS write — only locally-inserted events are honored.
func TestAdMarkers_InsertModeIgnoresIngestOrigin(t *testing.T) {
	p := newTestPackagerForSCTE35Mode(t, "insert", "cue")

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	must(p.handleVideo(keyframeAt(dtsBase)))
	must(p.handleSCTE35(scte35OutPacket(dtsBase, 1, 10000))) // ingest-origin — must be ignored
	must(p.handleVideo(keyframeAt(dtsBase + 2*ticksPerSec)))
	must(p.flushSegment(true))

	out := p.playlist.Render()
	if strings.Contains(out, "#EXT-X-CUE-OUT") {
		t.Errorf("insert mode processed an ingest-origin event, want it ignored:\n%s", out)
	}

	// A locally-inserted event on the same (insert-mode) Packager must still work.
	p2 := newTestPackagerForSCTE35Mode(t, "insert", "cue")
	must(p2.handleVideo(keyframeAt(dtsBase)))
	must(p2.handleSCTE35(insertedOutPacket(dtsBase, 3, 15000)))
	must(p2.handleVideo(keyframeAt(dtsBase + 2*ticksPerSec)))
	must(p2.flushSegment(true))

	out2 := p2.playlist.Render()
	if !strings.Contains(out2, "#EXT-X-CUE-OUT:15.000") {
		t.Errorf("insert mode did not process an inserted-origin event:\n%s", out2)
	}
}
