// Package media defines the core media types shared across all protocol
// handlers and packagers.  A MediaPacket is the single unit that flows
// through the system from ingest to output.
package media

import (
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/scte35"
)

// CodecType identifies a media codec.
type CodecType uint8

const (
	CodecUnknown CodecType = iota

	// Video
	CodecH264
	CodecH265
	CodecAV1
	CodecVP6
	CodecVP8
	CodecVP9

	// Audio
	CodecAAC
	CodecMP3
	CodecOpus
	CodecAC3
	CodecEAC3
	CodecSpeex
	CodecPCMA // G.711 A-law
	CodecPCMU // G.711 μ-law
	CodecMP2  // MPEG-1/2 Audio Layer II — DVB/broadcast sources; passthrough only, never transcoded

	// CodecSCTE35 identifies an in-band SCTE-35 splice_info_section carried
	// as a Packet's Data (raw section bytes) with SCTE35 set to the parsed
	// form. Deliberately placed after every video/audio codec so it falls
	// outside both IsVideo()'s and IsAudio()'s ranges — every existing
	// packet consumer that switches on IsVideo()/IsAudio() already treats
	// an unrecognized codec as a no-op, so this is safe by construction.
	CodecSCTE35

	// CodecDVBTeletext identifies one raw EBU teletext data_field (ETSI EN
	// 300 472 — reserved/field_parity/line_offset byte, framing_code byte,
	// magazine_and_packet_address, data_block; see internal/teletext)
	// carried as a Packet's Data, PTS from the enclosing PES packet.
	// Deliberately outside IsVideo()/IsAudio()'s ranges, same rationale as
	// CodecSCTE35.
	CodecDVBTeletext
)

func (c CodecType) String() string {
	switch c {
	case CodecH264:
		return "h264"
	case CodecH265:
		return "h265"
	case CodecAV1:
		return "av1"
	case CodecVP6:
		return "vp6"
	case CodecVP8:
		return "vp8"
	case CodecVP9:
		return "vp9"
	case CodecAAC:
		return "aac"
	case CodecMP3:
		return "mp3"
	case CodecOpus:
		return "opus"
	case CodecAC3:
		return "ac3"
	case CodecEAC3:
		return "eac3"
	case CodecSpeex:
		return "speex"
	case CodecMP2:
		return "mp2"
	case CodecSCTE35:
		return "scte35"
	case CodecDVBTeletext:
		return "dvb_teletext"
	default:
		return "unknown"
	}
}

func (c CodecType) IsVideo() bool {
	return c >= CodecH264 && c <= CodecVP9
}

func (c CodecType) IsAudio() bool {
	return c >= CodecAAC && c <= CodecMP2
}

// PacketType distinguishes data packets from in-band control units.
type PacketType uint8

const (
	PacketMedia    PacketType = iota // normal audio/video data
	PacketConfig                     // codec config / sequence header
	PacketKeyframe                   // video keyframe (implies PacketMedia)
	PacketEnd                        // stream end signal
)

// Packet is the universal media unit that flows through the pipeline.
// It carries a single audio or video access unit with all metadata needed
// for any downstream muxer or packager to handle it without talking back
// to the source.
//
// Zero-copy philosophy: Data is a byte slice whose backing array may be
// shared.  Consumers must not modify Data; they may retain it as long as
// needed.
type Packet struct {
	// Type distinguishes keyframes, config records, and regular data.
	Type PacketType

	// Codec identifies the codec.
	Codec CodecType

	// DTS is the decoding timestamp in 90 kHz ticks (MPEG convention).
	// All timestamps in the pipeline use this unit for uniformity.
	DTS int64

	// PTS is the presentation timestamp in 90 kHz ticks.
	// For audio: PTS == DTS.
	PTS int64

	// Duration in 90 kHz ticks.  0 means unknown (common for video).
	Duration int64

	// IsKeyframe is true for the first packet of a closed GOP.
	IsKeyframe bool

	// Data contains the raw codec payload.
	//  H.264: Annex-B start codes (0x00000001 + NAL)
	//  AAC:   ADTS frame (0xFF 0xF? ...)
	//  SCTE-35: the complete, unmodified splice_info_section (see SCTE35)
	//  etc.
	Data []byte

	// SCTE35 is the parsed form of Data when Codec == CodecSCTE35; nil for
	// every other codec. Data still carries the raw section (needed for
	// verbatim MPEG-TS pass-through); this is the parsed view HLS ad-marker
	// generation consumes instead of re-parsing Data itself.
	SCTE35 *scte35.Event

	// CompositionTimeOffset is PTS - DTS in 90 kHz ticks (H.264 B-frames).
	CompositionTimeOffset int64

	// WallClock is the wall-clock time the packet arrived (used for DVR
	// indexing and failover watchdog).
	WallClock time.Time

	// StreamID ties a packet back to its source stream name.
	StreamID string

	// VariantIndex is the ABR variant index (0 for non-ABR streams).
	VariantIndex int
}

