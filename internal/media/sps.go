package media

import "fmt"

// H264CodecString returns the RFC 6381 codec string for H.264, e.g. "avc1.64002a".
func H264CodecString(ci *CodecInfo) string {
	if ci == nil {
		return "avc1.640028"
	}
	return fmt.Sprintf("avc1.%02x%02x%02x", ci.Profile, ci.ProfileCompatibility, ci.Level)
}

// AACCodecString returns the RFC 6381 codec string for AAC-LC: "mp4a.40.2".
func AACCodecString(_ *CodecInfo) string {
	return "mp4a.40.2"
}

// AudioCodecString returns the RFC 6381 codec string for whatever audio
// codec ci actually describes — AAC is by far the common case (and every
// existing caller besides internal/dash's DASH packager only ever handles
// AAC), but a stream with real MP2 CodecInfo (see
// internal/ingest/mpegts/mp2.go) must advertise "mp4a.6B" (ISO/IEC 14496-1
// Table 5's objectTypeIndication for MPEG-1 Audio — see
// mux.MPEG1AudioObjectType), not a misleading AAC string.
func AudioCodecString(ci *CodecInfo) string {
	if ci != nil && ci.AudioCodec == CodecMP2 {
		return "mp4a.6B"
	}
	if ci != nil && ci.AudioCodec == CodecMP3 {
		return "mp4a.40.34" // the HLS spec's MP3 string, as hls.js and Apple expect
	}
	return AACCodecString(ci)
}

// HEVCCodecString returns the RFC 6381 codec string for HEVC, e.g.
// "hvc1.1.6.L93". The profile_compatibility_flags segment is bit-reversed
// per the spec's own (historical) convention; the trailing
// constraint-flags segment is omitted rather than guessed at — this
// package only extracts profile/tier/level from the SPS (see
// ParseHEVCSPS), not the underlying per-byte constraint indicators, and
// RFC 6381's grammar allows omitting an all-zero trailing segment.
func HEVCCodecString(ci *CodecInfo) string {
	if ci == nil {
		return "hvc1.1.6.L93" // Main profile, Level 3.1 — a reasonable generic default
	}
	spacePrefix := ""
	switch ci.HEVCProfileSpace {
	case 1:
		spacePrefix = "A"
	case 2:
		spacePrefix = "B"
	case 3:
		spacePrefix = "C"
	}
	tier := "L"
	if ci.HEVCTierFlag {
		tier = "H"
	}
	return fmt.Sprintf("hvc1.%s%d.%x.%s%d", spacePrefix, ci.HEVCProfileIDC, reverseBits32(ci.HEVCProfileCompat), tier, ci.HEVCLevelIDC)
}

// reverseBits32 reverses the bit order of a 32-bit value — RFC 6381's HEVC
// codec string encodes profile_compatibility_flags this way "for historic
// reasons."
func reverseBits32(v uint32) uint32 {
	var r uint32
	for i := 0; i < 32; i++ {
		r = (r << 1) | (v & 1)
		v >>= 1
	}
	return r
}

