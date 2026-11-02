package mpegts

// mp2Frame is one MPEG-1/2 Audio Layer II access unit extracted from a PES
// payload — the header stays in data (unlike splitADTS, which strips it):
// MP2's header carries no separate "raw payload" convention downstream
// consumers expect stripped, and passthrough muxing (see mux.writeMP4A's
// MPEG-1/2 Audio object type) uses each frame verbatim, header included.
type mp2Frame struct {
	data       []byte
	sampleRate int
	channels   int
}

// mp2BitrateKbps is the MPEG-1 Audio bitrate table (kbps), indexed by the
// 4-bit bitrate_index common to Layers I-III per ISO/IEC 11172-3 — index 0
// is "free" (unsupported here) and 15 is reserved/bad.
var mp2BitrateKbps = [16]int{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 0}

// mp2SampleRates maps the 2-bit sample_rate_index by MPEG version: index 0
// (MPEG-1) is what every source this codebase has actually observed uses
// (48/44.1/32kHz broadcast audio); MPEG-2/2.5's lower rates are included
// since the bit layout makes it free to support, not because it's been
// seen live.
var mp2SampleRates = [3][3]int{
	{44100, 48000, 32000}, // MPEG-1        (version bits 11)
	{22050, 24000, 16000}, // MPEG-2        (version bits 10)
	{11025, 12000, 8000},  // MPEG-2.5      (version bits 00)
}

// splitMP2 scans a PES payload for consecutive MPEG-1/2 Audio Layer II
// frames — mirrors splitADTS's tolerance for a payload that bundles several
// frames per PES and for stray bytes between frames (resyncs to the next
// 0xFFE.. sync word rather than aborting). Frames using a layer other than
// II, or a reserved bitrate/sample-rate index, are skipped: this codebase's
// only confirmed real source (DVB/broadcast MPEG-TS feeds) uses Layer II,
// matching this project's established pattern of a deliberately narrow,
// verified-against-a-real-encoder scope (see splitADTS, captiondecode,
// HEVC's own MPEG-TS-only scoping).
func splitMP2(data []byte) []mp2Frame {
	var frames []mp2Frame
	pos := 0
	for pos+4 <= len(data) {
		if data[pos] != 0xFF || data[pos+1]&0xE0 != 0xE0 {
			pos++
			continue
		}

		versionBits := (data[pos+1] >> 3) & 0x03
		layerBits := (data[pos+1] >> 1) & 0x03
		if layerBits != 0x02 { // 10 = Layer II; anything else is out of scope
			pos++
			continue
		}
		bitrateIndex := (data[pos+2] >> 4) & 0x0F
		sampleRateIndex := (data[pos+2] >> 2) & 0x03
		padding := (data[pos+2] >> 1) & 0x01
		channelMode := (data[pos+3] >> 6) & 0x03

		bitrateKbps := mp2BitrateKbps[bitrateIndex]
		if bitrateKbps == 0 || sampleRateIndex == 0x03 {
			pos++
			continue // free/bad bitrate or reserved sample rate — not a real frame
		}
		var versionRow int
		switch versionBits {
		case 0x03:
			versionRow = 0 // MPEG-1
		case 0x02:
			versionRow = 1 // MPEG-2
		default:
			versionRow = 2 // MPEG-2.5 (or reserved, treated the same)
		}
		sampleRate := mp2SampleRates[versionRow][sampleRateIndex]

		// ISO/IEC 11172-3's Layer II/III frame length formula (bytes),
		// bitrate in bits/sec.
		frameLength := 144*bitrateKbps*1000/sampleRate + int(padding)
		if frameLength < 4 || pos+frameLength > len(data) {
			break // truncated frame at the end of this payload — drop and stop
		}

		channels := 2
		if channelMode == 0x03 {
			channels = 1
		}

		frames = append(frames, mp2Frame{
			data:       data[pos : pos+frameLength],
			sampleRate: sampleRate,
			channels:   channels,
		})
		pos += frameLength
	}
	return frames
}
