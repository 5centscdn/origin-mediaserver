package livestore

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newTestStore() (*Store, *clock) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	s := New()
	s.now = c.now
	return s, c
}

func TestStore_WriteCopiesAndReads(t *testing.T) {
	s, _ := newTestStore()
	buf := []byte("segment")
	s.WriteFile("/d/hls/a/s/seg000001.ts", buf)
	buf[0] = 'X' // packagers reuse their buffers
	if got, ok := s.ReadFile("/d/hls/a/s/seg000001.ts"); !ok || string(got) != "segment" {
		t.Fatalf("ReadFile = %q, %v", got, ok)
	}
	if _, ok := s.ReadFile("/d/hls/a/s/seg000002.ts"); ok {
		t.Fatal("missing file found")
	}
	s.Remove("/d/hls/a/s/seg000001.ts")
	if s.Exists("/d/hls/a/s/seg000001.ts") {
		t.Fatal("Remove didn't remove")
	}
}

// Segments expire after the directory's TTL; playlists and init segments
// stay while the stream is live however old they are.
func TestStore_SegmentsExpirePinnedStay(t *testing.T) {
	s, c := newTestStore()
	const d = "/d/dash/a/s"
	s.SetTTL(d, 60*time.Second)
	s.WriteFile(d+"/init-video.mp4", []byte("i"))
	s.WriteFile(d+"/manifest.mpd", []byte("m"))
	s.WriteFile(d+"/seg-video-1.m4s", []byte("1"))
	c.advance(40 * time.Second)
	s.WriteFile(d+"/seg-video-2.m4s", []byte("2"))
	c.advance(30 * time.Second)
	s.Expire()
	if s.Exists(d + "/seg-video-1.m4s") {
		t.Error("segment 1 (70 s old, TTL 60 s) not expired")
	}
	for _, f := range []string{"seg-video-2.m4s", "init-video.mp4", "manifest.mpd"} {
		if !s.Exists(d + "/" + f) {
			t.Errorf("%s expired too early", f)
		}
	}
}

// A released directory keeps its playlist for ReleaseGrace (players
// finishing the last segments), then goes — unless a new source (reconnect,
// pull fallback) writes to it first.
func TestStore_ReleaseGraceAndTakeover(t *testing.T) {
	s, c := newTestStore()
	s.WriteFile("/d/hls/a/gone/index.m3u8", []byte("x"))
	s.WriteFile("/d/hls/a/gone/audio/index.m3u8", []byte("x"))
	s.WriteFile("/d/hls/a/back/index.m3u8", []byte("x"))
	s.Release("/d/hls/a/gone")
	s.Release("/d/hls/a/back")

	c.advance(ReleaseGrace / 2)
	s.Expire()
	if !s.Exists("/d/hls/a/gone/index.m3u8") {
		t.Fatal("playlist dropped before the grace period ended")
	}
	s.WriteFile("/d/hls/a/back/index.m3u8", []byte("y")) // the source reconnected

	c.advance(ReleaseGrace)
	s.Expire()
	if s.Exists("/d/hls/a/gone/index.m3u8") || s.Exists("/d/hls/a/gone/audio/index.m3u8") || s.HasDir("/d/hls/a/gone") {
		t.Error("released directory (and its audio/ subdirectory) still there after the grace period")
	}
	if got, _ := s.ReadFile("/d/hls/a/back/index.m3u8"); string(got) != "y" {
		t.Error("directory taken over by a new source was dropped")
	}
}

