package scte35ctrl

import (
	"context"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
)

// newTestManager builds a Manager against a real temp SQLite DB (the same
// db.Open(ctx, t.TempDir()) pattern internal/srt/server_test.go uses) and a
// real media.Registry — no mocking, since this package only ever touches
// real *media.Bus/*db.DB.
func newTestManager(t *testing.T) (*Manager, *media.Registry, context.Context) {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	registry := media.NewRegistry()
	mgr := NewManager(ctx, database, registry, metrics.New())
	return mgr, registry, ctx
}

func registerTestStream(t *testing.T, registry *media.Registry, app, name string) *media.Stream {
	t.Helper()
	s := &media.Stream{Key: media.StreamKey{App: app, Name: name}, Bus: media.NewBus()}
	if err := registry.Register(s); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return s
}

// setAppSCTE35Mode inserts a minimal applications row with the given
// scte35_mode — everything else falls back to its schema default.
func setAppSCTE35Mode(t *testing.T, mgr *Manager, ctx context.Context, app, mode string) {
	t.Helper()
	if _, err := mgr.database.ExecContext(ctx,
		`INSERT INTO applications (id, name, scte35_mode) VALUES (?, ?, ?)`, app, app, mode,
	); err != nil {
		t.Fatalf("insert application: %v", err)
	}
}

func videoPkt(pts int64, keyframe bool) *media.Packet {
	return &media.Packet{Type: media.PacketMedia, Codec: media.CodecH264, PTS: pts, DTS: pts, IsKeyframe: keyframe}
}

// waitVideoObserved publishes pkt repeatedly until w.currentPTS() reflects
// it — needed because ensureWatcher's Subscribe happens in a just-spawned
// goroutine, and media.Bus.Publish drops packets with no subscriber
// registered yet (a real, documented characteristic of Bus, not a bug);
// retrying the publish avoids a flaky fixed sleep.
func waitVideoObserved(t *testing.T, stream *media.Stream, w *streamWatcher, pts int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stream.Bus.Publish(videoPkt(pts, true))
		if p, ok := w.currentPTS(); ok && p == pts {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("watcher never observed the video packet")
}

func waitForSCTE35Packet(t *testing.T, sub *media.Subscription) *media.Packet {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case pkt := <-sub.Chan():
			if pkt.Codec == media.CodecSCTE35 {
				return pkt
			}
		case <-deadline:
			t.Fatal("timed out waiting for a CodecSCTE35 packet")
			return nil
		}
	}
}

// expectNoSCTE35Packet asserts no CodecSCTE35 packet arrives within a short
// window — used to confirm a cancelled event never fires.
func expectNoSCTE35Packet(t *testing.T, sub *media.Subscription) {
	t.Helper()
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case pkt := <-sub.Chan():
			if pkt.Codec == media.CodecSCTE35 {
				t.Fatalf("received an unexpected CodecSCTE35 packet: event_id=%d", pkt.SCTE35.EventID)
			}
		case <-deadline:
			return
		}
	}
}

// TestInsertOut_Immediate covers acceptance scenario 1: a source with no
// SCTE-35 gets a server-generated OUT, timed to the stream's own tracked
// PTS (never wall-clock).
func TestInsertOut_Immediate(t *testing.T) {
	mgr, registry, ctx := newTestManager(t)
	stream := registerTestStream(t, registry, "app1", "s1")
	setAppSCTE35Mode(t, mgr, ctx, "app1", "hybrid")

	w := mgr.ensureWatcher(stream)
	waitVideoObserved(t, stream, w, 1000)

	sub := stream.Bus.Subscribe(ctx)
	defer sub.Close()

	ev, err := mgr.InsertOut(ctx, "app1", "s1", OutRequest{
		EventID: 1, Command: "splice_insert", DurationMS: 30000, AutoReturn: true, Trigger: TriggerImmediate,
	})
	if err != nil {
		t.Fatalf("InsertOut: %v", err)
	}
	if ev.Status != "active" {
		t.Errorf("status = %q, want active", ev.Status)
	}

	pkt := waitForSCTE35Packet(t, sub)
	if !pkt.SCTE35.OutOfNetwork {
		t.Error("OutOfNetwork = false, want true")
	}
	if pkt.PTS != 1000 {
		t.Errorf("PTS = %d, want 1000 (the stream's own tracked PTS, not wall-clock)", pkt.PTS)
	}
}

