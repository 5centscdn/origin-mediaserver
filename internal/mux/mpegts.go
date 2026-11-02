// Package mux provides pure-Go media muxers.  No FFmpeg or external binaries.
package mux

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// ─── Constants ────────────────────────────────────────────────────────────────

const (
	TSPacketSize = 188
	TSSyncByte   = 0x47

	PIDPat    = 0x0000
	PIDNull   = 0x1FFF
	PIDPmt    = 0x1000
	PIDVideo  = 0x0100
	PIDAudio  = 0x0101
	PIDSCTE35 = 0x0102

	streamTypeH264   = 0x1B
	streamTypeH265   = 0x24
	streamTypeAV1    = 0x06 // private (AV1 uses 0x06 + descriptor in practice)
	streamTypeAAC    = 0x0F // ADTS AAC
	streamTypeMP3    = 0x03 // MPEG-1/2 Audio
	streamTypeAC3    = 0x81 // private — AC3 per ATSC
	streamTypeOpus   = 0x06 // private — Opus per DVB
	streamTypeSCTE35 = 0x86 // SCTE-35 splice_info_section, per ANSI/SCTE 35

	pesStartCode = 0x000001
	pesVideoID   = 0xE0
	pesAudioID   = 0xC0
)

// TSMuxer writes a single-program MPEG-TS bitstream to an io.Writer.
// It is stateless except for continuity counters and PAT/PMT intervals,
// making it safe to use per-segment (reset counters at the start of each).
type TSMuxer struct {
	w io.Writer

	videoPID   uint16
	audioPID   uint16
	videoSType uint8 // stream type in PMT
	audioSType uint8

	videoCC   uint8
	audioCC   uint8
	patCC     uint8
	pmtCC     uint8
	scte35CC  uint8
	scte35PID uint16 // 0 = disabled; set via EnableSCTE35

	pmtInterval int // write PAT+PMT every N packets (default 10)
	pktCount    int

	// audioOnly is set by NewTSMuxerAudioOnly for sources that never have a
	// video track at all (e.g. Icecast) — writePMT omits the video ES
	// entry entirely and points PCR_PID at the audio PID instead of always
	// declaring a video stream that will never carry a single packet.
	audioOnly bool

	// Parameter sets, inserted before each keyframe. H.264 uses sps/pps
	// only; HEVC additionally uses vps (see SetHEVCParamSets) — both share
	// the same insertion point in WriteVideo, just with vps prepended
	// first when present.
	vps []byte
	sps []byte
	pps []byte
}

// NewTSMuxer creates a muxer that writes to w.
func NewTSMuxer(w io.Writer, videoStreamType, audioStreamType uint8) *TSMuxer {
	return &TSMuxer{
		w:           w,
		videoPID:    PIDVideo,
		audioPID:    PIDAudio,
		videoSType:  videoStreamType,
		audioSType:  audioStreamType,
		pmtInterval: 10,
	}
}

// NewTSMuxerAudioOnly creates a muxer for a source that will never have a
// video track (e.g. Icecast) — WriteVideo must not be called on the result.
func NewTSMuxerAudioOnly(w io.Writer, audioStreamType uint8) *TSMuxer {
	return &TSMuxer{
		w:           w,
		audioPID:    PIDAudio,
		audioSType:  audioStreamType,
		audioOnly:   true,
		pmtInterval: 10,
	}
}

// SetSPSPPS provides H.264 parameter sets to be inserted before keyframes.
func (m *TSMuxer) SetSPSPPS(sps, pps []byte) {
	m.sps = sps
	m.pps = pps
}

// SetHEVCParamSets provides HEVC (H.265) parameter sets to be inserted
// before keyframes — VPS in addition to SPS/PPS, in that required order.
func (m *TSMuxer) SetHEVCParamSets(vps, sps, pps []byte) {
	m.vps = vps
	m.sps = sps
	m.pps = pps
}

// EnableSCTE35 turns on SCTE-35 pass-through for this muxer instance: once
// called, writePMT advertises a stream_type=0x86 elementary stream (with a
// "CUEI" registration_descriptor, the real-world convention downstream SSAI
// tools expect) on PIDSCTE35, and WriteSCTE35 becomes usable. Left unset
// (the default), a muxer's output is byte-for-byte identical to before this
// feature existed — callers (internal/hls, internal/dvr) only call this
// when the stream's resolved scte35_passthrough config is true.
func (m *TSMuxer) EnableSCTE35() {
	m.scte35PID = PIDSCTE35
}