// ParseSPSResolution extracts coded picture width and height from a raw H.264
// SPS NAL unit (bytes starting at the NAL header byte, WITHOUT start code).
// Returns 0,0 on any parse error.
func ParseSPSResolution(sps []byte) (width, height int) {
	if len(sps) < 4 {
		return 0, 0
	}
	br := newBitReader(sps)
	_ = br.readBits(8) // NAL header byte (nal_ref_idc + nal_unit_type)

	profileIDC := int(br.readBits(8))
	_ = br.readBits(8) // constraint flags
	_ = br.readBits(8) // level_idc
	_ = br.readUE()    // seq_parameter_set_id

	if profileIDC == 100 || profileIDC == 110 || profileIDC == 122 ||
		profileIDC == 244 || profileIDC == 44 || profileIDC == 83 ||
		profileIDC == 86 || profileIDC == 118 || profileIDC == 128 {
		chromaFmt := br.readUE()
		if chromaFmt == 3 {
			_ = br.readBits(1) // separate_colour_plane_flag
		}
		_ = br.readUE()          // bit_depth_luma_minus8
		_ = br.readUE()          // bit_depth_chroma_minus8
		_ = br.readBits(1)       // qpprime_y_zero_transform_bypass_flag
		if br.readBits(1) == 1 { // seq_scaling_matrix_present_flag
			count := 8
			if chromaFmt == 3 {
				count = 12
			}
			for i := 0; i < count; i++ {
				if br.readBits(1) == 1 { // seq_scaling_list_present_flag[i]
					size := 16
					if i >= 6 {
						size = 64
					}
					last, next := 8, 8
					for j := 0; j < size; j++ {
						if next != 0 {
							delta := br.readSE()
							next = (last + delta + 256) % 256
						}
						if next != 0 {
							last = next
						}
					}
				}
			}
		}
	}

	_ = br.readUE() // log2_max_frame_num_minus4
	pocType := br.readUE()
	switch pocType {
	case 0:
		_ = br.readUE() // log2_max_pic_order_cnt_lsb_minus4
	case 1:
		_ = br.readBits(1) // delta_pic_order_always_zero_flag
		_ = br.readSE()    // offset_for_non_ref_pic
		_ = br.readSE()    // offset_for_top_to_bottom_field
		n := br.readUE()
		for i := 0; i < n; i++ {
			_ = br.readSE()
		}
	}

	_ = br.readUE()    // max_num_ref_frames
	_ = br.readBits(1) // gaps_in_frame_num_value_allowed_flag

	picWidthMbs := int(br.readUE())
	picHeightMapUnits := int(br.readUE())
	frameMBsOnly := int(br.readBits(1))
	if frameMBsOnly == 0 {
		_ = br.readBits(1) // mb_adaptive_frame_field_flag
	}
	_ = br.readBits(1) // direct_8x8_inference_flag

	cropLeft, cropRight, cropTop, cropBottom := 0, 0, 0, 0
	if br.readBits(1) == 1 { // frame_cropping_flag
		cropLeft = int(br.readUE())
		cropRight = int(br.readUE())
		cropTop = int(br.readUE())
		cropBottom = int(br.readUE())
	}

	if br.err {
		return 0, 0
	}

	mul := 2 - frameMBsOnly
	width = (picWidthMbs+1)*16 - (cropLeft+cropRight)*2
	height = (picHeightMapUnits+1)*16*mul - (cropTop+cropBottom)*(2*mul)
	return width, height
}

// ─── bit reader ───────────────────────────────────────────────────────────────

type bitReader struct {
	data []byte
	pos  int // bit position
	err  bool
}

func newBitReader(b []byte) *bitReader { return &bitReader{data: b} }

func (br *bitReader) readBits(n int) uint32 {
	var v uint32
	for i := 0; i < n; i++ {
		byteIdx := br.pos / 8
		bitIdx := 7 - (br.pos % 8)
		if byteIdx >= len(br.data) {
			br.err = true
			return 0
		}
		v = (v << 1) | uint32((br.data[byteIdx]>>uint(bitIdx))&1)
		br.pos++
	}
	return v
}

// readUE reads an unsigned Exp-Golomb coded integer.
func (br *bitReader) readUE() int {
	leadZeros := 0
	for {
		bit := br.readBits(1)
		if br.err {
			return 0
		}
		if bit != 0 {
			break
		}
		leadZeros++
		if leadZeros > 31 {
			br.err = true
			return 0
		}
	}
	if leadZeros == 0 {
		return 0
	}
	suffix := int(br.readBits(leadZeros))
	return (1 << leadZeros) - 1 + suffix
}

// readSE reads a signed Exp-Golomb coded integer.
func (br *bitReader) readSE() int {
	v := br.readUE()
	if v%2 == 0 {
		return -(v / 2)
	}
	return (v + 1) / 2
}
