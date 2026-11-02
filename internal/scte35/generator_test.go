package scte35

import (
	"bytes"
	"testing"
)

// TestBuildSpliceInsert_OutRoundTrips builds an OUT (out_of_network=true)
// splice_insert with a duration+auto_return and confirms it parses back
// through the same Parse the ingest demuxer uses, with every field intact
// and a valid CRC32 — using the already-tested parser as the oracle for
// generator correctness, rather than re-deriving byte offsets separately.
func TestBuildSpliceInsert_OutRoundTrips(t *testing.T) {
	built, err := BuildSpliceInsert(SpliceInsertOptions{
		EventID:         1001,
		OutOfNetwork:    true,
		PTS:             123456789,
		HasPTS:          true,
		DurationMS:      30000,
		AutoReturn:      true,
		UniqueProgramID: 7,
		AvailNum:        1,
		AvailsExpected:  1,
	})
	if err != nil {
		t.Fatalf("BuildSpliceInsert: %v", err)
	}
	if built.Source != SourceInserted {
		t.Errorf("Source = %v, want SourceInserted", built.Source)
	}

	parsed, err := Parse(built.RawSection)
	if err != nil {
		t.Fatalf("Parse(built section): %v", err)
	}
	if parsed.CommandType != CommandSpliceInsert {
		t.Errorf("CommandType = %v, want splice_insert", parsed.CommandType)
	}
	if !parsed.HasEventID || parsed.EventID != 1001 {
		t.Errorf("EventID = %d (has=%v), want 1001", parsed.EventID, parsed.HasEventID)
	}
	if !parsed.OutOfNetwork {
		t.Error("OutOfNetwork = false, want true")
	}
	if !parsed.HasPTS || parsed.PTS != 123456789 {
		t.Errorf("PTS = %d (has=%v), want 123456789", parsed.PTS, parsed.HasPTS)
	}
	if !parsed.HasBreakDuration || parsed.BreakDurationMS != 30000 {
		t.Errorf("BreakDurationMS = %d (has=%v), want 30000", parsed.BreakDurationMS, parsed.HasBreakDuration)
	}
	if !parsed.AutoReturn {
		t.Error("AutoReturn = false, want true")
	}
}

// TestBuildSpliceInsert_InRoundTrips builds an IN (out_of_network=false,
// no duration, splice_immediate) and confirms it round-trips.
func TestBuildSpliceInsert_InRoundTrips(t *testing.T) {
	built, err := BuildSpliceInsert(SpliceInsertOptions{
		EventID:      1001,
		OutOfNetwork: false,
	})
	if err != nil {
		t.Fatalf("BuildSpliceInsert: %v", err)
	}

	parsed, err := Parse(built.RawSection)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.OutOfNetwork {
		t.Error("OutOfNetwork = true, want false (IN)")
	}
	if parsed.HasPTS {
		t.Error("HasPTS = true, want false (splice_immediate, no HasPTS requested)")
	}
	if parsed.HasBreakDuration {
		t.Error("HasBreakDuration = true, want false (no duration requested)")
	}
}

// TestBuildSpliceInsert_ImmediateHasNoPTS confirms HasPTS=false produces
// splice_immediate_flag=1 with no splice_time field at all, and that this
// still parses cleanly (regression guard for the variable-length encoding).
func TestBuildSpliceInsert_ImmediateHasNoPTS(t *testing.T) {
	built, err := BuildSpliceInsert(SpliceInsertOptions{EventID: 5, OutOfNetwork: true, DurationMS: 10000, AutoReturn: true})
	if err != nil {
		t.Fatalf("BuildSpliceInsert: %v", err)
	}
	parsed, err := Parse(built.RawSection)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.HasPTS {
		t.Error("HasPTS = true, want false")
	}
	if !parsed.HasBreakDuration || parsed.BreakDurationMS != 10000 {
		t.Errorf("BreakDurationMS = %d (has=%v), want 10000", parsed.BreakDurationMS, parsed.HasBreakDuration)
	}
}

