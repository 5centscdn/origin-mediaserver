package dvr

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/mux"
)

const (
	// writeQueueLen bounds segments waiting for disk/SQLite. The packet
	// loop never blocks on I/O: media.Bus drops packets for a slow
	// subscriber, which would corrupt every later segment, so a full queue
	// drops one whole segment instead (a clean gap in the recording).
	writeQueueLen = 64

	// audioWaitMS is how long after the first usable keyframe a session
	// waits for the audio config to show up (MPEG-TS/SRT ingest only learn
	// it from the first ADTS frame) before recording video alone.
	audioWaitMS = 3000

	// maxDTSJump is the largest forward DTS step treated as continuous;
	// anything bigger, or any step backwards, is an encoder restart or a
	// 33-bit MPEG-TS wrap and starts a new session instead of being
	// written into the timeline as a huge sample duration.
	maxDTSJump = 10 * 90000
)

// recorder is one recording run for one *media.Stream. It turns Bus
// packets into fMP4 segments (packet loop) and hands them to a writer
// goroutine that puts them on disk and in the index.
type recorder struct {
	engine *Engine
	stream *media.Stream
	opts   Options
	log    *slog.Logger

	jobs chan func(context.Context)

	// Session state, reset by endSession.
	inSession    bool
	sessionMS    int64
	sessionDir   string
	discSeq      int64
	hasVideo     bool
	hasAudio     bool
	videoMux     *mux.FMP4Muxer
	audioMux     *mux.FMP4Muxer
	audioClock   *mux.AudioClock
	startDTS     int64 // DTS of the session's first keyframe (or audio frame)
	sessionBase  int64 // tfdt startDTS maps to
	audioOffset  bool  // audioBase has been aligned to the first audio frame
	firstKeyWait int64 // wall ms the first usable keyframe was seen while waiting for audio
	warnedHEVC   bool

	// Where this recorder's previous session ended, for a session restarted
	// in-process (DTS jump) whose last segment may still be queued and so
	// not yet visible to Engine.lastPosition.
	hadSession   bool
	prevEndTicks int64

	// Open segment.
	segStartDTS  int64
	lastDTS      int64
	videoSamples []mux.Sample
	pendingVideo *mux.Sample
	audioSamples []mux.Sample

	// Track timelines: tfdt of the next fragment, 90 kHz since dvrEpoch.
	videoBase int64
	audioBase int64
}

func newRecorder(e *Engine, stream *media.Stream, opts Options) *recorder {
	return &recorder{
		engine: e,
		stream: stream,
		opts:   opts,
		log:    slog.With("component", "dvr", "app", stream.Key.App, "stream", stream.Key.Name),
		jobs:   make(chan func(context.Context), writeQueueLen),
	}
}

// run is the packet loop. prev is the previous recorder's done channel for
// the same key (nil if none); done is closed once this recorder's last
// segment is indexed.
func (r *recorder) run(ctx context.Context, prev <-chan struct{}, done chan struct{}) {
	sub := r.stream.Bus.Subscribe(ctx)
	defer sub.Close()

	writerDone := make(chan struct{})
	go r.writer(context.WithoutCancel(ctx), writerDone)
	if o := r.engine.observer; o != nil {
		o.RecordingStarted(r.stream.Key)
	}
	defer func() {
		close(r.jobs)
		<-writerDone
		close(done)
		if o := r.engine.observer; o != nil {
			o.RecordingStopped(r.stream.Key)
		}
	}()

	if prev != nil {
		select {
		case <-prev:
		case <-time.After(5 * time.Second):
			r.log.Warn("previous DVR recorder still flushing; starting anyway")
		case <-ctx.Done():
			return
		}
	}

	r.log.Info("DVR recording started", "window_s", r.opts.WindowS)
	for {
		select {
		case <-ctx.Done():
			r.endSession()
			return
		case pkt, ok := <-sub.Chan():
			if !ok {
				r.endSession()
				return
			}
			r.handlePacket(pkt)
		}
	}
}

