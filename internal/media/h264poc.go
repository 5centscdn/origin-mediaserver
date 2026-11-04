package media

// H.264 picture order count (POC): the display order the bitstream itself
// encodes, independent of any container timestamp. H264Retimer uses it to
// put pictures back in display order when a source sends B-frames with only
// one (sometimes duplicated) timestamp per picture. The approach comes from
// the standalone DVR server's flicker fix (v2.16.6, internal/fmp4/poc.go).
//
// Scope: what's needed for POC and nothing more. Supported: pic_order_cnt_type
// 0 (lsb/msb, what x264 and broadcast encoders send) and 2 (output order ==
// decode order), frame pictures including MBAFF. Not supported, reported as
// !ok so callers fall back: pic_order_cnt_type 1, field pictures
// (field_pic_flag = 1), and MMCO 5 (memory_management_control_operation 5
// resets POC mid-stream; the spec's prevPicOrderCnt adjustment for it is
// left out, as no encoder we've seen emits it outside an IDR).

// h264SPS holds the SPS fields needed to parse slice headers and decode POC.
type h264SPS struct {
	profileIDC          int
	separateColourPlane bool
	log2MaxFrameNum     int
	pocType             int
	log2MaxPocLsb       int
	frameMbsOnly        bool
	// maxNumReorderFrames is the VUI bitstream_restriction value: the most
	// pictures that can precede another in decode order and follow it in
	// display order. -1 when the SPS doesn't say.
	maxNumReorderFrames int
}

// h264PPS holds the PPS fields slice-header parsing needs.
type h264PPS struct {
	spsID                    int
	bottomFieldPicOrderInPOC bool // bottom_field_pic_order_in_frame_present_flag
}

// unescapeRBSP removes emulation-prevention bytes (00 00 03 → 00 00) so
// header fields can be read bit by bit.
func unescapeRBSP(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, c)
	}
	return out
}

// parseH264SPS parses an SPS NAL (header byte included, no start code).
func parseH264SPS(nal []byte) (id int, s h264SPS, ok bool) {
	if len(nal) < 4 {
		return 0, s, false
	}
	br := newBitReader(unescapeRBSP(nal))
	br.readBits(8) // NAL header
	s.profileIDC = int(br.readBits(8))
	br.readBits(16) // constraint flags, level_idc
	id = br.readUE()
	switch s.profileIDC {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma := br.readUE()
		if chroma == 3 {
			s.separateColourPlane = br.readBits(1) == 1
		}
		br.readUE()    // bit_depth_luma_minus8
		br.readUE()    // bit_depth_chroma_minus8
		br.readBits(1) // qpprime_y_zero_transform_bypass_flag
		if br.readBits(1) == 1 {
			n := 8
			if chroma == 3 {
				n = 12
			}
			for i := 0; i < n; i++ {
				if br.readBits(1) == 1 {
					size := 16
					if i >= 6 {
						size = 64
					}
					last, next := 8, 8
					for j := 0; j < size && next != 0; j++ {
						next = (last + br.readSE() + 256) % 256
						if next != 0 {
							last = next
						}
					}
				}
			}
		}
	}
	s.log2MaxFrameNum = br.readUE() + 4
	s.pocType = br.readUE()
	switch s.pocType {
	case 0:
		s.log2MaxPocLsb = br.readUE() + 4
	case 1:
		br.readBits(1)
		br.readSE()
		br.readSE()
		for n := br.readUE(); n > 0 && !br.err; n-- {
			br.readSE()
		}
	}
	br.readUE()    // max_num_ref_frames
	br.readBits(1) // gaps_in_frame_num_value_allowed_flag
	br.readUE()    // pic_width_in_mbs_minus1
	br.readUE()    // pic_height_in_map_units_minus1
	s.frameMbsOnly = br.readBits(1) == 1
	if !s.frameMbsOnly {
		br.readBits(1) // mb_adaptive_frame_field_flag
	}
	br.readBits(1) // direct_8x8_inference_flag
	if br.readBits(1) == 1 {
		br.readUE()
		br.readUE()
		br.readUE()
		br.readUE()
	}
	s.maxNumReorderFrames = -1
	if br.readBits(1) == 1 && !br.err { // vui_parameters_present_flag
		s.maxNumReorderFrames = parseVUIReorder(br)
	}
	if br.err || s.log2MaxFrameNum > 16 || s.log2MaxPocLsb > 16 {
		return 0, s, false
	}
	return id, s, true
}

// parseVUIReorder walks the VUI to bitstream_restriction and returns
// max_num_reorder_frames, or -1 if absent or unreadable.
func parseVUIReorder(br *bitReader) int {
	if br.readBits(1) == 1 { // aspect_ratio_info_present_flag
		if br.readBits(8) == 255 {
			br.readBits(32) // sar_width, sar_height
		}
	}
	if br.readBits(1) == 1 { // overscan_info_present_flag
		br.readBits(1)
	}
	if br.readBits(1) == 1 { // video_signal_type_present_flag
		br.readBits(4)
		if br.readBits(1) == 1 {
			br.readBits(24)
		}
	}
	if br.readBits(1) == 1 { // chroma_loc_info_present_flag
		br.readUE()
		br.readUE()
	}
	if br.readBits(1) == 1 { // timing_info_present_flag
		br.readBits(32)
		br.readBits(32)
		br.readBits(1)
	}
	nal := br.readBits(1) == 1
	if nal {
		skipHRD(br)
	}
	vcl := br.readBits(1) == 1
	if vcl {
		skipHRD(br)
	}
	if nal || vcl {
		br.readBits(1) // low_delay_hrd_flag
	}
	br.readBits(1)                     // pic_struct_present_flag
	if br.readBits(1) == 0 || br.err { // bitstream_restriction_flag
		return -1
	}
	br.readBits(1) // motion_vectors_over_pic_boundaries_flag
	for i := 0; i < 4; i++ {
		br.readUE()
	}
	n := br.readUE() // max_num_reorder_frames
	if br.err || n > 16 {
		return -1
	}
	return n
}