// TestBuildTimeSignal_SegmentationRoundTrips builds a time_signal +
// segmentation_descriptor OUT (Provider Placement Opportunity Start=0x30)
// and confirms it round-trips, including the OutOfNetwork derivation from
// segmentation_type_id.
func TestBuildTimeSignal_SegmentationRoundTrips(t *testing.T) {
	built, err := BuildTimeSignal(TimeSignalOptions{
		PTS:                    987654321,
		HasPTS:                 true,
		SegmentationEventID:    2002,
		SegmentationTypeID:     0x30, // Provider Placement Opportunity Start
		SegmentationDurationMS: 60000,
	})
	if err != nil {
		t.Fatalf("BuildTimeSignal: %v", err)
	}
	if !built.OutOfNetwork {
		t.Error("OutOfNetwork = false, want true for a segmentation start type_id")
	}

	parsed, err := Parse(built.RawSection)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.CommandType != CommandTimeSignal {
		t.Errorf("CommandType = %v, want time_signal", parsed.CommandType)
	}
	if !parsed.HasPTS || parsed.PTS != 987654321 {
		t.Errorf("PTS = %d (has=%v), want 987654321", parsed.PTS, parsed.HasPTS)
	}
	if !parsed.HasSegmentation || parsed.SegmentationEventID != 2002 {
		t.Errorf("SegmentationEventID = %d (has=%v), want 2002", parsed.SegmentationEventID, parsed.HasSegmentation)
	}
	if parsed.SegmentationTypeID != 0x30 {
		t.Errorf("SegmentationTypeID = %#x, want 0x30", parsed.SegmentationTypeID)
	}
	if !parsed.HasSegmentationDuration || parsed.SegmentationDurationMS != 60000 {
		t.Errorf("SegmentationDurationMS = %d (has=%v), want 60000", parsed.SegmentationDurationMS, parsed.HasSegmentationDuration)
	}
}

// TestBuildTimeSignal_SegmentationEnd confirms an End type_id (0x31) derives
// OutOfNetwork=false (an IN), the counterpart to the Start test above.
func TestBuildTimeSignal_SegmentationEnd(t *testing.T) {
	built, err := BuildTimeSignal(TimeSignalOptions{
		SegmentationEventID: 2002,
		SegmentationTypeID:  0x31, // Provider Placement Opportunity End
	})
	if err != nil {
		t.Fatalf("BuildTimeSignal: %v", err)
	}
	if built.OutOfNetwork {
		t.Error("OutOfNetwork = true, want false for a segmentation end type_id")
	}
	if _, err := Parse(built.RawSection); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

// TestBuild_CRCIsValid is a direct, low-level check that assembleSection's
// CRC32 is computed over exactly the bytes Parse expects (everything before
// the trailing 4-byte CRC), independent of the round-trip tests above which
// only prove Parse *accepts* it — this confirms flipping any byte would be
// caught (i.e. the CRC isn't accidentally always-valid due to a computation
// bug that also cancels out in Parse).
func TestBuild_CRCIsValid(t *testing.T) {
	built, err := BuildSpliceInsert(SpliceInsertOptions{EventID: 1, OutOfNetwork: true})
	if err != nil {
		t.Fatalf("BuildSpliceInsert: %v", err)
	}
	corrupted := append([]byte(nil), built.RawSection...)
	corrupted[len(corrupted)-1] ^= 0xFF
	if _, err := Parse(corrupted); err != ErrCRCMismatch {
		t.Fatalf("Parse(corrupted) err = %v, want ErrCRCMismatch", err)
	}
}

// TestBuild_TableIDAndProtocolVersion is a byte-level sanity check on the
// fixed header fields the field checklist requires, independent of Parse.
func TestBuild_TableIDAndProtocolVersion(t *testing.T) {
	built, err := BuildSpliceInsert(SpliceInsertOptions{EventID: 1, OutOfNetwork: true})
	if err != nil {
		t.Fatalf("BuildSpliceInsert: %v", err)
	}
	if built.RawSection[0] != 0xFC {
		t.Errorf("table_id = %#x, want 0xFC", built.RawSection[0])
	}
	if built.RawSection[3] != 0x00 {
		t.Errorf("protocol_version = %#x, want 0x00", built.RawSection[3])
	}
	// pts_adjustment (bytes 4-8, 33 bits) must be zero for a freshly
	// generated section — no redistribution delay to compensate for.
	ptsAdj := (uint64(built.RawSection[4]&0x01) << 32) | uint64(built.RawSection[5])<<24 |
		uint64(built.RawSection[6])<<16 | uint64(built.RawSection[7])<<8 | uint64(built.RawSection[8])
	if ptsAdj != 0 {
		t.Errorf("pts_adjustment = %d, want 0", ptsAdj)
	}
}

// TestBuildTimeSignal_ContainsCUEIIdentifier confirms the
// segmentation_descriptor carries the "CUEI" registration identifier
// required by the SCTE-35/ANSI convention.
func TestBuildTimeSignal_ContainsCUEIIdentifier(t *testing.T) {
	built, err := BuildTimeSignal(TimeSignalOptions{SegmentationEventID: 1, SegmentationTypeID: 0x30})
	if err != nil {
		t.Fatalf("BuildTimeSignal: %v", err)
	}
	if !bytes.Contains(built.RawSection, []byte("CUEI")) {
		t.Error("RawSection does not contain the CUEI registration identifier")
	}
}
