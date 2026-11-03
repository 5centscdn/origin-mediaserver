package failover

import (
	"context"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// With an "mp4" backup, a missing primary fails over to the file: a
// failover_backup stream is registered at the primary's own key, gets the
// file's CodecInfo, and carries frames, so existing playback URLs keep
// working.
func TestFailoverToMP4Backup(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.DataDir = t.TempDir()
	cfg.Failover.WatchdogIntervalMs = 20
	reg := media.NewRegistry()
	m := NewManager(cfg, reg, nil)
	live := make(chan *media.Stream, 1)
	m.OnBackupLive = func(s *media.Stream) { live <- s }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Watch(ctx, Rule{
		AppName: "live", PrimaryStream: "main",
		BackupURL: "../ingest/mp4file/testdata/clip.mp4", BackupProtocol: "mp4",
		StallTimeoutMs: 100, WarmupMs: 50,
		AutoRestore: true, // keeps the monitor (and its Status) alive while on backup
	})

	var backup *media.Stream
	select {
	case backup = <-live:
	case <-time.After(3 * time.Second):
		t.Fatal("no failover to the mp4 backup")
	}
	if backup.Key != (media.StreamKey{App: "live", Name: "main"}) || backup.SourceType != "failover_backup" {
		t.Fatalf("backup stream %+v", backup)
	}
	sub := backup.Bus.Subscribe(ctx)
	defer sub.Close()
	select {
	case p := <-sub.Chan():
		if p == nil {
			t.Fatal("bus closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no packets from the mp4 backup")
	}
	if ci := backup.Bus.CodecInfo(); ci == nil || ci.Width != 160 {
		t.Fatalf("backup CodecInfo %+v", ci)
	}
	deadline := time.Now().Add(time.Second)
	for m.Status("live", "main") != StateActiveBackup && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s := m.Status("live", "main"); s != StateActiveBackup {
		t.Errorf("state %v, want active_backup", s)
	}
}