func (r *recorder) handlePacket(pkt *media.Packet) {
	if pkt.Type != media.PacketMedia && pkt.Type != media.PacketKeyframe {
		return // PacketConfig: the session reads CodecInfo off the Bus instead
	}
	switch {
	case pkt.Codec == media.CodecH264:
		r.handleVideo(pkt)
	case pkt.Codec == media.CodecAAC:
		r.handleAudio(pkt)
	}
}

func (r *recorder) handleVideo(pkt *media.Packet) {
	if r.inSession && r.hasVideo && jumped(r.lastDTS, pkt.DTS) {
		r.log.Info("DTS discontinuity, starting a new DVR session", "from", r.lastDTS, "to", pkt.DTS)
		r.endSession()
	}

	if !r.inSession {
		if !pkt.IsKeyframe || !r.tryStartSession(pkt.DTS, true) {
			return
		}
	} else if !r.hasVideo {
		return // session started without video; it stays audio-only
	} else if pkt.IsKeyframe && pkt.DTS-r.segStartDTS >= int64(r.opts.SegmentDurationS)*90000 {
		r.cut(pkt.DTS)
	}

	r.lastDTS = pkt.DTS
	r.videoSamples, r.pendingVideo = mux.AppendPendingVideoSample(
		r.videoSamples, r.pendingVideo, pkt.DTS, pkt.PTS, pkt.IsKeyframe, pkt.Data)
}

func (r *recorder) handleAudio(pkt *media.Packet) {
	if r.inSession && !r.hasVideo && jumped(r.lastDTS, pkt.DTS) {
		r.log.Info("DTS discontinuity, starting a new DVR session", "from", r.lastDTS, "to", pkt.DTS)
		r.endSession()
	}
	if !r.inSession {
		ci := r.stream.Bus.CodecInfo()
		if ci == nil || len(ci.SPSNALu) > 0 || ci.VideoCodec != media.CodecUnknown {
			return // a video stream: sessions start on a keyframe
		}
		if !r.tryStartSession(pkt.DTS, false) {
			return
		}
	}
	if !r.hasAudio || pkt.DTS < r.startDTS {
		// Audio from before the first keyframe has no video to go with.
		return
	}
	if !r.hasVideo {
		if pkt.DTS-r.segStartDTS >= int64(r.opts.SegmentDurationS)*90000 {
			r.cut(pkt.DTS)
		}
		r.lastDTS = pkt.DTS
	}

	if !r.audioOffset {
		// Both tracks share one session origin (startDTS ↔ videoBase), so
		// the first audio frame's tfdt is offset by exactly its DTS
		// distance from the first keyframe — that's what keeps A/V sync.
		r.audioBase = r.sessionBase + (pkt.DTS - r.startDTS)
		r.audioOffset = true
	}
	r.audioSamples = append(r.audioSamples, mux.Sample{
		DTS:      pkt.DTS,
		PTS:      pkt.PTS,
		Duration: r.audioClock.NextDuration(pkt.PTS),
		IsKey:    true,
		Data:     pkt.Data,
	})
}

// jumped reports whether dts is a discontinuity after last.
func jumped(last, dts int64) bool {
	return dts < last || dts-last > maxDTSJump
}

// tryStartSession opens a recording session at dts if the codec config
// needed for the init segment is known. onVideo is true when called for a
// keyframe.
func (r *recorder) tryStartSession(dts int64, onVideo bool) bool {
	ci := r.stream.Bus.CodecInfo()
	if ci == nil {
		return false
	}
	if ci.VideoCodec == media.CodecH265 {
		if !r.warnedHEVC {
			r.log.Warn("DVR: HEVC recording is not supported yet (no hvc1 fMP4 sample entry); not recording")
			r.warnedHEVC = true
		}
		return false
	}
	hasVideo := onVideo && len(ci.SPSNALu) > 0
	hasAudio := ci.AudioCodec == media.CodecAAC && len(ci.AACConfig) > 0
	if onVideo && !hasVideo {
		return false
	}
	if hasVideo && !hasAudio && ci.AudioCodec == media.CodecAAC {
		// Audio is coming but its config isn't parsed yet — give it a
		// moment rather than record the whole session without sound.
		now := time.Now().UnixMilli()
		if r.firstKeyWait == 0 {
			r.firstKeyWait = now
		}
		if now-r.firstKeyWait < audioWaitMS {
			return false
		}
	}
	if !hasVideo && !hasAudio {
		return false
	}
	r.startSession(ci, dts, hasVideo, hasAudio)
	return true
}

