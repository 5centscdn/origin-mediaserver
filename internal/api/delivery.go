package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/5centscdn/origin-mediaserver/internal/dash"
	"github.com/5centscdn/origin-mediaserver/internal/dvr"
	"github.com/5centscdn/origin-mediaserver/internal/livestore"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/mux"
	"github.com/5centscdn/origin-mediaserver/internal/streamoutputs"
	"github.com/5centscdn/origin-mediaserver/internal/tsout"
	webrtcpkg "github.com/5centscdn/origin-mediaserver/internal/webrtc"
	panelfs "github.com/5centscdn/origin-mediaserver/panel"
)

// llhlsMaxWait bounds how long a blocking LL-HLS playlist reload can hold a
// connection open. Per spec, a server unable to satisfy the request within a
// reasonable time should just serve the current playlist, not error — the
// callers of awaitLLHLSUpdate do exactly that regardless of how it returns.
const llhlsMaxWait = 10 * time.Second

// awaitLLHLSUpdate implements LL-HLS blocking playlist reload: if the
// request carries _HLS_msn (optionally _HLS_part), it blocks until the
// stream's playlist has that segment/part, or until llhlsMaxWait elapses,
// whichever comes first. A no-op for ordinary requests (no _HLS_msn), for a
// stream that isn't currently live, or if LL-HLS isn't wired up at all.
func (s *Server) awaitLLHLSUpdate(r *http.Request, app, stream string) {
	if s.hlsPlaylists == nil {
		return
	}
	msn := parseIntQuery(r, "_HLS_msn", -1)
	if msn < 0 {
		return
	}
	part := parseIntQuery(r, "_HLS_part", -1)

	pl := s.hlsPlaylists.Get(media.StreamKey{App: app, Name: stream})
	if pl == nil {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), llhlsMaxWait)
	defer cancel()
	pl.WaitForUpdate(ctx, int(msn), int(part))
}

// ─── HLS Delivery ─────────────────────────────────────────────────────────────

func (s *Server) handleHLSMaster(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	stream := chi.URLParam(r, "stream")

	// For ABR groups, the "stream" param is the group name.
	// master.m3u8 lives in <data_dir>/hls/<app>/<group>/master.m3u8
	path := filepath.Join(s.cfg.Server.DataDir, "hls", app, stream, "master.m3u8")
	serveM3U8(w, r, path)
}

func (s *Server) handleHLSPlaylist(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	stream := chi.URLParam(r, "stream")

	path := filepath.Join(s.cfg.Server.DataDir, "hls", app, stream, "index.m3u8")
	serveM3U8(w, r, path)
}

func (s *Server) handleHLSSegment(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	stream := chi.URLParam(r, "stream")
	segment := chi.URLParam(r, "segment")

	// Sanitize — no path traversal.
	if !isSafeFileName(segment) {
		http.Error(w, "invalid segment name", http.StatusBadRequest)
		return
	}

	path := filepath.Join(s.cfg.Server.DataDir, "hls", app, stream, segment)
	serveSegment(w, r, path, segmentContentType(segment))
}

// ─── DASH Delivery ────────────────────────────────────────────────────────────

