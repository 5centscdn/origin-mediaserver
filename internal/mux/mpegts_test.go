package mux

import (
	"bytes"
	"testing"
)

// TestTSMuxer_HEVCParamSetsPrependedInOrder guards the HEVC-specific
// prepend path in WriteVideo: unlike H.264 (SPS+PPS only), HEVC keyframes
// must be preceded by VPS, then SPS, then PPS, each with its own Annex-B
// start code — real players require this exact order to join mid-stream.
func TestTSMuxer_HEVCParamSetsPrependedInOrder(t *testing.T) {
	var buf bytes.Buffer
	m := NewTSMuxer(&buf, StreamTypeH265, StreamTypeAAC)

	vps := []byte{0x40, 0x01, 0xAA}
	sps := []byte{0x42, 0x01, 0xBB}
	pps := []byte{0x44, 0x01, 0xCC}
	m.SetHEVCParamSets(vps, sps, pps)

	startCode := []byte{0x00, 0x00, 0x00, 0x01}
	sliceNAL := []byte{0x26, 0x01, 0xDD} // fake IDR slice payload
	// WriteVideo never adds its own start code before the slice — callers
	// (RTMP decode, the HLS TS packager) already supply one as part of
	// media.Packet.Data's Annex-B convention, so the input here must too.
	slice := append(append([]byte{}, startCode...), sliceNAL...)
	if err := m.WriteVideo(slice, 0, 0, true); err != nil {
		t.Fatalf("WriteVideo: %v", err)
	}

	out := buf.Bytes()
	want := append(append(append(append(append([]byte{}, startCode...), vps...), startCode...), sps...), startCode...)
	want = append(append(want, pps...), startCode...)
	want = append(want, sliceNAL...)

	if !bytes.Contains(out, want) {
		t.Errorf("expected the PES payload to contain VPS+SPS+PPS+slice in order, each with its own start code — got %d bytes total, want subsequence:\n%x", len(out), want)
	}
}

// TestTSMuxer_H264ParamSetsUnaffectedByHEVCPath guards against a
// regression in the shared prepend code: SetSPSPPS (H.264) must still work
// exactly as before — no VPS ever gets inserted for an H.264 stream.
func TestTSMuxer_H264ParamSetsUnaffectedByHEVCPath(t *testing.T) {
	var buf bytes.Buffer
	m := NewTSMuxer(&buf, StreamTypeH264, StreamTypeAAC)

	sps := []byte{0x67, 0x01, 0xBB}
	pps := []byte{0x68, 0x01, 0xCC}
	m.SetSPSPPS(sps, pps)

	startCode := []byte{0x00, 0x00, 0x00, 0x01}
	sliceNAL := []byte{0x65, 0x01, 0xDD}
	slice := append(append([]byte{}, startCode...), sliceNAL...) // caller-supplied start code — see the HEVC test's comment
	if err := m.WriteVideo(slice, 0, 0, true); err != nil {
		t.Fatalf("WriteVideo: %v", err)
	}

	out := buf.Bytes()
	want := append(append(append(append([]byte{}, startCode...), sps...), startCode...), pps...)
	want = append(append(want, startCode...), sliceNAL...)

	if !bytes.Contains(out, want) {
		t.Errorf("expected SPS+PPS+slice in order, got %d bytes total, want subsequence:\n%x", len(out), want)
	}
	if bytes.Contains(out, []byte{0x40, 0x01}) {
		t.Error("H.264 output should never contain an HEVC-style VPS NAL header")
	}
}

