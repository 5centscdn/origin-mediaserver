package media

import "testing"

func TestGetABRGroup_SortedByVariantIndex(t *testing.T) {
	r := NewRegistry()

	// Register out of order: index 3, then 1, then 2.
	streams := []*Stream{
		{Key: StreamKey{App: "live", Name: "camera_3"}, ABRGroup: "camera", ABRVariantIndex: 3},
		{Key: StreamKey{App: "live", Name: "camera_1"}, ABRGroup: "camera", ABRVariantIndex: 1},
		{Key: StreamKey{App: "live", Name: "camera_2"}, ABRGroup: "camera", ABRVariantIndex: 2},
	}
	for _, s := range streams {
		if err := r.Register(s); err != nil {
			t.Fatalf("Register(%v): %v", s.Key, err)
		}
	}

	got := r.GetABRGroup("live", "camera")
	if len(got) != 3 {
		t.Fatalf("GetABRGroup returned %d variants, want 3", len(got))
	}
	for i, want := range []int{1, 2, 3} {
		if got[i].ABRVariantIndex != want {
			t.Fatalf("variant at position %d has index %d, want %d (order: %v)",
				i, got[i].ABRVariantIndex, want, indexesOf(got))
		}
	}
}

func TestGetABRGroup_UnknownGroup(t *testing.T) {
	r := NewRegistry()
	if got := r.GetABRGroup("events", "camera"); got != nil {
		t.Fatalf("GetABRGroup(other app) = %v, want nil: groups are per app", got)
	}
	if got := r.GetABRGroup("live", "nonexistent"); got != nil {
		t.Fatalf("GetABRGroup(unknown) = %v, want nil", got)
	}
}

func indexesOf(streams []*Stream) []int {
	out := make([]int, len(streams))
	for i, s := range streams {
		out[i] = s.ABRVariantIndex
	}
	return out
}
