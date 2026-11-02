package captiondecode

// CEA608Decoder decodes CEA-608 (line-21) byte pairs from Field 1, channel
// CC1 only — the near-universal default caption channel (CC2/3/4 explicitly
// out of scope, mirroring this package's CEA-708 Service-1-only scope) —
// into plain text + timing Cues.
//
// Deliberately discards: Preamble Address Code row/indent/style, Mid-Row
// style codes, and Tab Offsets — only their byte pairs are consumed (so the
// decoder stays byte-aligned), never their positioning/styling effect. Only
// text and cue start/end timing survive.
type CEA608Decoder struct {
	mode caption608Mode

	// Pop-On mode: text accumulates in nonDisplayed until EOC (0x2F) swaps
	// it into displayed, which is the moment it actually becomes visible —
	// dispStartPTS is recorded at that swap, not while the text was being
	// silently built up.
	displayed    []rune
	nonDisplayed []rune
	dispStartPTS int64
	haveDisp     bool

	// Roll-Up / Paint-On mode: text is written directly to the visible
	// line as it arrives — lineStartPTS is recorded the moment the first
	// character lands in an empty line.
	line         []rune
	lineStartPTS int64
	haveLine     bool

	// CEA-608 retransmits every control code once for error resilience; a
	// decoder must execute an immediately-repeated identical control pair
	// only once. Does not apply to standard character pairs.
	lastCtrlB1, lastCtrlB2 byte
	haveLastCtrl           bool
}

type caption608Mode int

const (
	mode608None caption608Mode = iota
	mode608PopOn
	mode608RollUp
	mode608PaintOn
)

// NewCEA608Decoder creates a decoder with no visible/pending text.
func NewCEA608Decoder() *CEA608Decoder {
	return &CEA608Decoder{}
}

// Feed processes one Field-1 byte pair (odd-parity bit in bit 7, per spec —
// masked off here rather than validated, matching this codebase's established
// real-world-decoder leniency elsewhere) at presentation time pts (90kHz
// ticks). Returns a completed Cue if this pair caused one to flush.
func (d *CEA608Decoder) Feed(b1, b2 byte, pts int64) (Cue, bool) {
	b1 &= 0x7F
	b2 &= 0x7F

	if b1 == 0 { // null/padding filler
		d.haveLastCtrl = false
		return Cue{}, false
	}

	if b1 < 0x20 {
		if b1 > 0x17 {
			d.haveLastCtrl = false
			return Cue{}, false // CC2 first byte (0x18-0x1F) — out of scope
		}
		if d.haveLastCtrl && d.lastCtrlB1 == b1 && d.lastCtrlB2 == b2 {
			// Doubled control code — execute once, not twice.
			d.haveLastCtrl = false
			return Cue{}, false
		}
		d.lastCtrlB1, d.lastCtrlB2, d.haveLastCtrl = b1, b2, true
		return d.feedControl(b1, b2, pts)
	}

	d.haveLastCtrl = false
	d.appendChar(decodeStandardChar(b1), pts)
	if b2 != 0 {
		d.appendChar(decodeStandardChar(b2), pts)
	}
	return Cue{}, false
}

// Flush finalizes whatever cue is currently visible/accumulating — call
// once at stream end so the final caption isn't silently dropped.
func (d *CEA608Decoder) Flush(pts int64) (Cue, bool) {
	if d.haveDisp && len(d.displayed) > 0 {
		return d.flushDisplayed(pts)
	}
	if d.haveLine && len(d.line) > 0 {
		return d.flushLine(pts)
	}
	return Cue{}, false
}

func (d *CEA608Decoder) feedControl(b1, b2 byte, pts int64) (Cue, bool) {
	switch {
	case b2 >= 0x40 && b2 <= 0x7F:
		return Cue{}, false // Preamble Address Code — row/style/indent discarded
	case b1 == 0x11 && b2 >= 0x20 && b2 <= 0x2F:
		return Cue{}, false // Mid-Row style code — discarded
	case b1 == 0x11 && b2 >= 0x30 && b2 <= 0x3F:
		d.appendChar(specialCharSet[b2-0x30], pts)
		return Cue{}, false
	case (b1 == 0x12 || b1 == 0x13) && b2 >= 0x20 && b2 <= 0x3F:
		if ch, ok := extendedChar(b1, b2); ok {
			// Spec convention: an extended char is preceded by a standard-
			// set fallback character for decoders that don't support
			// extended sets; a decoder that DOES support them replaces it.
			d.backspaceCurrent()
			d.appendChar(ch, pts)
		}
		return Cue{}, false
	case b1 == 0x17 && b2 >= 0x21 && b2 <= 0x23:
		return Cue{}, false // Tab Offset — column position discarded
	case b1 == 0x14 && b2 >= 0x20 && b2 <= 0x2F:
		return d.miscControl(b2, pts)
	default:
		return Cue{}, false // reserved/unrecognized combination
	}
}

func (d *CEA608Decoder) miscControl(b2 byte, pts int64) (Cue, bool) {
	switch b2 {
	case 0x20: // RCL — Resume Caption Loading (enter Pop-On mode)
		d.mode = mode608PopOn
	case 0x21: // BS — Backspace
		d.backspaceCurrent()
	case 0x24: // DER — Delete to End of Row (editing-only; discarded)
	case 0x25, 0x26, 0x27: // RU2/RU3/RU4 — Roll-Up captions
		d.mode = mode608RollUp
	case 0x29: // RDC — Resume Direct Captioning (Paint-On mode)
		d.mode = mode608PaintOn
	case 0x2C: // EDM — Erase Displayed Memory
		return d.flushDisplayed(pts)
	case 0x2D: // CR — Carriage Return (Roll-Up: current line complete)
		return d.flushLine(pts)
	case 0x2E: // ENM — Erase Non-displayed Memory
		d.nonDisplayed = nil
	case 0x2F: // EOC — End Of Caption (Pop-On: swap memories, become visible)
		return d.swapMemories(pts)
	}
	return Cue{}, false
}