func (r *recorder) startSession(ci *media.CodecInfo, dts int64, hasVideo, hasAudio bool) {
	key := r.stream.Key
	prevSessionMS, prevEndTicks, prevDiscSeq, havePrev := r.engine.lastPosition(key)
	if r.hadSession {
		prevSessionMS = max(prevSessionMS, r.sessionMS)
		prevEndTicks = max(prevEndTicks, r.prevEndTicks)
		prevDiscSeq = max(prevDiscSeq, r.discSeq)
		havePrev = true
	}

	now := time.Now().UnixMilli()
	r.sessionMS = now
	if r.sessionMS <= prevSessionMS {
		r.sessionMS = prevSessionMS + 1
	}
	// Anchor to wall clock, but never before the previous session's end:
	// the stream's timeline must only move forward (see package doc).
	base := msToTicks(now)
	if base < prevEndTicks {
		base = prevEndTicks
	}
	r.discSeq = 0
	if havePrev {
		r.discSeq = prevDiscSeq + 1
	}

	r.inSession = true
	r.hasVideo, r.hasAudio = hasVideo, hasAudio
	r.startDTS, r.segStartDTS, r.lastDTS = dts, dts, dts
	r.sessionBase, r.videoBase, r.audioBase = base, base, base
	r.audioOffset = false
	r.sessionDir = filepath.Join(r.engine.streamDir(key), strconv.FormatInt(r.sessionMS, 10))

	var tracks []*mux.FMP4Track
	sess := sessionRow{
		SessionMS: r.sessionMS,
		Dir:       r.sessionDir,
		HasVideo:  hasVideo,
		HasAudio:  hasAudio,
	}
	if hasVideo {
		vt := &mux.FMP4Track{
			Type:      mux.TrackVideo,
			TrackID:   1,
			Timescale: 90000,
			Width:     uint16(ci.Width),
			Height:    uint16(ci.Height),
			SPS:       ci.SPSNALu,
			PPS:       ci.PPSNALu,
		}
		tracks = append(tracks, vt)
		r.videoMux = mux.NewFMP4Muxer(vt)
		sess.VideoCodecs = media.H264CodecString(ci)
		sess.Width, sess.Height = ci.Width, ci.Height
	}
	if hasAudio {
		sampleRate := mux.ParseAACSampleRate(ci.AACConfig)
		if sampleRate <= 0 {
			sampleRate = 44100
		}
		at := &mux.FMP4Track{
			Type:       mux.TrackAudio,
			TrackID:    2,
			Timescale:  90000,
			AACConfig:  ci.AACConfig,
			SampleRate: uint32(sampleRate),
			Channels:   uint8(mux.ParseAACChannels(ci.AACConfig)),
		}
		tracks = append(tracks, at)
		r.audioMux = mux.NewFMP4Muxer(at)
		r.audioClock = mux.NewAudioClock(sampleRate, 1024)
		sess.AudioCodecs = media.AACCodecString(ci)
		sess.SampleRate, sess.Channels = sampleRate, int(at.Channels)
	}

	inits := map[string][]byte{}
	var buf bytes.Buffer
	if err := mux.NewFMP4Muxer(tracks...).WriteInitSegment(&buf); err != nil {
		r.log.Error("DVR init segment", "err", err)
	}
	inits[initFileName(TrackMuxed)] = bytes.Clone(buf.Bytes())
	for _, t := range tracks {
		buf.Reset()
		if err := mux.NewFMP4Muxer(t).WriteInitSegment(&buf); err != nil {
			r.log.Error("DVR init segment", "err", err)
		}
		tr := TrackAudio
		if t.Type == mux.TrackVideo {
			tr = TrackVideo
		}
		inits[initFileName(tr)] = bytes.Clone(buf.Bytes())
	}

	r.log.Info("DVR session started", "session", r.sessionMS, "video", hasVideo, "audio", hasAudio)
	r.enqueue(func(ctx context.Context) {
		if err := os.MkdirAll(sess.Dir, 0o755); err != nil {
			r.log.Error("DVR mkdir", "err", err)
			return
		}
		for name, data := range inits {
			if err := os.WriteFile(filepath.Join(sess.Dir, name), data, 0o644); err != nil {
				r.log.Error("DVR write init", "err", err)
			}
		}
		if err := r.engine.insertSession(ctx, key, sess); err != nil {
			r.log.Error("DVR index session", "err", err)
		}
	})
}

