package mux

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// readBox reads one ISO-BMFF box header (size+type) at the start of b and
// returns its body and the total box size (header + body).
func readBox(t *testing.T, b []byte) (boxType string, body []byte, total int) {
	t.Helper()
	if len(b) < 8 {
		t.Fatalf("buffer too short for a box header: %d bytes", len(b))
	}
	size := binary.BigEndian.Uint32(b[0:4])
	boxType = string(b[4:8])
	if int(size) > len(b) {
		t.Fatalf("box %q size %d exceeds buffer length %d", boxType, size, len(b))
	}
	return boxType, b[8:size], int(size)
}

// findChildBox scans a box's body for a direct child box of the given type.
func findChildBox(t *testing.T, body []byte, want string) []byte {
	t.Helper()
	pos := 0
	for pos < len(body) {
		boxType, childBody, total := readBox(t, body[pos:])
		if boxType == want {
			return childBody
		}
		pos += total
	}
	t.Fatalf("box %q not found", want)
	return nil
}

func TestWriteMediaSegment_TrunDataOffset(t *testing.T) {
	track := &FMP4Track{
		Type:      TrackVideo,
		TrackID:   1,
		Timescale: 90000,
		Width:     1280,
		Height:    720,
	}
	muxer := NewFMP4Muxer(track)

	samples := []Sample{
		{DTS: 0, PTS: 0, Duration: 3000, IsKey: true, Data: []byte("keyframe-payload")},
		{DTS: 3000, PTS: 3000, Duration: 3000, Data: []byte("p-frame")},
	}

	var buf bytes.Buffer
	if err := muxer.WriteMediaSegment(&buf, 0, samples, 0); err != nil {
		t.Fatalf("WriteMediaSegment: %v", err)
	}
	out := buf.Bytes()

	moofType, moofBody, moofTotal := readBox(t, out)
	if moofType != "moof" {
		t.Fatalf("expected moof as first box, got %q", moofType)
	}

	trafBody := findChildBox(t, moofBody, "traf")
	trunBody := findChildBox(t, trafBody, "trun")

	// trun body layout: version(1) + flags(3) + sample_count(4) + data_offset(4) + ...
	if len(trunBody) < 12 {
		t.Fatalf("trun body too short: %d bytes", len(trunBody))
	}
	dataOffset := int32(binary.BigEndian.Uint32(trunBody[8:12]))

	wantDataOffset := int32(moofTotal + 8) // +8 = mdat's own size+type header
	if dataOffset != wantDataOffset {
		t.Fatalf("trun data_offset = %d, want %d (moof size %d + mdat header 8)", dataOffset, wantDataOffset, moofTotal)
	}

	// The data_offset is measured from the start of moof; verify it actually
	// lands on the first sample's first byte.
	firstSampleAt := int(dataOffset)
	if firstSampleAt+len(samples[0].Data) > len(out) {
		t.Fatalf("data_offset %d points past end of buffer (len %d)", dataOffset, len(out))
	}
	got := out[firstSampleAt : firstSampleAt+len(samples[0].Data)]
	if !bytes.Equal(got, samples[0].Data) {
		t.Fatalf("bytes at data_offset = %q, want %q", got, samples[0].Data)
	}
}

// TestWriteMediaSegment_SampleFlags verifies each sample's sync/non-sync bit
// is actually written into trun (sample-flags-present) — a real bug this
// project shipped for a while: with the flag omitted, every sample defaults
// to trex's default_sample_flags (0 = "is a sync sample" per ISO/IEC
// 14496-12's inverted sample_is_non_sync_sample bit), so every P-frame was
// incorrectly advertised as a random-access point. Caught only by a real
// strict browser MSE decoder (Chrome/dash.js rejecting video samples
// outright), not by this project's own box-layout tests or ffprobe.
func TestWriteMediaSegment_SampleFlags(t *testing.T) {
	track := &FMP4Track{Type: TrackVideo, TrackID: 1, Timescale: 90000, Width: 1280, Height: 720}
	muxer := NewFMP4Muxer(track)

	samples := []Sample{
		{DTS: 0, PTS: 0, Duration: 3000, IsKey: true, Data: []byte("key")},
		{DTS: 3000, PTS: 3000, Duration: 3000, IsKey: false, Data: []byte("p1")},
		{DTS: 6000, PTS: 6000, Duration: 3000, IsKey: false, Data: []byte("p2")},
	}

	var buf bytes.Buffer
	if err := muxer.WriteMediaSegment(&buf, 0, samples, 0); err != nil {
		t.Fatalf("WriteMediaSegment: %v", err)
	}

	_, moofBody, _ := readBox(t, buf.Bytes())
	trafBody := findChildBox(t, moofBody, "traf")
	trunBody := findChildBox(t, trafBody, "trun")

	flags := uint32(trunBody[1])<<16 | uint32(trunBody[2])<<8 | uint32(trunBody[3])
	if flags&0x000400 == 0 {
		t.Fatalf("trun flags = 0x%06X, missing sample-flags-present (0x000400)", flags)
	}

	// Per-sample fields start right after data_offset (4 bytes), in order
	// duration(4) + size(4) + flags(4) + cto(4) = 16 bytes/sample.
	const perSample = 16
	base := 12
	wantFlags := []uint32{0x02000000, 0x01010000, 0x01010000} // key, non-key, non-key
	for i, want := range wantFlags {
		off := base + i*perSample + 8 // skip this sample's duration+size to reach its flags field
		got := binary.BigEndian.Uint32(trunBody[off : off+4])
		if got != want {
			t.Errorf("sample %d (IsKey=%v) sample_flags = 0x%08X, want 0x%08X", i, samples[i].IsKey, got, want)
		}
	}
}
