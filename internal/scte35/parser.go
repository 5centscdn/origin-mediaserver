package scte35

import (
	"encoding/binary"
	"errors"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/mux"
)

// Wire splice_command_type values, per SCTE 35.
const (
	wireSpliceNull           = 0x00
	wireSpliceSchedule       = 0x04
	wireSpliceInsert         = 0x05
	wireTimeSignal           = 0x06
	wireBandwidthReservation = 0x07
	wirePrivateCommand       = 0xFF
)

// Descriptor tags, per SCTE 35 Table 17.
const (
	tagAvailDescriptor        = 0x00
	tagDTMFDescriptor         = 0x01
	tagSegmentationDescriptor = 0x02
	tagTimeDescriptor         = 0x03
	tagAudioDescriptor        = 0x04
)

const tableIDSpliceInfo = 0xFC

var (
	// ErrTooShort is returned when section is shorter than a minimal
	// splice_info_section header.
	ErrTooShort = errors.New("scte35: section too short")
	// ErrBadTableID is returned when section's table_id isn't 0xFC.
	ErrBadTableID = errors.New("scte35: not a splice_info_section (bad table_id)")
	// ErrTruncated is returned when section_length claims more bytes than
	// are actually present — e.g. a section that spans multiple TS packets,
	// which this parser (like this codebase's PAT/PMT parsers) doesn't
	// reassemble.
	ErrTruncated = errors.New("scte35: section truncated / spans multiple packets")
	// ErrCRCMismatch is returned when the section's CRC_32 doesn't match —
	// the caller should log and drop the section, not crash or abort the
	// stream.
	ErrCRCMismatch = errors.New("scte35: CRC32 mismatch")
)

// Parse parses a raw splice_info_section — as found directly on an MPEG-TS
// private-section PID with pointer_field already stripped — into an Event.
//
// On any error, ev is nil: unlike a malformed PAT/PMT (which this codebase
// already tolerates by simply not updating PID state), a malformed SCTE-35
// section must never be forwarded as if it were valid, so callers doing
// MPEG-TS pass-through check the error and drop the section rather than
// writing it through.
func Parse(section []byte) (*Event, error) {
	if len(section) < 14 {
		return nil, ErrTooShort
	}
	if section[0] != tableIDSpliceInfo {
		return nil, ErrBadTableID
	}

	sectionLength := int(binary.BigEndian.Uint16(section[1:3]) & 0x0FFF)
	total := 3 + sectionLength
	if total > len(section) || total < 14+4 {
		return nil, ErrTruncated
	}
	section = section[:total] // drop any trailing stuffing/next-section bytes

	crcOffset := total - 4
	gotCRC := binary.BigEndian.Uint32(section[crcOffset:])
	wantCRC := mux.CRC32MPEG2(section[:crcOffset])
	if gotCRC != wantCRC {
		return nil, ErrCRCMismatch
	}

	ev := &Event{
		RawSection: append([]byte(nil), section...),
		ReceivedAt: time.Now(),
	}

	// pts_adjustment: 33 bits — low bit of byte4, then all of bytes 5-8.
	ptsAdj := (uint64(section[4]&0x01) << 32) | uint64(binary.BigEndian.Uint32(section[5:9]))
	ev.PTSAdjustment = int64(ptsAdj)

	// tier (12 bits) + splice_command_length (12 bits) span bytes 10-12.
	splen := (int(section[11]&0x0F) << 8) | int(section[12])
	cmdType := section[13]
	ev.RawCommandType = cmdType
	ev.CommandType = classifyCommand(cmdType)

	cmdStart := 14
	cmdEnd := cmdStart + splen
	if cmdEnd < cmdStart || cmdEnd > crcOffset {
		// splice_command_length doesn't fit — can't safely locate the
		// descriptor loop either, but RawSection/CommandType are still
		// useful to a pass-through-only caller, so return what we have
		// rather than erroring out the whole section.
		return ev, nil
	}
	cmdBody := section[cmdStart:cmdEnd]

	switch cmdType {
	case wireTimeSignal:
		if pts, ok := parseSpliceTime(cmdBody); ok {
			ev.PTS = pts
			ev.HasPTS = true
		}
	case wireSpliceInsert:
		parseSpliceInsert(cmdBody, ev)
	}

	// descriptor_loop_length (16 bits) immediately follows the command.
	descLoopStart := cmdEnd
	if descLoopStart+2 > crcOffset {
		return ev, nil
	}
	descLoopLen := int(binary.BigEndian.Uint16(section[descLoopStart : descLoopStart+2]))
	pos := descLoopStart + 2
	descEnd := pos + descLoopLen
	if descEnd > crcOffset {
		descEnd = crcOffset // tolerate a bad length rather than aborting the whole section
	}

	for pos+2 <= descEnd {
		tag := section[pos]
		dlen := int(section[pos+1])
		dEnd := pos + 2 + dlen
		if dEnd > descEnd {
			break // malformed trailing descriptor — keep what parsed so far
		}
		raw := section[pos:dEnd]
		ev.RawDescriptors = append(ev.RawDescriptors, append([]byte(nil), raw...))
		if tag == tagSegmentationDescriptor && dlen >= 4 {
			parseSegmentationDescriptor(raw[2:], ev) // skip descriptor_tag + descriptor_length
		}
		pos = dEnd
	}

	return ev, nil
}

