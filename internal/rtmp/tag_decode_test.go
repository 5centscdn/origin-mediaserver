package rtmp

import (
	"encoding/base64"
	"encoding/binary"
	"testing"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// minimalAVCDecoderConfig builds a syntactically valid (7-byte, zero SPS/PPS)
// AVCDecoderConfigurationRecord — enough for parseAVCDecoderConfig to return
// a non-nil CodecInfo without needing real SPS/PPS bytes.
func minimalAVCDecoderConfig(profile, level byte) []byte {
	return []byte{
		1,       // configurationVersion
		profile, // AVCProfileIndication
		0x00,    // profile_compatibility
		level,   // AVCLevelIndication
		0xFF,    // lengthSizeMinusOne (+ reserved bits)
		0x00,    // numSPS = 0
		0x00,    // numPPS = 0
	}
}

func videoConfigTag(profile, level byte) *Message {
	data := append([]byte{
		0x17,             // frame_type=1 (key), codec_id=7 (AVC)
		0x00,             // avc_packet_type=0 (sequence header)
		0x00, 0x00, 0x00, // composition time offset
	}, minimalAVCDecoderConfig(profile, level)...)
	return &Message{TypeID: msgVideo, Data: data}
}

func audioConfigTag(ascBytes []byte) *Message {
	data := append([]byte{
		0xAF, // sound_format=10 (AAC)
		0x00, // aac_packet_type=0 (AudioSpecificConfig)
	}, ascBytes...)
	return &Message{TypeID: msgAudio, Data: data}
}

func TestDecodeVideoTag_SequenceHeaderReturnsCodecInfo(t *testing.T) {
	_, newCI := decodeVideoTag("cam1", nil, videoConfigTag(0x64, 0x1F))
	if newCI == nil {
		t.Fatal("expected non-nil CodecInfo for a sequence header tag")
	}
	if newCI.VideoCodec != media.CodecH264 {
		t.Errorf("VideoCodec = %v, want CodecH264", newCI.VideoCodec)
	}
	if newCI.Profile != 0x64 || newCI.Level != 0x1F {
		t.Errorf("Profile/Level = %x/%x, want 64/1f", newCI.Profile, newCI.Level)
	}
}

func TestDecodeAudioTag_MergesIntoExistingVideoCodecInfo(t *testing.T) {
	// video-then-audio: the AAC config must merge into (not replace) the
	// video-derived CodecInfo already on the bus — this is the case that
	// used to be an in-place mutation race before the decode extraction.
	_, videoCI := decodeVideoTag("cam1", nil, videoConfigTag(0x64, 0x1F))
	if videoCI == nil {
		t.Fatal("setup: expected video CodecInfo")
	}

	asc := []byte{0x12, 0x10} // arbitrary AudioSpecificConfig bytes
	_, mergedCI := decodeAudioTag("cam1", videoCI, audioConfigTag(asc))
	if mergedCI == nil {
		t.Fatal("expected non-nil merged CodecInfo")
	}

	if mergedCI.VideoCodec != media.CodecH264 || mergedCI.Profile != 0x64 || mergedCI.Level != 0x1F {
		t.Errorf("merged CodecInfo lost video fields: %+v", mergedCI)
	}
	if mergedCI.AudioCodec != media.CodecAAC {
		t.Errorf("AudioCodec = %v, want CodecAAC", mergedCI.AudioCodec)
	}
	if string(mergedCI.AACConfig) != string(asc) {
		t.Errorf("AACConfig = %x, want %x", mergedCI.AACConfig, asc)
	}

	// The merge must not have mutated the original video CodecInfo in place
	// (that in-place mutation was the pre-extraction race) — videoCI's own
	// AACConfig must still be unset.
	if videoCI.AACConfig != nil {
		t.Errorf("original video CodecInfo was mutated in place: AACConfig = %x", videoCI.AACConfig)
	}
}

func TestDecodeAudioTag_NoExistingCodecInfoCreatesFresh(t *testing.T) {
	asc := []byte{0x12, 0x10}
	_, newCI := decodeAudioTag("cam1", nil, audioConfigTag(asc))
	if newCI == nil {
		t.Fatal("expected non-nil CodecInfo")
	}
	if newCI.AudioCodec != media.CodecAAC {
		t.Errorf("AudioCodec = %v, want CodecAAC", newCI.AudioCodec)
	}
	if string(newCI.AACConfig) != string(asc) {
		t.Errorf("AACConfig = %x, want %x", newCI.AACConfig, asc)
	}
}

func TestDecodeVideoTag_SequenceHeaderReplacesRatherThanMerges(t *testing.T) {
	// audio-then-video: this is a pre-existing (unchanged) asymmetry, not a
	// regression — a video sequence header always installs a fresh
	// video-only CodecInfo, same as before this refactor. Documented here
	// so a future change to this behavior is a deliberate decision.
	_, audioCI := decodeAudioTag("cam1", nil, audioConfigTag([]byte{0x12, 0x10}))
	if audioCI == nil {
		t.Fatal("setup: expected audio CodecInfo")
	}

	_, videoCI := decodeVideoTag("cam1", audioCI, videoConfigTag(0x64, 0x1F))
	if videoCI == nil {
		t.Fatal("expected non-nil video CodecInfo")
	}
	if videoCI.AudioCodec == media.CodecAAC {
		t.Error("video sequence header unexpectedly preserved prior audio info — behavior changed, update this test and the doc comment on decodeVideoTag")
	}
}

// ─── Enhanced RTMP (HEVC) ───────────────────────────────────────────────────
//
// Fixture bytes are real libx265 output (64x64, keyint=3) — same capture as
// internal/media/hevc_sps_test.go's fixtures, plus one real IDR and one real
// trailing (P) slice, split on Annex-B start codes and base64-embedded.

const (
	hevcFixtureVPS   = "QAEMAf//AWAAAAMAkAAAAwAAAwAelZgJAA=="
	hevcFixtureSPS   = "QgEBAWAAAAMAkAAAAwAAAwAeoCCBBZZWaSTK8BaAgAAAAwCAAAAMhAA="
	hevcFixturePPS   = "RAHBcrQiQA=="
	hevcFixtureIDR   = "KAGvHYDC+auR8E6B/xzsWMgyuIAjZRzhH88FPbvGvTf/3Ft3AO3NoT8sTP4ARFzrxACGcNtR3cqLbQfQz///WPYAVrHaqv52be0iqGkqjGF0zaIfdtZwVlzu8gNRPWK5CtxVnyK9w96yH+qQAPswgRekkfadfbhuZmSbzq2birxeMYyPS+3aqfAtimPxhUGKBPEfdVxaVdw130hqJMWK5c97BaSQ5+x7776nsugZbSrsKSzkJGhbk/Lbj6v7N7Mdh7bTNbE1/eJXd9v2UnCTLJBYGRVgCd64r/rmb6q9mxgizDCWPHD9mh/tgJweMblnwqSyOXgPz627kOlrszt+zXr4FGrPlZGS8vfLOwqdSLgcN0zr/QzWDnsB5nyN77ZG6XgsqekYdVy8TT4bHK0nTqTTqS4QJRNpWZUJISMbDuKLqIcfCjXkCUpihFEU8FTnGN3ENXn+FqpfTAorGmnxzE8YgMJ584TxX4r5o1lZlv6h5nBsufpy3aPmj9WWAdZSJOKtZf26vJEOS8WmOLxSMWL2719LuA6d+IaHbTgfmQgX6Wy0AKYoKyUxTl8NGuisG6ehlKEX/c6c2xME6yQ5E+FJe9quj23lnRfwr+nuAGCf8t1T4y5tHSVekFzoXaz2VpWT1ABNeFU5R+hc3GVYfrs1hNZ5lxWB9w+14sIHDDHYCAvvY88RiQFx7kD2QyD5y/TVemz2yAA="
	hevcFixtureTrail = "AgHQEVeEMY5ArmhIYf10pxHwvtmQAA=="
)

func hevcB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return b
}