// WriteSCTE35 writes one splice_info_section verbatim — byte-for-byte, no
// rebuilding — as a single MPEG-2 private section on the SCTE-35 PID, the
// same way writePAT/writePMT write PSI sections. Every call writes exactly
// one section; true 1:1 pass-through, with no de-duplication at this layer
// (real encoders legitimately retransmit a section for reliability, and
// MPEG-TS pass-through must preserve that, unlike HLS marker generation —
// see internal/scte35.StateMachine, which is where retransmission
// suppression actually belongs). No-op error if EnableSCTE35 wasn't called.
func (m *TSMuxer) WriteSCTE35(section []byte) error {
	if m.scte35PID == 0 {
		return fmt.Errorf("mux: WriteSCTE35 called without EnableSCTE35")
	}
	payload := append([]byte{0x00}, section...) // pointer_field = 0
	return m.writeTSPacket(m.scte35PID, true, payload, &m.scte35CC)
}

// WriteVideo writes one video access unit.
// dts and pts are in 90 kHz ticks.
// isKeyframe triggers PAT/PMT and parameter-set insertion.
func (m *TSMuxer) WriteVideo(data []byte, dts, pts int64, isKeyframe bool) error {
	if isKeyframe {
		// Always precede keyframes with PAT+PMT so players can join mid-stream.
		if err := m.writePATPMT(); err != nil {
			return err
		}
		// Insert parameter sets before keyframes: VPS (HEVC only) + SPS + PPS.
		if len(m.vps) > 0 || len(m.sps) > 0 || len(m.pps) > 0 {
			var extraNALs []byte
			startCode := []byte{0x00, 0x00, 0x00, 0x01}
			if len(m.vps) > 0 {
				extraNALs = append(extraNALs, startCode...)
				extraNALs = append(extraNALs, m.vps...)
			}
			if len(m.sps) > 0 {
				extraNALs = append(extraNALs, startCode...)
				extraNALs = append(extraNALs, m.sps...)
			}
			if len(m.pps) > 0 {
				extraNALs = append(extraNALs, startCode...)
				extraNALs = append(extraNALs, m.pps...)
			}
			data = append(extraNALs, data...)
		}
	} else if m.pktCount%m.pmtInterval == 0 {
		if err := m.writePATPMT(); err != nil {
			return err
		}
	}
	m.pktCount++

	pes := buildVideoPES(data, dts, pts)
	return m.writePayload(m.videoPID, pes, isKeyframe, &m.videoCC)
}

// WriteAudio writes one audio access unit.
// pts is in 90 kHz ticks.
func (m *TSMuxer) WriteAudio(data []byte, pts int64) error {
	if m.audioOnly {
		// There's no keyframe to key off of (no video at all), so PAT/PMT
		// insertion — needed periodically so players can join mid-stream —
		// has to be driven by the audio packet count instead of WriteVideo's
		// keyframe/pmtInterval logic.
		if m.pktCount%m.pmtInterval == 0 {
			if err := m.writePATPMT(); err != nil {
				return err
			}
		}
		m.pktCount++
	}
	pes := buildAudioPES(data, pts)
	return m.writePayload(m.audioPID, pes, false, &m.audioCC)
}

// ─── PAT / PMT ───────────────────────────────────────────────────────────────

func (m *TSMuxer) writePATPMT() error {
	if err := m.writePAT(); err != nil {
		return err
	}
	return m.writePMT()
}

func (m *TSMuxer) writePAT() error {
	// PAT section payload (without CRC — we add it below)
	var section bytes.Buffer
	section.Write([]byte{
		0x00,       // table_id = PAT
		0xB0, 0x0D, // section_syntax_indicator=1, 0, reserved=11, section_length=13
		0x00, 0x01, // transport_stream_id = 1
		0xC1, // reserved=11, version=0, current_next=1
		0x00, // section_number = 0
		0x00, // last_section_number = 0
		// Program 1 → PMT PID 0x1000
		0x00, 0x01, // program_number = 1
		0x10, 0x00, // reserved=111, PMT_PID = 0x1000
	})

	crc32Bytes := make([]byte, 4)
	binary.BigEndian.PutUint32(crc32Bytes, crc32MPEG2(section.Bytes()))
	section.Write(crc32Bytes)

	payload := append([]byte{0x00}, section.Bytes()...) // pointer_field = 0
	return m.writeTSPacket(PIDPat, true, payload, &m.patCC)
}

