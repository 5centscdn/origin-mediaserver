// Package hls provides the HLS packager: it reads from a MediaBus and
// produces .ts or .fmp4 segment files + M3U8 playlists served by the HTTP layer.
package hls

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
	"github.com/5centscdn/origin-mediaserver/internal/mux"
	"github.com/5centscdn/origin-mediaserver/internal/scte35"
)

// Packager consumes packets from a MediaBus and writes HLS segments.
type Packager struct {
	cfg        *config.Config
	stream     *media.Stream
	outDir     string // absolute path: <data_dir>/hls/<app>/<stream>
	playlist   *Playlist
	plRegistry *PlaylistRegistry

	// splitAudioTrack mode (see useSplitAudio) writes video and audio as
	// two fully independent playlists/segment files instead of combining
	// them into one .m4s — audioOutDir/audioPlaylist are its counterparts
	// to outDir/playlist, unused otherwise.
	splitAudioTrack bool
	audioOutDir     string
	audioPlaylist   *Playlist

	// Segment building state
	mu            sync.Mutex
	segBuf        bytes.Buffer // current segment's TS data
	segMuxer      *mux.TSMuxer
	segStartDTS   int64
	segStartWall  time.Time
	segSeq        int
	keyframeCount int

	// fMP4/CMAF state (used when cfg.HLS.Container is "fmp4" or "cmaf").
	// One combined .m4s per segment: two independent single-track muxers
	// (so each gets its own correct sequence-number space) write into the
	// same buffer back to back — see muxFMP4Fragment.
	//
	// The fmp4*Samples/fmp4PendingVideo/fmp4*BaseDT fields track whatever
	// the *current mux unit* is: with LL-HLS off, that's the whole segment
	// (flushed at keyframe+target-duration boundaries, as before); with
	// LL-HLS on, it's one part (flushed every PartDurationMs — see
	// useLLHLS/closePart/closeSegment). Either way there is exactly one
	// WriteMediaSegment call per flush, so baseDecodeTime is never double-counted.
	fmp4Video, fmp4Audio *mux.FMP4Muxer
	fmp4VideoSamples     []mux.Sample
	fmp4AudioSamples     []mux.Sample
	fmp4PendingVideo     *mux.Sample // held back until the next packet's DTS completes its Duration
	fmp4VideoBaseDT      int64       // running total, 90kHz ticks, for tfdt
	fmp4AudioBaseDT      int64
	fmp4InitWritten      bool

	// audioClock computes each AAC frame's Duration without accumulating
	// rounding drift over a long stream (see mux.AudioClock) — lazily
	// created once the real sample rate is known, in handleAudioFMP4.
	audioClock *mux.AudioClock

	// LL-HLS part-cadence state (used when useLLHLS()).
	partStartDTS int64
	partSeq      int
	segAccumBuf  bytes.Buffer // concatenation of this segment's parts so far — becomes seg%06d.m4s verbatim, no re-muxing

	// SCTE-35 state — see admarkers.go and SetSCTE35Config. scte35Mode
	// ("disabled", "passthrough", "insert", or "hybrid") gates both writing
	// received sections into segMuxer's SCTE-35 PID (TS container only) and
	// whether incoming (SourceIngest) vs. locally inserted (SourceInserted)
	// events are processed at all — see passthroughEnabled/insertEnabled.
	// adMarkerFormat ("", "none", "cue", "splicepoint", or "daterange")
	// gates HLS playlist ad-marker generation, independent of scte35Mode —
	// it controls *how* a break is rendered, not *where* events come from.
	// scteState is always created (cheap) even if unused.
	scte35Mode         string
	adMarkerFormat     string
	scteState          *scte35.StateMachine
	metrics            *metrics.Metrics // optional — see SetMetrics
	scteDuplicatesSeen int64            // last DuplicatesSuppressed() value observed, for delta-based counter increments

	// anchorDTS/anchorWall are captured once, from the first packet this
	// Packager ever processes, and used only to map a later SCTE-35 event's
	// DTS to a real wall-clock instant for EXT-X-DATERANGE — deliberately
	// separate from segStartWall (which stays time.Now()-at-segment-start,
	// unchanged, so non-SCTE-35 streams are byte-for-byte unaffected).
	anchorDTS  int64
	anchorWall time.Time

	// pending* accumulate marker data from handleSCTE35 until the next
	// segment flush (TS container path only — see flushSegmentAt), then are
	// drained onto that Segment's marker fields and reset.
	pendingCueOut      *CueOutMarker
	pendingCueIn       bool
	pendingDateRanges  []DateRange
	pendingSplicepoint string

	// activeBreak* record the OUT that started the currently-open ad break,
	// so the matching IN (which may arrive with no PTS/duration of its own)
	// can still render a complete DATERANGE END-DATE/START-DATE pair, and so
	// ad_marker_format=cue's CUE-OUT-CONT tags can keep carrying the
	// triggering splice_info_section (base64, SCTE35=...) on every segment
	// for the rest of the break, not just the one the OUT landed on.
	activeBreakStartWall time.Time
	activeBreakEventID   uint32
	activeBreakSCTE35    string

	log *slog.Logger
}

