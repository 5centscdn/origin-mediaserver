package mpegts

import "testing"

// A real MPEG-1 Layer II header: version=11 (MPEG-1), layer=10 (Layer II),
// protection_bit=1 (no CRC), bitrate_index=1010 (192kbps), sample_rate_index=01
// (48000Hz), padding=0, private=0, channel_mode=00 (stereo), mode_ext=00,
// copyright=0, original=1, emphasis=00. Matches the real source this
// feature was built against (ffprobe: "Audio: mp2 ..., 48000 Hz, stereo,
// ..., 192 kb/s").
var mp2Header192kbps48kHzStereo = []byte{0xFF, 0xFD, 0xA4, 0x04}

// mp2Frame192kbps48kHz is one full, real-parameter frame: header +
// 572 bytes of payload = 576 bytes total (144*192000/48000 = 576, exactly —
// no padding needed).
func makeMP2Frame(fill byte) []byte {
	frame := make([]byte, 576)
	copy(frame, mp2Header192kbps48kHzStereo)
	for i := 4; i < len(frame); i++ {
		frame[i] = fill
	}
	return frame
}

func TestSplitMP2_SingleFrame(t *testing.T) {
	frame := makeMP2Frame(0xAB)
	frames := splitMP2(frame)

	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	f := frames[0]
	if f.sampleRate != 48000 {
		t.Errorf("sampleRate = %d, want 48000", f.sampleRate)
	}
	if f.channels != 2 {
		t.Errorf("channels = %d, want 2", f.channels)
	}
	if len(f.data) != 576 {
		t.Fatalf("frame data length = %d, want 576", len(f.data))
	}
	// Passthrough: header included verbatim, unlike splitADTS.
	for i := 0; i < 4; i++ {
		if f.data[i] != mp2Header192kbps48kHzStereo[i] {
			t.Errorf("frame data[%d] = %#x, want header byte %#x (header must be kept, not stripped)", i, f.data[i], mp2Header192kbps48kHzStereo[i])
		}
	}
}

func TestSplitMP2_MultipleFramesInOnePESPayload(t *testing.T) {
	var payload []byte
	payload = append(payload, makeMP2Frame(0x11)...)
	payload = append(payload, makeMP2Frame(0x22)...)
	payload = append(payload, makeMP2Frame(0x33)...)

	frames := splitMP2(payload)
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
	}
	for i, want := range []byte{0x11, 0x22, 0x33} {
		if frames[i].data[4] != want {
			t.Errorf("frame %d payload byte = %#x, want %#x", i, frames[i].data[4], want)
		}
	}
}

func TestSplitMP2_ResyncsPastGarbageBytes(t *testing.T) {
	var payload []byte
	payload = append(payload, 0x00, 0x11, 0x22) // junk before the first real sync
	payload = append(payload, makeMP2Frame(0xAA)...)

	frames := splitMP2(payload)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1 (should skip leading junk), frames=%+v", len(frames), frames)
	}
}

func TestSplitMP2_TruncatedTrailingFrameIsDropped(t *testing.T) {
	full := makeMP2Frame(0xAA)
	truncated := full[:300] // a real frame claims 576 bytes but only 300 are present

	frames := splitMP2(truncated)
	if len(frames) != 0 {
		t.Fatalf("got %d frames from a truncated trailing frame, want 0", len(frames))
	}
}

func TestSplitMP2_SkipsNonLayerIIFrames(t *testing.T) {
	// Same header but layer bits set to 01 (Layer III) instead of 10 (Layer II).
	header := []byte{0xFF, 0xFB, 0xA4, 0x04} // 1111 11 01 1 = 0xFB
	frame := make([]byte, 576)
	copy(frame, header)

	frames := splitMP2(frame)
	if len(frames) != 0 {
		t.Errorf("got %d frames for a non-Layer-II header, want 0 (Layer II only is in scope)", len(frames))
	}
}
