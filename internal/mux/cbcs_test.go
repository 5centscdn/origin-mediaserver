package mux_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"os"
	"testing"

	"github.com/5centscdn/origin-mediaserver/internal/ingest/mpegts"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/mux"
)

// ── tiny ISO BMFF reader ──────────────────────────────────────────────────

type box struct {
	typ  string
	body []byte
	off  int // offset of the box header within the parsed buffer
}

func boxes(t *testing.T, b []byte, base int) []box {
	t.Helper()
	var out []box
	for off := 0; off < len(b); {
		if off+8 > len(b) {
			t.Fatalf("truncated box header at %d", base+off)
		}
		size := int(binary.BigEndian.Uint32(b[off:]))
		if size < 8 || off+size > len(b) {
			t.Fatalf("bad box size %d at %d", size, base+off)
		}
		out = append(out, box{typ: string(b[off+4 : off+8]), body: b[off+8 : off+size], off: base + off})
		off += size
	}
	return out
}

func find(t *testing.T, bs []box, typ string) box {
	t.Helper()
	for _, b := range bs {
		if b.typ == typ {
			return b
		}
	}
	t.Fatalf("no %s box", typ)
	return box{}
}

// path walks container boxes; offsets are relative to the outer buffer.
func path(t *testing.T, buf []byte, types ...string) box {
	t.Helper()
	cur := box{body: buf, off: -8}
	for _, typ := range types {
		cur = find(t, boxes(t, cur.body, cur.off+8), typ)
	}
	return cur
}

// ── fixtures ──────────────────────────────────────────────────────────────

func x264Samples(t *testing.T) (sps, pps []byte, samples []mux.Sample) {
	t.Helper()
	ts, err := os.ReadFile("../ingest/mpegts/testdata/x264_bframes.ts")
	if err != nil {
		t.Fatal(err)
	}
	bus := media.NewBus()
	sub := bus.Subscribe(context.Background())
	defer sub.Close()
	d := mpegts.NewDemuxer(bus)
	if err := d.Run(context.Background(), bytes.NewReader(ts)); err != nil && err.Error() != "EOF" {
		t.Logf("demux: %v", err)
	}
	d.Flush()
	var pending *mux.Sample
	for done := false; !done; {
		select {
		case p := <-sub.Chan():
			if p.Codec != media.CodecH264 || len(p.Data) == 0 {
				continue
			}
			samples, pending = mux.AppendPendingVideoSample(samples, pending, p.DTS, p.PTS, p.IsKeyframe, p.Data)
		default:
			done = true
		}
	}
	// The demuxer moves SPS/PPS into the stream's CodecInfo; take them
	// straight from the TS bytes (both fit in the first video TS packet).
	nalAt := func(typ byte) []byte {
		i := bytes.Index(ts, []byte{0, 0, 1, typ})
		if i < 0 {
			return nil
		}
		nal := ts[i+3:]
		for j := 0; j+2 < len(nal); j++ { // next start code (00 00 00 / 00 00 01)
			if nal[j] == 0 && nal[j+1] == 0 && nal[j+2] <= 1 {
				nal = nal[:j]
				break
			}
		}
		return nal
	}
	sps, pps = nalAt(0x67), nalAt(0x68)
	if len(samples) < 50 || sps == nil || pps == nil {
		t.Fatalf("fixture: %d samples, sps=%v", len(samples), sps != nil)
	}
	return sps, pps, samples
}

func testEnc() *mux.TrackEncryption {
	e := &mux.TrackEncryption{PSSH: [][]byte{fakePSSH()}, Params: media.NewH264ParamSets()}
	copy(e.KID[:], "kid-0123456789ab")
	copy(e.Key[:], "key-0123456789ab")
	copy(e.IV[:], "iv--0123456789ab")
	return e
}

func fakePSSH() []byte {
	b := []byte{0, 0, 0, 32, 'p', 's', 's', 'h', 0, 0, 0, 0}
	b = append(b, []byte("\xed\xef\x8b\xa9\x79\xd6\x4a\xce\xa3\xc8\x27\xdc\xd5\x1d\x21\xed")...)
	return append(b, 0, 0, 0, 0)
}

// ── independent cbcs decryption from the boxes ───────────────────────────

type tencInfo struct {
	crypt, skip int
	kid, iv     []byte
}

