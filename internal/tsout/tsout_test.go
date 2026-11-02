package tsout

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/mux"
	"github.com/5centscdn/origin-mediaserver/internal/scte35"
)

// syncRecorder is httptest.ResponseRecorder's shape, but with a mutex
// around the body buffer — Serve writes from its own goroutine while these
// tests poll the accumulated bytes from the test goroutine, and
// httptest.ResponseRecorder's plain *bytes.Buffer isn't safe for that
// (confirmed by -race: a real, but test-harness-only, data race — nothing
// in Serve itself touches its writer from more than one goroutine).
type syncRecorder struct {
	mu     sync.Mutex
	header http.Header
	body   []byte
	code   int
}

func newSyncRecorder() *syncRecorder {
	return &syncRecorder{header: make(http.Header), code: http.StatusOK}
}

func (r *syncRecorder) Header() http.Header { return r.header }

func (r *syncRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.body = append(r.body, p...)
	return len(p), nil
}

func (r *syncRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.code = code
}

func (r *syncRecorder) Flush() {}

func (r *syncRecorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.body)
}

func (r *syncRecorder) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, len(r.body))
	copy(out, r.body)
	return out
}

func newTestStream() *media.Stream {
	s := &media.Stream{Key: media.StreamKey{App: "app1", Name: "s1"}, Bus: media.NewBus()}
	s.Bus.SetCodecInfo(&media.CodecInfo{
		VideoCodec: media.CodecH264, AudioCodec: media.CodecAAC,
		SPSNALu: []byte{0x67, 0x42, 0xC0, 0x1E}, PPSNALu: []byte{0x68, 0xCE},
		AACConfig: []byte{0x12, 0x10},
	})
	return s
}

func annexB(nal ...byte) []byte {
	return append([]byte{0x00, 0x00, 0x00, 0x01}, nal...)
}

// findPID reports whether any 188-byte TS packet in data carries pid.
func findPID(data []byte, pid uint16) bool {
	for off := 0; off+188 <= len(data); off += 188 {
		pkt := data[off : off+188]
		if (uint16(pkt[1]&0x1F)<<8)|uint16(pkt[2]) == pid {
			return true
		}
	}
	return false
}

func scte35Packet(source scte35.Source) *media.Packet {
	section := []byte{0xFC, 0x30, 0x00, 0x01, 0xAA, 0xBB}
	return &media.Packet{
		Codec: media.CodecSCTE35, Data: section,
		SCTE35: &scte35.Event{CommandType: scte35.CommandSpliceInsert, RawSection: section, Source: source},
	}
}

// runServe starts Serve in the background against a fresh recorder, waits
// briefly for it to subscribe, publishes a real keyframe + audio frame (so
// every test exercises the same "first real video before anything is
// written" gate), then hands back the recorder plus a stop func that
// cancels and waits for Serve to actually return.
func runServe(t *testing.T, stream *media.Stream, scte35Mode string) (*syncRecorder, func()) {
	t.Helper()
	rec := newSyncRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, rec, stream, scte35Mode) }()
	time.Sleep(20 * time.Millisecond) // let Serve subscribe before publishing

	stream.Bus.Publish(&media.Packet{
		Type: media.PacketMedia, Codec: media.CodecH264, DTS: 0, PTS: 0,
		IsKeyframe: true, Data: annexB(0x65, 0x01, 0xAA),
	})
	stream.Bus.Publish(&media.Packet{
		Type: media.PacketMedia, Codec: media.CodecAAC, DTS: 0, PTS: 0,
		Data: []byte{0x01, 0x02, 0x03},
	})

	return rec, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Serve did not return after ctx cancellation")
		}
	}
}

func waitForBytes(t *testing.T, rec *syncRecorder, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for rec.Len() < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d bytes, got %d", n, rec.Len())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestServe_WritesValidTSWithVideoAndAudio(t *testing.T) {
	stream := newTestStream()
	rec, stop := runServe(t, stream, "disabled")
	waitForBytes(t, rec, 188*2)
	stop()

	body := rec.Bytes()
	if len(body)%188 != 0 {
		t.Errorf("body length %d is not a multiple of 188 (TS packet size)", len(body))
	}
	if body[0] != 0x47 {
		t.Errorf("first byte = 0x%02X, want sync byte 0x47", body[0])
	}
	if !findPID(body, mux.PIDVideo) {
		t.Error("no video PID (0x0100) packet found in output")
	}
	if rec.Header().Get("Content-Type") != "video/MP2T" {
		t.Errorf("Content-Type = %q, want video/MP2T", rec.Header().Get("Content-Type"))
	}
}

func TestServe_PassthroughForwardsIngestSCTE35(t *testing.T) {
	stream := newTestStream()
	rec, stop := runServe(t, stream, "passthrough")
	waitForBytes(t, rec, 188*2)

	stream.Bus.Publish(scte35Packet(scte35.SourceIngest))
	waitForBytes(t, rec, 188*3)
	stop()

	if !findPID(rec.Bytes(), mux.PIDSCTE35) {
		t.Error("passthrough mode: expected an SCTE-35 PID (0x0102) packet, found none")
	}
}

func TestServe_PassthroughIgnoresInsertedSource(t *testing.T) {
	stream := newTestStream()
	rec, stop := runServe(t, stream, "passthrough")
	waitForBytes(t, rec, 188*2)

	// SourceInserted must NOT be forwarded in pure "passthrough" mode (only
	// "insert"/"hybrid" allow it) — same scte35.SourceAllowed semantics
	// internal/hls.Packager uses.
	stream.Bus.Publish(scte35Packet(scte35.SourceInserted))
	time.Sleep(100 * time.Millisecond)
	stop()

	if findPID(rec.Bytes(), mux.PIDSCTE35) {
		t.Error("passthrough mode forwarded a SourceInserted SCTE-35 packet, want it dropped")
	}
}

func TestServe_DisabledModeNeverWritesSCTE35PID(t *testing.T) {
	stream := newTestStream()
	rec, stop := runServe(t, stream, "disabled")
	waitForBytes(t, rec, 188*2)

	stream.Bus.Publish(scte35Packet(scte35.SourceIngest))
	time.Sleep(100 * time.Millisecond)
	stop()

	if findPID(rec.Bytes(), mux.PIDSCTE35) {
		t.Error("scte35_mode=disabled: expected no SCTE-35 PID at all, found one")
	}
}

func TestServe_NoVideoBeforeFirstKeyframe(t *testing.T) {
	stream := newTestStream()
	rec := newSyncRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, rec, stream, "disabled") }()
	time.Sleep(20 * time.Millisecond)

	// A non-keyframe video packet before any keyframe must be dropped, not
	// written mid-GOP where a fresh connection could never decode it.
	stream.Bus.Publish(&media.Packet{
		Type: media.PacketMedia, Codec: media.CodecH264, DTS: 0, PTS: 0,
		IsKeyframe: false, Data: annexB(0x41, 0x01, 0xBB),
	})
	time.Sleep(100 * time.Millisecond)
	if rec.Len() != 0 {
		t.Errorf("wrote %d bytes before any keyframe arrived, want 0", rec.Len())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after ctx cancellation")
	}
}

func TestServe_ReturnsWhenContextCancelled(t *testing.T) {
	stream := newTestStream()
	rec := newSyncRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, rec, stream, "disabled") }()
	time.Sleep(20 * time.Millisecond)

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Serve returned nil error after ctx cancellation, want context.Canceled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return promptly after ctx cancellation")
	}
}
