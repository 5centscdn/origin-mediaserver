package mux

import "testing"

func TestAppendPendingVideoSample(t *testing.T) {
	var samples []Sample
	var pending *Sample

	dtss := []int64{0, 3000, 6000, 9000}
	for i, dts := range dtss {
		samples, pending = AppendPendingVideoSample(samples, pending, dts, dts, i == 0, []byte{byte(i)})
	}

	// 4 packets in → 3 completed samples (the 4th is still pending).
	if len(samples) != 3 {
		t.Fatalf("got %d completed samples, want 3", len(samples))
	}
	for i, s := range samples {
		if s.DTS != dtss[i] {
			t.Errorf("samples[%d].DTS = %d, want %d", i, s.DTS, dtss[i])
		}
		if s.Duration != 3000 {
			t.Errorf("samples[%d].Duration = %d, want 3000", i, s.Duration)
		}
	}
	if pending == nil || pending.DTS != 9000 {
		t.Fatalf("pending = %+v, want a sample with DTS=9000", pending)
	}
	if !samples[0].IsKey {
		t.Error("samples[0].IsKey = false, want true (first packet was a keyframe)")
	}
}

func TestFinalizePendingVideoSample(t *testing.T) {
	pending := &Sample{DTS: 9000, PTS: 9000}
	samples, remaining := FinalizePendingVideoSample(nil, pending, 3000)

	if remaining != nil {
		t.Errorf("remaining pending = %+v, want nil", remaining)
	}
	if len(samples) != 1 {
		t.Fatalf("got %d samples, want 1", len(samples))
	}
	if samples[0].Duration != 3000 {
		t.Errorf("Duration = %d, want 3000 (fallback)", samples[0].Duration)
	}

	// Finalizing a nil pending is a safe no-op.
	samples2, remaining2 := FinalizePendingVideoSample(samples, nil, 3000)
	if len(samples2) != 1 || remaining2 != nil {
		t.Errorf("finalizing nil pending changed state: samples=%v remaining=%v", samples2, remaining2)
	}
}

func TestParseAACSampleRateAndChannels(t *testing.T) {
	// AudioSpecificConfig for AAC-LC, 44100 Hz (index 4), stereo (2 channels):
	// objectType(5 bits)=00010, samplingFreqIndex(4 bits)=0100, channelConfig(4 bits)=0010, 3 trailing bits=000
	// bitstream: 00010 0100 0010 000 -> byte0=00010010=0x12, byte1=00010000=0x10
	cfg := []byte{0x12, 0x10}
	if sr := ParseAACSampleRate(cfg); sr != 44100 {
		t.Errorf("ParseAACSampleRate = %d, want 44100", sr)
	}
	if ch := ParseAACChannels(cfg); ch != 2 {
		t.Errorf("ParseAACChannels = %d, want 2", ch)
	}

	if sr := ParseAACSampleRate(nil); sr != 0 {
		t.Errorf("ParseAACSampleRate(nil) = %d, want 0", sr)
	}
	if ch := ParseAACChannels([]byte{0x12}); ch != 0 {
		t.Errorf("ParseAACChannels(short) = %d, want 0", ch)
	}
}

// TestAppendPendingVideoSample_ConvertsAnnexBToAVCC verifies real Annex-B
// input (multiple start-code-delimited NALs, e.g. SPS+PPS+IDR as a real
// encoder sends for a keyframe access unit) is correctly re-framed as
// AVCC/length-prefixed — the format ISOBMFF's avc1 sample data requires.
// Regression test for a real bug: omitting this left a leading Annex-B
// start code in every fMP4 sample, which a strict browser MSE decoder
// (Chrome) reads as a NAL length of 1, corrupting decode — undetected by
// this project's own tests and by lenient tools (ffprobe) alike.
func TestAppendPendingVideoSample_ConvertsAnnexBToAVCC(t *testing.T) {
	sps := []byte{0x67, 0x42, 0x00, 0x1e}
	pps := []byte{0x68, 0xce, 0x3c, 0x80}
	idr := []byte{0x65, 0x88, 0x84, 0x00, 0x37}

	annexB := []byte{0, 0, 0, 1}
	annexB = append(annexB, sps...)
	annexB = append(annexB, 0, 0, 0, 1)
	annexB = append(annexB, pps...)
	annexB = append(annexB, 0, 0, 0, 1)
	annexB = append(annexB, idr...)

	var samples []Sample
	var pending *Sample
	samples, pending = AppendPendingVideoSample(samples, pending, 0, 0, true, annexB)
	// Complete it with a second call (duration comes from the delta).
	samples, _ = AppendPendingVideoSample(samples, pending, 3000, 3000, false, []byte{0x41})

	if len(samples) != 1 {
		t.Fatalf("got %d completed samples, want 1", len(samples))
	}
	got := samples[0].Data

	want := []byte{0, 0, 0, byte(len(sps))}
	want = append(want, sps...)
	want = append(want, 0, 0, 0, byte(len(pps)))
	want = append(want, pps...)
	want = append(want, 0, 0, 0, byte(len(idr)))
	want = append(want, idr...)

	if string(got) != string(want) {
		t.Errorf("converted sample data = %x, want %x", got, want)
	}
	// The critical assertion: no leading Annex-B start code (00 00 00 01)
	// — the exact byte sequence a strict decoder misreads as a NAL length.
	if len(got) >= 4 && got[0] == 0 && got[1] == 0 && got[2] == 0 && got[3] == 1 {
		t.Error("converted sample still starts with an Annex-B start code (00 00 00 01)")
	}
}
