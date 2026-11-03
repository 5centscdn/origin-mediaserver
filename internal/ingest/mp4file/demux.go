// Package mp4file plays an MP4 file as a live source: it reads the file's
// sample tables and publishes its frames onto a media.Bus in real time,
// looping forever, with timestamps that keep increasing across loops. The
// failover manager uses it for "mp4" backups (a slate or filler loop shown
// while the primary encoder is down; internal/failover).
//
// Scope: progressive (non-fragmented) MP4/MOV with an H.264 video track
// (avc1/avc3 + avcC) and/or an AAC audio track (mp4a + esds), the files
// ffmpeg, x264, HandBrake and cameras write, moov before or after mdat.
// The first edit-list entry's media_time is honoured, as ffmpeg's demuxer
// does, so B-frame files start at PTS 0. Not supported, reported as an error
// up front: fragmented MP4 (moof), HEVC (hvc1/hev1), and files with neither
// a usable H.264 nor AAC track. Other tracks (subtitles, timecode) are
// ignored. Only the first track of each kind is played.
package mp4file

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// track is one parsed trak: its codec config and sample table.
type track struct {
	video     bool
	timescale uint32
	shift     int64 // edit-list media_time, in timescale units

	// H.264
	lengthSize int
	avcC       []byte
	sps, pps   []byte

	// AAC
	asc []byte

	samples []sample
}

type sample struct {
	off  int64
	size uint32
	dts  int64 // timescale units
	cto  int32 // composition offset
	key  bool
}

// file is a parsed MP4: the tracks Play will publish.
type file struct {
	video, audio *track
}

// box is a parsed box header: payload is [start, end) of the file or buffer.
type box struct {
	typ        string
	start, end int64
}

// readBoxHeader reads the box header at off in r (size limit end).
func readBoxHeader(r io.ReaderAt, off, end int64) (box, error) {
	var h [16]byte
	if _, err := r.ReadAt(h[:8], off); err != nil {
		return box{}, err
	}
	size := int64(binary.BigEndian.Uint32(h[:4]))
	b := box{typ: string(h[4:8]), start: off + 8}
	switch size {
	case 0:
		size = end - off
	case 1:
		if _, err := r.ReadAt(h[8:16], off+8); err != nil {
			return box{}, err
		}
		size = int64(binary.BigEndian.Uint64(h[8:16]))
		b.start = off + 16
	}
	if size < b.start-off || off+size > end {
		return box{}, fmt.Errorf("mp4: box %q at %d has bad size %d", b.typ, off, size)
	}
	b.end = off + size
	return b, nil
}

// children walks the boxes inside [start, end) of buf.
func children(buf []byte, start, end int64) ([]box, error) {
	r := bytesReaderAt(buf)
	var out []box
	for off := start; off+8 <= end; {
		b, err := readBoxHeader(r, off, end)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
		off = b.end
	}
	return out, nil
}

type bytesReaderAt []byte

