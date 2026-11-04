// Origin Media Server — entry point.
// Single binary: no external database, no FFmpeg, no separate processes required.
//
// Usage:
//
//	./origin-mediaserver --config config.toml
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/api"
	"github.com/5centscdn/origin-mediaserver/internal/config"
	dbpkg "github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/dvr"
	"github.com/5centscdn/origin-mediaserver/internal/dvrowner"
	"github.com/5centscdn/origin-mediaserver/internal/failover"
	"github.com/5centscdn/origin-mediaserver/internal/hls"
	"github.com/5centscdn/origin-mediaserver/internal/icecast"
	"github.com/5centscdn/origin-mediaserver/internal/livestore"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
	"github.com/5centscdn/origin-mediaserver/internal/node"
	"github.com/5centscdn/origin-mediaserver/internal/pull"
	"github.com/5centscdn/origin-mediaserver/internal/rtmp"
	"github.com/5centscdn/origin-mediaserver/internal/scte35ctrl"
	"github.com/5centscdn/origin-mediaserver/internal/srt"
	"github.com/5centscdn/origin-mediaserver/internal/streamoutputs"
	"github.com/5centscdn/origin-mediaserver/internal/sysmon"
	"github.com/5centscdn/origin-mediaserver/internal/webrtc"
)

func main() {
	configPath := flag.String("config", "config.toml", "path to configuration file")
	flag.Parse()

	// ── Load configuration ───────────────────────────────────────────────────
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	// ── Configure structured logging ─────────────────────────────────────────
	var logHandler slog.Handler
	logLevel := slog.LevelInfo
	switch cfg.Server.LogLevel {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: logLevel}
	if cfg.Server.LogFormat == "json" {
		logHandler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		logHandler = slog.NewTextHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(logHandler))

	slog.Info("Origin Media Server starting",
		"node", cfg.Server.NodeName,
		"data_dir", cfg.Server.DataDir,
	)

	// ── Create data directory ────────────────────────────────────────────────
	if err := os.MkdirAll(cfg.Server.DataDir, 0o755); err != nil {
		slog.Error("create data dir", "err", err)
		os.Exit(1)
	}

	// ── Root context — cancelled on SIGINT / SIGTERM ─────────────────────────
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// ── Open embedded SQLite database ────────────────────────────────────────
	// modernc.org/sqlite is a pure-Go SQLite port — no CGO, no external .so.
	database, err := dbpkg.Open(ctx, cfg.Server.DataDir)
	if err != nil {
		slog.Error("open database", "err", err)
		os.Exit(1)
	}
	defer database.Close()

	// Live HLS/DASH output is served from RAM; under memory pressure it
	// moves to disk instead of risking an OOM kill (internal/livestore).
	// A choice saved from the panel's Settings page wins over config.toml.
	lc := cfg.LiveCache
	var saved api.LiveCacheSetting
	if ok, err := database.GetSetting(ctx, api.LiveCacheSettingKey, &saved); err != nil {
		slog.Warn("read saved live cache setting", "err", err)
	} else if ok {
		lc.Mode, lc.MemoryHighPct, lc.MemoryLowPct = saved.Mode, saved.MemoryHighPct, saved.MemoryLowPct
		slog.Info("live cache: using the setting saved from the panel (overrides config.toml)")
	}
	livestore.Default().StartMonitor(lc.Mode, lc.MemoryHighPct, lc.MemoryLowPct,
		time.Duration(lc.CheckIntervalS)*time.Second)

	// ── Core components ──────────────────────────────────────────────────────
	registry := media.NewRegistry()
	m := metrics.New()
	dvrEngine := dvr.NewEngine(cfg, database, m)
	hlsPlaylists := hls.NewPlaylistRegistry()

	// ── Multi-node cluster (optional) ────────────────────────────────────────
	// Peer discovery/health over NATS (internal/node), plus nDVR stream
	// ownership (internal/dvrowner): which node answers an nDVR stream's
	// playlists behind a CDN. Set up before any ingest starts so the DVR
	// engine reports every recording to the tracker. Started further down.
	var nodeMgr *node.Manager
	var dvrOwner *dvrowner.Tracker
	if cfg.Multinode.Enabled {
		nodeMgr = node.NewManager(cfg, registry, m, cfg.Multinode.AdvertiseURL)
		dvrOwner = dvrowner.New(nodeMgr, dvrowner.NewDeps(registry, dvrEngine,
			func(ctx context.Context, app, stream string) bool {
				return streamoutputs.DVREnabled(ctx, cfg, database, app, stream)
			}), cfg.Multinode.OwnershipPolicy)
		dvrEngine.SetObserver(dvrOwner)
		nodeMgr.SetDVRReporter(dvrOwner.Report)
	}
	failoverMgr := failover.NewManager(cfg, registry, m)

	// OnBackupLive: fired when a failover backup source is registered, so it
	// gets the exact same HLS/DVR output wiring a normal RTMP publish would.
	failoverMgr.OnBackupLive = func(stream *media.Stream) {
		m.ActiveStreams.Set(float64(len(registry.All())))
		m.ActiveABRGroups.Set(float64(len(registry.AllABRGroups())))
		streamoutputs.Start(ctx, cfg, database, dvrEngine, hlsPlaylists, stream, m)
	}

	// Load and start monitoring already-enabled failover rules — otherwise
	// only rules created/edited after startup (via the API) would ever run.
	if rules, err := database.ListEnabledFailoverRules(ctx); err != nil {
		slog.Warn("load failover rules", "err", err)
	} else {
		for _, r := range rules {
			failoverMgr.Watch(ctx, failover.Rule{
				AppName:        r.AppName,
				PrimaryStream:  r.PrimaryStream,
				BackupURL:      r.BackupURL,
				BackupProtocol: r.BackupProtocol,
				StallTimeoutMs: r.StallTimeoutMs,
				WarmupMs:       r.WarmupMs,
				AutoRestore:    r.AutoRestore,
			})
		}
	}

	pullMgr := pull.NewManager(database, registry, m)
	scte35Mgr := scte35ctrl.NewManager(ctx, database, registry, m)

	// OnLive: fired when a pull source's stream is registered, so it gets
	// the exact same HLS/DVR output wiring a normal RTMP publish would.
	pullMgr.OnLive = func(stream *media.Stream) {
		m.ActiveStreams.Set(float64(len(registry.All())))
		m.ActiveABRGroups.Set(float64(len(registry.AllABRGroups())))
		streamoutputs.Start(ctx, cfg, database, dvrEngine, hlsPlaylists, stream, m)
	}

	// Reset any 'active' status left over from a previous crash (nothing is
	// actually running yet), then auto-start every non-on-demand source.
	if err := database.ResetAllPullSourceStatus(ctx, "inactive"); err != nil {
		slog.Warn("reset pull source status", "err", err)
	}
	if sources, err := database.ListAutoStartPullSources(ctx); err != nil {
		slog.Warn("load pull sources", "err", err)
	} else {
		for _, src := range sources {
			if err := pullMgr.Start(ctx, src); err != nil {
				slog.Warn("auto-start pull source", "id", src.ID, "err", err)
			}
		}
	}

	// ── HTTP API + Delivery + Panel ──────────────────────────────────────────
	webrtcMgr := webrtc.NewManager(cfg, registry, m)

	// OnPublish: fired for every new WHIP-ingested stream, so it gets the
	// exact same HLS/DASH/DVR output wiring a normal RTMP publish would.
	webrtcMgr.OnPublish = func(stream *media.Stream) {
		m.ActiveStreams.Set(float64(len(registry.All())))
		m.ActiveABRGroups.Set(float64(len(registry.AllABRGroups())))
		streamoutputs.Start(ctx, cfg, database, dvrEngine, hlsPlaylists, stream, m)
	}

	if nodeMgr != nil {
		go dvrOwner.Run(ctx)
		go func() {
			if err := nodeMgr.Run(ctx); err != nil && ctx.Err() == nil {
				slog.Error("multinode: cluster manager stopped", "err", err)
			}
		}()
	}

	apiServer := api.New(ctx, cfg, database, registry, dvrEngine, failoverMgr, pullMgr, hlsPlaylists, webrtcMgr, nodeMgr, m, scte35Mgr)
	if dvrOwner != nil {
		apiServer.SetDVROwner(dvrOwner)
	}

	// Dashboard metrics: one goroutine sampling /proc every 5 s.
	monCfg := sysmon.Config{
		DataDir:    cfg.Server.DataDir,
		DVRPath:    cfg.DVR.StoragePath,
		HTTPListen: cfg.HTTP.Listen,
		RTMPListen: cfg.RTMP.Listen,
	}
	if cfg.HTTP.TLSEnabled {
		monCfg.HTTPSListen = cfg.HTTP.TLSListen
	}
	if cfg.RTMP.TLSEnabled {
		monCfg.RTMPSListen = cfg.RTMP.TLSListen
	}
	if cfg.SRT.Enabled {
		monCfg.SRTListen = cfg.SRT.Listen
	}
	monitor := sysmon.New(monCfg, sysmon.Sources{
		Registry:  registry,
		PullStats: pullMgr.Stats,
		DVRBytes: func(ctx context.Context) (int64, error) {
			var n int64
			err := database.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes), 0) FROM dvr_segments`).Scan(&n)
			return n, err
		},
	})
	go monitor.Run(ctx)
	apiServer.SetMonitor(monitor)
	if err := apiServer.EnsureBootstrapAdmin(ctx); err != nil {
		slog.Error("bootstrap admin", "err", err)
		os.Exit(1)
	}

	httpServer := &http.Server{
		Addr:         cfg.HTTP.Listen,
		Handler:      apiServer.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // 0 = no timeout (HLS segments can be large; LL-HLS uses long-poll)
		IdleTimeout:  120 * time.Second,
	}

	// httpsServer is a second, independent listener on cfg.HTTP.TLSListen —
	// plain HTTP on httpServer above keeps running unchanged when TLS is
	// enabled (see HTTPConfig.TLSListen's doc comment), so URLs already
	// handed out over http://<host><cfg.HTTP.Listen>/... keep working.
	var httpsServer *http.Server
	if cfg.HTTP.TLSEnabled {
		httpsServer = &http.Server{
			Addr:         cfg.HTTP.TLSListen,
			Handler:      apiServer.Handler(),
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 0,
			IdleTimeout:  120 * time.Second,
		}
	}

	// ── RTMP server ──────────────────────────────────────────────────────────
	rtmpServer := rtmp.NewServer(cfg, registry, database, m)

	// OnPublish: fired for every new stream.
	rtmpServer.OnPublish = func(stream *media.Stream) {
		m.ActiveStreams.Set(float64(len(registry.All())))
		m.ActiveABRGroups.Set(float64(len(registry.AllABRGroups())))
		streamoutputs.Start(ctx, cfg, database, dvrEngine, hlsPlaylists, stream, m)
	}

	rtmpServer.OnUnpublish = func(stream *media.Stream) {
		m.ActiveStreams.Set(float64(len(registry.All())))
		m.ActiveABRGroups.Set(float64(len(registry.AllABRGroups())))
		slog.Info("stream stopped", "stream", stream.Key)
	}

	// ── SRT server (optional) ─────────────────────────────────────────────────
	var srtServer *srt.Server
	if cfg.SRT.Enabled {
		srtServer = srt.NewServer(cfg, registry, database)
		srtServer.SetMetrics(m)
		srtServer.OnPublish = func(stream *media.Stream) {
			m.ActiveStreams.Set(float64(len(registry.All())))
			m.ActiveABRGroups.Set(float64(len(registry.AllABRGroups())))
			streamoutputs.Start(ctx, cfg, database, dvrEngine, hlsPlaylists, stream, m)
		}
		srtServer.OnUnpublish = func(stream *media.Stream) {
			m.ActiveStreams.Set(float64(len(registry.All())))
			m.ActiveABRGroups.Set(float64(len(registry.AllABRGroups())))
			slog.Info("stream stopped", "stream", stream.Key)
		}
	}

	// ── Icecast server (optional) ─────────────────────────────────────────────
	var icecastServer *icecast.Server
	if cfg.Icecast.Enabled {
		icecastServer = icecast.NewServer(cfg, registry, database)
		icecastServer.OnPublish = func(stream *media.Stream) {
			m.ActiveStreams.Set(float64(len(registry.All())))
			m.ActiveABRGroups.Set(float64(len(registry.AllABRGroups())))
			streamoutputs.Start(ctx, cfg, database, dvrEngine, hlsPlaylists, stream, m)
		}
		icecastServer.OnUnpublish = func(stream *media.Stream) {
			m.ActiveStreams.Set(float64(len(registry.All())))
			m.ActiveABRGroups.Set(float64(len(registry.AllABRGroups())))
			slog.Info("stream stopped", "stream", stream.Key)
		}
	}

	// ── DVR cleanup background task ──────────────────────────────────────────
	// Every minute regardless of window size: windows are per app/stream
	// (dvr.Options.WindowS) and a pass with nothing to delete is one
	// indexed query per recorded stream.
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := dvrEngine.CleanupOldSegments(ctx); err != nil {
					slog.Warn("dvr cleanup", "err", err)
				}
			}
		}
	}()

	// ── Start all listeners ──────────────────────────────────────────────────
	errCh := make(chan error, 5)

	// HTTP — always plain HTTP on cfg.HTTP.Listen, TLS status notwithstanding.
	go func() {
		slog.Info("HTTP server listening", "addr", cfg.HTTP.Listen)
		errCh <- httpServer.ListenAndServe()
	}()

	// HTTPS — only if enabled, on its own address (cfg.HTTP.TLSListen).
	if httpsServer != nil {
		go func() {
			slog.Info("HTTPS server listening", "addr", cfg.HTTP.TLSListen)
			errCh <- httpsServer.ListenAndServeTLS(cfg.HTTP.TLSCert, cfg.HTTP.TLSKey)
		}()
	}

	// RTMP
	go func() {
		errCh <- rtmpServer.ListenAndServe(ctx)
	}()

	// SRT — only if enabled
	if srtServer != nil {
		go func() {
			errCh <- srtServer.ListenAndServe(ctx)
		}()
	}

	// Icecast — only if enabled
	if icecastServer != nil {
		go func() {
			errCh <- icecastServer.ListenAndServe(ctx)
		}()
	}

	// RTMPS (TLS) — only if enabled
	if cfg.RTMP.TLSEnabled {
		go func() {
			cert, err := tls.LoadX509KeyPair(cfg.HTTP.TLSCert, cfg.HTTP.TLSKey)
			if err != nil {
				errCh <- fmt.Errorf("load TLS cert: %w", err)
				return
			}
			tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}}
			errCh <- rtmpServer.ListenAndServeTLS(ctx, tlsCfg)
		}()
	}

	// ── Wait for shutdown ────────────────────────────────────────────────────
	select {
	case <-ctx.Done():
		slog.Info("shutting down...")

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			slog.Warn("http shutdown", "err", err)
		}
		if httpsServer != nil {
			if err := httpsServer.Shutdown(shutdownCtx); err != nil {
				slog.Warn("https shutdown", "err", err)
			}
		}

		slog.Info("shutdown complete")

	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}
}

// Stream output start/reload logic (HLS/DASH packaging, DVR recording,
// SCTE-35 config resolution) lives in internal/streamoutputs, shared with
// internal/api's per-stream reload endpoint. Both a normal RTMP publish
// (rtmpServer.OnPublish) and a failover backup pull (failoverMgr.OnBackupLive)
// go through streamoutputs.Start, so a backup stream is indistinguishable
// from a primary one to every downstream consumer.
