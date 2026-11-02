package captiondecode

import "testing"

func TestCEA608_PopOn_ProducesCueOnSecondEOC(t *testing.T) {
	d := NewCEA608Decoder()

	// RCL — enter Pop-On mode.
	if _, ok := d.Feed(0x14, 0x20, 100); ok {
		t.Fatal("RCL should not itself produce a cue")
	}
	// Text "HI" into the non-displayed buffer.
	if _, ok := d.Feed('H', 'I', 110); ok {
		t.Fatal("text should not itself produce a cue")
	}
	// First EOC: swaps memories, makes "HI" visible — nothing to flush yet.
	if _, ok := d.Feed(0x14, 0x2F, 200); ok {
		t.Fatal("first EOC should not flush a cue (nothing was visible before it)")
	}
	// EDM: erase displayed memory — flushes the now-visible "HI".
	cue, ok := d.Feed(0x14, 0x2C, 250)
	if !ok {
		t.Fatal("expected EDM to flush a cue")
	}
	if cue.Text != "HI" || cue.StartPTS != 200 || cue.EndPTS != 250 {
		t.Errorf("cue = %+v, want {StartPTS:200 EndPTS:250 Text:HI}", cue)
	}
}

func TestCEA608_DoubledControlCodeIsExecutedOnce(t *testing.T) {
	d := NewCEA608Decoder()
	d.Feed(0x14, 0x20, 100) // RCL
	d.Feed('H', 'I', 110)
	// EOC sent twice in a row (real-world error-resilience convention).
	d.Feed(0x14, 0x2F, 200)
	if _, ok := d.Feed(0x14, 0x2F, 201); ok {
		t.Fatal("doubled EOC should be suppressed, not executed a second time")
	}
	// A third, later EDM should still flush exactly the one caption.
	cue, ok := d.Feed(0x14, 0x2C, 250)
	if !ok || cue.Text != "HI" {
		t.Errorf("cue = %+v, ok=%v — doubling suppression corrupted state", cue, ok)
	}
}

func TestCEA608_RollUp_FlushesOnCarriageReturn(t *testing.T) {
	d := NewCEA608Decoder()
	d.Feed(0x14, 0x25, 101) // RU2
	d.Feed('B', 'Y', 110)
	d.Feed('E', 0, 120) // odd-length text: second byte 0 is padding, not a char

	cue, ok := d.Feed(0x14, 0x2D, 150) // CR
	if !ok {
		t.Fatal("expected CR to flush the roll-up line")
	}
	if cue.Text != "BYE" || cue.StartPTS != 110 || cue.EndPTS != 150 {
		t.Errorf("cue = %+v, want {StartPTS:110 EndPTS:150 Text:BYE}", cue)
	}
}

func TestCEA608_StandardCharOverrides(t *testing.T) {
	d := NewCEA608Decoder()
	d.Feed(0x14, 0x25, 100) // RU2
	// 0x2A -> á, 0x7E -> ñ (Basic North American Character Set deviations).
	d.Feed(0x2A, 0x7E, 110)
	cue, ok := d.Feed(0x14, 0x2D, 150)
	if !ok {
		t.Fatal("expected a cue")
	}
	if cue.Text != "áñ" {
		t.Errorf("cue.Text = %q, want %q", cue.Text, "áñ")
	}
}

func TestCEA608_CC2IsIgnored(t *testing.T) {
	d := NewCEA608Decoder()
	// 0x1C/0x20 is CC2's RCL (first byte 0x18-0x1F) — out of scope.
	if _, ok := d.Feed(0x1C, 0x20, 100); ok {
		t.Fatal("CC2 control codes should be ignored, not processed")
	}
	// Confirm mode never changed: feeding CC1 text now should be dropped
	// (mode608None), not silently captured under an accidental CC2 RCL.
	d.Feed('H', 'I', 110)
	if _, ok := d.Feed(0x14, 0x2F, 200); ok {
		t.Fatal("no text should have been captured since mode was never set")
	}
}

func TestCEA608_Flush_AtStreamEnd(t *testing.T) {
	d := NewCEA608Decoder()
	d.Feed(0x14, 0x25, 100) // RU2
	d.Feed('H', 'I', 110)
	cue, ok := d.Flush(500)
	if !ok || cue.Text != "HI" || cue.StartPTS != 110 || cue.EndPTS != 500 {
		t.Errorf("Flush() = %+v, ok=%v, want a completed HI cue", cue, ok)
	}
}
