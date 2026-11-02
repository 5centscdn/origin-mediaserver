package media

import (
	"encoding/hex"
	"testing"
)

// buildSEI builds a minimal but structurally real SEI NAL (type 6) with a
// single SEI message: the given payloadType, followed by a payload whose
// first byte is countryCode (only meaningful for payloadType 4) and the
// rest is arbitrary filler, terminated by rbsp_trailing_bits (0x80).
func buildSEI(payloadType byte, countryCode byte, filler int) []byte {
	payload := make([]byte, 0, 1+filler)
	payload = append(payload, countryCode)
	for i := 0; i < filler; i++ {
		payload = append(payload, 0x41) // arbitrary, never 0x00 so it can't trigger emulation prevention
	}
	nal := []byte{0x06, payloadType, byte(len(payload))}
	nal = append(nal, payload...)
	nal = append(nal, 0x80) // rbsp_trailing_bits
	return nal
}

func TestHasCEA608708_DetectsRegisteredUserDataWithATSCCountryCode(t *testing.T) {
	nal := buildSEI(4, 0xB5, 6)
	if !HasCEA608708(nal) {
		t.Error("HasCEA608708 = false, want true for payload_type=4, country_code=0xB5")
	}
}

func TestHasCEA608708_IgnoresOtherPayloadTypes(t *testing.T) {
	// payload_type=1 is pic_timing — extremely common, never caption data.
	nal := buildSEI(1, 0xB5, 6)
	if HasCEA608708(nal) {
		t.Error("HasCEA608708 = true for payload_type=1 (pic_timing), want false")
	}
}

func TestHasCEA608708_IgnoresNonATSCCountryCode(t *testing.T) {
	// payload_type=4 but a different itu_t_t35_country_code (not ATSC/CEA).
	nal := buildSEI(4, 0x26, 6) // 0x26 = China, arbitrary non-ATSC example
	if HasCEA608708(nal) {
		t.Error("HasCEA608708 = true for a non-ATSC country code, want false")
	}
}

func TestHasCEA608708_RejectsNonSEINAL(t *testing.T) {
	slice := []byte{0x65, 0x01, 0x02, 0x03}
	if HasCEA608708(slice) {
		t.Error("HasCEA608708 = true for a non-SEI NAL (type 5), want false")
	}
}

func TestHasCEA608708_SurvivesEmulationPreventionBytes(t *testing.T) {
	// Force an emulation-prevention byte into the payload: two zero bytes
	// followed by something <= 3 (here the country code slot deliberately
	// isn't 0xB5-adjacent to 00 00, so build the filler to contain a real
	// "00 00 03" escape sequence the parser must strip back out).
	payload := []byte{0xB5, 0x00, 0x00, 0x03, 0x01, 0x02} // encoder-escaped form of {0xB5, 0x00, 0x00, 0x01, 0x02}
	nal := []byte{0x06, 4, byte(len(payload))}
	nal = append(nal, payload...)
	nal = append(nal, 0x80)
	if !HasCEA608708(nal) {
		t.Error("HasCEA608708 = false for a payload containing an emulation-prevention sequence, want true")
	}
}

// realATSCSEINALHex is byte-for-byte the first SEI NAL (payload_type=4,
// country_code 0xB5, provider ATSC, "GA94", cc_data()) captured from a real
// RTMP encoder's published stream in production — the exact fixture that
// exposed UsesCEA708DTVCCService's reason for existing: this stream's
// cc_data() carries real CEA-708 DTVCC service data (cc_type 3 then
// repeated cc_type 2 triplets), not legacy CEA-608 field pairs, so
// signaling it with HLS's CC1 INSTREAM-ID (the 608 convention) made every
// player correctly find nothing on that channel and render no captions at
// all, despite the caption bytes themselves being fully intact.
const realATSCSEINALHex = "0604" +
	"39" + // payload_size (57 bytes — this fixture is truncated partway
	// through the real capture's cc_count=31 triplet list; that's fine,
	// UsesCEA708DTVCCService only needs to see the first couple)
	"b5" + "0031" + "47413934" + "03" + // country_code, provider(ATSC), "GA94", user_data_type_code(cc_data)
	"5f" + "ff" + // cc_count byte (process_cc_data_flag=1, cc_count=31), em_data
	"ff223f" + "fe8c01" + "fe8098" + "fe204b" + "fe3203" + "fe1f00" + "fe9700" + "fe0000" +
	"fe0091" + "fe3f00" + "fe0092" + "fe0000" + "fe2841" + "fe6e64" + "fe2079" + "fe6f" // truncated triplet, fine — parser stops at cc_count/len bound

func hexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func TestUsesCEA708DTVCCService_DetectsRealEncoderOutput(t *testing.T) {
	nal := hexBytes(t, realATSCSEINALHex)
	if !HasCEA608708(nal) {
		t.Fatal("HasCEA608708 = false for a real captured ATSC SEI NAL, want true")
	}
	if !UsesCEA708DTVCCService(nal) {
		t.Error("UsesCEA708DTVCCService = false for a real encoder's CEA-708 DTVCC stream, want true")
	}
}

func TestUsesCEA708DTVCCService_FalseForPlainCEA608FieldPairs(t *testing.T) {
	// Same ATSC/GA94/cc_data() framing, but every triplet's cc_type is 0
	// (NTSC_CC_FIELD_1) — real legacy line-21 CEA-608 data, not 708.
	payload := []byte{0x00, 0x31} // provider = ATSC
	payload = append(payload, []byte("GA94")...)
	payload = append(payload, 0x03)          // cc_data()
	payload = append(payload, 0x02, 0xFF)    // cc_count=2, em_data
	payload = append(payload, 0xFC, 0x41, 0x42) // marker=11111,valid=1,type=00; data
	payload = append(payload, 0xFC, 0x43, 0x44)

	nal := []byte{0x06, 0x04, byte(1 + len(payload)), 0xB5}
	nal = append(nal, payload...)
	nal = append(nal, 0x80)

	if !HasCEA608708(nal) {
		t.Fatal("HasCEA608708 = false, want true")
	}
	if UsesCEA708DTVCCService(nal) {
		t.Error("UsesCEA708DTVCCService = true for plain cc_type=0 CEA-608 field pairs, want false")
	}
}

func TestUsesCEA708DTVCCService_FalseWithoutATSCFraming(t *testing.T) {
	// HasCEA608708-detectable (country_code 0xB5) but not real ATSC
	// cc_data() framing (no "GA94"/provider/user_data_type_code) — the
	// generic fixture the other tests in this file already use.
	nal := buildSEI(4, 0xB5, 10)
	if UsesCEA708DTVCCService(nal) {
		t.Error("UsesCEA708DTVCCService = true for a payload with no real ATSC cc_data() framing, want false")
	}
}

func TestHasCEA608708_MultiByteLengthEncoding(t *testing.T) {
	// payload_size >= 255 must be encoded as one or more 0xFF continuation
	// bytes followed by the remainder — build a payload exactly 260 bytes
	// long (0xFF + 5) to exercise that.
	payload := make([]byte, 260)
	payload[0] = 0xB5
	for i := 1; i < len(payload); i++ {
		payload[i] = 0x41
	}
	nal := []byte{0x06, 4, 0xFF, 5} // payload_type=4 (1 byte), payload_size=255+5=260 (0xFF then 5)
	nal = append(nal, payload...)
	nal = append(nal, 0x80)
	if !HasCEA608708(nal) {
		t.Error("HasCEA608708 = false for a multi-byte (0xFF-continuation) payload_size encoding, want true")
	}
}
