// Package srt implements SRT ingest: a listening server (mirroring
// internal/rtmp.Server's shape, not internal/pull.Manager's per-source-row
// dial model — SRT encoders connect in, the same as RTMP) that accepts SRT
// "caller" connections and demuxes their payload — by far the dominant
// real-world convention, MPEG-TS-in-SRT — through the existing
// internal/ingest/mpegts.Demuxer, so no new depacketization code is needed
// here at all.
package srt

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	gosrt "github.com/datarhei/gosrt"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/ingest/mpegts"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
)

// Server accepts SRT ingest connections. App/stream resolution and publish-
// key/allow-any-stream checks are DB-app-only (no config.RTMPApp legacy
// path, no FMLE %i ABR pattern matching) — mirroring WHIP's simpler model
// rather than RTMP's dual config+DB one, since SRT is a new protocol with
// no equivalent of FMLE's single-connection multi-bitrate mechanism.
type Server struct {
	cfg      *config.Config
	db       *db.DB
	registry *media.Registry
	metrics  *metrics.Metrics // optional — see SetMetrics; nil is a safe no-op

	// OnPublish is called in a new goroutine each time a stream starts
	// publishing. Wire this up in main.go to start HLS/DASH packagers and
	// DVR recording, exactly like internal/rtmp.Server's OnPublish.
	OnPublish func(stream *media.Stream)

	// OnUnpublish is called when a stream's SRT connection ends.
	OnUnpublish func(stream *media.Stream)
}

// NewServer creates an SRT ingest server.
func NewServer(cfg *config.Config, registry *media.Registry, database *db.DB) *Server {
	return &Server{cfg: cfg, db: database, registry: registry}
}

// SetMetrics attaches a metrics recorder used for SCTE-35 parse-error
// counting (see handleConnRequest). Optional — a nil or never-called Server
// simply doesn't record it.
func (s *Server) SetMetrics(m *metrics.Metrics) {
	s.metrics = m
}

// ListenAndServe starts the SRT listener. It blocks until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srtCfg := gosrt.DefaultConfig()
	if s.cfg.SRT.LatencyMs > 0 {
		srtCfg.Latency = time.Duration(s.cfg.SRT.LatencyMs) * time.Millisecond
	}
	if s.cfg.SRT.Passphrase != "" {
		srtCfg.Passphrase = s.cfg.SRT.Passphrase
		srtCfg.EnforcedEncryption = true
	}

	ln, err := gosrt.Listen("srt", s.cfg.SRT.Listen, srtCfg)
	if err != nil {
		return fmt.Errorf("srt listen %s: %w", s.cfg.SRT.Listen, err)
	}

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	slog.Info("SRT server listening", "addr", s.cfg.SRT.Listen)

	for {
		req, err := ln.Accept2()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handleConnRequest(ctx, req)
	}
}

