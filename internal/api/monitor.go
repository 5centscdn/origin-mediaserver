package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/livestore"
	"github.com/5centscdn/origin-mediaserver/internal/sysmon"
)

// SetMonitor attaches the dashboard metrics collector (cmd/server starts it).
func (s *Server) SetMonitor(c *sysmon.Collector) { s.monitor = c }

// handleMonitor serves GET /api/v1/monitor: the collector's latest snapshot
// (system, network, services, stream events) plus the live cache status.
// Cheap: it copies cached values, no I/O.
func (s *Server) handleMonitor(w http.ResponseWriter, r *http.Request) {
	if s.monitor == nil {
		jsonError(w, http.StatusServiceUnavailable, "monitoring not running")
		return
	}
	data, _ := json.Marshal(struct {
		sysmon.Snapshot
		LiveCache livestore.Status `json:"live_cache"`
		ABRGroups int              `json:"abr_groups_live"`
	}{s.monitor.Snapshot(), livestore.Default().Status(), len(s.registry.AllABRGroups())})
	jsonOK(w, data)
}

// monitorRanges are the chart ranges the panel offers.
var monitorRanges = map[string]time.Duration{
	"5m": 5 * time.Minute, "15m": 15 * time.Minute, "1h": time.Hour, "24h": 24 * time.Hour,
}

// handleMonitorHistory serves GET /api/v1/monitor/history?range=5m|15m|1h|24h:
// 5 s samples up to 1 h, 1-minute averages for 24 h.
func (s *Server) handleMonitorHistory(w http.ResponseWriter, r *http.Request) {
	if s.monitor == nil {
		jsonError(w, http.StatusServiceUnavailable, "monitoring not running")
		return
	}
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "15m"
	}
	d, ok := monitorRanges[rng]
	if !ok {
		jsonError(w, http.StatusBadRequest, "range must be 5m, 15m, 1h or 24h")
		return
	}
	samples, step := s.monitor.History(d)
	if samples == nil {
		samples = []sysmon.Sample{}
	}
	data, _ := json.Marshal(map[string]any{"range": rng, "step_s": step, "samples": samples})
	jsonOK(w, data)
}
