package api

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// abrTrack is one rendition of an ABR master, from either a manual group
// (abr_groups/abr_variants, edited in the panel) or an automatic %i group
// (media.Registry). Zero/empty fields mean "auto": filled in from the live
// stream when the master is rendered.
type abrTrack struct {
	app, stream  string
	bandwidth    int // peak bps
	avgBandwidth int // 0 = omit AVERAGE-BANDWIDTH
	width        int
	height       int
	frameRate    float64
	codecs       string
	audioOnly    bool
	label        string
}

// abrMaster renders the HLS master for /{app}/{name}/playlist.m3u8 when
// name is an ABR group. found is false when it isn't one. A manual group
// wins over an automatic %i group of the same name, since it was set up on
// purpose.
//
// Offline tracks are left out: a player can't tell a listed variant has no
// data behind it and can get stuck on it. When every track is offline, ok
// is false and the caller answers 503 rather than an empty master that
// looks healthy (the standalone DVR server's hls.LiveABRMaster does the
// same, after the same incidents).
func (s *Server) abrMaster(ctx context.Context, app, name string) (body string, ok, found bool) {
	tracks, found := s.manualABRTracks(ctx, app, name)
	if found {
		tracks = s.expandPatternTracks(ctx, tracks, false)
	}
	if !found {
		for _, st := range s.registry.GetABRGroup(app, name) {
			tracks = append(tracks, abrTrack{
				app: st.Key.App, stream: st.Key.Name,
				bandwidth: st.ABRBandwidthHint, label: st.ABRLabel,
			})
			found = true
		}
	}
	if !found {
		return "", false, false
	}

	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	sb.WriteString("#EXT-X-VERSION:3\n")
	live := 0
	for _, t := range tracks {
		st := s.registry.Get(media.StreamKey{App: t.app, Name: t.stream})
		if st == nil {
			continue
		}
		live++
		fmt.Fprintf(&sb, "#EXT-X-STREAM-INF:%s\n%s/%s/chunks.m3u8\n", streamInf(t, st), t.app, t.stream)
	}
	return sb.String(), live > 0, true
}

// manualABRTracks loads a panel-configured group's tracks in order.
func (s *Server) manualABRTracks(ctx context.Context, app, name string) ([]abrTrack, bool) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT v.src_app_name, v.stream_name, v.bandwidth, v.avg_bandwidth, v.width, v.height,
		        v.frame_rate, v.codecs, v.audio_only, v.label
		 FROM abr_groups g
		 JOIN abr_variants v ON v.group_id = g.id
		 WHERE g.app_name = ? AND g.stream_name = ?
		 ORDER BY v.sort_order ASC, v.bandwidth DESC`, app, name)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	var tracks []abrTrack
	for rows.Next() {
		var t abrTrack
		var audioOnly int
		if err := rows.Scan(&t.app, &t.stream, &t.bandwidth, &t.avgBandwidth, &t.width, &t.height,
			&t.frameRate, &t.codecs, &audioOnly, &t.label); err != nil {
			continue
		}
		if t.app == "" {
			t.app = app
		}
		t.audioOnly = audioOnly == 1
		tracks = append(tracks, t)
	}
	return tracks, len(tracks) > 0
}

// expandPatternTracks replaces each track whose stream is a pattern
// ("test%i", "cam_*") with one track per matching stream, so a manual group
// can say "every FMLE rendition" instead of listing test1, test2, test3:
// renditions added or removed in the encoder show up without editing the
// group. Matches are the live streams, plus, when withRecorded is set (the
// rewind master), streams with nDVR recordings, which outlive the encoder.
//
// Bandwidth and resolution set on a pattern track would be wrong for all
// but one rendition, so expanded tracks take those from each stream (auto);
// codecs, frame rate and audio-only carry over. A stream already listed by
// name earlier in the group isn't added twice. Matches are ordered by their
// number (test2 before test10).
func (s *Server) expandPatternTracks(ctx context.Context, tracks []abrTrack, withRecorded bool) []abrTrack {
	var out []abrTrack
	seen := map[string]bool{}
	add := func(t abrTrack) {
		if k := t.app + "/" + t.stream; !seen[k] {
			seen[k] = true
			out = append(out, t)
		}
	}
	for _, t := range tracks {
		if !db.IsStreamPattern(t.stream) {
			add(t)
		}
	}
	for _, t := range tracks {
		if !db.IsStreamPattern(t.stream) {
			continue
		}
		names := map[string]bool{}
		for _, st := range s.registry.All() {
			if st.Key.App == t.app && db.MatchStreamPattern(t.stream, st.Key.Name) {
				names[st.Key.Name] = true
			}
		}
		if withRecorded {
			for _, n := range s.recordedStreams(ctx, t.app) {
				if db.MatchStreamPattern(t.stream, n) {
					names[n] = true
				}
			}
		}
		sorted := make([]string, 0, len(names))
		for n := range names {
			sorted = append(sorted, n)
		}
		sort.Slice(sorted, func(i, j int) bool { return naturalLess(sorted[i], sorted[j]) })
		for _, n := range sorted {
			e := t
			e.stream, e.label = n, n
			e.bandwidth, e.avgBandwidth, e.width, e.height = 0, 0, 0, 0
			add(e)
		}
	}
	// Keep the group's own order: named tracks where they were, pattern
	// matches in place of their pattern.
	// A named track that the pattern also matches keeps its own settings
	// but is placed among the pattern's matches, in number order.
	var ordered []abrTrack
	placed := map[string]bool{}
	for _, t := range tracks {
		var group []abrTrack
		for _, o := range out {
			k := o.app + "/" + o.stream
			if placed[k] || o.app != t.app || !db.MatchStreamPattern(t.stream, o.stream) {
				continue
			}
			placed[k] = true
			group = append(group, o)
		}
		sort.SliceStable(group, func(i, j int) bool { return naturalLess(group[i].stream, group[j].stream) })
		ordered = append(ordered, group...)
	}
	return ordered
}

// recordedStreams lists app's streams that have nDVR sessions.
func (s *Server) recordedStreams(ctx context.Context, app string) []string {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT stream_name FROM dvr_sessions WHERE app_name = ?`, app)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			out = append(out, n)
		}
	}
	return out
}

