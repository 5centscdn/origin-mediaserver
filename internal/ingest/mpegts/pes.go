package mpegts

// pesHeader holds a parsed PES packet's optional timestamps plus the
// elementary stream payload that follows the header.
type pesHeader struct {
	pts, dts       int64
	hasPTS, hasDTS bool
	payload        []byte
}

// parsePES parses one reassembled PES packet (starting at its 00 00 01
// start code) and returns its optional PTS/DTS plus the elementary stream
// payload. Returns ok=false if data is too short or doesn't start with a
// valid PES start code / optional-header marker — both are treated as
// malformed input from a reassembly bug or a non-conformant sender, not
// worth propagating as an error given this demuxer's ingest-tolerance goal.
func parsePES(data []byte) (pesHeader, bool) {
	if len(data) < 9 || data[0] != 0x00 || data[1] != 0x00 || data[2] != 0x01 {
		return pesHeader{}, false
	}
	// data[3] = stream_id (0xE0-0xEF video, 0xC0-0xDF audio) — not needed,
	// the caller already knows which PID (and therefore which track) this is.
	// data[4:6] = PES_packet_length — ignored; reassembly by "next start
	// indicator" (see Demuxer.handlePES) already handles both the bounded
	// (audio) and unbounded/zero (video) cases uniformly.

	flags1 := data[6] // top 2 bits must be '10' per spec
	if flags1&0xC0 != 0x80 {
		return pesHeader{}, false
	}
	flags2 := data[7]
	headerDataLength := int(data[8])
	headerStart := 9
	if headerStart+headerDataLength > len(data) {
		return pesHeader{}, false
	}

	var h pesHeader
	ptsDtsFlags := (flags2 >> 6) & 0x03
	off := headerStart
	switch {
	case ptsDtsFlags == 0x02 && off+5 <= len(data):
		h.pts = decodeTimestamp(data[off : off+5])
		h.hasPTS = true
	case ptsDtsFlags == 0x03 && off+10 <= len(data):
		h.pts = decodeTimestamp(data[off : off+5])
		h.dts = decodeTimestamp(data[off+5 : off+10])
		h.hasPTS = true
		h.hasDTS = true
	}

	h.payload = data[headerStart+headerDataLength:]
	return h, true
}

// decodeTimestamp decodes a 5-byte PTS or DTS field: a 33-bit value at
// 90 kHz, which is already this pipeline's own tick unit (media.Packet.DTS/
// PTS) — no rescale needed, unlike e.g. WebRTC's 48 kHz Opus clock.
func decodeTimestamp(b []byte) int64 {
	return (int64(b[0]&0x0E) << 29) | (int64(b[1]) << 22) | (int64(b[2]&0xFE) << 14) | (int64(b[3]) << 7) | (int64(b[4]) >> 1)
}
