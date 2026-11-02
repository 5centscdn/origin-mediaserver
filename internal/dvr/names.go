package dvr

import (
	"strconv"
	"strings"
)

// Request is a parsed nDVR playlist/manifest file name — the last path
// element of /{app}/{stream}/<name>. The names match the standalone DVR
// server's (and Nimble's) so players and CDN rules written for those keep
// working against this origin unchanged.
type Request struct {
	DASH bool // manifest_dvr*.mpd instead of playlist_dvr*.m3u8
	Kind RequestKind

	// FromMS/LengthS are set for KindRange: an absolute UTC start (the
	// URL carries Unix seconds) and a length in seconds.
	FromMS  int64
	LengthS int64

	// ShiftS/DepthS are set for KindTimeshift: the live edge shifted back
	// ShiftS seconds, showing DepthS seconds of history behind it (0 =
	// the default depth, see Engine.timeshiftDepthS).
	ShiftS int64
	DepthS int64
}

// RequestKind selects which slice of the recording a Request covers.
type RequestKind int

const (
	// KindWindow is the whole rewind window, live (playlist_dvr.m3u8,
	// manifest_dvr.mpd).
	KindWindow RequestKind = iota
	// KindRange is a fixed time range: VOD once it's fully in the past
	// (playlist_dvr_range-{from}-{len}.m3u8, manifest_dvr_range-....mpd).
	KindRange
	// KindTimeshift is a live view delayed by a fixed offset
	// (playlist_dvr_timeshift-{shift}[-{depth}].m3u8 / .mpd).
	KindTimeshift
)

// ParseRequest parses an nDVR playlist/manifest file name. ok is false for
// anything else, so callers can fall through to their other routes.
func ParseRequest(name string) (req Request, ok bool) {
	var body string
	switch {
	case strings.HasPrefix(name, "playlist_dvr") && strings.HasSuffix(name, ".m3u8"):
		body = strings.TrimSuffix(strings.TrimPrefix(name, "playlist_dvr"), ".m3u8")
	case strings.HasPrefix(name, "manifest_dvr") && strings.HasSuffix(name, ".mpd"):
		body = strings.TrimSuffix(strings.TrimPrefix(name, "manifest_dvr"), ".mpd")
		req.DASH = true
	default:
		return req, false
	}

	switch {
	case body == "":
		req.Kind = KindWindow
		return req, true

	case strings.HasPrefix(body, "_range-"):
		a, b, n := splitInts(strings.TrimPrefix(body, "_range-"))
		if n != 2 || a <= 0 || b <= 0 {
			return req, false
		}
		req.Kind, req.FromMS, req.LengthS = KindRange, a*1000, b
		return req, true

	case strings.HasPrefix(body, "_timeshift-"):
		a, b, n := splitInts(strings.TrimPrefix(body, "_timeshift-"))
		if n == 0 || a <= 0 || b < 0 {
			return req, false
		}
		req.Kind, req.ShiftS, req.DepthS = KindTimeshift, a, b
		return req, true
	}
	return req, false
}

// splitInts parses "a" or "a-b" into up to two non-negative integers; n is
// how many were present (0 on any parse error).
func splitInts(s string) (a, b int64, n int) {
	parts := strings.Split(s, "-")
	if len(parts) > 2 {
		return 0, 0, 0
	}
	var vals [2]int64
	for i, p := range parts {
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil || v < 0 {
			return 0, 0, 0
		}
		vals[i] = v
	}
	return vals[0], vals[1], len(parts)
}

// File is a parsed recording file name — the last path element of
// /{app}/{stream}/dvr/{session}/<name>.
type File struct {
	Init  bool  // init segment rather than a media segment
	MSN   int64 // media segment's per-stream sequence number
	Track Track
}

// Track selects which part of a recorded file to serve.
type Track int

const (
	// TrackMuxed is the whole file: video and audio together (HLS).
	TrackMuxed Track = iota
	// TrackVideo is the video moof+mdat only (DASH video Representation).
	TrackVideo
	// TrackAudio is the audio moof+mdat only (DASH audio Representation).
	TrackAudio
)

// ParseFile parses init.mp4, init_v.mp4, init_a.mp4, {msn}.m4s,
// {msn}_v.m4s and {msn}_a.m4s.
func ParseFile(name string) (f File, ok bool) {
	var base string
	switch {
	case strings.HasSuffix(name, ".mp4"):
		base, f.Init = strings.TrimSuffix(name, ".mp4"), true
	case strings.HasSuffix(name, ".m4s"):
		base = strings.TrimSuffix(name, ".m4s")
	default:
		return f, false
	}
	switch {
	case strings.HasSuffix(base, "_v"):
		base, f.Track = strings.TrimSuffix(base, "_v"), TrackVideo
	case strings.HasSuffix(base, "_a"):
		base, f.Track = strings.TrimSuffix(base, "_a"), TrackAudio
	}
	if f.Init {
		return f, base == "init"
	}
	msn, err := strconv.ParseInt(base, 10, 64)
	if err != nil || msn < 0 {
		return f, false
	}
	f.MSN = msn
	return f, true
}

// initFileName is the on-disk (and URL) name of a session's init segment
// for track.
func initFileName(t Track) string {
	switch t {
	case TrackVideo:
		return "init_v.mp4"
	case TrackAudio:
		return "init_a.mp4"
	}
	return "init.mp4"
}
