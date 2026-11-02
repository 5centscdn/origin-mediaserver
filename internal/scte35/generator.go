package scte35

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/mux"
)

// SpliceInsertOptions describes a splice_insert() command to build.
type SpliceInsertOptions struct {
	EventID      uint32
	OutOfNetwork bool // true = OUT (ad break start), false = IN (return to network)

	// PTS is the splice point in the stream's own 90kHz PTS domain — the
	// caller (internal/scte35ctrl) is responsible for deriving this from
	// the stream's actual tracked media clock, never wall-clock. HasPTS
	// false means splice_immediate_flag=1 (act now, no specific time).
	PTS    int64
	HasPTS bool

	// DurationMS/AutoReturn populate break_duration(); DurationMS <= 0
	// means duration_flag=0 (no declared duration — typically only
	// meaningful on an OUT).
	DurationMS int64
	AutoReturn bool

	UniqueProgramID uint16
	AvailNum        uint8
	AvailsExpected  uint8
}

// TimeSignalOptions describes a time_signal() command, always paired with a
// segmentation_descriptor (a time_signal with no segmentation_descriptor
// carries no event identity at all, so this generator always includes one).
type TimeSignalOptions struct {
	PTS    int64
	HasPTS bool

	SegmentationEventID    uint32
	SegmentationTypeID     uint8
	SegmentationDurationMS int64 // <= 0 means segmentation_duration_flag=0
}

// BuildSpliceInsert constructs a complete, CRC32-valid splice_info_section
// carrying a splice_insert() command, per the SCTE-35 field checklist:
// table_id, section_length, protocol_version, pts_adjustment (always 0 for
// a freshly generated section — there is no prior redistribution delay to
// compensate for), tier, splice_command_type, splice_event_id,
// out_of_network_indicator, program_splice_flag (always 1 — whole program,
// matching parser.go's only fully-modeled case), duration_flag,
// break_duration+auto_return, unique_program_id, avail_num,
// avails_expected, and CRC32.
func BuildSpliceInsert(opts SpliceInsertOptions) (*Event, error) {
	var cmd []byte
	cmd = binary.BigEndian.AppendUint32(cmd, opts.EventID)
	cmd = append(cmd, 0x7F) // splice_event_cancel_indicator=0, reserved=1111111

	flags := byte(0x40) // program_splice_flag=1
	if opts.OutOfNetwork {
		flags |= 0x80
	}
	durationFlag := opts.DurationMS > 0
	if durationFlag {
		flags |= 0x20
	}
	spliceImmediate := !opts.HasPTS
	if spliceImmediate {
		flags |= 0x10
	}
	flags |= 0x0F // reserved bits set (matches common real-world encoder output)
	cmd = append(cmd, flags)

	if !spliceImmediate {
		cmd = append(cmd, encodeSpliceTime(opts.PTS)...)
	}
	if durationFlag {
		cmd = append(cmd, encodeBreakDuration(opts.DurationMS, opts.AutoReturn)...)
	}
	cmd = binary.BigEndian.AppendUint16(cmd, opts.UniqueProgramID)
	cmd = append(cmd, opts.AvailNum, opts.AvailsExpected)

	section, err := assembleSection(wireSpliceInsert, cmd, nil)
	if err != nil {
		return nil, err
	}

	ev := &Event{
		RawSection:       section,
		CommandType:      CommandSpliceInsert,
		RawCommandType:   wireSpliceInsert,
		HasEventID:       true,
		EventID:          opts.EventID,
		OutOfNetwork:     opts.OutOfNetwork,
		HasBreakDuration: durationFlag,
		BreakDurationMS:  opts.DurationMS,
		AutoReturn:       opts.AutoReturn && durationFlag,
		ReceivedAt:       time.Now(),
		Source:           SourceInserted,
	}
	if !spliceImmediate {
		ev.PTS = opts.PTS
		ev.HasPTS = true
	}
	return ev, nil
}