// TestTSMuxerAudioOnly_PMTOmitsVideoAndPointsPCRAtAudio guards the Icecast
// ingest scope decision: a source with no video track at all must not
// declare a video ES entry that will never carry a single packet, and
// PCR_PID must point at a PID that actually receives data instead of the
// hardcoded video PID every other constructor path uses.
func TestTSMuxerAudioOnly_PMTOmitsVideoAndPointsPCRAtAudio(t *testing.T) {
	var buf bytes.Buffer
	m := NewTSMuxerAudioOnly(&buf, StreamTypeMP3)

	if err := m.WriteAudio([]byte{0xAA, 0xBB, 0xCC}, 1000); err != nil {
		t.Fatalf("WriteAudio: %v", err)
	}

	pmt := findTSPacket(t, buf.Bytes(), PIDPmt)
	// pointer_field(1) + table_id(1) + 2 length bytes + 5 fixed header bytes
	// = section starts at payload[9] for PCR_PID (E1 xx at bytes[8:10] of
	// the section, i.e. payload offset 1+9 = 10 for the low byte).
	section := pmt[1:] // strip pointer_field
	pcrPID := (uint16(section[8]&0x1F) << 8) | uint16(section[9])
	if pcrPID != PIDAudio {
		t.Errorf("PCR_PID = 0x%04X, want audio PID 0x%04X", pcrPID, PIDAudio)
	}

	esInfo := section[12 : len(section)-4] // after program_info_length, before CRC
	if len(esInfo) != 5 {
		t.Fatalf("ES info length = %d bytes, want exactly 5 (one audio entry, no video entry): %x", len(esInfo), esInfo)
	}
	if esInfo[0] != StreamTypeMP3 {
		t.Errorf("only ES entry's stream_type = 0x%02X, want MP3 (0x%02X)", esInfo[0], StreamTypeMP3)
	}
	esPID := (uint16(esInfo[1]&0x1F) << 8) | uint16(esInfo[2])
	if esPID != PIDAudio {
		t.Errorf("only ES entry's PID = 0x%04X, want audio PID 0x%04X", esPID, PIDAudio)
	}
}

// TestTSMuxer_SCTE35Disabled guards the non-regression requirement: a
// muxer that never calls EnableSCTE35 must produce a PMT with no SCTE-35 ES
// entry at all — this is how existing streams stay byte-for-byte unchanged
// when scte35_passthrough is off.
func TestTSMuxer_SCTE35Disabled(t *testing.T) {
	var buf bytes.Buffer
	m := NewTSMuxer(&buf, StreamTypeH264, StreamTypeAAC)
	if err := m.WriteVideo([]byte{0x00, 0x00, 0x00, 0x01, 0x65}, 0, 0, true); err != nil {
		t.Fatalf("WriteVideo: %v", err)
	}

	pmt := findTSPacket(t, buf.Bytes(), PIDPmt)
	section := pmt[1:]
	esInfo := section[12 : len(section)-4]
	if bytes.Contains(esInfo, []byte{streamTypeSCTE35}) {
		t.Errorf("PMT ES info contains stream_type 0x86 without EnableSCTE35 having been called: %x", esInfo)
	}

	if err := m.WriteSCTE35([]byte{0xFC, 0x00}); err == nil {
		t.Error("WriteSCTE35 without EnableSCTE35 = nil error, want an error")
	}
}

// TestTSMuxer_SCTE35EnabledAdvertisesPIDAndPassesThroughVerbatim covers the
// PMT-advertisement and pass-through requirements: once EnableSCTE35 is
// called, the PMT must declare a stream_type=0x86 ES entry (with a "CUEI"
// registration_descriptor) on the SCTE-35 PID, and WriteSCTE35 must write
// the exact input bytes back out unmodified.
func TestTSMuxer_SCTE35EnabledAdvertisesPIDAndPassesThroughVerbatim(t *testing.T) {
	var buf bytes.Buffer
	m := NewTSMuxer(&buf, StreamTypeH264, StreamTypeAAC)
	m.EnableSCTE35()

	if err := m.WriteVideo([]byte{0x00, 0x00, 0x00, 0x01, 0x65}, 0, 0, true); err != nil {
		t.Fatalf("WriteVideo: %v", err)
	}

	pmt := findTSPacket(t, buf.Bytes(), PIDPmt)
	section := pmt[1:]
	esInfo := section[12 : len(section)-4]

	found := false
	for pos := 0; pos+5 <= len(esInfo); {
		streamType := esInfo[pos]
		pid := (uint16(esInfo[pos+1]&0x1F) << 8) | uint16(esInfo[pos+2])
		esInfoLen := int(esInfo[pos+3]&0x0F)<<8 | int(esInfo[pos+4])
		if streamType == streamTypeSCTE35 {
			found = true
			if pid != PIDSCTE35 {
				t.Errorf("SCTE-35 ES PID = 0x%04X, want 0x%04X", pid, PIDSCTE35)
			}
			descriptor := esInfo[pos+5 : pos+5+esInfoLen]
			if !bytes.Contains(descriptor, []byte("CUEI")) {
				t.Errorf("SCTE-35 ES info descriptor = %x, want a CUEI registration_descriptor", descriptor)
			}
		}
		pos += 5 + esInfoLen
	}
	if !found {
		t.Fatalf("no stream_type 0x86 ES entry found in PMT after EnableSCTE35: %x", esInfo)
	}

	section35 := []byte{0xFC, 0x30, 0x00, 0x00, 0xDE, 0xAD, 0xBE, 0xEF} // not a real valid section — pass-through must not care
	if err := m.WriteSCTE35(section35); err != nil {
		t.Fatalf("WriteSCTE35: %v", err)
	}
	scte := findTSPacket(t, buf.Bytes(), PIDSCTE35)
	got := scte[1:] // strip pointer_field
	if !bytes.Equal(got, section35) {
		t.Errorf("WriteSCTE35 output = %x, want verbatim %x (no rebuilding)", got, section35)
	}
}

