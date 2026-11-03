package media

import (
	"testing"
)

// bitWriter builds H.264 parameter sets and slice headers for tests.
type bitWriter struct {
	b    []byte
	nbit int
}

func (w *bitWriter) u(n int, v uint32) {
	for i := n - 1; i >= 0; i-- {
		if w.nbit%8 == 0 {
			w.b = append(w.b, 0)
		}
		if v>>uint(i)&1 == 1 {
			w.b[len(w.b)-1] |= 1 << uint(7-w.nbit%8)
		}
		w.nbit++
	}
}

func (w *bitWriter) ue(v int) {
	v++
	n := 0
	for t := v; t > 1; t >>= 1 {
		n++
	}
	w.u(n, 0)
	w.u(n+1, uint32(v))
}

func (w *bitWriter) se(v int) {
	if v > 0 {
		w.ue(2*v - 1)
	} else {
		w.ue(-2 * v)
	}
}

func (w *bitWriter) bytes() []byte {
	w.u(1, 1) // rbsp_stop_one_bit
	for w.nbit%8 != 0 {
		w.u(1, 0)
	}
	return w.b
}

// testSPS is a High-profile SPS with POC type 0, log2_max_poc_lsb 8, and,
// if reorder >= 0, a VUI bitstream_restriction with max_num_reorder_frames.
func testSPS(reorder int) []byte {
	w := &bitWriter{}
	w.u(8, 0x67)
	w.u(8, 100) // profile_idc High
	w.u(16, 0x001f)
	w.ue(0) // sps id
	w.ue(1) // chroma 4:2:0
	w.ue(0)
	w.ue(0)
	w.u(1, 0)
	w.u(1, 0) // no scaling matrix
	w.ue(0)   // log2_max_frame_num 4
	w.ue(0)   // poc type 0
	w.ue(4)   // log2_max_poc_lsb 8
	w.ue(4)   // max_num_ref_frames
	w.u(1, 0)
	w.ue(79)  // 1280
	w.ue(44)  // 720
	w.u(1, 1) // frame_mbs_only
	w.u(1, 1) // direct_8x8
	w.u(1, 0) // no cropping
	if reorder < 0 {
		w.u(1, 0)
		return w.bytes()
	}
	w.u(1, 1) // vui present
	w.u(1, 0) // aspect
	w.u(1, 0) // overscan
	w.u(1, 0) // video signal
	w.u(1, 0) // chroma loc
	w.u(1, 1) // timing
	w.u(32, 1001)
	w.u(32, 60000)
	w.u(1, 1)
	w.u(1, 0) // nal hrd
	w.u(1, 0) // vcl hrd
	w.u(1, 0) // pic_struct
	w.u(1, 1) // bitstream_restriction
	w.u(1, 1)
	w.ue(2)
	w.ue(1)
	w.ue(16)
	w.ue(16)
	w.ue(reorder)
	w.ue(4)
	return w.bytes()
}

func testPPS() []byte {
	w := &bitWriter{}
	w.u(8, 0x68)
	w.ue(0)
	w.ue(0)
	w.u(1, 1) // CABAC
	w.u(1, 0) // bottom_field_pic_order_in_frame_present
	return w.bytes()
}

// testSlice is a slice header: sliceType 2=I 0=P 1=B.
func testSlice(idr bool, sliceType, firstMB, frameNum, pocLsb int) []byte {
	w := &bitWriter{}
	switch {
	case idr:
		w.u(8, 0x65)
	case sliceType == 1:
		w.u(8, 0x01) // non-reference B
	default:
		w.u(8, 0x41)
	}
	w.ue(firstMB)
	w.ue(sliceType + 5)
	w.ue(0)
	w.u(4, uint32(frameNum&15))
	if idr {
		w.ue(0)
	}
	w.u(8, uint32(pocLsb&255))
	w.u(16, 0xABCD) // rest of the slice
	return w.bytes()
}

// gop is one IBBP… GOP in decode order: display index of each picture,
// its slice type and whether it's a reference.
type gopPic struct {
	disp      int
	sliceType int
}

// ibbpGOP returns n pictures (n-1 divisible by 3) of I P B B P B B … in
// decode order.
func ibbpGOP(n int) []gopPic {
	pics := []gopPic{{0, 2}}
	for p := 3; p < n; p += 3 {
		pics = append(pics, gopPic{p, 0}, gopPic{p - 2, 1}, gopPic{p - 1, 1})
	}
	return pics
}

type pushed struct {
	disp int
	pkt  *Packet
}

// runGOPs pushes gops GOPs of n pictures at 3000 ticks/picture, with the
// source PTS for each picture chosen by srcPTS(gopBase, pic, decodeIdx).
func runGOPs(t *testing.T, r *H264Retimer, gops, n int, srcPTS func(base int64, p gopPic, di int) int64) (in []pushed, out []*Packet) {
	t.Helper()
	frameNum := 0
	for g := 0; g < gops; g++ {
		base := int64(900000 + g*n*3000)
		for di, p := range ibbpGOP(n) {
			if di == 0 {
				frameNum = 0
			}
			pkt := &Packet{Codec: CodecH264, PTS: srcPTS(base, p, di), IsKeyframe: di == 0}
			nal := testSlice(di == 0, p.sliceType, 0, frameNum, 2*p.disp)
			if p.sliceType != 1 {
				frameNum++
			}
			in = append(in, pushed{g*n + p.disp, pkt})
			out = append(out, r.Push(pkt, nal)...)
		}
	}
	out = append(out, r.Flush()...)
	return in, out
}

