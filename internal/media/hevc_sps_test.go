package media

import (
	"encoding/base64"
	"testing"
)

// Fixture: a real HEVC VPS/SPS/PPS encoded by libx265 (64x64, 3 frames,
// keyint=3) — captured once via `ffmpeg -f lavfi -i testsrc=size=64x64:rate=25
// -frames:v 3 -c:v libx265 -x265-params keyint=3 -f hevc ...`, NAL units
// split on Annex-B start codes and base64-embedded, matching this
// project's existing fixture style for other codecs' tests.
const (
	fixtureHEVCVPS = "QAEMAf//AWAAAAMAkAAAAwAAAwAelZgJAA=="
	fixtureHEVCSPS = "QgEBAWAAAAMAkAAAAwAAAwAeoCCBBZZWaSTK8BaAgAAAAwCAAAAMhAA="
	fixtureHEVCPPS = "RAHBcrQiQA=="
)

func decodeHEVCFixture(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return b
}

func TestParseHEVCSPS_RealEncoderOutput(t *testing.T) {
	sps := decodeHEVCFixture(t, fixtureHEVCSPS)

	width, height, ptl, ok := ParseHEVCSPS(sps)
	if !ok {
		t.Fatal("ParseHEVCSPS: ok = false, want true")
	}
	if width != 64 || height != 64 {
		t.Errorf("got %dx%d, want 64x64", width, height)
	}
	// x265's default output: Main profile (1), Main tier (low, so
	// TierFlag=false), profile_space=0.
	if ptl.ProfileSpace != 0 {
		t.Errorf("ProfileSpace = %d, want 0", ptl.ProfileSpace)
	}
	if ptl.TierFlag {
		t.Error("TierFlag = true, want false (Main tier)")
	}
	if ptl.ProfileIDC != 1 {
		t.Errorf("ProfileIDC = %d, want 1 (Main)", ptl.ProfileIDC)
	}
	if ptl.LevelIDC == 0 {
		t.Error("LevelIDC = 0, want a real nonzero level")
	}
}

func TestParseHEVCSPS_TooShort(t *testing.T) {
	if _, _, _, ok := ParseHEVCSPS([]byte{0x42, 0x01}); ok {
		t.Error("expected ok=false for a too-short SPS")
	}
}

func TestHEVCCodecString(t *testing.T) {
	sps := decodeHEVCFixture(t, fixtureHEVCSPS)
	_, _, ptl, ok := ParseHEVCSPS(sps)
	if !ok {
		t.Fatal("setup: ParseHEVCSPS failed")
	}
	ci := &CodecInfo{
		VideoCodec:        CodecH265,
		HEVCProfileSpace:  ptl.ProfileSpace,
		HEVCTierFlag:      ptl.TierFlag,
		HEVCProfileIDC:    ptl.ProfileIDC,
		HEVCProfileCompat: ptl.ProfileCompat,
		HEVCLevelIDC:      ptl.LevelIDC,
	}
	got := HEVCCodecString(ci)
	if len(got) < len("hvc1.1.") || got[:7] != "hvc1.1." {
		t.Errorf("HEVCCodecString(ci) = %q, want it to start with %q", got, "hvc1.1.")
	}
	if HEVCCodecString(nil) == "" {
		t.Error("HEVCCodecString(nil) should return a non-empty generic default")
	}
}
