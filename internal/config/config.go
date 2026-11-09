package config

import (
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server    ServerConfig    `toml:"server"`
	HTTP      HTTPConfig      `toml:"http"`
	RTMP      RTMPConfig      `toml:"rtmp"`
	SRT       SRTConfig       `toml:"srt"`
	Icecast   IcecastConfig   `toml:"icecast"`
	MpegTS    MpegTSConfig    `toml:"mpegts"`
	HLS       HLSConfig       `toml:"hls"`
	DASH      DASHConfig      `toml:"dash"`
	DVR       DVRConfig       `toml:"dvr"`
	Failover  FailoverConfig  `toml:"failover"`
	WebRTC    WebRTCConfig    `toml:"webrtc"`
	Auth      AuthConfig      `toml:"auth"`
	Metrics   MetricsConfig   `toml:"metrics"`
	LiveCache LiveCacheConfig `toml:"live_cache"`
	Multinode MultinodeConfig `toml:"multinode"`

	// dvrMu guards DVR.Enabled and DVR.WindowSeconds, the only DVR fields
	// mutable at runtime (via the panel's DVR settings page). DVR.StoragePath
	// is fixed at load time — active recorders capture it at start and never
	// re-read it, so it is read directly elsewhere without this lock.
	dvrMu sync.RWMutex
}

// DVRWindowSeconds returns the current DVR rewind window, safe for
// concurrent use with UpdateDVRRuntime.
func (c *Config) DVRWindowSeconds() int {
	c.dvrMu.RLock()
	defer c.dvrMu.RUnlock()
	return c.DVR.WindowSeconds
}

// DVREnabled returns whether DVR recording is currently enabled, safe for
// concurrent use with UpdateDVRRuntime.
func (c *Config) DVREnabled() bool {
	c.dvrMu.RLock()
	defer c.dvrMu.RUnlock()
	return c.DVR.Enabled
}

// UpdateDVRRuntime applies a live change to DVR.Enabled / DVR.WindowSeconds.
// windowSeconds <= 0 leaves the window unchanged; enabled == nil leaves the
// enabled flag unchanged.
func (c *Config) UpdateDVRRuntime(windowSeconds int, enabled *bool) {
	c.dvrMu.Lock()
	defer c.dvrMu.Unlock()
	if windowSeconds > 0 {
		c.DVR.WindowSeconds = windowSeconds
	}
	if enabled != nil {
		c.DVR.Enabled = *enabled
	}
}

type ServerConfig struct {
	NodeName  string `toml:"node_name"`
	DataDir   string `toml:"data_dir"`
	LogLevel  string `toml:"log_level"`
	LogFormat string `toml:"log_format"`
}

type HTTPConfig struct {
	Listen string `toml:"listen"`
	// TLSListen is a second, independent listener for HTTPS — plain HTTP
	// on Listen keeps running unchanged when TLSEnabled is true (matching
	// RTMPConfig's existing Listen/TLSListen split), so URLs already
	// handed out over http://<host><Listen>/... keep working after HTTPS
	// is turned on rather than breaking.
	TLSListen   string   `toml:"tls_listen"`
	TLSEnabled  bool     `toml:"tls_enabled"`
	TLSCert     string   `toml:"tls_cert"`
	TLSKey      string   `toml:"tls_key"`
	CORSOrigins []string `toml:"cors_origins"`
}

type RTMPConfig struct {
	Listen         string `toml:"listen"`
	TLSListen      string `toml:"tls_listen"`
	TLSEnabled     bool   `toml:"tls_enabled"`
	IdleTimeoutMs  int    `toml:"idle_timeout_ms"`
	MaxConnsPerApp int    `toml:"max_connections_per_app"`
	// RouteApp is a routing app for encoders that only have a stream key
	// field (OBS): publishing to rtmp://host/{RouteApp} with stream name
	// "{app}{RouteSeparator}{stream}" lands on {app}/{stream}. "" = off.
	// See rtmp.conn.routeTarget.
	RouteApp       string    `toml:"route_app"`
	RouteSeparator string    `toml:"route_separator"`
	Applications   []RTMPApp `toml:"applications"`
}

