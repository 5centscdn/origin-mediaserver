package captiondecode

// CEA708Decoder decodes CEA-708 DTVCC packets (cc_type 2/3 triplets),
// Service 1 only (the near-universal default caption service — services
// 2-63 are ignored), into plain text + timing Cues.
//
// Deliberately discards: window position/anchor, pen color/style/font,
// multi-window overlap. Only one "current" window is tracked (whichever
// SetCurrentWindow/DefineWindow last selected) — enough for the common
// single-caption-window case real encoders use for basic captions. Every
// command byte's parameter length is still correctly consumed even when
// its effect is discarded, so the decoder never desyncs from the byte
// stream because of a command it doesn't implement.
type CEA708Decoder struct {
	packet    []byte // in-progress DTVCC packet being reassembled
	packetLen int    // declared total length (header byte included) of the in-progress packet

	text         []rune
	textStartPTS int64
	haveText     bool

	currentWindow int8 // -1 = none defined yet
	visible       bool // whether currentWindow is currently displayed (DSW seen, no HDW/DLW since)
}

// NewCEA708Decoder creates a decoder with no window defined and no pending
// DTVCC packet.
func NewCEA708Decoder() *CEA708Decoder {
	return &CEA708Decoder{currentWindow: -1}
}

// Feed processes one cc_data triplet at presentation time pts (90kHz
// ticks). ccType is the triplet's 2-bit type (0/1 = CEA-608 field data, not
// this decoder's concern; 2 = DTVCC_PACKET_DATA continuation; 3 =
// DTVCC_PACKET_START). b1/b2 are cc_data_1/cc_data_2 — CEA-708 carries no
// parity bit, unlike CEA-608, so these are used as-is. Returns every Cue
// completed while processing this triplet — a single DTVCC packet can
// contain multiple window show/hide commands, so more than one Cue may
// complete at once.
func (d *CEA708Decoder) Feed(ccValid bool, ccType uint8, b1, b2 byte, pts int64) []Cue {
	if !ccValid {
		return nil
	}
	switch ccType {
	case 3: // DTVCC_PACKET_START
		// b1's low 6 bits are packet_size_code; actual packet size in
		// bytes (header byte included) is packet_size_code*2, or 128 when
		// the code is 0, per spec.
		sizeCode := int(b1 & 0x3F)
		size := sizeCode * 2
		if size == 0 {
			size = 128
		}
		d.packet = append(d.packet[:0], b1, b2)
		d.packetLen = size
	case 2: // DTVCC_PACKET_DATA (continuation)
		if d.packet == nil {
			return nil // continuation with no packet start seen — drop
		}
		d.packet = append(d.packet, b1, b2)
	default:
		return nil // CEA-608 field data — not this decoder's concern
	}

	if d.packet != nil && len(d.packet) >= d.packetLen {
		pkt := d.packet[:d.packetLen]
		d.packet = nil
		return d.processPacket(pkt, pts)
	}
	return nil
}

// Flush finalizes whatever cue is currently visible — call once at stream
// end so the final caption isn't silently dropped.
func (d *CEA708Decoder) Flush(pts int64) (Cue, bool) {
	if d.visible && d.haveText && len(d.text) > 0 {
		cue := Cue{StartPTS: d.textStartPTS, EndPTS: pts, Text: string(d.text)}
		d.clearText()
		return cue, true
	}
	return Cue{}, false
}

// processPacket walks a complete DTVCC packet's service blocks, processing
// only Service 1 (byte 0 is the sequence_number+packet_size_code header,
// not part of the service-block area).
//
// The service_number==7 "extended service number" path (services 7-63,
// signaled via an extra header byte) is a best-effort implementation, not
// independently verified against a real multi-service fixture — no fixture
// carrying a non-default service number was available to test against.
// Since Service 1 is always signaled directly (never via this path), any
// imprecision here cannot corrupt Service 1's own decoded text, only (in
// the worst case) misalign the search for a differently-numbered service's
// block within the same packet.
func (d *CEA708Decoder) processPacket(pkt []byte, pts int64) []Cue {
	var cues []Cue
	i := 1 // skip the packet header byte
	for i < len(pkt) {
		header := pkt[i]
		i++
		serviceNum := int(header >> 5)
		blockSize := int(header & 0x1F)

		if serviceNum == 7 {
			if i >= len(pkt) {
				break
			}
			serviceNum = int(pkt[i] & 0x3F)
			i++
		}

		if blockSize == 0 {
			continue // null block
		}
		if i+blockSize > len(pkt) {
			blockSize = len(pkt) - i // truncated — take what's left rather than overrun
		}
		block := pkt[i : i+blockSize]
		i += blockSize

		if serviceNum == 1 {
			cues = append(cues, d.processServiceBlock(block, pts)...)
		}
		// Other services: skipped, per this package's Service-1-only scope.
	}
	return cues
}

