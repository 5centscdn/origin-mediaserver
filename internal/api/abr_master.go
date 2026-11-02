package api

import (
	"context"
	"fmt"
	"strings"
	"time"

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
		hasAudio := ci.AudioCodec != media.CodecUnknown
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
