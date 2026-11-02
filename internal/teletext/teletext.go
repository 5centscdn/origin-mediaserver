// Package teletext decodes one DVB Teletext subtitle page (ETSI EN 300 706,
// carried per ETSI EN 300 472) into plain-text cues — the counterpart to
// internal/captiondecode's CEA-608/708 decoder, for sources whose captions
// arrive as legacy page-based teletext instead of SEI-embedded data (see
// media.CodecInfo.HasDVBTeletextSubtitle's doc comment for how the two are
// distinguished).
//
// Scope, matching this project's established pattern of a deliberately
// narrow, verified-against-a-real-encoder decode (see captiondecode,
// internal/ingest/mpegts's H.264/AAC-only demux): one target
// (magazine, page) — the specific page a stream's teletext_descriptor
// identifies as carrying subtitles — decoded as plain text, no styling
// (color/double-height/boxing all discarded, matching this project's other
// caption formats' "timing + text only" scope), no national character
// subset switching (printable 7-bit codes are treated as plain ASCII,
// correct for the large majority of the printable range in every Latin
// teletext subset). Packets 26/28/29 (enhancement data) are out of scope
// entirely, same as packets 26+ for CEA-708 (multi-service, out of scope).
package teletext

import (
	"strings"

	"github.com/5centscdn/origin-mediaserver/internal/captiondecode"
)

const (
	minRow = 1
	maxRow = 25 // packets 1-25 are ordinary display rows; row 0 is the header, handled separately
)

// Decoder accumulates one target page's rows across teletext packets and
// emits a captiondecode.Cue whenever a fully-received page transmission
// differs from what's currently considered "on screen" — real broadcasts
// retransmit an unchanged subtitle page many times per second for
// robustness (a late-tuning receiver, or an SRT sender's retry), so a cue
// boundary is cut only on an actual content change, not on every repeated
// header (see Feed's doc comment for the two-cycle-lag comparison this
// requires). Not safe for concurrent use — matches
// captiondecode.Extractor's own contract (fed from a single ingest
// goroutine).
type Decoder struct {
	magazine int // 1-8 (EN 300 706's magazine 0 means 8 — see decodeMagazineAndPacket)
	page     int // 0x00-0xFF, BCD-style hex page number (e.g. page "801" is magazine 8, page 0x01)

	// rows accumulates the transmission cycle currently in progress —
	// finalized and compared against displayedText when the NEXT header
	// for this page arrives (see Feed).
	rows      [maxRow + 1]string
	rowSet    [maxRow + 1]bool
	haveStart bool  // false until the first header for our page has ever been seen
	cycleAt   int64 // PTS of the header that started the in-progress cycle

	displayedText  string
	displayedStart int64
	displaying     bool
}

// NewDecoder creates a decoder for one specific (magazine, page) — see
// internal/ingest/mpegts's teletext_descriptor parsing for how that's
// identified from the PMT.
func NewDecoder(magazine, page int) *Decoder {
	return &Decoder{magazine: magazine, page: page}
}

// Feed processes one raw EBU teletext data_field (44 bytes: the
// reserved/field_parity/line_offset byte, the framing_code byte,
// magazine_and_packet_address, and data_block — ETSI EN 300 472 table 2)
// extracted from a PES packet whose own timestamp is pts (90kHz ticks).
// Returns a completed Cue when a page transmission cycle for this
// Decoder's target page has just finished and its content differs from
// what was previously displayed — never more than one Cue per call, since
// at most one header for our page can appear per data_field.
func (d *Decoder) Feed(dataField []byte, pts int64) []captiondecode.Cue {
	if len(dataField) < 44 {
		return nil
	}
	// Skip reserved/field_parity/line_offset(1) + framing_code(1) — the
	// reference decode model (and this package's own verification, see
	// teletext_test.go) starts at magazine_and_packet_address; framing_code
	// is a fixed sync marker with nothing to decode.
	buf := make([]byte, 42)
	copy(buf, dataField[2:44])
	invertBuffer(buf)

	x := unhamW84(buf[0], buf[1])
	packetNr := int(x>>3) & 0x1F
	magNr := int(x) & 0x07
	if magNr == 0 {
		magNr = 8 // EN 300 706: magazine number 0 in the 3-bit field means magazine 8
	}
	if magNr != d.magazine {
		return nil
	}

	if packetNr >= minRow && packetNr <= maxRow {
		d.rows[packetNr] = decodeRowText(buf[2:42])
		d.rowSet[packetNr] = true
		return nil
	}
	if packetNr == 0 {
		return d.handleHeader(buf, pts)
	}
	return nil // packets 26-31: enhancement data, out of scope
}