func (m *TSMuxer) writePMT() error {
	// Build elementary stream info. audioOnly sources (Icecast) never write
	// a video packet at all — declaring a video ES here would advertise a
	// PID no player will ever see a single byte on.
	var esInfo []byte
	pcrPID := m.videoPID
	if !m.audioOnly {
		esInfo = append(esInfo,
			m.videoSType,
			byte(m.videoPID>>8)|0xE0, byte(m.videoPID),
			0xF0, 0x00, // reserved, ES_info_length = 0
		)
	} else {
		pcrPID = m.audioPID
	}
	esInfo = append(esInfo,
		m.audioSType,
		byte(m.audioPID>>8)|0xE0, byte(m.audioPID),
		0xF0, 0x00,
	)
	if m.scte35PID != 0 {
		// registration_descriptor (tag 0x05, length 4, "CUEI") — the
		// real-world SCTE-35/ANSI convention downstream ad-insertion tools
		// look for to recognize the stream_type=0x86 ES as SCTE-35.
		esInfo = append(esInfo,
			streamTypeSCTE35,
			byte(m.scte35PID>>8)|0xE0, byte(m.scte35PID),
			0xF0, 0x06, // ES_info_length = 6 (2-byte descriptor header + 4-byte "CUEI")
			0x05, 0x04, 'C', 'U', 'E', 'I',
		)
	}

	sectionLen := 9 + len(esInfo) + 4 // fixed + esInfo + CRC

	var section bytes.Buffer
	section.Write([]byte{
		0x02,                           // table_id = PMT
		byte(0xB0 | (sectionLen >> 8)), // section_syntax + length high
		byte(sectionLen),               // length low
		0x00, 0x01,                     // program_number = 1
		0xC1,                                     // version=0, current_next=1
		0x00,                                     // section_number
		0x00,                                     // last_section_number
		byte(0xE0 | (pcrPID >> 8)), byte(pcrPID), // reserved + PCR_PID
		0xF0, 0x00, // reserved + program_info_length = 0
	})
	section.Write(esInfo)

	crc32Bytes := make([]byte, 4)
	binary.BigEndian.PutUint32(crc32Bytes, crc32MPEG2(section.Bytes()))
	section.Write(crc32Bytes)

	payload := append([]byte{0x00}, section.Bytes()...)
	return m.writeTSPacket(PIDPmt, true, payload, &m.pmtCC)
}

// ─── PES building ─────────────────────────────────────────────────────────────

func buildVideoPES(payload []byte, dts, pts int64) []byte {
	var pes bytes.Buffer

	// PES start code + stream_id
	pes.Write([]byte{0x00, 0x00, 0x01, pesVideoID})

	// PES packet length — 0 means unbounded (valid for video)
	pes.Write([]byte{0x00, 0x00})

	// Optional PES header flags
	ptsDtsFlags := byte(0xC0) // both PTS and DTS present
	pes.Write([]byte{
		0x80, // marker bits + PES_scrambling_control + flags
		ptsDtsFlags,
		0x0A, // PES_header_data_length = 10 (5 bytes PTS + 5 bytes DTS)
	})

	// PTS (5 bytes)
	pes.Write(encodePTSDTS(pts, 0x03))
	// DTS (5 bytes)
	pes.Write(encodePTSDTS(dts, 0x01))

	// Payload
	pes.Write(payload)

	return pes.Bytes()
}

func buildAudioPES(payload []byte, pts int64) []byte {
	var pes bytes.Buffer

	pes.Write([]byte{0x00, 0x00, 0x01, pesAudioID})

	pesLen := 8 + len(payload) // 3 optional PES header bytes + 5 PTS bytes + payload
	pes.Write([]byte{byte(pesLen >> 8), byte(pesLen)})

	pes.Write([]byte{
		0x80, // flags
		0x80, // PTS only
		0x05, // header_data_length = 5
	})

	pes.Write(encodePTSDTS(pts, 0x02))

	pes.Write(payload)
	return pes.Bytes()
}

// encodePTSDTS encodes a 33-bit timestamp into the 5-byte PTS/DTS field.
// markerHigh: 0x02 for PTS-only, 0x03 for PTS when DTS follows, 0x01 for DTS.
func encodePTSDTS(ts int64, markerHigh byte) []byte {
	b := make([]byte, 5)
	b[0] = (markerHigh << 4) | byte((ts>>29)&0x0E) | 0x01
	b[1] = byte(ts >> 22)
	b[2] = byte(ts>>14)&0xFE | 0x01
	b[3] = byte(ts >> 7)
	b[4] = byte(ts<<1)&0xFE | 0x01
	return b
}

