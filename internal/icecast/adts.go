package icecast

// adtsFrameInfo describes one parsed ADTS-framed AAC access unit header.
// Duplicated from internal/ingest/mpegts's splitADTS rather than shared
// cross-package — same small-helper-duplication convention used
// throughout this codebase's ingest packages — adapted here for
// incremental parsing of a continuous byte stream (Icecast has no PES
// packet boundaries at all) rather than splitting a single already-
// complete payload.
type adtsFrameInfo struct {
	length          int // total frame length in bytes, ADTS header included
	headerLen       int // 7 (no CRC) or 9 (with CRC) — payload starts after this
	audioObjectType byte
	sfIndex         byte
	channelConfig   byte
}

// parseADTSHeader parses the ADTS frame header at the start of data.
// Returns ok=false if data doesn't start with a valid sync word (caller
// should advance one byte and retry) or if data is too short to contain a
// full header yet (caller should wait for more bytes — indistinguishable
// from "invalid" here, which is fine since feed's resync loop handles both
// by needing more data before making progress).
func parseADTSHeader(data []byte) (adtsFrameInfo, bool) {
	if len(data) < 7 {
		return adtsFrameInfo{}, false
	}
	if data[0] != 0xFF || data[1]&0xF0 != 0xF0 {
		return adtsFrameInfo{}, false
	}

	protectionAbsent := data[1]&0x01 != 0
	headerLen := 9
	if protectionAbsent {
		headerLen = 7
	}

	profile := (data[2] >> 6) & 0x03
	sfIndex := (data[2] >> 2) & 0x0F
	channelConfig := (data[2]&0x01)<<2 | (data[3]>>6)&0x03
	frameLength := (int(data[3]&0x03) << 11) | (int(data[4]) << 3) | int(data[5]>>5)

	if frameLength < headerLen {
		return adtsFrameInfo{}, false
	}

	return adtsFrameInfo{
		length:          frameLength,
		headerLen:       headerLen,
		audioObjectType: profile + 1,
		sfIndex:         sfIndex,
		channelConfig:   channelConfig,
	}, true
}

// adtsSampleRates is the standard ADTS sampling-frequency-index table.
var adtsSampleRates = [...]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// buildAudioSpecificConfig synthesizes the 2-byte raw AudioSpecificConfig
// media.CodecInfo.AACConfig expects — same construction as
// internal/ingest/mpegts's identically-named helper.
func buildAudioSpecificConfig(audioObjectType, sfIndex, channelConfig byte) []byte {
	b0 := (audioObjectType << 3) | (sfIndex >> 1)
	b1 := (sfIndex&0x01)<<7 | (channelConfig << 3)
	return []byte{b0, b1}
}
