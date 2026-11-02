package mux

// WrapADTS prepends a 7-byte ADTS header to a raw AAC frame, given its
// AudioSpecificConfig bytes (media.CodecInfo.AACConfig — passed as a plain
// []byte, not the CodecInfo type itself, since internal/media already
// imports internal/scte35 which imports this package; taking media.
// CodecInfo directly here would complete an import cycle). RTMP (and
// several other ingest paths) carry AAC without ADTS headers; MPEG-TS
// requires ADTS. Shared by internal/hls's TS-container packager and
// internal/tsout's continuous MPEG-TS-over-HTTP output — both mux AAC into
// TSMuxer the same way, so this lives here rather than in either caller.
func WrapADTS(rawAAC, aacConfig []byte) []byte {
	// Parse AudioSpecificConfig to get profile, sample rate index, channel config.
	// AudioSpecificConfig: 5 bits objectType, 4 bits samplingFrequencyIndex,
	//                       4 bits channelConfiguration, ...
	if len(aacConfig) < 2 {
		return rawAAC
	}

	objectType := (aacConfig[0] >> 3) & 0x1F // 5 bits
	if objectType == 31 {
		objectType = 32 + ((aacConfig[0] & 0x07) << 3) | (aacConfig[1] >> 5)
	}

	sfIndex := (aacConfig[0]&0x07)<<1 | (aacConfig[1] >> 7)
	chanConfig := (aacConfig[1] >> 3) & 0x0F

	// ADTS frame length includes the 7-byte header.
	frameLen := len(rawAAC) + 7

	adts := make([]byte, 7)
	adts[0] = 0xFF
	adts[1] = 0xF1 // ID=0 (MPEG-4), layer=0, protection_absent=1
	adts[2] = byte((objectType-1)<<6) | byte(sfIndex<<2) | byte(chanConfig>>2)
	adts[3] = byte(chanConfig&0x03)<<6 | byte(frameLen>>11)
	adts[4] = byte(frameLen >> 3)
	adts[5] = byte(frameLen&0x07)<<5 | 0x1F
	adts[6] = 0xFC

	out := make([]byte, 0, 7+len(rawAAC))
	out = append(out, adts...)
	out = append(out, rawAAC...)
	return out
}