func (b bytesReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// parse reads the top-level boxes of r (size bytes) and the moov's tracks.
func parse(r io.ReaderAt, size int64) (*file, error) {
	var moov []byte
	for off := int64(0); off+8 <= size; {
		b, err := readBoxHeader(r, off, size)
		if err != nil {
			return nil, err
		}
		switch b.typ {
		case "moov":
			if b.end-b.start > 256<<20 {
				return nil, errors.New("mp4: moov larger than 256 MB")
			}
			moov = make([]byte, b.end-b.start)
			if _, err := r.ReadAt(moov, b.start); err != nil {
				return nil, fmt.Errorf("mp4: read moov: %w", err)
			}
		case "moof":
			return nil, errors.New("mp4: fragmented MP4 is not supported; remux with: ffmpeg -i in.mp4 -c copy -movflags +faststart out.mp4")
		}
		off = b.end
	}
	if moov == nil {
		return nil, errors.New("mp4: no moov box (not an MP4 file?)")
	}

	boxes, err := children(moov, 0, int64(len(moov)))
	if err != nil {
		return nil, err
	}
	f := &file{}
	var unsupported string
	for _, b := range boxes {
		if b.typ != "trak" {
			continue
		}
		t, codec, err := parseTrak(moov[b.start:b.end])
		if err != nil {
			return nil, err
		}
		switch {
		case t == nil:
			if codec != "" {
				unsupported = codec
			}
		case t.video && f.video == nil:
			f.video = t
		case !t.video && f.audio == nil:
			f.audio = t
		}
	}
	if f.video == nil && f.audio == nil {
		if unsupported != "" {
			return nil, fmt.Errorf("mp4: codec %q is not supported (H.264 and AAC only)", unsupported)
		}
		return nil, errors.New("mp4: no H.264 or AAC track")
	}
	for _, t := range []*track{f.video, f.audio} {
		if t != nil && len(t.samples) == 0 {
			return nil, errors.New("mp4: track has no samples (fragmented MP4 is not supported)")
		}
	}
	return f, nil
}

// parseTrak parses one trak. It returns a nil track (and the sample entry
// type, if any) for tracks that aren't H.264 video or AAC audio.
func parseTrak(buf []byte) (*track, string, error) {
	t := &track{}
	var stbl []byte
	var handler string
	walk := func(b []byte) ([]box, error) { return children(b, 0, int64(len(b))) }

	top, err := walk(buf)
	if err != nil {
		return nil, "", err
	}
	for _, b := range top {
		p := buf[b.start:b.end]
		switch b.typ {
		case "edts":
			if eb, err := walk(p); err == nil {
				for _, e := range eb {
					if e.typ == "elst" {
						t.shift = parseElst(p[e.start:e.end])
					}
				}
			}
		case "mdia":
			mb, err := walk(p)
			if err != nil {
				return nil, "", err
			}
			for _, m := range mb {
				q := p[m.start:m.end]
				switch m.typ {
				case "mdhd":
					if len(q) >= 24 && q[0] == 1 {
						t.timescale = binary.BigEndian.Uint32(q[20:24])
					} else if len(q) >= 16 {
						t.timescale = binary.BigEndian.Uint32(q[12:16])
					}
				case "hdlr":
					if len(q) >= 12 {
						handler = string(q[8:12])
					}
				case "minf":
					nb, err := walk(q)
					if err != nil {
						return nil, "", err
					}
					for _, n := range nb {
						if n.typ == "stbl" {
							stbl = q[n.start:n.end]
						}
					}
				}
			}
		}
	}
	if stbl == nil || t.timescale == 0 || (handler != "vide" && handler != "soun") {
		return nil, "", nil
	}
	t.video = handler == "vide"

	sb, err := walk(stbl)
	if err != nil {
		return nil, "", err
	}
	get := func(typ string) []byte {
		for _, b := range sb {
			if b.typ == typ {
				return stbl[b.start:b.end]
			}
		}
		return nil
	}

	codec, err := t.parseStsd(get("stsd"))
	if err != nil || codec != "" {
		return nil, codec, err
	}
	if err := t.buildSamples(get("stts"), get("ctts"), get("stss"), get("stsc"), get("stsz"), get("stco"), get("co64")); err != nil {
		return nil, "", err
	}
	return t, "", nil
}

// parseElst returns the first non-empty edit's media_time (ffmpeg's
// behaviour for the common single-edit file).
func parseElst(p []byte) int64 {
	if len(p) < 8 {
		return 0
	}
	v1 := p[0] == 1
	n := int(binary.BigEndian.Uint32(p[4:8]))
	off := 8
	for i := 0; i < n; i++ {
		var mt int64
		if v1 {
			if off+20 > len(p) {
				return 0
			}
			mt = int64(binary.BigEndian.Uint64(p[off+8 : off+16]))
			off += 20
		} else {
			if off+12 > len(p) {
				return 0
			}
			mt = int64(int32(binary.BigEndian.Uint32(p[off+4 : off+8])))
			off += 12
		}
		if mt >= 0 {
			return mt
		}
	}
	return 0
}

// parseStsd reads the first sample entry. It returns the entry type when
// it isn't one this package plays.
func (t *track) parseStsd(p []byte) (string, error) {
	if len(p) < 16 {
		return "", errors.New("mp4: stsd too short")
	}
	r := bytesReaderAt(p)
	e, err := readBoxHeader(r, 8, int64(len(p)))
	if err != nil {
		return "", err
	}
	entry := p[e.start:e.end]
	switch e.typ {
	case "avc1", "avc3":
		if !t.video || len(entry) < 78 {
			return e.typ, nil
		}
		cb, err := children(entry, 78, int64(len(entry)))
		if err != nil {
			return "", err
		}
		for _, c := range cb {
			if c.typ == "avcC" {
				return "", t.parseAvcC(entry[c.start:c.end])
			}
		}
		return "", errors.New("mp4: avc1 without avcC")
	case "mp4a":
		if t.video || len(entry) < 28 {
			return e.typ, nil
		}
		// QuickTime sound sample description versions 1 and 2 add fields.
		skip := int64(28)
		switch binary.BigEndian.Uint16(entry[8:10]) {
		case 1:
			skip += 16
		case 2:
			skip += 36
		}
		if skip > int64(len(entry)) {
			return "", errors.New("mp4: short mp4a entry")
		}
		cb, err := children(entry, skip, int64(len(entry)))
		if err != nil {
			return "", err
		}
		for _, c := range cb {
			switch c.typ {
			case "esds":
				return "", t.parseEsds(entry[c.start:c.end])
			case "wave": // QuickTime: esds inside wave
				wb, err := children(entry[c.start:c.end], 0, c.end-c.start)
				if err != nil {
					return "", err
				}
				for _, w := range wb {
					if w.typ == "esds" {
						return "", t.parseEsds(entry[c.start:c.end][w.start:w.end])
					}
				}
			}
		}
		return "", errors.New("mp4: mp4a without esds")
	}
	return e.typ, nil
}

func (t *track) parseAvcC(p []byte) error {
	if len(p) < 7 {
		return errors.New("mp4: avcC too short")
	}
	t.avcC = append([]byte(nil), p...)
	t.lengthSize = int(p[4]&3) + 1
	pos := 5
	read := func(n int) [][]byte {
		var out [][]byte
		for i := 0; i < n && pos+2 <= len(p); i++ {
			l := int(binary.BigEndian.Uint16(p[pos:]))
			pos += 2
			if pos+l > len(p) {
				break
			}
			out = append(out, append([]byte(nil), p[pos:pos+l]...))
			pos += l
		}
		return out
	}
	nsps := int(p[pos] & 0x1F)
	pos++
	spss := read(nsps)
	var ppss [][]byte
	if pos < len(p) {
		n := int(p[pos])
		pos++
		ppss = read(n)
	}
	if len(spss) == 0 || len(ppss) == 0 {
		return errors.New("mp4: avcC without SPS/PPS")
	}
	t.sps, t.pps = spss[0], ppss[0]
	return nil
}

// parseEsds extracts the AudioSpecificConfig from an esds box.
func (t *track) parseEsds(p []byte) error {
	if len(p) < 4 {
		return errors.New("mp4: esds too short")
	}
	pos := 4
	desc := func() (tag byte, n int, ok bool) {
		if pos >= len(p) {
			return 0, 0, false
		}
		tag = p[pos]
		pos++
		for i := 0; i < 4 && pos < len(p); i++ {
			b := p[pos]
			pos++
			n = n<<7 | int(b&0x7F)
			if b&0x80 == 0 {
				break
			}
		}
		return tag, n, pos+n <= len(p)
	}
	tag, _, ok := desc()
	if !ok || tag != 0x03 {
		return errors.New("mp4: esds without ES_Descriptor")
	}
	if pos+3 > len(p) {
		return errors.New("mp4: short ES_Descriptor")
	}
	flags := p[pos+2]
	pos += 3
	if flags&0x80 != 0 {
		pos += 2
	}
	if flags&0x40 != 0 && pos < len(p) {
		pos += 1 + int(p[pos])
	}
	if flags&0x20 != 0 {
		pos += 2
	}
	tag, n, ok := desc()
	if !ok || tag != 0x04 || n < 13 {
		return errors.New("mp4: esds without DecoderConfigDescriptor")
	}
	if oti := p[pos]; oti != 0x40 && oti != 0x66 && oti != 0x67 && oti != 0x68 {
		return fmt.Errorf("mp4: audio object type 0x%02x is not AAC", oti)
	}
	pos += 13
	tag, n, ok = desc()
	if !ok || tag != 0x05 || n < 2 {
		return errors.New("mp4: esds without AudioSpecificConfig")
	}
	t.asc = append([]byte(nil), p[pos:pos+n]...)
	return nil
}

// buildSamples expands the stbl tables into one entry per sample.
func (t *track) buildSamples(stts, ctts, stss, stsc, stsz, stco, co64 []byte) error {
	u32 := binary.BigEndian.Uint32
	if len(stsz) < 12 || len(stts) < 8 || len(stsc) < 8 {
		return errors.New("mp4: missing sample tables")
	}
	count := int(u32(stsz[8:12]))
	if count > 50_000_000 {
		return errors.New("mp4: too many samples")
	}
	fixed := u32(stsz[4:8])
	if fixed == 0 && len(stsz) < 12+4*count {
		return errors.New("mp4: short stsz")
	}
	t.samples = make([]sample, count)
	for i := range t.samples {
		if fixed != 0 {
			t.samples[i].size = fixed
		} else {
			t.samples[i].size = u32(stsz[12+4*i:])
		}
	}

	// Timing.
	var dts int64
	i := 0
	for e, n := 0, int(u32(stts[4:8])); e < n && 8+8*e+8 <= len(stts); e++ {
		c, d := int(u32(stts[8+8*e:])), int64(u32(stts[12+8*e:]))
		for k := 0; k < c && i < count; k++ {
			t.samples[i].dts = dts
			dts += d
			i++
		}
	}
	if i < count {
		return errors.New("mp4: stts covers fewer samples than stsz")
	}
	if len(ctts) >= 8 {
		i = 0
		for e, n := 0, int(u32(ctts[4:8])); e < n && 8+8*e+8 <= len(ctts); e++ {
			c, o := int(u32(ctts[8+8*e:])), int32(u32(ctts[12+8*e:]))
			for k := 0; k < c && i < count; k++ {
				t.samples[i].cto = o
				i++
			}
		}
	}

	// Sync samples: no stss means every sample is a sync sample.
	if len(stss) >= 8 {
		for e, n := 0, int(u32(stss[4:8])); e < n && 8+4*e+4 <= len(stss); e++ {
			if k := int(u32(stss[8+4*e:])) - 1; k >= 0 && k < count {
				t.samples[k].key = true
			}
		}
	} else {
		for k := range t.samples {
			t.samples[k].key = true
		}
	}

	// Offsets: chunks (stco/co64) split into samples by stsc.
	var chunks []int64
	switch {
	case len(stco) >= 8:
		for e, n := 0, int(u32(stco[4:8])); e < n && 8+4*e+4 <= len(stco); e++ {
			chunks = append(chunks, int64(u32(stco[8+4*e:])))
		}
	case len(co64) >= 8:
		for e, n := 0, int(u32(co64[4:8])); e < n && 8+8*e+8 <= len(co64); e++ {
			chunks = append(chunks, int64(binary.BigEndian.Uint64(co64[8+8*e:])))
		}
	default:
		return errors.New("mp4: missing stco/co64")
	}
	nsc := int(u32(stsc[4:8]))
	if len(stsc) < 8+12*nsc || nsc == 0 {
		return errors.New("mp4: bad stsc")
	}
	i = 0
	for e := 0; e < nsc; e++ {
		first := int(u32(stsc[8+12*e:])) - 1
		per := int(u32(stsc[12+12*e:]))
		last := len(chunks)
		if e+1 < nsc {
			last = int(u32(stsc[8+12*(e+1):])) - 1
		}
		for c := first; c < last && c < len(chunks); c++ {
			off := chunks[c]
			for k := 0; k < per && i < count; k++ {
				t.samples[i].off = off
				off += int64(t.samples[i].size)
				i++
			}
		}
	}
	if i < count {
		return errors.New("mp4: chunk table covers fewer samples than stsz")
	}
	return nil
}