func checkMonotonic(t *testing.T, out []*Packet) {
	t.Helper()
	for i, p := range out {
		if p.DTS > p.PTS {
			t.Fatalf("packet %d: DTS %d > PTS %d", i, p.DTS, p.PTS)
		}
		if i > 0 && p.DTS <= out[i-1].DTS {
			t.Fatalf("packet %d: DTS %d not after %d", i, p.DTS, out[i-1].DTS)
		}
	}
}

// Correct PTS, no DTS: PTS is kept, DTS is rebuilt increasing and <= PTS.
func TestRetimer_RebuildsDTS(t *testing.T) {
	r := NewH264Retimer(nil)
	r.SetSPS(testSPS(-1))
	r.SetPPS(testPPS())
	in, out := runGOPs(t, r, 3, 31, func(base int64, p gopPic, _ int) int64 { return base + int64(p.disp)*3000 })
	if len(out) != len(in) {
		t.Fatalf("got %d packets, want %d", len(out), len(in))
	}
	checkMonotonic(t, out)
	for i, p := range in {
		if p.pkt != out[i] {
			t.Fatalf("packet %d emitted out of decode order", i)
		}
		if want := int64(900000 + p.disp*3000); p.pkt.PTS != want {
			t.Errorf("picture %d: PTS %d, want %d", p.disp, p.pkt.PTS, want)
		}
	}
}

// The v2.16.6 case: the encoder stamps PTS in decode order, so B-frames
// carry the wrong slot (and with real encoders, duplicates). POC must put
// every picture back at its display slot.
func TestRetimer_RepairsPTSFromPOC(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  func(base int64, p gopPic, di int) int64
	}{
		{"decode-order PTS", func(base int64, _ gopPic, di int) int64 { return base + int64(di)*3000 }},
		{"pairs of duplicate PTS", func(base int64, _ gopPic, di int) int64 { return base + int64(di/2*2)*3000 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewH264Retimer(nil)
			r.SetSPS(testSPS(1))
			r.SetPPS(testPPS())
			in, out := runGOPs(t, r, 2, 31, tc.src)
			if len(out) != len(in) {
				t.Fatalf("got %d packets, want %d", len(out), len(in))
			}
			// Display order by PTS must match display order by POC.
			byDisp := map[int]int64{}
			for _, p := range in {
				byDisp[p.disp] = p.pkt.PTS
			}
			bad := 0
			for d := 1; d < len(in); d++ {
				if byDisp[d] < byDisp[d-1] {
					bad++
				}
			}
			if bad > 0 {
				t.Errorf("%d pictures out of display order after retiming", bad)
			}
			for i := 1; i < len(out); i++ {
				if out[i].DTS < out[i-1].DTS || out[i].DTS > out[i].PTS {
					t.Fatalf("packet %d: DTS %d (prev %d) PTS %d", i, out[i].DTS, out[i-1].DTS, out[i].PTS)
				}
			}
		})
	}
}

// An SPS that claims no reordering on a stream with B-frames: D grows and
// the stream still comes out valid.
func TestRetimer_GrowsDepth(t *testing.T) {
	r := NewH264Retimer(nil)
	r.SetSPS(testSPS(0))
	r.SetPPS(testPPS())
	_, out := runGOPs(t, r, 3, 31, func(base int64, p gopPic, _ int) int64 { return base + int64(p.disp)*3000 })
	if r.Depth() < 1 {
		t.Fatalf("depth %d, want grown to >= 1", r.Depth())
	}
	for i := 1; i < len(out); i++ {
		if out[i].DTS < out[i-1].DTS || out[i].DTS > out[i].PTS {
			t.Fatalf("packet %d: DTS %d (prev %d) PTS %d", i, out[i].DTS, out[i-1].DTS, out[i].PTS)
		}
	}
}

// Slices after the first of a picture share its timestamps.
func TestRetimer_MultiSlice(t *testing.T) {
	r := NewH264Retimer(nil)
	r.SetSPS(testSPS(1))
	r.SetPPS(testPPS())
	var out []*Packet
	var all []*Packet
	frameNum := 0
	for di, p := range ibbpGOP(13) {
		for s := 0; s < 3; s++ {
			pkt := &Packet{Codec: CodecH264, PTS: 900000 + int64(p.disp)*3000}
			all = append(all, pkt)
			out = append(out, r.Push(pkt, testSlice(di == 0, p.sliceType, s*100, frameNum, 2*p.disp))...)
		}
		if p.sliceType != 1 {
			frameNum++
		}
	}
	out = append(out, r.Flush()...)
	if len(out) != len(all) {
		t.Fatalf("got %d packets, want %d", len(out), len(all))
	}
	for i := 0; i < len(all); i += 3 {
		if all[i].PTS != all[i+1].PTS || all[i].PTS != all[i+2].PTS || all[i].DTS != all[i+2].DTS {
			t.Fatalf("slices of picture %d have different timestamps", i/3)
		}
		if out[i] != all[i] || out[i+2] != all[i+2] {
			t.Fatalf("slices out of order at %d", i)
		}
	}
}

// Without SPS/PPS (POC unknown) pictures are ordered by source PTS: a
// correct-PTS stream still gets a valid DTS.
func TestRetimer_NoParameterSets(t *testing.T) {
	r := NewH264Retimer(nil)
	in, out := runGOPs(t, r, 2, 31, func(base int64, p gopPic, _ int) int64 { return base + int64(p.disp)*3000 })
	if len(out) != len(in) {
		t.Fatalf("got %d packets, want %d", len(out), len(in))
	}
	checkMonotonic(t, out)
}

func TestParseH264SPS_Reorder(t *testing.T) {
	for _, want := range []int{-1, 0, 2} {
		_, s, ok := parseH264SPS(testSPS(want))
		if !ok || s.maxNumReorderFrames != want || s.log2MaxPocLsb != 8 || s.pocType != 0 {
			t.Errorf("reorder %d: ok=%v %+v", want, ok, s)
		}
	}
}
