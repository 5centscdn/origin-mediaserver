package icecast

// mp3FrameInfo describes one parsed MPEG audio frame header (ISO/IEC
// 11172-3 / 13818-3) — enough to know the frame's total length (so it can
// be sliced out of a continuous byte stream) and its sample rate/channel
// count for CodecInfo.
type mp3FrameInfo struct {
	length          int // total frame length in bytes, header included
	sampleRate      int
	channels        int
	samplesPerFrame int
}

// mpegVersion/layer bit patterns (from the second header byte).
const (
	mpegVersion2_5 = 0
	mpegVersion2   = 2
	mpegVersion1   = 3

	layerIII = 1
	layerII  = 2
	layerI   = 3
)

// bitrateTables[versionIsV1][layerIndex] where layerIndex: 0=LayerI,
// 1=LayerII, 2=LayerIII. MPEG2/2.5 share one table for LayerII/III.
var bitrateTableV1 = [3][15]int{
	{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448}, // Layer I
	{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},    // Layer II
	{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},     // Layer III
}

var bitrateTableV2 = [3][15]int{
	{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256}, // Layer I
	{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},      // Layer II
	{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},      // Layer III
}

var sampleRateTableV1 = [3]int{44100, 48000, 32000}
var sampleRateTableV2 = [3]int{22050, 24000, 16000}
var sampleRateTableV25 = [3]int{11025, 12000, 8000}

// parseMP3Header parses a 4-byte MPEG audio frame header at the start of
// data and returns the frame's total length and audio parameters. Returns
// ok=false if data doesn't start with a valid sync word / reserved field
// combination (caller should advance one byte and retry — the standard
// MPEG audio resync strategy).
func parseMP3Header(data []byte) (mp3FrameInfo, bool) {
	if len(data) < 4 {
		return mp3FrameInfo{}, false
	}
	if data[0] != 0xFF || data[1]&0xE0 != 0xE0 {
		return mp3FrameInfo{}, false
	}

	version := (data[1] >> 3) & 0x03
	layer := (data[1] >> 1) & 0x03
	if layer == 0 || version == 1 { // layer "reserved", version "reserved"
		return mp3FrameInfo{}, false
	}
	bitrateIdx := (data[2] >> 4) & 0x0F
	sampleRateIdx := (data[2] >> 2) & 0x03
	padding := (data[2] >> 1) & 0x01
	if bitrateIdx == 0 || bitrateIdx == 15 || sampleRateIdx == 3 {
		return mp3FrameInfo{}, false // "free" bitrate / reserved sample rate — not handled
	}
	channelMode := (data[3] >> 6) & 0x03
	channels := 2
	if channelMode == 3 {
		channels = 1 // mono
	}

	var bitrate, sampleRate int
	layerIdx := 3 - int(layer) // I=3->0, II=2->1, III=1->2
	if version == mpegVersion1 {
		bitrate = bitrateTableV1[layerIdx][bitrateIdx] * 1000
		sampleRate = sampleRateTableV1[sampleRateIdx]
	} else {
		bitrate = bitrateTableV2[layerIdx][bitrateIdx] * 1000
		if version == mpegVersion2 {
			sampleRate = sampleRateTableV2[sampleRateIdx]
		} else {
			sampleRate = sampleRateTableV25[sampleRateIdx]
		}
	}
	if bitrate == 0 || sampleRate == 0 {
		return mp3FrameInfo{}, false
	}

	var length, samplesPerFrame int
	switch layer {
	case layerI:
		samplesPerFrame = 384
		length = (12*bitrate/sampleRate + int(padding)) * 4
	case layerII:
		samplesPerFrame = 1152
		length = 144*bitrate/sampleRate + int(padding)
	default: // layerIII
		if version == mpegVersion1 {
			samplesPerFrame = 1152
			length = 144*bitrate/sampleRate + int(padding)
		} else {
			// MPEG2/2.5 Layer III halves the samples/frame (576) and uses
			// the "LSF" frame-length formula (72, not 144).
			samplesPerFrame = 576
			length = 72*bitrate/sampleRate + int(padding)
		}
	}
	if length < 4 {
		return mp3FrameInfo{}, false
	}

	return mp3FrameInfo{
		length:          length,
		sampleRate:      sampleRate,
		channels:        channels,
		samplesPerFrame: samplesPerFrame,
	}, true
}
