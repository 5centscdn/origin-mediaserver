package dvr

import "testing"

func TestParseRequest(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
		want Request
	}{
		{"playlist_dvr.m3u8", true, Request{Kind: KindWindow}},
		{"manifest_dvr.mpd", true, Request{DASH: true, Kind: KindWindow}},
		{"playlist_dvr_range-1793597867-1737.m3u8", true, Request{Kind: KindRange, FromMS: 1793597867000, LengthS: 1737}},
		{"manifest_dvr_range-1793597867-60.mpd", true, Request{DASH: true, Kind: KindRange, FromMS: 1793597867000, LengthS: 60}},
		{"playlist_dvr_timeshift-3600.m3u8", true, Request{Kind: KindTimeshift, ShiftS: 3600}},
		{"playlist_dvr_timeshift-3600-120.m3u8", true, Request{Kind: KindTimeshift, ShiftS: 3600, DepthS: 120}},
		{"manifest_dvr_timeshift-60.mpd", true, Request{DASH: true, Kind: KindTimeshift, ShiftS: 60}},

		{"playlist.m3u8", false, Request{}},
		{"playlist_dvr.mpd", false, Request{}},
		{"playlist_dvr_range-100.m3u8", false, Request{}},
		{"playlist_dvr_range-0-10.m3u8", false, Request{}},
		{"playlist_dvr_range-1-2-3.m3u8", false, Request{}},
		{"playlist_dvr_range-x-10.m3u8", false, Request{}},
		{"playlist_dvr_timeshift-0.m3u8", false, Request{}},
		{"playlist_dvr_other.m3u8", false, Request{}},
	}
	for _, c := range cases {
		got, ok := ParseRequest(c.name)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParseRequest(%q) = %+v, %v; want %+v, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestParseFile(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
		want File
	}{
		{"init.mp4", true, File{Init: true}},
		{"init_v.mp4", true, File{Init: true, Track: TrackVideo}},
		{"init_a.mp4", true, File{Init: true, Track: TrackAudio}},
		{"42.m4s", true, File{MSN: 42}},
		{"42_v.m4s", true, File{MSN: 42, Track: TrackVideo}},
		{"0_a.m4s", true, File{MSN: 0, Track: TrackAudio}},

		{"init_x.mp4", false, File{}},
		{"seg.mp4", false, File{}},
		{"-1.m4s", false, File{}},
		{"42.ts", false, File{}},
		{"../42.m4s", false, File{}},
	}
	for _, c := range cases {
		got, ok := ParseFile(c.name)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParseFile(%q) = %+v, %v; want %+v, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}
