package media

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Bus is an in-process pub/sub channel for a single logical stream.
// Multiple goroutines (packagers, DVR, WebRTC, MPEG-TS output) subscribe
// and receive every packet published by the ingest handler.
//
// Design notes:
//   - Non-blocking publish: if a subscriber's buffer is full, it drops the
//     packet rather than back-pressuring the ingest goroutine.
//   - Subscribers are removed automatically when their context is cancelled.
//   - This replaces Redis pub/sub for single-node deployments.
type Bus struct {
	mu   sync.RWMutex
	subs map[uint64]*Subscription

	nextID  atomic.Uint64
	closed  atomic.Bool

	// bytesIn is the running count of raw ingest bytes received on this
	// bus. It lives here rather than on Stream because every ingest
	// path — direct-publish (SRT/RTMP servers) and outbound-pull (rtmp.Pull,
	// rtsp.Pull, mpegts.PullHTTP/ListenUDP, srt.Pull) alike — already holds
	// a *Bus, mirroring why CodecInfo lives here too.
	bytesIn atomic.Int64

	// rate* back IngestBitrate: a sample of bytesIn, and the rate measured
	// between the last two samples (at least rateWindow apart).
	rateMu    sync.Mutex
	rateBytes int64
	rateAt    time.Time
	rateBps   int64

	// lastMedia is when the last audio/video packet was published (unix
	// nanoseconds, 0 = never). The pull manager's fallback watchdog uses it
	// to tell a live upstream from one that connected but sends nothing.
	lastMedia atomic.Int64

	// Codec information extracted from the sequence header. Guarded by mu
	// (last-write-wins) rather than a sync.Once — failover can rebind a
	// stream's underlying source to a backup with different codec params,
	// so this must be updatable, not fixed on first write.
	codecInfo *CodecInfo
}

// Subscription is a single subscriber's receive channel.
type Subscription struct {
	id  uint64
	ch  chan *Packet
	bus *Bus
}

// Chan returns the channel on which packets arrive.
func (s *Subscription) Chan() <-chan *Packet { return s.ch }

// Close removes this subscription from the bus.
func (s *Subscription) Close() {
	s.bus.unsubscribe(s.id)
}

const subscriberBufSize = 512

// NewBus creates a new Bus.
func NewBus() *Bus {
	return &Bus{
		subs: make(map[uint64]*Subscription),
	}
}

// Subscribe returns a Subscription that receives all future packets.
// The caller must call sub.Close() or cancel the context when done.
func (b *Bus) Subscribe(ctx context.Context) *Subscription {
	id := b.nextID.Add(1)
	sub := &Subscription{
		id:  id,
		ch:  make(chan *Packet, subscriberBufSize),
		bus: b,
	}

	b.mu.Lock()
	b.subs[id] = sub
	b.mu.Unlock()

	// Auto-remove on context cancellation.
	go func() {
		<-ctx.Done()
		sub.Close()
	}()

	return sub
}

// Publish sends pkt to all active subscribers.
// It never blocks; slow subscribers are dropped.
func (b *Bus) Publish(pkt *Packet) {
	if b.closed.Load() {
		return
	}
	if pkt.Codec.IsVideo() || pkt.Codec.IsAudio() {
		b.lastMedia.Store(time.Now().UnixNano())
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, sub := range b.subs {
		select {
		case sub.ch <- pkt:
		default:
			// Subscriber is too slow; drop this packet for it.
		}
	}
}

// LastMediaAt returns when the last audio/video packet was published, or
// the zero time if none has been.
func (b *Bus) LastMediaAt() time.Time {
	if n := b.lastMedia.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

// AddBytesIn atomically adds n to the running ingest byte counter.
func (b *Bus) AddBytesIn(n int64) {
	b.bytesIn.Add(n)
}

// rateWindow is the shortest span IngestBitrate measures over: short
// enough to follow the source, long enough to smooth GOP-sized bursts.
const rateWindow = 5 * time.Second

// IngestBitrate returns the recent ingest rate in bits per second. It's
// measured lazily: each call at least rateWindow after the previous sample
// takes a new one, so the value follows the source rather than averaging
// over the stream's whole life. 0 until two samples exist.
func (b *Bus) IngestBitrate() int64 {
	b.rateMu.Lock()
	defer b.rateMu.Unlock()
	now, n := time.Now(), b.bytesIn.Load()
	if b.rateAt.IsZero() {
		b.rateBytes, b.rateAt = n, now
		return 0
	}
	if el := now.Sub(b.rateAt); el >= rateWindow {
		b.rateBps = int64(float64(n-b.rateBytes) * 8 / el.Seconds())
		b.rateBytes, b.rateAt = n, now
	}
	return b.rateBps
}

// BytesIn atomically reads the running ingest byte counter.
func (b *Bus) BytesIn() int64 {
	return b.bytesIn.Load()
}

// SetCodecInfo replaces the cached codec info. Last write wins — callers
// that merge fields (e.g. adding audio config to video-derived info) must
// pass a fully-formed new *CodecInfo, not mutate the one CodecInfo() returned.
func (b *Bus) SetCodecInfo(ci *CodecInfo) {
	b.mu.Lock()
	b.codecInfo = ci
	b.mu.Unlock()
}

// CodecInfo returns the cached codec info, or nil if not yet received.
func (b *Bus) CodecInfo() *CodecInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.codecInfo
}

// Close drains and removes all subscribers, then marks the bus as closed.
// Any future Publish calls are no-ops.
func (b *Bus) Close() {
	b.closed.Store(true)

	b.mu.Lock()
	defer b.mu.Unlock()

	for id, sub := range b.subs {
		close(sub.ch)
		delete(b.subs, id)
	}
}

// SubscriberCount returns the current number of active subscribers.
func (b *Bus) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

func (b *Bus) unsubscribe(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if sub, ok := b.subs[id]; ok {
		close(sub.ch)
		delete(b.subs, id)
	}
}
