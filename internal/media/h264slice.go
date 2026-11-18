package media

// H.264 slice header length, for cbcs encryption (internal/mux/cbcs.go).
// ISO/IEC 23001-7 requires the NAL header and the whole slice header of a
// video NAL to stay in the clear under 'cbcs' so decoders (and FairPlay)
// can parse them; only slice data is encrypted. That needs the header's
// exact length, which means walking every syntax element of H.264 7.3.3,
// reusing the SPS/PPS parsing that POC decoding already does
// (h264poc.go).
//
// Scope: slice NAL types 1 and 5, frame and field pictures, CAVLC and
// CABAC, POC types 0-2, weighted prediction, long-term references.
// Not supported (reported as !ok, and the caller leaves that NAL clear):
// slice groups / FMO (Baseline/Extended only), MVC/SVC extension NALs,
// and parameter sets not seen yet.

// H264ParamSets keeps a stream's SPS and PPS by ID. Not safe for
// concurrent use; each muxer track owns one.
type H264ParamSets struct {
	sps map[int]h264SPS
	pps map[int]h264PPS
}

// NewH264ParamSets returns an empty set.
func NewH264ParamSets() *H264ParamSets {
	return &H264ParamSets{sps: map[int]h264SPS{}, pps: map[int]h264PPS{}}
}

// Add parses an SPS (type 7) or PPS (type 8) NAL (header byte included, no
// start code or length prefix). Other NAL types are ignored.
func (ps *H264ParamSets) Add(nal []byte) {
	if len(nal) == 0 {
		return
	}
	switch nal[0] & 0x1F {
	case 7:
		if id, s, ok := parseH264SPS(nal); ok {
			ps.sps[id] = s
		}
	case 8:
		if id, p, ok := parseH264PPS(nal); ok {
			ps.pps[id] = p
		}
	}
}

// maxSliceHeaderScan bounds how much of a slice is unescaped to find the
// header's end. Long ref-list modification or weight tables still fit.
const maxSliceHeaderScan = 1024

// SliceHeaderSize returns how many bytes of nal (as stored, emulation
// prevention bytes included) the NAL header plus slice header occupy,
// rounded up to a whole byte. ok is false when it can't be determined.
func (ps *H264ParamSets) SliceHeaderSize(nal []byte) (n int, ok bool) {
	bits, head, _, ok := ps.sliceHeaderBits(nal)
	if !ok {
		return 0, false
	}
	return escapedLen(head, (bits+7)/8), true
}