// BuildTimeSignal constructs a complete, CRC32-valid splice_info_section
// carrying a time_signal() command plus a segmentation_descriptor — a
// time_signal with no descriptor carries no usable event identity, so this
// generator always attaches one, per the field checklist's
// "segmentation_descriptor where requested."
func BuildTimeSignal(opts TimeSignalOptions) (*Event, error) {
	var cmd []byte
	spliceImmediate := !opts.HasPTS
	if spliceImmediate {
		cmd = append(cmd, 0x7F) // time_specified_flag=0, reserved
	} else {
		cmd = append(cmd, encodeSpliceTime(opts.PTS)...)
	}

	var desc []byte
	desc = append(desc, []byte("CUEI")...)
	desc = binary.BigEndian.AppendUint32(desc, opts.SegmentationEventID)
	desc = append(desc, 0x00) // segmentation_event_cancel_indicator=0

	durationFlag := opts.SegmentationDurationMS > 0
	flags := byte(0x80) // program_segmentation_flag=1 (whole program)
	if durationFlag {
		flags |= 0x40
	}
	flags |= 0x20 // delivery_not_restricted_flag=1 (no further restriction byte)
	flags |= 0x1F // reserved bits
	desc = append(desc, flags)

	if durationFlag {
		desc = append(desc, encodeSegmentationDuration(opts.SegmentationDurationMS)...)
	}
	desc = append(desc, 0x00, 0x00) // segmentation_upid_type=0x00 (not used), segmentation_upid_length=0
	desc = append(desc, opts.SegmentationTypeID)
	desc = append(desc, 0x00, 0x00) // segment_num, segments_expected

	descLoop := append([]byte{tagSegmentationDescriptor, byte(len(desc))}, desc...)

	section, err := assembleSection(wireTimeSignal, cmd, descLoop)
	if err != nil {
		return nil, err
	}

	ev := &Event{
		RawSection:              section,
		CommandType:             CommandTimeSignal,
		RawCommandType:          wireTimeSignal,
		HasSegmentation:         true,
		SegmentationEventID:     opts.SegmentationEventID,
		SegmentationTypeID:      opts.SegmentationTypeID,
		HasSegmentationDuration: durationFlag,
		SegmentationDurationMS:  opts.SegmentationDurationMS,
		OutOfNetwork:            isSegmentationStart(opts.SegmentationTypeID),
		ReceivedAt:              time.Now(),
		Source:                  SourceInserted,
	}
	if !spliceImmediate {
		ev.PTS = opts.PTS
		ev.HasPTS = true
	}
	return ev, nil
}

// encodeSpliceTime builds a splice_time() with time_specified_flag=1 (5
// bytes: flag+reserved+33-bit PTS).
func encodeSpliceTime(pts int64) []byte {
	b := make([]byte, 5)
	b[0] = 0x80 | byte((pts>>32)&0x01) | 0x7E // time_specified_flag=1, reserved=111111, pts bit32
	binary.BigEndian.PutUint32(b[1:5], uint32(pts))
	return b
}

// encodeBreakDuration builds break_duration(): auto_return(1) + reserved(6)
// + 33-bit duration in 90kHz ticks, from a millisecond duration.
func encodeBreakDuration(durationMS int64, autoReturn bool) []byte {
	ticks := msToTicks(durationMS)
	b := make([]byte, 5)
	b[0] = 0x7E // reserved
	if autoReturn {
		b[0] |= 0x80
	}
	b[0] |= byte((ticks >> 32) & 0x01)
	binary.BigEndian.PutUint32(b[1:5], uint32(ticks))
	return b
}

// encodeSegmentationDuration builds segmentation_duration(): a plain 40-bit
// unsigned tick count (no auto_return/reserved bits, unlike break_duration).
func encodeSegmentationDuration(durationMS int64) []byte {
	ticks := msToTicks(durationMS)
	b := make([]byte, 5)
	b[0] = byte(ticks >> 32)
	binary.BigEndian.PutUint32(b[1:5], uint32(ticks))
	return b
}

func msToTicks(ms int64) int64 {
	return ms * 90000 / 1000
}

// assembleSection builds a complete splice_info_section from a command
// body and an optional pre-built descriptor loop (tag+length+data,
// concatenated — no descriptor_loop_length prefix, which this function adds
// itself), computing a correct CRC32 via the same MPEG-2 CRC every PSI/
// private section in this codebase uses (mux.CRC32MPEG2). Mirrors the exact
// byte layout parser.go's Parse expects, field-for-field.
func assembleSection(cmdType uint8, cmdBody, descLoop []byte) ([]byte, error) {
	if len(cmdBody) > 0xFFF {
		return nil, fmt.Errorf("scte35: splice_command_length %d exceeds 12 bits", len(cmdBody))
	}
	if len(descLoop) > 0xFFFF {
		return nil, fmt.Errorf("scte35: descriptor_loop_length %d exceeds 16 bits", len(descLoop))
	}

	var body []byte
	body = append(body, 0x00)                         // protocol_version
	body = append(body, 0x00, 0x00, 0x00, 0x00, 0x00) // encrypted_packet=0, encryption_algorithm=0, pts_adjustment=0
	body = append(body, 0x00)                         // cw_index
	// tier = 0xFFF (spec convention for "not used"), splice_command_length filled in below.
	body = append(body, 0xFF, 0xF0|byte(len(cmdBody)>>8&0x0F), byte(len(cmdBody)))
	body = append(body, cmdType)
	body = append(body, cmdBody...)
	body = binary.BigEndian.AppendUint16(body, uint16(len(descLoop)))
	body = append(body, descLoop...)

	sectionLength := len(body) + 4 // + CRC32
	if sectionLength > 0x0FFF {
		return nil, fmt.Errorf("scte35: section_length %d exceeds 12 bits", sectionLength)
	}

	var section []byte
	section = append(section, tableIDSpliceInfo)
	section = append(section, 0x30|byte(sectionLength>>8&0x0F), byte(sectionLength))
	section = append(section, body...)

	crc := mux.CRC32MPEG2(section)
	section = binary.BigEndian.AppendUint32(section, crc)

	return section, nil
}