type RTMPApp struct {
	Name       string `toml:"name"`
	PublishKey string `toml:"publish_key"`
	// PushLogin/PushPassword require RTMP publishers to log in with Adobe
	// RTMP authentication (FMLE's username/password prompt, OBS "Use
	// authentication", rtmp://user:pass@host/app). Empty login = off.
	PushLogin    string       `toml:"push_login"`
	PushPassword string       `toml:"push_password"`
	ABRPatterns  []ABRPattern `toml:"abr_patterns"`
}

// ABRPattern defines a wildcard rule for grouping multi-bitrate RTMP streams.
//
// FMLE and compatible encoders publish multiple streams on one TCP connection
// using stream names like "camera_1", "camera_2", "camera_3".  The %i token
// matches any integer suffix, letting the server automatically group those
// streams into a single ABR rendition set.
type ABRPattern struct {
	// Pattern is a stream name with %i as a placeholder for the variant index.
	// Example: "camera_%i" matches "camera_1", "camera_2", "camera_3".
	Pattern    string           `toml:"pattern"`
	GroupName  string           `toml:"group_name"`
	AutoDetect bool             `toml:"auto_detect"`
	Variants   []ABRVariantHint `toml:"variants"`
}

// ABRVariantHint provides optional metadata for a specific variant index.
// Index must match the %i integer in the stream name.
type ABRVariantHint struct {
	Index     int    `toml:"index"`
	Label     string `toml:"label"`
	Height    int    `toml:"height"`
	Bandwidth int    `toml:"bandwidth"`
}

type SRTConfig struct {
	Enabled    bool   `toml:"enabled"`
	Listen     string `toml:"listen"`
	LatencyMs  int    `toml:"latency_ms"`
	Passphrase string `toml:"passphrase"`
}

// IcecastConfig configures the Icecast/SHOUTcast-style HTTP SOURCE/PUT
// ingest listener — audio-only (MP3 or ADTS AAC), no video.
type IcecastConfig struct {
	Enabled bool   `toml:"enabled"`
	Listen  string `toml:"listen"`
}

type MpegTSConfig struct {
	UDPListen string `toml:"udp_listen"`
}

type HLSConfig struct {
	PathPrefix       string      `toml:"path_prefix"`
	SegmentDurationS int         `toml:"segment_duration_s"`
	PlaylistWindow   int         `toml:"playlist_window"`
	Container        string      `toml:"container"` // ts | fmp4 | cmaf
	LLHls            LLHlsConfig `toml:"ll_hls"`
	// SeparateAudioTrack splits fMP4/CMAF output into independent video-only
	// and audio-only playlists/segments (linked via EXT-X-MEDIA in the
	// master playlist) instead of one combined .m4s per segment — the
	// combined form works in native/non-MSE HLS clients but isn't
	// guaranteed under strict browser MSE (hls.js). No effect on the TS
	// container (already interleaved, not combined-fMP4-specific) or when
	// ll_hls.enabled is also set (LL-HLS parts across two independent
	// playlists is a distinct, not-yet-implemented combination).
	SeparateAudioTrack bool `toml:"separate_audio_track"`
}

type LLHlsConfig struct {
	Enabled        bool `toml:"enabled"`
	PartDurationMs int  `toml:"part_duration_ms"`
	HoldBackParts  int  `toml:"hold_back_parts"`
}