func parseTenc(t *testing.T, init []byte, entry string) tencInfo {
	t.Helper()
	stsd := path(t, init, "moov", "trak", "mdia", "minf", "stbl", "stsd")
	// stsd: fullbox(4) + entry_count(4), then the sample entry box.
	ent := find(t, boxes(t, stsd.body[8:], 0), entry)
	hdr := 78 // VisualSampleEntry fields
	if entry == "enca" {
		hdr = 28
	}
	sinf := find(t, boxes(t, ent.body[hdr:], 0), "sinf")
	sb := boxes(t, sinf.body, 0)
	if f := find(t, sb, "frma"); string(f.body) != map[string]string{"encv": "avc1", "enca": "mp4a"}[entry] {
		t.Fatalf("frma %q", f.body)
	}
	if s := find(t, sb, "schm"); string(s.body[4:8]) != "cbcs" {
		t.Fatalf("schm %q", s.body[4:8])
	}
	tenc := find(t, boxes(t, find(t, sb, "schi").body, 0), "tenc").body
	if tenc[0] != 1 || tenc[6] != 1 || tenc[7] != 0 || tenc[24] != 16 {
		t.Fatalf("tenc header % x", tenc[:8])
	}
	return tencInfo{crypt: int(tenc[5] >> 4), skip: int(tenc[5] & 15), kid: tenc[8:24], iv: tenc[25:41]}
}

type subs struct{ clear, prot int }

// decryptSegment decrypts one moof+mdat with key, using only what the
// boxes say (tenc pattern + IV, saio → senc entries, trun sizes).
func decryptSegment(t *testing.T, seg []byte, ti tencInfo, key []byte) [][]byte {
	t.Helper()
	top := boxes(t, seg, 0)
	moof, mdat := find(t, top, "moof"), find(t, top, "mdat")
	traf := find(t, boxes(t, moof.body, moof.off+8), "traf")
	tb := boxes(t, traf.body, traf.off+8)
	trun := find(t, tb, "trun").body
	n := int(binary.BigEndian.Uint32(trun[4:]))
	sizes := make([]int, n)
	for i := 0; i < n; i++ {
		sizes[i] = int(binary.BigEndian.Uint32(trun[12+16*i+4:]))
	}
	saiz, saio := find(t, tb, "saiz").body, find(t, tb, "saio").body
	if int(binary.BigEndian.Uint32(saiz[5:])) != n {
		t.Fatalf("saiz count")
	}
	auxOff := moof.off + int(binary.BigEndian.Uint32(saio[8:]))
	senc := find(t, tb, "senc")
	if want := senc.off + 8 + 8; auxOff != want {
		t.Fatalf("saio points at %d, senc data at %d", auxOff, want)
	}
	aux := seg[auxOff:]
	blk, _ := aes.NewCipher(key)
	data := mdat.body
	var out [][]byte
	for i := 0; i < n; i++ {
		if int(saiz[9+i]) != 2+6*int(binary.BigEndian.Uint16(aux)) {
			t.Fatalf("sample %d: saiz size mismatch", i)
		}
		cnt := int(binary.BigEndian.Uint16(aux))
		var ss []subs
		total := 0
		for j := 0; j < cnt; j++ {
			e := aux[2+6*j:]
			s := subs{int(binary.BigEndian.Uint16(e)), int(binary.BigEndian.Uint32(e[2:]))}
			ss = append(ss, s)
			total += s.clear + s.prot
		}
		aux = aux[2+6*cnt:]
		if total != sizes[i] {
			t.Fatalf("sample %d: subsamples cover %d of %d bytes", i, total, sizes[i])
		}
		s := append([]byte(nil), data[:sizes[i]]...)
		data = data[sizes[i]:]
		pos := 0
		for _, sub := range ss {
			pos += sub.clear
			prot := s[pos : pos+sub.prot]
			pos += sub.prot
			dec := cipher.NewCBCDecrypter(blk, ti.iv)
			full := len(prot) &^ 15
			if ti.crypt == 0 && ti.skip == 0 {
				dec.CryptBlocks(prot[:full], prot[:full])
				continue
			}
			for o := 0; o < full; o += (ti.crypt + ti.skip) * 16 {
				e := o + ti.crypt*16
				if e > full {
					e = full
				}
				dec.CryptBlocks(prot[o:e], prot[o:e])
			}
		}
		out = append(out, s)
	}
	return out
}

// ── tests ─────────────────────────────────────────────────────────────────

