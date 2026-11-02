package scte35ctrl

import (
	"context"
	"sync"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// ptsWaiter is one armed "fire when the stream's tracked PTS reaches
// target" callback (used for both next_keyframe — implicitly, via the
// keyframe waiter list below — and start_pts triggers).
type ptsWaiter struct {
	targetPTS int64
	fn        func(pts int64)
}

// streamWatcher tracks one live media.Stream's real media PTS clock by
// observing video packets on its bus — never wall-clock — so insertion
// timing can be calculated in the stream's own MPEG-TS PTS domain, per the
// task's explicit requirement. One instance per live *media.Stream,
// identified by pointer (not just StreamKey) so a reconnect — which
// registers a brand new *media.Stream/Bus at the same key — never reuses a
// watcher tied to the old, now-closed bus (see Manager.ensureWatcher).
type streamWatcher struct {
	stream *media.Stream

	mu              sync.Mutex
	lastVideoPTS    int64
	hasVideo        bool
	firstVideoCh    chan struct{} // closed exactly once, the first time hasVideo becomes true
	keyframeWaiters []func(pts int64)
	ptsWaiters      []ptsWaiter
}

func newStreamWatcher(stream *media.Stream) *streamWatcher {
	return &streamWatcher{stream: stream, firstVideoCh: make(chan struct{})}
}

// run subscribes to the stream's bus and blocks until it closes (stream
// ends) or ctx is cancelled (server shutdown) — mirrors every other bus
// consumer's `for pkt := range sub.Chan()` shape in this codebase (e.g.
// hls.Packager.Run).
func (w *streamWatcher) run(ctx context.Context) {
	sub := w.stream.Bus.Subscribe(ctx)
	defer sub.Close()

	for pkt := range sub.Chan() {
		if !pkt.Codec.IsVideo() {
			continue
		}

		w.mu.Lock()
		w.lastVideoPTS = pkt.PTS
		firstVideo := !w.hasVideo
		w.hasVideo = true

		var toFire []func(pts int64)
		if pkt.IsKeyframe && len(w.keyframeWaiters) > 0 {
			toFire = append(toFire, w.keyframeWaiters...)
			w.keyframeWaiters = nil
		}
		if len(w.ptsWaiters) > 0 {
			remaining := w.ptsWaiters[:0]
			for _, pw := range w.ptsWaiters {
				if pkt.PTS >= pw.targetPTS {
					toFire = append(toFire, pw.fn)
				} else {
					remaining = append(remaining, pw)
				}
			}
			w.ptsWaiters = remaining
		}
		pts := pkt.PTS
		w.mu.Unlock()

		if firstVideo {
			close(w.firstVideoCh)
		}

		for _, fn := range toFire {
			fn(pts)
		}
	}
}

// currentPTS returns the most recently observed video PTS, or (0, false) if
// no video has been seen yet on this stream.
func (w *streamWatcher) currentPTS() (int64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastVideoPTS, w.hasVideo
}

// waitForFirstVideo blocks until this watcher has observed at least one
// video packet, ctx is done, or timeout elapses — whichever comes first.
// Exists because a streamWatcher's clock only starts once Manager.
// ensureWatcher subscribes it to the bus, not retroactively: the very
// first InsertOut(..., TriggerImmediate) call for a given live stream
// always creates a brand new watcher, and checking currentPTS() right away
// (the old behavior) raced the subscription against the next packet and
// lost almost every time — failing with ErrNoVideoYet even for a stream
// that had been live for hours. A live stream's next video packet is
// normally milliseconds away, so this bounded wait turns that "always
// fails on the first try" case into "just works," while a genuinely
// stalled/dead stream (no packets at all within timeout) still correctly
// reports ErrNoVideoYet.
func (w *streamWatcher) waitForFirstVideo(ctx context.Context, timeout time.Duration) (int64, bool) {
	if pts, ok := w.currentPTS(); ok {
		return pts, true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-w.firstVideoCh:
		return w.currentPTS()
	case <-ctx.Done():
		return 0, false
	case <-timer.C:
		return 0, false
	}
}

// onNextKeyframe arms fn to run the moment the next video keyframe is
// observed — used for start:"next_keyframe".
func (w *streamWatcher) onNextKeyframe(fn func(pts int64)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.keyframeWaiters = append(w.keyframeWaiters, fn)
}

// onPTSReached arms fn to run once the stream's tracked PTS reaches target,
// firing immediately (synchronously) if that's already true — used for
// start_pts.
func (w *streamWatcher) onPTSReached(target int64, fn func(pts int64)) {
	w.mu.Lock()
	if w.hasVideo && w.lastVideoPTS >= target {
		pts := w.lastVideoPTS
		w.mu.Unlock()
		fn(pts)
		return
	}
	w.ptsWaiters = append(w.ptsWaiters, ptsWaiter{targetPTS: target, fn: fn})
	w.mu.Unlock()
}
