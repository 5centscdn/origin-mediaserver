package media

// HEVCProfileTierLevel holds the profile/tier/level fields extracted from
// an HEVC SPS's profile_tier_level() — the fields RFC 6381's hvc1 codec
// string is built from (see HEVCCodecString).
type HEVCProfileTierLevel struct {
	ProfileSpace  uint8
	TierFlag      bool
	ProfileIDC    uint8
	ProfileCompat uint32
	LevelIDC      uint8
}

// ParseHEVCSPS extracts coded picture width/height and profile/tier/level
// from a raw HEVC (H.265) SPS NAL unit (2-byte NAL header included, no
// start code). ok is false on any parse error or unsupported edge case.
//
// Unlike ParseSPSResolution (H.264), this cannot skip emulation-prevention
// removal: HEVC's profile_tier_level() is dominated by long runs of
// reserved zero bits (general_reserved_zero_43bits alone is more than 5
// bytes of zeros), which very commonly produces literal 0x000000/0x000001/
// 0x000002/0x000003 sequences in the RBSP — real encoders (verified against
// libx265's actual output) routinely emit 0x03 emulation-prevention bytes
// right in the middle of this exact field. Skipping de-emulation here
// (as H.264's parser gets away with, since its early fields rarely
// contain forced zero runs long enough to trigger it) silently misreads
// every bit from that point on.
func ParseHEVCSPS(sps []byte) (width, height int, ptl HEVCProfileTierLevel, ok bool) {
	sps = removeEmulationPrevention(sps)
	if len(sps) < 13 {
		return 0, 0, ptl, false
	}
	br := newBitReader(sps)
	_ = br.readBits(16) // NAL header (2 bytes: forbidden_zero_bit + nal_unit_type + nuh_layer_id + nuh_temporal_id_plus1)

	_ = br.readBits(4) // sps_video_parameter_set_id
	maxSubLayersMinus1 := int(br.readBits(3))
	_ = br.readBits(1) // sps_temporal_id_nesting_flag

	if maxSubLayersMinus1 != 0 {
		return 0, 0, ptl, false // see doc comment
	}

	// profile_tier_level(profilePresentFlag=1, maxNumSubLayersMinus1=0):
	// fixed 96-bit "general" block, no sub-layer arrays follow.
	ptl.ProfileSpace = uint8(br.readBits(2))
	ptl.TierFlag = br.readBits(1) == 1
	ptl.ProfileIDC = uint8(br.readBits(5))
	ptl.ProfileCompat = br.readBits(32)
	_ = br.readBits(4)  // general_progressive/interlaced/non_packed/frame_only_constraint_flag
	_ = br.readBits(43) // general_reserved_zero_43bits / profile-specific constraint flags
	_ = br.readBits(1)  // general_inbld_flag (or reserved, depending on spec version)
	ptl.LevelIDC = uint8(br.readBits(8))

	_ = br.readUE() // sps_seq_parameter_set_id
	chromaFormatIDC := br.readUE()
	if chromaFormatIDC == 3 {
		_ = br.readBits(1) // separate_colour_plane_flag
	}
	width = br.readUE()
	height = br.readUE()

	if br.readBits(1) == 1 { // conformance_window_flag
		confWinLeft := br.readUE()
		confWinRight := br.readUE()
		confWinTop := br.readUE()
		confWinBottom := br.readUE()

		// SubWidthC/SubHeightC per the chroma format table — 4:2:0 halves
		// both axes, 4:2:2 halves only width, monochrome/4:4:4 don't scale.
		subWidthC, subHeightC := 1, 1
		switch chromaFormatIDC {
		case 1: // 4:2:0
			subWidthC, subHeightC = 2, 2
		case 2: // 4:2:2
			subWidthC = 2
		}
		width -= (confWinLeft + confWinRight) * subWidthC
		height -= (confWinTop + confWinBottom) * subHeightC
	}

	if br.err {
		return 0, 0, HEVCProfileTierLevel{}, false
	}
	return width, height, ptl, true
}

// removeEmulationPrevention strips Annex-B emulation-prevention bytes: a
// real encoder inserts a 0x03 after any "00 00" pair inside RBSP data
// whenever the following byte would otherwise be 0x00-0x03 (so a genuine
// start code can never appear mid-NAL) — a real parser must reverse that
// before interpreting the bits as anything meaningful.
func removeEmulationPrevention(data []byte) []byte {
	out := make([]byte, 0, len(data))
	zeroRun := 0
	for _, b := range data {
		if zeroRun >= 2 && b == 0x03 {
			zeroRun = 0
			continue // drop the emulation-prevention byte itself
		}
		out = append(out, b)
		if b == 0 {
			zeroRun++
		} else {
			zeroRun = 0
		}
	}
	return out
}
