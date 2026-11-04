// Package api wires together all HTTP handlers: REST API, HLS/DASH delivery,
// WebRTC WHIP/WHEP, and the embedded management panel.
package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/5centscdn/origin-mediaserver/internal/sysmon"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/dvr"
	"github.com/5centscdn/origin-mediaserver/internal/dvrowner"
	"github.com/5centscdn/origin-mediaserver/internal/failover"
	"github.com/5centscdn/origin-mediaserver/internal/hls"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
	"github.com/5centscdn/origin-mediaserver/internal/node"
	"github.com/5centscdn/origin-mediaserver/internal/pull"
	"github.com/5centscdn/origin-mediaserver/internal/scte35ctrl"
	"github.com/5centscdn/origin-mediaserver/internal/webrtc"
)

// Server holds all dependencies for the HTTP layer.
type Server struct {
	monitor      *sysmon.Collector // dashboard metrics; nil = /monitor returns 503
	cfg          *config.Config
	db           *db.DB
	registry     *media.Registry
	dvr          *dvr.Engine
	failover     *failover.Manager
	pull         *pull.Manager
	metrics      *metrics.Metrics
	hlsPlaylists *hls.PlaylistRegistry
	webrtcMgr    *webrtc.Manager
	nodeMgr      *node.Manager     // nil when multinode.enabled is false
	dvrOwner     *dvrowner.Tracker // nil when multinode.enabled is false
	scte35       *scte35ctrl.Manager

	// baseCtx is the process's root context (not a request's r.Context(),
	// which is cancelled as soon as the response is sent) — used when a
	// handler needs to start something long-lived, like failover.Manager.Watch
	// or pull.Manager.Start.
	baseCtx context.Context
}

// New creates the HTTP server. nodeMgr may be nil (multinode.enabled is
// false) — handlers must check for that before calling into it.
func New(
	baseCtx context.Context,
	cfg *config.Config,
	database *db.DB,
	registry *media.Registry,
	dvrEngine *dvr.Engine,
	failoverMgr *failover.Manager,
	pullMgr *pull.Manager,
	hlsPlaylists *hls.PlaylistRegistry,
	webrtcMgr *webrtc.Manager,
	nodeMgr *node.Manager,
	m *metrics.Metrics,
	scte35Mgr *scte35ctrl.Manager,
) *Server {
	return &Server{
		cfg:          cfg,
		db:           database,
		registry:     registry,
		dvr:          dvrEngine,
		failover:     failoverMgr,
		pull:         pullMgr,
		hlsPlaylists: hlsPlaylists,
		webrtcMgr:    webrtcMgr,
		nodeMgr:      nodeMgr,
		metrics:      m,
		baseCtx:      baseCtx,
		scte35:       scte35Mgr,
	}
}