// naturalLess orders names with numbers by value: test2 < test10.
func naturalLess(a, b string) bool {
	for a != "" && b != "" {
		ai, bi := digitRun(a), digitRun(b)
		if ai > 0 && bi > 0 {
			na, _ := strconv.Atoi(a[:ai])
			nb, _ := strconv.Atoi(b[:bi])
			if na != nb {
				return na < nb
			}
			a, b = a[ai:], b[bi:]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func digitRun(s string) int {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return n
}

// streamInf renders t's EXT-X-STREAM-INF attributes, filling every "auto"
// field from the live stream st. Without RESOLUTION/CODECS and distinct
// BANDWIDTHs, players can't tell renditions apart (the DVR server's
// 2026-09-30 JW Player incident).
func streamInf(t abrTrack, st *media.Stream) string {
	ci := st.Bus.CodecInfo()
	bw := t.bandwidth
	if bw <= 0 {
		bw = st.ABRBandwidthHint // e.g. FMLE's totalDatarate
	}
	if bw <= 0 {
		bw = measuredBandwidth(st)
	}
	attrs := fmt.Sprintf("BANDWIDTH=%d", bw)
	if t.avgBandwidth > 0 {
		attrs += fmt.Sprintf(",AVERAGE-BANDWIDTH=%d", t.avgBandwidth)
	}

	w, h, codecs := t.width, t.height, t.codecs
	if ci != nil {
		hasAudio := media.TSAudioCodec(ci.AudioCodec)
		hasVideo := !t.audioOnly && ci.VideoCodec != media.CodecUnknown
		if w == 0 && h == 0 && hasVideo {
			w, h = ci.Width, ci.Height
		}
		if codecs == "" {
			var parts []string
			if hasVideo {
				parts = append(parts, videoCodecString(ci))
			}
			if hasAudio {
				parts = append(parts, media.AudioCodecString(ci))
			}
			codecs = strings.Join(parts, ",")
		}
	}
	if codecs == "" && t.audioOnly {
		codecs = "mp4a.40.2"
	}
	return formatStreamInf(attrs, w, h, t.frameRate, codecs, t.label, t.audioOnly)
}

// formatStreamInf appends the optional EXT-X-STREAM-INF attributes to attrs
// (which already holds BANDWIDTH and maybe AVERAGE-BANDWIDTH), in the order
// both the live and the rewind ABR masters use.
func formatStreamInf(attrs string, w, h int, frameRate float64, codecs, label string, audioOnly bool) string {
	if !audioOnly && w > 0 && h > 0 {
		attrs += fmt.Sprintf(",RESOLUTION=%dx%d", w, h)
	}
	if frameRate > 0 {
		attrs += fmt.Sprintf(",FRAME-RATE=%.3f", frameRate)
	}
	if codecs != "" {
		attrs += fmt.Sprintf(",CODECS=%q", codecs)
	}
	if label != "" {
		attrs += fmt.Sprintf(",NAME=%q", label)
	}
	return attrs
}

// measuredBandwidth estimates a peak BANDWIDTH from st's average ingest
// rate since it started, with 20% headroom (BANDWIDTH is a peak, and an
// under-stated one makes players pick a rendition they can't sustain).
// Falls back to 500 kbps for the first seconds, so the tag (required by
// RFC 8216) is never 0.
func measuredBandwidth(st *media.Stream) int {
	secs := time.Since(st.StartedAt).Seconds()
	if secs < 3 {
		return 500000
	}
	bps := float64(st.Bus.BytesIn()) * 8 / secs
	if bps <= 0 {
		return 500000
	}
	return int(bps * 1.2)
}