// handleHeader processes a packet-0 (page header) for our magazine —
// finalizing whichever transmission cycle was in progress (see Feed's doc
// comment) before starting a new one.
func (d *Decoder) handleHeader(buf []byte, pts int64) []captiondecode.Cue {
	units, ok1 := unhamAndCheck(buf[2])
	tens, ok2 := unhamAndCheck(buf[3])
	if !ok1 || !ok2 {
		return nil // uncorrectable header, ignore rather than risk misrouting to the wrong page
	}
	pageNr := int(units) | int(tens)<<4
	if pageNr != d.page {
		return nil // a header for a different page sharing our magazine
	}

	c4EraseNow := buf[5]&0x80 != 0 // EN 300 706 clause 9.4.1: erase page

	var cues []captiondecode.Cue
	if d.haveStart {
		text := d.joinRows()
		if c4EraseNow {
			text = ""
		}
		if text != d.displayedText {
			if d.displaying && d.displayedText != "" {
				cues = append(cues, captiondecode.Cue{
					StartPTS: d.displayedStart,
					EndPTS:   pts,
					Text:     d.displayedText,
				})
			}
			d.displayedText = text
			d.displayedStart = d.cycleAt
			d.displaying = text != ""
		}
	}

	d.rows = [maxRow + 1]string{}
	d.rowSet = [maxRow + 1]bool{}
	d.cycleAt = pts
	d.haveStart = true
	return cues
}

// joinRows concatenates every received, non-blank row (in row-number
// order) with newlines — matching ebuttd.Render/webvtt.RenderSamples'
// existing convention of plain multi-line text with no positioning.
func (d *Decoder) joinRows() string {
	var b strings.Builder
	for i := minRow; i <= maxRow; i++ {
		if !d.rowSet[i] {
			continue
		}
		line := strings.TrimRight(d.rows[i], " ")
		if line == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	return b.String()
}

// Flush closes out whatever cue is currently displayed at stream end (or a
// segment boundary needing a best-effort final cue) — mirrors
// captiondecode.Extractor.Flush's role for CEA-608/708.
func (d *Decoder) Flush(pts int64) (captiondecode.Cue, bool) {
	if !d.displaying || d.displayedText == "" {
		return captiondecode.Cue{}, false
	}
	cue := captiondecode.Cue{StartPTS: d.displayedStart, EndPTS: pts, Text: d.displayedText}
	d.displaying = false
	return cue, true
}

// decodeRowText converts one 40-byte teletext display row (a display or
// header packet's data_block, already bit-inverted) into plain text:
// parity-stripped, control codes (0x00-0x1F: color/flash/box/height, all
// styling this project's other caption formats also discard — see this
// package's doc comment) and DEL (0x7F) rendered as spaces rather than
// dropped, preserving column alignment the way a real teletext display
// would (a control code occupies a character cell but shows as blank).
func decodeRowText(data []byte) string {
	row := make([]byte, len(data))
	copy(row, data)
	stripParity(row)

	out := make([]byte, len(row))
	for i, c := range row {
		if c < 0x20 || c == 0x7F {
			out[i] = ' '
		} else {
			out[i] = c
		}
	}
	return string(out)
}
