package dash

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/captiondecode"
	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/ebuttd"
	"github.com/5centscdn/origin-mediaserver/internal/livestore"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/mux"
	"github.com/5centscdn/origin-mediaserver/internal/teletext"
	"github.com/5centscdn/origin-mediaserver/internal/webvtt"
)

// Packager consumes packets from a MediaBus and writes DASH segments: unlike
// HLS's combined-file fMP4 shortcut, DASH's AdaptationSet model mandates
// genuinely separate video/audio init and media segments — two independent
// single-track FMP4Muxers, two independent file sequences, no
// byte-concatenation needed.
//
// Single-rendition only (mirrors how hls.Packager packages one Stream at a
// time) — a combined multi-bitrate MPD across an ABR group's variants is out
// of scope for this stage.
type Packager struct {
	cfg    *config.Config
	stream *media.Stream
	outDir string // absolute path: <data_dir>/dash/<app>/<stream>

	mu           sync.Mutex
	fmp4Video    *mux.FMP4Muxer
	fmp4Audio    *mux.FMP4Muxer
	videoSamples []mux.Sample
	audioSamples []mux.Sample
	pendingVideo *mux.Sample // held back until the next packet's DTS completes its Duration
	videoBaseDT  int64       // running total, 90kHz ticks, for tfdt
	audioBaseDT  int64
	// tsOrigin is the source DTS that timeline position 0 stands for (the
	// first segment's keyframe, set once haveOrigin). audioPos is where the next
	// audio sample lands on the timeline. handleAudio compares the two with
	// each frame's real DTS so a gap in the source's audio stretches the
	// timeline instead of pulling every later audio segment earlier than
	// the video segment with the same number: summing nominal frame
	// durations alone drifted ~6 s in 100 min on a pull from the internet,
	// and players stall on audio and video that far apart.
	tsOrigin    int64
	haveOrigin  bool
	audioPos    int64
	segSeq      int
	segStartDTS int64
	initWritten bool

	// EBU-TT-D subtitle track (internal/captiondecode + internal/ebuttd),
	// opt-in via cfg.DASH.DVB.SubtitlesEnabled. Piggybacks video/audio's own
	// segSeq/segStartDTS clock — a text segment is cut alongside every
	// video/audio segment, not on its own independent timer, since captions
	// arrive embedded in the video's own SEI data with no schedule of their
	// own to speak of.
	fmp4Text      *mux.FMP4Muxer
	textExtractor *captiondecode.Extractor
	// teletextDecoder decodes DVB Teletext subtitles (internal/teletext) —
	// an independent source from textExtractor's CEA-608/708, fed from its
	// own dedicated media.CodecDVBTeletext packets rather than SEI bytes in
	// video packets (see handlePacket). Both write into the same textCues;
	// a real stream only ever produces packets for whichever source it
	// actually has, so having both active whenever subtitles are enabled is
	// harmless — see SetDVBConfig.
	teletextDecoder  *teletext.Decoder
	subtitlesEnabled bool                // remembers SetDVBConfig's flag until CodecInfo tells us the teletext page to decode (see handleTeletext)
	textCues         []captiondecode.Cue // accumulates until pruned in closeSegment
	textBaseDT       int64
	lastVideoDTS     int64 // for a best-effort final caption flush at stream end

	// dvbProfile mirrors config.DVBConfig.Enabled, resolved per-app/stream
	// (see SetDVBConfig) rather than read directly from global cfg — same
	// setter-after-construction pattern SetSCTE35Config already uses for
	// its own per-stream-resolved knobs.
	dvbProfile bool

	// videoTimeline/audioTimeline/textTimeline back the manifest's real
	// <SegmentTimeline> (see mpd.SegmentTimelineEntry's doc comment) — a
	// bounded rolling window of each track's most recently produced
	// segments' real start/duration, rewritten to manifest.mpd on every
	// segment cut (mirrors internal/hls/playlist.go's own per-segment
	// rewrite-to-disk pattern). manifestParams holds the static fields
	// (codecs, DVB signaling, etc.) set once in ensureInit; the timeline
	// fields are filled in fresh on every rewrite.
	videoTimeline, audioTimeline, textTimeline []SegmentTimelineEntry
	manifestParams                             ManifestParams

	// audioClock computes each AAC frame's Duration without accumulating
	// rounding drift over a long stream (see mux.AudioClock) — lazily
	// created once the real sample rate is known, in handleAudio.
	audioClock *mux.AudioClock

	log *slog.Logger
}

