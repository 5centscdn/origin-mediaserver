package dash

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/5centscdn/origin-mediaserver/internal/captiondecode"
	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// buildTestSEINAL constructs a real H.264 SEI NAL (type 6) carrying one
// ATSC A/53 cc_data() triplet — the same construction captiondecode's own
// tests use, duplicated here since it's a small, test-only helper and this
// package has no dependency on captiondecode's internal test code.
func buildTestSEINAL(triplet [3]byte) []byte {
	payload := []byte{0xB5, 0x00, 0x31, 'G', 'A', '9', '4', 0x03, 0xC1, 0xFF}
	payload = append(payload, triplet[:]...)
	rbsp := []byte{0x04, byte(len(payload))}
	rbsp = append(rbsp, payload...)
	rbsp = append(rbsp, 0x80)
	return append([]byte{0x06}, rbsp...)
}

// annexB wraps one or more NALs with 4-byte Annex-B start codes, matching
// media.Packet.Data's documented convention.
func annexB(nals ...[]byte) []byte {
	var out []byte
	for _, n := range nals {
		out = append(out, 0, 0, 0, 1)
		out = append(out, n...)
	}
	return out
}

// runCaptionSequence drives a real Packager through handleVideo/closeSegment
// with real CEA-608 caption bytes ("HI") embedded in synthetic H.264 SEI
// NALs, exercising the full SEI-NAL -> captiondecode -> (ebuttd|webvtt) ->
// fMP4 pipeline, not any one piece in isolation. Returns the temp output
// directory so callers can assert on the format-specific seg-text-1.m4s
// contents.
func runCaptionSequence(t *testing.T, dvbProfile bool) string {
	t.Helper()
	cfg := &config.Config{}
	cfg.DASH.SegmentDurationS = 4

	dir := t.TempDir()
	stream := &media.Stream{Key: media.StreamKey{App: "test", Name: "s1"}, Bus: media.NewBus()}
	stream.Bus.SetCodecInfo(&media.CodecInfo{
		VideoCodec: media.CodecH264,
		Width:      640, Height: 360,
		SPSNALu: []byte{0x67, 0x42, 0x00, 0x1e, 0x00},
		PPSNALu: []byte{0x68, 0xce},
	})

	p := &Packager{
		cfg: cfg, stream: stream, outDir: dir, log: slog.Default(),
		segSeq:        1,
		textExtractor: captiondecode.NewExtractor(),
		dvbProfile:    dvbProfile,
	}

	sliceNAL := []byte{0x41, 0xAA, 0xBB} // dummy non-IDR-looking slice payload; content is opaque to this test

	// Keyframe at DTS=90000 (1s) opens the segment — deliberately not DTS=0:
	// segStartDTS<=0 is this packager's (pre-existing, documented, real-
	// encoder-never-hits-it) "segment not open" sentinel, which a literal
	// zero DTS would collide with.
	const t0 = 90000
	if err := p.handleVideo(&media.Packet{DTS: t0, PTS: t0, IsKeyframe: true, Data: annexB(sliceNAL)}); err != nil {
		t.Fatalf("handleVideo (open): %v", err)
	}

	// RCL (enter Pop-On), at t0+1s.
	rcl := buildTestSEINAL([3]byte{0xFC, 0x14, 0x20})
	if err := p.handleVideo(&media.Packet{DTS: t0 + 90000, PTS: t0 + 90000, Data: annexB(rcl, sliceNAL)}); err != nil {
		t.Fatalf("handleVideo (RCL): %v", err)
	}

	// Text "HI".
	text := buildTestSEINAL([3]byte{0xFC, 'H', 'I'})
	if err := p.handleVideo(&media.Packet{DTS: t0 + 99000, PTS: t0 + 99000, Data: annexB(text, sliceNAL)}); err != nil {
		t.Fatalf("handleVideo (text): %v", err)
	}

	// EOC (swap memories — becomes visible), at t0+2s.
	eoc := buildTestSEINAL([3]byte{0xFC, 0x14, 0x2F})
	if err := p.handleVideo(&media.Packet{DTS: t0 + 180000, PTS: t0 + 180000, Data: annexB(eoc, sliceNAL)}); err != nil {
		t.Fatalf("handleVideo (EOC): %v", err)
	}

	// EDM (flush the visible "HI"), at t0+3s.
	edm := buildTestSEINAL([3]byte{0xFC, 0x14, 0x2C})
	if err := p.handleVideo(&media.Packet{DTS: t0 + 270000, PTS: t0 + 270000, Data: annexB(edm, sliceNAL)}); err != nil {
		t.Fatalf("handleVideo (EDM): %v", err)
	}

	// Next keyframe past the 4s segment duration closes segment 1.
	if err := p.handleVideo(&media.Packet{DTS: t0 + 4*90000 + 1, PTS: t0 + 4*90000 + 1, IsKeyframe: true, Data: annexB(sliceNAL)}); err != nil {
		t.Fatalf("handleVideo (close): %v", err)
	}

	initPath := filepath.Join(dir, "init-text.mp4")
	if _, err := os.Stat(initPath); err != nil {
		t.Errorf("expected init-text.mp4 to exist: %v", err)
	}

	return dir
}

// TestPackager_TextTrack_EndToEnd_DVBProfile verifies the DVB-profile path:
// EBU-TT-D XML (codecs="stpp") — DVB-DASH mandates it.
func TestPackager_TextTrack_EndToEnd_DVBProfile(t *testing.T) {
	dir := runCaptionSequence(t, true)

	data, err := os.ReadFile(filepath.Join(dir, "seg-text-1.m4s"))
	if err != nil {
		t.Fatalf("expected seg-text-1.m4s to exist: %v", err)
	}
	s := string(data)
	for _, want := range []string{"<tt", "HI", "</tt>"} {
		if !strings.Contains(s, want) {
			t.Errorf("seg-text-1.m4s missing %q; does not contain the expected EBU-TT-D cue text, got:\n%q", want, s)
		}
	}
}

// TestPackager_TextTrack_EndToEnd_NonDVBUsesWebVTT verifies the non-DVB path:
// WebVTT-in-ISOBMFF (codecs="wvtt") samples — live EBU-TT-D/fragmented-TTML
// rendering was found unreliable across web DASH clients (see
// internal/webvtt's doc comment), so this is the default caption path
// outside the DVB profile.
func TestPackager_TextTrack_EndToEnd_NonDVBUsesWebVTT(t *testing.T) {
	dir := runCaptionSequence(t, false)

	data, err := os.ReadFile(filepath.Join(dir, "seg-text-1.m4s"))
	if err != nil {
		t.Fatalf("expected seg-text-1.m4s to exist: %v", err)
	}
	s := string(data)
	for _, want := range []string{"vttc", "payl", "HI"} {
		if !strings.Contains(s, want) {
			t.Errorf("seg-text-1.m4s missing %q; does not contain the expected WebVTT cue, got:\n%q", want, s)
		}
	}
	if strings.Contains(s, "<tt") {
		t.Errorf("expected no EBU-TT-D XML in a non-DVB-profile text segment, got:\n%q", s)
	}
}
