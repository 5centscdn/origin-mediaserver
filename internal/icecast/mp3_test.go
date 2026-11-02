package icecast

import (
	"encoding/base64"
	"testing"
)

func decodeFixture(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return b
}

// TestParseMP3Header_RealLibmp3lameFixture walks a real libmp3lame-encoded
// MP3 stream frame by frame using only parseMP3Header's own reported frame
// lengths, and asserts that doing so consumes the entire fixture exactly
// (no gaps, no overruns) — the real test of whether the frame-length
// arithmetic (bitrate/sample-rate tables, the Layer III length formula) is
// actually correct, not just plausible-looking.
func TestParseMP3Header_RealLibmp3lameFixture(t *testing.T) {
	data := decodeFixture(t, fixtureMP3)

	pos := 0
	frames := 0
	for pos < len(data) {
		info, ok := parseMP3Header(data[pos:])
		if !ok {
			t.Fatalf("parseMP3Header failed at offset %d (%d bytes remaining): % x", pos, len(data)-pos, data[pos:min(pos+8, len(data))])
		}
		if info.sampleRate != 44100 {
			t.Errorf("frame %d: sampleRate = %d, want 44100", frames, info.sampleRate)
		}
		if info.channels != 2 {
			t.Errorf("frame %d: channels = %d, want 2", frames, info.channels)
		}
		if info.samplesPerFrame != 1152 {
			t.Errorf("frame %d: samplesPerFrame = %d, want 1152 (MPEG1 Layer III)", frames, info.samplesPerFrame)
		}
		pos += info.length
		frames++
	}
	if pos != len(data) {
		t.Errorf("consumed %d bytes, want exactly %d (the whole fixture)", pos, len(data))
	}
	// 14, not ffprobe's reported 13: libmp3lame prepends a syntactically
	// real MPEG audio frame carrying its Xing/LAME VBR info tag as payload
	// (visible as "Info" in the fixture's raw bytes) — most decoders
	// recognize and skip this frame as encoder metadata, but structurally
	// it's a valid frame like any other, and this parser doesn't need to
	// special-case it: it's just one more opaque audio frame passed
	// through to the muxer, exactly like every other MP3 frame.
	if frames != 14 {
		t.Errorf("parsed %d frames, want 14", frames)
	}
}

func TestParseMP3Header_RejectsBadSync(t *testing.T) {
	if _, ok := parseMP3Header([]byte{0x00, 0x00, 0x00, 0x00}); ok {
		t.Error("expected rejection of a non-sync-word header")
	}
	if _, ok := parseMP3Header([]byte{0xFF}); ok {
		t.Error("expected rejection of a too-short buffer")
	}
}