func (d *CEA708Decoder) processServiceBlock(block []byte, pts int64) []Cue {
	var cues []Cue
	i := 0
	for i < len(block) {
		b := block[i]
		i++
		switch {
		case b == 0x10: // EXT1 — next byte selects an extended C2/C3/G2/G3 code
			if i >= len(block) {
				return cues
			}
			ext := block[i]
			i++
			switch {
			case ext >= 0x20 && ext <= 0x7F: // G2 — extended printable
				if ch, ok := g2Char(ext); ok {
					d.appendChar(ch, pts)
				}
			case ext >= 0xA0: // G3 — extended printable (rarely used)
				if ch, ok := g3Char(ext); ok {
					d.appendChar(ch, pts)
				}
			default: // C2/C3 — parameter bytes only, no text; skip correctly to stay aligned
				i += extendedParamLen(ext, block[i:])
			}
		case b <= 0x0F: // C0 code set
			n, cue, ok := d.handleC0(b, block[i:], pts)
			i += n
			if ok {
				cues = append(cues, cue)
			}
		case b >= 0x20 && b <= 0x7F: // G0 — standard printable (ASCII-compatible)
			d.appendChar(rune(b), pts)
		case b >= 0x80 && b <= 0x9F: // C1 — caption command control codes
			n, cue, ok := d.handleC1(b, block[i:], pts)
			i += n
			if ok {
				cues = append(cues, cue)
			}
		default: // 0xA0-0xFF — G1, extended Latin character set
			if ch, ok := g1Char(b); ok {
				d.appendChar(ch, pts)
			}
		}
	}
	return cues
}

// handleC0 processes one C0 control code (0x00-0x0F; 0x10 EXT1 is handled
// by the caller). Returns how many additional parameter bytes it consumed.
func (d *CEA708Decoder) handleC0(b byte, rest []byte, pts int64) (consumed int, cue Cue, ok bool) {
	switch b {
	case 0x08: // BS — Backspace
		d.backspace()
	case 0x0C: // FF — Form Feed: clears the window and homes the cursor
		c, flushed := d.clearAndFlush(pts)
		return 0, c, flushed
	case 0x0D, 0x0E: // CR / HCR — treat as a line break within the window's text
		d.appendChar('\n', pts)
	case 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17: // reserved, 1 param byte per spec's range grouping
		return 1, Cue{}, false
	case 0x18, 0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F: // reserved, 2 param bytes
		return 2, Cue{}, false
	}
	return 0, Cue{}, false
}

// handleC1 processes one caption command control code (0x80-0x9F). Returns
// how many additional parameter bytes it consumed — critical to get right
// even for commands whose effect (window position/style) this decoder
// discards, so later bytes in the block stay correctly aligned.
func (d *CEA708Decoder) handleC1(b byte, rest []byte, pts int64) (consumed int, cue Cue, ok bool) {
	switch {
	case b >= 0x80 && b <= 0x87: // CW0-CW7 — SetCurrentWindow
		d.currentWindow = int8(b - 0x80)
		return 0, Cue{}, false
	case b == 0x88: // CLW — ClearWindows
		d.clearText()
		return 1, Cue{}, false
	case b == 0x89: // DSW — DisplayWindows
		if len(rest) < 1 {
			return 0, Cue{}, false
		}
		if d.currentWindow >= 0 && rest[0]&(1<<uint(d.currentWindow)) != 0 {
			d.visible = true
		}
		return 1, Cue{}, false
	case b == 0x8A: // HDW — HideWindows
		if len(rest) < 1 {
			return 0, Cue{}, false
		}
		if d.currentWindow >= 0 && rest[0]&(1<<uint(d.currentWindow)) != 0 {
			c, flushed := d.hideCurrent(pts)
			return 1, c, flushed
		}
		return 1, Cue{}, false
	case b == 0x8B: // TGW — ToggleWindows — ambiguous show/hide effect, discarded
		return 1, Cue{}, false
	case b == 0x8C: // DLW — DeleteWindows
		if len(rest) < 1 {
			return 0, Cue{}, false
		}
		if d.currentWindow >= 0 && rest[0]&(1<<uint(d.currentWindow)) != 0 {
			c, flushed := d.hideCurrent(pts)
			d.clearText()
			return 1, c, flushed
		}
		return 1, Cue{}, false
	case b == 0x8D: // DLY — Delay
		return 1, Cue{}, false
	case b == 0x8E: // DLC — DelayCancel
		return 0, Cue{}, false
	case b == 0x8F: // RST — Reset
		d.reset()
		return 0, Cue{}, false
	case b == 0x90: // SPA — SetPenAttributes
		return 2, Cue{}, false
	case b == 0x91: // SPC — SetPenColor
		return 3, Cue{}, false
	case b == 0x92: // SPL — SetPenLocation
		return 2, Cue{}, false
	case b >= 0x93 && b <= 0x96: // reserved
		return 0, Cue{}, false
	case b == 0x97: // SWA — SetWindowAttributes
		return 4, Cue{}, false
	case b >= 0x98 && b <= 0x9F: // DF0-DF7 — DefineWindow (does not itself display it)
		d.currentWindow = int8(b - 0x98)
		return 6, Cue{}, false
	}
	return 0, Cue{}, false
}

