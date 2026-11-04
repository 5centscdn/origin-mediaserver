package dvrowner

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/node"
)

// BenchmarkCheck measures the per-request cost: a non-nDVR stream (cached
// bypass) and an nDVR stream decided against two peers, with 2,000
// recorded streams tracked.
func BenchmarkCheck(b *testing.B) {
	local := newFakeLocal()
	local.dvrOn["live/cam"] = true
	cl := &fakeCluster{peers: []node.PeerDVR{
		{NodeID: "id-b", NodeName: "origin-b", Priority: 100, Fresh: true, Known: true,
			State: node.DVRState{DVRStartMS: t1000, DVREndMS: t1030}},
		{NodeID: "id-c", NodeName: "origin-c", Priority: 100, Fresh: true},
	}}
	tr := New(cl, local.deps(), "")
	for i := 0; i < 2000; i++ {
		tr.SegmentIndexed(media.StreamKey{App: "live", Name: fmt.Sprint("s", i)}, t1000, t1030)
	}
	local.publish("live", "cam", time.Now())
	ctx := context.Background()
	b.Run("bypass", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			tr.Check(ctx, "live", "plain")
		}
	})
	b.Run("ndvr", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			tr.Check(ctx, "live", "cam")
		}
	})
}