// useFMP4 reports whether this packager should produce fMP4/CMAF segments
// instead of MPEG-TS. Unrecognized Container values fall back to TS (the
// pre-existing default) rather than silently doing something unexpected.
func (p *Packager) useFMP4() bool {
	return p.cfg.HLS.Container == "fmp4" || p.cfg.HLS.Container == "cmaf"
}

// useLLHLS reports whether this packager should cut LL-HLS partial segments.
// Requires both the feature flag and fMP4 (LL-HLS parts are fMP4 fragments —
// this codebase doesn't support them over MPEG-TS).
func (p *Packager) useLLHLS() bool {
	return p.useFMP4() && p.cfg.HLS.LLHls.Enabled
}

// useSplitAudio reports whether this packager should write video and audio
// as independent playlists/segment files (see splitAudioTrack) rather than
// one combined .m4s per segment. Sticks to whatever NewPackager decided —
// unlike useFMP4/useLLHLS this isn't purely a config passthrough, since the
// ll_hls+separate_audio_track combination is rejected at construction time.
func (p *Packager) useSplitAudio() bool {
	return p.splitAudioTrack
}

// NewPackager creates an HLS packager for the given stream.
// outDir is created automatically. registry may be nil (e.g. in tests) —
// LL-HLS blocking reload just won't be able to find this stream's playlist.
func NewPackager(cfg *config.Config, stream *media.Stream, baseDataDir string, registry *PlaylistRegistry) (*Packager, error) {
	outDir := filepath.Join(baseDataDir, "hls", stream.Key.App, stream.Key.Name)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("hls mkdir: %w", err)
	}

	playlist := NewPlaylist(outDir, cfg.HLS.PlaylistWindow, cfg.HLS.SegmentDurationS)

	p := &Packager{
		cfg:        cfg,
		stream:     stream,
		outDir:     outDir,
		playlist:   playlist,
		plRegistry: registry,
		scteState:  scte35.NewStateMachine(),
		log:        slog.With("app", stream.Key.App, "stream", stream.Key.Name),
	}

	if cfg.HLS.LLHls.Enabled && !p.useFMP4() {
		p.log.Warn("ll_hls.enabled is set but hls.container is not fmp4/cmaf — LL-HLS requires fMP4, ignoring")
	}
	if p.useLLHLS() {
		playlist.EnableParts(float64(cfg.HLS.LLHls.PartDurationMs)/1000.0, float64(cfg.HLS.LLHls.HoldBackParts)*float64(cfg.HLS.LLHls.PartDurationMs)/1000.0)
	}

	if cfg.HLS.SeparateAudioTrack && p.useFMP4() {
		if cfg.HLS.LLHls.Enabled {
			p.log.Warn("separate_audio_track is set together with ll_hls.enabled — that combination isn't supported yet, ignoring separate_audio_track")
		} else {
			p.splitAudioTrack = true
			p.audioOutDir = filepath.Join(outDir, "audio")
			if err := os.MkdirAll(p.audioOutDir, 0o755); err != nil {
				return nil, fmt.Errorf("hls audio mkdir: %w", err)
			}
			p.audioPlaylist = NewPlaylist(p.audioOutDir, cfg.HLS.PlaylistWindow, cfg.HLS.SegmentDurationS)
		}
	}

	if registry != nil {
		registry.Register(stream.Key, playlist)
	}

	return p, nil
}

// SetSCTE35Config configures this Packager's SCTE-35 behavior — the
// per-stream resolved values of the outputs.scte35_mode and
// outputs.hls.ad_marker_format config fields (see internal/db). Call once,
// before Run; a zero value ("", "") leaves this Packager's output
// byte-for-byte identical to before this feature existed. mode must be one
// of "disabled", "passthrough", "insert", or "hybrid" (anything else
// behaves like "disabled"); adMarkerFormat must be one of "", "none",
// "cue", "splicepoint", or "daterange" — an unrecognized value behaves like
// "none" (see handleSCTE35).
func (p *Packager) SetSCTE35Config(mode, adMarkerFormat string) {
	p.scte35Mode = mode
	p.adMarkerFormat = adMarkerFormat
}

// passthroughEnabled reports whether this stream's mode allows forwarding
// incoming (SourceIngest) SCTE-35 to MPEG-TS output / HLS markers.
func (p *Packager) passthroughEnabled() bool {
	return p.scte35Mode == "passthrough" || p.scte35Mode == "hybrid"
}

// insertEnabled reports whether this stream's mode allows processing
// locally-inserted (SourceInserted) SCTE-35 — note this only gates whether
// *this Packager* forwards/renders such events; internal/scte35ctrl itself
// separately refuses to insert at all for a non-insert/hybrid stream, so
// this is a defense-in-depth check, not the primary gate.
func (p *Packager) insertEnabled() bool {
	return p.scte35Mode == "insert" || p.scte35Mode == "hybrid"
}

// SetMetrics attaches a metrics recorder for SCTE-35 counters (see
// admarkers.go). Optional — a nil or never-called Packager simply doesn't
// record them.
func (p *Packager) SetMetrics(m *metrics.Metrics) {
	p.metrics = m
}