// NewPackager creates a DASH packager for the given stream. outDir is
// created automatically.
func NewPackager(cfg *config.Config, stream *media.Stream, baseDataDir string) (*Packager, error) {
	// Output lives in memory (internal/livestore). Segments stay as long
	// as the manifest's timeline lists them, plus time to fetch the last.
	outDir := filepath.Join(baseDataDir, "dash", stream.Key.App, stream.Key.Name)
	windowSegments := defaultManifestTimelineSegments
	if cfg.DASH.DVRWindowS > 0 && cfg.DASH.SegmentDurationS > 0 {
		windowSegments = cfg.DASH.DVRWindowS / cfg.DASH.SegmentDurationS
	}
	livestore.Default().SetTTL(outDir, time.Duration(windowSegments*max(cfg.DASH.SegmentDurationS, 1))*time.Second+30*time.Second)

	p := &Packager{
		cfg:    cfg,
		stream: stream,
		outDir: outDir,
		segSeq: 1, // MPD SegmentTemplate declares startNumber="1" — the first segment must be numbered to match
		log:    slog.With("app", stream.Key.App, "stream", stream.Key.Name),
	}
	return p, nil
}

// SetDVBConfig applies this stream's resolved DVB-DASH profile/subtitles
// settings — called once, right after NewPackager, before Run (mirrors
// SetSCTE35Config's existing pattern). enabled/subtitlesEnabled are already
// fully resolved (global config.toml master switch AND'd with the per-app/
// stream DB override — see internal/streamoutputs), so ensureInit and
// closeSegment only ever need to consult this Packager's own fields, never
// reach back into global cfg for these two flags.
func (p *Packager) SetDVBConfig(enabled, subtitlesEnabled bool) {
	p.dvbProfile = enabled
	if subtitlesEnabled {
		p.textExtractor = captiondecode.NewExtractor()
		p.subtitlesEnabled = true
	}
}

// Run subscribes to the stream bus and packages packets until ctx is done.
func (p *Packager) Run(ctx context.Context) {
	sub := p.stream.Bus.Subscribe(ctx)
	defer sub.Close()
	defer livestore.Default().Release(p.outDir)

	p.log.Info("DASH packager started", "dir", p.outDir)

	for {
		select {
		case <-ctx.Done():
			p.flushTextAtEnd()
			p.closeSegment(-1)
			return

		case pkt, ok := <-sub.Chan():
			if !ok {
				p.flushTextAtEnd()
				p.closeSegment(-1)
				return
			}
			if err := p.handlePacket(pkt); err != nil {
				p.log.Warn("packet error", "err", err)
			}
		}
	}
}

// flushTextAtEnd finalizes whatever caption is currently visible/
// accumulating in the extractor at stream end (best-effort — see
// captiondecode.Extractor.Flush) so the final caption isn't silently
// dropped from the last segment cut.
func (p *Packager) flushTextAtEnd() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.textExtractor != nil {
		p.textCues = append(p.textCues, p.textExtractor.Flush(p.lastVideoDTS)...)
	}
	if p.teletextDecoder != nil {
		if cue, ok := p.teletextDecoder.Flush(p.lastVideoDTS); ok {
			p.textCues = append(p.textCues, cue)
		}
	}
}

