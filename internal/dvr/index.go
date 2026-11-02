package dvr

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// ErrNoRecording is returned when a request matches no recorded segments.
var ErrNoRecording = errors.New("dvr: nothing recorded in the requested range")

// Manifest renders the HLS playlist or DASH MPD for req on app/stream.
// Segment URIs are relative ("dvr/{session}/…") unless uriPrefix is set,
// so a playlist served from /{app}/{stream}/ resolves them there.
func (e *Engine) Manifest(ctx context.Context, app, stream string, req Request, uriPrefix string) (body, contentType string, err error) {
	key := media.StreamKey{App: app, Name: stream}
	now := time.Now()
	nowMS := now.UnixMilli()
	window := int64(e.Window(app, stream))
	segDurS := e.cfg.HLS.SegmentDurationS
	if segDurS <= 0 {
		segDurS = 4
	}

	segs, sel, err := e.selectSegments(ctx, key, req, nowMS, window)
	if err != nil {
		return "", "", err
	}
	shiftS, depthS, toMS := sel.shiftS, sel.depthS, sel.toMS
	sessions, err := e.sessionsFor(ctx, key, segs)
	if err != nil {
		return "", "", err
	}

	if req.DASH {
		opts := mpdOptions{
			Prefix:   uriPrefix,
			Now:      now,
			SegDurS:  segDurS,
			ShiftS:   shiftS,
			DepthS:   depthS,
			Dynamic:  req.Kind != KindRange,
			Sessions: sessions,
		}
		return renderMPD(segs, opts), "application/dash+xml", nil
	}

	kind := hlsLive
	if req.Kind == KindRange {
		kind = hlsEvent
		if toMS <= nowMS {
			kind = hlsVOD
		}
	}
	return renderHLS(segs, kind, uriPrefix, segDurS), "application/vnd.apple.mpegurl", nil
}

// selection is the time range a Request resolved to.
type selection struct {
	fromMS, toMS, shiftS, depthS int64
}

// selectSegments resolves req against the rewind window and returns its
// segments, oldest first. Shared by Manifest and VariantInfo so an ABR
// rewind master lists exactly the variants whose own playlist would serve.
func (e *Engine) selectSegments(ctx context.Context, key media.StreamKey, req Request, nowMS, window int64) ([]segmentRow, selection, error) {
	var sel selection
	switch req.Kind {
	case KindWindow:
		sel.fromMS, sel.toMS, sel.depthS = nowMS-window*1000, nowMS+3600_000, window
	case KindRange:
		sel.fromMS, sel.toMS = req.FromMS, req.FromMS+req.LengthS*1000
	case KindTimeshift:
		if req.ShiftS >= window {
			return nil, sel, fmt.Errorf("%w: timeshift %ds is beyond the %ds rewind window", ErrNoRecording, req.ShiftS, window)
		}
		sel.shiftS, sel.depthS = req.ShiftS, req.DepthS
		if sel.depthS <= 0 {
			sel.depthS = int64(e.timeshiftDepthS())
		}
		sel.toMS = nowMS - sel.shiftS*1000
		sel.fromMS = sel.toMS - sel.depthS*1000
	}

	segs, err := e.segmentsInRange(ctx, key, sel.fromMS, sel.toMS)
	if err != nil {
		return nil, sel, err
	}
	if req.Kind == KindTimeshift {
		// Only segments that had fully finished by the delayed edge.
		n := len(segs)
		for n > 0 && segs[n-1].StartMS+segs[n-1].DurationMS > sel.toMS {
			n--
		}
		segs = segs[:n]
	}
	if len(segs) == 0 {
		return nil, sel, ErrNoRecording
	}
	return segs, sel, nil
}

// Variant describes one recorded rendition as an HLS master's
// EXT-X-STREAM-INF needs it, taken from the recording itself so an ABR
// rewind master still works after the encoder has disconnected.
type Variant struct {
	Bandwidth    int64 // peak bps over the selected segments
	AvgBandwidth int64 // mean bps over the selected segments
	Width        int
	Height       int
	Codecs       string // RFC 6381, video then audio
}

// VariantInfo describes app/stream's recording for req. It returns
// ErrNoRecording (see IsNotFound) when req selects nothing.
func (e *Engine) VariantInfo(ctx context.Context, app, stream string, req Request) (Variant, error) {
	key := media.StreamKey{App: app, Name: stream}
	segs, _, err := e.selectSegments(ctx, key, req, time.Now().UnixMilli(), int64(e.Window(app, stream)))
	if err != nil {
		return Variant{}, err
	}
	var v Variant
	var bytes, durMS int64
	for _, sg := range segs {
		if sg.DurationMS <= 0 {
			continue
		}
		bytes += sg.Size
		durMS += sg.DurationMS
		if bps := sg.Size * 8000 / sg.DurationMS; bps > v.Bandwidth {
			v.Bandwidth = bps
		}
	}
	if durMS > 0 {
		v.AvgBandwidth = bytes * 8000 / durMS
	}
	// The newest session describes what a player will end up on.
	sessions, err := e.sessionsFor(ctx, key, segs[len(segs)-1:])
	if err != nil {
		return Variant{}, err
	}
	if sess, ok := sessions[segs[len(segs)-1].SessionMS]; ok {
		v.Width, v.Height = sess.Width, sess.Height
		var codecs []string
		if sess.HasVideo && sess.VideoCodecs != "" {
			codecs = append(codecs, sess.VideoCodecs)
		}
		if sess.HasAudio && sess.AudioCodecs != "" {
			codecs = append(codecs, sess.AudioCodecs)
		}
		v.Codecs = strings.Join(codecs, ",")
		if !sess.HasVideo {
			v.Width, v.Height = 0, 0
		}
	}
	return v, nil
}