// ─── TS packet writing ────────────────────────────────────────────────────────

// writePayload splits a PES into 188-byte TS packets.
func (m *TSMuxer) writePayload(pid uint16, data []byte, randomAccess bool, cc *uint8) error {
	first := true
	for len(data) > 0 {
		pktData := make([]byte, TSPacketSize)
		pktData[0] = TSSyncByte

		// PID
		pktData[1] = byte(pid >> 8)
		if first {
			pktData[1] |= 0x40 // PUSI: set on first TS packet of every PES
		}
		pktData[2] = byte(pid)

		// adaptation_field_control = 01 (payload only) for most packets
		// When there's not enough data to fill the packet, use adaptation field padding.
		payloadAvail := TSPacketSize - 4
		if first && randomAccess {
			// No extra AF needed for keyframe — just payload
		}

		var payload []byte
		if len(data) >= payloadAvail {
			payload = data[:payloadAvail]
			data = data[payloadAvail:]
		} else {
			// Pad with adaptation field
			padding := payloadAvail - len(data)
			payload = data
			data = nil

			// Build adaptation field: 1 byte length + (padding>1: 1 byte
			// flags + (padding-2) stuffing bytes). padding==1 needs an AF
			// too — a bare adaptation_field_length=0 byte, no flags/
			// stuffing following — NOT the "no AF, just payload" path
			// below: that path only has 184 bytes of room, so a 183-byte
			// payload plus no AF would leave one uninitialized zero byte
			// at the end of the packet, which a conformant demuxer reads
			// back as genuine stream data (a real bug this exact `if`
			// used to have: `padding >= 2` silently let padding==1 fall
			// through to the no-AF branch below).
			if padding >= 1 {
				af := make([]byte, padding)
				af[0] = byte(padding - 1)
				if padding > 1 {
					af[1] = 0x00 // no flags
					for i := 2; i < padding; i++ {
						af[i] = 0xFF // stuffing
					}
				}
				pktData[3] = 0x30 | (*cc & 0x0F) // adaptation + payload
				copy(pktData[4:], af)
				copy(pktData[4+padding:], payload)
				*cc = (*cc + 1) & 0x0F
				if _, err := m.w.Write(pktData); err != nil {
					return err
				}
				first = false
				continue
			}
		}

		pktData[3] = 0x10 | (*cc & 0x0F) // payload only
		copy(pktData[4:], payload)
		*cc = (*cc + 1) & 0x0F

		if _, err := m.w.Write(pktData); err != nil {
			return err
		}
		first = false
	}
	return nil
}

func (m *TSMuxer) writeTSPacket(pid uint16, pusi bool, payload []byte, cc *uint8) error {
	pkt := make([]byte, TSPacketSize)
	pkt[0] = TSSyncByte
	pkt[1] = byte(pid >> 8)
	if pusi {
		pkt[1] |= 0x40
	}
	pkt[2] = byte(pid)

	avail := TSPacketSize - 4
	var stuffLen int
	if len(payload) < avail {
		stuffLen = avail - len(payload)
	}

	if stuffLen > 0 {
		// Adaptation field with stuffing bytes
		af := make([]byte, stuffLen)
		af[0] = byte(stuffLen - 1)
		if stuffLen > 1 {
			af[1] = 0x00
		}
		for i := 2; i < stuffLen; i++ {
			af[i] = 0xFF
		}
		pkt[3] = 0x30 | (*cc & 0x0F)
		copy(pkt[4:], af)
		copy(pkt[4+stuffLen:], payload)
	} else {
		pkt[3] = 0x10 | (*cc & 0x0F)
		copy(pkt[4:], payload)
	}

	*cc = (*cc + 1) & 0x0F
	_, err := m.w.Write(pkt)
	return err
}

// ─── CRC32/MPEG-2 ─────────────────────────────────────────────────────────────

var crc32Table [256]uint32

func init() {
	for i := 0; i < 256; i++ {
		crc := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
		crc32Table[i] = crc
	}
}

func crc32MPEG2(data []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, b := range data {
		crc = (crc << 8) ^ crc32Table[((crc>>24)^uint32(b))&0xFF]
	}
	return crc
}