func (p *Packager) handlePacket(pkt *media.Packet) error {
	if pkt.Type == media.PacketEnd {
		return p.closeSegment(-1)
	}
	if pkt.Codec.IsVideo() {
		return p.handleVideo(pkt)
	}
	if pkt.Codec.IsAudio() {
		return p.handleAudio(pkt)
	}
	if pkt.Codec == media.CodecDVBTeletext {
		return p.handleTeletext(pkt)
	}
	return nil
}

// handleTeletext feeds one raw EBU teletext data_field (see
// internal/ingest/mpegts's CodecDVBTeletext packets) into this stream's
// teletext.Decoder, lazily created on first use — CodecInfo only learns the
// target (magazine, page) once the source's PMT has been parsed, which
// happens after SetDVBConfig (called right after NewPackager, before any
// packets flow) — mirrors ensureInit's own "build lazily once CodecInfo is
// available" pattern.
func (p *Packager) handleTeletext(pkt *media.Packet) error {
	if !p.subtitlesEnabled {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.teletextDecoder == nil {
		ci := p.stream.Bus.CodecInfo()
		if ci == nil || !ci.HasDVBTeletextSubtitle {
			return nil // PMT not parsed yet, or this source has no teletext subtitle page
		}
		p.teletextDecoder = teletext.NewDecoder(ci.TeletextMagazine, ci.TeletextPage)
	}
	p.textCues = append(p.textCues, p.teletextDecoder.Feed(pkt.Data, pkt.PTS)...)
	return nil
}

// handleVideo mirrors hls.Packager.handleVideoFMP4's keyframe/target-duration
// segment-cut logic, minus the LL-HLS part-cadence branch (no parts in DASH
// scope this stage).
func (p *Packager) handleVideo(pkt *media.Packet) error {
	if pkt.Type == media.PacketConfig {
		return nil // CodecInfo is already on the Bus; tracks are built lazily at first flush
	}

	if pkt.IsKeyframe {
		if p.segStartDTS > 0 {
			durationMs := (pkt.DTS - p.segStartDTS) / 90
			if durationMs >= int64(p.cfg.DASH.SegmentDurationS*1000) {
				if err := p.closeSegment(pkt.DTS); err != nil {
					return err
				}
			}
		}
		if p.segStartDTS <= 0 {
			p.segStartDTS = pkt.DTS
			// Only once a segment is really open: a keyframe at DTS 0 (an
			// RTMP pull's first frame) doesn't open one (<= 0 is "not
			// open"), and an origin taken from it would put every audio
			// frame a GOP late.
			if !p.haveOrigin && p.segStartDTS > 0 {
				p.tsOrigin, p.haveOrigin = pkt.DTS, true
			}
		}
	}

	p.lastVideoDTS = pkt.DTS

	if p.textExtractor != nil {
		if cues := p.extractCaptions(pkt.Data, pkt.PTS); len(cues) > 0 {
			p.mu.Lock()
			p.textCues = append(p.textCues, cues...)
			p.mu.Unlock()
		}
	}

	if p.segStartDTS <= 0 {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.videoSamples, p.pendingVideo = mux.AppendPendingVideoSample(
		p.videoSamples, p.pendingVideo, pkt.DTS, pkt.PTS, pkt.IsKeyframe, pkt.Data)
	return nil
}

// extractCaptions finds and decodes an SEI NAL (type 6) within one video
// access unit's Annex-B byte stream (pkt.Data — this codebase's H.264
// convention keeps the start code in Data itself, see
// internal/rtmp/connection.go's convertAVCCToAnnexB), feeding it to the
// CEA-608/708 extractor. Independent of segStartDTS/segment state — cues
// accumulate against their own absolute PTS regardless of whether a
// segment is currently open, and get partitioned by segment window in
// closeSegment.
func (p *Packager) extractCaptions(data []byte, pts int64) []captiondecode.Cue {
	var cues []captiondecode.Cue
	for _, nal := range splitAnnexB(data) {
		if len(nal) > 0 && nal[0]&0x1F == 6 {
			cues = append(cues, p.textExtractor.Feed(nal, pts)...)
		}
	}
	return cues
}

// splitAnnexB splits a start-code-delimited Annex-B byte stream into
// individual NAL unit payloads (start codes stripped). Duplicated rather
// than shared — this codebase already has the identical helper duplicated
// per-package in internal/ingest/mpegts, internal/ingest/rtsp, and
// internal/webrtc, with no existing common package for it.
func splitAnnexB(data []byte) [][]byte {
	var nals [][]byte
	start := -1
	i := 0
	for i+2 < len(data) {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			if start >= 0 {
				nals = append(nals, data[start:i])
			}
			i += 3
			start = i
			continue
		}
		if i+3 < len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			if start >= 0 {
				nals = append(nals, data[start:i])
			}
			i += 4
			start = i
			continue
		}
		i++
	}
	if start >= 0 && start < len(data) {
		nals = append(nals, data[start:])
	}
	return nals
}