func (d *CEA608Decoder) appendChar(ch rune, pts int64) {
	switch d.mode {
	case mode608PopOn:
		d.nonDisplayed = append(d.nonDisplayed, ch)
	case mode608RollUp, mode608PaintOn:
		if !d.haveLine {
			d.lineStartPTS = pts
			d.haveLine = true
		}
		d.line = append(d.line, ch)
	default:
		// No mode-setting command seen yet — a real encoder always sends
		// one before text; silently drop rather than guess a destination.
	}
}

func (d *CEA608Decoder) backspaceCurrent() {
	switch d.mode {
	case mode608PopOn:
		if len(d.nonDisplayed) > 0 {
			d.nonDisplayed = d.nonDisplayed[:len(d.nonDisplayed)-1]
		}
	case mode608RollUp, mode608PaintOn:
		if len(d.line) > 0 {
			d.line = d.line[:len(d.line)-1]
		}
	}
}

func (d *CEA608Decoder) flushDisplayed(pts int64) (Cue, bool) {
	if !d.haveDisp || len(d.displayed) == 0 {
		d.haveDisp = false
		d.displayed = nil
		return Cue{}, false
	}
	cue := Cue{StartPTS: d.dispStartPTS, EndPTS: pts, Text: string(d.displayed)}
	d.displayed = nil
	d.haveDisp = false
	return cue, true
}

func (d *CEA608Decoder) flushLine(pts int64) (Cue, bool) {
	if !d.haveLine || len(d.line) == 0 {
		d.haveLine = false
		d.line = nil
		return Cue{}, false
	}
	cue := Cue{StartPTS: d.lineStartPTS, EndPTS: pts, Text: string(d.line)}
	d.line = nil
	d.haveLine = false
	return cue, true
}

func (d *CEA608Decoder) swapMemories(pts int64) (Cue, bool) {
	var flushed Cue
	var ok bool
	if d.haveDisp && len(d.displayed) > 0 {
		flushed = Cue{StartPTS: d.dispStartPTS, EndPTS: pts, Text: string(d.displayed)}
		ok = true
	}
	d.displayed = d.nonDisplayed
	d.nonDisplayed = nil
	d.dispStartPTS = pts
	d.haveDisp = len(d.displayed) > 0
	return flushed, ok
}

// ─── Character tables ──────────────────────────────────────────────────────

// standardCharOverrides holds the "Basic North American Character Set"'s
// deviations from plain ASCII at these specific code points (CEA-608-B).
// Every other byte in 0x20-0x7F decodes identically to ASCII.
var standardCharOverrides = map[byte]rune{
	0x27: '’', // ’
	0x2A: 'á', // á
	0x5C: 'é', // é
	0x5E: 'í', // í
	0x5F: 'ó', // ó
	0x60: 'ú', // ú
	0x7B: 'ç', // ç
	0x7C: '÷', // ÷
	0x7D: 'Ñ', // Ñ
	0x7E: 'ñ', // ñ
	0x7F: '■', // ▪ (solid block)
}

func decodeStandardChar(b byte) rune {
	if r, ok := standardCharOverrides[b]; ok {
		return r
	}
	return rune(b)
}

// specialCharSet is the "Special North American Character Set", selected by
// first byte 0x11 + second byte 0x30-0x3F, indexed by (b2 - 0x30).
var specialCharSet = [16]rune{
	'®', // ® 0x30
	'°', // ° 0x31
	'½', // ½ 0x32
	'¿', // ¿ 0x33
	'™', // ™ 0x34
	'¢', // ¢ 0x35
	'£', // £ 0x36
	'♪', // ♪ 0x37
	'à', // à 0x38
	' ', // transparent space 0x39 — rendered as an ordinary space
	'è', // è 0x3A
	'â', // â 0x3B
	'ê', // ê 0x3C
	'î', // î 0x3D
	'ô', // ô 0x3E
	'û', // û 0x3F
}

// extendedChar covers the "Extended Western European Character Set" 1
// (first byte 0x12, French/misc accented Latin) and 2 (first byte 0x13,
// Portuguese/German/Danish). Deliberately partial: only entries this
// implementation is confident about are mapped; everything else returns
// ok=false (silently discarded, matching appendChar's own "no clear
// destination" leniency) rather than risk fabricating a wrong diacritic.
// These extended sets are rarely used in real North American broadcast
// captions in practice — worth completing from the CEA-608-B spec table
// directly if a real stream is found to need it.
func extendedChar(b1, b2 byte) (rune, bool) {
	if b1 == 0x12 {
		switch b2 {
		case 0x20:
			return 'Á', true // Á
		case 0x21:
			return 'É', true // É
		case 0x22:
			return 'Ó', true // Ó
		case 0x23:
			return 'Ú', true // Ú
		case 0x24:
			return 'Ü', true // Ü
		case 0x25:
			return 'ü', true // ü
		case 0x27:
			return '¡', true // ¡
		}
	}
	if b1 == 0x13 {
		switch b2 {
		case 0x20:
			return 'Ã', true // Ã
		case 0x21:
			return 'ã', true // ã
		case 0x2F:
			return 'Ë', true // Ë
		}
	}
	return 0, false
}
