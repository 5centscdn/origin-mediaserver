package mpegts

import (
	"context"
	"os"
	"sort"
	"testing"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// testdata/x264_bframes.ts is 4 s of 320x180 x264 (bframes=3,
// b-pyramid=normal, keyint 30): 120 pictures, 83 of them B, made with
//
//	ffmpeg -f lavfi -i testsrc2=size=320x180:rate=30 -t 4 -c:v libx264 \
//	  -preset veryfast -crf 38 \
//	  -x264-params bframes=3:b-pyramid=normal:keyint=30:min-keyint=30:scenecut=0 \
//	  -an -f mpegts x264_bframes.ts
//
// The tests rewrite its video PES headers to look like the encoders that
// broke DVR playback: no DTS at all, and PTS that doesn't follow display
// order (the standalone DVR server's v2.16.6 incident).

// pesTimes is one video PES header's timestamps, in stream order.
type pesTimes struct{ pts, dts int64 }

// rewriteVideoPES walks ts and, for each video PES header, calls fn with the
// original timestamps; fn returns the PTS to write. Any DTS is removed:
// PTS_DTS_flags becomes "PTS only" and the DTS bytes become stuffing (0xFF),
// so the header length is unchanged.
func rewriteVideoPES(t *testing.T, ts []byte, videoPID uint16, fn func(i int, orig pesTimes) int64) (out []byte, orig []pesTimes) {
	t.Helper()
	out = append([]byte(nil), ts...)
	for off := 0; off+188 <= len(out); off += 188 {
		p := out[off : off+188]
		pid := uint16(p[1]&0x1F)<<8 | uint16(p[2])
		if pid != videoPID || p[1]&0x40 == 0 {
			continue
		}
		hdr := 4
		if p[3]&0x20 != 0 {
			hdr += 1 + int(p[4])
		}
		pes := p[hdr:]
		if len(pes) < 14 || pes[0] != 0 || pes[1] != 0 || pes[2] != 1 {
			continue
		}
		var o pesTimes
		switch pes[7] >> 6 {
		case 3:
			o = pesTimes{decodeTimestamp(pes[9:14]), decodeTimestamp(pes[14:19])}
		case 2: // muxers leave DTS out when it equals PTS
			o = pesTimes{decodeTimestamp(pes[9:14]), decodeTimestamp(pes[9:14])}
		default:
			t.Fatalf("video PES without PTS")
		}
		pts := fn(len(orig), o)
		orig = append(orig, o)
		if pes[7]>>6 == 3 {
			for i := 14; i < 19; i++ {
				pes[i] = 0xFF
			}
		}
		pes[7] = pes[7]&0x3F | 0x80
		writeTimestamp(pes[9:14], 0x2, pts)
	}
	return out, orig
}

func writeTimestamp(b []byte, prefix byte, ts int64) {
	b[0] = prefix<<4 | byte(ts>>29)&0x0E | 1
	b[1] = byte(ts >> 22)
	b[2] = byte(ts>>14) | 1
	b[3] = byte(ts >> 7)
	b[4] = byte(ts<<1) | 1
}

// demuxVideo runs ts through a Demuxer and returns the H.264 packets
// published, in order.
func demuxVideo(t *testing.T, ts []byte) []*media.Packet {
	t.Helper()
	bus := media.NewBus()
	sub := bus.Subscribe(context.Background())
	defer sub.Close()
	d := NewDemuxer(bus)
	d.feedPackets(ts) // 120 pictures fit the subscriber's 512-packet buffer
	d.Flush()
	var got []*media.Packet
	for {
		select {
		case p := <-sub.Chan():
			if p.Codec == media.CodecH264 {
				got = append(got, p)
			}
		default:
			return got
		}
	}
}

func findVideoPID(t *testing.T, ts []byte) uint16 {
	t.Helper()
	d := NewDemuxer(media.NewBus())
	d.feedPackets(ts[:188*40])
	if d.videoPID == 0 {
		t.Fatal("no video PID in test clip")
	}
	return d.videoPID
}

func TestDemuxRetime_RealX264(t *testing.T) {
	ts, err := os.ReadFile("testdata/x264_bframes.ts")
	if err != nil {
		t.Fatal(err)
	}
	pid := findVideoPID(t, ts)

	for _, tc := range []struct {
		name string
		pts  func(i int, o pesTimes) int64
	}{
		// PTS correct, DTS missing: the MPEG-TS/RTP case on our origin.
		{"no DTS", func(_ int, o pesTimes) int64 { return o.pts }},
		// PTS in decode order: every reordered picture carries the
		// wrong slot.
		{"no DTS, decode-order PTS", func(_ int, o pesTimes) int64 { return o.dts }},
		// The v2.16.6 encoder: duplicate PTS values.
		{"no DTS, duplicate PTS", func(i int, o pesTimes) int64 { return o.dts - int64(i%2)*3000 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mod, orig := rewriteVideoPES(t, ts, pid, tc.pts)
			got := demuxVideo(t, mod)
			if len(got) != len(orig) {
				t.Fatalf("got %d video packets, want %d", len(got), len(orig))
			}
			for i := 1; i < len(got); i++ {
				if got[i].DTS <= got[i-1].DTS {
					t.Fatalf("packet %d: DTS %d not after %d", i, got[i].DTS, got[i-1].DTS)
				}
				if got[i].DTS > got[i].PTS {
					t.Fatalf("packet %d: DTS %d > PTS %d", i, got[i].DTS, got[i].PTS)
				}
			}
			// Display order: rank of each picture by output PTS must equal
			// its rank by x264's own PTS.
			rank := func(v []int64) []int {
				idx := make([]int, len(v))
				for i := range idx {
					idx[i] = i
				}
				sort.SliceStable(idx, func(a, b int) bool { return v[idx[a]] < v[idx[b]] })
				return idx
			}
			var want, have []int64
			for i := range got {
				want = append(want, orig[i].pts)
				have = append(have, got[i].PTS)
			}
			rw, rh := rank(want), rank(have)
			wrong := 0
			for i := range rw {
				if rw[i] != rh[i] {
					wrong++
				}
			}
			if wrong > 0 {
				t.Errorf("%d of %d pictures out of display order", wrong, len(got))
			}
			if tc.name == "no DTS" {
				for i := range got {
					if got[i].PTS != orig[i].pts {
						t.Fatalf("picture %d: PTS changed %d -> %d", i, orig[i].pts, got[i].PTS)
					}
				}
			}
		})
	}
}

// A source that does send DTS is passed through untouched.
func TestDemuxRetime_DTSPassthrough(t *testing.T) {
	ts, err := os.ReadFile("testdata/x264_bframes.ts")
	if err != nil {
		t.Fatal(err)
	}
	got := demuxVideo(t, ts)
	_, orig := rewriteVideoPES(t, ts, findVideoPID(t, ts), func(_ int, o pesTimes) int64 { return o.pts })
	if len(got) != len(orig) {
		t.Fatalf("got %d packets, want %d", len(got), len(orig))
	}
	for i := range got {
		if got[i].PTS != orig[i].pts || got[i].DTS != orig[i].dts {
			t.Fatalf("packet %d: %d/%d, want %d/%d", i, got[i].PTS, got[i].DTS, orig[i].pts, orig[i].dts)
		}
	}
}