// handleAudio appends one raw audio frame per packet — a fixed samples-per-
// frame codec (AAC's 1024, MP2's 1152) means the nominal duration is known
// up front from the sample rate, no pending sample needed (mirrors
// hls.Packager.handleAudioFMP4). The actual per-frame duration comes from
// mux.AudioClock rather than a fresh `90000*samplesPerFrame/sampleRate`
// truncation every frame, which would otherwise accumulate real rounding
// drift over a long stream.
func (p *Packager) handleAudio(pkt *media.Packet) error {
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
		samplesPerFrame := 1024
		if ci := p.stream.Bus.CodecInfo(); ci != nil {
			if ci.AudioCodec == media.CodecMP2 {
				samplesPerFrame = 1152
				if ci.SampleRate > 0 {
					sampleRate = ci.SampleRate
				}
			} else if sr := mux.ParseAACSampleRate(ci.AACConfig); sr > 0 {
				sampleRate = sr
			}
		}
		p.audioClock = mux.NewAudioClock(sampleRate, samplesPerFrame)
	}
	duration := p.audioClock.NextDuration(pkt.PTS)

	// Keep the audio timeline on the source's clock (see tsOrigin).
	nominal := int64(duration)
	switch rel := pkt.DTS - p.tsOrigin; {
	case rel-p.audioPos > nominal:
		// Audio missing from the source: lengthen the previous frame, or
		// start this segment later, by the gap.
		gap := rel - p.audioPos
		if n := len(p.audioSamples); n > 0 {
			p.audioSamples[n-1].Duration += uint32(gap)
		} else {
			p.audioBaseDT += gap
		}
		p.audioPos = rel
	case p.audioPos-rel > nominal && p.audioPos-rel < 5*90000:
		// More audio than time (the source repeated or overlapped
		// frames): drop this one rather than run ahead of the video.
		return nil
	}
	p.audioPos += nominal

	p.audioSamples = append(p.audioSamples, mux.Sample{
		DTS:      pkt.DTS,
		PTS:      pkt.PTS,
		Duration: duration,
		IsKey:    true,
		Data:     pkt.Data,
	})
	return nil
}

