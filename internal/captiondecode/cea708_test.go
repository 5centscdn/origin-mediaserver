package captiondecode

import "testing"

// feedPacket splits a complete, hand-built DTVCC packet into 2-byte triplets
// (the first as DTVCC_PACKET_START, the rest as DTVCC_PACKET_DATA) and feeds
// them to d, returning whatever cues complete on the final triplet (a real
// DTVCC packet only finishes decoding once its last byte arrives).
func feedPacket(d *CEA708Decoder, pkt []byte, pts int64) []Cue {
	var cues []Cue
	for i := 0; i+1 < len(pkt); i += 2 {
		ccType := uint8(3)
		if i > 0 {
			ccType = 2
		}
		cues = d.Feed(true, ccType, pkt[i], pkt[i+1], pts)
	}
	return cues
}

// TestCEA708_ServiceOne_WindowLifecycleAcrossPackets builds two real DTVCC
// packets by hand: the first defines+shows window 0 and writes "HI" into it
// (Service 1), the second later hides that window — verifying state
// (current window, visibility, accumulated text) survives correctly across
// separate packets/frames, and that DefineWindow's 6 parameter bytes and
// SetWindowAttributes-adjacent commands are correctly skipped without
// desyncing subsequent text.
func TestCEA708_ServiceOne_WindowLifecycleAcrossPackets(t *testing.T) {
	d := NewCEA708Decoder()

	// Packet 1: CW0, DF0(+6 arbitrary param bytes), DSW(bitmap=1), "HI".
	block1 := []byte{0x80, 0x98, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x89, 0x01, 'H', 'I'}
	svcHeader1 := byte((1 << 5) | len(block1)) // service_number=1, block_size=len(block1)
	body1 := append([]byte{svcHeader1}, block1...)
	pktHeader1 := byte((len(body1) + 1) / 2) // packet_size_code = total_len/2 (total = 1+len(body1))
	pkt1 := append([]byte{pktHeader1}, body1...)
	if len(pkt1)%2 != 0 {
		t.Fatalf("test construction bug: packet 1 length %d is odd", len(pkt1))
	}

	if cues := feedPacket(d, pkt1, 100); len(cues) != 0 {
		t.Fatalf("packet 1 should not flush any cue yet, got %+v", cues)
	}

	// Packet 2: CW0, HDW(bitmap=1), NUL padding (to keep the packet even-length).
	block2 := []byte{0x80, 0x8A, 0x01, 0x00}
	svcHeader2 := byte((1 << 5) | len(block2))
	body2 := append([]byte{svcHeader2}, block2...)
	pktHeader2 := byte((len(body2) + 1) / 2)
	pkt2 := append([]byte{pktHeader2}, body2...)
	if len(pkt2)%2 != 0 {
		t.Fatalf("test construction bug: packet 2 length %d is odd", len(pkt2))
	}

	cues := feedPacket(d, pkt2, 200)
	if len(cues) != 1 {
		t.Fatalf("expected exactly 1 cue from packet 2, got %+v", cues)
	}
	if cues[0].Text != "HI" || cues[0].StartPTS != 100 || cues[0].EndPTS != 200 {
		t.Errorf("cue = %+v, want {StartPTS:100 EndPTS:200 Text:HI}", cues[0])
	}
}

// TestCEA708_ServiceOtherThanOneIsIgnored confirms a non-Service-1 block is
// skipped without corrupting Service 1's own decode in the same packet.
func TestCEA708_ServiceOtherThanOneIsIgnored(t *testing.T) {
	d := NewCEA708Decoder()

	// Service 2 block: CW0, "XX" (would corrupt text if wrongly attributed to service 1).
	svc2Block := []byte{0x80, 'X', 'X'}
	svc2Header := byte((2 << 5) | len(svc2Block))

	// Service 1 block: CW0, DSW(bitmap=1), "OK".
	svc1Block := []byte{0x80, 0x89, 0x01, 'O', 'K'}
	svc1Header := byte((1 << 5) | len(svc1Block))

	body := append([]byte{svc2Header}, svc2Block...)
	body = append(body, svc1Header)
	body = append(body, svc1Block...)
	pktHeader := byte((len(body) + 1) / 2)
	pkt := append([]byte{pktHeader}, body...)
	if len(pkt)%2 != 0 {
		pkt = append(pkt, 0x00) // pad to even length; adjust size code accordingly
		pkt[0] = byte(len(pkt) / 2)
	}

	feedPacket(d, pkt, 100)

	// Now hide service 1's window and confirm its text is exactly "OK",
	// not contaminated by service 2's "XX".
	hideBlock := []byte{0x8A, 0x01, 0x00, 0x00}
	hideHeader := byte((1 << 5) | len(hideBlock))
	hideBody := append([]byte{hideHeader}, hideBlock...)
	hidePktHeader := byte((len(hideBody) + 1) / 2)
	hidePkt := append([]byte{hidePktHeader}, hideBody...)
	if len(hidePkt)%2 != 0 {
		t.Fatalf("test construction bug: hide packet length %d is odd", len(hidePkt))
	}

	cues := feedPacket(d, hidePkt, 200)
	if len(cues) != 1 {
		t.Fatalf("expected exactly 1 cue, got %+v", cues)
	}
	if cues[0].Text != "OK" {
		t.Errorf("cue.Text = %q, want %q (service 2 data must not leak into service 1's text)", cues[0].Text, "OK")
	}
}

func TestCEA708_Flush_AtStreamEnd(t *testing.T) {
	d := NewCEA708Decoder()
	block := []byte{0x80, 0x89, 0x01, 'H', 'I', 0x00}
	header := byte((1 << 5) | len(block))
	body := append([]byte{header}, block...)
	pktHeader := byte((len(body) + 1) / 2)
	pkt := append([]byte{pktHeader}, body...)
	if len(pkt)%2 != 0 {
		t.Fatalf("test construction bug: packet length %d is odd", len(pkt))
	}

	feedPacket(d, pkt, 100)
	cue, ok := d.Flush(500)
	if !ok || cue.Text != "HI" || cue.StartPTS != 100 || cue.EndPTS != 500 {
		t.Errorf("Flush() = %+v, ok=%v, want a completed HI cue", cue, ok)
	}
}
