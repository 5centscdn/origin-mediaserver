package hls

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestWaitForUpdate_SegmentOnly(t *testing.T) {
	pl := NewPlaylist(t.TempDir(), 5, 2)

	done := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		pl.WaitForUpdate(ctx, 3, -1) // wait for segment seq 3 to exist, no particular part
		close(done)
	}()

	// Adding earlier segments must not satisfy the wait.
	for seq := 0; seq < 3; seq++ {
		pl.AddSegment(Segment{Seq: seq, URI: "seg.m4s", DurationS: 2})
		select {
		case <-done:
			t.Fatalf("WaitForUpdate returned early after segment %d", seq)
		case <-time.After(20 * time.Millisecond):
		}
	}

	pl.AddSegment(Segment{Seq: 3, URI: "seg003.m4s", DurationS: 2})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForUpdate did not return after the target segment was added")
	}
}

func TestWaitForUpdate_Part(t *testing.T) {
	pl := NewPlaylist(t.TempDir(), 5, 2)
	pl.EnableParts(0.35, 1.05)

	done := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		pl.WaitForUpdate(ctx, 0, 2) // wait for the 3rd part (index 2) of segment 0
		close(done)
	}()

	for i := 0; i < 2; i++ {
		pl.AddPart(0, Part{URI: "part.m4s", DurationS: 0.35, Independent: i == 0})
		select {
		case <-done:
			t.Fatalf("WaitForUpdate returned early after part %d", i)
		case <-time.After(20 * time.Millisecond):
		}
	}

	pl.AddPart(0, Part{URI: "part002.m4s", DurationS: 0.35})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForUpdate did not return after the target part was added")
	}
}

func TestWaitForUpdate_SegmentFinalizedSatisfiesPendingPartWait(t *testing.T) {
	pl := NewPlaylist(t.TempDir(), 5, 2)
	pl.EnableParts(0.35, 1.05)

	// Waiting on a part that never arrives — but the segment finalizing
	// (with fewer parts than requested) must still satisfy the wait, since
	// satisfiesLocked treats "segment already finalized" as sufficient
	// regardless of the requested part index.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		pl.WaitForUpdate(ctx, 0, 10)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	pl.AddPart(0, Part{URI: "part000.m4s", DurationS: 0.35, Independent: true})
	pl.AddSegment(Segment{Seq: 0, URI: "seg000000.m4s", DurationS: 2})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForUpdate did not return after the segment was finalized")
	}
}

func TestWaitForUpdate_ContextCancellation(t *testing.T) {
	pl := NewPlaylist(t.TempDir(), 5, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	pl.WaitForUpdate(ctx, 100, -1) // segment 100 never arrives
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("WaitForUpdate took %v to return after context cancellation, want ~50ms", elapsed)
	}
}

func TestSatisfiesLocked(t *testing.T) {
	pl := NewPlaylist(t.TempDir(), 5, 2)
	pl.AddSegment(Segment{Seq: 0, URI: "a", DurationS: 2})
	pl.AddSegment(Segment{Seq: 1, URI: "b", DurationS: 2})

	pl.mu.RLock()
	defer pl.mu.RUnlock()

	if !pl.satisfiesLocked(0, -1) {
		t.Error("expected seq 0 to already be satisfied")
	}
	if !pl.satisfiesLocked(1, -1) {
		t.Error("expected seq 1 to already be satisfied")
	}
	if pl.satisfiesLocked(2, -1) {
		t.Error("expected seq 2 (not yet added) to not be satisfied")
	}
}

