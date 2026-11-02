package captiondecode

import "testing"

// buildSEINAL constructs a real, well-formed H.264 SEI NAL (type 6) carrying
// one ATSC A/53 cc_data() triplet, so this test exercises the actual
// media.ExtractCCData path (SEI message parsing, ITU-T T.35/ATSC framing) in
// addition to the decoder logic already covered by cea608_test.go/
// cea708_test.go — not just the decoders in isolation.
func buildSEINAL(triplets []byte) []byte {
	ccCount := len(triplets) / 3
	ccDataByte0 := byte(0xC0 | byte(ccCount&0x1F)) // process_cc_data_flag=1, reserved bits set, cc_count
	payload := []byte{0xB5, 0x00, 0x31}            // itu_t_t35_country_code, provider_code
	payload = append(payload, 'G', 'A', '9', '4')
	payload = append(payload, 0x03) // user_data_type_code: cc_data()
	payload = append(payload, ccDataByte0, 0xFF)
	payload = append(payload, triplets...)

	rbsp := []byte{0x04, byte(len(payload))} // payload_type=4 (user data), payload_size
	rbsp = append(rbsp, payload...)
	rbsp = append(rbsp, 0x80) // rbsp_trailing_bits

	return append([]byte{0x06}, rbsp...) // NAL header: type=6 (SEI)
}

func TestExtractor_CEA608_EndToEndFromRealSEINAL(t *testing.T) {
	e := NewExtractor()

	// One triplet: marker_bits=11111, cc_valid=1, cc_type=00 (field 1) ->
	// 0xFC; cc_data_1/2 = 0x14,0x20 (RCL, CC1).
	nal := buildSEINAL([]byte{0xFC, 0x14, 0x20})
	if cues := e.Feed(nal, 100); len(cues) != 0 {
		t.Fatalf("RCL alone should not produce a cue, got %+v", cues)
	}

	nal = buildSEINAL([]byte{0xFC, 'H', 'I'})
	e.Feed(nal, 110)

	nal = buildSEINAL([]byte{0xFC, 0x14, 0x2F}) // EOC
	e.Feed(nal, 200)

	nal = buildSEINAL([]byte{0xFC, 0x14, 0x2C}) // EDM
	cues := e.Feed(nal, 250)
	if len(cues) != 1 {
		t.Fatalf("expected 1 cue after EDM, got %+v", cues)
	}
	if cues[0].Text != "HI" || cues[0].StartPTS != 200 || cues[0].EndPTS != 250 {
		t.Errorf("cue = %+v, want {StartPTS:200 EndPTS:250 Text:HI}", cues[0])
	}
}

func TestExtractor_MalformedSEIProducesNoCues(t *testing.T) {
	e := NewExtractor()
	if cues := e.Feed([]byte{0x06, 0x01, 0x02}, 100); cues != nil {
		t.Errorf("malformed/non-caption SEI should yield nil, got %+v", cues)
	}
}

func TestExtractor_NonSEINALProducesNoCues(t *testing.T) {
	e := NewExtractor()
	// NAL type 1 (non-IDR slice), not type 6 — must be rejected outright.
	if cues := e.Feed([]byte{0x01, 0xAA, 0xBB}, 100); cues != nil {
		t.Errorf("non-SEI NAL should yield nil, got %+v", cues)
	}
}
