package mpegts

// adtsFrame is one AAC access unit extracted from an ADTS-framed PES
// payload: the raw AAC bytes (ADTS header stripped — downstream consumers
// expect raw AAC, matching the FLV/RTMP convention; see
// internal/hls/packager.go's wrapADTS, which re-wraps raw AAC for its own TS
// output) plus the fields needed to synthesize an AudioSpecificConfig.
type adtsFrame struct {
	data            []byte
	audioObjectType byte // ADTS profile + 1 (ADTS profile 1 = AAC-LC = audioObjectType 2)
	sfIndex         byte
	channelConfig   byte
}

// splitADTS scans a PES payload for consecutive ADTS frames — a single PES
// packet commonly bundles several 1024-sample AAC frames — and returns
// each one. Malformed/misaligned bytes are skipped byte-by-byte until the
// next sync word, tolerating stray bytes from a non-conformant sender
// rather than aborting the whole payload.
func splitADTS(data []byte) []adtsFrame {
	var frames []adtsFrame
	pos := 0
	for pos+7 <= len(data) {
		if data[pos] != 0xFF || data[pos+1]&0xF0 != 0xF0 {
			pos++
			continue
		}

		protectionAbsent := data[pos+1]&0x01 != 0
		headerLen := 9
		if protectionAbsent {
			headerLen = 7
		}

		profile := (data[pos+2] >> 6) & 0x03
		sfIndex := (data[pos+2] >> 2) & 0x0F
		channelConfig := (data[pos+2]&0x01)<<2 | (data[pos+3]>>6)&0x03
		frameLength := (int(data[pos+3]&0x03) << 11) | (int(data[pos+4]) << 3) | int(data[pos+5]>>5)

		if frameLength < headerLen || pos+frameLength > len(data) {
			break // truncated frame at the end of this payload — drop and stop
		}

		frames = append(frames, adtsFrame{
			data:            data[pos+headerLen : pos+frameLength],
			audioObjectType: profile + 1,
			sfIndex:         sfIndex,
			channelConfig:   channelConfig,
		})
		pos += frameLength
	}
	return frames
}

// buildAudioSpecificConfig synthesizes the 2-byte raw AudioSpecificConfig
// media.CodecInfo.AACConfig expects (see mux.ParseAACSampleRate/
// ParseAACChannels) from ADTS header fields — ADTS itself never carries an
// AudioSpecificConfig directly, only the equivalent profile/sfIndex/channel
// fields inline in every frame's fixed header.
func buildAudioSpecificConfig(audioObjectType, sfIndex, channelConfig byte) []byte {
	b0 := (audioObjectType << 3) | (sfIndex >> 1)
	b1 := (sfIndex&0x01)<<7 | (channelConfig << 3)
	return []byte{b0, b1}
}