// TestTSMuxer_WriteAudio_OneByteShortPadding is a regression test for a real
// bug found while debugging a live SRT stream that ffplay/hls.js reported
// as corrupt ("PES packet size mismatch", stream=1/audio): writePayload's
// adaptation-field-stuffing branch only fired for `padding >= 2`, so a PES
// whose final TS-packet fragment was exactly 183 bytes (1 byte short of
// the 184-byte payload area, i.e. padding==1) fell through to the
// no-adaptation-field path and left one uninitialized zero byte at the end
// of the packet — a byte a conformant demuxer reads back as real stream
// data, corrupting every PES that happened to land on that exact size.
// This constructs an audio payload sized so the resulting PES is exactly
// 183 bytes (fits in one TS packet with padding==1) and confirms the
// packet carries a correctly-sized adaptation field instead of a stray
// trailing byte.
func TestTSMuxer_WriteAudio_OneByteShortPadding(t *testing.T) {
	// buildAudioPES's fixed overhead is 14 bytes (4 start+id, 2 length
	// field, 3 flags/header-length, 5 PTS) — 169 bytes of payload makes a
	// 183-byte PES, one byte short of a full 184-byte TS payload area.
	const rawPayloadLen = 169
	payload := make([]byte, rawPayloadLen)
	for i := range payload {
		payload[i] = byte(i)
	}

	var buf bytes.Buffer
	m := NewTSMuxer(&buf, StreamTypeH264, StreamTypeAAC)
	if err := m.WriteAudio(payload, 1000); err != nil {
		t.Fatalf("WriteAudio: %v", err)
	}

	got := findTSPacket(t, buf.Bytes(), PIDAudio)
	const wantPESLen = 14 + rawPayloadLen // 183
	if len(got) != wantPESLen {
		t.Fatalf("audio TS packet payload = %d bytes, want %d (padding==1 must still get a 1-byte adaptation field, not a bare short payload with a stray trailing byte)", len(got), wantPESLen)
	}
	if !bytes.Equal(got[len(got)-rawPayloadLen:], payload) {
		t.Errorf("reassembled PES payload tail does not match the original audio payload")
	}
}

// findTSPacket returns the payload of the first 188-byte packet matching
// pid, skipping the 4-byte TS header and — if present (adaptation_field_
// control bit 0x20 set) — the adaptation field used to pad short payloads
// out to a full packet.
func findTSPacket(t *testing.T, data []byte, pid uint16) []byte {
	t.Helper()
	for off := 0; off+TSPacketSize <= len(data); off += TSPacketSize {
		pkt := data[off : off+TSPacketSize]
		gotPID := (uint16(pkt[1]&0x1F) << 8) | uint16(pkt[2])
		if gotPID != pid {
			continue
		}
		payloadStart := 4
		if pkt[3]&0x20 != 0 { // adaptation field present
			payloadStart += 1 + int(pkt[4])
		}
		return pkt[payloadStart:]
	}
	t.Fatalf("no TS packet found for PID 0x%04X", pid)
	return nil
}