// closeSegment finalizes the pending video sample, ensures the init segments
// + manifest exist, muxes each track's buffered samples into its own
// seg-{track}-N.m4s file, and advances segment state. nextDTS is the DTS of
// the packet that triggered this close (the new segment's first keyframe);
// pass -1 when closing the very last segment of a stream.
func (p *Packager) closeSegment(nextDTS int64) error {
	p.mu.Lock()

	if !p.initWritten {
		if err := p.ensureInit(); err != nil {
			p.mu.Unlock()
			p.log.Warn("dash init segment not ready yet", "err", err)
			return nil
		}
	}

	if p.pendingVideo != nil {
		var dur uint32
		switch {
		case nextDTS > 0:
			dur = uint32(nextDTS - p.pendingVideo.DTS)
		case len(p.videoSamples) > 0:
			dur = p.videoSamples[len(p.videoSamples)-1].Duration
		default:
			dur = uint32(p.cfg.DASH.SegmentDurationS) * 90000
		}
		p.videoSamples, p.pendingVideo = mux.FinalizePendingVideoSample(p.videoSamples, p.pendingVideo, dur)
	}

	if len(p.videoSamples) == 0 && len(p.audioSamples) == 0 {
		p.mu.Unlock()
		return nil
	}

	videoSamples := p.videoSamples
	audioSamples := p.audioSamples
	p.videoSamples = nil
	p.audioSamples = nil
	// Cut audio at the video cut's timestamp, not at whatever has arrived
	// by now: an RTMP source can send audio seconds ahead of the video it
	// goes with (the kaumudy pull leads by ~2 s), and cutting by arrival
	// would end every audio segment that much later than the video segment
	// with the same number. Frames past the cut open the next segment.
	// A source gap handleAudio folded into the last frame before the cut
	// would make this segment's audio run past its video; that part of
	// the gap moves to the start of the next segment instead (audioSkip).
	var audioSkip int64
	if nextDTS > 0 {
		for i, s := range audioSamples {
			if s.DTS >= nextDTS {
				p.audioSamples = append([]mux.Sample(nil), audioSamples[i:]...)
				audioSamples = audioSamples[:i]
				if i > 0 && p.audioClock != nil {
					last := &audioSamples[i-1]
					keep := max(nextDTS-last.DTS, int64(p.audioClock.Nominal()))
					if over := int64(last.Duration) - keep; over > 0 {
						last.Duration -= uint32(over)
						audioSkip = over
					}
				}
				break
			}
		}
	}

	videoMuxer := p.fmp4Video
	audioMuxer := p.fmp4Audio
	videoBaseDT := p.videoBaseDT
	audioBaseDT := p.audioBaseDT

	seq := p.segSeq
	p.segSeq++

	startDTS := p.segStartDTS
	p.segStartDTS = -1

	endDTS := nextDTS
	if endDTS <= 0 {
		endDTS = p.lastVideoDTS // closing the very last segment of a stream
	}

	// Snapshot the cues accumulated so far, then prune ones that will never
	// overlap a future segment again (EndPTS at or before this cut) — cues
	// spanning into later segments are deliberately kept for the next cut
	// to render too (ebuttd.Render clips each rendering to its own
	// segment's window regardless).
	textMuxer := p.fmp4Text
	textBaseDT := p.textBaseDT
	var textCues []captiondecode.Cue
	if textMuxer != nil {
		textCues = append(textCues, p.textCues...)
		kept := p.textCues[:0]
		for _, c := range p.textCues {
			if c.EndPTS > endDTS {
				kept = append(kept, c)
			}
		}
		p.textCues = kept
	}

	p.mu.Unlock()

	var totalVideoDur, totalAudioDur int64

	if videoMuxer != nil && len(videoSamples) > 0 {
		var buf bytes.Buffer
		if err := videoMuxer.WriteMediaSegment(&buf, 0, videoSamples, videoBaseDT); err != nil {
			return fmt.Errorf("dash mux video: %w", err)
		}
		segName := fmt.Sprintf("seg-video-%d.m4s", seq)
		livestore.Default().WriteFile(filepath.Join(p.outDir, segName), buf.Bytes())
		for _, s := range videoSamples {
			totalVideoDur += int64(s.Duration)
		}
	}

	if audioMuxer != nil && len(audioSamples) > 0 {
		var buf bytes.Buffer
		if err := audioMuxer.WriteMediaSegment(&buf, 0, audioSamples, audioBaseDT); err != nil {
			return fmt.Errorf("dash mux audio: %w", err)
		}
		segName := fmt.Sprintf("seg-audio-%d.m4s", seq)
		livestore.Default().WriteFile(filepath.Join(p.outDir, segName), buf.Bytes())
		for _, s := range audioSamples {
			totalAudioDur += int64(s.Duration)
		}
	}

	var totalTextDur int64
	if textMuxer != nil && startDTS > 0 {
		durTicks := endDTS - startDTS

		var textSamples []mux.Sample
		if p.dvbProfile {
			doc := ebuttd.Render(textCues, startDTS, durTicks)
			textSamples = []mux.Sample{{DTS: textBaseDT, PTS: textBaseDT, Duration: uint32(durTicks), IsKey: true, Data: doc}}
		} else {
			for _, s := range webvtt.RenderSamples(textCues, startDTS, durTicks) {
				textSamples = append(textSamples, mux.Sample{
					DTS: textBaseDT + s.DTS, PTS: textBaseDT + s.DTS,
					Duration: uint32(s.Duration), IsKey: true, Data: s.Data,
				})
			}
		}

		var buf bytes.Buffer
		if err := textMuxer.WriteMediaSegment(&buf, 0, textSamples, textBaseDT); err != nil {
			return fmt.Errorf("dash mux text: %w", err)
		}
		segName := fmt.Sprintf("seg-text-%d.m4s", seq)
		livestore.Default().WriteFile(filepath.Join(p.outDir, segName), buf.Bytes())
		totalTextDur = durTicks
	}

	p.mu.Lock()
	if totalVideoDur > 0 {
		p.videoTimeline = append(p.videoTimeline, SegmentTimelineEntry{Number: seq, StartTicks: videoBaseDT, DurationTicks: totalVideoDur})
	}
	if totalAudioDur > 0 {
		p.audioTimeline = append(p.audioTimeline, SegmentTimelineEntry{Number: seq, StartTicks: audioBaseDT, DurationTicks: totalAudioDur})
	}
	if totalTextDur > 0 {
		p.textTimeline = append(p.textTimeline, SegmentTimelineEntry{Number: seq, StartTicks: textBaseDT, DurationTicks: totalTextDur})
	}
	p.videoBaseDT += totalVideoDur
	p.audioBaseDT += totalAudioDur + audioSkip
	p.textBaseDT += totalTextDur

	// The manifest only needs enough recent history for a client to anchor
	// live-edge/current-segment timing — not the entire stream's history,
	// which would grow the manifest and this in-memory window unboundedly
	// on a long-running stream. When DVRWindowS is 0 (disk retention/seek-
	// back disabled), fall back to a fixed, generous window purely for this
	// bounding purpose — unrelated to (and never wider than) whatever
	// cleanupOldSegment actually keeps on disk.
	windowSegments := defaultManifestTimelineSegments
	if p.cfg.DASH.DVRWindowS > 0 {
		windowSegments = p.cfg.DASH.DVRWindowS / p.cfg.DASH.SegmentDurationS
	}
	cutoff := seq - windowSegments
	p.videoTimeline = trimTimeline(p.videoTimeline, cutoff)
	p.audioTimeline = trimTimeline(p.audioTimeline, cutoff)
	p.textTimeline = trimTimeline(p.textTimeline, cutoff)

	manifestParams := p.manifestParams
	manifestParams.VideoTimeline = append([]SegmentTimelineEntry(nil), p.videoTimeline...)
	manifestParams.AudioTimeline = append([]SegmentTimelineEntry(nil), p.audioTimeline...)
	manifestParams.TextTimeline = append([]SegmentTimelineEntry(nil), p.textTimeline...)
	p.mu.Unlock()

	if err := WriteMPD(p.outDir, manifestParams); err != nil {
		p.log.Warn("dash rewrite manifest.mpd", "err", err)
	}

	durationS := float64(p.cfg.DASH.SegmentDurationS)
	if nextDTS > 0 && startDTS > 0 {
		durationS = float64(nextDTS-startDTS) / 90000.0
	}

	if p.cfg.DASH.DVRWindowS > 0 {
		p.cleanupOldSegment(seq)
	}

	p.log.Debug("dash segment written", "seq", seq, "dur_s", fmt.Sprintf("%.3f", durationS))
	return nil
}

