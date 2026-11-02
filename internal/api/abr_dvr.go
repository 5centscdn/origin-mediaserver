package api

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/5centscdn/origin-mediaserver/internal/dvr"
)

// abrDVRMaster renders the nDVR rewind master for ABR group name, the
// rewind counterpart of abrMaster: /{app}/{group}/playlist_dvr*.m3u8 lists
// one EXT-X-STREAM-INF per variant, pointing at that variant's own nDVR
// playlist with the same file name under the group
// ({srcApp}/{srcStream}/playlist_dvr*.m3u8, served by
// handleVariantDVRManifest, the same layout as the live master), so window,
// range and timeshift requests carry through unchanged. The DVR engine records each variant as its own
// stream; nothing is recorded under the group name.
//
// Variants come from, in order: a manual group (abr_groups), the live %i
// group in the registry, or, once the encoder has gone away, the recorded
// streams named {group}{N} (the %i naming FMLE uses, e.g. test1..test3 for
// group test). The last is only tried when name has no recording of its own.
//
// Only variants with recording in req are listed, so the master never points
// a player at a 404. found is false when name isn't a group; ok is false
// when it is one but no variant has anything in req.
//
// HLS only. A DASH group rewind (manifest_dvr*.mpd) would need the
// variants merged into one MPD's adaptation set and isn't implemented.
func (s *Server) abrDVRMaster(ctx context.Context, app, name, file string, req dvr.Request) (body string, ok, found bool) {
	tracks, found := s.manualABRTracks(ctx, app, name)
	if !found {
		for _, st := range s.registry.GetABRGroup(app, name) {
			tracks = append(tracks, abrTrack{
				app: st.Key.App, stream: st.Key.Name, label: st.ABRLabel,
			})
			found = true
		}
	}
	if !found {
		if _, err := s.dvr.VariantInfo(ctx, app, name, req); err == nil {
			return "", false, false // a real recorded stream, not a group
		}
		for _, stream := range s.recordedVariants(ctx, app, name) {
			tracks = append(tracks, abrTrack{app: app, stream: stream})
			found = true
		}
	}
	if !found {
		return "", false, false
	}

	var sb strings.Builder
	sb.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	listed := 0
	for _, t := range tracks {
		v, err := s.dvr.VariantInfo(ctx, t.app, t.stream, req)
		if err != nil {
			continue
		}
		listed++
		fmt.Fprintf(&sb, "#EXT-X-STREAM-INF:%s\n%s/%s/%s\n", dvrStreamInf(t, v), t.app, t.stream, file)
	}
	return sb.String(), listed > 0, true
}

// dvrStreamInf renders t's EXT-X-STREAM-INF for rewind. Panel-set values
// win; anything left "auto" comes from the recording (not the live stream,
// which may be gone), so renditions stay distinguishable after the encoder
// disconnects.
func dvrStreamInf(t abrTrack, v dvr.Variant) string {
	bw := int64(t.bandwidth)
	if bw <= 0 {
		bw = v.Bandwidth
	}
	if bw <= 0 {
		bw = 500000 // RFC 8216 requires BANDWIDTH; never 0
	}
	attrs := fmt.Sprintf("BANDWIDTH=%d", bw)
	avg := int64(t.avgBandwidth)
	if avg <= 0 {
		avg = v.AvgBandwidth
	}
	if avg > 0 && avg <= bw {
		attrs += fmt.Sprintf(",AVERAGE-BANDWIDTH=%d", avg)
	}
	w, h, codecs := t.width, t.height, t.codecs
	if w == 0 && h == 0 {
		w, h = v.Width, v.Height
	}
	if codecs == "" {
		codecs = v.Codecs
	}
	return formatStreamInf(attrs, w, h, t.frameRate, codecs, t.label, t.audioOnly)
}

// recordedVariants returns the recorded streams in app named {group}{N},
// ordered by N: the %i variants of group as the DVR engine stored them.
func (s *Server) recordedVariants(ctx context.Context, app, group string) []string {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT stream_name FROM dvr_sessions WHERE app_name = ? AND stream_name LIKE ? ESCAPE '\'`,
		app, likeEscape(group)+"%")
	if err != nil {
		return nil
	}
	defer rows.Close()
	re := regexp.MustCompile("^" + regexp.QuoteMeta(group) + "([0-9]+)$")
	type variant struct {
		name string
		idx  int
	}
	var vs []variant
	for rows.Next() {
		var n string
		if rows.Scan(&n) != nil {
			continue
		}
		if m := re.FindStringSubmatch(n); m != nil {
			idx, _ := strconv.Atoi(m[1])
			vs = append(vs, variant{n, idx})
		}
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].idx < vs[j].idx })
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.name
	}
	return out
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
