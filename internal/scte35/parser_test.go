package scte35

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/5centscdn/origin-mediaserver/internal/mux"
)

// buildSpliceInsert constructs a syntactically valid splice_insert
// splice_info_section with a correct CRC32, for use across the parser and
// state-machine tests. durationMS <= 0 means duration_flag=0.
func buildSpliceInsert(t *testing.T, eventID uint32, outOfNetwork bool, durationMS int64, autoReturn bool) []byte {
	t.Helper()

	var cmd bytes.Buffer
	eventIDBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(eventIDBytes, eventID)
	cmd.Write(eventIDBytes)
	cmd.WriteByte(0x00) // splice_event_cancel_indicator=0, reserved

	flags := byte(0x40) // program_splice_flag=1
	if outOfNetwork {
		flags |= 0x80
	}
	durationFlag := durationMS > 0
	if durationFlag {
		flags |= 0x20
	}
	flags |= 0x10 // splice_immediate_flag=1 (skip splice_time entirely — simplest valid form)
	cmd.WriteByte(flags)

	if durationFlag {
		ticks := durationMS * 90000 / 1000
		b := make([]byte, 5)
		if autoReturn {
			b[0] |= 0x80
		}
		b[0] |= byte((ticks >> 32) & 0x01)
		binary.BigEndian.PutUint32(b[1:5], uint32(ticks))
		cmd.Write(b)
	}
	cmd.Write([]byte{0x00, 0x01}) // unique_program_id
	cmd.WriteByte(0x00)           // avail_num
	cmd.WriteByte(0x00)           // avails_expected

	return buildSection(t, wireSpliceInsert, cmd.Bytes(), nil)
}

// buildSection assembles a complete splice_info_section from a command body
// and optional raw descriptor-loop bytes, computing a correct CRC32.
func buildSection(t *testing.T, cmdType uint8, cmdBody []byte, descriptors []byte) []byte {
	t.Helper()

	var body bytes.Buffer
	body.WriteByte(0x00)                             // protocol_version                             body[0]
	body.Write([]byte{0x00, 0x00, 0x00, 0x00, 0x00}) // encrypted_packet+algorithm+pts_adjustment(5)  body[1..5]
	body.WriteByte(0x00)                             // cw_index                                     body[6]
	body.Write([]byte{0x00, 0x00, 0x00})             // tier(12) + splice_command_length(12) placeholder body[7..9]
	body.WriteByte(cmdType)                          //                                               body[10]
	body.Write(cmdBody)                              //                                               body[11...]
	descLen := make([]byte, 2)
	binary.BigEndian.PutUint16(descLen, uint16(len(descriptors)))
	body.Write(descLen)
	body.Write(descriptors)

	raw := body.Bytes()
	// Patch splice_command_length now that cmdBody's length is known
	// (tier stays 0): body[8] low nibble + body[9] full byte.
	splen := len(cmdBody)
	raw[8] = byte(splen >> 8 & 0x0F)
	raw[9] = byte(splen)

	sectionLength := len(raw) + 4 // + CRC32, excluding table_id+section_length_field themselves
	var section bytes.Buffer
	section.WriteByte(tableIDSpliceInfo)
	section.WriteByte(0x30 | byte(sectionLength>>8&0x0F)) // section_syntax_indicator=0,private_indicator=0,reserved=11
	section.WriteByte(byte(sectionLength))
	section.Write(raw)

	crc := mux.CRC32MPEG2(section.Bytes())
	crcBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(crcBytes, crc)
	section.Write(crcBytes)

	return section.Bytes()
}

func TestParse_SpliceInsert_OutAndIn(t *testing.T) {
	out := buildSpliceInsert(t, 42, true, 30000, true)
	ev, err := Parse(out)
	if err != nil {
		t.Fatalf("Parse(out): %v", err)
	}
	if !ev.OutOfNetwork || ev.EventID != 42 || ev.BreakDurationMS != 30000 || !ev.AutoReturn {
		t.Fatalf("unexpected OUT event: %+v", ev)
	}

	in := buildSpliceInsert(t, 42, false, 0, false)
	evIn, err := Parse(in)
	if err != nil {
		t.Fatalf("Parse(in): %v", err)
	}
	if evIn.OutOfNetwork {
		t.Fatal("IN event has OutOfNetwork=true")
	}
}