func TestRender_PreloadHint(t *testing.T) {
	pl := NewPlaylist(t.TempDir(), 5, 2)
	pl.EnableParts(0.35, 1.05)
	pl.AddPart(0, Part{URI: "seg000000_part000.m4s", DurationS: 0.35, Independent: true})

	// Before any hint is set, none should appear.
	if strings.Contains(pl.Render(), "EXT-X-PRELOAD-HINT") {
		t.Error("unexpected EXT-X-PRELOAD-HINT before SetPreloadHint was ever called")
	}

	pl.SetPreloadHint("seg000000_part001.m4s")
	out := pl.Render()
	want := `#EXT-X-PRELOAD-HINT:TYPE=PART,URI="seg000000_part001.m4s"`
	if !strings.Contains(out, want) {
		t.Errorf("Render() missing %q, got:\n%s", want, out)
	}

	// A part rendered after the hint was set must still appear as a normal
	// EXT-X-PART entry too — the hint doesn't replace it.
	pl.AddPart(0, Part{URI: "seg000000_part001.m4s", DurationS: 0.35})
	out = pl.Render()
	if !strings.Contains(out, `#EXT-X-PART:DURATION=0.350,URI="seg000000_part001.m4s"`) {
		t.Errorf("Render() missing the finalized part, got:\n%s", out)
	}
}

func TestRender_PreloadHintOmittedWithoutParts(t *testing.T) {
	pl := NewPlaylist(t.TempDir(), 5, 2)
	pl.SetPreloadHint("seg000000_part000.m4s") // no EnableParts call — hint must never surface
	pl.AddSegment(Segment{Seq: 0, URI: "seg000000.ts", DurationS: 2})

	if strings.Contains(pl.Render(), "EXT-X-PRELOAD-HINT") {
		t.Error("EXT-X-PRELOAD-HINT must not appear when LL-HLS parts aren't enabled")
	}
}

func TestRenderDelta_SkipsOldSegments(t *testing.T) {
	// targetDurS=3 with 2s segments avoids render()'s "bump TARGETDURATION
	// up to ceil(maxSeen)+1" rounding kicking in (which would otherwise
	// silently raise the effective target duration this test computes
	// canSkipUntilS from).
	pl := NewPlaylist(t.TempDir(), 20, 3)
	pl.EnableParts(0.35, 1.05)

	// canSkipUntilS = 6 * targetDur(3) = 18s. 20 segments * 2s = 40s total,
	// so segments older than the most recent 9 (18s) should be skippable.
	for seq := 0; seq < 20; seq++ {
		pl.AddSegment(Segment{Seq: seq, URI: "seg.m4s", DurationS: 2})
	}

	full := pl.Render()
	if strings.Contains(full, "EXT-X-SKIP") {
		t.Error("Render() (non-delta) must never contain EXT-X-SKIP")
	}

	delta := pl.RenderDelta()
	if !strings.Contains(delta, "#EXT-X-SKIP:SKIPPED-SEGMENTS=11") {
		t.Errorf("RenderDelta() missing expected EXT-X-SKIP count, got:\n%s", delta)
	}
	if !strings.Contains(delta, "CAN-SKIP-UNTIL=18.000") {
		t.Errorf("RenderDelta() missing CAN-SKIP-UNTIL in EXT-X-SERVER-CONTROL, got:\n%s", delta)
	}
	// The most recent 9 segments' worth of duration must still be listed in full.
	if strings.Count(delta, "#EXTINF:") != 9 {
		t.Errorf("RenderDelta() listed %d segments in full, want 9", strings.Count(delta, "#EXTINF:"))
	}
	if strings.Count(full, "#EXTINF:") != 20 {
		t.Errorf("Render() listed %d segments, want all 20", strings.Count(full, "#EXTINF:"))
	}
}

func TestRenderDelta_NoSkipWhenWindowTooSmall(t *testing.T) {
	pl := NewPlaylist(t.TempDir(), 5, 6) // canSkipUntilS = 6*6 = 36s; window only holds ~10s
	pl.EnableParts(0.35, 1.05)
	for seq := 0; seq < 5; seq++ {
		pl.AddSegment(Segment{Seq: seq, URI: "seg.m4s", DurationS: 2})
	}

	delta := pl.RenderDelta()
	if strings.Contains(delta, "EXT-X-SKIP") {
		t.Errorf("RenderDelta() should skip nothing when the whole window doesn't cover canSkipUntilS, got:\n%s", delta)
	}
	if strings.Count(delta, "#EXTINF:") != 5 {
		t.Errorf("RenderDelta() listed %d segments, want all 5 (nothing skippable)", strings.Count(delta, "#EXTINF:"))
	}
}
