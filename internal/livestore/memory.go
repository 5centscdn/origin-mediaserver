package livestore

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// memUsedPct is how full memory is, in percent, or -1 if it can't be read
// (not Linux). It's the higher of:
//   - the system: (MemTotal - MemAvailable) / MemTotal, from /proc/meminfo.
//     MemAvailable counts reclaimable page cache as free, which matters:
//     disk mode itself fills the page cache.
//   - this process's cgroup, when it has a memory limit (systemd
//     MemoryMax=, a container): usage minus inactive file cache over the
//     limit. That limit is what the OOM killer enforces.
//
// A variable so tests can fake memory pressure.
var memUsedPct = func() float64 {
	pct := systemMemPct()
	if c := cgroupMemPct(); c > pct {
		pct = c
	}
	return pct
}

func systemMemPct() float64 {
	m := readKV("/proc/meminfo")
	total, avail := m["MemTotal"], m["MemAvailable"]
	if total <= 0 {
		return -1
	}
	return 100 * float64(total-avail) / float64(total)
}

// readKV parses "Key: 123 kB" (meminfo) and "key 123" (memory.stat) lines.
func readKV(path string) map[string]int64 {
	out := map[string]int64{}
	fh, err := os.Open(path)
	if err != nil {
		return out
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		f := strings.Fields(strings.Replace(sc.Text(), ":", " ", 1))
		if len(f) >= 2 {
			if v, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				out[f[0]] = v
			}
		}
	}
	return out
}

func readInt(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return v, err == nil // "max" (no limit) fails here
}

// cgroupMemPct reads this process's cgroup memory use against its limit
// (cgroup v2, then v1), or -1 if there's no limit.
func cgroupMemPct() float64 {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		switch {
		case parts[0] == "0" && parts[1] == "": // v2
			dir := "/sys/fs/cgroup" + parts[2]
			limit, ok := readInt(dir + "/memory.max")
			cur, ok2 := readInt(dir + "/memory.current")
			if ok && ok2 && limit > 0 {
				used := cur - readKV(dir + "/memory.stat")["inactive_file"]
				return 100 * float64(used) / float64(limit)
			}
		case strings.Contains(parts[1], "memory"): // v1
			dir := "/sys/fs/cgroup/memory" + parts[2]
			limit, ok := readInt(dir + "/memory.limit_in_bytes")
			cur, ok2 := readInt(dir + "/memory.usage_in_bytes")
			if ok && ok2 && limit > 0 && limit < 1<<60 { // v1 "no limit" is a huge number
				used := cur - readKV(dir + "/memory.stat")["total_inactive_file"]
				return 100 * float64(used) / float64(limit)
			}
		}
	}
	return -1
}

// check reads memory once and, in hybrid mode, switches storage across
// the thresholds.
func (s *Store) check() {
	pct := memUsedPct()
	s.mu.Lock()
	s.memPct = pct
	high, low, disk, policy := s.highPct, s.lowPct, s.diskMode, s.policy
	s.mu.Unlock()
	if pct < 0 {
		return
	}
	switch policy {
	case "hybrid":
		switch {
		case !disk && pct >= float64(high):
			slog.Warn("live cache: memory high, switching live output to disk", "memory_used_pct", round1(pct), "threshold_pct", high)
			s.setDiskMode(true)
		case disk && pct < float64(low):
			slog.Info("live cache: memory back to normal, live output in RAM again", "memory_used_pct", round1(pct), "resume_pct", low)
			s.setDiskMode(false)
		}
	case "memory":
		if pct >= float64(high) {
			slog.Warn("live cache: memory high and mode is memory, not moving to disk", "memory_used_pct", round1(pct), "threshold_pct", high)
		}
	}
}

// Configure sets the storage mode ("hybrid", "memory" or "disk"; see the
// package doc) and thresholds, and applies them at once: disk moves what's
// in RAM to disk, memory sends new files to RAM again, hybrid decides from
// the current memory reading. Safe to call while serving (the panel's
// Settings page does).
func (s *Store) Configure(mode string, highPct, lowPct int) {
	switch mode {
	case "memory", "disk":
	default:
		mode = "hybrid"
	}
	s.mu.Lock()
	s.policy, s.highPct, s.lowPct = mode, highPct, lowPct
	s.mu.Unlock()
	slog.Info("live cache", "mode", mode, "memory_high_pct", highPct, "memory_low_pct", lowPct)
	switch mode {
	case "disk":
		s.setDiskMode(true)
	case "memory":
		s.setDiskMode(false)
	default:
		// Hybrid: back to RAM unless memory is already high (then check
		// below keeps it on disk, or moves it there).
		if pct := memUsedPct(); pct < float64(highPct) {
			s.setDiskMode(false)
		}
	}
	s.check()
}

// StartMonitor configures the store (see Configure) and checks memory
// every interval: hybrid switches storage at the thresholds, and every
// mode reports the reading in Status. Call once at startup.
func (s *Store) StartMonitor(mode string, highPct, lowPct int, interval time.Duration) {
	s.Configure(mode, highPct, lowPct)
	go func() {
		for range time.Tick(interval) {
			s.check()
		}
	}()
}

// Status is the live cache's state for the API and panel.
type Status struct {
	Policy        string  `json:"policy"` // configured mode: "hybrid", "memory" or "disk"
	Mode          string  `json:"mode"`   // where new output goes now: "memory" or "disk"
	MemoryUsedPct float64 `json:"memory_used_pct"`
	HighPct       int     `json:"memory_high_pct"`
	LowPct        int     `json:"memory_low_pct"`
	Since         string  `json:"since,omitempty"` // when Mode last changed (RFC 3339)
	Files         int     `json:"files"`           // in RAM
	Bytes         int64   `json:"bytes"`
	DiskFiles     int     `json:"disk_files"`
	DiskBytes     int64   `json:"disk_bytes"`
	Warning       string  `json:"warning,omitempty"`
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }

// Status reports the current mode, memory reading and sizes.
func (s *Store) Status() Status {
	mf, mb, df, db := s.Stats()
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Status{
		Policy: s.policy, Mode: "memory", MemoryUsedPct: round1(s.memPct), HighPct: s.highPct, LowPct: s.lowPct,
		Files: mf, Bytes: mb, DiskFiles: df, DiskBytes: db,
	}
	if !s.since.IsZero() {
		st.Since = s.since.UTC().Format(time.RFC3339)
	}
	if s.diskMode {
		st.Mode = "disk"
	}
	switch {
	case s.policy == "hybrid" && s.diskMode:
		st.Warning = fmt.Sprintf("Memory at %.0f%% (limit %d%%): live HLS/DASH output switched from RAM to disk to avoid running out of memory. It moves back to RAM below %d%%.",
			s.memPct, s.highPct, s.lowPct)
	case s.policy == "memory" && s.memPct >= float64(s.highPct) && s.highPct > 0:
		st.Warning = fmt.Sprintf("Memory at %.0f%% (limit %d%%) and the live cache is set to memory only, so live output is not moving to disk. The server may run out of memory: free memory, or set [live_cache] mode to hybrid.",
			s.memPct, s.highPct)
	}
	return st
}