func skipHRD(br *bitReader) {
	cnt := br.readUE() + 1
	br.readBits(8) // bit_rate_scale, cpb_size_scale
	for i := 0; i < cnt && i < 32 && !br.err; i++ {
		br.readUE()
		br.readUE()
		br.readBits(1)
	}
	br.readBits(20)
}

// parseH264PPS parses a PPS NAL (header byte included, no start code).
func parseH264PPS(nal []byte) (id int, p h264PPS, ok bool) {
	if len(nal) < 2 {
		return 0, p, false
	}
	br := newBitReader(unescapeRBSP(nal))
	br.readBits(8)
	id = br.readUE()
	p.spsID = br.readUE()
	br.readBits(1) // entropy_coding_mode_flag
	p.bottomFieldPicOrderInPOC = br.readBits(1) == 1
	return id, p, !br.err
}

// h264Slice is the part of a slice header POC decoding needs.
type h264Slice struct {
	firstMB   int
	sliceType int // 0..9; %5 == 2 is I, 1 is B
	idr       bool
	refIDC    int
	frameNum  int
	pocLsb    int
	deltaBot  int
	sps       h264SPS
}

// parseH264SliceHeader reads a slice NAL's header up to the POC fields.
func parseH264SliceHeader(nal []byte, spss map[int]h264SPS, ppss map[int]h264PPS) (h h264Slice, ok bool) {
	if len(nal) < 2 {
		return h, false
	}
	h.refIDC = int(nal[0]>>5) & 3
	h.idr = nal[0]&0x1F == 5
	// The header fits well inside the first 64 bytes; don't unescape the
	// whole slice.
	head := nal
	if len(head) > 64 {
		head = head[:64]
	}
	br := newBitReader(unescapeRBSP(head))
	br.readBits(8)
	h.firstMB = br.readUE()
	h.sliceType = br.readUE()
	pps, okP := ppss[br.readUE()]
	if !okP {
		return h, false
	}
	sps, okS := spss[pps.spsID]
	if !okS {
		return h, false
	}
	h.sps = sps
	if sps.separateColourPlane {
		br.readBits(2)
	}
	h.frameNum = int(br.readBits(sps.log2MaxFrameNum))
	if !sps.frameMbsOnly && br.readBits(1) == 1 {
		return h, false // field picture: not supported
	}
	if h.idr {
		br.readUE() // idr_pic_id
	}
	switch sps.pocType {
	case 0:
		h.pocLsb = int(br.readBits(sps.log2MaxPocLsb))
		if pps.bottomFieldPicOrderInPOC {
			h.deltaBot = br.readSE()
		}
	case 2:
	default:
		return h, false
	}
	return h, !br.err
}

// pocDecoder tracks the state POC decoding carries from picture to picture
// (H.264 8.2.1.1 for type 0, 8.2.1.3 for type 2).
type pocDecoder struct {
	prevMsb, prevLsb int
	// type 2
	prevFrameNum, frameNumOffset int
}

// decode returns h's frame POC (min of top and bottom field POC).
func (d *pocDecoder) decode(h h264Slice) int {
	if h.sps.pocType == 2 {
		maxFrameNum := 1 << h.sps.log2MaxFrameNum
		if h.idr {
			d.frameNumOffset = 0
		} else if d.prevFrameNum > h.frameNum {
			d.frameNumOffset += maxFrameNum
		}
		d.prevFrameNum = h.frameNum
		poc := 2 * (d.frameNumOffset + h.frameNum)
		if h.refIDC == 0 {
			poc--
		}
		return poc
	}

	if h.idr {
		d.prevMsb, d.prevLsb = 0, 0
	}
	maxLsb := 1 << h.sps.log2MaxPocLsb
	msb := d.prevMsb
	switch {
	case h.pocLsb < d.prevLsb && d.prevLsb-h.pocLsb >= maxLsb/2:
		msb += maxLsb
	case h.pocLsb > d.prevLsb && h.pocLsb-d.prevLsb > maxLsb/2:
		msb -= maxLsb
	}
	top := msb + h.pocLsb
	poc := top
	if bot := top + h.deltaBot; bot < poc {
		poc = bot
	}
	if h.refIDC != 0 {
		d.prevMsb, d.prevLsb = msb, h.pocLsb
	}
	return poc
}

// H264FirstMBInSlice returns a slice NAL's first_mb_in_slice (nal without
// start code). 0 starts a new picture; anything else continues the picture
// of the slice before it. ok is false for a NAL too short to tell.
func H264FirstMBInSlice(nal []byte) (int, bool) {
	if len(nal) < 2 {
		return 0, false
	}
	head := nal
	if len(head) > 16 {
		head = head[:16]
	}
	br := newBitReader(unescapeRBSP(head))
	br.readBits(8)
	mb := br.readUE()
	return mb, !br.err
}