// cut closes the open segment; nextDTS is the DTS of the first packet of
// the next one (the cutting keyframe), or -1 when the session is ending.
func (r *recorder) cut(nextDTS int64) {
	if r.pendingVideo != nil {
		var dur uint32
		switch {
		case nextDTS > 0:
			dur = uint32(nextDTS - r.pendingVideo.DTS)
		case len(r.videoSamples) > 0:
			dur = r.videoSamples[len(r.videoSamples)-1].Duration
		default:
			dur = 3000 // one frame at 30 fps
		}
		r.videoSamples, r.pendingVideo = mux.FinalizePendingVideoSample(r.videoSamples, r.pendingVideo, dur)
	}
	if len(r.videoSamples) == 0 && len(r.audioSamples) == 0 {
		return
	}

	seg := segmentRow{
		SessionMS: r.sessionMS,
		DiscSeq:   r.discSeq,
		VideoTfdt: r.videoBase,
		AudioTfdt: r.audioBase,
	}
	var buf bytes.Buffer
	if len(r.videoSamples) > 0 {
		if err := r.videoMux.WriteMediaSegment(&buf, 0, r.videoSamples, r.videoBase); err != nil {
			r.log.Warn("DVR mux video", "err", err)
		}
		for _, s := range r.videoSamples {
			seg.VideoDur += int64(s.Duration)
		}
	}
	seg.VideoBytes = int64(buf.Len())
	if len(r.audioSamples) > 0 {
		if err := r.audioMux.WriteMediaSegment(&buf, 0, r.audioSamples, r.audioBase); err != nil {
			r.log.Warn("DVR mux audio", "err", err)
		}
		for _, s := range r.audioSamples {
			seg.AudioDur += int64(s.Duration)
		}
	}
	r.videoBase += seg.VideoDur
	r.audioBase += seg.AudioDur
	r.videoSamples, r.audioSamples = nil, nil
	if nextDTS > 0 {
		r.segStartDTS = nextDTS
	}

	if r.hasVideo {
		seg.StartMS, seg.DurationMS = ticksToMS(seg.VideoTfdt), seg.VideoDur/90
	} else {
		seg.StartMS, seg.DurationMS = ticksToMS(seg.AudioTfdt), seg.AudioDur/90
	}
	seg.Size = int64(buf.Len())
	data := buf.Bytes()

	t := time.UnixMilli(seg.StartMS).UTC()
	seg.Path = filepath.Join(r.sessionDir, t.Format("2006-01-02"), fmt.Sprintf("%d.m4s", seg.StartMS))
	key := r.stream.Key
	r.enqueue(func(ctx context.Context) {
		if err := os.MkdirAll(filepath.Dir(seg.Path), 0o755); err != nil {
			r.log.Error("DVR mkdir", "err", err)
			return
		}
		if err := os.WriteFile(seg.Path, data, 0o644); err != nil {
			r.log.Error("DVR write segment", "err", err)
			return
		}
		if err := r.engine.insertSegment(ctx, key, seg); err != nil {
			r.log.Error("DVR index segment", "err", err)
			os.Remove(seg.Path)
			return
		}
		if o := r.engine.observer; o != nil {
			o.SegmentIndexed(key, seg.StartMS, seg.StartMS+seg.DurationMS)
		}
		r.engine.updateMetrics(ctx, key)
	})
}

// endSession flushes the open segment and resets session state, so the
// next usable packet starts a new session.
func (r *recorder) endSession() {
	if !r.inSession {
		return
	}
	r.cut(-1)
	r.inSession = false
	r.hadSession = true
	r.prevEndTicks = max(r.videoBase, r.audioBase)
	r.videoMux, r.audioMux, r.audioClock = nil, nil, nil
	r.videoSamples, r.pendingVideo, r.audioSamples = nil, nil, nil
	r.firstKeyWait = 0
}