// sliceHeaderBits returns the header length in RBSP bits, the scanned
// (escaped) prefix of nal, and the unescaped RBSP it was read from.
func (ps *H264ParamSets) sliceHeaderBits(nal []byte) (bits int, head, rbsp []byte, ok bool) {
	fail := func() (int, []byte, []byte, bool) { return 0, nil, nil, false }
	if len(nal) < 2 {
		return fail()
	}
	nalType := nal[0] & 0x1F
	if nalType != 1 && nalType != 5 {
		return fail()
	}
	head = nal
	if len(head) > maxSliceHeaderScan {
		head = head[:maxSliceHeaderScan]
	}
	rbsp = unescapeRBSP(head)
	br := newBitReader(rbsp)
	br.readBits(8)
	refIDC := int(nal[0]>>5) & 3
	idr := nalType == 5

	br.readUE() // first_mb_in_slice
	sliceType := br.readUE() % 5
	pps, okP := ps.pps[br.readUE()]
	if !okP || !pps.full {
		return fail()
	}
	sps, okS := ps.sps[pps.spsID]
	if !okS {
		return fail()
	}
	const (
		sliceP, sliceB, sliceI, sliceSP, sliceSI = 0, 1, 2, 3, 4
	)
	if sps.separateColourPlane {
		br.readBits(2) // colour_plane_id
	}
	br.readBits(sps.log2MaxFrameNum) // frame_num
	fieldPic := false
	if !sps.frameMbsOnly {
		fieldPic = br.readBits(1) == 1
		if fieldPic {
			br.readBits(1) // bottom_field_flag
		}
	}
	if idr {
		br.readUE() // idr_pic_id
	}
	switch sps.pocType {
	case 0:
		br.readBits(sps.log2MaxPocLsb)
		if pps.bottomFieldPicOrderInPOC && !fieldPic {
			br.readSE()
		}
	case 1:
		if !sps.deltaPicOrderAlwaysZero {
			br.readSE()
			if pps.bottomFieldPicOrderInPOC && !fieldPic {
				br.readSE()
			}
		}
	}
	if pps.redundantPicCntPresent {
		br.readUE()
	}
	if sliceType == sliceB {
		br.readBits(1) // direct_spatial_mv_pred_flag
	}
	numL0, numL1 := pps.numRefIdxL0Default, pps.numRefIdxL1Default
	if sliceType == sliceP || sliceType == sliceSP || sliceType == sliceB {
		if br.readBits(1) == 1 { // num_ref_idx_active_override_flag
			numL0 = br.readUE() + 1
			if sliceType == sliceB {
				numL1 = br.readUE() + 1
			}
		}
	}
	if numL0 > 32 || numL1 > 32 {
		return fail()
	}
	// ref_pic_list_modification
	refListMod := func() {
		if br.readBits(1) == 1 {
			for i := 0; i < 100 && !br.err; i++ {
				idc := br.readUE()
				if idc == 3 {
					return
				}
				if idc > 5 {
					br.err = true
					return
				}
				br.readUE() // abs_diff_pic_num_minus1 / long_term_pic_num / abs_diff_view_idx
			}
			br.err = true
		}
	}
	if sliceType != sliceI && sliceType != sliceSI {
		refListMod()
		if sliceType == sliceB {
			refListMod()
		}
	}
	if (pps.weightedPred && (sliceType == sliceP || sliceType == sliceSP)) ||
		(pps.weightedBipredIDC == 1 && sliceType == sliceB) {
		chromaArrayType := sps.chromaFormatIDC
		if sps.separateColourPlane {
			chromaArrayType = 0
		}
		br.readUE() // luma_log2_weight_denom
		if chromaArrayType != 0 {
			br.readUE() // chroma_log2_weight_denom
		}
		weights := func(n int) {
			for i := 0; i < n && !br.err; i++ {
				if br.readBits(1) == 1 {
					br.readSE()
					br.readSE()
				}
				if chromaArrayType != 0 && br.readBits(1) == 1 {
					for j := 0; j < 2; j++ {
						br.readSE()
						br.readSE()
					}
				}
			}
		}
		weights(numL0)
		if sliceType == sliceB {
			weights(numL1)
		}
	}
	if refIDC != 0 { // dec_ref_pic_marking
		if idr {
			br.readBits(2) // no_output_of_prior_pics_flag, long_term_reference_flag
		} else if br.readBits(1) == 1 { // adaptive_ref_pic_marking_mode_flag
			for i := 0; ; i++ {
				if i >= 100 || br.err {
					return fail()
				}
				mmco := br.readUE()
				if mmco == 0 {
					break
				}
				if mmco > 6 {
					return fail()
				}
				if mmco == 1 || mmco == 3 {
					br.readUE()
				}
				if mmco == 2 {
					br.readUE()
				}
				if mmco == 3 || mmco == 6 {
					br.readUE()
				}
				if mmco == 4 {
					br.readUE()
				}
			}
		}
	}
	if pps.cabac && sliceType != sliceI && sliceType != sliceSI {
		br.readUE() // cabac_init_idc
	}
	br.readSE() // slice_qp_delta
	if sliceType == sliceSP || sliceType == sliceSI {
		if sliceType == sliceSP {
			br.readBits(1) // sp_for_switch_flag
		}
		br.readSE() // slice_qs_delta
	}
	if pps.deblockingControl {
		if br.readUE() != 1 { // disable_deblocking_filter_idc
			br.readSE()
			br.readSE()
		}
	}
	if br.err {
		return fail()
	}
	return br.pos, head, rbsp, true
}

// escapedLen maps a length in unescaped RBSP bytes back to the number of
// stored bytes, counting the emulation-prevention bytes unescapeRBSP drops.
func escapedLen(b []byte, rbspBytes int) int {
	zeros, kept := 0, 0
	for i, c := range b {
		if kept >= rbspBytes {
			return i
		}
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		kept++
	}
	return len(b)
}
