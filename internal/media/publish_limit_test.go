package media

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func camLimit(max int) []PublishLimit {
	return []PublishLimit{{Pattern: "cam_%i", Max: max, Match: func(n string) bool { return strings.HasPrefix(n, "cam_") }}}
}

// RegisterLimited counts only same-app streams under the pattern, and
// a slot frees up when a stream stops.
func TestRegisterLimited(t *testing.T) {
	r := NewRegistry()
	lim := camLimit(2)
	reg := func(app, name string) error {
		return r.RegisterLimited(&Stream{Key: StreamKey{App: app, Name: name}}, lim)
	}
	for _, n := range []string{"cam_1", "cam_2", "other"} {
		if err := reg("live", n); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
	var le *PublishLimitError
	if err := reg("live", "cam_3"); !errors.As(err, &le) || le.Max != 2 {
		t.Fatalf("third cam = %v, want PublishLimitError", err)
	}
	if err := reg("events", "cam_3"); err != nil {
		t.Fatalf("other app counted against the limit: %v", err)
	}
	if r.LimitReached("live", "cam_9", lim) == nil {
		t.Error("LimitReached = nil with the limit full")
	}
	r.Unregister(r.Get(StreamKey{App: "live", Name: "cam_1"}))
	if err := reg("live", "cam_3"); err != nil {
		t.Fatalf("slot not freed after a stream stopped: %v", err)
	}
}

// Encoders racing for the last slots: exactly Max win.
func TestRegisterLimitedRace(t *testing.T) {
	r := NewRegistry()
	lim := camLimit(3)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "cam_" + strings.Repeat("1", i+1)
			if r.RegisterLimited(&Stream{Key: StreamKey{App: "live", Name: name}}, lim) == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if ok != 3 {
		t.Fatalf("%d streams registered, want 3", ok)
	}
}
