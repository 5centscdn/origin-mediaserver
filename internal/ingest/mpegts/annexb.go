package mpegts

// splitAnnexB splits a start-code-delimited Annex-B byte stream into
// individual NAL unit payloads (start codes stripped). Same algorithm as
// internal/webrtc's splitAnnexB — duplicated rather than shared, since
// there's no existing common package for this and the two ingest paths
// otherwise have nothing else in common.
func splitAnnexB(data []byte) [][]byte {
	var nals [][]byte
	start := -1
	i := 0
	for i+2 < len(data) {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			if start >= 0 {
				nals = append(nals, data[start:i])
			}
			i += 3
			start = i
			continue
		}
		if i+3 < len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			if start >= 0 {
				nals = append(nals, data[start:i])
			}
			i += 4
			start = i
			continue
		}
		i++
	}
	if start >= 0 && start < len(data) {
		nals = append(nals, data[start:])
	}
	return nals
}

// withStartCode re-attaches a 4-byte Annex-B start code to a bare NAL.
// media.Packet.Data's documented H.264 convention keeps the start code in
// Data itself (see internal/rtmp/connection.go's convertAVCCToAnnexB) — this
// demuxer follows that convention for consistency with the RTMP ingest
// path, unlike internal/webrtc's WHIP path, which publishes bare NALs.
func withStartCode(nal []byte) []byte {
	out := make([]byte, 0, len(nal)+4)
	out = append(out, 0, 0, 0, 1)
	out = append(out, nal...)
	return out
}