// timeshiftDepthS is the default history behind a timeshifted live edge:
// the same length as the normal live HLS playlist.
func (e *Engine) timeshiftDepthS() int {
	seg, win := e.cfg.HLS.SegmentDurationS, e.cfg.HLS.PlaylistWindow
	if seg <= 0 {
		seg = 4
	}
	if win <= 0 {
		win = 6
	}
	return seg * win
}

// ResolveFile maps a recording file request to the bytes to serve: a byte
// range of path (n = -1 for the whole file).
func (e *Engine) ResolveFile(ctx context.Context, app, stream string, sessionMS int64, f File) (path string, off, n int64, err error) {
	key := media.StreamKey{App: app, Name: stream}
	if f.Init {
		var dir string
		err := e.database.QueryRowContext(ctx,
			`SELECT dir_path FROM dvr_sessions WHERE app_name = ? AND stream_name = ? AND session_ms = ?`,
			key.App, key.Name, sessionMS).Scan(&dir)
		if err != nil {
			return "", 0, 0, errNotFound
		}
		return filepath.Join(dir, initFileName(f.Track)), 0, -1, nil
	}

	var size, videoBytes int64
	err = e.database.QueryRowContext(ctx, `
		SELECT file_path, size_bytes, video_bytes FROM dvr_segments
		WHERE app_name = ? AND stream_name = ? AND msn = ? AND session_ms = ?`,
		key.App, key.Name, f.MSN, sessionMS).Scan(&path, &size, &videoBytes)
	if err != nil {
		return "", 0, 0, errNotFound
	}
	switch f.Track {
	case TrackVideo:
		if videoBytes == 0 {
			return "", 0, 0, errNotFound
		}
		return path, 0, videoBytes, nil
	case TrackAudio:
		if size-videoBytes <= 0 {
			return "", 0, 0, errNotFound
		}
		return path, videoBytes, size - videoBytes, nil
	}
	return path, 0, -1, nil
}

// IsNotFound reports whether err means the requested file isn't recorded.
func IsNotFound(err error) bool {
	return errors.Is(err, errNotFound) || errors.Is(err, ErrNoRecording) || errors.Is(err, os.ErrNotExist)
}

func (e *Engine) segmentsInRange(ctx context.Context, key media.StreamKey, fromMS, toMS int64) ([]segmentRow, error) {
	rows, err := e.database.QueryContext(ctx, `
		SELECT msn, session_ms, disc_seq, start_pts_ms, duration_ms, file_path, size_bytes,
		       video_bytes, video_tfdt, video_dur, audio_tfdt, audio_dur
		FROM dvr_segments
		WHERE app_name = ? AND stream_name = ? AND session_ms > 0
		  AND end_pts_ms > ? AND start_pts_ms < ?
		ORDER BY msn`,
		key.App, key.Name, fromMS, toMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []segmentRow
	for rows.Next() {
		var s segmentRow
		if err := rows.Scan(&s.MSN, &s.SessionMS, &s.DiscSeq, &s.StartMS, &s.DurationMS, &s.Path, &s.Size,
			&s.VideoBytes, &s.VideoTfdt, &s.VideoDur, &s.AudioTfdt, &s.AudioDur); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (e *Engine) sessionsFor(ctx context.Context, key media.StreamKey, segs []segmentRow) (map[int64]sessionRow, error) {
	ids := map[int64]bool{}
	var args []any
	args = append(args, key.App, key.Name)
	for _, s := range segs {
		if !ids[s.SessionMS] {
			ids[s.SessionMS] = true
			args = append(args, s.SessionMS)
		}
	}
	rows, err := e.database.QueryContext(ctx, `
		SELECT session_ms, dir_path, has_video, has_audio, video_codecs, audio_codecs,
		       width, height, sample_rate, channels
		FROM dvr_sessions
		WHERE app_name = ? AND stream_name = ? AND session_ms IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]sessionRow, len(ids))
	for rows.Next() {
		var s sessionRow
		var hv, ha sql.NullInt64
		if err := rows.Scan(&s.SessionMS, &s.Dir, &hv, &ha, &s.VideoCodecs, &s.AudioCodecs,
			&s.Width, &s.Height, &s.SampleRate, &s.Channels); err != nil {
			return nil, err
		}
		s.HasVideo, s.HasAudio = hv.Int64 == 1, ha.Int64 == 1
		out[s.SessionMS] = s
	}
	return out, rows.Err()
}
