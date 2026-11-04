package dvrowner

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/dvr"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
	"github.com/5centscdn/origin-mediaserver/internal/node"
)

// fakeCluster counts DVRView calls, so tests can prove non-nDVR streams
// never touch cluster state.
type fakeCluster struct {
	views atomic.Int64
	kicks atomic.Int64
	peers []node.PeerDVR
}

func (f *fakeCluster) NodeID() string   { return "id-a" }
func (f *fakeCluster) NodeName() string { return "origin-a" }
func (f *fakeCluster) Priority() int    { return 100 }
func (f *fakeCluster) Ready() bool      { return true }
func (f *fakeCluster) Kick()            { f.kicks.Add(1) }
func (f *fakeCluster) DVRView(app, stream string) []node.PeerDVR {
	f.views.Add(1)
	return f.peers
}

// fakeLocal is a node's local state: live streams, recordings and which
// streams have nDVR enabled.
type fakeLocal struct {
	mu        sync.Mutex
	live      map[media.StreamKey]*media.Stream
	tl        map[media.StreamKey]dvr.Timeline
	dvrOn     map[string]bool // "app/stream"
	eligCalls atomic.Int64
}

func newFakeLocal() *fakeLocal {
	return &fakeLocal{live: map[media.StreamKey]*media.Stream{}, tl: map[media.StreamKey]dvr.Timeline{}, dvrOn: map[string]bool{}}
}

func (l *fakeLocal) deps() Deps {
	return Deps{
		Eligible: func(_ context.Context, app, stream string) bool {
			l.eligCalls.Add(1)
			l.mu.Lock()
			defer l.mu.Unlock()
			return l.dvrOn[app+"/"+stream]
		},
		Live: func(k media.StreamKey) *media.Stream {
			l.mu.Lock()
			defer l.mu.Unlock()
			return l.live[k]
		},
		LiveKeys: func() []media.StreamKey {
			l.mu.Lock()
			defer l.mu.Unlock()
			var out []media.StreamKey
			for k := range l.live {
				out = append(out, k)
			}
			return out
		},
		Timelines: func(context.Context) (map[media.StreamKey]dvr.Timeline, error) {
			l.mu.Lock()
			defer l.mu.Unlock()
			out := map[media.StreamKey]dvr.Timeline{}
			for k, v := range l.tl {
				out[k] = v
			}
			return out, nil
		},
	}
}

func (l *fakeLocal) publish(app, stream string, since time.Time) {
	k := media.StreamKey{App: app, Name: stream}
	l.mu.Lock()
	l.live[k] = &media.Stream{Key: k, StartedAt: since}
	l.mu.Unlock()
}

func (l *fakeLocal) unpublish(app, stream string) {
	l.mu.Lock()
	delete(l.live, media.StreamKey{App: app, Name: stream})
	l.mu.Unlock()
}

var cam = media.StreamKey{App: "live", Name: "cam"}
var plain = media.StreamKey{App: "live", Name: "plain"}

// Cases 1 and 19: a stream without nDVR is bypassed after one cached
// lookup, never reaches cluster state, and isn't reported to peers.
func TestNonDVRStreamBypassed(t *testing.T) {
	local := newFakeLocal()
	local.dvrOn["live/cam"] = true
	local.publish("live", "plain", time.Now())
	local.publish("live", "cam", time.Now())
	cl := &fakeCluster{}
	tr := New(cl, local.deps(), "")

	for i := 0; i < 100; i++ {
		if d := tr.Check(context.Background(), "live", "plain"); d.Verdict != Bypass {
			t.Fatalf("non-nDVR stream: %v, want bypass", d.Verdict)
		}
	}
	if n := cl.views.Load(); n != 0 {
		t.Errorf("non-nDVR checks read cluster state %d times, want 0", n)
	}
	if n := local.eligCalls.Load(); n != 1 {
		t.Errorf("eligibility resolved %d times for 100 requests, want 1 (cached)", n)
	}

	rep := tr.Report()
	if len(rep) != 1 || rep[0].Stream != "cam" || !rep[0].Publishing {
		t.Errorf("Report = %+v, want only live/cam", rep)
	}

	if d := tr.Check(context.Background(), "live", "cam"); d.Verdict != Owner {
		t.Errorf("nDVR stream published here: %v, want owner", d.Verdict)
	}
	if cl.views.Load() != 1 {
		t.Errorf("nDVR check should read cluster state once")
	}
}

