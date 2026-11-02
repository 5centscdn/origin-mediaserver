package mpegts

import "encoding/binary"

// stripSection strips a PSI-style pointer_field prefix from a payload and
// returns the section bytes plus the section_length-derived total section
// size, without interpreting anything past the generic
// table_id/section_length header. Shared by handleSCTE35 (psi and PAT/PMT
// duplicate this pointer_field-stripping prologue independently today —
// this is only pulled out for the new SCTE-35 path, not retrofitted onto
// parsePAT/parsePMT, to keep this change minimal).
func stripSectionPointer(payload []byte) (section []byte, ok bool) {
	if len(payload) < 1 {
		return nil, false
	}
	pointerField := int(payload[0])
	if 1+pointerField >= len(payload) {
		return nil, false
	}
	return payload[1+pointerField:], true
}

// parsePAT extracts the PID of the first program's PMT from a PAT section.
// Scope: the section is assumed to fit in a single TS packet (true for
// effectively every real single-program encoder) and only the first
// non-network program entry is used — this demuxer is single-program only,
// matching mux.TSMuxer's own single-program output.
func parsePAT(payload []byte) (pmtPID uint16, ok bool) {
	if len(payload) < 1 {
		return 0, false
	}
	pointerField := int(payload[0])
	if 1+pointerField >= len(payload) {
		return 0, false
	}
	section := payload[1+pointerField:]
	if len(section) < 8 || section[0] != 0x00 {
		return 0, false // not a PAT (table_id must be 0x00)
	}

	sectionLength := int(binary.BigEndian.Uint16(section[1:3]) & 0x0FFF)
	if 3+sectionLength > len(section) || sectionLength < 5+4 {
		return 0, false // truncated, likely spans multiple TS packets — unsupported
	}

	// Program loop: offset 8 (after table_id + section_length + transport_stream_id +
	// version/current_next + section_number + last_section_number), ending 4 bytes
	// before the section end (CRC32).
	programs := section[8 : 3+sectionLength-4]
	for i := 0; i+4 <= len(programs); i += 4 {
		programNumber := binary.BigEndian.Uint16(programs[i : i+2])
		pid := binary.BigEndian.Uint16(programs[i+2:i+4]) & 0x1FFF
		if programNumber != 0 { // 0 = network PID entry, not a real program
			return pid, true
		}
	}
	return 0, false
}

// pmtStream describes one elementary stream entry from a PMT.
type pmtStream struct {
	streamType  uint8
	pid         uint16
	descriptors []byte // raw ES descriptor loop bytes (es_info_length long) — see parseTeletextDescriptor
}

// parsePMT extracts the elementary stream list from a PMT section. Same
// single-TS-packet scope note as parsePAT.
func parsePMT(payload []byte) ([]pmtStream, bool) {
	if len(payload) < 1 {
		return nil, false
	}
	pointerField := int(payload[0])
	if 1+pointerField >= len(payload) {
		return nil, false
	}
	section := payload[1+pointerField:]
	if len(section) < 12 || section[0] != 0x02 {
		return nil, false // not a PMT (table_id must be 0x02)
	}

	sectionLength := int(binary.BigEndian.Uint16(section[1:3]) & 0x0FFF)
	if 3+sectionLength > len(section) {
		return nil, false
	}
	programInfoLength := int(binary.BigEndian.Uint16(section[10:12]) & 0x0FFF)

	esLoopStart := 12 + programInfoLength
	esLoopEnd := 3 + sectionLength - 4 // exclude CRC32
	if esLoopStart > esLoopEnd || esLoopEnd > len(section) {
		return nil, false
	}

	var streams []pmtStream
	pos := esLoopStart
	for pos+5 <= esLoopEnd {
		streamType := section[pos]
		pid := binary.BigEndian.Uint16(section[pos+1:pos+3]) & 0x1FFF
		esInfoLength := int(binary.BigEndian.Uint16(section[pos+3:pos+5]) & 0x0FFF)
		var descriptors []byte
		if pos+5+esInfoLength <= esLoopEnd {
			descriptors = section[pos+5 : pos+5+esInfoLength]
		}
		streams = append(streams, pmtStream{streamType: streamType, pid: pid, descriptors: descriptors})
		pos += 5 + esInfoLength
	}
	return streams, true
}

// teletextSubtitlePage is one language/page entry from a teletext_descriptor
// (ETSI EN 300 468 §6.2.43) identified as carrying subtitles.
type teletextSubtitlePage struct {
	magazine int // 1-8
	page     int // 0x00-0xFF, BCD-style hex page number — see teletext.Decoder
	language string
}

const teletextDescriptorTag = 0x56

// teletextType values (ETSI EN 300 468 table 100) this codebase treats as
// carrying subtitles — 0x02 (subtitle page) and 0x05 (subtitle page for the
// hard of hearing). Every other type (initial page, additional info,
// programme schedule, etc.) is a normal teletext service, not a caption
// track, and is out of scope (see internal/teletext's own doc comment).
func isTeletextSubtitleType(teletextType byte) bool {
	return teletextType == 0x02 || teletextType == 0x05
}

// parseTeletextSubtitlePage scans a stream's ES descriptor loop for a
// teletext_descriptor and returns the first entry flagged as a subtitle
// page — a teletext PID commonly multiplexes many unrelated pages (news,
// weather, program guide...), only one of which is the actual subtitle
// service this codebase decodes (see internal/teletext).
func parseTeletextSubtitlePage(descriptors []byte) (teletextSubtitlePage, bool) {
	pos := 0
	for pos+2 <= len(descriptors) {
		tag := descriptors[pos]
		length := int(descriptors[pos+1])
		pos += 2
		if pos+length > len(descriptors) {
			break
		}
		body := descriptors[pos : pos+length]
		pos += length
		if tag != teletextDescriptorTag {
			continue
		}
		for i := 0; i+5 <= len(body); i += 5 {
			language := string(body[i : i+3])
			typeAndMag := body[i+3]
			teletextType := typeAndMag >> 3
			magazine := int(typeAndMag & 0x07)
			if magazine == 0 {
				magazine = 8 // ETSI EN 300 468 / EN 300 706: magazine 0 means magazine 8
			}
			pageBCD := body[i+4]
			if isTeletextSubtitleType(teletextType) {
				return teletextSubtitlePage{magazine: magazine, page: int(pageBCD), language: language}, true
			}
		}
	}
	return teletextSubtitlePage{}, false
}
