package teletext

import "testing"

// rawHammingByte builds a synthetic "as it would appear in the PES, before
// bit-inversion" Hamming-8/4-encoded byte for nibble, using this package's
// own (independently cross-verified against dvbsnoop and CCExtractor — see
// hamming.go) decode table as the sole source of truth rather than
// re-deriving the parity equations by hand: find any table index that
// decodes cleanly to nibble, then un-invert it, since invertTab is its own
// inverse. This sidesteps the risk of a second, independently-wrong
// transcription of the encode-side parity math.
func rawHammingByte(t *testing.T, nibble byte) byte {
	t.Helper()
	for i := 0; i < 256; i++ {
		if unham84Tab[byte(i)] == nibble {
			return invertTab[byte(i)]
		}
	}
	t.Fatalf("no clean Hamming-8/4 codeword found for nibble %#x", nibble)
	return 0
}

func TestHammingRoundTrip_AllNibbles(t *testing.T) {
	for n := byte(0); n <= 0x0F; n++ {
		raw := rawHammingByte(t, n)
		table := invertByte(raw)
		got, ok := unhamAndCheck(table)
		if !ok {
			t.Fatalf("nibble %#x: rawHammingByte round-trip reported uncorrectable error", n)
		}
		if got != n {
			t.Errorf("nibble %#x: round-trip = %#x", n, got)
		}
	}
}

// buildDataField assembles one raw (pre-inversion, as it would arrive in a
// PES packet) 44-byte EBU teletext data_field: reserved/field_parity/
// line_offset + framing_code (2 placeholder bytes, not validated by Feed)
// + magazine_and_packet_address (2 Hamming bytes) + data_block (40 bytes,
// each odd-parity bit left as 0 — Feed only strips it, never checks it).
func buildDataField(t *testing.T, magazine, packetNr int, dataBlock [40]byte) []byte {
	t.Helper()
	field := make([]byte, 44)
	field[0] = 0x00 // reserved/field_parity/line_offset — unused by Feed
	field[1] = 0xE4 // framing_code — unused by Feed

	mag3 := byte(magazine)
	if magazine == 8 {
		mag3 = 0 // EN 300 706: magazine 8 is transmitted as 3-bit value 0
	}
	x := (byte(packetNr) << 3) | (mag3 & 0x07)
	lsb := rawHammingByte(t, x&0x0F)
	msb := rawHammingByte(t, (x>>4)&0x0F)
	field[2] = lsb
	field[3] = msb

	copy(field[4:44], dataBlock[:])
	return field
}

// asciiRow builds a 40-byte data_block of plain ASCII text, padded with
// spaces — pre-inverted (see invertTab) so that Feed's mandatory bit-
// reversal of the whole data_field recovers the original ASCII, exactly as
// a real encoder transmits it (character bytes are bit-reversed in the PES
// exactly like Hamming-coded bytes are — this isn't Hamming-specific).
func asciiRow(text string) [40]byte {
	var row [40]byte
	for i := range row {
		row[i] = invertByte(' ')
	}
	for i := 0; i < len(text) && i < len(row); i++ {
		row[i] = invertByte(text[i])
	}
	return row
}

func TestDecoder_SimpleSubtitleCue(t *testing.T) {
	d := NewDecoder(8, 0x01)

	// Header for magazine 8, page 0x01 (units=1, tens=0), no control bits set.
	var headerBlock [40]byte
	headerBlock[0] = rawHammingByte(t, 0x01) // page units nibble
	headerBlock[1] = rawHammingByte(t, 0x00) // page tens nibble
	// remaining header display bytes (clock/etc.) left zero — irrelevant to subtitle text

	header1 := buildDataField(t, 8, 0, headerBlock)
	if cues := d.Feed(header1, 1000); len(cues) != 0 {
		t.Fatalf("first header (bootstrap) should emit no cue, got %+v", cues)
	}

	row1 := buildDataField(t, 8, 1, asciiRow("Hello from teletext"))
	if cues := d.Feed(row1, 1000); len(cues) != 0 {
		t.Fatalf("a display row should never itself emit a cue, got %+v", cues)
	}

	// A second header finishes the first real cycle ("Hello from
	// teletext"), which immediately becomes the displayed content — no cue
	// yet, since nothing was displayed *before* it (a cue is the gap
	// between two displayed states, not the first one).
	header2 := buildDataField(t, 8, 0, headerBlock)
	cues := d.Feed(header2, 2000)
	if len(cues) != 0 {
		t.Fatalf("expected no cue yet (nothing was previously displayed), got %+v", cues)
	}

	// New, different content for the second cycle — the header that closes
	// *this* cycle must flush the first cue (start=header1's PTS,
	// end=this header's PTS).
	row2 := buildDataField(t, 8, 1, asciiRow("Second line of text"))
	d.Feed(row2, 2000)

	header3 := buildDataField(t, 8, 0, headerBlock)
	cues = d.Feed(header3, 3000)
	if len(cues) != 1 {
		t.Fatalf("expected exactly 1 cue when content changes, got %d: %+v", len(cues), cues)
	}
	if cues[0].Text != "Hello from teletext" {
		t.Errorf("cue text = %q, want %q", cues[0].Text, "Hello from teletext")
	}
	if cues[0].StartPTS != 1000 {
		t.Errorf("cue StartPTS = %d, want 1000 (the header that began this content)", cues[0].StartPTS)
	}
	if cues[0].EndPTS != 3000 {
		t.Errorf("cue EndPTS = %d, want 3000 (the header that started the NEXT, changed cycle)", cues[0].EndPTS)
	}
}