func TestNilTrackerBypasses(t *testing.T) {
	var tr *Tracker
	if d := tr.Check(context.Background(), "live", "cam"); d.Verdict != Bypass {
		t.Errorf("nil tracker: %v", d.Verdict)
	}
	tr.InvalidateEligibility() // must not panic
}

func TestInvalidateEligibility(t *testing.T) {
	local := newFakeLocal()
	tr := New(&fakeCluster{}, local.deps(), "")
	if tr.Eligible(context.Background(), cam) {
		t.Fatal("cam should start ineligible")
	}
	local.mu.Lock()
	local.dvrOn["live/cam"] = true
	local.mu.Unlock()
	if tr.Eligible(context.Background(), cam) {
		t.Fatal("cached answer should still be used before invalidation")
	}
	tr.InvalidateEligibility()
	if !tr.Eligible(context.Background(), cam) {
		t.Error("after invalidation nDVR should be seen as enabled")
	}
}

// Timeline bookkeeping comes from recorder events, not DB polling; and
// the archive-range policy uses it.
func TestTimelineFromEvents(t *testing.T) {
	local := newFakeLocal()
	local.dvrOn["live/cam"] = true
	tr := New(&fakeCluster{}, local.deps(), "")

	if rep := tr.Report(); len(rep) != 0 {
		t.Fatalf("nothing recorded or live, Report = %+v", rep)
	}
	tr.SegmentIndexed(cam, t1000, t1000+4000)
	tr.SegmentIndexed(cam, t1000+4000, t1000+8000)
	tr.SegmentIndexed(cam, t1000+8000, t1000+8000) // empty: ignored

	rep := tr.Report()
	if len(rep) != 1 || rep[0].DVRStartMS != t1000 || rep[0].DVREndMS != t1000+8000 || rep[0].Publishing {
		t.Fatalf("Report = %+v", rep)
	}
	if !tr.CoversRange("live", "cam", t1000+1000, t1000+2000) {
		t.Error("range inside the recording should be covered")
	}
	if tr.CoversRange("live", "cam", t1000+9000, t1000+20000) {
		t.Error("range after the recording should not be covered")
	}

	// A prune re-reads the index (here: empty) and drops the timeline.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tr.Run(ctx)
	tr.Pruned()
	deadline := time.Now().Add(2 * time.Second)
	for len(tr.Report()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("pruned timeline still reported")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ─── two real nodes over NATS ─────────────────────────────────────────────────

type testNode struct {
	local  *fakeLocal
	mgr    *node.Manager
	owner  *Tracker
	cancel context.CancelFunc
}

func startNode(t *testing.T, natsURL, name string) *testNode {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.NodeName = name
	cfg.Multinode.NATSUrl = natsURL
	cfg.Multinode.HeartbeatIntervalS = 1
	cfg.Multinode.PeerForgetS = 30
	cfg.Multinode.NodePriority = 100
	local := newFakeLocal()
	local.dvrOn["live/cam"] = true
	mgr := node.NewManager(cfg, media.NewRegistry(), metrics.New(), "http://"+name+":8080")
	tr := New(mgr, local.deps(), PolicyFirstPublisher)
	mgr.SetDVRReporter(tr.Report)
	ctx, cancel := context.WithCancel(context.Background())
	go mgr.Run(ctx)
	go tr.Run(ctx)
	n := &testNode{local: local, mgr: mgr, owner: tr, cancel: cancel}
	t.Cleanup(cancel)
	return n
}

// waitVerdict waits for n's verdict on live/cam to become want, naming
// owner ("" = don't care).
func waitVerdict(t *testing.T, n *testNode, want Verdict, owner, what string) Decision {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for {
		d := n.owner.Check(context.Background(), "live", "cam")
		if d.Verdict == want && (owner == "" || d.Owner == owner) {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: verdict %v owner %q (%s), want %v %q", what, d.Verdict, d.Owner, d.Reason, want, owner)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestTwoNodes runs two real node managers against a real NATS server and
// walks cases 2, 3, 6, 7, 11 and a clean shutdown, plus case 19 on the
// wire: heartbeats never mention the stream without nDVR.
func TestTwoNodes(t *testing.T) {
	srv := natstest.RunRandClientPortServer()
	defer srv.Shutdown()

	// Watch the wire for case 19.
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	var leaked atomic.Bool
	var sawDVR atomic.Bool
	nc.Subscribe("mediaserver.nodes.*.heartbeat", func(m *nats.Msg) {
		var hb node.Heartbeat
		json.Unmarshal(m.Data, &hb)
		for _, st := range hb.DVR {
			if st.Stream == "plain" {
				leaked.Store(true)
			}
			if st.Stream == "cam" {
				sawDVR.Store(true)
			}
		}
	})

	a := startNode(t, srv.ClientURL(), "origin-a")
	b := startNode(t, srv.ClientURL(), "origin-b")
	a.local.publish("live", "plain", time.Now()) // no nDVR

	// Before the views are ready and with nothing anywhere: 404 on both.
	waitVerdict(t, a, NotOwner, "", "nothing published")

	// Case 2: encoder on A; B has an older recording.
	b.local.mu.Lock()
	b.local.tl[cam] = dvr.Timeline{StartMS: t1000 - 3600_000, EndMS: t1000, Segments: 900}
	b.local.mu.Unlock()
	b.owner.Pruned() // reload B's timelines
	a.local.publish("live", "cam", time.Now())
	a.owner.RecordingStarted(cam)
	waitVerdict(t, a, Owner, "origin-a", "A live")
	waitVerdict(t, b, NotOwner, "origin-a", "B with stale DVR while A live")

	// Case 11 → 3: the encoder moves to B. A's recording ends at t1030,
	// B starts publishing (and recording) after.
	a.local.unpublish("live", "cam")
	a.owner.SegmentIndexed(cam, t1030-4000, t1030)
	a.owner.RecordingStopped(cam)
	b.local.publish("live", "cam", time.Now())
	b.owner.RecordingStarted(cam)
	waitVerdict(t, b, Owner, "origin-b", "B live")
	waitVerdict(t, a, NotOwner, "origin-b", "A after encoder moved")

	// Case 7: encoder gone everywhere, B recorded last.
	b.local.unpublish("live", "cam")
	b.owner.SegmentIndexed(cam, t1045-4000, t1045)
	b.owner.RecordingStopped(cam)
	waitVerdict(t, b, Owner, "origin-b", "offline, B latest")
	waitVerdict(t, a, NotOwner, "origin-b", "offline, A older")

	// Case 6: A turns out to have the later recording.
	a.owner.SegmentIndexed(cam, t1045, t1045+4000)
	a.mgr.Kick()
	waitVerdict(t, a, Owner, "origin-a", "offline, A latest")
	waitVerdict(t, b, NotOwner, "origin-a", "offline, B older")

	// Clean shutdown of A: B takes over at once, no 503 window.
	a.cancel()
	waitVerdict(t, b, Owner, "origin-b", "A left cleanly")

	if leaked.Load() {
		t.Error("a heartbeat listed live/plain, which has no nDVR")
	}
	if !sawDVR.Load() {
		t.Error("no heartbeat carried live/cam's DVR state")
	}
}