// handleConnRequest resolves the incoming connection's streamid, validates
// it against the DB app's publish key / allow-any-stream rules exactly like
// WHIP does, and — if accepted — runs an MPEG-TS demuxer over the
// connection until it closes.
func (s *Server) handleConnRequest(ctx context.Context, req gosrt.ConnRequest) {
	app, streamName, play, err := parseStreamID(req.StreamId())
	if err != nil {
		slog.Warn("srt: rejecting connection with unparseable streamid", "streamid", req.StreamId(), "err", err)
		req.Reject(gosrt.REJX_BAD_REQUEST)
		return
	}
	if play {
		s.handlePlay(ctx, req, media.StreamKey{App: app, Name: stripKeyFromName(streamName)})
		return
	}

	appCfg, err := s.db.GetApplicationByName(ctx, app)
	if err != nil || appCfg == nil || !appCfg.Enabled {
		slog.Warn("srt: rejecting connection for unknown or disabled app", "app", app)
		req.Reject(gosrt.REJX_NOTFOUND)
		return
	}

	if appCfg.PublishKey != "" {
		if !validatePublishKey(streamName, appCfg.PublishKey) {
			slog.Warn("srt: rejecting connection with bad publish key", "app", app, "stream", streamName)
			req.Reject(gosrt.REJX_UNAUTHORIZED)
			return
		}
		streamName = stripKeyFromName(streamName)
	}

	if !appCfg.AllowAnyStream {
		sc, err := s.db.GetStreamConfig(ctx, appCfg.ID, streamName)
		if err != nil || sc == nil {
			slog.Warn("srt: rejecting connection for unconfigured stream", "app", app, "stream", streamName)
			req.Reject(gosrt.REJX_FORBIDDEN)
			return
		}
	}

	key := media.StreamKey{App: app, Name: streamName}
	if existing := s.registry.Get(key); existing != nil {
		slog.Warn("srt: rejecting connection, stream already publishing", "stream", key)
		req.Reject(gosrt.REJX_CONFLICT)
		return
	}
	limits, _ := s.db.PublishLimits(ctx, appCfg.ID, streamName)
	if le := s.registry.LimitReached(app, streamName, limits); le != nil {
		slog.Warn("srt: rejecting connection, publish limit reached", "stream", key, "pattern", le.Pattern, "max", le.Max)
		req.Reject(gosrt.REJX_OVERLOAD)
		return
	}

	conn, err := req.Accept()
	if err != nil {
		slog.Warn("srt: accept failed", "app", app, "stream", streamName, "err", err)
		return
	}
	defer conn.Close()

	stream := &media.Stream{
		Key:        key,
		SourceType: "srt",
		SourceAddr: conn.RemoteAddr().String(),
		StartedAt:  time.Now(),
	}
	if err := s.registry.RegisterLimited(stream, limits); err != nil {
		slog.Warn("srt: register failed", "stream", key, "err", err)
		return
	}

	slog.Info("srt: stream publishing", "stream", key, "remote", stream.SourceAddr)
	if s.OnPublish != nil {
		go s.OnPublish(stream)
	}

	demux := mpegts.NewDemuxer(stream.Bus)
	demux.SetLogContext("stream", key.String(), "source", "srt", "mode", "listen")
	demux.OnBytes = func(n int) { stream.Bus.AddBytesIn(int64(n)) }
	if s.metrics != nil {
		demux.OnSCTE35Error = func(error) {
			s.metrics.SCTE35ParseErrorsTotal.WithLabelValues(app, streamName).Inc()
		}
	}
	if err := demux.Run(ctx, conn); err != nil {
		slog.Warn("srt: demux ended with error", "stream", key, "err", err)
	}

	s.registry.Unregister(stream)
	slog.Info("srt: stream stopped", "stream", key)
	if s.OnUnpublish != nil {
		go s.OnUnpublish(stream)
	}
}

// parseStreamID splits an SRT streamid into app/streamName and whether the
// caller wants to play (request mode) rather than publish. Accepted forms:
//
//   - "app/stream": publish (this codebase's original convention, the same
//     path shape as WHIP's /whip/{app}/{stream});
//   - "play/app/stream": play;
//   - the SRT Access Control form "#!::r=app/stream,m=request" (play) or
//     "m=publish" (or no m: publish), which ffmpeg, OBS, vMix and Nimble
//     use. Other keys (u=, s=, t=...) are ignored.
func parseStreamID(id string) (app, streamName string, play bool, err error) {
	if rest, ok := strings.CutPrefix(id, "#!::"); ok {
		var resource, mode string
		for _, kv := range strings.Split(rest, ",") {
			k, v, _ := strings.Cut(kv, "=")
			switch strings.TrimSpace(k) {
			case "r":
				resource = strings.TrimSpace(v)
			case "m":
				mode = strings.ToLower(strings.TrimSpace(v))
			}
		}
		if mode != "" && mode != "request" && mode != "publish" {
			return "", "", false, fmt.Errorf("streamid mode %q not supported (request or publish)", mode)
		}
		id, play = resource, mode == "request"
	} else if rest, ok := strings.CutPrefix(strings.TrimPrefix(id, "/"), "play/"); ok {
		id, play = rest, true
	}
	id = strings.TrimPrefix(id, "/")
	app, streamName, ok := strings.Cut(id, "/")
	if !ok || app == "" || streamName == "" {
		return "", "", false, fmt.Errorf("streamid must be \"app/streamName\", \"play/app/streamName\" or \"#!::r=app/streamName,m=request\", got %q", id)
	}
	return app, streamName, play, nil
}

// validatePublishKey and stripKeyFromName mirror internal/rtmp's
// identically-named helpers: the publish key travels appended to the
// stream name as "streamName?key" (SRT's streamid has no separate header
// or field for it, the same constraint RTMP's stream-name-based publish
// command has) — duplicated locally rather than exported cross-package,
// consistent with this codebase's convention for small ingest helpers.
func validatePublishKey(streamName, key string) bool {
	return streamName == key || len(streamName) > len(key) &&
		streamName[len(streamName)-len(key)-1] == '?' &&
		streamName[len(streamName)-len(key):] == key
}

func stripKeyFromName(name string) string {
	if i := strings.LastIndexByte(name, '?'); i >= 0 {
		return name[:i]
	}
	return name
}