func TestDecoder_BroadcastRedundancyDoesNotSplitCue(t *testing.T) {
	d := NewDecoder(8, 0x01)
	var headerBlock [40]byte
	headerBlock[0] = rawHammingByte(t, 0x01)
	headerBlock[1] = rawHammingByte(t, 0x00)

	pts := int64(0)

	d.Feed(buildDataField(t, 8, 0, headerBlock), pts)
	pts += 1000
	d.Feed(buildDataField(t, 8, 1, asciiRow("Same line every time")), pts)
	pts += 1000

	var lastCues int
	for i := 0; i < 5; i++ {
		d.Feed(buildDataField(t, 8, 0, headerBlock), pts)
		pts += 1000
		cues := d.Feed(buildDataField(t, 8, 1, asciiRow("Same line every time")), pts)
		pts += 1000
		lastCues += len(cues)
	}
	if lastCues != 0 {
		t.Errorf("repeated identical content across 5 retransmission cycles produced %d cues, want 0 (no cue boundary until content actually changes)", lastCues)
	}

	// Now change the content — this should finally flush exactly one cue
	// spanning the whole time the repeated text was displayed.
	d.Feed(buildDataField(t, 8, 0, headerBlock), pts)
	pts += 1000
	cues := d.Feed(buildDataField(t, 8, 1, asciiRow("Different line now")), pts)
	_ = cues // the flush happens on the NEXT header, not here

	final := d.Feed(buildDataField(t, 8, 0, headerBlock), pts+1000)
	if len(final) != 1 {
		t.Fatalf("expected exactly 1 cue once content changes, got %d: %+v", len(final), final)
	}
	if final[0].Text != "Same line every time" {
		t.Errorf("flushed cue text = %q, want %q", final[0].Text, "Same line every time")
	}
	if final[0].StartPTS != 0 {
		t.Errorf("flushed cue StartPTS = %d, want 0", final[0].StartPTS)
	}
}

func TestDecoder_ErasePageClearsWithoutNewContent(t *testing.T) {
	d := NewDecoder(8, 0x01)
	var headerBlock [40]byte
	headerBlock[0] = rawHammingByte(t, 0x01)
	headerBlock[1] = rawHammingByte(t, 0x00)

	d.Feed(buildDataField(t, 8, 0, headerBlock), 0)
	d.Feed(buildDataField(t, 8, 1, asciiRow("Visible caption")), 0)

	next := d.Feed(buildDataField(t, 8, 0, headerBlock), 1000)
	if len(next) != 0 {
		t.Fatalf("expected no cue yet, got %+v", next)
	}

	// Erase-page header: C4 is read directly off the post-inversion byte at
	// buf[5] (= data_block[3]), bit 0x80 — see handleHeader. Since inversion
	// swaps bit0<->bit7, setting the RAW byte's bit0 (0x01) here becomes
	// bit7 (0x80) once Feed inverts it.
	var eraseHeader [40]byte
	eraseHeader[0] = rawHammingByte(t, 0x01)
	eraseHeader[1] = rawHammingByte(t, 0x00)
	eraseHeader[3] = 0x01 // raw bit0 -> C4 (post-invert bit 0x80)

	eraseField := buildDataField(t, 8, 0, eraseHeader)
	cues := d.Feed(eraseField, 2000)
	if len(cues) != 1 {
		t.Fatalf("expected the previously-displayed cue to flush on erase, got %d: %+v", len(cues), cues)
	}
	if cues[0].Text != "Visible caption" {
		t.Errorf("flushed cue text = %q, want %q", cues[0].Text, "Visible caption")
	}
	if cues[0].EndPTS != 2000 {
		t.Errorf("flushed cue EndPTS = %d, want 2000", cues[0].EndPTS)
	}
}

func TestDecoder_IgnoresOtherMagazinesAndPages(t *testing.T) {
	d := NewDecoder(8, 0x01)
	var headerBlock [40]byte
	headerBlock[0] = rawHammingByte(t, 0x02) // page 0x02, not our target 0x01
	headerBlock[1] = rawHammingByte(t, 0x00)

	if cues := d.Feed(buildDataField(t, 8, 0, headerBlock), 0); len(cues) != 0 {
		t.Errorf("header for a different page must be ignored, got %+v", cues)
	}

	var wrongMagHeader [40]byte
	wrongMagHeader[0] = rawHammingByte(t, 0x01)
	wrongMagHeader[1] = rawHammingByte(t, 0x00)
	if cues := d.Feed(buildDataField(t, 3, 0, wrongMagHeader), 0); len(cues) != 0 {
		t.Errorf("header for a different magazine must be ignored, got %+v", cues)
	}
}