func (d *CEA708Decoder) appendChar(ch rune, pts int64) {
	if d.currentWindow < 0 {
		return // no window defined yet — drop
	}
	if !d.haveText {
		d.textStartPTS = pts
		d.haveText = true
	}
	d.text = append(d.text, ch)
}

func (d *CEA708Decoder) backspace() {
	if len(d.text) > 0 {
		d.text = d.text[:len(d.text)-1]
	}
}

func (d *CEA708Decoder) clearText() {
	d.text = nil
	d.haveText = false
}

func (d *CEA708Decoder) hideCurrent(pts int64) (Cue, bool) {
	wasVisible := d.visible
	d.visible = false
	if wasVisible && d.haveText && len(d.text) > 0 {
		cue := Cue{StartPTS: d.textStartPTS, EndPTS: pts, Text: string(d.text)}
		d.clearText()
		return cue, true
	}
	return Cue{}, false
}

func (d *CEA708Decoder) clearAndFlush(pts int64) (Cue, bool) {
	if d.visible && d.haveText && len(d.text) > 0 {
		cue := Cue{StartPTS: d.textStartPTS, EndPTS: pts, Text: string(d.text)}
		d.clearText()
		return cue, true
	}
	d.clearText()
	return Cue{}, false
}

func (d *CEA708Decoder) reset() {
	d.currentWindow = -1
	d.visible = false
	d.clearText()
}

// extendedParamLen returns how many additional bytes a C2 (0x00-0x1F) or C3
// (0x80-0x9F) extended code consumes beyond its own byte, per the CEA-708
// spec's range-based parameter-length grouping.
func extendedParamLen(ext byte, rest []byte) int {
	switch {
	case ext <= 0x07:
		return 0
	case ext <= 0x0F:
		return 1
	case ext <= 0x17:
		return 2
	case ext <= 0x1F:
		return 3
	case ext >= 0x80 && ext <= 0x87:
		return 4
	case ext >= 0x88 && ext <= 0x8F:
		return 5
	case ext >= 0x90 && ext <= 0x9F:
		if len(rest) < 1 {
			return 0
		}
		return 1 + int(rest[0]&0x7F) // the length byte itself, plus its declared count
	default:
		return 0
	}
}

// g1Char maps a G1 (0xA0-0xFF) code — CEA-708's "extended Latin character
// set", which mirrors ISO 8859-1 (Latin-1) at these code points — directly
// to the identical Unicode code point (Unicode's first 256 code points are
// Latin-1 by design, so no lookup table is needed here).
func g1Char(b byte) (rune, bool) {
	return rune(b), true
}

// g2Char maps a G2 (extended printable, selected via EXT1 + 0x20-0x7F)
// code to a character. Deliberately partial — only entries this
// implementation is confident about are mapped; everything else returns
// ok=false (discarded) rather than risk a fabricated glyph. G2 is rarely
// used by real encoders for basic captions.
func g2Char(b byte) (rune, bool) {
	switch b {
	case 0x20:
		return ' ', true // transparent space
	case 0x25:
		return '…', true // … horizontal ellipsis
	case 0x2C:
		return 'Œ', true // Œ
	case 0x31:
		return '‘', true // ‘
	case 0x32:
		return '’', true // ’
	case 0x33:
		return '“', true // “
	case 0x34:
		return '”', true // ”
	case 0x35:
		return '•', true // •
	case 0x39:
		return '™', true // ™
	case 0x3C:
		return 'œ', true // œ
	}
	return 0, false
}

// g3Char maps a G3 (selected via EXT1 + 0xA0-0xFF) code. Only 0xA0 ("[CC]"
// logo glyph) is defined by the spec in any commonly-referenced form;
// everything else is reserved and discarded.
func g3Char(b byte) (rune, bool) {
	if b == 0xA0 {
		return '□', true // no true Unicode glyph for the [CC] logo — placeholder box
	}
	return 0, false
}
