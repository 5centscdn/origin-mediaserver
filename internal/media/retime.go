package media

import "log/slog"

// H264Retimer rebuilds DTS, and repairs PTS, for H.264 that arrives with one
// timestamp per picture: MPEG-TS PES packets that carry a PTS but no DTS, and
// RTP, which only has a presentation timestamp.
//
// Why: with B-frames, decode order and display order differ, so a stream
// whose DTS is just its PTS has DTS going backwards. The DVR recorder takes
// that as a discontinuity on every B-frame (a 10 s test stream recorded
// 1.8 s in 9 sessions) and fMP4 sample durations go negative. Some
// encoders also stamp several pictures with the same PTS. Sorting by PTS
// then can't recover display order, and playback flickers back and forth
// (the standalone DVR server's v2.16.6 incident: 344 of 837 frames out of
// order).
//
// How: the display order is read from the bitstream itself, as the picture
// order count (h264poc.go). Pictures are held in a short reorder buffer of
// depth D, and the source's PTS values, sorted, are handed out to pictures in
// POC order, so each picture gets the timestamp of the slot it is really
// shown in. A duplicate value is moved into the gap before the next distinct
// one, since fMP4 needs strictly increasing timestamps. DTS is then the corrected PTS sequence lagged by D pictures
// (the same construction ffmpeg uses for has_b_frames), which keeps it
// increasing and never above PTS. D comes from the SPS VUI
// (max_num_reorder_frames), else 0 for Baseline and 2 otherwise, and grows if
// the stream turns out to reorder deeper. Latency added is D pictures.
//
// When POC can't be decoded (see h264poc.go's scope, or a slice before the
// first SPS/PPS), pictures are ordered by their source PTS instead, which
// still yields a valid DTS but can't fix duplicate PTS values.
//
// Slices are pushed in decode order, one Packet each, with the source PTS in
// Packet.PTS. A slice with first_mb_in_slice > 0 belongs to the picture
// before it and gets the same timestamps. Callers use a retimer only while
// the source has no DTS of its own; see ingest/mpegts and ingest/rtsp.
type H264Retimer struct {
	log  *slog.Logger
	spss map[int]h264SPS
	ppss map[int]h264PPS
	poc  pocDecoder

	depth     int  // D
	depthSet  bool // D came from a decoded SPS
	usePOC    bool // current period is keyed by POC (else by source PTS)
	havePOC   bool // poc decoder has a starting point (IDR or I slice)
	lastKey   int64
	keyedAny  bool
	lastSrc   int64
	haveSrc   bool
	lastDTS   int64
	haveDTS   bool
	frameDur  int64
	decodeIdx int64 // decode index of the next picture

	dec  []*retimePic // pictures not yet emitted, decode order
	pend []*retimePic // pictures without an assigned PTS
	pool []int64      // source PTS values not yet assigned

	// assigned holds the corrected PTS values in ascending (display) order;
	// assigned[i-assignedBase] is the i-th of the current run.
	assigned     []int64
	assignedBase int64

	last *retimePic // latest picture, for slices with first_mb > 0
}

type retimePic struct {
	idx     int64
	key     int64
	pts     int64
	dts     int64
	done    bool // pts assigned
	emitted bool
	pkts    []*Packet
}

// maxRetimeDepth caps D. H.264 allows at most 16 reordered pictures.
const maxRetimeDepth = 16

// retimeJump is the largest source PTS step, in 90 kHz ticks, treated as
// continuous. A bigger jump (source restart, 33-bit wrap) starts a new run.
const retimeJump = 10 * 90000

// NewH264Retimer returns a retimer; log may be nil.
func NewH264Retimer(log *slog.Logger) *H264Retimer {
	if log == nil {
		log = slog.Default()
	}
	return &H264Retimer{
		log: log, spss: map[int]h264SPS{}, ppss: map[int]h264PPS{},
		depth: 2, frameDur: 3000,
	}
}

// SetSPS records an SPS NAL (no start code). The reorder depth is taken from
// the first one that states it.
func (r *H264Retimer) SetSPS(nal []byte) {
	id, s, ok := parseH264SPS(nal)
	if !ok {
		return
	}
	r.spss[id] = s
	if !r.depthSet {
		switch {
		case s.maxNumReorderFrames >= 0:
			r.depth = s.maxNumReorderFrames
		case s.profileIDC == 66:
			r.depth = 0 // Baseline has no B-frames
		}
		r.depthSet = true
	}
}

// SetPPS records a PPS NAL (no start code).
func (r *H264Retimer) SetPPS(nal []byte) {
	if id, p, ok := parseH264PPS(nal); ok {
		r.ppss[id] = p
	}
}

// Depth returns the current reorder depth D.
func (r *H264Retimer) Depth() int { return r.depth }