// buildHEVCDecoderConfig builds a minimal-but-valid HEVCDecoderConfigurationRecord
// wrapping one VPS/SPS/PPS array each. The fixed header fields don't need to
// be individually correct: parseHEVCDecoderConfig re-derives profile/tier/
// level from the SPS itself (see media.ParseHEVCSPS), not from this record's
// own redundant header copies of the same fields.
func buildHEVCDecoderConfig(vps, sps, pps []byte) []byte {
	out := make([]byte, 23)
	out[0] = 1  // configurationVersion
	out[22] = 3 // numOfArrays

	appendArray := func(nalType byte, nal []byte) {
		out = append(out, nalType&0x3F, 0x00, 0x01) // array header + numNalus=1
		lenBuf := make([]byte, 2)
		binary.BigEndian.PutUint16(lenBuf, uint16(len(nal)))
		out = append(out, lenBuf...)
		out = append(out, nal...)
	}
	appendArray(32, vps)
	appendArray(33, sps)
	appendArray(34, pps)
	return out
}

// buildCodedFramesX builds an Enhanced RTMP PacketTypeCodedFramesX payload:
// AVCC-style (4-byte length-prefixed) NALUs, no composition time.
func buildCodedFramesX(nal []byte) []byte {
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(nal)))
	return append(lenBuf, nal...)
}

