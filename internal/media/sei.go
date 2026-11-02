package media

// HasCEA608708 reports whether nal — a single H.264 SEI NAL unit (type 6,
// the 1-byte NAL header included, no Annex-B start code, emulation-
// prevention bytes NOT yet removed) — carries a CEA-608/708 closed-caption
// payload: an SEI message of payload_type=4 ("user data registered by
// Rec. ITU-T T.35") whose first payload byte (itu_t_t35_country_code) is
// 0xB5 (ANSI/SCTE — the country code ATSC/CEA-608/708 always uses). This
// is the standard way real broadcast/professional encoders signal closed
// captions in an H.264 elementary stream; not every SEI NAL carries
// captions (buffering_period and pic_timing SEI are far more common and
// unrelated), so this checks the actual payload_type/country_code rather
// than just "an SEI NAL is present."
func HasCEA608708(nal []byte) bool {
	_, ok := extractCEAUserData(nal)
	return ok
}

// UsesCEA708DTVCCService reports whether the CEA-608/708 user data in nal
// (see HasCEA608708) actually carries CEA-708 DTVCC service-block data
// (cc_type 2 "DTVCC_PACKET_DATA" or 3 "DTVCC_PACKET_START", per CEA-708/
// ATSC A/53 Part 4's cc_data()) rather than plain legacy CEA-608 line-21
// field pairs (cc_type 0 "NTSC_CC_FIELD_1" or 1 "NTSC_CC_FIELD_2").
//
// This distinction matters because HLS's #EXT-X-MEDIA INSTREAM-ID
// attribute uses two different naming conventions depending on which of
// these a stream actually carries: "CC1".."CC4" for legacy 608 field
// data, "SERVICE1".."SERVICE63" for 708 services. Real modern encoders
// commonly send 708 DTVCC service data (not plain 608) even though the
// SEI wrapper format is identical either way — signaling it as CC1 when
// it's really 708 makes a player correctly look for 608 field data,
// correctly find none, and render nothing, even though real caption data
// is present in the stream (confirmed live: a real encoder's 708 stream
// wrongly advertised as CC1 produced zero output from both ffmpeg's own
// eia_608 decoder and a browser player, despite the caption bytes
// themselves being intact and well-formed 708 data).
//
// false for a payload that isn't real cc_data() at all (i.e. doesn't use
// the ATSC A/53 "GA94" + user_data_type_code=3 framing every real
// broadcast encoder uses — HasCEA608708 itself doesn't require that
// framing, only the country code, so it can be true here while this
// still returns false for a non-ATSC or malformed payload).
func UsesCEA708DTVCCService(nal []byte) bool {
	triplets, ok := ExtractCCData(nal)
	if !ok {
		return false
	}
	for i := 0; i*3+3 <= len(triplets); i++ {
		b0 := triplets[i*3]
		ccValid := (b0 >> 2) & 0x01
		ccType := b0 & 0x03
		if ccValid == 1 && (ccType == 2 || ccType == 3) {
			return true
		}
	}
	return false
}

// ExtractCCData returns nal's raw cc_data() triplets — each 3 bytes:
// (marker_bits(5)+cc_valid(1)+cc_type(2)), cc_data_1, cc_data_2 — if nal
// carries a real ATSC A/53 cc_data() payload (the "GA94" + cc_data()
// framing every real broadcast encoder uses), or ok=false otherwise.
//
// This is the actual character/command byte data that HasCEA608708/
// UsesCEA708DTVCCService deliberately never return (they only classify
// presence/type via each triplet's first byte) — added for
// internal/captiondecode, which needs cc_data_1/cc_data_2 to decode real
// caption text rather than just detect that captions exist.
func ExtractCCData(nal []byte) (triplets []byte, ok bool) {
	payload, ok := extractCEAUserData(nal)
	if !ok || len(payload) < 8 {
		return nil, false
	}
	// ATSC1_data(): itu_t_t35_provider_code(2) + user_identifier(4,
	// "GA94") + user_data_type_code(1), then cc_data().
	provider := int(payload[0])<<8 | int(payload[1])
	if provider != 0x0031 { // ATSC
		return nil, false
	}
	if string(payload[2:6]) != "GA94" {
		return nil, false
	}
	if payload[6] != 0x03 { // user_data_type_code: cc_data()
		return nil, false
	}

	// cc_data(): 1 byte (reserved(1)+process_cc_data_flag(1)+zero_bit(1)+
	// cc_count(5)), 1 byte reserved/em_data (0xFF), then cc_count
	// triplets of (marker_bits(5)+cc_valid(1)+cc_type(2), cc_data_1,
	// cc_data_2).
	rest := payload[7:]
	if len(rest) < 2 {
		return nil, false
	}
	ccCount := int(rest[0] & 0x1F)
	need := ccCount * 3
	all := rest[2:]
	if len(all) < need {
		need = (len(all) / 3) * 3 // truncated payload — take whatever complete triplets exist
	}
	return all[:need], true
}

// extractCEAUserData walks nal's SEI message list (see HasCEA608708) and
// returns the payload bytes immediately following itu_t_t35_country_code
// (0xB5) of its first payload_type=4 message, or ok=false if there is no
// such message. Shared by HasCEA608708 and UsesCEA708DTVCCService so the
// message-list walk (RBSP de-escaping, payload_type/payload_size parsing)
// lives in exactly one place.
func extractCEAUserData(nal []byte) (payload []byte, ok bool) {
	if len(nal) < 2 || nal[0]&0x1F != 6 {
		return nil, false
	}
	rbsp := removeEmulationPrevention(nal[1:])

	i := 0
	for i < len(rbsp) {
		payloadType := 0
		for i < len(rbsp) && rbsp[i] == 0xFF {
			payloadType += 255
			i++
		}
		if i >= len(rbsp) {
			return nil, false
		}
		payloadType += int(rbsp[i])
		i++

		payloadSize := 0
		for i < len(rbsp) && rbsp[i] == 0xFF {
			payloadSize += 255
			i++
		}
		if i >= len(rbsp) {
			return nil, false
		}
		payloadSize += int(rbsp[i])
		i++

		if i+payloadSize > len(rbsp) {
			return nil, false // malformed / truncated — stop rather than read out of bounds
		}
		if payloadType == 4 && payloadSize >= 1 && rbsp[i] == 0xB5 {
			return rbsp[i+1 : i+payloadSize], true
		}
		i += payloadSize

		// rbsp_trailing_bits (a single 0x80 byte, or nothing left) marks
		// the end of the SEI message list.
		if i < len(rbsp) && rbsp[i] == 0x80 {
			break
		}
	}
	return nil, false
}

// removeEmulationPrevention (H.264/H.265 Annex-B RBSP de-escaping) is
// already defined in hevc_sps.go — reused here as-is.
