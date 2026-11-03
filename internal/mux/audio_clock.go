package mux

// AudioClock computes each audio frame's Duration (90kHz ticks) for a
// fixed-samples-per-frame codec (AAC-LC's 1024 samples) without the two
// distinct sources of drift a naive `90000*samplesPerFrame/sampleRate`
// computed once and reused for every frame accumulates over a long stream:
//
//  1. Rounding drift: 90000*1024/44100 = 2089.795..., truncated to 2089
//     every frame. That's deterministic — the same fractional tick lost on
//     every single frame, forever — and it compounds: over an hour at
//     ~43.07 frames/sec, truncating 0.795 ticks/frame accumulates to
//     roughly 1.4 real seconds of audio/video desync, even though nothing
//     about the source ever actually drifted. AudioClock eliminates this
//     entirely (see NextDuration) using the same exact-rounding
//     accumulator technique mux.AppendPendingVideoSample already gets for
//     free by deriving video durations from real DTS deltas — audio's
//     fixed frame size means it can't just measure a delta the same way,
//     but it can still track the exact cumulative tick count instead of
//     re-truncating from zero every frame.
//  2. Real timeline discontinuities: a dropped frame, encoder restart, or
//     genuine clock drift in the source. NextDuration resyncs to the
//     incoming packet's own PTS whenever it has diverged from the
//     accumulated expectation by more than one nominal frame duration,
//     rather than silently continuing to extrapolate from a now-stale
//     baseline — see the Corrections counter.
type AudioClock struct {
	sampleRate      int
	samplesPerFrame int

	accumSamples int64 // total samples emitted so far
	accumTicks   int64 // total ticks emitted so far == floor(accumSamples*90000/sampleRate), always

	expectedPTS int64 // -1 until the first frame
	haveFirst   bool

	// Corrections counts how many times NextDuration has resynced to the
	// source's real PTS after detecting a discontinuity larger than one
	// nominal frame duration — a non-zero count means real drift/gaps were
	// actually observed and corrected, not just rounding noise.
	Corrections int
}

// NewAudioClock creates a clock for a fixed-samples-per-frame audio codec.
func NewAudioClock(sampleRate, samplesPerFrame int) *AudioClock {
	return &AudioClock{
		sampleRate:      sampleRate,
		samplesPerFrame: samplesPerFrame,
		expectedPTS:     -1,
	}
}

// Nominal is one frame's duration in 90 kHz ticks, rounded down.
func (c *AudioClock) Nominal() uint32 {
	return uint32(90000 * c.samplesPerFrame / c.sampleRate)
}

// NextDuration returns the Duration (90kHz ticks) to assign to the next
// frame, whose incoming timestamp is pts (also 90kHz ticks). Call exactly
// once per frame, in order.
func (c *AudioClock) NextDuration(pts int64) uint32 {
	nominal := uint32(90000 * c.samplesPerFrame / c.sampleRate)

	if !c.haveFirst {
		c.haveFirst = true
		c.expectedPTS = pts
	} else if delta := pts - c.expectedPTS; delta < 0 || delta > int64(nominal) {
		// Diverged from the accumulated expectation by more than one
		// frame's worth — a real gap or clock jump, not rounding noise.
		// Resync to the source's own truth rather than let the error
		// persist or compound.
		c.expectedPTS = pts
		c.Corrections++
	}

	c.accumSamples += int64(c.samplesPerFrame)
	// The exact total tick count for accumSamples samples, recomputed
	// fresh from the running sample total each time (not accumulated by
	// repeatedly adding a pre-truncated per-frame value) — this is what
	// keeps the cumulative rounding error permanently bounded to less
	// than 1 tick, instead of compounding without limit.
	totalTicks := c.accumSamples * 90000 / int64(c.sampleRate)
	duration := uint32(totalTicks - c.accumTicks)
	c.accumTicks = totalTicks
	c.expectedPTS += int64(duration)
	return duration
}