func classifyCommand(cmdType uint8) CommandType {
	switch cmdType {
	case wireSpliceNull:
		return CommandSpliceNull
	case wireTimeSignal:
		return CommandTimeSignal
	case wireSpliceInsert:
		return CommandSpliceInsert
	case wireSpliceSchedule:
		return CommandSpliceSchedule
	case wireBandwidthReservation:
		return CommandBandwidthReservation
	case wirePrivateCommand:
		return CommandPrivate
	default:
		return CommandUnknown
	}
}

// parseSpliceTime parses a splice_time() structure and returns (pts, true)
// if time_specified_flag was set; (0, false) otherwise (including "not
// enough bytes", which is treated as "no time specified" rather than an
// error — the caller already has the full RawSection for a definitive
// answer if needed).
func parseSpliceTime(b []byte) (int64, bool) {
	if len(b) < 1 || b[0]&0x80 == 0 {
		return 0, false
	}
	if len(b) < 5 {
		return 0, false
	}
	pts := (uint64(b[0]&0x01) << 32) | uint64(binary.BigEndian.Uint32(b[1:5]))
	return int64(pts), true
}

// spliceTimeLen reports how many bytes a splice_time() occupies: 1 byte
// when time_specified_flag is unset, 5 bytes when it's set.
func spliceTimeLen(b []byte) int {
	if len(b) < 1 || b[0]&0x80 == 0 {
		return 1
	}
	return 5
}

// parseSpliceInsert parses a splice_insert() command body into ev. Only the
// program_splice_flag=1 (whole-program, not component-level) case is fully
// modeled — component-level splicing is rare in practice, and RawSection
// already preserves the exact bytes for any pass-through consumer, so this
// simply stops populating further fields for that case rather than
// guessing at the component loop layout.
func parseSpliceInsert(b []byte, ev *Event) {
	if len(b) < 5 {
		return
	}
	ev.EventID = binary.BigEndian.Uint32(b[0:4])
	ev.HasEventID = true
	if b[4]&0x80 != 0 { // splice_event_cancel_indicator
		return
	}
	if len(b) < 6 {
		return
	}
	flags := b[5]
	ev.OutOfNetwork = flags&0x80 != 0
	programSpliceFlag := flags&0x40 != 0
	durationFlag := flags&0x20 != 0
	spliceImmediateFlag := flags&0x10 != 0

	pos := 6
	if !programSpliceFlag {
		return // component-level splicing — not modeled, see doc comment
	}
	if !spliceImmediateFlag {
		if pos >= len(b) {
			return
		}
		if pts, ok := parseSpliceTime(b[pos:]); ok {
			ev.PTS = pts
			ev.HasPTS = true
		}
		pos += spliceTimeLen(b[pos:])
	}

	if durationFlag {
		if pos+5 <= len(b) {
			ev.AutoReturn = b[pos]&0x80 != 0
			dur := (uint64(b[pos]&0x01) << 32) | uint64(binary.BigEndian.Uint32(b[pos+1:pos+5]))
			ev.BreakDurationMS = ticksToMS(int64(dur))
			ev.HasBreakDuration = true
		}
	}
	// unique_program_id / avail_num / avails_expected follow but aren't
	// modeled on Event — RawSection retains them for anyone who needs them.
}

// parseSegmentationDescriptor parses a segmentation_descriptor()'s
// identifier+private-data bytes (i.e. everything after descriptor_tag and
// descriptor_length) into ev.
func parseSegmentationDescriptor(b []byte, ev *Event) {
	if len(b) < 4 {
		return
	}
	b = b[4:] // skip the 4-byte identifier ("CUEI")
	if len(b) < 5 {
		return
	}
	ev.SegmentationEventID = binary.BigEndian.Uint32(b[0:4])
	ev.HasSegmentation = true
	if b[4]&0x80 != 0 { // segmentation_event_cancel_indicator
		return
	}
	if len(b) < 6 {
		return
	}
	flags := b[5]
	programSegFlag := flags&0x80 != 0
	segDurationFlag := flags&0x40 != 0

	pos := 6
	if !programSegFlag {
		if pos >= len(b) {
			return
		}
		componentCount := int(b[pos])
		pos++
		pos += componentCount * 6 // component_tag(1) + reserved(1) + pts_offset(5, 33 bits)
		if pos > len(b) {
			return
		}
	}

	if segDurationFlag {
		if pos+5 > len(b) {
			return
		}
		dur := (uint64(b[pos]) << 32) | uint64(binary.BigEndian.Uint32(b[pos+1:pos+5]))
		ev.SegmentationDurationMS = ticksToMS(int64(dur))
		ev.HasSegmentationDuration = true
		pos += 5
	}

	if pos+2 > len(b) {
		return
	}
	ev.UPIDType = b[pos]
	upidLen := int(b[pos+1])
	pos += 2
	if pos+upidLen > len(b) {
		return
	}
	ev.UPID = append([]byte(nil), b[pos:pos+upidLen]...)
	pos += upidLen

	if pos < len(b) {
		ev.SegmentationTypeID = b[pos]
	}
}

// ticksToMS converts a 90kHz tick count to milliseconds.
func ticksToMS(ticks int64) int64 {
	return ticks * 1000 / 90000
}