// Push adds one slice in decode order. nal is the slice NAL without start
// code; pkt carries the source PTS. It returns the packets now ready to
// publish, in decode order, with DTS and PTS set.
func (r *H264Retimer) Push(pkt *Packet, nal []byte) []*Packet {
	h, okHdr := parseH264SliceHeader(nal, r.spss, r.ppss)

	// A continuation slice of the previous picture.
	if okHdr && h.firstMB > 0 && r.last != nil {
		if r.last.emitted {
			pkt.PTS, pkt.DTS = r.last.pts, r.last.dts
			return []*Packet{pkt}
		}
		r.last.pkts = append(r.last.pkts, pkt)
		return nil
	}

	var out []*Packet
	src := pkt.PTS
	if r.haveSrc && (src-r.lastSrc > retimeJump || r.lastSrc-src > retimeJump) {
		r.log.Info("retime: source PTS jump, restarting", "from", r.lastSrc, "to", src)
		out = append(out, r.flush(true)...)
	}
	r.lastSrc, r.haveSrc = src, true

	// Choose the ordering key: POC when it can be decoded from a known
	// starting point, else the source PTS.
	isI := okHdr && h.sliceType%5 == 2
	if okHdr && (h.idr || (isI && !r.havePOC)) {
		// Nothing after an IDR is shown before it; pictures before an I
		// slice that starts POC tracking are ordered on their own.
		out = append(out, r.flush(false)...)
		r.usePOC, r.havePOC = true, true
		r.keyedAny = false
	}
	key := src
	if r.usePOC {
		if okHdr {
			key = int64(r.poc.decode(h))
		} else {
			// Lost POC mid-period: finish the period, carry on by PTS.
			out = append(out, r.flush(false)...)
			r.usePOC, r.havePOC = false, false
			r.keyedAny = false
		}
	}

	// A picture shown before one already assigned: the stream reorders
	// deeper than D. Grow D; this picture is placed as well as possible.
	if r.keyedAny && key < r.lastKey && r.depth < maxRetimeDepth {
		r.depth++
		r.log.Info("retime: deeper reordering than expected, growing buffer", "depth", r.depth)
	}

	p := &retimePic{idx: r.decodeIdx, key: key, pkts: []*Packet{pkt}}
	r.decodeIdx++
	r.last = p
	r.dec = append(r.dec, p)
	r.pend = append(r.pend, p)
	r.pool = append(r.pool, src)
	for len(r.pend) > r.depth {
		r.assignNext()
	}
	return append(out, r.emitReady()...)
}

// Flush assigns and returns everything buffered, e.g. when the source ends
// or starts sending DTS.
func (r *H264Retimer) Flush() []*Packet { return r.flush(true) }

// flush assigns all pending pictures and emits them. reset also restarts
// the DTS run (after a source discontinuity or at the end).
func (r *H264Retimer) flush(reset bool) []*Packet {
	for len(r.pend) > 0 {
		r.assignNext()
	}
	out := r.emitReady()
	if reset {
		r.decodeIdx, r.assigned, r.assignedBase = 0, nil, 0
		r.haveDTS, r.haveSrc, r.keyedAny = false, false, false
	}
	return out
}

// assignNext gives the pending picture shown first the smallest unassigned
// source PTS.
func (r *H264Retimer) assignNext() {
	pi := 0
	for i, p := range r.pend {
		if p.key < r.pend[pi].key || (p.key == r.pend[pi].key && p.idx < r.pend[pi].idx) {
			pi = i
		}
	}
	vi := 0
	for i, v := range r.pool {
		if v < r.pool[vi] {
			vi = i
		}
	}
	p, v := r.pend[pi], r.pool[vi]
	r.pend = append(r.pend[:pi], r.pend[pi+1:]...)
	r.pool = append(r.pool[:vi], r.pool[vi+1:]...)

	if n := len(r.assigned); n > 0 {
		last := r.assigned[n-1]
		if v <= last {
			// A duplicate source PTS. Two pictures can't share a slot,
			// so place this one in the gap before the next distinct
			// value (or one frame on if there's none yet).
			next := int64(-1)
			for _, w := range r.pool {
				if w > last && (next < 0 || w < next) {
					next = w
				}
			}
			step := r.frameDur
			if next > 0 && (next-last)/2 < step {
				step = (next - last) / 2
			}
			if step < 1 {
				step = 1
			}
			v = last + step
		} else if d := v - last; d < 90000 {
			r.frameDur = d
		}
	}
	p.pts, p.done = v, true
	r.assigned = append(r.assigned, v)
	r.lastKey, r.keyedAny = p.key, true
}

// emitReady emits pictures from the front of the decode queue whose PTS is
// assigned, setting DTS to the corrected PTS D pictures back.
func (r *H264Retimer) emitReady() []*Packet {
	var out []*Packet
	for len(r.dec) > 0 && r.dec[0].done {
		p := r.dec[0]
		r.dec = r.dec[1:]

		k := p.idx - int64(r.depth) - r.assignedBase
		var dts int64
		switch {
		case k >= 0 && k < int64(len(r.assigned)):
			dts = r.assigned[k]
		case len(r.assigned) > 0:
			dts = r.assigned[0] - (int64(r.depth)-p.idx+r.assignedBase)*r.frameDur
		default:
			dts = p.pts
		}
		if dts > p.pts {
			dts = p.pts
		}
		if r.haveDTS && dts < r.lastDTS {
			dts = r.lastDTS // D grew mid-run; never go backwards
		}
		if dts > p.pts {
			p.pts = dts // unplaceable picture: show it as soon as decodable
		}
		r.lastDTS, r.haveDTS = dts, true
		p.dts, p.emitted = dts, true
		for _, pkt := range p.pkts {
			pkt.PTS, pkt.DTS = p.pts, dts
		}
		out = append(out, p.pkts...)
		p.pkts = nil
	}
	r.trim()
	return out
}

// trim drops assigned values no future DTS lookup can need.
func (r *H264Retimer) trim() {
	keepFrom := r.decodeIdx - int64(r.depth) - 1
	if len(r.dec) > 0 {
		keepFrom = r.dec[0].idx - int64(r.depth) - 1
	}
	if drop := keepFrom - r.assignedBase; drop > 32 && drop <= int64(len(r.assigned)) {
		r.assigned = append([]int64(nil), r.assigned[drop:]...)
		r.assignedBase += drop
	}
}