// handleDASHManifest serves /dash/{app}/{stream}/manifest.mpd.
//
// For ABR groups formed by RTMP config %i patterns (e.g. FMLE's camera_%i):
// the in-memory media.Registry is the live source of truth, mirroring
// handlePlaylist's registry-first HLS master playlist branch — a
// multi-bitrate MPD is rendered fresh on every request from each variant's
// live CodecInfo, pointing at each variant's independently-packaged
// segments via per-Representation BaseURL (see dash.RenderGroupMPD).
//
// For a single live stream: falls through to the static single-rendition
// manifest.mpd each stream's own dash.Packager already wrote to disk.
func (s *Server) handleDASHManifest(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	stream := chi.URLParam(r, "stream")
	s.registry.TouchViewer(media.StreamKey{App: app, Name: stream}, viewerClientID(r))

	if variants := s.registry.GetABRGroup(app, stream); len(variants) > 0 {
		params := dash.GroupManifestParams{
			AvailabilityStart: variants[0].StartedAt,
			SegmentDurationS:  s.cfg.DASH.SegmentDurationS,
			MinBufferTimeS:    s.cfg.DASH.MinBufferTimeS,
			DVRWindowS:        s.cfg.DASH.DVRWindowS,
			PathPrefix:        s.cfg.DASH.PathPrefix,
		}
		for _, v := range variants {
			vp := dash.GroupVariantParams{
				App:       v.Key.App,
				Stream:    v.Key.Name,
				Bandwidth: v.ABRBandwidthHint,
			}
			if ci := v.Bus.CodecInfo(); ci != nil {
				if len(ci.SPSNALu) > 0 {
					vp.HasVideo = true
					vp.VideoWidth, vp.VideoHeight = ci.Width, ci.Height
					vp.VideoCodecs = media.H264CodecString(ci)
				}
				if len(ci.AACConfig) > 0 {
					vp.HasAudio = true
					vp.AudioCodecs = media.AACCodecString(ci)
					vp.AudioSampleRate = mux.ParseAACSampleRate(ci.AACConfig)
					vp.AudioChannels = mux.ParseAACChannels(ci.AACConfig)
				}
			}
			params.Variants = append(params.Variants, vp)
		}

		data, err := dash.RenderGroupMPD(params)
		if err != nil {
			http.Error(w, "render mpd: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/dash+xml")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Write(data)
		return
	}

	path := filepath.Join(s.cfg.Server.DataDir, "dash", app, stream, "manifest.mpd")
	data, ok := livestore.Default().ReadFile(path)
	if !ok {
		http.Error(w, "stream not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/dash+xml")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(data)
}

// ─── Continuous MPEG-TS delivery (internal/tsout) ────────────────────────────

// handleLiveTS serves /{app}/{stream}/mpeg.2ts — a continuous, unsegmented
// MPEG-TS byte stream for the life of the HTTP connection, for a downstream
// consumer (e.g. a transcoder) that wants to pull raw TS directly rather
// than poll an HLS/DASH playlist. Public (no auth), matching every other
// media-delivery route in this section — see router.go's route grouping.
func (s *Server) handleLiveTS(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	stream := chi.URLParam(r, "stream")

	streamObj := s.registry.Get(media.StreamKey{App: app, Name: stream})
	if streamObj == nil {
		http.Error(w, "stream not found", http.StatusNotFound)
		return
	}
	streamObj.Viewers.Touch(viewerClientID(r))

	scte35Mode := streamoutputs.ResolveSCTE35Mode(r.Context(), s.db, app, stream)
	if err := tsout.Serve(r.Context(), w, streamObj, scte35Mode); err != nil {
		// The client disconnecting (ctx.Err()) or the stream ending mid-
		// response are ordinary, frequent occurrences here, not failures
		// worth an ERROR-level log — Serve has already started writing the
		// response by the time any of these can happen, so there's nothing
		// left to do but let the connection close.
		slog.Debug("live ts stream ended", "app", app, "stream", stream, "err", err)
	}
}

// ─── nDVR rewind (internal/dvr) ───────────────────────────────────────────────

// handleDVRManifest serves the nDVR playlists/manifests under
// /{app}/{stream}/ — playlist_dvr.m3u8, playlist_dvr_range-{from}-{len}.m3u8,
// playlist_dvr_timeshift-{shift}[-{depth}].m3u8 and their manifest_dvr*.mpd
// DASH equivalents (see dvr.ParseRequest). For an ABR group the HLS names
// return a rewind master over its variants (abrDVRMaster). Public like every other delivery
// route. Segment URIs are relative (dvr/{session}/…), served by
// handleDVRFile.
func (s *Server) handleDVRManifest(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	stream := chi.URLParam(r, "stream")
	file := chi.URLParam(r, "dvrfile")
	req, ok := dvr.ParseRequest(file)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// ABR group rewind: a master over the variants' own recordings.
	if !req.DASH {
		if body, ok, found := s.abrDVRMaster(r.Context(), app, stream, file, req); found {
			if !ok {
				http.Error(w, "dvr: no variant of this ABR group has recording in the requested range", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Header().Set("Cache-Control", "no-cache, no-store")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Write([]byte(body))
			return
		}
	}
	s.serveDVRManifest(w, r, app, stream, req, "")
}

// handleLegacyDVRPlaylist keeps the original /dvr/{app}/{stream}/index.m3u8
// ?start=<ms>&end=<ms> URL working as an HLS range request. Without start it
// is the whole rewind window, live.
func (s *Server) handleLegacyDVRPlaylist(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	stream := chi.URLParam(r, "stream")

	req := dvr.Request{Kind: dvr.KindWindow}
	if startMS := parseIntQuery(r, "start", 0); startMS > 0 {
		endMS := parseIntQuery(r, "end", 0)
		if endMS <= startMS {
			endMS = time.Now().UnixMilli()
		}
		req = dvr.Request{Kind: dvr.KindRange, FromMS: startMS, LengthS: (endMS - startMS + 999) / 1000}
	}
	s.serveDVRManifest(w, r, app, stream, req, "/"+app+"/"+stream+"/")
}

func (s *Server) serveDVRManifest(w http.ResponseWriter, r *http.Request, app, stream string, req dvr.Request, prefix string) {
	s.registry.TouchViewer(media.StreamKey{App: app, Name: stream}, viewerClientID(r))
	body, contentType, err := s.dvr.Manifest(r.Context(), app, stream, req, prefix)
	if err != nil {
		if dvr.IsNotFound(err) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		slog.Warn("dvr manifest", "app", app, "stream", stream, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	if req.Kind == dvr.KindRange && strings.Contains(body, "#EXT-X-ENDLIST") {
		w.Header().Set("Cache-Control", "public, max-age=60")
	} else {
		w.Header().Set("Cache-Control", "no-cache, no-store")
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write([]byte(body))
}

// handleDVRFile serves /{app}/{stream}/dvr/{session}/{file}: a session's
// init segment or a recorded segment, whole (HLS) or just its video/audio
// half (DASH) — see dvr.ParseFile.
func (s *Server) handleDVRFile(w http.ResponseWriter, r *http.Request) {
	s.serveDVRFile(w, r, chi.URLParam(r, "app"), chi.URLParam(r, "stream"))
}

// handleVariantDVRManifest serves an ABR variant's rewind playlist under
// its group, /{abrApp}/{abrStream}/{srcApp}/{srcStream}/playlist_dvr*.m3u8,
// which is what abrDVRMaster's variant URIs resolve to (the same layout as
// the live variant route). Its segment URIs stay relative, so they resolve
// to handleVariantDVRFile under the same prefix.
func (s *Server) handleVariantDVRManifest(w http.ResponseWriter, r *http.Request) {
	req, ok := dvr.ParseRequest(chi.URLParam(r, "dvrfile"))
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	s.serveDVRManifest(w, r, chi.URLParam(r, "srcApp"), chi.URLParam(r, "srcStream"), req, "")
}

// handleVariantDVRFile serves /{abrApp}/{abrStream}/{srcApp}/{srcStream}/dvr/{session}/{file}.
func (s *Server) handleVariantDVRFile(w http.ResponseWriter, r *http.Request) {
	s.serveDVRFile(w, r, chi.URLParam(r, "srcApp"), chi.URLParam(r, "srcStream"))
}

func (s *Server) serveDVRFile(w http.ResponseWriter, r *http.Request, app, stream string) {
	sessionMS, err := strconv.ParseInt(chi.URLParam(r, "session"), 10, 64)
	f, ok := dvr.ParseFile(chi.URLParam(r, "file"))
	if err != nil || !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	path, off, n, err := s.dvr.ResolveFile(r.Context(), app, stream, sessionMS, f)
	if err != nil {
		http.Error(w, "segment not found", http.StatusNotFound)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		http.Error(w, "segment not found", http.StatusNotFound)
		return
	}
	defer file.Close()
	if n < 0 {
		st, err := file.Stat()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		n = st.Size()
	}

	contentType := "video/mp4"
	if f.Track == dvr.TrackAudio {
		contentType = "audio/mp4"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=31536000") // recorded files never change
	w.Header().Set("Access-Control-Allow-Origin", "*")
	http.ServeContent(w, r, "", time.Time{}, io.NewSectionReader(file, off, n))
}

// ─── WebRTC WHIP (sub-second-latency ingest) ─────────────────────────────────

func (s *Server) handleWHIP(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.WebRTC.Enabled {
		http.Error(w, "WHIP: disabled", http.StatusNotImplemented)
		return
	}

	app := chi.URLParam(r, "app")
	streamName := chi.URLParam(r, "stream")

	appCfg, err := s.db.GetApplicationByName(r.Context(), app)
	if err != nil || appCfg == nil || !appCfg.Enabled {
		http.Error(w, "unknown or disabled application", http.StatusNotFound)
		return
	}

	// WHIP-idiomatic auth: a bearer token, not RTMP's embedded-in-stream-name
	// convention (there's no natural place to embed a key in a WHIP URL path).
	if appCfg.PublishKey != "" {
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		if token == auth || token != appCfg.PublishKey {
			http.Error(w, "invalid or missing publish key", http.StatusUnauthorized)
			return
		}
	}

	if !appCfg.AllowAnyStream {
		sc, err := s.db.GetStreamConfig(r.Context(), appCfg.ID, streamName)
		if err != nil || sc == nil {
			http.Error(w, "stream not configured", http.StatusNotFound)
			return
		}
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read offer: "+err.Error(), http.StatusBadRequest)
		return
	}

	answerSDP, resourceID, err := s.webrtcMgr.CreatePublishSession(r.Context(), app, streamName, string(body))
	if err != nil {
		switch {
		case errors.Is(err, webrtcpkg.ErrStreamAlreadyLive):
			http.Error(w, "stream already publishing", http.StatusConflict)
		default:
			http.Error(w, "whip negotiation: "+err.Error(), http.StatusBadRequest)
		}
		return
	}

	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", strings.TrimSuffix(r.URL.Path, "/")+"/"+resourceID)
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(answerSDP))
}

func (s *Server) handleWHIPDelete(w http.ResponseWriter, r *http.Request) {
	resource := chi.URLParam(r, "resource")
	s.webrtcMgr.DeleteSession(resource)
	w.WriteHeader(http.StatusNoContent)
}

// ─── WebRTC WHEP (sub-second-latency playback) ───────────────────────────────

func (s *Server) handleWHEP(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.WebRTC.Enabled {
		http.Error(w, "WHEP: disabled", http.StatusNotImplemented)
		return
	}

	app := chi.URLParam(r, "app")
	streamName := chi.URLParam(r, "stream")

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read offer: "+err.Error(), http.StatusBadRequest)
		return
	}

	answerSDP, resourceID, err := s.webrtcMgr.CreateSession(r.Context(), app, streamName, string(body))
	if err != nil {
		switch {
		case errors.Is(err, webrtcpkg.ErrStreamNotLive):
			http.Error(w, "stream not live", http.StatusNotFound)
		case errors.Is(err, webrtcpkg.ErrUnsupportedCodec):
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		default:
			http.Error(w, "whep negotiation: "+err.Error(), http.StatusBadRequest)
		}
		return
	}

	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", strings.TrimSuffix(r.URL.Path, "/")+"/"+resourceID)
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(answerSDP))
}

func (s *Server) handleWHEPDelete(w http.ResponseWriter, r *http.Request) {
	resource := chi.URLParam(r, "resource")
	s.webrtcMgr.DeleteSession(resource)
	w.WriteHeader(http.StatusNoContent)
}

// ─── Panel handler ────────────────────────────────────────────────────────────

// panelHandler serves the embedded Next.js static export.
// Because Next.js uses trailingSlash:true, each route is a directory with an
// index.html.  For any path that doesn't match a real file we fall back to
// the root index.html so client-side routing works.
func (s *Server) panelHandler() http.Handler {
	sub, err := fs.Sub(panelfs.FS, "out") //nolint — panelfs is the package name from panel/panelfs.go
	if err != nil {
		panic("panel embed missing: " + err.Error())
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upath := strings.TrimPrefix(r.URL.Path, "/")

		// Try exact file first; if missing fall back to the Next.js page
		// directory index.html (trailingSlash export), then root index.html.
		candidates := []string{
			upath,
			strings.TrimSuffix(upath, "/") + "/index.html",
			"index.html",
		}
		for _, c := range candidates {
			if c == "" {
				c = "index.html"
			}
			f, ferr := sub.Open(c)
			if ferr != nil {
				continue
			}
			stat, serr := f.Stat()
			if serr != nil || stat.IsDir() {
				f.Close()
				continue
			}
			// Serve the file directly — avoid http.FileServer to prevent
			// directory-redirect logic from issuing 301s for SPA routes.
			// Set Content-Type explicitly: Go's MIME sniff misidentifies
			// minified JS as text/plain on systems without a .js MIME entry.
			switch strings.ToLower(filepath.Ext(c)) {
			case ".js", ".mjs":
				w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
			case ".css":
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
			case ".html":
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
			case ".json":
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
			case ".svg":
				w.Header().Set("Content-Type", "image/svg+xml")
			case ".ico":
				w.Header().Set("Content-Type", "image/x-icon")
			case ".png":
				w.Header().Set("Content-Type", "image/png")
			case ".woff2":
				w.Header().Set("Content-Type", "font/woff2")
			case ".woff":
				w.Header().Set("Content-Type", "font/woff")
			}
			if strings.HasSuffix(c, ".html") {
				w.Header().Set("Cache-Control", "no-cache")
			} else {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.ServeContent(w, r, stat.Name(), stat.ModTime(), f.(io.ReadSeeker))
			f.Close()
			return
		}
		http.NotFound(w, r)
	})
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// serveM3U8 serves a live playlist from the live cache (internal/livestore).
func serveM3U8(w http.ResponseWriter, r *http.Request, path string) {
	data, ok := livestore.Default().ReadFile(path)
	if !ok {
		http.Error(w, "stream not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(data)
}

// serveSegment serves a live segment, part or init file from the live
// cache (internal/livestore): RAM, or disk while memory is short.
// ServeContent handles Range requests.
func serveSegment(w http.ResponseWriter, r *http.Request, path, contentType string) {
	content, _, ok := livestore.Default().Open(path)
	if !ok {
		http.Error(w, "segment not found", http.StatusNotFound)
		return
	}
	defer content.Close()

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=31536000") // segments are immutable
	w.Header().Set("Access-Control-Allow-Origin", "*")

	http.ServeContent(w, r, "", time.Time{}, content)
}

func segmentContentType(name string) string {
	if len(name) > 3 && name[len(name)-3:] == ".ts" {
		return "video/MP2T"
	}
	if len(name) > 4 && (name[len(name)-4:] == ".m4s" || name[len(name)-4:] == ".mp4") {
		// DASH's separately-packaged tracks (internal/dash/packager.go's
		// seg-video-/seg-audio-/seg-text-/init-video/init-audio/init-text
		// naming) must be served with the Content-Type matching what the
		// MPD's own mimeType attribute declares for that track — a real
		// conformance gap this replaces: "video/iso.segment" isn't a
		// registered media type and doesn't match the manifest at all.
		// HLS's combined-file fMP4 segments (seg%06d.m4s, video+audio
		// muxed together) fall through to the video/mp4 default, matching
		// Apple's own convention for muxed CMAF segments.
		switch {
		case strings.Contains(name, "video"):
			return "video/mp4"
		case strings.Contains(name, "audio"):
			return "audio/mp4"
		case strings.Contains(name, "text"):
			return "application/mp4"
		default:
			return "video/mp4"
		}
	}
	return "application/octet-stream"
}

// isSafeFileName rejects path traversal and other dangerous names.
// videoCodecString returns the RFC 6381 codec string for whatever video
// codec ci actually describes — H.264 is by far the common case, but a
// stream with real HEVC CodecInfo (see internal/rtmp/tag_decode.go's
// Enhanced RTMP path) must advertise "hvc1...", not a misleading "avc1...".
func videoCodecString(ci *media.CodecInfo) string {
	if ci != nil && ci.VideoCodec == media.CodecH265 {
		return media.HEVCCodecString(ci)
	}
	return media.H264CodecString(ci)
}

func isSafeFileName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, c := range name {
		if c == '/' || c == '\\' {
			return false
		}
	}
	return true
}

func parseIntQuery(r *http.Request, key string, defaultVal int64) int64 {
	s := r.URL.Query().Get(key)
	if s == "" {
		return defaultVal
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return defaultVal
	}
	return v
}

// viewerClientID derives a per-client identity for concurrent-viewer
// tracking from the request: the client's IP (preferring the leftmost
// X-Forwarded-For entry, for deployments behind a reverse proxy/CDN) plus
// User-Agent, so distinct devices/browsers behind the same NAT still count
// separately. This is a best-effort heuristic, not a real session ID — HLS/
// DASH clients carry no such identifier — but it's a large improvement over
// the previous "viewers" figure, which counted internal packet-consumer
// goroutines rather than actual player connections.
func viewerClientID(r *http.Request) string {
	ip := r.RemoteAddr
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if i := strings.IndexByte(fwd, ','); i >= 0 {
			ip = strings.TrimSpace(fwd[:i])
		} else {
			ip = strings.TrimSpace(fwd)
		}
	} else if h, _, err := net.SplitHostPort(ip); err == nil {
		ip = h
	}
	return ip + "|" + r.UserAgent()
}

// ─── Nimble-compatible HLS delivery ──────────────────────────────────────────

// countingWriter wraps an http.ResponseWriter to track bytes actually written,
// so byte-out metrics reflect what was sent even for range/conditional requests.
type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

// handlePlaylist serves /{app}/{stream}/playlist.m3u8 — a master playlist.
//
// For ABR groups (manual or %i — see abrMaster): a multi-variant master.
//
// For single live streams: generates a one-variant master pointing to chunks.m3u8
// with BANDWIDTH, RESOLUTION, CODECS derived from the stream's CodecInfo.
func (s *Server) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	stream := chi.URLParam(r, "stream")

	// ── ABR group: manual (panel/DB) first, then automatic %i ────────────────
	if body, ok, found := s.abrMaster(r.Context(), app, stream); found {
		if !ok {
			http.Error(w, "all variants in this ABR group are offline", http.StatusServiceUnavailable)
			return
		}
		if s.metrics != nil {
			s.metrics.HLSPlaylistRequests.WithLabelValues(app, stream, "master").Inc()
		}
		hlsResponse(w, body)
		return
	}

	// ── Fall back to single live stream ──────────────────────────────────────
	streamObj := s.registry.Get(media.StreamKey{App: app, Name: stream})
	if streamObj == nil {
		http.Error(w, "stream not found", http.StatusNotFound)
		return
	}
	streamObj.Viewers.Touch(viewerClientID(r))

	ci := streamObj.Bus.CodecInfo()
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	sb.WriteString("#EXT-X-VERSION:3\n")

	// hls.separate_audio_track mode (see internal/hls.Packager.useSplitAudio,
	// same gating condition): link the audio-only playlist into the master
	// via EXT-X-MEDIA + the video variant's AUDIO attribute, so MSE players
	// (hls.js) pick up independent video/audio SourceBuffers instead of
	// expecting a combined-track .m4s.
	splitAudio := s.cfg.HLS.SeparateAudioTrack && !s.cfg.HLS.LLHls.Enabled &&
		(s.cfg.HLS.Container == "fmp4" || s.cfg.HLS.Container == "cmaf") &&
		ci != nil && ci.AudioCodec != media.CodecUnknown
	if splitAudio {
		sb.WriteString(`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="audio",AUTOSELECT=YES,DEFAULT=YES,URI="audio/index.m3u8"` + "\n")
	}

	// In-band CEA-608/708 closed captions (SEI NALs already carried through
	// the H.264 elementary stream — see internal/ingest/mpegts.flushVideo
	// and media.HasCEA608708) need no separate URI: the data lives inside
	// the video PES itself, this just tells the player where to look.
	// CLOSED-CAPTIONS=NONE is only emitted once CodecInfo is actually known
	// (ci != nil) — per RFC 8216 that attribute means "definitely none",
	// which isn't yet knowable before the source's first sequence header.
	//
	// INSTREAM-ID uses two different conventions depending on what the
	// source's cc_data() actually carries (see media.UsesCEA708DTVCCService's
	// doc comment — confirmed against a real encoder that sends 708, not
	// 608, despite HasCEA608708 being true either way): "CC1" for legacy
	// CEA-608 field data, "SERVICE1" for CEA-708 DTVCC service data.
	// Signaling the wrong one makes a compliant player correctly look for
	// data on the wrong channel and render nothing, even though the caption
	// bytes themselves are present and intact.
	hasCC := ci != nil && ci.HasCEA608708
	ccGroupID, ccInstreamID := "cc1", "CC1"
	if ci != nil && ci.CEA708DTVCCService {
		ccGroupID, ccInstreamID = "cc708", "SERVICE1"
	}
	if hasCC {
		fmt.Fprintf(&sb, `#EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS,GROUP-ID=%q,NAME="English",LANGUAGE="en",AUTOSELECT=YES,DEFAULT=YES,INSTREAM-ID=%q`+"\n",
			ccGroupID, ccInstreamID)
	}

	// Same rules as the ABR master's streamInf: measured BANDWIDTH (0 is
	// invalid), the real frame rate when known, and CODECS naming only
	// tracks the segments carry (G.711 audio from an RTSP camera is
	// dropped; advertising it makes players wait for audio).
	attrs := fmt.Sprintf("BANDWIDTH=%d", measuredBandwidth(streamObj))
	if ci != nil {
		if ci.Width > 0 && ci.Height > 0 {
			if ci.FrameRate > 0 {
				attrs += fmt.Sprintf(",FRAME-RATE=%.3f", ci.FrameRate)
			}
			codecs := videoCodecString(ci)
			if media.TSAudioCodec(ci.AudioCodec) {
				codecs += "," + media.AudioCodecString(ci)
			}
			attrs += fmt.Sprintf(",RESOLUTION=%dx%d,CODECS=%q", ci.Width, ci.Height, codecs)
		}
	}
	if splitAudio {
		attrs += `,AUDIO="audio"`
	}
	if ci != nil {
		if hasCC {
			attrs += fmt.Sprintf(`,CLOSED-CAPTIONS=%q`, ccGroupID)
		} else {
			attrs += `,CLOSED-CAPTIONS=NONE`
		}
	}
	if s.metrics != nil {
		s.metrics.HLSPlaylistRequests.WithLabelValues(app, stream, "master").Inc()
	}
	fmt.Fprintf(&sb, "#EXT-X-STREAM-INF:%s\nchunks.m3u8\n", attrs)
	hlsResponse(w, sb.String())
}

// handleChunks serves /{app}/{stream}/chunks.m3u8 — the live sliding-window media playlist.
func (s *Server) handleChunks(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	stream := chi.URLParam(r, "stream")
	if s.metrics != nil {
		s.metrics.HLSPlaylistRequests.WithLabelValues(app, stream, "media").Inc()
	}
	s.registry.TouchViewer(media.StreamKey{App: app, Name: stream}, viewerClientID(r))
	s.awaitLLHLSUpdate(r, app, stream)
	path := filepath.Join(s.cfg.Server.DataDir, "hls", app, stream, "index.m3u8")
	s.servePlaylistOrDelta(w, r, app, stream, path)
}

// servePlaylistOrDelta serves the stored playlist, unless the request
// carries ?_HLS_skip=YES and this stream has a live *hls.Playlist to render
// a Delta Update from (see Playlist.RenderDelta) — in which case that's
// rendered fresh and returned instead, since the stored playlist only ever
// holds the full (non-delta) playlist.
func (s *Server) servePlaylistOrDelta(w http.ResponseWriter, r *http.Request, app, stream, fallbackPath string) {
	if r.URL.Query().Get("_HLS_skip") == "YES" && s.hlsPlaylists != nil {
		if pl := s.hlsPlaylists.Get(media.StreamKey{App: app, Name: stream}); pl != nil {
			hlsResponse(w, pl.RenderDelta())
			return
		}
	}
	serveM3U8(w, r, fallbackPath)
}

// handleDirectSegment serves /{app}/{stream}/{segment} — a single .ts segment.
// handleDirectSegment serves /{app}/{stream}/{segment} — an HLS segment, or
// (now that DASH is mounted with no path prefix of its own — see router.go)
// a DASH segment/init file sharing this same URL shape. Tries the HLS
// output first, falling back to DASH's — extension alone can't
// disambiguate, since fmp4/cmaf-container HLS and DASH can both produce
// .m4s/.mp4 files.
func (s *Server) handleDirectSegment(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	stream := chi.URLParam(r, "stream")
	segment := chi.URLParam(r, "segment")
	if !isSafeFileName(segment) {
		http.Error(w, "invalid segment name", http.StatusBadRequest)
		return
	}

	hlsPath := filepath.Join(s.cfg.Server.DataDir, "hls", app, stream, segment)
	if livestore.Default().Exists(hlsPath) {
		s.serveHLSSegment(w, r, hlsPath, segment, app, stream)
		return
	}

	s.registry.TouchViewer(media.StreamKey{App: app, Name: stream}, viewerClientID(r))
	dashPath := filepath.Join(s.cfg.Server.DataDir, "dash", app, stream, segment)
	serveSegment(w, r, dashPath, "video/iso.segment")
}

// handleAudioChunks serves /{app}/{stream}/audio/index.m3u8 — the
// audio-only media playlist written when hls.separate_audio_track is on
// (see internal/hls.Packager.useSplitAudio).
func (s *Server) handleAudioChunks(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	stream := chi.URLParam(r, "stream")
	if s.metrics != nil {
		s.metrics.HLSPlaylistRequests.WithLabelValues(app, stream, "audio").Inc()
	}
	s.registry.TouchViewer(media.StreamKey{App: app, Name: stream}, viewerClientID(r))
	path := filepath.Join(s.cfg.Server.DataDir, "hls", app, stream, "audio", "index.m3u8")
	serveM3U8(w, r, path)
}

// handleAudioSegment serves /{app}/{stream}/audio/{segment} — an
// audio-only init segment or .m4s, counterpart to handleDirectSegment.
func (s *Server) handleAudioSegment(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	stream := chi.URLParam(r, "stream")
	segment := chi.URLParam(r, "segment")
	if !isSafeFileName(segment) {
		http.Error(w, "invalid segment name", http.StatusBadRequest)
		return
	}
	path := filepath.Join(s.cfg.Server.DataDir, "hls", app, stream, "audio", segment)
	s.serveHLSSegment(w, r, path, segment, app, stream)
}

// handleVariantChunks serves /{abrApp}/{abrStream}/{srcApp}/{srcStream}/chunks.m3u8.
// It reads the source stream's own media playlist (written by the HLS packager).
func (s *Server) handleVariantChunks(w http.ResponseWriter, r *http.Request) {
	srcApp := chi.URLParam(r, "srcApp")
	srcStream := chi.URLParam(r, "srcStream")
	if s.metrics != nil {
		s.metrics.HLSPlaylistRequests.WithLabelValues(srcApp, srcStream, "media").Inc()
	}
	s.registry.TouchViewer(media.StreamKey{App: srcApp, Name: srcStream}, viewerClientID(r))
	s.awaitLLHLSUpdate(r, srcApp, srcStream)
	path := filepath.Join(s.cfg.Server.DataDir, "hls", srcApp, srcStream, "index.m3u8")
	s.servePlaylistOrDelta(w, r, srcApp, srcStream, path)
}

// handleVariantSegment serves /{abrApp}/{abrStream}/{srcApp}/{srcStream}/{segment}.
func (s *Server) handleVariantSegment(w http.ResponseWriter, r *http.Request) {
	srcApp := chi.URLParam(r, "srcApp")
	srcStream := chi.URLParam(r, "srcStream")
	segment := chi.URLParam(r, "segment")
	if !isSafeFileName(segment) {
		http.Error(w, "invalid segment name", http.StatusBadRequest)
		return
	}
	path := filepath.Join(s.cfg.Server.DataDir, "hls", srcApp, srcStream, segment)
	s.serveHLSSegment(w, r, path, segment, srcApp, srcStream)
}

// serveHLSSegment serves an HLS segment file and records request-count and
// bytes-out metrics labeled by app/stream.
func (s *Server) serveHLSSegment(w http.ResponseWriter, r *http.Request, path, segment, app, stream string) {
	s.registry.TouchViewer(media.StreamKey{App: app, Name: stream}, viewerClientID(r))
	if s.metrics == nil {
		serveSegment(w, r, path, segmentContentType(segment))
		return
	}
	s.metrics.HLSSegmentRequests.WithLabelValues(app, stream).Inc()
	cw := &countingWriter{ResponseWriter: w}
	serveSegment(cw, r, path, segmentContentType(segment))
	s.metrics.HLSSegmentBytesOut.WithLabelValues(app, stream).Add(float64(cw.n))
}

// handleDASHUTCTime serves /dash-utc-time — the self-hosted endpoint
// [dash.dvb]'s UTCTiming element points at (scheme
// urn:mpeg:dash:utc:http-xsdate:2014, see internal/dash/mpd.go), which
// expects a plain-text xs:dateTime response body.
func (s *Server) handleDASHUTCTime(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	fmt.Fprint(w, time.Now().UTC().Format(time.RFC3339Nano))
}

func hlsResponse(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write([]byte(content))
}