// At the high mark the store moves what's in RAM to disk (that's what
// frees memory) and writes new files there, reports a warning, and goes
// back to RAM below the low mark; files on disk are deleted when they
// expire or are replaced.
func TestStore_DiskModeUnderMemoryPressure(t *testing.T) {
	s, c := newTestStore()
	root := t.TempDir()
	d := filepath.Join(root, "hls", "a", "s")
	s.SetTTL(d, time.Minute)
	pct := 50.0
	orig := memUsedPct
	memUsedPct = func() float64 { return pct }
	defer func() { memUsedPct = orig }()
	s.highPct, s.lowPct = 90, 80 // policy defaults to hybrid

	s.WriteFile(d+"/index.m3u8", []byte("playlist-1"))
	s.WriteFile(d+"/seg000001.ts", []byte("seg-1"))
	s.check()
	if st := s.Status(); st.Mode != "memory" || st.Warning != "" || st.Files != 2 {
		t.Fatalf("at 50%%: %+v", st)
	}
	if _, err := os.Stat(d + "/seg000001.ts"); err == nil {
		t.Fatal("memory mode wrote to disk")
	}

	pct = 92
	s.check()
	st := s.Status()
	if st.Mode != "disk" || st.Warning == "" || st.Files != 0 || st.DiskFiles != 2 {
		t.Fatalf("at 92%%: %+v", st)
	}
	if b, err := os.ReadFile(d + "/seg000001.ts"); err != nil || string(b) != "seg-1" {
		t.Fatalf("segment not moved to disk: %q %v", b, err)
	}
	s.WriteFile(d+"/seg000002.ts", []byte("seg-2"))
	if b, err := os.ReadFile(d + "/seg000002.ts"); err != nil || string(b) != "seg-2" {
		t.Fatalf("new segment not written to disk: %q %v", b, err)
	}
	rd, n, ok := s.Open(d + "/seg000002.ts")
	if !ok || n != 5 {
		t.Fatalf("Open from disk: ok=%v size=%d", ok, n)
	}
	b, _ := io.ReadAll(rd)
	rd.Close()
	if string(b) != "seg-2" {
		t.Fatalf("Open read %q", b)
	}

	pct = 85 // between the marks: stays on disk
	s.check()
	if s.Status().Mode != "disk" {
		t.Fatal("left disk mode above the low mark")
	}

	pct = 70
	s.check()
	if st := s.Status(); st.Mode != "memory" || st.Warning != "" {
		t.Fatalf("at 70%%: %+v", st)
	}
	s.WriteFile(d+"/index.m3u8", []byte("playlist-2")) // rewritten in RAM
	if _, err := os.Stat(d + "/index.m3u8"); err == nil {
		t.Error("stale playlist left on disk after it was rewritten in RAM")
	}
	if got, _ := s.ReadFile(d + "/index.m3u8"); string(got) != "playlist-2" {
		t.Errorf("playlist = %q", got)
	}

	c.advance(2 * time.Minute)
	s.Expire()
	for _, f := range []string{"seg000001.ts", "seg000002.ts"} {
		if _, err := os.Stat(d + "/" + f); err == nil {
			t.Errorf("expired %s still on disk", f)
		}
	}
}

// mode = "memory" never moves to disk, only warns; mode = "disk" writes
// everything to disk from the start, without a warning.
func TestStore_MemoryAndDiskModes(t *testing.T) {
	orig := memUsedPct
	defer func() { memUsedPct = orig }()
	memUsedPct = func() float64 { return 95 }

	mem, _ := newTestStore()
	mem.policy, mem.highPct, mem.lowPct = "memory", 90, 80
	d := filepath.Join(t.TempDir(), "hls", "a", "s")
	mem.WriteFile(d+"/seg000001.ts", []byte("x"))
	mem.check()
	if st := mem.Status(); st.Mode != "memory" || st.DiskFiles != 0 || st.Warning == "" {
		t.Fatalf("memory mode at 95%%: %+v", st)
	}
	if _, err := os.Stat(d + "/seg000001.ts"); err == nil {
		t.Fatal("memory mode wrote to disk")
	}

	disk, _ := newTestStore()
	disk.StartMonitor("disk", 90, 80, time.Hour)
	d2 := filepath.Join(t.TempDir(), "dash", "a", "s")
	disk.WriteFile(d2+"/seg-video-1.m4s", []byte("y"))
	if b, err := os.ReadFile(d2 + "/seg-video-1.m4s"); err != nil || string(b) != "y" {
		t.Fatalf("disk mode didn't write to disk: %q %v", b, err)
	}
	if st := disk.Status(); st.Policy != "disk" || st.Mode != "disk" || st.Warning != "" {
		t.Fatalf("disk mode status: %+v", st)
	}
}