// CodecInfo carries the codec parameters extracted from the sequence header.
// These are populated once per stream when the encoder sends its config record.
type CodecInfo struct {
	VideoCodec           CodecType
	AudioCodec           CodecType
	Width                int
	Height               int
	FrameRate            float64
	SampleRate           int
	Channels             int
	Profile              uint8 // H.264 profile_idc
	ProfileCompatibility uint8 // H.264 profile_compatibility (byte 2 of AVCDecoderConfig)
	Level                uint8 // H.264 level_idc
	BitDepth             uint8

	// Raw VPS/SPS/PPS bytes. VPSNALu is HEVC-only (nil for H.264); SPSNALu/
	// PPSNALu are used by both (needed to build fMP4 init segment and HLS).
	VPSNALu []byte
	SPSNALu []byte
	PPSNALu []byte

	// HEVC profile/tier/level fields (RFC 6381 hvc1 codec string), extracted
	// from the SPS's profile_tier_level() — see ParseHEVCSPS.
	// Unset (all zero) for other codecs.
	HEVCProfileSpace  uint8
	HEVCTierFlag      bool
	HEVCProfileIDC    uint8
	HEVCProfileCompat uint32
	HEVCLevelIDC      uint8

	// Raw AudioSpecificConfig bytes for AAC.
	AACConfig []byte

	// HasCEA608708 is true once an SEI NAL carrying CEA-608/708 closed
	// captions (see HasCEA608708 func) has been observed in the source —
	// sticky for the life of the stream (never reset to false once set),
	// so a caption that only appears on some frames still gets advertised.
	// Drives HLS's #EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS signaling.
	HasCEA608708 bool

	// CEA708DTVCCService is true once a caption SEI has been observed
	// carrying real CEA-708 DTVCC service data (see
	// UsesCEA708DTVCCService), as opposed to plain legacy CEA-608 line-21
	// field pairs — real modern encoders commonly send 708, not 608, even
	// though HasCEA608708 is true either way. Also sticky. Selects HLS's
	// INSTREAM-ID convention: "SERVICE1" for 708, "CC1" for 608 — using
	// the wrong one makes a compliant player correctly find no data on
	// the channel it was told to look at and render nothing, even though
	// real caption data is present (see UsesCEA708DTVCCService's doc
	// comment for how this was confirmed against a real encoder).
	CEA708DTVCCService bool

	// HasDVBTeletextSubtitle is true once the PMT's teletext_descriptor
	// (ETSI EN 300 468) has identified a teletext_type 0x02/0x05 (subtitle)
	// page on this stream — a completely different transport (page-based
	// EBU/ITU-R BT.653 teletext, ETSI EN 300 472/706) from CEA-608/708's
	// SEI-embedded data, decoded by internal/teletext instead of
	// internal/captiondecode. TeletextMagazine/TeletextPage identify which
	// page carries the subtitle content (a teletext PID commonly multiplexes
	// many unrelated pages).
	HasDVBTeletextSubtitle bool
	TeletextMagazine       int
	TeletextPage           int
}