func TestDecodeVideoTag_HEVCSequenceStart(t *testing.T) {
	vps := hevcB64(t, hevcFixtureVPS)
	sps := hevcB64(t, hevcFixtureSPS)
	pps := hevcB64(t, hevcFixturePPS)
	configRecord := buildHEVCDecoderConfig(vps, sps, pps)

	// 0x90 = ex-header flag (bit7) + frame_type=1/key (bits6-4) + packetType=0/SequenceStart (bits3-0)
	data := append([]byte{0x90, 'h', 'v', 'c', '1'}, configRecord...)
	pkt, ci := decodeVideoTag("cam1", nil, &Message{TypeID: msgVideo, Data: data})

	if ci == nil {
		t.Fatal("expected non-nil CodecInfo from an HEVC SequenceStart")
	}
	if ci.VideoCodec != media.CodecH265 {
		t.Errorf("VideoCodec = %v, want CodecH265", ci.VideoCodec)
	}
	if ci.Width != 64 || ci.Height != 64 {
		t.Errorf("got %dx%d, want 64x64 (real libx265 SPS)", ci.Width, ci.Height)
	}
	if ci.HEVCProfileIDC != 1 {
		t.Errorf("HEVCProfileIDC = %d, want 1 (Main)", ci.HEVCProfileIDC)
	}
	if ci.HEVCLevelIDC != 30 {
		t.Errorf("HEVCLevelIDC = %d, want 30 (ffprobe ground truth: level 3.0)", ci.HEVCLevelIDC)
	}
	if len(ci.VPSNALu) == 0 || len(ci.PPSNALu) == 0 {
		t.Error("expected VPSNALu/PPSNALu to be populated")
	}
	if pkt == nil || pkt.Type != media.PacketConfig {
		t.Errorf("expected a PacketConfig packet, got %+v", pkt)
	}
}