// TestInsertOut_ExplicitIn covers acceptance scenario 2: a manual IN
// produces a real splice_insert with out_of_network=false.
func TestInsertOut_ExplicitIn(t *testing.T) {
	mgr, registry, ctx := newTestManager(t)
	stream := registerTestStream(t, registry, "app1", "s2")
	setAppSCTE35Mode(t, mgr, ctx, "app1", "insert")

	w := mgr.ensureWatcher(stream)
	waitVideoObserved(t, stream, w, 5000)

	sub := stream.Bus.Subscribe(ctx)
	defer sub.Close()

	if _, err := mgr.InsertOut(ctx, "app1", "s2", OutRequest{
		EventID: 2, Command: "splice_insert", Trigger: TriggerImmediate,
	}); err != nil {
		t.Fatalf("InsertOut: %v", err)
	}
	waitForSCTE35Packet(t, sub) // drain the OUT

	ev, err := mgr.InsertIn(ctx, "app1", "s2", 2)
	if err != nil {
		t.Fatalf("InsertIn: %v", err)
	}
	if ev.Status != "completed" {
		t.Errorf("status = %q, want completed", ev.Status)
	}

	pkt := waitForSCTE35Packet(t, sub)
	if pkt.SCTE35.OutOfNetwork {
		t.Error("OutOfNetwork = true, want false (IN)")
	}
	if pkt.SCTE35.EventID != 2 {
		t.Errorf("EventID = %d, want 2", pkt.SCTE35.EventID)
	}
}