func (r *recorder) enqueue(job func(context.Context)) {
	select {
	case r.jobs <- job:
	default:
		r.log.Warn("DVR write queue full (storage too slow?); dropping a segment")
	}
}

// writer runs queued disk/index jobs in order until jobs is closed.
func (r *recorder) writer(ctx context.Context, done chan struct{}) {
	defer close(done)
	for job := range r.jobs {
		job(ctx)
	}
}

// ─── index writes ─────────────────────────────────────────────────────────────

type sessionRow struct {
	SessionMS   int64
	Dir         string
	HasVideo    bool
	HasAudio    bool
	VideoCodecs string
	AudioCodecs string
	Width       int
	Height      int
	SampleRate  int
	Channels    int
}

type segmentRow struct {
	MSN        int64
	SessionMS  int64
	DiscSeq    int64
	StartMS    int64
	DurationMS int64
	Path       string
	Size       int64
	VideoBytes int64
	VideoTfdt  int64
	VideoDur   int64
	AudioTfdt  int64
	AudioDur   int64
}

// lastPosition returns where key's recording left off: its newest session,
// the end of its timeline (max tfdt+duration over both tracks) and its
// newest segment's discontinuity sequence. ok is false if nothing's
// recorded.
func (e *Engine) lastPosition(key media.StreamKey) (sessionMS, endTicks, discSeq int64, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var s, end, disc sql.NullInt64
	err := e.database.QueryRowContext(ctx, `
		SELECT MAX(session_ms),
		       MAX(MAX(video_tfdt + video_dur), MAX(audio_tfdt + audio_dur)),
		       MAX(disc_seq)
		FROM dvr_segments
		WHERE app_name = ? AND stream_name = ? AND session_ms > 0`,
		key.App, key.Name).Scan(&s, &end, &disc)
	if err != nil || !s.Valid {
		// A session can exist with no segments yet (e.g. it ended within
		// its first segment); still never reuse its id.
		var last sql.NullInt64
		e.database.QueryRowContext(ctx,
			`SELECT MAX(session_ms) FROM dvr_sessions WHERE app_name = ? AND stream_name = ?`,
			key.App, key.Name).Scan(&last)
		return last.Int64, 0, 0, false
	}
	return s.Int64, end.Int64, disc.Int64, true
}

func (e *Engine) insertSession(ctx context.Context, key media.StreamKey, s sessionRow) error {
	_, err := e.database.ExecContext(ctx, `
		INSERT OR REPLACE INTO dvr_sessions
		  (app_name, stream_name, session_ms, dir_path, has_video, has_audio,
		   video_codecs, audio_codecs, width, height, sample_rate, channels)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		key.App, key.Name, s.SessionMS, s.Dir, boolToInt(s.HasVideo), boolToInt(s.HasAudio),
		s.VideoCodecs, s.AudioCodecs, s.Width, s.Height, s.SampleRate, s.Channels)
	return err
}

// insertSegment indexes seg, assigning it the stream's next media sequence
// number in the same statement.
func (e *Engine) insertSegment(ctx context.Context, key media.StreamKey, s segmentRow) error {
	_, err := e.database.ExecContext(ctx, `
		INSERT INTO dvr_segments
		  (id, app_name, stream_name, seq, msn, session_ms, disc_seq,
		   start_pts_ms, end_pts_ms, duration_ms, file_path, size_bytes,
		   video_bytes, video_tfdt, video_dur, audio_tfdt, audio_dur)
		SELECT ?, ?, ?, 0,
		       COALESCE((SELECT MAX(msn) + 1 FROM dvr_segments
		                 WHERE app_name = ? AND stream_name = ? AND session_ms > 0), 0),
		       ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?`,
		uuid.New().String(), key.App, key.Name,
		key.App, key.Name,
		s.SessionMS, s.DiscSeq,
		s.StartMS, s.StartMS+s.DurationMS, s.DurationMS, s.Path, s.Size,
		s.VideoBytes, s.VideoTfdt, s.VideoDur, s.AudioTfdt, s.AudioDur)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
