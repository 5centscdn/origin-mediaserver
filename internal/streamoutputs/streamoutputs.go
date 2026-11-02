// Package streamoutputs resolves a stream's effective application/stream-
// config DB settings (HLS/DASH toggles, SCTE-35 mode, ad marker format, DVR)
// and (re)starts its output pipeline accordingly. It is shared by
// cmd/server/main.go — which calls Start exactly once per stream
// registration, from every ingest protocol's OnPublish/OnLive callback —
// and internal/api's stream-reload endpoint, which calls Reload to re-apply
// a changed app/stream config to an already-live stream without touching
// its ingest connection, its registry entry, or any other stream.
package streamoutputs

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/dash"
	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/dvr"
	"github.com/5centscdn/origin-mediaserver/internal/hls"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
)

// ResolveSCTE35Mode looks up app's effective scte35_mode for stream name —
// the same app/stream-config resolution apply (below) does inline for the
// HLS/DASH/DVR pipeline, factored out here so other output paths that
// don't go through Start/Reload at all (internal/tsout's continuous
// MPEG-TS-over-HTTP handler, which is per-request, not per-stream-
// registration) can still gate their own SCTE-35 forwarding identically.
// Falls back to "disabled" when the app isn't configured in the DB, same
// as apply's own default.
func ResolveSCTE35Mode(ctx context.Context, database *db.DB, app, name string) string {
	a, err := database.GetApplicationByName(ctx, app)
	if err != nil || a == nil {
		return "disabled"
	}
	mode := a.SCTE35Mode
	if streamCfg, err := database.GetStreamConfig(ctx, a.ID, name); err == nil && streamCfg != nil {
		mode = db.EffectiveSCTE35Mode(streamCfg.SCTE35Mode, mode)
	}
	return mode
}

// Start begins the output pipeline for a newly-registered stream, and
// arranges for it to be torn down automatically once the stream's Bus
// closes (i.e. the underlying ingest connection ends). Call exactly once
// per stream registration — to re-apply a changed config to an
// already-live stream, use Reload instead.
func Start(
	ctx context.Context,
	cfg *config.Config,
	database *db.DB,
	dvrEngine *dvr.Engine,
	hlsPlaylists *hls.PlaylistRegistry,
	stream *media.Stream,
	m *metrics.Metrics,
) {
	streamCtx, streamCancel := context.WithCancel(ctx)
	stream.SetLifetimeCtx(streamCtx)

	go func() {
		sub := stream.Bus.Subscribe(streamCtx)
		defer sub.Close()
		for range sub.Chan() {
		}
		streamCancel()
	}()

	apply(streamCtx, cfg, database, dvrEngine, hlsPlaylists, stream, m)
}

// Reload re-resolves stream's effective app/stream-config from the DB and
// restarts its output pipeline (HLS/DASH packagers, DVR recording) — e.g.
// after an admin changes an application's SCTE-35 mode or DASH toggle while
// a stream using it is already live. The underlying ingest connection
// (RTMP/SRT socket, pull-manager connection) and the stream's registration
// in media.Registry are both left completely untouched; only the output
// goroutines started by the previous Start/Reload call are stopped and
// replaced. Returns an error if stream's output pipeline was never started
// (Start wasn't called — shouldn't happen for anything returned by
// Registry.Get/All) or has already ended.
func Reload(
	cfg *config.Config,
	database *db.DB,
	dvrEngine *dvr.Engine,
	hlsPlaylists *hls.PlaylistRegistry,
	stream *media.Stream,
	m *metrics.Metrics,
) error {
	parentCtx := stream.LifetimeCtx()
	if parentCtx == nil {
		return fmt.Errorf("stream %s outputs were never started", stream.Key)
	}
	if parentCtx.Err() != nil {
		return fmt.Errorf("stream %s output lifetime has already ended", stream.Key)
	}
	apply(parentCtx, cfg, database, dvrEngine, hlsPlaylists, stream, m)
	return nil
}