type DASHConfig struct {
	PathPrefix       string `toml:"path_prefix"`
	SegmentDurationS int    `toml:"segment_duration_s"`
	MinBufferTimeS   int    `toml:"min_buffer_time_s"`
	// DVRWindowS enables DASH timeshift: segments are kept on disk (and
	// advertised via the MPD's timeShiftBufferDepth) for this many seconds
	// instead of being cleaned up immediately, letting a compliant DASH
	// client seek backward within the window using the same $Number$
	// SegmentTemplate addressing it already uses for live playback — no
	// separate recording engine needed (unlike HLS's DVR, which requires
	// one; see internal/dvr). 0 disables DVR cleanup/advertising entirely.
	DVRWindowS int `toml:"dvr_window_s"`

	DVB DVBConfig `toml:"dvb"`
}

// DVBConfig gates DVB-DASH profile signaling and EBU-TT-D subtitles —
// see internal/dash (mpd.go's DVBProfile handling, packager.go's text
// track) and internal/captiondecode (where the caption text actually comes
// from: real-time decode of the encoder's own embedded CEA-608/708 data,
// not AI-generated). Both flags are independent: subtitles can be enabled
// without full profile conformance, or vice versa.
type DVBConfig struct {
	// Enabled turns on full DVB-DASH profile signaling: MPD@profiles URN
	// list, a self-hosted UTCTiming endpoint, a BaseURL with
	// dvb:priority/weight, and suppresses suggestedPresentationDelay
	// (deliberately omitted under DVB-DASH, unlike normal DASH practice).
	Enabled bool `toml:"enabled"`
	// SubtitlesEnabled turns on the EBU-TT-D subtitle AdaptationSet
	// (codecs="stpp"), decoded in real time from the stream's own
	// CEA-608/708 embedded captions.
	SubtitlesEnabled bool `toml:"subtitles_enabled"`
}

type DVRConfig struct {
	Enabled       bool   `toml:"enabled"`
	StoragePath   string `toml:"storage_path"`
	WindowSeconds int    `toml:"window_seconds"`
}

type FailoverConfig struct {
	WatchdogIntervalMs int `toml:"watchdog_interval_ms"`
	StallTimeoutMs     int `toml:"stall_timeout_ms"`
	WarmupBufferMs     int `toml:"warmup_buffer_ms"`
}

// WebRTCConfig gates WHIP ingest and WHEP playback support.
type WebRTCConfig struct {
	Enabled bool `toml:"enabled"`

	// ICEServers are STUN/TURN URLs handed to pion for NAT traversal, e.g.
	// ["stun:stun.l.google.com:19302"]. Empty is valid — pion still gathers
	// host candidates, sufficient for same-host/same-LAN testing.
	ICEServers []string `toml:"ice_servers"`

	// ICEUDPPortMin/Max restrict the ephemeral UDP port range used for ICE
	// candidates (useful behind a firewall that only forwards a fixed
	// range). 0/0 means OS-assigned ephemeral ports.
	ICEUDPPortMin uint16 `toml:"ice_udp_port_min"`
	ICEUDPPortMax uint16 `toml:"ice_udp_port_max"`
}

type AuthConfig struct {
	JWTSecret         string               `toml:"jwt_secret"`
	JWTExpiryHours    int                  `toml:"jwt_expiry_hours"`
	RefreshExpiryDays int                  `toml:"refresh_expiry_days"`
	BootstrapAdmin    BootstrapAdminConfig `toml:"bootstrap_admin"`
}

type BootstrapAdminConfig struct {
	Email    string `toml:"email"`
	Password string `toml:"password"`
}

// LiveCacheConfig controls where live HLS/DASH output is kept (see
// internal/livestore). Mode:
//   - "hybrid" (default): RAM, moving to disk under <data_dir> while memory
//     use is at or above MemoryHighPct, back to RAM below MemoryLowPct (the
//     gap stops it switching back and forth at the limit).
//   - "memory": always RAM; only a warning at MemoryHighPct.
//   - "disk": always disk.
type LiveCacheConfig struct {
	Mode           string `toml:"mode"`
	MemoryHighPct  int    `toml:"memory_high_pct"`
	MemoryLowPct   int    `toml:"memory_low_pct"`
	CheckIntervalS int    `toml:"check_interval_s"`
}

