package srt

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	gosrt "github.com/datarhei/gosrt"

	"github.com/5centscdn/origin-mediaserver/internal/ingest/mpegts"
	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// Pull dials url as an SRT caller (as opposed to Server's listener/callee
// role) and demuxes its MPEG-TS payload onto bus — SRT Pull/Caller ingest's
// counterpart to Server's SRT Publish/Listen. Blocks until the connection
// ends or ctx is cancelled, matching every other internal/pull.Manager
// protocol's Run-until-done contract (see internal/pull/manager.go's
// supportedProtocols).
//
// passphrase decrypts an upstream source encrypted with SRT's AES
// passphrase scheme (Haivision SRT / Nimble's "Passphrase" field) — empty
// means the upstream is unencrypted, mirroring ListenAndServe's own
// Passphrase-gated EnforcedEncryption logic on the listen side. pbkeylen is
// the AES key length in bytes (16/24/32); 0 leaves gosrt's default (16).
//
// This is intentionally a thin wrapper: it does exactly what
// Server.handleConnRequest does after req.Accept() — hand the raw
// gosrt.Conn to the same mpegts.Demuxer used for SRT Listen (and every
// other MPEG-TS-carrying ingest path). No SCTE-35 or MPEG-TS parsing is
// duplicated here or anywhere else per-protocol; both SRT modes, and any
// future MPEG-TS-carrying transport (UDP, HTTP, RIST, ...), share the one
// demuxer.
func Pull(ctx context.Context, url string, bus *media.Bus, passphrase string, pbkeylen int) error {
	dialCfg := gosrt.DefaultConfig()
	if passphrase != "" {
		dialCfg.Passphrase = passphrase
		dialCfg.EnforcedEncryption = true
		if pbkeylen != 0 {
			dialCfg.PBKeylen = pbkeylen
		}
	}
	// gosrt.Dial's address argument is a bare host:port (network is the
	// separate "srt" argument, mirroring net.Dial) — but the panel's Pull
	// Sources URL field naturally invites a full "srt://host:port" like
	// every other protocol's URL there, so strip the scheme if present
	// rather than failing with a "too many colons in address" dial error.
	dialAddr := strings.TrimPrefix(url, "srt://")
	conn, err := gosrt.Dial("srt", dialAddr, dialCfg)
	if err != nil {
		return fmt.Errorf("srt dial %s: %w", url, err)
	}
	defer conn.Close()

	demux := mpegts.NewDemuxer(bus)
	demux.SetLogContext("source", "srt", "mode", "pull", "url", url)
	demux.OnBytes = func(n int) { bus.AddBytesIn(int64(n)) }
	if err := demux.Run(ctx, conn); err != nil {
		slog.Warn("srt: pull demux ended with error", "url", url, "err", err)
		return err
	}
	return nil
}
