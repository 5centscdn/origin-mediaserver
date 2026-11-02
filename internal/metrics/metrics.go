// Package metrics exposes Prometheus-compatible metrics for the media server.
// The scrape endpoint at /metrics is compatible with any Prometheus or
// OpenMetrics-compliant scraper (Grafana Agent, Victoria Metrics, etc.).
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds all Prometheus counters and gauges for Origin Media Server.
type Metrics struct {
	registry *prometheus.Registry

	// Ingest
	RTMPConnections prometheus.Gauge
	RTMPBytesIn     *prometheus.CounterVec
	ActiveStreams   prometheus.Gauge
	ActiveABRGroups prometheus.Gauge

	// HLS delivery
	HLSPlaylistRequests *prometheus.CounterVec
	HLSSegmentRequests  *prometheus.CounterVec
	HLSSegmentBytesOut  *prometheus.CounterVec

	// DVR
	DVRSegmentsStored *prometheus.GaugeVec
	DVRStorageBytes   *prometheus.GaugeVec

	// Failover
	FailoverEvents *prometheus.CounterVec

	// WebRTC
	WHEPSessions prometheus.Gauge
	WHIPSessions prometheus.Gauge

	// Multi-node clustering
	ClusterPeers prometheus.Gauge

	// SCTE-35
	SCTE35EventsTotal      *prometheus.CounterVec
	SCTE35OutTotal         *prometheus.CounterVec
	SCTE35InTotal          *prometheus.CounterVec
	SCTE35ParseErrorsTotal *prometheus.CounterVec
	SCTE35DuplicatesTotal  *prometheus.CounterVec

	// SCTE-35 server-side insertion (internal/scte35ctrl)
	SCTE35InsertedTotal     *prometheus.CounterVec
	SCTE35InsertErrorsTotal *prometheus.CounterVec
	SCTE35ActiveBreaks      *prometheus.GaugeVec
	SCTE35PassthroughTotal  *prometheus.CounterVec
}