type MetricsConfig struct {
	Enabled bool   `toml:"enabled"`
	Path    string `toml:"path"`
}

type MultinodeConfig struct {
	Enabled   bool   `toml:"enabled"`
	NATSUrl   string `toml:"nats_url"`
	NATSCreds string `toml:"nats_creds"`
	// AdvertiseURL is this node's own externally-reachable base URL (e.g.
	// "http://node1.example.com:8080"), published in its heartbeat so
	// peers/panels can link directly to it. Required for a real multi-host
	// deployment — http.Listen alone (often "0.0.0.0:...") isn't a usable
	// address for anyone but this host itself.
	AdvertiseURL       string `toml:"advertise_url"`
	HeartbeatIntervalS int    `toml:"heartbeat_interval_s"`

	// nDVR stream ownership (internal/dvrowner). Applies only to streams
	// with nDVR enabled, and only while multinode is enabled.
	//
	// OwnershipPolicy picks the owner when more than one node is
	// receiving the same stream: "first_publisher" (default) keeps the
	// node that started publishing first, so a second encoder connection
	// never steals playback; "priority" prefers the lowest NodePriority.
	// Either way ties fall back to NodePriority, then node name.
	OwnershipPolicy string `toml:"ownership_policy"`
	// NodePriority: lower wins (1 = primary). It's the tie-breaker for
	// equal publish times and equal DVR end times, and the main rule
	// under ownership_policy = "priority". Default 100.
	NodePriority int `toml:"node_priority"`
	// PeerForgetS is how long a peer that stopped heartbeating stays known
	// as "unreachable" before it's treated as gone. Until then nDVR
	// ownership that depends on it answers 503 rather than guessing.
	// Default 60.
	PeerForgetS int `toml:"peer_forget_s"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

func (c *Config) applyDefaults() {
	switch c.LiveCache.Mode {
	case "memory", "disk", "hybrid":
	default:
		c.LiveCache.Mode = "hybrid"
	}
	if c.LiveCache.MemoryHighPct <= 0 || c.LiveCache.MemoryHighPct > 100 {
		c.LiveCache.MemoryHighPct = 90
	}
	if c.LiveCache.MemoryLowPct <= 0 || c.LiveCache.MemoryLowPct >= c.LiveCache.MemoryHighPct {
		c.LiveCache.MemoryLowPct = max(c.LiveCache.MemoryHighPct-10, 1)
	}
	if c.LiveCache.CheckIntervalS <= 0 {
		c.LiveCache.CheckIntervalS = 5
	}
	if c.Server.DataDir == "" {
		c.Server.DataDir = "./data"
	}
	if c.Server.LogLevel == "" {
		c.Server.LogLevel = "info"
	}
	if c.HTTP.Listen == "" {
		c.HTTP.Listen = "0.0.0.0:8080"
	}
	if c.HTTP.TLSEnabled && c.HTTP.TLSListen == "" {
		c.HTTP.TLSListen = "0.0.0.0:443"
	}
	if c.RTMP.Listen == "" {
		c.RTMP.Listen = "0.0.0.0:1935"
	}
	if c.RTMP.IdleTimeoutMs == 0 {
		c.RTMP.IdleTimeoutMs = 10000
	}
	if c.RTMP.MaxConnsPerApp == 0 {
		c.RTMP.MaxConnsPerApp = 100
	}
	if c.HLS.SegmentDurationS == 0 {
		c.HLS.SegmentDurationS = 6
	}
	if c.HLS.PlaylistWindow == 0 {
		c.HLS.PlaylistWindow = 5
	}
	if c.HLS.Container == "" {
		c.HLS.Container = "ts"
	}
	if c.HLS.PathPrefix == "" {
		c.HLS.PathPrefix = "/hls"
	}
	if c.HLS.LLHls.Enabled {
		if c.HLS.LLHls.PartDurationMs == 0 {
			c.HLS.LLHls.PartDurationMs = 350
		}
		if c.HLS.LLHls.HoldBackParts == 0 {
			c.HLS.LLHls.HoldBackParts = 3
		}
	}
	// DASH.PathPrefix has no forced default (unlike HLS.PathPrefix above):
	// DASH delivery is mounted with no URL prefix at all, same as HLS/raw-TS
	// (see router.go) — an empty PathPrefix is the normal, intended value,
	// consulted only for absolute BaseURL construction in a multi-bitrate
	// ABR group's manifest (internal/dash.RenderGroupMPD), where it
	// correctly renders as no prefix. Set it explicitly in config.toml only
	// if DASH is deliberately reverse-proxied under a real path prefix.
	if c.DASH.SegmentDurationS == 0 {
		c.DASH.SegmentDurationS = 4
	}
	if c.DVR.StoragePath == "" {
		c.DVR.StoragePath = "./data/dvr"
	}
	if c.DVR.WindowSeconds == 0 {
		c.DVR.WindowSeconds = 3600
	}
	if c.Failover.WatchdogIntervalMs == 0 {
		c.Failover.WatchdogIntervalMs = 1000
	}
	if c.Failover.StallTimeoutMs == 0 {
		c.Failover.StallTimeoutMs = 5000
	}
	if c.Failover.WarmupBufferMs == 0 {
		c.Failover.WarmupBufferMs = 2000
	}
	if c.Auth.JWTExpiryHours == 0 {
		c.Auth.JWTExpiryHours = 24
	}
	if c.Auth.RefreshExpiryDays == 0 {
		c.Auth.RefreshExpiryDays = 7
	}
	if c.RTMP.RouteSeparator == "" {
		c.RTMP.RouteSeparator = "-"
	}
	if c.Metrics.Path == "" {
		c.Metrics.Path = "/metrics"
	}
	if c.Multinode.HeartbeatIntervalS == 0 {
		c.Multinode.HeartbeatIntervalS = 5
	}
	if c.Multinode.OwnershipPolicy == "" {
		c.Multinode.OwnershipPolicy = "first_publisher"
	}
	if c.Multinode.NodePriority == 0 {
		c.Multinode.NodePriority = 100
	}
	if c.Multinode.PeerForgetS == 0 {
		c.Multinode.PeerForgetS = 60
	}
	if c.Multinode.AdvertiseURL == "" {
		c.Multinode.AdvertiseURL = defaultAdvertiseURL(c)
	}
}

// defaultAdvertiseURL best-effort-derives a reachable base URL when
// multinode.advertise_url is unset: this host's own hostname plus the
// HTTP listen port. Real multi-host deployments behind NAT/a load
// balancer/a different public hostname should set advertise_url
// explicitly — this fallback exists so multinode mode is still usable
// out of the box on a single LAN.
func defaultAdvertiseURL(c *Config) string {
	scheme := "http"
	if c.HTTP.TLSEnabled {
		scheme = "https"
	}
	_, port, err := net.SplitHostPort(c.HTTP.Listen)
	if err != nil {
		port = "8080"
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "localhost"
	}
	return fmt.Sprintf("%s://%s:%s", scheme, host, port)
}

func (c *Config) validate() error {
	if c.Auth.JWTSecret == "" || c.Auth.JWTSecret == "CHANGE-ME-minimum-32-characters-long" {
		return fmt.Errorf("auth.jwt_secret must be set to a secure random string")
	}
	if len(c.Auth.JWTSecret) < 32 {
		return fmt.Errorf("auth.jwt_secret must be at least 32 characters")
	}
	return nil
}

// AppByName returns the RTMP application config for the given app name, or nil.
func (c *RTMPConfig) AppByName(name string) *RTMPApp {
	for i := range c.Applications {
		if c.Applications[i].Name == name {
			return &c.Applications[i]
		}
	}
	return nil
}
