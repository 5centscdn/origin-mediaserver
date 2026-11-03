package rtmp

import (
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// decodeVideoTag parses one RTMP video tag (msg.Data) into a media.Packet.
//
// currentCI is a snapshot of the stream's CodecInfo before this tag; it is
// not mutated. If the tag is a codec sequence header, decodeVideoTag also
// returns a new *media.CodecInfo the caller should install via
// Bus.SetCodecInfo (nil if no update is needed). The returned packet is nil
// if the tag carries no publishable payload (unsupported codec, malformed
// tag, end-of-sequence marker, etc.) — callers must still apply a non-nil
// returned CodecInfo even when the packet is nil.
//
// This is shared by the inbound RTMP connection handler (handleVideoData)
// and the outbound RTMP puller (Pull) — both decode the same wire format,
// just from packets arriving on different sides of a connection.
func decodeVideoTag(streamName string, currentCI *media.CodecInfo, msg *Message) (*media.Packet, *media.CodecInfo) {
	if len(msg.Data) < 2 {
		return nil, nil
	}

	// Classic RTMP video tag layout:
	//   byte 0: frame_type (4 bits) | codec_id (4 bits)
	//   byte 1: avc_packet_type (for H.264: 0=config, 1=nalu, 2=end)
	//   bytes 2-4: composition time offset (24-bit signed, for H.264)
	//   bytes 5+: payload
	//
	// Enhanced RTMP (used for HEVC) repurposes byte 0 entirely: bit 7 is an
	// "is this enhanced" flag, and when set, bits 3-0 become a PacketType
	// (not a codec ID) — the actual codec is identified by a 4-byte FourCC
	// in bytes 1-4 instead. Classic RTMP never sets bit 7 (frame_type only
	// ever uses bits 4-6, codec_id bits 0-3), so the two framings can't
	// collide.
	isEnhanced := msg.Data[0]&0x80 != 0

	var frameType byte
	if isEnhanced {
		frameType = (msg.Data[0] >> 4) & 0x07 // bits 6-4 only; bit 7 is the enhanced flag
	} else {
		frameType = (msg.Data[0] >> 4) & 0x0F
	}
	isKeyframe := frameType == 1

	pkt := &media.Packet{
		DTS:        int64(msg.Timestamp) * 90, // convert ms → 90 kHz ticks
		IsKeyframe: isKeyframe,
		WallClock:  time.Now(),
		StreamID:   streamName,
	}

	var newCI *media.CodecInfo

	if isEnhanced {
		if len(msg.Data) < 5 {
			return nil, nil
		}
		packetType := msg.Data[0] & 0x0F
		fourCC := string(msg.Data[1:5])
		payload := msg.Data[5:]

		if fourCC != "hvc1" && fourCC != "hevc" {
			return nil, nil // other Enhanced RTMP codecs (AV1, VP9, ...) not supported yet
		}
		pkt.Codec = media.CodecH265

		switch packetType {
		case 0: // PacketTypeSequenceStart: HEVCDecoderConfigurationRecord
			pkt.Type = media.PacketConfig
			pkt.Data = payload
			if ci := parseHEVCDecoderConfig(payload); ci != nil {
				keepAudio(ci, currentCI)
				newCI = ci
			}
		case 1: // PacketTypeCodedFrames: 3-byte composition time, then AVCC-style NALUs
			if len(payload) < 3 {
				return nil, newCI
			}
			cts := int64(int32(uint32(payload[0])<<16|uint32(payload[1])<<8|uint32(payload[2])) << 8 >> 8)
			pkt.CompositionTimeOffset = cts * 90
			pkt.PTS = pkt.DTS + pkt.CompositionTimeOffset
			if isKeyframe {
				pkt.Type = media.PacketKeyframe
			} else {
				pkt.Type = media.PacketMedia
			}
			pkt.Data = convertAVCCToAnnexB(payload[3:])
		case 3: // PacketTypeCodedFramesX: AVCC-style NALUs directly, composition time assumed 0
			pkt.PTS = pkt.DTS
			if isKeyframe {
				pkt.Type = media.PacketKeyframe
			} else {
				pkt.Type = media.PacketMedia
			}
			pkt.Data = convertAVCCToAnnexB(payload)
		case 2: // PacketTypeSequenceEnd
			pkt.Type = media.PacketEnd
		default:
			return nil, newCI
		}
	} else {
		codecID := msg.Data[0] & 0x0F // 7=H.264
		switch codecID {
		case 7: // H.264 / AVC
			pkt.Codec = media.CodecH264
			if len(msg.Data) >= 5 {
				avcPacketType := msg.Data[1]
				cts := int64(int32(uint32(msg.Data[2])<<16|uint32(msg.Data[3])<<8|uint32(msg.Data[4])) << 8 >> 8)
				pkt.CompositionTimeOffset = cts * 90
				pkt.PTS = pkt.DTS + pkt.CompositionTimeOffset

				switch avcPacketType {
				case 0: // AVCDecoderConfigurationRecord (sequence header)
					pkt.Type = media.PacketConfig
					pkt.Data = msg.Data[5:]
					if ci := parseAVCDecoderConfig(pkt.Data); ci != nil {
						keepAudio(ci, currentCI)
						newCI = ci
					}
				case 1: // NALU(s)
					if isKeyframe {
						pkt.Type = media.PacketKeyframe
					} else {
						pkt.Type = media.PacketMedia
					}
					pkt.Data = convertAVCCToAnnexB(msg.Data[5:])
				case 2: // end of sequence
					pkt.Type = media.PacketEnd
				}
			}

		default:
			return nil, nil // unsupported codec
		}
	}

	if pkt.Data == nil {
		return nil, newCI
	}
	return pkt, newCI
}

// decodeAudioTag parses one RTMP audio tag (msg.Data) into a media.Packet.
// Same contract as decodeVideoTag — see its doc comment.
func decodeAudioTag(streamName string, currentCI *media.CodecInfo, msg *Message) (*media.Packet, *media.CodecInfo) {
	if len(msg.Data) < 2 {
		return nil, nil
	}

	// RTMP audio tag layout:
	//   byte 0: sound_format (4 bits) | sound_rate (2) | sound_size (1) | sound_type (1)
	//   byte 1: aac_packet_type (for AAC: 0=config, 1=raw)

	soundFormat := (msg.Data[0] >> 4) & 0x0F // 10=AAC, 2=MP3, 11=Speex

	pkt := &media.Packet{
		DTS:       int64(msg.Timestamp) * 90,
		PTS:       int64(msg.Timestamp) * 90,
		WallClock: time.Now(),
		StreamID:  streamName,
	}

	var newCI *media.CodecInfo

	switch soundFormat {
	case 10: // AAC
		pkt.Codec = media.CodecAAC
		aacPacketType := msg.Data[1]
		switch aacPacketType {
		case 0: // AudioSpecificConfig
			pkt.Type = media.PacketConfig
			pkt.Data = msg.Data[2:]
			// Merge into a copy of the existing CodecInfo (e.g. video-derived
			// width/height/profile set earlier) rather than mutating the
			// struct CodecInfo() returned — that struct may be read
			// concurrently by other goroutines (HLS packager, DVR, HTTP
			// handlers) at any time.
			if currentCI != nil {
				merged := *currentCI
				merged.AudioCodec = media.CodecAAC
				merged.AACConfig = pkt.Data
				newCI = &merged
			} else {
				newCI = &media.CodecInfo{
					AudioCodec: media.CodecAAC,
					AACConfig:  pkt.Data,
				}
			}
		case 1: // Raw AAC frame
			pkt.Type = media.PacketMedia
			pkt.Data = msg.Data[2:]
		}

	case 2: // MP3
		pkt.Codec = media.CodecMP3
		pkt.Type = media.PacketMedia
		pkt.Data = msg.Data[1:]
		// MP3 has no config record, so CodecInfo is set from the frames
		// themselves. Without it outputs assumed AAC: FMLE's default MP3
		// audio was declared AAC in the HLS PMT and CODECS, and hls.js
		// failed with fragParsingError. Checked on every frame, so it also
		// recovers after a video sequence header replaces CodecInfo.
		if currentCI == nil || currentCI.AudioCodec != media.CodecMP3 || currentCI.SampleRate == 0 {
			rate, ch := mp3FrameInfo(pkt.Data)
			var merged media.CodecInfo
			if currentCI != nil {
				merged = *currentCI
			}
			merged.AudioCodec = media.CodecMP3
			merged.AACConfig = nil
			merged.SampleRate, merged.Channels = rate, ch
			newCI = &merged
		}

	case 11: // Speex
		pkt.Codec = media.CodecSpeex
		pkt.Type = media.PacketMedia
		pkt.Data = msg.Data[1:]

	default:
		return nil, nil
	}

	if pkt.Data == nil {
		return nil, newCI
	}
	return pkt, newCI
}

// keepAudio copies the audio fields of prev into a CodecInfo freshly built
// from a video sequence header. Without it, audio that was described first
// (AAC config, or MP3 detected from frames) vanished whenever the encoder
// sent or re-sent its video config, and outputs initialised in that window
// saw no audio codec.
func keepAudio(ci, prev *media.CodecInfo) {
	if prev == nil {
		return
	}
	ci.AudioCodec = prev.AudioCodec
	ci.AACConfig = prev.AACConfig
	ci.SampleRate = prev.SampleRate
	ci.Channels = prev.Channels
}

// mp3SampleRates is indexed by [version][sample_rate_index], version 0 =
// MPEG-1, 1 = MPEG-2, 2 = MPEG-2.5.
var mp3SampleRates = [3][3]int{
	{44100, 48000, 32000},
	{22050, 24000, 16000},
	{11025, 12000, 8000},
}

// mp3FrameInfo reads the sample rate and channel count from an MPEG audio
// frame header (ISO/IEC 11172-3 2.4.1.3) at the start of b. It returns 0, 0
// if b doesn't start with one.
func mp3FrameInfo(b []byte) (rate, channels int) {
	if len(b) < 4 || b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return 0, 0
	}
	var v int
	switch (b[1] >> 3) & 3 {
	case 3:
		v = 0
	case 2:
		v = 1
	case 0:
		v = 2
	default:
		return 0, 0
	}
	idx := (b[2] >> 2) & 3
	if idx == 3 {
		return 0, 0
	}
	channels = 2
	if b[3]>>6 == 3 {
		channels = 1
	}
	return mp3SampleRates[v][idx], channels
}