// Run subscribes to the stream bus and packages packets until ctx is done.
func (p *Packager) Run(ctx context.Context) {
	sub := p.stream.Bus.Subscribe(ctx)
	defer sub.Close()
	if p.plRegistry != nil {
		defer p.plRegistry.Unregister(p.stream.Key)
	}

	p.log.Info("HLS packager started", "dir", p.outDir)

	for {
		select {
		case <-ctx.Done():
			p.flushSegment(true)
			return

		case pkt, ok := <-sub.Chan():
			if !ok {
				p.flushSegment(true)
				return
			}
			if err := p.handlePacket(pkt); err != nil {
				p.log.Warn("packet error", "err", err)
			}
		}
	}
}

func (p *Packager) handlePacket(pkt *media.Packet) error {
	if pkt.Type == media.PacketEnd {
		p.flushSegment(true)
		return nil
	}

	if pkt.Codec.IsVideo() {
		return p.handleVideo(pkt)
	}
	if pkt.Codec.IsAudio() {
		return p.handleAudio(pkt)
	}
	if pkt.Codec == media.CodecSCTE35 {
		return p.handleSCTE35(pkt)
	}
	return nil
}

func (p *Packager) handleVideo(pkt *media.Packet) error {
	if p.useFMP4() {
		return p.handleVideoFMP4(pkt)
	}

	// Codec config packet: store SPS/PPS for muxer init (RTMP's
	// out-of-band AVCDecoderConfigurationRecord).
	if pkt.Type == media.PacketConfig {
		if ci := p.stream.Bus.CodecInfo(); ci != nil {
			p.initMuxer(ci)
		}
		return nil
	}

	if p.segMuxer == nil {
		// Sources with only in-band parameter sets (WHIP, MPEG-TS ingest —
		// no separate out-of-band config packet) never send a
		// PacketConfig; lazily init from CodecInfo directly instead, once
		// it's been populated by the SPS NAL that must precede any real
		// keyframe. Mirrors handleVideoFMP4/ensureInit's same lazy pattern.
		if ci := p.stream.Bus.CodecInfo(); ci != nil && len(ci.SPSNALu) > 0 {
			p.initMuxer(ci)
		}
		if p.segMuxer == nil {
			return nil // still waiting for codec config
		}
	}

	// Cut a new segment at every keyframe once we've accumulated enough duration.
	// segStartDTS is -1 until the first keyframe; > 0 while a segment is open.
	if pkt.IsKeyframe {
		if p.segStartDTS > 0 {
			durationMs := (pkt.DTS - p.segStartDTS) / 90
			if durationMs >= int64(p.cfg.HLS.SegmentDurationS*1000) {
				if err := p.flushSegmentAt(pkt.DTS); err != nil {
					return err
				}
			}
		}

		// Start a new segment at this keyframe (flushSegment resets segStartDTS to -1).
		if p.segStartDTS <= 0 {
			p.segStartDTS = pkt.DTS
			p.segStartWall = time.Now()
		}
	}

	// Drop non-keyframe packets that arrive before the first keyframe.
	if p.segStartDTS <= 0 {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.segMuxer.WriteVideo(pkt.Data, pkt.DTS, pkt.PTS, pkt.IsKeyframe); err != nil {
		return fmt.Errorf("ts mux video: %w", err)
	}
	return nil
}

func (p *Packager) handleAudio(pkt *media.Packet) error {
	if p.useFMP4() {
		return p.handleAudioFMP4(pkt)
	}

	if pkt.Type != media.PacketMedia {
		return nil // skip config / end packets
	}

	// Icecast is audio-only by protocol — there's no video keyframe to
	// drive segMuxer init or segment cuts off of, unlike every other
	// ingest source. Handled as its own branch rather than folding into
	// the logic below, which is entirely keyframe/segStartDTS-from-video
	// shaped.
	if p.stream.SourceType == "icecast" {
		return p.handleAudioOnlyTS(pkt)
	}

	if p.segMuxer == nil || p.segStartDTS <= 0 {
		return nil // wait until a segment is open
	}

	// The TS muxer's PMT audio stream_type is fixed at construction (AAC
	// unless MP3/AC3, see initMuxer) — an unsupported codec (e.g. Opus, from
	// a WHIP-ingested stream) would otherwise get muxed as raw bytes into a
	// PES the PMT still claims is AAC, corrupting playback for any demuxer
	// that trusts it. Skip audio entirely rather than lie about the codec —
	// mirrors initFMP4Muxers already omitting the audio track outright when
	// CodecInfo doesn't describe a codec that path supports.
	switch pkt.Codec {
	case media.CodecAAC, media.CodecMP3, media.CodecAC3:
	default:
		return nil
	}

	// Convert raw AAC to ADTS if needed.
	audioData := pkt.Data
	if pkt.Codec == media.CodecAAC {
		audioData = mux.WrapADTS(pkt.Data, aacConfigOf(p.stream.Bus.CodecInfo()))
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	return p.segMuxer.WriteAudio(audioData, pkt.PTS) // PTS already in 90 kHz ticks
}

// handleAudioOnlyTS is handleAudio's counterpart for sources with no video
// track at all (Icecast): it owns muxer init and segment-cut-by-duration
// itself instead of piggybacking on handleVideo's keyframe-driven logic.
func (p *Packager) handleAudioOnlyTS(pkt *media.Packet) error {
	switch pkt.Codec {
	case media.CodecAAC, media.CodecMP3:
	default:
		return nil // Icecast only ever produces these two; anything else is unexpected
	}

	if p.segMuxer == nil {
		ci := p.stream.Bus.CodecInfo()
		if ci == nil {
			return nil // wait for the first frame's CodecInfo
		}
		p.initAudioOnlyMuxer(ci)
	}

	if p.segStartDTS <= 0 {
		p.segStartDTS = pkt.DTS
		p.segStartWall = time.Now()
	} else if durationMs := (pkt.DTS - p.segStartDTS) / 90; durationMs >= int64(p.cfg.HLS.SegmentDurationS*1000) {
		if err := p.flushSegmentAt(pkt.DTS); err != nil {
			return err
		}
		p.segStartDTS = pkt.DTS
		p.segStartWall = time.Now()
	}

	audioData := pkt.Data
	if pkt.Codec == media.CodecAAC {
		audioData = mux.WrapADTS(pkt.Data, aacConfigOf(p.stream.Bus.CodecInfo()))
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	return p.segMuxer.WriteAudio(audioData, pkt.PTS)
}

// initAudioOnlyMuxer is initMuxer's counterpart for audio-only sources —
// builds a TSMuxer with mux.NewTSMuxerAudioOnly instead, so the PMT never
// declares a video ES entry that will never carry a single packet.
func (p *Packager) initAudioOnlyMuxer(ci *media.CodecInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.segMuxer != nil {
		return
	}

	audioStreamType := mux.StreamTypeAAC
	if ci.AudioCodec == media.CodecMP3 {
		audioStreamType = mux.StreamTypeMP3
	}

	p.segMuxer = mux.NewTSMuxerAudioOnly(&p.segBuf, uint8(audioStreamType))
	if p.passthroughEnabled() || p.insertEnabled() {
		p.segMuxer.EnableSCTE35()
	}
	p.segStartDTS = -1
	p.log.Debug("TS muxer initialized (audio-only)", "audio", ci.AudioCodec)
}

// handleVideoFMP4 mirrors handleVideo's keyframe/target-duration segment-cut
// logic (same segStartDTS/segStartWall bookkeeping, shared with the TS path)
// but buffers samples for a batched WriteMediaSegment call instead of
// streaming into a per-packet TS muxer — see muxFMP4Fragment. When
// useLLHLS(), it also cuts a partial segment (LL-HLS "part") every
// PartDurationMs, independent of the keyframe-anchored segment cut.
func (p *Packager) handleVideoFMP4(pkt *media.Packet) error {
	if pkt.Type == media.PacketConfig {
		return nil // CodecInfo is already on the Bus; tracks are built lazily at first flush
	}

	if pkt.IsKeyframe {
		if p.segStartDTS > 0 {
			durationMs := (pkt.DTS - p.segStartDTS) / 90
			if durationMs >= int64(p.cfg.HLS.SegmentDurationS*1000) {
				if err := p.closeSegmentAny(pkt.DTS); err != nil {
					return err
				}
			}
		}
		if p.segStartDTS <= 0 {
			p.segStartDTS = pkt.DTS
			p.segStartWall = time.Now()
			p.partStartDTS = pkt.DTS
		}
	} else if p.useLLHLS() && p.partStartDTS > 0 {
		durationMs := (pkt.DTS - p.partStartDTS) / 90
		if durationMs >= int64(p.cfg.HLS.LLHls.PartDurationMs) {
			if err := p.closePart(pkt.DTS); err != nil {
				return err
			}
		}
	}

	if p.segStartDTS <= 0 {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.fmp4VideoSamples, p.fmp4PendingVideo = mux.AppendPendingVideoSample(
		p.fmp4VideoSamples, p.fmp4PendingVideo, pkt.DTS, pkt.PTS, pkt.IsKeyframe, pkt.Data)
	return nil
}

// handleAudioFMP4 appends one AAC frame to the audio sample buffer. Unlike
// video, audio doesn't need delta-based duration in the sense of holding a
// sample back: AAC-LC frames are a fixed 1024 samples each, so the nominal
// duration is known immediately from the sample rate. But a duration
// computed fresh from that fixed rate every single frame silently
// accumulates rounding drift over a long stream — see mux.AudioClock,
// which this delegates to specifically to avoid that. Raw AAC is used
// as-is (no ADTS wrapping — that framing is MPEG-TS-specific; fMP4 players
// get the codec config from the init segment's AudioSpecificConfig
// instead).
func (p *Packager) handleAudioFMP4(pkt *media.Packet) error {
	if pkt.Type != media.PacketMedia {
		return nil
	}
	if p.segStartDTS <= 0 {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.audioClock == nil {
		sampleRate := 44100 // fallback, used only if the real sample rate can't be determined yet
		if ci := p.stream.Bus.CodecInfo(); ci != nil {
			if sr := mux.ParseAACSampleRate(ci.AACConfig); sr > 0 {
				sampleRate = sr
			}
		}
		p.audioClock = mux.NewAudioClock(sampleRate, 1024)
	}
	duration := p.audioClock.NextDuration(pkt.PTS)

	p.fmp4AudioSamples = append(p.fmp4AudioSamples, mux.Sample{
		DTS:      pkt.DTS,
		PTS:      pkt.PTS,
		Duration: duration,
		IsKey:    true,
		Data:     pkt.Data,
	})
	return nil
}

// flushSegment finalizes the current segment buffer and writes it to disk.
// nextDTS is the DTS of the first packet of the next segment (used for accurate duration).
// Pass -1 when flushing the last segment of a stream.
func (p *Packager) flushSegment(isLast bool) error {
	if p.useFMP4() {
		return p.closeSegmentAny(-1)
	}
	return p.flushSegmentAt(-1)
}

func (p *Packager) flushSegmentAt(nextDTS int64) error {
	p.mu.Lock()
	if p.segBuf.Len() == 0 {
		p.mu.Unlock()
		return nil
	}

	segData := make([]byte, p.segBuf.Len())
	copy(segData, p.segBuf.Bytes())
	p.segBuf.Reset()

	seq := p.segSeq
	p.segSeq++

	startDTS := p.segStartDTS
	startWall := p.segStartWall
	p.segStartDTS = -1
	p.mu.Unlock()

	// Use DTS delta for accurate duration; fall back to configured target.
	durationS := float64(p.cfg.HLS.SegmentDurationS)
	if nextDTS > 0 && startDTS > 0 {
		durationS = float64(nextDTS-startDTS) / 90000.0
	}

	segName := fmt.Sprintf("seg%06d.ts", seq)
	segPath := filepath.Join(p.outDir, segName)
	if err := os.WriteFile(segPath, segData, 0o644); err != nil {
		return fmt.Errorf("write segment %s: %w", segPath, err)
	}

	markers := p.drainPendingMarkers()

	p.playlist.AddSegment(Segment{
		Seq:         seq,
		URI:         segName,
		DurationS:   durationS,
		WallTime:    startWall,
		CueOut:      markers.cueOut,
		CueOutCont:  markers.cueOutCont,
		CueIn:       markers.cueIn,
		DateRanges:  markers.dateRanges,
		Splicepoint: markers.splicepoint,
	})

	p.log.Debug("segment written", "seq", seq, "size", len(segData), "dur_s", fmt.Sprintf("%.3f", durationS))
	return nil
}

func (p *Packager) initMuxer(ci *media.CodecInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.segMuxer != nil {
		return // already initialized
	}

	videoStreamType := mux.StreamTypeH264
	switch ci.VideoCodec {
	case media.CodecH265:
		videoStreamType = mux.StreamTypeH265
	}

	audioStreamType := mux.StreamTypeAAC
	switch ci.AudioCodec {
	case media.CodecMP3:
		audioStreamType = mux.StreamTypeMP3
	case media.CodecAC3:
		audioStreamType = mux.StreamTypeAC3
	}

	p.segMuxer = mux.NewTSMuxer(&p.segBuf, uint8(videoStreamType), uint8(audioStreamType))
	if p.passthroughEnabled() || p.insertEnabled() {
		p.segMuxer.EnableSCTE35()
	}

	if len(ci.SPSNALu) > 0 {
		if ci.VideoCodec == media.CodecH265 {
			p.segMuxer.SetHEVCParamSets(ci.VPSNALu, ci.SPSNALu, ci.PPSNALu)
		} else {
			p.segMuxer.SetSPSPPS(ci.SPSNALu, ci.PPSNALu)
		}
	}

	p.segStartDTS = -1
	p.log.Debug("TS muxer initialized", "video", ci.VideoCodec, "audio", ci.AudioCodec)
}

// ─── fMP4 / CMAF ──────────────────────────────────────────────────────────────

// muxFMP4Fragment finalizes the pending video sample (using nextDTS to
// compute its exact Duration via delta, or a fallback if nextDTS <= 0 — the
// very last fragment of the stream, with no next packet to measure from)
// and muxes the buffered video+audio samples into one combined byte slice
// (two independent moof+mdat pairs — see the Packager.fmp4Video/fmp4Audio
// doc comment). Lazily builds the fMP4 muxers + writes init.mp4 on its first
// call. Returns (nil, nil) if there's nothing buffered — a harmless no-op.
func (p *Packager) muxFMP4Fragment(nextDTS int64) ([]byte, error) {
	p.mu.Lock()

	if !p.fmp4InitWritten {
		if err := p.initFMP4Muxers(); err != nil {
			p.mu.Unlock()
			p.log.Warn("fmp4 init segment not ready yet", "err", err)
			return nil, nil
		}
	}

	if p.fmp4PendingVideo != nil {
		var dur uint32
		switch {
		case nextDTS > 0:
			dur = uint32(nextDTS - p.fmp4PendingVideo.DTS)
		case len(p.fmp4VideoSamples) > 0:
			dur = p.fmp4VideoSamples[len(p.fmp4VideoSamples)-1].Duration
		default:
			dur = uint32(p.cfg.HLS.SegmentDurationS) * 90000
		}
		p.fmp4VideoSamples, p.fmp4PendingVideo = mux.FinalizePendingVideoSample(p.fmp4VideoSamples, p.fmp4PendingVideo, dur)
	}

	if len(p.fmp4VideoSamples) == 0 && len(p.fmp4AudioSamples) == 0 {
		p.mu.Unlock()
		return nil, nil
	}

	videoSamples := p.fmp4VideoSamples
	audioSamples := p.fmp4AudioSamples
	p.fmp4VideoSamples = nil
	p.fmp4AudioSamples = nil

	videoMuxer := p.fmp4Video
	audioMuxer := p.fmp4Audio
	videoBaseDT := p.fmp4VideoBaseDT
	audioBaseDT := p.fmp4AudioBaseDT

	p.mu.Unlock()

	var buf bytes.Buffer
	var totalVideoDur, totalAudioDur int64

	if videoMuxer != nil && len(videoSamples) > 0 {
		if err := videoMuxer.WriteMediaSegment(&buf, 0, videoSamples, videoBaseDT); err != nil {
			return nil, fmt.Errorf("fmp4 mux video: %w", err)
		}
		for _, s := range videoSamples {
			totalVideoDur += int64(s.Duration)
		}
	}
	if audioMuxer != nil && len(audioSamples) > 0 {
		if err := audioMuxer.WriteMediaSegment(&buf, 0, audioSamples, audioBaseDT); err != nil {
			return nil, fmt.Errorf("fmp4 mux audio: %w", err)
		}
		for _, s := range audioSamples {
			totalAudioDur += int64(s.Duration)
		}
	}

	p.mu.Lock()
	p.fmp4VideoBaseDT += totalVideoDur
	p.fmp4AudioBaseDT += totalAudioDur
	p.mu.Unlock()

	return buf.Bytes(), nil
}

// closePart flushes the samples buffered since the last part (or segment
// start) as one mux fragment: mux it (see muxFMP4Fragment), append its bytes
// to the in-progress segment's accumulator (segAccumBuf — the eventual full
// segment file is exactly this concatenation, no re-muxing needed), and —
// only when useLLHLS() — also write it as its own small LL-HLS part file and
// record it on the playlist. With LL-HLS off this still runs (once per
// segment, from closeSegment) purely to produce the accumulator content;
// it just skips the extra part file/playlist entry nothing would ever read.
func (p *Packager) closePart(nextDTS int64) error {
	data, err := p.muxFMP4Fragment(nextDTS)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}

	p.mu.Lock()
	p.segAccumBuf.Write(data)
	seq := p.segSeq
	partIdx := p.partSeq
	p.partSeq++
	partStart := p.partStartDTS
	p.partStartDTS = nextDTS
	p.mu.Unlock()

	if !p.useLLHLS() {
		return nil
	}

	partDurationS := float64(p.cfg.HLS.LLHls.PartDurationMs) / 1000.0
	if nextDTS > 0 && partStart > 0 {
		partDurationS = float64(nextDTS-partStart) / 90000.0
	}

	partName := fmt.Sprintf("seg%06d_part%03d.m4s", seq, partIdx)
	partPath := filepath.Join(p.outDir, partName)
	if err := os.WriteFile(partPath, data, 0o644); err != nil {
		return fmt.Errorf("write part %s: %w", partPath, err)
	}

	p.playlist.AddPart(seq, Part{URI: partName, DurationS: partDurationS, Independent: partIdx == 0})
	p.updatePreloadHint()
	return nil
}

// updatePreloadHint tells the playlist what the next LL-HLS part's filename
// will be, from the segment/part counters' current state — called right
// after they've advanced (end of closePart/closeSegment), so the hint
// always points at the part that's about to start accumulating, not the
// one that was just written.
func (p *Packager) updatePreloadHint() {
	p.mu.Lock()
	seq := p.segSeq
	partIdx := p.partSeq
	p.mu.Unlock()
	p.playlist.SetPreloadHint(fmt.Sprintf("seg%06d_part%03d.m4s", seq, partIdx))
}

// closeSegment finalizes the segment currently being built: first closes out
// whatever part is still in progress (so no buffered samples are lost), then
// writes segAccumBuf's contents — the concatenation of all this segment's
// parts — as the traditional whole-segment file and records it on the
// playlist. nextDTS is the DTS of the packet that triggered this close (the
// new segment's first keyframe); pass -1 when closing the very last segment
// of a stream.
func (p *Packager) closeSegment(nextDTS int64) error {
	if err := p.closePart(nextDTS); err != nil {
		return err
	}

	p.mu.Lock()
	if p.segAccumBuf.Len() == 0 {
		p.mu.Unlock()
		return nil
	}
	data := make([]byte, p.segAccumBuf.Len())
	copy(data, p.segAccumBuf.Bytes())
	p.segAccumBuf.Reset()

	seq := p.segSeq
	p.segSeq++
	p.partSeq = 0

	startDTS := p.segStartDTS
	startWall := p.segStartWall
	p.segStartDTS = -1
	p.mu.Unlock()

	durationS := float64(p.cfg.HLS.SegmentDurationS)
	if nextDTS > 0 && startDTS > 0 {
		durationS = float64(nextDTS-startDTS) / 90000.0
	}

	segName := fmt.Sprintf("seg%06d.m4s", seq)
	segPath := filepath.Join(p.outDir, segName)
	if err := os.WriteFile(segPath, data, 0o644); err != nil {
		return fmt.Errorf("write segment %s: %w", segPath, err)
	}

	p.playlist.AddSegment(Segment{
		Seq:       seq,
		URI:       segName,
		DurationS: durationS,
		WallTime:  startWall,
	})
	if p.useLLHLS() {
		// closePart (above) already updated the hint once, using the
		// pre-reset seq/partSeq — recompute now that they've rolled over
		// to the new segment, or that stale hint would point at a part
		// index that will never be written under this segment's number.
		p.updatePreloadHint()
	}

	p.log.Debug("fmp4 segment written", "seq", seq, "size", len(data), "dur_s", fmt.Sprintf("%.3f", durationS))
	return nil
}

// closeSegmentAny dispatches to closeSegment or closeSegmentSplit depending
// on useSplitAudio() — the two call sites (handleVideoFMP4's keyframe cut,
// flushSegment's end-of-stream flush) don't need to know which mode is active.
func (p *Packager) closeSegmentAny(nextDTS int64) error {
	if p.useSplitAudio() {
		return p.closeSegmentSplit(nextDTS)
	}
	return p.closeSegment(nextDTS)
}

// closeSegmentSplit is closeSegment's counterpart for split-audio-track mode
// (useSplitAudio()): video and audio are muxed and written as two fully
// independent segment files + playlist entries — mirroring dash.Packager's
// existing dual-track structure — instead of one combined .m4s. LL-HLS
// parts are out of scope for this mode (rejected in NewPackager), so unlike
// closeSegment this never goes through closePart/segAccumBuf: there is
// exactly one flush per segment, straight from the sample buffers.
func (p *Packager) closeSegmentSplit(nextDTS int64) error {
	p.mu.Lock()

	if !p.fmp4InitWritten {
		if err := p.initFMP4MuxersSplit(); err != nil {
			p.mu.Unlock()
			p.log.Warn("fmp4 split init segments not ready yet", "err", err)
			return nil
		}
	}

	if p.fmp4PendingVideo != nil {
		var dur uint32
		switch {
		case nextDTS > 0:
			dur = uint32(nextDTS - p.fmp4PendingVideo.DTS)
		case len(p.fmp4VideoSamples) > 0:
			dur = p.fmp4VideoSamples[len(p.fmp4VideoSamples)-1].Duration
		default:
			dur = uint32(p.cfg.HLS.SegmentDurationS) * 90000
		}
		p.fmp4VideoSamples, p.fmp4PendingVideo = mux.FinalizePendingVideoSample(p.fmp4VideoSamples, p.fmp4PendingVideo, dur)
	}

	if len(p.fmp4VideoSamples) == 0 && len(p.fmp4AudioSamples) == 0 {
		p.mu.Unlock()
		return nil
	}

	videoSamples := p.fmp4VideoSamples
	audioSamples := p.fmp4AudioSamples
	p.fmp4VideoSamples = nil
	p.fmp4AudioSamples = nil

	videoMuxer := p.fmp4Video
	audioMuxer := p.fmp4Audio
	videoBaseDT := p.fmp4VideoBaseDT
	audioBaseDT := p.fmp4AudioBaseDT

	seq := p.segSeq
	p.segSeq++

	startDTS := p.segStartDTS
	startWall := p.segStartWall
	p.segStartDTS = -1
	p.mu.Unlock()

	videoDurationS := float64(p.cfg.HLS.SegmentDurationS)
	if nextDTS > 0 && startDTS > 0 {
		videoDurationS = float64(nextDTS-startDTS) / 90000.0
	}

	var totalVideoDur, totalAudioDur int64

	if videoMuxer != nil && len(videoSamples) > 0 {
		var buf bytes.Buffer
		if err := videoMuxer.WriteMediaSegment(&buf, 0, videoSamples, videoBaseDT); err != nil {
			return fmt.Errorf("fmp4 mux video: %w", err)
		}
		segName := fmt.Sprintf("seg%06d.m4s", seq)
		if err := os.WriteFile(filepath.Join(p.outDir, segName), buf.Bytes(), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", segName, err)
		}
		for _, s := range videoSamples {
			totalVideoDur += int64(s.Duration)
		}
		p.playlist.AddSegment(Segment{Seq: seq, URI: segName, DurationS: videoDurationS, WallTime: startWall})
	}

	if audioMuxer != nil && len(audioSamples) > 0 {
		var buf bytes.Buffer
		if err := audioMuxer.WriteMediaSegment(&buf, 0, audioSamples, audioBaseDT); err != nil {
			return fmt.Errorf("fmp4 mux audio: %w", err)
		}
		segName := fmt.Sprintf("seg%06d.m4s", seq)
		if err := os.WriteFile(filepath.Join(p.audioOutDir, segName), buf.Bytes(), 0o644); err != nil {
			return fmt.Errorf("write audio/%s: %w", segName, err)
		}
		// Audio's own accumulated sample duration, not the video segment's
		// durationS — audio frame boundaries don't line up exactly with
		// video keyframe boundaries, so the two tracks' segments are
		// naturally slightly different lengths (same approximation
		// dash.Packager already makes).
		var audioDurationS float64
		for _, s := range audioSamples {
			totalAudioDur += int64(s.Duration)
			audioDurationS += float64(s.Duration) / 90000.0
		}
		p.audioPlaylist.AddSegment(Segment{Seq: seq, URI: segName, DurationS: audioDurationS})
	}

	p.mu.Lock()
	p.fmp4VideoBaseDT += totalVideoDur
	p.fmp4AudioBaseDT += totalAudioDur
	p.mu.Unlock()

	p.log.Debug("fmp4 split segments written", "seq", seq, "video", videoMuxer != nil, "audio", audioMuxer != nil)
	return nil
}

// initFMP4MuxersSplit is initFMP4Muxers' counterpart for split-audio-track
// mode: builds the same two single-track FMP4Muxers, but writes each one's
// own init segment separately (init.mp4 in outDir, audio/init.mp4 in
// audioOutDir) instead of one combined init.mp4 describing both tracks —
// mirrors dash.Packager.ensureInit's identical approach. Called with p.mu
// already held.
func (p *Packager) initFMP4MuxersSplit() error {
	ci := p.stream.Bus.CodecInfo()
	if ci == nil {
		return fmt.Errorf("no codec info available yet")
	}

	hasVideo := len(ci.SPSNALu) > 0
	hasAudio := len(ci.AACConfig) > 0
	if !hasVideo && !hasAudio {
		return fmt.Errorf("no video or audio codec info available yet")
	}

	if hasVideo {
		videoTrack := &mux.FMP4Track{
			Type:      mux.TrackVideo,
			TrackID:   1,
			Timescale: 90000,
			Width:     uint16(ci.Width),
			Height:    uint16(ci.Height),
			SPS:       ci.SPSNALu,
			PPS:       ci.PPSNALu,
		}
		p.fmp4Video = mux.NewFMP4Muxer(videoTrack)

		var buf bytes.Buffer
		if err := p.fmp4Video.WriteInitSegment(&buf); err != nil {
			return fmt.Errorf("write init.mp4: %w", err)
		}
		if err := os.WriteFile(filepath.Join(p.outDir, "init.mp4"), buf.Bytes(), 0o644); err != nil {
			return fmt.Errorf("write init.mp4: %w", err)
		}
		p.playlist.SetInitSegment("init.mp4")
	}

	if hasAudio {
		audioTrack := &mux.FMP4Track{
			Type:       mux.TrackAudio,
			TrackID:    2,
			Timescale:  90000,
			AACConfig:  ci.AACConfig,
			SampleRate: uint32(mux.ParseAACSampleRate(ci.AACConfig)),
			Channels:   uint8(mux.ParseAACChannels(ci.AACConfig)),
		}
		p.fmp4Audio = mux.NewFMP4Muxer(audioTrack)

		var buf bytes.Buffer
		if err := p.fmp4Audio.WriteInitSegment(&buf); err != nil {
			return fmt.Errorf("write audio/init.mp4: %w", err)
		}
		if err := os.WriteFile(filepath.Join(p.audioOutDir, "init.mp4"), buf.Bytes(), 0o644); err != nil {
			return fmt.Errorf("write audio/init.mp4: %w", err)
		}
		p.audioPlaylist.SetInitSegment("init.mp4")
	}

	p.fmp4InitWritten = true
	p.log.Info("fMP4 split init segments written", "video", hasVideo, "audio", hasAudio)
	return nil
}

// initFMP4Muxers builds the video/audio track muxers and writes init.mp4
// once, using whatever CodecInfo is available at the time of the first
// flush. Called with p.mu already held. Tracks with no codec info available
// (e.g. a video-only stream never sends AACConfig) are simply omitted —
// not an error. Like the TS path's initMuxer, this never re-runs if codec
// parameters change mid-stream (FMP4Muxer has no re-init support; splicing
// in a new init segment + EXT-X-DISCONTINUITY is a separate future need).
func (p *Packager) initFMP4Muxers() error {
	ci := p.stream.Bus.CodecInfo()
	if ci == nil {
		return fmt.Errorf("no codec info available yet")
	}

	var tracks []*mux.FMP4Track

	if len(ci.SPSNALu) > 0 {
		videoTrack := &mux.FMP4Track{
			Type:      mux.TrackVideo,
			TrackID:   1,
			Timescale: 90000,
			Width:     uint16(ci.Width),
			Height:    uint16(ci.Height),
			SPS:       ci.SPSNALu,
			PPS:       ci.PPSNALu,
		}
		tracks = append(tracks, videoTrack)
		p.fmp4Video = mux.NewFMP4Muxer(videoTrack)
	}

	if len(ci.AACConfig) > 0 {
		audioTrack := &mux.FMP4Track{
			Type:       mux.TrackAudio,
			TrackID:    2,
			Timescale:  90000,
			AACConfig:  ci.AACConfig,
			SampleRate: uint32(mux.ParseAACSampleRate(ci.AACConfig)),
			Channels:   uint8(mux.ParseAACChannels(ci.AACConfig)),
		}
		tracks = append(tracks, audioTrack)
		p.fmp4Audio = mux.NewFMP4Muxer(audioTrack)
	}

	if len(tracks) == 0 {
		return fmt.Errorf("no video or audio codec info available yet")
	}

	var buf bytes.Buffer
	if err := mux.NewFMP4Muxer(tracks...).WriteInitSegment(&buf); err != nil {
		return fmt.Errorf("write init segment: %w", err)
	}
	if err := os.WriteFile(filepath.Join(p.outDir, "init.mp4"), buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write init.mp4: %w", err)
	}

	p.playlist.SetInitSegment("init.mp4")
	p.fmp4InitWritten = true
	p.log.Info("fMP4 init segment written", "video", p.fmp4Video != nil, "audio", p.fmp4Audio != nil)
	return nil
}

// aacConfigOf nil-safely extracts AACConfig for mux.WrapADTS, which takes
// the raw bytes rather than *media.CodecInfo (see WrapADTS's doc comment).
func aacConfigOf(ci *media.CodecInfo) []byte {
	if ci == nil {
		return nil
	}
	return ci.AACConfig
}