// defaultManifestTimelineSegments bounds how many recent segments' worth of
// SegmentTimeline history the manifest carries when DVRWindowS is 0 — see
// closeSegment's use of it.
const defaultManifestTimelineSegments = 30

// trimTimeline drops entries at or before cutoff (a segment number),
// keeping the slice sorted ascending by Number as closeSegment always
// appends it.
func trimTimeline(entries []SegmentTimelineEntry, cutoff int) []SegmentTimelineEntry {
	i := 0
	for i < len(entries) && entries[i].Number <= cutoff {
		i++
	}
	return entries[i:]
}

// cleanupOldSegment removes the single video/audio segment pair that just
// fell outside the DVR window, now that segment latestSeq has been cut —
// cheaper than scanning the directory on every cut, since at most one
// segment ages out per new one written. A no-op (segments simply don't
// exist yet) until the stream has been running longer than the window.
func (p *Packager) cleanupOldSegment(latestSeq int) {
	maxSegments := p.cfg.DASH.DVRWindowS / p.cfg.DASH.SegmentDurationS
	expireSeq := latestSeq - maxSegments
	if expireSeq < 1 { // startNumber="1" — nothing before it ever existed
		return
	}
	for _, segName := range []string{
		fmt.Sprintf("seg-video-%d.m4s", expireSeq),
		fmt.Sprintf("seg-audio-%d.m4s", expireSeq),
		fmt.Sprintf("seg-text-%d.m4s", expireSeq), // no-op (os.IsNotExist below) when subtitles aren't enabled
	} {
		livestore.Default().Remove(filepath.Join(p.outDir, segName))
	}
}