func TestCBCSVideoRoundTrip(t *testing.T) {
	sps, pps, samples := x264Samples(t)
	clearTrack := &mux.FMP4Track{Type: mux.TrackVideo, TrackID: 1, Timescale: 90000, Width: 320, Height: 180, SPS: sps, PPS: pps}
	encTrack := *clearTrack
	enc := testEnc()
	encTrack.Encryption = enc

	var init bytes.Buffer
	if err := mux.NewFMP4Muxer(&encTrack).WriteInitSegment(&init); err != nil {
		t.Fatal(err)
	}
	ti := parseTenc(t, init.Bytes(), "encv")
	if ti.crypt != 1 || ti.skip != 9 || !bytes.Equal(ti.kid, enc.KID[:]) || !bytes.Equal(ti.iv, enc.IV[:]) {
		t.Fatalf("tenc %+v", ti)
	}
	pssh := path(t, init.Bytes(), "moov", "pssh")
	if !bytes.Equal(pssh.body, fakePSSH()[8:]) {
		t.Fatal("pssh not copied into moov")
	}

	orig := make([][]byte, len(samples))
	for i, s := range samples {
		orig[i] = append([]byte(nil), s.Data...)
	}
	var seg bytes.Buffer
	if err := mux.NewFMP4Muxer(&encTrack).WriteMediaSegment(&seg, 0, samples, 0); err != nil {
		t.Fatal(err)
	}
	for i, s := range samples {
		if !bytes.Equal(s.Data, orig[i]) {
			t.Fatalf("sample %d: caller's data was modified", i)
		}
	}
	mdat := find(t, boxes(t, seg.Bytes(), 0), "mdat").body
	var all []byte
	for _, o := range orig {
		all = append(all, o...)
	}
	if bytes.Equal(mdat, all) {
		t.Fatal("mdat not encrypted")
	}
	got := decryptSegment(t, seg.Bytes(), ti, enc.Key[:])
	for i := range got {
		if !bytes.Equal(got[i], orig[i]) {
			t.Fatalf("sample %d: decrypted bytes differ", i)
		}
	}
	// A second fragment from the same track (LL-HLS parts) must work too:
	// the constant IV resets per subsample, nothing carries over.
	var seg2 bytes.Buffer
	m := mux.NewFMP4Muxer(&encTrack)
	m.WriteMediaSegment(&seg2, 0, samples[:10], 0)
	seg2.Reset()
	m.WriteMediaSegment(&seg2, 0, samples[10:20], 0)
	for i, d := range decryptSegment(t, seg2.Bytes(), ti, enc.Key[:]) {
		if !bytes.Equal(d, orig[10+i]) {
			t.Fatalf("second fragment sample %d differs", i)
		}
	}
}

func TestCBCSAudioRoundTrip(t *testing.T) {
	tr := &mux.FMP4Track{Type: mux.TrackAudio, TrackID: 2, Timescale: 48000, SampleRate: 48000, Channels: 2,
		AACConfig: []byte{0x11, 0x90}, Encryption: testEnc()}
	var init bytes.Buffer
	if err := mux.NewFMP4Muxer(tr).WriteInitSegment(&init); err != nil {
		t.Fatal(err)
	}
	ti := parseTenc(t, init.Bytes(), "enca")
	if ti.crypt != 0 || ti.skip != 0 {
		t.Fatalf("audio pattern %d:%d, want full-sample 0:0", ti.crypt, ti.skip)
	}
	var samples []mux.Sample
	var orig [][]byte
	for i, n := range []int{7, 16, 200, 371} {
		d := bytes.Repeat([]byte{byte(i + 1)}, n)
		orig = append(orig, append([]byte(nil), d...))
		samples = append(samples, mux.Sample{DTS: int64(i * 1024), PTS: int64(i * 1024), Duration: 1024, Data: d})
	}
	var seg bytes.Buffer
	if err := mux.NewFMP4Muxer(tr).WriteMediaSegment(&seg, 0, samples, 0); err != nil {
		t.Fatal(err)
	}
	mdat := find(t, boxes(t, seg.Bytes(), 0), "mdat").body
	if !bytes.Equal(mdat[:7], orig[0]) {
		t.Error("a sample shorter than a block must stay clear")
	}
	if bytes.Equal(mdat[7:23], orig[1]) {
		t.Error("16-byte sample not encrypted")
	}
	for i, d := range decryptSegment(t, seg.Bytes(), ti, tr.Encryption.Key[:]) {
		if !bytes.Equal(d, orig[i]) {
			t.Fatalf("audio sample %d differs", i)
		}
	}
}

func TestCBCSRefusesUnsupportedCodec(t *testing.T) {
	tr := &mux.FMP4Track{Type: mux.TrackAudio, TrackID: 2, Timescale: 48000, AudioObjectType: mux.MPEG1AudioObjectType, Encryption: testEnc()}
	if err := mux.NewFMP4Muxer(tr).WriteInitSegment(&bytes.Buffer{}); err == nil {
		t.Error("MP2 init accepted with encryption")
	}
	if err := mux.NewFMP4Muxer(tr).WriteMediaSegment(&bytes.Buffer{}, 0, []mux.Sample{{Data: []byte{1}}}, 0); err == nil {
		t.Error("MP2 segment written (would be clear) with encryption set")
	}
}
