package mux

import "testing"

// TestAudioClock_NoCumulativeRoundingDrift proves the actual bug this type
// fixes: 90000*1024/44100 = 2088.435..., truncated to 2088 if computed
// fresh every frame — a deterministic per-frame loss that would accumulate
// without bound over a long stream. AudioClock's running total must always
// exactly equal floor(N*1024*90000/44100), never drift from it.
func TestAudioClock_NoCumulativeRoundingDrift(t *testing.T) {
	const sampleRate = 44100
	const samplesPerFrame = 1024
	c := NewAudioClock(sampleRate, samplesPerFrame)

	const numFrames = 100_000 // ~38.7 minutes of audio — plenty to expose compounding truncation
	pts := int64(0)
	var totalDuration int64
	for i := 0; i < numFrames; i++ {
		d := c.NextDuration(pts)
		totalDuration += int64(d)
		pts += int64(d) // simulate a perfectly-clocked source: next frame's PTS is exactly where this one ended
	}

	wantExact := int64(numFrames) * samplesPerFrame * 90000 / sampleRate
	if totalDuration != wantExact {
		t.Errorf("cumulative duration = %d, want exactly %d (drift of %d ticks = %.3f real seconds)",
			totalDuration, wantExact, totalDuration-wantExact, float64(totalDuration-wantExact)/90000.0)
	}

	// The naive "compute fresh every frame" approach this replaces would
	// have drifted by exactly this much — confirm that failure mode is
	// real, so the test above is actually exercising something.
	naivePerFrame := int64(90000 * samplesPerFrame / sampleRate)
	naiveTotal := naivePerFrame * numFrames
	if naiveTotal == wantExact {
		t.Fatal("test setup is broken: the naive per-frame truncation should NOT equal the exact total")
	}

	if c.Corrections != 0 {
		t.Errorf("Corrections = %d, want 0 for a perfectly-clocked source", c.Corrections)
	}
}

// TestAudioClock_ResyncsOnRealGap verifies a genuine timeline discontinuity
// (e.g. dropped frames) is detected and corrected — the clock must not
// keep extrapolating from a now-stale baseline forever.
func TestAudioClock_ResyncsOnRealGap(t *testing.T) {
	const sampleRate = 44100
	c := NewAudioClock(sampleRate, 1024)

	pts := int64(0)
	for i := 0; i < 10; i++ {
		d := c.NextDuration(pts)
		pts += int64(d)
	}
	if c.Corrections != 0 {
		t.Fatalf("setup: expected 0 corrections before the gap, got %d", c.Corrections)
	}

	// Simulate 5 dropped frames: the source's real PTS jumps far ahead of
	// what the clock's accumulated expectation would predict.
	pts += 5 * int64(90000*1024/sampleRate)
	d := c.NextDuration(pts)
	if c.Corrections != 1 {
		t.Errorf("Corrections = %d, want 1 after a real gap", c.Corrections)
	}
	// The exact-rounding accumulator legitimately varies each frame's
	// duration by ±1 tick depending on where it sits in its rounding
	// cycle (only the cumulative sum over many frames is guaranteed
	// exact) — so check "close to nominal," not bit-for-bit equality.
	nominal := int64(90000 * 1024 / sampleRate)
	if diff := int64(d) - nominal; diff < -2 || diff > 2 {
		t.Errorf("duration immediately after resync = %d, want within 2 ticks of nominal %d", d, nominal)
	}

	// After resyncing, the clock must resume tracking the source's real
	// (post-gap) timeline, not the stale pre-gap one.
	nextExpected := pts + int64(d)
	d2 := c.NextDuration(nextExpected)
	if c.Corrections != 1 {
		t.Errorf("Corrections = %d, want still 1 — the very next frame should not itself look like a gap", c.Corrections)
	}
	_ = d2
}

// TestAudioClock_FirstFrameNeverCounts verifies the very first call
// establishes the baseline rather than being (incorrectly) treated as a
// discontinuity from the zero-value expectedPTS.
func TestAudioClock_FirstFrameNeverCounts(t *testing.T) {
	c := NewAudioClock(48000, 1024)
	c.NextDuration(1_000_000) // an arbitrary, non-zero starting PTS
	if c.Corrections != 0 {
		t.Errorf("Corrections = %d, want 0 — the first frame must establish the baseline, not trigger a resync", c.Corrections)
	}
}
