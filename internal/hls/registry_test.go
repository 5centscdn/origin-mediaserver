package hls

import (
	"strings"
	"testing"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// A new source on the same key (reconnect, pull fallback switch) continues
// the old playlist: media sequence keeps counting, old segments stay, the
// first new segment is a discontinuity, and file names don't collide.
func TestPlaylist_ContinueFromPrevious(t *testing.T) {
	reg := NewPlaylistRegistry()
	key := media.StreamKey{App: "live", Name: "cam1"}

	old := NewPlaylist(t.TempDir(), 3, 6)
	reg.Register(key, old)
	for i := 0; i < 5; i++ {
		old.AddSegment(Segment{Seq: i, URI: "seg" + string(rune('0'+i)) + ".ts", DurationS: 6})
	}
	reg.Retire(key, old)

	prev := reg.Previous(key)
	if prev != old {
		t.Fatal("Previous didn't return the retired playlist")
	}
	pl := NewPlaylist(t.TempDir(), 3, 6)
	next := pl.ContinueFrom(prev)
	if next != 6 { // last was 4; 5 is left for the old packager's final flush
		t.Fatalf("next seq = %d, want 6", next)
	}
	pl.AddSegment(Segment{Seq: next, URI: "seg6.ts", DurationS: 6, IsDiscont: true})
	out := pl.render(false)
	for _, want := range []string{"#EXT-X-MEDIA-SEQUENCE:3\n", "seg3.ts", "seg4.ts", "#EXT-X-DISCONTINUITY\n#EXTINF:6.000000,\nseg6.ts"} {
		if !strings.Contains(out, want) {
			t.Errorf("playlist missing %q:\n%s", want, out)
		}
	}

	// Once the discontinuity slides out, DISCONTINUITY-SEQUENCE counts it.
	for i := 7; i < 10; i++ {
		pl.AddSegment(Segment{Seq: i, URI: "x.ts", DurationS: 6})
	}
	if out := pl.render(false); !strings.Contains(out, "#EXT-X-DISCONTINUITY-SEQUENCE:1\n") || strings.Contains(out, "#EXT-X-DISCONTINUITY\n") {
		t.Errorf("after the discontinuity left the window:\n%s", out)
	}

	// A late Retire from the old packager doesn't remove the new one.
	reg.Register(key, pl)
	reg.Retire(key, old)
	if reg.Get(key) != pl {
		t.Error("old packager's Retire removed the new playlist")
	}
}
