// Package icecast implements Icecast/SHOUTcast-style HTTP source ingest:
// a listening server (mirroring internal/rtmp.Server's shape, not
// internal/pull.Manager's per-source-row dial model — encoders connect in,
// same as RTMP/SRT) that accepts an HTTP-like SOURCE (Icecast2-native) or
// PUT (modern HTTP) request carrying a continuous, unframed MP3 or ADTS
// AAC byte stream. Audio-only by protocol — there is no video track,
// ever, which is why internal/hls.Packager's TS path has a dedicated
// audio-only branch keyed on Stream.SourceType == "icecast".
//
// Deliberately not supported: legacy passwordless SHOUTcast v1 handshakes
// (the client sends just a bare password line with no HTTP-style request
// at all) — real-world encoders (ffmpeg's icecast:// output, Liquidsoap,
// BUTT) all speak the SOURCE/PUT form this package implements.
package icecast

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// Server accepts Icecast source connections. App/stream resolution and
// publish-key/allow-any-stream checks are DB-app-only, mirroring WHIP/SRT
// rather than RTMP's dual config+DB model — Icecast has no FMLE-%i-style
// multi-bitrate mechanism to preserve.
type Server struct {
	cfg      *config.Config
	db       *db.DB
	registry *media.Registry

	// OnPublish is called in a new goroutine each time a stream starts
	// publishing — same contract as internal/rtmp.Server/internal/srt.Server.
	OnPublish func(stream *media.Stream)

	// OnUnpublish is called when a stream's connection ends.
	OnUnpublish func(stream *media.Stream)
}

// NewServer creates an Icecast ingest server.
func NewServer(cfg *config.Config, registry *media.Registry, database *db.DB) *Server {
	return &Server{cfg: cfg, db: database, registry: registry}
}

// ListenAndServe starts the Icecast listener. It blocks until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Icecast.Listen)
	if err != nil {
		return fmt.Errorf("icecast listen %s: %w", s.cfg.Icecast.Listen, err)
	}

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	slog.Info("Icecast server listening", "addr", s.cfg.Icecast.Listen)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)

	method, path, err := readRequestLine(r)
	if err != nil {
		return
	}
	if method != "SOURCE" && method != "PUT" {
		writeResponse(conn, 405, "Method Not Allowed")
		return
	}

	headers, err := readHeaders(r)
	if err != nil {
		return
	}

	app, streamName, ok := parseIcecastPath(path)
	if !ok {
		slog.Warn("icecast: rejecting connection with unparseable mountpoint", "path", path)
		writeResponse(conn, 400, "Bad Request")
		return
	}

	appCfg, err := s.db.GetApplicationByName(ctx, app)
	if err != nil || appCfg == nil || !appCfg.Enabled {
		slog.Warn("icecast: rejecting connection for unknown or disabled app", "app", app)
		writeResponse(conn, 404, "Not Found")
		return
	}

	if appCfg.PublishKey != "" && !checkBasicAuthPassword(headers["authorization"], appCfg.PublishKey) {
		slog.Warn("icecast: rejecting connection with bad publish key", "app", app, "stream", streamName)
		writeResponse(conn, 401, "Unauthorized")
		return
	}

	if !appCfg.AllowAnyStream {
		sc, err := s.db.GetStreamConfig(ctx, appCfg.ID, streamName)
		if err != nil || sc == nil {
			slog.Warn("icecast: rejecting connection for unconfigured stream", "app", app, "stream", streamName)
			writeResponse(conn, 403, "Forbidden")
			return
		}
	}

	codec, ok := codecFromContentType(headers["content-type"])
	if !ok {
		slog.Warn("icecast: rejecting connection with unsupported content-type", "content_type", headers["content-type"])
		writeResponse(conn, 415, "Unsupported Media Type")
		return
	}

	key := media.StreamKey{App: app, Name: streamName}
	if existing := s.registry.Get(key); existing != nil {
		slog.Warn("icecast: rejecting connection, stream already publishing", "stream", key)
		writeResponse(conn, 409, "Conflict")
		return
	}
	limits, _ := s.db.PublishLimits(ctx, appCfg.ID, streamName)
	if le := s.registry.LimitReached(app, streamName, limits); le != nil {
		slog.Warn("icecast: rejecting connection, publish limit reached", "stream", key, "pattern", le.Pattern, "max", le.Max)
		writeResponse(conn, 403, "Forbidden")
		return
	}

	// Handshake accepted. A PUT request may have sent Expect: 100-continue
	// before its body; Icecast2's native SOURCE method never does. Either
	// way, streaming keeps this connection open indefinitely, so this 200
	// is the ONLY status line this connection will ever see — there's no
	// final response to send once the stream ends, since by then the
	// client has simply hung up.
	if strings.EqualFold(headers["expect"], "100-continue") {
		fmt.Fprintf(conn, "HTTP/1.1 100 Continue\r\n\r\n")
	}
	fmt.Fprintf(conn, "HTTP/1.0 200 OK\r\n\r\n")

	stream := &media.Stream{
		Key:        key,
		SourceType: "icecast",
		SourceAddr: conn.RemoteAddr().String(),
		StartedAt:  time.Now(),
	}
	if err := s.registry.RegisterLimited(stream, limits); err != nil {
		slog.Warn("icecast: register failed", "stream", key, "err", err)
		return
	}

	slog.Info("icecast: stream publishing", "stream", key, "codec", codec, "remote", stream.SourceAddr)
	if s.OnPublish != nil {
		go s.OnPublish(stream)
	}

	src := newSource(stream.Bus, codec)
	if err := src.run(ctx, r); err != nil {
		slog.Warn("icecast: source ended with error", "stream", key, "err", err)
	}

	s.registry.Unregister(stream)
	slog.Info("icecast: stream stopped", "stream", key)
	if s.OnUnpublish != nil {
		go s.OnUnpublish(stream)
	}
}