// Handler builds and returns the root HTTP mux.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RealIP)
	r.Use(middleware.RequestID)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(corsMiddleware(s.cfg.HTTP.CORSOrigins))

	// ── Prometheus metrics ───────────────────────────────────────────────────
	if s.cfg.Metrics.Enabled {
		r.Get(s.cfg.Metrics.Path, s.metrics.Handler())
	}

	// ── DVB-DASH UTC time source ─────────────────────────────────────────────
	// Self-hosted (not a third-party time service) so a node keeps working
	// standalone — same principle already applied to SQLite/NATS — for DASH
	// clients resolving [dash.dvb]'s UTCTiming element (internal/dash/mpd.go).
	// Deliberately unauthenticated: DASH clients hit this directly, with no
	// JWT of their own.
	r.Get("/dash-utc-time", s.handleDASHUTCTime)

	// /hls/ legacy routes intentionally removed — use /{app}/{stream}/playlist.m3u8

	// ── DVR rewind (legacy URL) ──────────────────────────────────────────────
	// GET /dvr/{app}/{stream}/index.m3u8?start=<ms>&end=<ms> — kept for old
	// links; the nDVR URLs live under /{app}/{stream}/ (see below).
	r.Route("/dvr", func(r chi.Router) {
		r.Get("/{app}/{stream}/index.m3u8", s.handleLegacyDVRPlaylist)
		// The panel's own "DVR" settings page also lives at /dvr (Next.js
		// route panel/src/app/dvr/). Mounting this sub-router at "/dvr"
		// otherwise shadows that page entirely: chi's own 404 fires for
		// "/dvr" and "/dvr/" (neither matches the two patterns above)
		// before the request ever reaches the panel catch-all
		// (r.Handle("/*", s.panelHandler()) below). Falling through to the
		// panel handler here fixes that without changing either the
		// panel's URL or the DVR playback URL scheme.
		r.NotFound(s.panelHandler().ServeHTTP)
	})

	// ── WebRTC WHIP ingest ───────────────────────────────────────────────────
	// POST /whip/{app}/{stream}
	r.Route("/whip", func(r chi.Router) {
		r.Post("/{app}/{stream}", s.handleWHIP)
		r.Delete("/{app}/{stream}/{resource}", s.handleWHIPDelete)
	})

	// ── WebRTC WHEP playback ─────────────────────────────────────────────────
	// POST /whep/{app}/{stream}
	// DELETE /whep/{app}/{stream}/{resource}
	r.Route("/whep", func(r chi.Router) {
		r.Post("/{app}/{stream}", s.handleWHEP)
		r.Delete("/{app}/{stream}/{resource}", s.handleWHEPDelete)
	})

	// ── REST API ─────────────────────────────────────────────────────────────
	r.Route("/api/v1", func(r chi.Router) {
		// Public: auth
		r.Post("/auth/login", s.handleLogin)
		r.Post("/auth/refresh", s.handleRefresh)

		// Protected endpoints
		r.Group(func(r chi.Router) {
			r.Use(s.authMiddleware)

			r.Post("/auth/logout", s.handleLogout)

			// Users: admin only — any lower role managing users could just
			// promote itself.
			r.Group(func(r chi.Router) {
				r.Use(s.requireRole("admin"))

				// Saved server list (servers table): servers added by hand on
				// the panel's Cluster → Servers page, shown next to the peers
				// discovered over NATS (/cluster). Admin only.
				r.Get("/servers", s.handleListServers)
				r.Post("/servers", s.handleAddServer)
				r.Delete("/servers/{id}", s.handleDeleteServer)
				r.Get("/users", s.handleListUsers)
				r.Post("/users", s.handleCreateUser)
				r.Put("/users/{id}", s.handleUpdateUser)
				r.Delete("/users/{id}", s.handleDeleteUser)
			})

			// API tokens: admin, or the user managing their own. A token
			// carries its owner's role, so self-service grants nothing new.
			r.Group(func(r chi.Router) {
				r.Use(s.requireSelfOrRole("admin"))
				r.Get("/users/{id}/tokens", s.handleListTokens)
				r.Post("/users/{id}/tokens", s.handleCreateToken)
				r.Delete("/users/{id}/tokens/{tid}", s.handleDeleteToken)
			})

			// Everything else: viewers read, operators change.
			r.Group(func(r chi.Router) {
				r.Use(s.requireRoleByMethod)

				// Streams (live state). Two literal path segments ({app}/{name}),
				// not a single {id}="app/name" composite — chi v5 matches on
				// RawPath and never unescapes, so a client sending a "%2F"-
				// escaped id (as encodeURIComponent("app/name") produces) hands
				// the handler that literal escape sequence back, unsplit; a real
				// two-segment route sidesteps that entirely.
				r.Get("/streams", s.handleListStreams)
				r.Get("/streams/{app}/{name}", s.handleGetStream)
				r.Post("/streams/{app}/{name}/kick", s.handleKickStream)
				r.Post("/streams/{app}/{name}/reload", s.handleReloadStream)
				r.Post("/streams/{app}/{name}/failover", s.handleTriggerFailover)
				r.Get("/streams/{app}/{name}/dvr", s.handleStreamDVRInfo)

				// Server-side SCTE-35 insertion (internal/scte35ctrl) — same
				// two-segment convention as the routes above.
				r.Post("/streams/{app}/{name}/scte35", s.handleInsertSCTE35)
				r.Get("/streams/{app}/{name}/scte35", s.handleListSCTE35)
				r.Delete("/streams/{app}/{name}/scte35/{event_id}", s.handleCancelSCTE35)

				// ABR groups
				r.Get("/abr-groups", s.handleListABRGroups)
				r.Post("/abr-groups", s.handleCreateABRGroup)
				r.Get("/abr-groups/{id}", s.handleGetABRGroup)
				r.Put("/abr-groups/{id}", s.handleUpdateABRGroup)
				r.Delete("/abr-groups/{id}", s.handleDeleteABRGroup)
				r.Put("/abr-groups/{id}/tracks", s.handleReplaceABRTracks)
				r.Get("/abr-groups/{id}/variants", s.handleListABRVariants)
				r.Post("/abr-groups/{id}/variants", s.handleAddABRVariant)
				r.Put("/abr-groups/{id}/variants/{vid}", s.handleUpdateABRVariant)
				r.Delete("/abr-groups/{id}/variants/{vid}", s.handleDeleteABRVariant)

				// Pull sources
				r.Get("/pull-sources", s.handleListPullSources)
				r.Post("/pull-sources", s.handleCreatePullSource)
				r.Put("/pull-sources/{id}", s.handleUpdatePullSource)
				r.Delete("/pull-sources/{id}", s.handleDeletePullSource)
				r.Post("/pull-sources/{id}/start", s.handleStartPull)
				r.Post("/pull-sources/{id}/stop", s.handleStopPull)

				// Failover rules
				r.Get("/failover-rules", s.handleListFailoverRules)
				r.Post("/failover-rules", s.handleCreateFailoverRule)
				r.Put("/failover-rules/{id}", s.handleUpdateFailoverRule)
				r.Delete("/failover-rules/{id}", s.handleDeleteFailoverRule)
				r.Post("/failover-rules/{id}/trigger", s.handleTriggerFailoverRule)

				// DVR config
				r.Get("/dvr-config", s.handleGetDVRConfig)
				r.Put("/dvr-config", s.handleUpdateDVRConfig)

				// Dashboard monitoring (internal/sysmon)
				r.Get("/monitor", s.handleMonitor)
				r.Get("/monitor/history", s.handleMonitorHistory)

				// Live cache (where live HLS/DASH is kept); read via GET /node
				r.Put("/live-cache-config", s.handleUpdateLiveCacheConfig)

				// Applications + per-stream configs
				r.Get("/applications", s.handleListApplications)
				r.Get("/applications/{id}", s.handleGetApplication)
				r.Get("/applications/{id}/streams", s.handleListStreamConfigs)
				// Writes can switch nDVR on or off, which changes which
				// streams take part in cluster ownership.
				r.Group(func(r chi.Router) {
					r.Use(s.invalidateDVROwnership)
					r.Post("/applications", s.handleCreateApplication)
					r.Put("/applications/{id}", s.handleUpdateApplication)
					r.Delete("/applications/{id}", s.handleDeleteApplication)
					r.Post("/applications/{id}/streams", s.handleCreateStreamConfig)
					r.Put("/applications/{id}/streams/{sid}", s.handleUpdateStreamConfig)
					r.Delete("/applications/{id}/streams/{sid}", s.handleDeleteStreamConfig)
				})

				// Node info (this node only — multi-node adds more here)
				r.Get("/node", s.handleNodeInfo)

				// Multi-node cluster: peers auto-discovered via NATS heartbeat
				// (see internal/node) — read-only, distinct from the manually-
				// curated "servers" bookmark list below.
				r.Get("/nodes", s.handleListNodes)
				// Cluster → Servers page: multinode status + every known
				// node, including unreachable ones.
				r.Get("/cluster", s.handleCluster)

			})
		})
	})

	// ── Panel static assets — must be before HLS wildcards ──────────────────
	// Next.js puts all compiled assets under /_next/. A 5-segment /_next path
	// like /_next/static/chunks/app/page.js would otherwise match the HLS
	// /{app}/{stream}/{srcApp}/{srcStream}/{segment} wildcard route.
	r.Handle("/_next/*", s.panelHandler())

	// ── Nimble-compatible HLS/DASH/raw-TS delivery (no path prefix) ─────────
	// Single stream:
	//   GET /{app}/{stream}/playlist.m3u8  → HLS master playlist (1 variant → chunks.m3u8)
	//   GET /{app}/{stream}/chunks.m3u8   → HLS sliding-window media playlist
	//   GET /{app}/{stream}/{seg}.ts       → HLS segment
	//   GET /{app}/{stream}/manifest.mpd   → DASH manifest
	//   GET /{app}/{stream}/mpeg.2ts       → continuous raw MPEG-TS (internal/tsout)
	//
	// ABR group:
	//   GET /{abrApp}/{abrStream}/playlist.m3u8                        → multi-variant master
	//   GET /{abrApp}/{abrStream}/{srcApp}/{srcStream}/chunks.m3u8     → variant playlist
	//   GET /{abrApp}/{abrStream}/{srcApp}/{srcStream}/{seg}.ts        → variant segment
	//   GET /{abrApp}/{abrStream}/playlist_dvr*.m3u8                    → rewind master
	//   GET /{abrApp}/{abrStream}/{srcApp}/{srcStream}/playlist_dvr*.m3u8 → variant rewind
	//
	// hls.separate_audio_track mode (see internal/hls.Packager.useSplitAudio):
	//   GET /{app}/{stream}/audio/index.m3u8  → audio-only media playlist
	//   GET /{app}/{stream}/audio/{seg}       → audio-only segment/init
	//
	// More-specific routes (more path segments / literal endings) are registered
	// first so chi's radix tree always resolves them before the wildcards.
	// DASH's own segments (init-{track}.mp4, seg-{track}-{seq}.m4s) share the
	// final {segment} wildcard with HLS's — handleDirectSegment disambiguates
	// by which on-disk directory (hls/ vs dash/) actually has the file,
	// since fmp4/cmaf-container HLS can use the same .m4s/.mp4 extensions
	// DASH does, so the extension alone can't tell them apart.
	r.Get("/{app}/{stream}/audio/index.m3u8", s.handleAudioChunks)
	r.Get("/{app}/{stream}/audio/{segment}", s.handleAudioSegment)
	r.Get("/{app}/{stream}/{srcApp}/{srcStream}/chunks.m3u8", s.handleVariantChunks)
	// ABR variant rewind (renderABRDVRMaster's variant URIs), same layout as live.
	r.Get("/{app}/{stream}/{srcApp}/{srcStream}/{dvrfile:(playlist|manifest)_dvr[^/]*}", s.handleVariantDVRManifest)
	r.Get("/{app}/{stream}/{srcApp}/{srcStream}/dvr/{session}/{file}", s.handleVariantDVRFile)
	r.Get("/{app}/{stream}/{srcApp}/{srcStream}/{segment}", s.handleVariantSegment)
	r.Get("/{app}/{stream}/playlist.m3u8", s.handlePlaylist)
	r.Get("/{app}/{stream}/chunks.m3u8", s.handleChunks)
	r.Get("/{app}/{stream}/manifest.mpd", s.handleDASHManifest)
	// Continuous, unsegmented MPEG-TS over one long-lived HTTP response
	// (internal/tsout) — the raw-TS alternative to the segmented .ts/
	// playlist delivery above, for a downstream consumer (e.g. a
	// transcoder) that wants to pull one live TS byte stream directly.
	// Named mpeg.2ts (not e.g. stream.ts) to match the real-world
	// convention other origins (e.g. Nimble) already use for this.
	r.Get("/{app}/{stream}/mpeg.2ts", s.handleLiveTS)
	// nDVR rewind (internal/dvr): playlist_dvr.m3u8 (whole rewind window,
	// live), playlist_dvr_range-{from}-{len}.m3u8 (VOD), playlist_dvr_
	// timeshift-{shift}[-{depth}].m3u8 (delayed live), and the same three
	// as manifest_dvr*.mpd for DASH — all from one fMP4 recording. A regexp
	// param so chi prefers it over the {segment} catch-all below.
	r.Get("/{app}/{stream}/{dvrfile:(playlist|manifest)_dvr[^/]*}", s.handleDVRManifest)
	r.Get("/{app}/{stream}/dvr/{session}/{file}", s.handleDVRFile)
	r.Get("/{app}/{stream}/{segment}", s.handleDirectSegment)

	// ── Management panel (embedded static files) ─────────────────────────────
	// In production: panel is compiled to static files and embedded via go:embed.
	// In development: proxy to Next.js dev server on :3000.
	r.Handle("/*", s.panelHandler())

	return r
}

// corsMiddleware adds permissive CORS headers for listed origins.
func corsMiddleware(origins []string) func(http.Handler) http.Handler {
	originsMap := make(map[string]bool)
	for _, o := range origins {
		originsMap[o] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if originsMap[origin] || originsMap["*"] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
				w.Header().Set("Access-Control-Max-Age", "86400")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// jsonError writes a JSON error response.
func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write([]byte(`{"error":"` + strings.ReplaceAll(msg, `"`, `'`) + `"}`))
}

// jsonOK writes a JSON success response.
func jsonOK(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}
