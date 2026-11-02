// Package tsout implements continuous, unsegmented MPEG-TS delivery over a
// single long-lived HTTP response — the "raw MPEG-TS" output alternative to
// HLS (segmented .ts files + .m3u8 playlist) and DASH, for a downstream
// consumer (e.g. a transcoder) that wants to pull one live TS byte stream
// directly rather than poll a playlist. It reuses mux.TSMuxer exactly as
// internal/hls's TS-container packager does — same PAT/PMT/SPS-PPS/ADTS/
// SCTE-35 handling — just without any segmenting, playlist, or ad-marker
// tag rendering, none of which apply to a continuous byte stream.
package tsout

import (
	"context"
	"fmt"
	"net/http"

	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/mux"
	"github.com/5centscdn/origin-mediaserver/internal/scte35"
)

// Serve streams stream's live media as continuous MPEG-TS onto w until the
// client disconnects (ctx done — pass the request's context), the stream
// itself ends (its Bus closes), or a write error occurs. Sets the response
// headers and flushes after every packet, so bytes reach the client as
// they're produced rather than being buffered — the request's http.Server
// must not impose a write timeout (this codebase's already doesn't; see
// cmd/server/main.go's httpServer.WriteTimeout=0, needed for LL-HLS's
// long-poll for the same reason).
//
// scte35Mode gates which SCTE-35 packets get forwarded into the TS stream
// (see scte35.SourceAllowed) — the same passthrough/insert/hybrid/disabled
// semantics internal/hls.Packager's handleSCTE35 applies, minus HLS's
// ad-marker tag rendering: raw MPEG-TS conveys SCTE-35 purely via the PID
// 0x86 splice_info_section, nothing else.
//
// Each call gets its own *mux.TSMuxer — its own PAT/PMT/continuity
// counters — unlike HLS/DASH's one packager per *stream*: a continuous
// push response is inherently one per *viewer connection*, not per stream.
//
// Limitation: like every other TS-container output in this codebase,
// requires a video track (H.264/H.265) to ever start — an audio-only
// source (e.g. Icecast) never gets a muxer, since there's no in-band SPS
// to trigger one on. internal/hls.Packager's handleAudioOnlyTS special-
// cases that; this package does not, since no MPEG-TS-over-HTTP request
// against an audio-only stream has been a real use case yet.
func Serve(ctx context.Context, w http.ResponseWriter, stream *media.Stream, scte35Mode string) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("tsout: ResponseWriter does not support flushing")
	}

	sub := stream.Bus.Subscribe(ctx)
	defer sub.Close()

	scte35Enabled := scte35Mode == "passthrough" || scte35Mode == "insert" || scte35Mode == "hybrid"

	w.Header().Set("Content-Type", "video/MP2T")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var muxer *mux.TSMuxer
	gotKeyframe := false // never start mid-GOP — a fresh connection must decode from frame one

	tryInitMuxer := func() {
		if muxer != nil {
			return
		}
		ci := stream.Bus.CodecInfo()
		if ci == nil || len(ci.SPSNALu) == 0 {
			return // still waiting for the in-band SPS/sequence header
		}
		muxer = newMuxer(w, ci, scte35Enabled)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case pkt, ok := <-sub.Chan():
			if !ok {
				return nil // stream ended
			}

			if pkt.Type == media.PacketConfig {
				tryInitMuxer()
				continue
			}
			tryInitMuxer()
			if muxer == nil {
				continue
			}

			switch {
			case pkt.Codec.IsVideo():
				if pkt.IsKeyframe {
					gotKeyframe = true
				}
				if !gotKeyframe {
					continue
				}
				if err := muxer.WriteVideo(pkt.Data, pkt.DTS, pkt.PTS, pkt.IsKeyframe); err != nil {
					return fmt.Errorf("tsout: write video: %w", err)
				}

			case pkt.Codec.IsAudio():
				if !gotKeyframe {
					continue // keep audio aligned to the same starting point as video
				}
				// Same codec gate as internal/hls.Packager.handleAudio: an
				// unsupported codec (e.g. Opus, from a WHIP-ingested
				// stream) would otherwise get muxed as raw bytes into a PES
				// the PMT still claims is AAC/MP3/AC3, corrupting playback
				// for any demuxer that trusts it — skip rather than lie.
				switch pkt.Codec {
				case media.CodecAAC, media.CodecMP3, media.CodecAC3:
				default:
					continue
				}
				audioData := pkt.Data
				if pkt.Codec == media.CodecAAC {
					audioData = mux.WrapADTS(pkt.Data, stream.Bus.CodecInfo().AACConfig)
				}
				if err := muxer.WriteAudio(audioData, pkt.PTS); err != nil {
					return fmt.Errorf("tsout: write audio: %w", err)
				}

			case pkt.Codec == media.CodecSCTE35:
				if !scte35Enabled || pkt.SCTE35 == nil || !scte35.SourceAllowed(scte35Mode, pkt.SCTE35.Source) {
					continue
				}
				if err := muxer.WriteSCTE35(pkt.Data); err != nil {
					return fmt.Errorf("tsout: write scte35: %w", err)
				}

			default:
				continue
			}

			flusher.Flush()
		}
	}
}

// newMuxer constructs a TSMuxer for ci, writing to w — mirrors internal/
// hls.Packager.initMuxer's stream-type/param-set selection exactly.
func newMuxer(w http.ResponseWriter, ci *media.CodecInfo, scte35Enabled bool) *mux.TSMuxer {
	videoType := uint8(mux.StreamTypeH264)
	if ci.VideoCodec == media.CodecH265 {
		videoType = mux.StreamTypeH265
	}
	audioType := uint8(mux.StreamTypeAAC)
	switch ci.AudioCodec {
	case media.CodecMP3:
		audioType = mux.StreamTypeMP3
	case media.CodecAC3:
		audioType = mux.StreamTypeAC3
	}

	m := mux.NewTSMuxer(w, videoType, audioType)
	if scte35Enabled {
		m.EnableSCTE35()
	}
	if ci.VideoCodec == media.CodecH265 {
		m.SetHEVCParamSets(ci.VPSNALu, ci.SPSNALu, ci.PPSNALu)
	} else {
		m.SetSPSPPS(ci.SPSNALu, ci.PPSNALu)
	}
	return m
}