func readRequestLine(r *bufio.Reader) (method, path string, err error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", "", err
	}
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 {
		return "", "", fmt.Errorf("malformed request line: %q", line)
	}
	return parts[0], parts[1], nil
}

func readHeaders(r *bufio.Reader) (map[string]string, error) {
	headers := make(map[string]string)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return headers, nil
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
}

// parseIcecastPath splits an Icecast mountpoint path into app/streamName,
// using the same "/app/streamName" convention as internal/srt's
// parseStreamID — Icecast mountpoints are otherwise flat, opaque strings
// with no app/stream hierarchy of their own, so this codebase's app/stream
// model has to be layered on via the path an operator configures their
// encoder with (e.g. icecast://source:key@host:8005/live/radio1).
func parseIcecastPath(path string) (app, streamName string, ok bool) {
	path = strings.TrimPrefix(path, "/")
	app, streamName, found := strings.Cut(path, "/")
	if !found || app == "" || streamName == "" {
		return "", "", false
	}
	return app, streamName, true
}

// checkBasicAuthPassword validates an HTTP Basic Authorization header's
// password against key. The username is ignored — Icecast's SOURCE method
// conventionally sends "source" and PUT-based encoders vary, but this
// codebase's whole publish-key model (RTMP, WHIP, SRT) is a single shared
// secret per application, not a per-user credential.
func checkBasicAuthPassword(header, key string) bool {
	const prefix = "Basic "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	_, password, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return false
	}
	return password == key
}

// codecFromContentType maps an Icecast source's declared Content-Type to
// this codebase's CodecType — only the two codecs the rest of the pipeline
// (TS muxing, HLS) already understands. Ogg/Vorbis, Opus-in-Ogg, and other
// Icecast-supported formats are deliberately not handled.
func codecFromContentType(ct string) (media.CodecType, bool) {
	switch strings.ToLower(strings.TrimSpace(ct)) {
	case "audio/mpeg", "audio/mp3":
		return media.CodecMP3, true
	case "audio/aac", "audio/aacp":
		return media.CodecAAC, true
	default:
		return media.CodecUnknown, false
	}
}

func writeResponse(conn net.Conn, status int, statusText string) {
	fmt.Fprintf(conn, "HTTP/1.0 %d %s\r\n\r\n", status, statusText)
}