// New creates and registers all metrics.
func New() *Metrics {
	reg := prometheus.NewRegistry()

	// Include Go runtime and process metrics.
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	m := &Metrics{
		registry: reg,

		RTMPConnections: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "origin",
			Subsystem: "rtmp",
			Name:      "connections_active",
			Help:      "Number of active RTMP ingest connections.",
		}),

		RTMPBytesIn: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "origin",
			Subsystem: "rtmp",
			Name:      "bytes_in_total",
			Help:      "Total bytes received from RTMP publishers.",
		}, []string{"app", "stream"}),

		ActiveStreams: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "origin",
			Name:      "active_streams",
			Help:      "Number of currently live streams.",
		}),

		ActiveABRGroups: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "origin",
			Name:      "active_abr_groups",
			Help:      "Number of active ABR rendition groups.",
		}),

		HLSPlaylistRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "origin",
			Subsystem: "hls",
			Name:      "playlist_requests_total",
			Help:      "Total HLS playlist (M3U8) requests.",
		}, []string{"app", "stream", "type"}),

		HLSSegmentRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "origin",
			Subsystem: "hls",
			Name:      "segment_requests_total",
			Help:      "Total HLS segment requests.",
		}, []string{"app", "stream"}),

		HLSSegmentBytesOut: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "origin",
			Subsystem: "hls",
			Name:      "segment_bytes_out_total",
			Help:      "Total bytes sent as HLS segments.",
		}, []string{"app", "stream"}),

		DVRSegmentsStored: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "origin",
			Subsystem: "dvr",
			Name:      "segments_stored",
			Help:      "Number of DVR segments currently on disk.",
		}, []string{"app", "stream"}),

		DVRStorageBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "origin",
			Subsystem: "dvr",
			Name:      "storage_bytes",
			Help:      "Total DVR storage used in bytes.",
		}, []string{"app", "stream"}),

		FailoverEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "origin",
			Subsystem: "failover",
			Name:      "events_total",
			Help:      "Total failover events by type.",
		}, []string{"app", "stream", "trigger"}),

		WHEPSessions: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "origin",
			Subsystem: "webrtc",
			Name:      "whep_sessions_active",
			Help:      "Active WebRTC WHEP playback sessions.",
		}),

		WHIPSessions: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "origin",
			Subsystem: "webrtc",
			Name:      "whip_sessions_active",
			Help:      "Active WebRTC WHIP ingest sessions.",
		}),

		ClusterPeers: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "origin",
			Subsystem: "cluster",
			Name:      "peers_active",
			Help:      "Number of other nodes currently visible via multi-node heartbeat discovery.",
		}),

		SCTE35EventsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "origin",
			Subsystem: "scte35",
			Name:      "events_total",
			Help:      "Total SCTE-35 splice_info_section messages detected, by command type.",
		}, []string{"app", "stream", "command"}),

		SCTE35OutTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "origin",
			Subsystem: "scte35",
			Name:      "out_total",
			Help:      "Total SCTE-35-driven ad-break OUT transitions (HLS marker layer).",
		}, []string{"app", "stream"}),

		SCTE35InTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "origin",
			Subsystem: "scte35",
			Name:      "in_total",
			Help:      "Total SCTE-35-driven ad-break IN transitions (HLS marker layer).",
		}, []string{"app", "stream"}),

		SCTE35ParseErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "origin",
			Subsystem: "scte35",
			Name:      "parse_errors_total",
			Help:      "Total SCTE-35 sections dropped due to a parse or CRC error.",
		}, []string{"app", "stream"}),

		SCTE35DuplicatesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "origin",
			Subsystem: "scte35",
			Name:      "duplicates_total",
			Help:      "Total retransmitted SCTE-35 messages suppressed by dedup (no marker re-emitted).",
		}, []string{"app", "stream"}),

		SCTE35InsertedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "origin",
			Subsystem: "scte35",
			Name:      "inserted_total",
			Help:      "Total server-generated SCTE-35 sections injected, by action (out/in).",
		}, []string{"app", "stream", "action"}),

		SCTE35InsertErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "origin",
			Subsystem: "scte35",
			Name:      "insert_errors_total",
			Help:      "Total server-side SCTE-35 insertion failures (generation or injection errors).",
		}, []string{"app", "stream"}),

		SCTE35ActiveBreaks: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "origin",
			Subsystem: "scte35",
			Name:      "active_breaks",
			Help:      "1 if a server-managed (inserted) ad break is currently active for the stream, else 0.",
		}, []string{"app", "stream"}),

		SCTE35PassthroughTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "origin",
			Subsystem: "scte35",
			Name:      "passthrough_total",
			Help:      "Total incoming SCTE-35 sections actually forwarded to MPEG-TS/HLS output under passthrough or hybrid mode.",
		}, []string{"app", "stream"}),
	}

	reg.MustRegister(
		m.RTMPConnections,
		m.RTMPBytesIn,
		m.ActiveStreams,
		m.ActiveABRGroups,
		m.HLSPlaylistRequests,
		m.HLSSegmentRequests,
		m.HLSSegmentBytesOut,
		m.DVRSegmentsStored,
		m.DVRStorageBytes,
		m.FailoverEvents,
		m.WHEPSessions,
		m.WHIPSessions,
		m.ClusterPeers,
		m.SCTE35EventsTotal,
		m.SCTE35OutTotal,
		m.SCTE35InTotal,
		m.SCTE35ParseErrorsTotal,
		m.SCTE35DuplicatesTotal,
		m.SCTE35InsertedTotal,
		m.SCTE35InsertErrorsTotal,
		m.SCTE35ActiveBreaks,
		m.SCTE35PassthroughTotal,
	)

	return m
}

// Handler returns an http.HandlerFunc suitable for the /metrics endpoint.
func (m *Metrics) Handler() http.HandlerFunc {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}).ServeHTTP
}