// audioSampleRate/audioChannels return ci's audio parameters regardless of
// codec: MP2 carries them directly in CodecInfo (see mergeMP2CodecInfo, and
// hasAudio's own doc comment on why MP2 has no AACConfig-equivalent);
// AAC encodes them in AACConfig instead.
func audioSampleRate(ci *media.CodecInfo) int {
	if ci.AudioCodec == media.CodecMP2 {
		return ci.SampleRate
	}
	return mux.ParseAACSampleRate(ci.AACConfig)
}

func audioChannels(ci *media.CodecInfo) int {
	if ci.AudioCodec == media.CodecMP2 {
		return ci.Channels
	}
	return mux.ParseAACChannels(ci.AACConfig)
}

// ensureInit builds the video/audio track muxers and writes init-video.mp4 /
// init-audio.mp4 + the (static, written-once) manifest.mpd, using whatever
// CodecInfo is available at the time of the first flush. Called with p.mu
// already held. Mirrors hls.Packager.initFMP4Muxers — FMP4Muxer has no
// re-init support, so this never re-runs if codec parameters change
// mid-stream.
func (p *Packager) ensureInit() error {
	ci := p.stream.Bus.CodecInfo()
	if ci == nil {
		return fmt.Errorf("no codec info available yet")
	}

	hasVideo := len(ci.SPSNALu) > 0
	// MP2 (see internal/ingest/mpegts/mp2.go) has no AACConfig-equivalent
	// out-of-band config — CodecInfo.SampleRate/Channels are how it signals
	// "audio codec info is available" instead (see mergeMP2CodecInfo).
	hasAudio := len(ci.AACConfig) > 0 || (ci.AudioCodec == media.CodecMP2 && ci.SampleRate > 0)
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
			return fmt.Errorf("write init-video.mp4: %w", err)
		}
		livestore.Default().WriteFile(filepath.Join(p.outDir, "init-video.mp4"), buf.Bytes())
	}

	if hasAudio {
		audioTrack := &mux.FMP4Track{
			Type:      mux.TrackAudio,
			TrackID:   2,
			Timescale: 90000,
		}
		audioTrack.SampleRate = uint32(audioSampleRate(ci))
		audioTrack.Channels = uint8(audioChannels(ci))
		if ci.AudioCodec == media.CodecMP2 {
			audioTrack.AudioObjectType = mux.MPEG1AudioObjectType
		} else {
			audioTrack.AACConfig = ci.AACConfig
		}
		p.fmp4Audio = mux.NewFMP4Muxer(audioTrack)

		var buf bytes.Buffer
		if err := p.fmp4Audio.WriteInitSegment(&buf); err != nil {
			return fmt.Errorf("write init-audio.mp4: %w", err)
		}
		livestore.Default().WriteFile(filepath.Join(p.outDir, "init-audio.mp4"), buf.Bytes())
	}

	hasText := p.textExtractor != nil
	if hasText {
		textTrack := &mux.FMP4Track{
			Type:      mux.TrackText,
			TrackID:   3,
			Timescale: 90000,
		}
		// EBU-TT-D under the DVB profile (DVB-DASH mandates it); WebVTT
		// otherwise — live EBU-TT-D/fragmented-TTML rendering was found
		// unreliable across web DASH clients (see internal/webvtt's doc
		// comment), while WebVTT populates the browser's native TextTrack
		// directly with no custom renderer in the loop.
		if p.dvbProfile {
			textTrack.Namespace = "http://www.w3.org/ns/ttml http://www.w3.org/ns/ttml#styling http://www.w3.org/ns/ttml#metadata http://www.ebu.ch/tt/ttml/profiles/ebu-tt-1"
		} else {
			textTrack.TextFormat = "wvtt"
		}
		p.fmp4Text = mux.NewFMP4Muxer(textTrack)

		var buf bytes.Buffer
		if err := p.fmp4Text.WriteInitSegment(&buf); err != nil {
			return fmt.Errorf("write init-text.mp4: %w", err)
		}
		livestore.Default().WriteFile(filepath.Join(p.outDir, "init-text.mp4"), buf.Bytes())
	}

	params := ManifestParams{
		AvailabilityStart: p.stream.StartedAt,
		SegmentDurationS:  p.cfg.DASH.SegmentDurationS,
		MinBufferTimeS:    p.cfg.DASH.MinBufferTimeS,
		DVRWindowS:        p.cfg.DASH.DVRWindowS,
		HasVideo:          hasVideo,
		VideoWidth:        ci.Width,
		VideoHeight:       ci.Height,
		VideoCodecs:       media.H264CodecString(ci),
		HasAudio:          hasAudio,
		AudioCodecs:       media.AudioCodecString(ci),
		AudioSampleRate:   audioSampleRate(ci),
		AudioChannels:     audioChannels(ci),
		HasText:           hasText,
		TeletextSourced:   ci.HasDVBTeletextSubtitle,
	}
	if p.dvbProfile {
		params.DVBProfile = true
		params.UTCTimingURL = p.cfg.Multinode.AdvertiseURL + "/dash-utc-time"
		params.BaseURL = fmt.Sprintf("%s%s/%s/%s/", p.cfg.Multinode.AdvertiseURL, p.cfg.DASH.PathPrefix, p.stream.Key.App, p.stream.Key.Name)
	}
	// Stored (sans timeline fields, which are nil until the first segment
	// closes) so closeSegment can rewrite the manifest with fresh timeline
	// data on every subsequent cut without re-deriving these static fields.
	p.manifestParams = params
	if err := WriteMPD(p.outDir, params); err != nil {
		return fmt.Errorf("write manifest.mpd: %w", err)
	}

	p.initWritten = true
	p.log.Info("DASH init segments + manifest written", "video", hasVideo, "audio", hasAudio, "text", hasText)
	return nil
}