func TestDecodeVideoTag_HEVCCodedFramesX(t *testing.T) {
	idr := hevcB64(t, hevcFixtureIDR)
	trail := hevcB64(t, hevcFixtureTrail)

	// 0x93 = ex-header + frame_type=1/key + packetType=3/CodedFramesX
	idrData := append([]byte{0x93, 'h', 'v', 'c', '1'}, buildCodedFramesX(idr)...)
	idrPkt, _ := decodeVideoTag("cam1", nil, &Message{TypeID: msgVideo, Data: idrData})
	if idrPkt == nil {
		t.Fatal("expected a non-nil keyframe packet")
	}
	if !idrPkt.IsKeyframe || idrPkt.Type != media.PacketKeyframe {
		t.Errorf("expected a keyframe packet, got IsKeyframe=%v Type=%v", idrPkt.IsKeyframe, idrPkt.Type)
	}
	if idrPkt.Data[0] != 0 || idrPkt.Data[1] != 0 || idrPkt.Data[2] != 0 || idrPkt.Data[3] != 1 {
		t.Errorf("expected Annex-B start code, got % x", idrPkt.Data[:4])
	}

	// 0x83 = ex-header + frame_type=2/inter + packetType=3/CodedFramesX
	trailData := append([]byte{0x83, 'h', 'v', 'c', '1'}, buildCodedFramesX(trail)...)
	trailPkt, _ := decodeVideoTag("cam1", nil, &Message{TypeID: msgVideo, Timestamp: 40, Data: trailData})
	if trailPkt == nil {
		t.Fatal("expected a non-nil delta packet")
	}
	if trailPkt.IsKeyframe {
		t.Error("expected a non-keyframe packet")
	}
	if trailPkt.PTS != trailPkt.DTS {
		t.Errorf("CodedFramesX must assume zero composition time: PTS=%d, DTS=%d", trailPkt.PTS, trailPkt.DTS)
	}
}

func TestDecodeVideoTag_HEVCCodedFrames_WithCompositionTime(t *testing.T) {
	idr := hevcB64(t, hevcFixtureIDR)
	cts := []byte{0x00, 0x00, 0x05} // +5 ms composition time offset
	payload := append(append([]byte{}, cts...), func() []byte {
		lenBuf := make([]byte, 4)
		binary.BigEndian.PutUint32(lenBuf, uint32(len(idr)))
		return append(lenBuf, idr...)
	}()...)

	// 0x91 = ex-header + frame_type=1/key + packetType=1/CodedFrames
	data := append([]byte{0x91, 'h', 'v', 'c', '1'}, payload...)
	pkt, _ := decodeVideoTag("cam1", nil, &Message{TypeID: msgVideo, Timestamp: 100, Data: data})
	if pkt == nil {
		t.Fatal("expected a non-nil packet")
	}
	wantCTS := int64(5 * 90)
	if pkt.CompositionTimeOffset != wantCTS {
		t.Errorf("CompositionTimeOffset = %d, want %d", pkt.CompositionTimeOffset, wantCTS)
	}
	if pkt.PTS != pkt.DTS+wantCTS {
		t.Errorf("PTS = %d, want DTS(%d)+CTS(%d) = %d", pkt.PTS, pkt.DTS, wantCTS, pkt.DTS+wantCTS)
	}
}

func TestDecodeVideoTag_EnhancedRTMP_UnsupportedFourCC(t *testing.T) {
	// av01 (AV1) is a real Enhanced RTMP FourCC but deliberately not
	// supported yet (see decodeVideoTag's doc comment) — must be silently
	// ignored, not misdecoded as if it were HEVC.
	data := []byte{0x90, 'a', 'v', '0', '1', 0x00, 0x00, 0x00, 0x00}
	pkt, ci := decodeVideoTag("cam1", nil, &Message{TypeID: msgVideo, Data: data})
	if pkt != nil || ci != nil {
		t.Errorf("expected (nil, nil) for an unsupported Enhanced RTMP FourCC, got pkt=%+v ci=%+v", pkt, ci)
	}
}

func TestDecodeVideoTag_ClassicRTMPStillWorksAlongsideEnhanced(t *testing.T) {
	// Guards against a regression in the isEnhanced bit-layout switch:
	// classic H.264 (bit 7 of byte 0 always 0) must decode identically to
	// before, unaffected by the new Enhanced RTMP branch.
	pkt, ci := decodeVideoTag("cam1", nil, videoConfigTag(0x64, 0x1F))
	if ci == nil || ci.VideoCodec != media.CodecH264 {
		t.Fatalf("classic AVC sequence header broke: pkt=%+v ci=%+v", pkt, ci)
	}
}