// apply resolves effective config and (re)starts HLS/DASH/DVR, parented on
// parentCtx via a fresh child context — cancelling whatever output
// generation was previously running for this stream first, so repeated
// calls (from Reload) neither leak goroutines nor double-run packagers.
func apply(
	parentCtx context.Context,
	cfg *config.Config,
	database *db.DB,
	dvrEngine *dvr.Engine,
	hlsPlaylists *hls.PlaylistRegistry,
	stream *media.Stream,
	m *metrics.Metrics,
) {
	stream.CancelOutputs()
	ctx, outputsCancel := context.WithCancel(parentCtx)
	stream.SetOutputsCancel(outputsCancel)

	// Look up per-application and per-stream output settings from the DB.
	// Falls back to global config defaults when the app isn't in the DB.
	var appCfg *db.AppConfig
	var streamCfg *db.StreamConfig
	if a, err := database.GetApplicationByName(ctx, stream.Key.App); err == nil && a != nil {
		appCfg = a
		streamCfg, _ = database.GetStreamConfig(ctx, a.ID, stream.Key.Name)
	}

	// Resolve effective protocol flags.
	outHLS := true   // HLS on by default for config-managed apps
	outDASH := false // DASH off by default (opt-in), unlike HLS
	// nDVR: config.toml's [dvr] enabled records every stream by default;
	// an application can switch it on for its own streams, and a stream
	// config's -1/0/1 override wins over both. The window resolves the same
	// way, with 0 meaning "inherit".
	outDVR := cfg.DVREnabled()
	dvrOpts := dvr.Options{}
	outDVBDash := false      // DVB-DASH profile: off by default (opt-in), narrowed further below by config.toml's global master switch
	outDVBSubs := false      // EBU-TT-D subtitles: same
	scte35Mode := "disabled" // outputs.scte35_mode, "disabled" by default
	adMarkerFormat := "none" // HLS ad_marker_format, "none" by default
	if appCfg != nil {
		outHLS = appCfg.OutHLS
		outDASH = appCfg.OutDASH
		outDVR = outDVR || appCfg.DVREnabled
		dvrOpts.WindowS = appCfg.DVRWindowS
		dvrOpts.SegmentDurationS = appCfg.HLSSegDurationS
		outDVBDash = appCfg.DVBDashEnabled
		outDVBSubs = appCfg.DVBSubtitlesEnabled
		scte35Mode = appCfg.SCTE35Mode
		adMarkerFormat = appCfg.AdMarkerFormat
	}
	if streamCfg != nil {
		outHLS = db.EffectiveOut(streamCfg.OutHLS, outHLS)
		outDASH = db.EffectiveOut(streamCfg.OutDASH, outDASH)
		if streamCfg.DVREnabled == 1 {
			outDVR = true
		} else if streamCfg.DVREnabled == 0 {
			outDVR = false
		}
		if streamCfg.DVRWindowS > 0 {
			dvrOpts.WindowS = streamCfg.DVRWindowS
		}
		outDVBDash = db.EffectiveOut(streamCfg.DVBDashEnabled, outDVBDash)
		outDVBSubs = db.EffectiveOut(streamCfg.DVBSubtitlesEnabled, outDVBSubs)
		scte35Mode = db.EffectiveSCTE35Mode(streamCfg.SCTE35Mode, scte35Mode)
		adMarkerFormat = db.EffectiveAdMarkerFormat(streamCfg.AdMarkerFormat, adMarkerFormat)
	}
	// config.toml's [dash.dvb] flags are the master switches — the per-app/
	// stream DB toggles above only ever narrow them further, never turn a
	// flag on when the global feature is off.
	outDVBDash = outDVBDash && cfg.DASH.DVB.Enabled
	outDVBSubs = outDVBSubs && cfg.DASH.DVB.SubtitlesEnabled

	if outHLS {
		packager, err := hls.NewPackager(cfg, stream, cfg.Server.DataDir, hlsPlaylists)
		if err != nil {
			slog.Error("hls packager init", "stream", stream.Key, "err", err)
			outputsCancel()
			return
		}
		packager.SetSCTE35Config(scte35Mode, adMarkerFormat)
		packager.SetMetrics(m)
		go packager.Run(ctx)
		slog.Info("HLS packager started", "stream", stream.Key,
			"url", "/"+stream.Key.App+"/"+stream.Key.Name+"/playlist.m3u8")
	}

	if outDASH {
		dashPackager, err := dash.NewPackager(cfg, stream, cfg.Server.DataDir)
		if err != nil {
			slog.Error("dash packager init", "stream", stream.Key, "err", err)
		} else {
			dashPackager.SetDVBConfig(outDVBDash, outDVBSubs)
			go dashPackager.Run(ctx)
			slog.Info("DASH packager started", "stream", stream.Key,
				"url", cfg.DASH.PathPrefix+"/"+stream.Key.App+"/"+stream.Key.Name+"/manifest.mpd")
		}
	}

	if outDVR {
		dvrEngine.StartRecording(ctx, stream, dvrOpts)
	}
}
