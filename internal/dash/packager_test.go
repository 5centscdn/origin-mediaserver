package dash

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/media"
)

func touchSegmentFiles(t *testing.T, dir string, seq int) {
	t.Helper()
	for _, name := range []string{"seg-video-", "seg-audio-"} {
		path := filepath.Join(dir, fmt.Sprintf("%s%d.m4s", name, seq))
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

func segmentFilesExist(dir string, seq int) bool {
	_, err1 := os.Stat(filepath.Join(dir, fmt.Sprintf("seg-video-%d.m4s", seq)))
	_, err2 := os.Stat(filepath.Join(dir, fmt.Sprintf("seg-audio-%d.m4s", seq)))
	return err1 == nil && err2 == nil
}

// TestCleanupOldSegment_RemovesOnlyExpiredPair verifies the DVR window
// cleanup deletes exactly the one segment pair that just aged out — not
// more, not less — and leaves everything still within the window alone.
func TestCleanupOldSegment_RemovesOnlyExpiredPair(t *testing.T) {
	cfg := &config.Config{}
	cfg.DASH.SegmentDurationS = 4
	cfg.DASH.DVRWindowS = 12 // window holds 3 segments (12/4)

	dir := t.TempDir()
	stream := &media.Stream{Key: media.StreamKey{App: "test", Name: "s1"}, Bus: media.NewBus()}
	p := &Packager{cfg: cfg, stream: stream, outDir: dir, log: slog.Default()}

	for seq := 1; seq <= 5; seq++ {
		touchSegmentFiles(t, dir, seq)
	}

	// Cutting segment 5: maxSegments=3, expireSeq=5-3=2 — segment 1 stays
	// (never touched by this call), segment 2 should be removed.
	p.cleanupOldSegment(5)

	if segmentFilesExist(dir, 2) {
		t.Error("segment 2 should have been removed (aged out of the 12s/3-segment window)")
	}
	for _, seq := range []int{1, 3, 4, 5} {
		if !segmentFilesExist(dir, seq) {
			t.Errorf("segment %d should NOT have been removed", seq)
		}
	}
}

// TestCleanupOldSegment_NeverDeletesBeforeStartNumber guards against
// deleting nonexistent low-numbered segments (or worse, some unrelated
// negative/zero-indexed path) before the window has actually filled up —
// the MPD's SegmentTemplate declares startNumber="1", so expireSeq < 1
// must be a no-op.
func TestCleanupOldSegment_NeverDeletesBeforeStartNumber(t *testing.T) {
	cfg := &config.Config{}
	cfg.DASH.SegmentDurationS = 4
	cfg.DASH.DVRWindowS = 40 // window holds 10 segments — far more than cut so far

	dir := t.TempDir()
	stream := &media.Stream{Key: media.StreamKey{App: "test", Name: "s1"}, Bus: media.NewBus()}
	p := &Packager{cfg: cfg, stream: stream, outDir: dir, log: slog.Default()}

	touchSegmentFiles(t, dir, 1)
	touchSegmentFiles(t, dir, 2)

	p.cleanupOldSegment(2) // expireSeq = 2-10 = -8, well before startNumber

	if !segmentFilesExist(dir, 1) || !segmentFilesExist(dir, 2) {
		t.Error("cleanup ran before the window filled up — should have been a no-op")
	}
}
