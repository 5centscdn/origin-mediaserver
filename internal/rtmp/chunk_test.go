package rtmp

import (
	"bytes"
	"testing"
)

// TestChunkType3Timestamps covers compressed headers on one chunk stream:
// Type 0 (absolute 1000), Type 2 (delta 40), then two Type-3 messages,
// which must each advance by the last delta. A Type-3 continuation chunk
// of a multi-chunk message must not advance it again, and a Type-3 message
// straight after a Type 0 keeps the Type 0's timestamp.
func TestChunkType3Timestamps(t *testing.T) {
	const csid = 6
	var b bytes.Buffer
	payload := func(n int) []byte { return bytes.Repeat([]byte{0xAB}, n) }

	// Type 0: ts=1000, len=10, type 9 (video), msid=1.
	b.Write([]byte{0<<6 | csid, 0x00, 0x03, 0xE8, 0x00, 0x00, 0x0A, 0x09, 0x01, 0x00, 0x00, 0x00})
	b.Write(payload(10))
	// Type 2: delta=40.
	b.Write([]byte{2<<6 | csid, 0x00, 0x00, 0x28})
	b.Write(payload(10))
	// Type 3: new message, expect 1080.
	b.Write([]byte{3<<6 | csid})
	b.Write(payload(10))
	// Type 1: delta=40, len=200 (two chunks at chunk size 128), expect 1120.
	b.Write([]byte{1<<6 | csid, 0x00, 0x00, 0x28, 0x00, 0x00, 0xC8, 0x09})
	b.Write(payload(128))
	b.Write([]byte{3<<6 | csid}) // continuation: no advance
	b.Write(payload(72))
	// Type 3: new message of 200 bytes, expect 1160.
	b.Write([]byte{3<<6 | csid})
	b.Write(payload(128))
	b.Write([]byte{3<<6 | csid})
	b.Write(payload(72))
	// Type 0: ts=5000, len=10, then Type 3: stays 5000.
	b.Write([]byte{0<<6 | csid, 0x00, 0x13, 0x88, 0x00, 0x00, 0x0A, 0x09, 0x01, 0x00, 0x00, 0x00})
	b.Write(payload(10))
	b.Write([]byte{3<<6 | csid})
	b.Write(payload(10))

	d := newChunkDemuxer(&b)
	want := []struct {
		ts  uint32
		len int
	}{{1000, 10}, {1040, 10}, {1080, 10}, {1120, 200}, {1160, 200}, {5000, 10}, {5000, 10}}
	for i, w := range want {
		msg, err := d.ReadMessage()
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if msg.Timestamp != w.ts || len(msg.Data) != w.len {
			t.Errorf("message %d: ts=%d len=%d, want ts=%d len=%d", i, msg.Timestamp, len(msg.Data), w.ts, w.len)
		}
	}
}
