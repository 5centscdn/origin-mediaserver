package mpegts

import (
	"context"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// TestListenUDP_AcceptsSchemePrefix guards against a real bug found while
// verifying this live: pull_sources.url values look like "udp://host:port"
// (matching every other pull protocol's URL shape), but net.ResolveUDPAddr
// rejects a "udp://" scheme prefix outright ("too many colons in
// address") — ListenUDP must strip it before resolving.
func TestListenUDP_AcceptsSchemePrefix(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	bus := media.NewBus()

	errCh := make(chan error, 1)
	go func() { errCh <- ListenUDP(ctx, "udp://127.0.0.1:0", bus) }()

	time.Sleep(50 * time.Millisecond) // let the socket bind
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("ListenUDP(%q) returned err: %v", "udp://127.0.0.1:0", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ListenUDP to return after cancel")
	}
}
