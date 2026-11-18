package media

import (
	"context"
	"testing"
	"time"
)

// TestBusSubscribeAfterClose: subscribing to a closed bus (an output that
// starts after a fast publish/disconnect) gets a closed channel, so its
// reader exits instead of waiting forever.
func TestBusSubscribeAfterClose(t *testing.T) {
	b := NewBus()
	b.Close()
	sub := b.Subscribe(context.Background())
	select {
	case _, ok := <-sub.Chan():
		if ok {
			t.Fatal("received a packet from a closed bus")
		}
	case <-time.After(time.Second):
		t.Fatal("subscription on a closed bus never closed")
	}
	sub.Close() // must not panic (double close)
	if n := b.SubscriberCount(); n != 0 {
		t.Fatalf("SubscriberCount = %d, want 0", n)
	}
}