// TestInsertOut_DurationAutoReturn covers acceptance scenario 3: an OUT
// with a declared duration automatically fires a real IN after that
// duration elapses, with no further API call.
func TestInsertOut_DurationAutoReturn(t *testing.T) {
	mgr, registry, ctx := newTestManager(t)
	stream := registerTestStream(t, registry, "app1", "s3")
	setAppSCTE35Mode(t, mgr, ctx, "app1", "insert")

	w := mgr.ensureWatcher(stream)
	waitVideoObserved(t, stream, w, 9000)

	sub := stream.Bus.Subscribe(ctx)
	defer sub.Close()

	// A short duration (well under a second) keeps the test fast — the
	// mechanism (time.AfterFunc) doesn't care about the unit's magnitude.
	if _, err := mgr.InsertOut(ctx, "app1", "s3", OutRequest{
		EventID: 3, Command: "splice_insert", DurationMS: 50, AutoReturn: true, Trigger: TriggerImmediate,
	}); err != nil {
		t.Fatalf("InsertOut: %v", err)
	}
	outPkt := waitForSCTE35Packet(t, sub)
	if !outPkt.SCTE35.OutOfNetwork {
		t.Fatal("first packet should be the OUT")
	}

	inPkt := waitForSCTE35Packet(t, sub)
	if inPkt.SCTE35.OutOfNetwork {
		t.Error("auto-return packet has OutOfNetwork=true, want false (IN)")
	}
	if inPkt.SCTE35.EventID != 3 {
		t.Errorf("auto-return EventID = %d, want 3", inPkt.SCTE35.EventID)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		ev, err := mgr.database.GetLatestSCTE35Event(ctx, "app1", "s3", 3)
		if err != nil {
			t.Fatalf("GetLatestSCTE35Event: %v", err)
		}
		if ev.Status == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("event status = %q, want completed", ev.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestInsertOut_DuplicateRejected guards acceptance scenario 7's other
// half: an event_id that's already active can't be started again.
func TestInsertOut_DuplicateRejected(t *testing.T) {
	mgr, registry, ctx := newTestManager(t)
	stream := registerTestStream(t, registry, "app1", "s4")
	setAppSCTE35Mode(t, mgr, ctx, "app1", "insert")

	w := mgr.ensureWatcher(stream)
	waitVideoObserved(t, stream, w, 100)

	if _, err := mgr.InsertOut(ctx, "app1", "s4", OutRequest{
		EventID: 4, Command: "splice_insert", Trigger: TriggerImmediate,
	}); err != nil {
		t.Fatalf("first InsertOut: %v", err)
	}

	_, err := mgr.InsertOut(ctx, "app1", "s4", OutRequest{
		EventID: 4, Command: "splice_insert", Trigger: TriggerImmediate,
	})
	if err != ErrDuplicateEvent {
		t.Fatalf("second InsertOut err = %v, want ErrDuplicateEvent", err)
	}
}

// TestInsertOut_ModeDisallowsInsert confirms "passthrough" (and the
// default "disabled") mode rejects an insertion attempt outright.
func TestInsertOut_ModeDisallowsInsert(t *testing.T) {
	mgr, registry, ctx := newTestManager(t)
	registerTestStream(t, registry, "app1", "s5")
	setAppSCTE35Mode(t, mgr, ctx, "app1", "passthrough")

	_, err := mgr.InsertOut(ctx, "app1", "s5", OutRequest{EventID: 5, Command: "splice_insert", Trigger: TriggerImmediate})
	if err != ErrModeDisallowsInsert {
		t.Fatalf("err = %v, want ErrModeDisallowsInsert", err)
	}
}

// TestInsertOut_ImmediateSucceedsWithoutPreWarmingWatcher is the regression
// test for a real production bug: the very first InsertOut(..., Trigger:
// TriggerImmediate) call for any given live stream always failed with
// ErrNoVideoYet, no matter how long the stream had actually been live —
// because ensureWatcher's Subscribe happens in a just-spawned goroutine
// (see waitVideoObserved's doc comment above, which works around the exact
// same race in this test file's own harness), and the old code checked
// currentPTS() synchronously, microseconds later, with no chance for that
// goroutine to have processed even one packet yet. A real caller (the
// HTTP API) has no way to "pre-warm" the watcher the way this test file's
// other tests do via an explicit mgr.ensureWatcher + waitVideoObserved
// before calling InsertOut — so this test deliberately does NOT do that
// either, simulating exactly what a real first-ever API call sees: a
// stream that's actively publishing video, and InsertOut called without
// any warm-up at all.
func TestInsertOut_ImmediateSucceedsWithoutPreWarmingWatcher(t *testing.T) {
	mgr, registry, ctx := newTestManager(t)
	stream := registerTestStream(t, registry, "app1", "s6")
	setAppSCTE35Mode(t, mgr, ctx, "app1", "hybrid")

	// Simulate a real live stream: keep publishing video packets in the
	// background for the duration of the test, exactly like a real
	// ingest goroutine would, with no coordination with InsertOut at all.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		pts := int64(1000)
		for {
			select {
			case <-stop:
				return
			default:
				stream.Bus.Publish(videoPkt(pts, true))
				pts += 90000 / 30 // ~30fps
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	sub := stream.Bus.Subscribe(ctx)
	defer sub.Close()

	ev, err := mgr.InsertOut(ctx, "app1", "s6", OutRequest{
		EventID: 6, Command: "splice_insert", DurationMS: 30000, AutoReturn: true, Trigger: TriggerImmediate,
	})
	if err != nil {
		t.Fatalf("InsertOut on a fresh, never-pre-warmed watcher = %v, want success", err)
	}
	if ev.Status != "active" {
		t.Errorf("status = %q, want active", ev.Status)
	}
	waitForSCTE35Packet(t, sub)
}

// TestCancel_PreventsPendingNextKeyframeFromFiring confirms a cancelled,
// not-yet-fired next_keyframe event never actually injects anything once
// its keyframe does arrive.
func TestCancel_PreventsPendingNextKeyframeFromFiring(t *testing.T) {
	mgr, registry, ctx := newTestManager(t)
	stream := registerTestStream(t, registry, "app1", "s6")
	setAppSCTE35Mode(t, mgr, ctx, "app1", "insert")

	w := mgr.ensureWatcher(stream)
	waitVideoObserved(t, stream, w, 100) // non-keyframe observation to start the watcher; doesn't fire the arm

	sub := stream.Bus.Subscribe(ctx)
	defer sub.Close()

	ev, err := mgr.InsertOut(ctx, "app1", "s6", OutRequest{
		EventID: 6, Command: "splice_insert", Trigger: TriggerNextKeyframe,
	})
	if err != nil {
		t.Fatalf("InsertOut: %v", err)
	}
	if ev.Status != "pending" {
		t.Fatalf("status = %q, want pending (next_keyframe hasn't fired yet)", ev.Status)
	}

	if err := mgr.Cancel(ctx, "app1", "s6", 6); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	stream.Bus.Publish(videoPkt(2000, true)) // the keyframe the event was waiting for
	expectNoSCTE35Packet(t, sub)
}

// TestCancel_RejectsActiveEvent confirms an already-fired (immediate)
// event can't be cancelled — "cancel a pending event where technically
// possible" implies an active one is not.
func TestCancel_RejectsActiveEvent(t *testing.T) {
	mgr, registry, ctx := newTestManager(t)
	stream := registerTestStream(t, registry, "app1", "s7")
	setAppSCTE35Mode(t, mgr, ctx, "app1", "insert")

	w := mgr.ensureWatcher(stream)
	waitVideoObserved(t, stream, w, 100)

	if _, err := mgr.InsertOut(ctx, "app1", "s7", OutRequest{EventID: 7, Command: "splice_insert", Trigger: TriggerImmediate}); err != nil {
		t.Fatalf("InsertOut: %v", err)
	}
	if err := mgr.Cancel(ctx, "app1", "s7", 7); err != ErrCannotCancel {
		t.Fatalf("Cancel err = %v, want ErrCannotCancel", err)
	}
}

// TestInsertOut_ABRGroupFanOut covers acceptance scenario 6: one API call
// against one rendition injects the same event_id into every sibling
// rendition sharing an ABR group (the RTMP %i-pattern mechanism, via
// Stream.ABRGroup — the DB-backed abr_variants mechanism is exercised only
// via the real abr_groups/abr_variants tables in a full integration
// environment, not re-derived here).
func TestInsertOut_ABRGroupFanOut(t *testing.T) {
	mgr, registry, ctx := newTestManager(t)
	setAppSCTE35Mode(t, mgr, ctx, "app1", "insert")

	s720 := &media.Stream{Key: media.StreamKey{App: "app1", Name: "ch_720"}, Bus: media.NewBus(), ABRGroup: "ch", ABRVariantIndex: 0}
	s1080 := &media.Stream{Key: media.StreamKey{App: "app1", Name: "ch_1080"}, Bus: media.NewBus(), ABRGroup: "ch", ABRVariantIndex: 1}
	if err := registry.Register(s720); err != nil {
		t.Fatalf("register s720: %v", err)
	}
	if err := registry.Register(s1080); err != nil {
		t.Fatalf("register s1080: %v", err)
	}

	w720 := mgr.ensureWatcher(s720)
	w1080 := mgr.ensureWatcher(s1080)
	waitVideoObserved(t, s720, w720, 4000)
	waitVideoObserved(t, s1080, w1080, 4500) // deliberately different PTS — independent encoder clocks

	sub720 := s720.Bus.Subscribe(ctx)
	defer sub720.Close()
	sub1080 := s1080.Bus.Subscribe(ctx)
	defer sub1080.Close()

	// Target the 720p stream specifically; both renditions must still fire.
	if _, err := mgr.InsertOut(ctx, "app1", "ch_720", OutRequest{
		EventID: 8, Command: "splice_insert", DurationMS: 10000, AutoReturn: true, Trigger: TriggerImmediate,
	}); err != nil {
		t.Fatalf("InsertOut: %v", err)
	}

	pkt720 := waitForSCTE35Packet(t, sub720)
	pkt1080 := waitForSCTE35Packet(t, sub1080)

	if pkt720.SCTE35.EventID != 8 || pkt1080.SCTE35.EventID != 8 {
		t.Fatalf("event_id mismatch: 720=%d 1080=%d, want 8 on both", pkt720.SCTE35.EventID, pkt1080.SCTE35.EventID)
	}
	if pkt720.PTS != 4000 {
		t.Errorf("720p PTS = %d, want 4000 (its own tracked clock)", pkt720.PTS)
	}
	if pkt1080.PTS != 4500 {
		t.Errorf("1080p PTS = %d, want 4500 (its own tracked clock, independent of 720p's)", pkt1080.PTS)
	}
}

// TestReconnect_OrphanCancelsPendingEvents covers acceptance scenario 7's
// main case: when a stream's bus closes (disconnect), any still-pending
// event for it is cancelled — so a later reconnect starts clean and never
// replays it.
func TestReconnect_OrphanCancelsPendingEvents(t *testing.T) {
	mgr, registry, ctx := newTestManager(t)
	stream := registerTestStream(t, registry, "app1", "s9")
	setAppSCTE35Mode(t, mgr, ctx, "app1", "insert")

	w := mgr.ensureWatcher(stream)
	waitVideoObserved(t, stream, w, 100)

	if _, err := mgr.InsertOut(ctx, "app1", "s9", OutRequest{
		EventID: 9, Command: "splice_insert", Trigger: TriggerNextKeyframe, // never fires: no keyframe arrives before disconnect
	}); err != nil {
		t.Fatalf("InsertOut: %v", err)
	}

	registry.Unregister(stream) // closes the bus — simulates the connection ending

	deadline := time.Now().Add(2 * time.Second)
	for {
		ev, err := mgr.database.GetLatestSCTE35Event(ctx, "app1", "s9", 9)
		if err != nil {
			t.Fatalf("GetLatestSCTE35Event: %v", err)
		}
		if ev.Status == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %q, want cancelled after stream disconnect", ev.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A reconnect (new *media.Stream/Bus at the same key) must be able to
	// reuse event_id 9 — it's cancelled, not completed.
	stream2 := registerTestStream(t, registry, "app1", "s9")
	w2 := mgr.ensureWatcher(stream2)
	if w2 == w {
		t.Fatal("ensureWatcher reused the stale watcher for the reconnected stream")
	}
	waitVideoObserved(t, stream2, w2, 200)
	if _, err := mgr.InsertOut(ctx, "app1", "s9", OutRequest{EventID: 9, Command: "splice_insert", Trigger: TriggerImmediate}); err != nil {
		t.Fatalf("InsertOut after reconnect: %v", err)
	}
}