func TestParse_TruncatedSection(t *testing.T) {
	if _, err := Parse([]byte{0xFC, 0x30}); err != ErrTooShort {
		t.Fatalf("err = %v, want ErrTooShort", err)
	}
}

func TestParse_BadTableID(t *testing.T) {
	section := buildSpliceInsert(t, 1, true, 0, false)
	section[0] = 0x00
	if _, err := Parse(section); err != ErrBadTableID {
		t.Fatalf("err = %v, want ErrBadTableID", err)
	}
}

func TestParse_CRCMismatch(t *testing.T) {
	section := buildSpliceInsert(t, 1, true, 0, false)
	section[len(section)-1] ^= 0xFF // corrupt CRC
	if _, err := Parse(section); err != ErrCRCMismatch {
		t.Fatalf("err = %v, want ErrCRCMismatch", err)
	}
}

func TestParse_MalformedDoesNotPanic(t *testing.T) {
	// A grab-bag of short/garbage inputs that must return an error, never
	// panic — this is the "malformed messages must not crash the stream"
	// requirement.
	inputs := [][]byte{
		nil,
		{},
		{0xFC},
		bytes.Repeat([]byte{0xFF}, 3),
		bytes.Repeat([]byte{0x00}, 14),
		{0xFC, 0xFF, 0xFF, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	}
	for i, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("input %d: Parse panicked: %v", i, r)
				}
			}()
			Parse(in)
		}()
	}
}

func TestParse_SegmentationDescriptor(t *testing.T) {
	// time_signal command with a segmentation_descriptor (Provider
	// Placement Opportunity Start = 0x30) in the descriptor loop.
	var cmd bytes.Buffer
	cmd.WriteByte(0x00) // splice_time: time_specified_flag=0

	var desc bytes.Buffer
	desc.WriteString("CUEI") // identifier
	segEventID := make([]byte, 4)
	binary.BigEndian.PutUint32(segEventID, 99)
	desc.Write(segEventID)
	desc.WriteByte(0x00)                             // cancel=0
	desc.WriteByte(0xC0)                             // program_segmentation_flag=1, segmentation_duration_flag=1, delivery_not_restricted=0... actually set delivery_not_restricted_flag bit(0x20)=0 -> needs restriction byte; simplify: set 0xE0
	desc.Write([]byte{0x00, 0x00, 0x00, 0x00, 0x28}) // segmentation_duration = 40 (90kHz ticks) ~0ms, keep simple: 0x28 = 40 ticks
	desc.WriteByte(0x08)                             // segmentation_upid_type = 0x08 (Ad-ID)
	desc.WriteByte(0x04)                             // segmentation_upid_length = 4
	desc.WriteString("TEST")                         // segmentation_upid
	desc.WriteByte(0x30)                             // segmentation_type_id = Provider Placement Opportunity Start
	desc.WriteByte(0x00)                             // segment_num
	desc.WriteByte(0x01)                             // segments_expected

	descBody := desc.Bytes()
	descLoop := append([]byte{tagSegmentationDescriptor, byte(len(descBody))}, descBody...)

	section := buildSection(t, wireTimeSignal, cmd.Bytes(), descLoop)
	ev, err := Parse(section)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !ev.HasSegmentation || ev.SegmentationEventID != 99 {
		t.Fatalf("segmentation not parsed: %+v", ev)
	}
	if ev.SegmentationTypeID != 0x30 {
		t.Fatalf("SegmentationTypeID = %#x, want 0x30", ev.SegmentationTypeID)
	}
	if string(ev.UPID) != "TEST" {
		t.Fatalf("UPID = %q, want TEST", ev.UPID)
	}
	if len(ev.RawDescriptors) != 1 {
		t.Fatalf("RawDescriptors = %d entries, want 1", len(ev.RawDescriptors))
	}
}

func TestParse_UnknownDescriptorPreserved(t *testing.T) {
	// A descriptor tag this parser doesn't specifically model must still be
	// preserved verbatim in RawDescriptors.
	unknown := []byte{0x7F, 0x05, 'C', 'U', 'E', 'I', 0xAB}
	section := buildSection(t, wireSpliceNull, nil, unknown)
	ev, err := Parse(section)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(ev.RawDescriptors) != 1 || !bytes.Equal(ev.RawDescriptors[0], unknown) {
		t.Fatalf("RawDescriptors = %x, want %x preserved verbatim", ev.RawDescriptors, unknown)
	}
}
