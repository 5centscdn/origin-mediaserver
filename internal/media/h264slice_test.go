package media

import (
	"os"
	"testing"
)

// tsVideoNALs pulls the H.264 elementary stream out of an MPEG-TS file
// (the first PID carrying a video PES) and splits it into NALs. Just
// enough TS parsing for a fixture; the real demuxer is ingest/mpegts.
func tsVideoNALs(t *testing.T, path string) [][]byte {
	t.Helper()
	ts, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var es []byte
	videoPID := -1
	for off := 0; off+188 <= len(ts); off += 188 {
		p := ts[off : off+188]
		if p[0] != 0x47 {
			t.Fatal("lost TS sync")
		}
		pid := int(p[1]&0x1F)<<8 | int(p[2])
		pusi := p[1]&0x40 != 0
		payload := p[4:]
		if afc := p[3] >> 4 & 3; afc == 2 {
			continue
		} else if afc == 3 {
			payload = p[5+int(p[4]):]
		}
		if pusi && len(payload) > 9 && payload[0] == 0 && payload[1] == 0 && payload[2] == 1 {
			if videoPID < 0 && payload[3]&0xF0 == 0xE0 {
				videoPID = pid
			}
			if pid != videoPID {
				continue
			}
			payload = payload[9+int(payload[8]):]
		} else if pid != videoPID {
			continue
		}
		es = append(es, payload...)
	}
	var nals [][]byte
	start := -1
	for i := 0; i+3 <= len(es); i++ {
		if es[i] == 0 && es[i+1] == 0 && es[i+2] == 1 {
			if start >= 0 {
				end := i
				for end > start && es[end-1] == 0 {
					end--
				}
				nals = append(nals, es[start:end])
			}
			start = i + 3
			i += 2
		}
	}
	if start >= 0 {
		nals = append(nals, es[start:])
	}
	return nals
}

// TestSliceHeaderSize_RealX264 checks the header end on real x264 output
// (CABAC, B-pyramid, weighted P prediction): CABAC slice data starts with
// cabac_alignment_one_bit until a byte boundary, so every bit between the
// parsed header end and the next byte must be 1. A parser that's off by
// even one syntax element fails that on some slice.
func TestSliceHeaderSize_RealX264(t *testing.T) {
	nals := tsVideoNALs(t, "../ingest/mpegts/testdata/x264_bframes.ts")
	ps := NewH264ParamSets()
	slices, types := 0, map[int]int{}
	for _, nal := range nals {
		switch nal[0] & 0x1F {
		case 7, 8:
			ps.Add(nal)
			continue
		case 1, 5:
		default:
			continue
		}
		bits, _, rbsp, ok := ps.sliceHeaderBits(nal)
		if !ok {
			t.Fatalf("slice %d: header not parsed", slices)
		}
		pps := ps.pps[0]
		if !pps.cabac {
			t.Fatal("fixture expected to be CABAC")
		}
		for b := bits; b%8 != 0; b++ {
			if rbsp[b/8]>>(7-uint(b%8))&1 != 1 {
				t.Fatalf("slice %d: bit %d after header end %d is 0 (misparsed header)", slices, b, bits)
			}
		}
		n, ok := ps.SliceHeaderSize(nal)
		if !ok || n < 2 || n >= len(nal) {
			t.Fatalf("slice %d: size %d of %d", slices, n, len(nal))
		}
		// Slice type count, so the test proves it covered I, P and B.
		br := newBitReader(rbsp)
		br.readBits(8)
		br.readUE()
		types[br.readUE()%5]++
		slices++
	}
	if slices == 0 || types[0] == 0 || types[1] == 0 || types[2] == 0 {
		t.Fatalf("coverage: %d slices, types %v", slices, types)
	}
}

func TestSliceHeaderSize_UnknownPPS(t *testing.T) {
	ps := NewH264ParamSets()
	if _, ok := ps.SliceHeaderSize([]byte{0x65, 0x88, 0x84, 0x00}); ok {
		t.Error("parsed a slice without its PPS")
	}
	if _, ok := ps.SliceHeaderSize([]byte{0x06, 0x05}); ok {
		t.Error("SEI treated as a slice")
	}
}

func TestEscapedLen(t *testing.T) {
	b := []byte{0x00, 0x00, 0x03, 0x01, 0xAA}
	// RBSP is 00 00 01 AA: 3 RBSP bytes span 4 stored bytes.
	if got := escapedLen(b, 3); got != 4 {
		t.Errorf("escapedLen=%d want 4", got)
	}
	if got := escapedLen(b, 2); got != 2 {
		t.Errorf("escapedLen=%d want 2", got)
	}
}
