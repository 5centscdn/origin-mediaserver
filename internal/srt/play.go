package srt

import (
	"context"
	"io"
	"log/slog"

	gosrt "github.com/datarhei/gosrt"

	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/tsout"
)

// SRT playback: a caller in request mode (streamid
// "#!::r=app/stream,m=request" or "play/app/stream") gets the live stream
// as continuous MPEG-TS, the same bytes /{app}/{stream}/mpeg.2ts serves
// over HTTP (internal/tsout). Any live stream can be played, whatever it
// was ingested over. Public, like the other playback outputs.
//
// e.g. ffplay 'srt://host:4000?streamid=#!::r=live/cam,m=request'

// SCTE35Mode, if set, resolves a stream's SCTE-35 mode for playback (wired
// to streamoutputs.ResolveSCTE35Mode in main.go). nil = SCTE-35 not sent.
var SCTE35Mode func(ctx context.Context, app, stream string) string

func (s *Server) handlePlay(ctx context.Context, req gosrt.ConnRequest, key media.StreamKey) {
	st := s.registry.Get(key)
	if st == nil {
		slog.Info("srt play: stream not found", "stream", key)
		req.Reject(gosrt.REJX_NOTFOUND)
		return
	}
	conn, err := req.Accept()
	if err != nil {
		slog.Warn("srt play: accept failed", "stream", key, "err", err)
		return
	}
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	st.Viewers.Touch("srt:" + remote)
	slog.Info("srt play started", "stream", key, "remote", remote)

	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The peer closing the connection only shows as a failed read.
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, err := conn.Read(buf); err != nil {
				cancel()
				return
			}
		}
	}()

	mode := "disabled"
	if SCTE35Mode != nil {
		mode = SCTE35Mode(pctx, key.App, key.Name)
	}
	w := &datagramWriter{w: conn}
	err = tsout.Write(pctx, w, w.Flush, st, mode)
	slog.Info("srt play ended", "stream", key, "remote", remote, "err", err)
}

// datagramSize is the usual SRT payload for MPEG-TS: 7 TS packets.
const datagramSize = 7 * 188

// datagramWriter groups TS packets into whole 1316-byte SRT payloads.
// gosrt sends a short packet for any remainder of each Write, which would
// split TS packets across datagrams for receivers that expect them whole.
type datagramWriter struct {
	w   io.Writer
	buf []byte
}

func (d *datagramWriter) Write(p []byte) (int, error) {
	d.buf = append(d.buf, p...)
	for len(d.buf) >= datagramSize {
		if _, err := d.w.Write(d.buf[:datagramSize]); err != nil {
			return 0, err
		}
		d.buf = d.buf[datagramSize:]
	}
	return len(p), nil
}

// Flush sends what's buffered, whole TS packets only, so a frame isn't
// held until the next one fills the datagram.
func (d *datagramWriter) Flush() error {
	n := len(d.buf) / 188 * 188
	if n == 0 {
		return nil
	}
	if _, err := d.w.Write(d.buf[:n]); err != nil {
		return err
	}
	d.buf = append(d.buf[:0], d.buf[n:]...)
	return nil
}
