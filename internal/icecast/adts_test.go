package icecast

import (
	"bytes"
	"testing"
)

func TestParseADTSHeader(t *testing.T) {
	rawAAC := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	const sfIndex, channelConfig = 4, 2 // 44100Hz, stereo
	frame := buildTestADTS(rawAAC, sfIndex, channelConfig)

	info, ok := parseADTSHeader(frame)
	if !ok {
		t.Fatal("parseADTSHeader: not ok")
	}
	if info.length != len(frame) {
		t.Errorf("length = %d, want %d", info.length, len(frame))
	}
	if info.headerLen != 7 {
		t.Errorf("headerLen = %d, want 7 (no CRC)", info.headerLen)
	}
	if got := buildAudioSpecificConfig(info.audioObjectType, info.sfIndex, info.channelConfig); len(got) != 2 {
		t.Errorf("buildAudioSpecificConfig returned %d bytes, want 2", len(got))
	}
	if !bytes.Equal(frame[info.headerLen:info.length], rawAAC) {
		t.Errorf("stripped payload = % x, want % x", frame[info.headerLen:info.length], rawAAC)
	}
}

func TestParseADTSHeader_RejectsBadSync(t *testing.T) {
	if _, ok := parseADTSHeader([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}); ok {
		t.Error("expected rejection of a non-sync-word header")
	}
	if _, ok := parseADTSHeader([]byte{0xFF, 0xF1}); ok {
		t.Error("expected rejection of a too-short buffer")
	}
}

// buildTestADTS wraps rawAAC in a 7-byte ADTS header (no CRC) — same
// construction as internal/ingest/mpegts's test helper of the same shape,
// duplicated per this codebase's small-test-helper convention.
func buildTestADTS(rawAAC []byte, sfIndex, channelConfig byte) []byte {
	frameLength := 7 + len(rawAAC)
	header := []byte{
		0xFF, 0xF1,
		(1 << 6) | (sfIndex << 2) | (channelConfig >> 2),
		(channelConfig&0x03)<<6 | byte(frameLength>>11),
		byte(frameLength >> 3),
		byte(frameLength<<5) | 0x1F,
		0xFC,
	}
	return append(header, rawAAC...)
}
