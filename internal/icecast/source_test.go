package icecast

import (
	"context"
	"testing"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// TestSource_MP3RealFixture feeds a real libmp3lame-encoded stream through
// source.feed in small, arbitrarily-sized chunks — simulating how bytes
// actually arrive off a TCP connection (never neatly aligned to frame
// boundaries) — and confirms every frame is published with the correct
// codec, correct CodecInfo, and a strictly monotonically increasing PTS.
func TestSource_MP3RealFixture(t *testing.T) {
	data := decodeFixture(t, fixtureMP3)

	bus := media.NewBus()
	sub := bus.Subscribe(context.Background())
	defer sub.Close()

	src := newSource(bus, media.CodecMP3)

	const chunkSize = 137 // deliberately not aligned to any frame boundary
	for i := 0; i < len(data); i += chunkSize {
		end := min(i+chunkSize, len(data))
		src.feed(data[i:end])
	}

	var pkts []*media.Packet
drain:
	for {
		select {
		case pkt := <-sub.Chan():
			pkts = append(pkts, pkt)
		default:
			break drain
		}
	}

	if len(pkts) != 14 {
		t.Fatalf("got %d packets, want 14 (see TestParseMP3Header_RealLibmp3lameFixture)", len(pkts))
	}
	for i, pkt := range pkts {
		if pkt.Codec != media.CodecMP3 {
			t.Errorf("packet %d: Codec = %v, want CodecMP3", i, pkt.Codec)
		}
		if pkt.WallClock.IsZero() {
			t.Errorf("packet %d: WallClock is zero", i)
		}
		if i > 0 && pkt.PTS <= pkts[i-1].PTS {
			t.Errorf("packet %d: PTS = %d, not strictly greater than packet %d's PTS %d", i, pkt.PTS, i-1, pkts[i-1].PTS)
		}
	}

	ci := bus.CodecInfo()
	if ci == nil || ci.AudioCodec != media.CodecMP3 || ci.SampleRate != 44100 || ci.Channels != 2 {
		t.Errorf("CodecInfo = %+v, want MP3/44100/stereo", ci)
	}
}

// TestSource_AAC exercises the ADTS path with several frames batched into
// one feed call, mirroring how a real read() from a TCP socket could
// deliver many frames at once.
func TestSource_AAC(t *testing.T) {
	frame1 := buildTestADTS([]byte{0x01, 0x02}, 4, 2)
	frame2 := buildTestADTS([]byte{0x03, 0x04, 0x05}, 4, 2)
	frame3 := buildTestADTS([]byte{0x06}, 4, 2)
	payload := append(append(append([]byte{}, frame1...), frame2...), frame3...)

	bus := media.NewBus()
	sub := bus.Subscribe(context.Background())
	defer sub.Close()

	src := newSource(bus, media.CodecAAC)
	src.feed(payload)

	var pkts []*media.Packet
drain:
	for {
		select {
		case pkt := <-sub.Chan():
			pkts = append(pkts, pkt)
		default:
			break drain
		}
	}

	if len(pkts) != 3 {
		t.Fatalf("got %d packets, want 3", len(pkts))
	}
	wantData := [][]byte{{0x01, 0x02}, {0x03, 0x04, 0x05}, {0x06}}
	for i, pkt := range pkts {
		if pkt.Codec != media.CodecAAC {
			t.Errorf("packet %d: Codec = %v, want CodecAAC", i, pkt.Codec)
		}
		if string(pkt.Data) != string(wantData[i]) {
			t.Errorf("packet %d: Data = % x, want % x (ADTS header should be stripped)", i, pkt.Data, wantData[i])
		}
		if i > 0 && pkt.PTS <= pkts[i-1].PTS {
			t.Errorf("packet %d: PTS = %d, not strictly greater than packet %d's PTS %d", i, pkt.PTS, i-1, pkts[i-1].PTS)
		}
	}

	ci := bus.CodecInfo()
	if ci == nil || ci.AudioCodec != media.CodecAAC || len(ci.AACConfig) != 2 {
		t.Errorf("CodecInfo = %+v, want CodecAAC with a 2-byte AACConfig", ci)
	}
}

// TestSource_ResyncsPastGarbage confirms a source can recover from
// leading garbage bytes (e.g. an ID3 tag some encoders prepend, or any
// non-conformant junk) by scanning forward for the next valid sync word
// one byte at a time, rather than getting permanently stuck.
func TestSource_ResyncsPastGarbage(t *testing.T) {
	data := decodeFixture(t, fixtureMP3)
	garbage := []byte("this is not an mp3 frame, just junk bytes before the real stream starts")
	withGarbage := append(garbage, data...)

	bus := media.NewBus()
	sub := bus.Subscribe(context.Background())
	defer sub.Close()

	src := newSource(bus, media.CodecMP3)
	src.feed(withGarbage)

	select {
	case pkt := <-sub.Chan():
		if pkt.Codec != media.CodecMP3 {
			t.Errorf("Codec = %v, want CodecMP3", pkt.Codec)
		}
	default:
		t.Fatal("expected at least one published packet after resyncing past garbage")
	}
}
