// Package sysmon collects the dashboard's server metrics: CPU, memory,
// disk, network throughput, connections, service (listener) health and
// stream events, plus a bounded history for the charts.
//
// One goroutine samples every Interval (5 s) from /proc and statfs: no
// process spawning, no per-request work. API handlers read the cached
// Snapshot and History. History is two fixed rings: every sample for the
// last hour (720 × 5 s) and per-minute averages for the last 24 h (1440 ×
// 60 s), about 130 KB in total.
//
// Scope and accuracy:
//   - Linux only. Elsewhere (or if a /proc file can't be read) the field is
//     null and the panel shows it as unavailable; nothing is estimated.
//   - Network in/out are host NIC counters (/proc/net/dev), excluding
//     loopback and virtual bridges, so they include all traffic on the box,
//     not only this server's. A counter that goes backwards (interface
//     reset, wrap) skips that sample instead of producing a spike.
//   - Ingest is the sum of every stream's received bytes (media.Bus
//     BytesIn), tracked per *Stream so a reconnect doesn't count twice. A
//     stream that ends loses at most the bytes since the previous sample.
//   - Connections are established TCP sockets on this server's HTTP and
//     RTMP listen ports (/proc/net/tcp{,6}). SRT is UDP and has no
//     connection state there.
//   - Service status is whether the listen port is actually bound (LISTEN
//     for TCP, bound for UDP), read from the same tables.
//   - Stream events come from media.Registry (streams started/ended) and
//     the pull manager (connection attempts/failures).
package sysmon

import (
	"context"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

const (
	fineEvery    = 5 * time.Second
	fineSlots    = 720 // 1 h of 5 s samples
	coarseEvery  = time.Minute
	coarseSlots  = 1440 // 24 h of 1 min averages
	dvrEvery     = time.Minute
	stalledAfter = 10 * time.Second // no media for this long = stalled
)

// Sample is one point of history. Pointer fields are nil when the metric
// couldn't be read for that interval; event fields count events within it.
type Sample struct {
	T            int64    `json:"t"` // unix ms, end of the interval
	NetInBps     *float64 `json:"net_in_bps"`
	NetOutBps    *float64 `json:"net_out_bps"`
	IngestBps    float64  `json:"ingest_bps"`
	CPUPct       *float64 `json:"cpu_pct"`
	ProcCPUPct   *float64 `json:"proc_cpu_pct"`
	MemPct       *float64 `json:"mem_pct"`
	Streams      float64  `json:"streams"` // average over the interval for 1-min points
	Viewers      float64  `json:"viewers"`
	Published    int64    `json:"published"`
	Ended        int64    `json:"ended"`
	PullAttempts int64    `json:"pull_attempts"`
	PullFailures int64    `json:"pull_failures"`
}

// Usage is a used/total pair in bytes.
type Usage struct {
	Path  string  `json:"path,omitempty"`
	Total int64   `json:"total"`
	Used  int64   `json:"used"`
	Pct   float64 `json:"pct"`
}

// Service is one listener's state: "up", "down", "disabled" or "unknown".
type Service struct {
	Name    string `json:"name"`
	Proto   string `json:"proto"` // tcp/udp
	Listen  string `json:"listen"`
	Status  string `json:"status"`
	Enabled bool   `json:"enabled"`
}

// Snapshot is the current state, refreshed every sample.
type Snapshot struct {
	Time      int64 `json:"time"`
	IntervalS int   `json:"interval_s"`
	Host      struct {
		Hostname string   `json:"hostname"`
		Cores    int      `json:"cores"`
		UptimeS  *float64 `json:"uptime_s"`
	} `json:"host"`
	Process struct {
		UptimeS    float64  `json:"uptime_s"`
		RSS        *int64   `json:"rss_bytes"`
		CPUPct     *float64 `json:"cpu_pct"` // share of the whole machine
		Goroutines int      `json:"goroutines"`
	} `json:"process"`
	CPUPct  *float64 `json:"cpu_pct"`
	Memory  *Usage   `json:"memory"`
	Disk    *Usage   `json:"disk"`     // filesystem holding data_dir
	DVRDisk *Usage   `json:"dvr_disk"` // filesystem holding DVR storage
	// DVRBytes is the size of all recordings (dvr_segments), refreshed
	// every minute; nil until the first query returns.
	DVRBytes *int64 `json:"dvr_bytes"`
	Network  struct {
		InBps     *float64 `json:"in_bps"`
		OutBps    *float64 `json:"out_bps"`
		IngestBps float64  `json:"ingest_bps"`
	} `json:"network"`
	Connections *struct {
		HTTP  int `json:"http"`
		RTMP  int `json:"rtmp"`
		Total int `json:"total"`
	} `json:"connections"`
	Services []Service `json:"services"`
	Streams  struct {
		Active  int `json:"active"`
		Stalled int `json:"stalled"`
		Viewers int `json:"viewers"`
	} `json:"streams"`
	// Totals since the process started.
	Totals struct {
		Since        int64 `json:"since"` // unix ms
		BytesIn      int64 `json:"bytes_in"`
		Published    int64 `json:"published"`
		Ended        int64 `json:"ended"`
		PullAttempts int64 `json:"pull_attempts"`
		PullFailures int64 `json:"pull_failures"`
	} `json:"totals"`
}

// Config tells the collector what to watch.
type Config struct {
	DataDir     string
	DVRPath     string
	HTTPListen  string
	HTTPSListen string // "" when TLS is off
	RTMPListen  string
	RTMPSListen string // "" when RTMPS is off
	SRTListen   string // "" when SRT is off
}

// Sources are the in-process counters the collector reads.
type Sources struct {
	Registry  *media.Registry
	PullStats func() (attempts, failures int64)
	// DVRBytes returns the total size of all recordings; called once a
	// minute. nil = not available.
	DVRBytes func(ctx context.Context) (int64, error)
}

// Collector samples metrics in the background.
type Collector struct {
	cfg   Config
	src   Sources
	start time.Time

	mu       sync.RWMutex
	snap     Snapshot
	fine     ring
	coarse   ring
	pending  []Sample // fine samples not yet folded into a coarse point
	dvrBytes *int64

	// previous readings, owned by the sampling goroutine
	prevAt      time.Time
	prevCPU     cpuTimes
	prevCPUOK   bool
	prevProc    uint64
	prevProcOK  bool
	prevRx      uint64
	prevTx      uint64
	prevNetOK   bool
	streamBytes map[*media.Stream]int64
	endedBytes  int64
	prevBytesIn int64
	prevPub     int64
	prevEnd     int64
	prevAtt     int64
	prevFail    int64
}

// New creates a collector; call Run to start sampling.
func New(cfg Config, src Sources) *Collector {
	return &Collector{
		cfg:         cfg,
		src:         src,
		start:       time.Now(),
		fine:        newRing(fineSlots),
		coarse:      newRing(coarseSlots),
		streamBytes: map[*media.Stream]int64{},
	}
}

// Run samples until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) {
	c.sample(time.Now()) // baseline for rates
	c.refreshDVR(ctx)
	tick := time.NewTicker(fineEvery)
	defer tick.Stop()
	dvr := time.NewTicker(dvrEvery)
	defer dvr.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			c.sample(now)
		case <-dvr.C:
			c.refreshDVR(ctx)
		}
	}
}

func (c *Collector) refreshDVR(ctx context.Context) {
	if c.src.DVRBytes == nil {
		return
	}
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if n, err := c.src.DVRBytes(qctx); err == nil {
		c.mu.Lock()
		c.dvrBytes = &n
		c.mu.Unlock()
	}
}

func f64(v float64) *float64 { return &v }

// sample takes one reading. The first call only records baselines (rates
// need two readings), so it adds no history point.
func (c *Collector) sample(now time.Time) {
	var s Snapshot
	s.Time = now.UnixMilli()
	s.IntervalS = int(fineEvery / time.Second)
	s.Host.Hostname, _ = os.Hostname()
	s.Host.Cores = runtime.NumCPU()
	if up, ok := readUptime(); ok {
		s.Host.UptimeS = &up
	}
	s.Process.UptimeS = now.Sub(c.start).Seconds()
	s.Process.Goroutines = runtime.NumGoroutine()
	if rss, ok := readRSS(); ok {
		s.Process.RSS = &rss
	}

	first := c.prevAt.IsZero()
	elapsed := now.Sub(c.prevAt).Seconds()

	// CPU
	cpu, cores, cpuOK := readCPUTimes()
	if cores > 0 {
		s.Host.Cores = cores
	}
	if cpuOK && c.prevCPUOK && cpu.total > c.prevCPU.total {
		dt := float64(cpu.total - c.prevCPU.total)
		di := float64(cpu.idle - c.prevCPU.idle)
		s.CPUPct = f64(clampPct(100 * (dt - di) / dt))
	}
	proc, procOK := readProcCPU()
	if procOK && c.prevProcOK && elapsed > 0 && proc >= c.prevProc {
		used := float64(proc-c.prevProc) / clkTck // CPU-seconds
		s.Process.CPUPct = f64(clampPct(100 * used / elapsed / float64(max(s.Host.Cores, 1))))
	}
	c.prevCPU, c.prevCPUOK, c.prevProc, c.prevProcOK = cpu, cpuOK, proc, procOK

	// Memory
	if m, ok := readMemInfo(); ok {
		used := m.total - m.avail
		s.Memory = &Usage{Total: m.total, Used: used, Pct: 100 * float64(used) / float64(m.total)}
	}

	// Disks
	s.Disk = statfs(c.cfg.DataDir)
	s.DVRDisk = statfs(c.cfg.DVRPath)

	// Network (host NICs). A counter going backwards skips this interval.
	rx, tx, netOK := readNetCounters()
	if netOK && c.prevNetOK && elapsed > 0 && rx >= c.prevRx && tx >= c.prevTx {
		s.Network.InBps = f64(float64(rx-c.prevRx) * 8 / elapsed)
		s.Network.OutBps = f64(float64(tx-c.prevTx) * 8 / elapsed)
	}
	c.prevRx, c.prevTx, c.prevNetOK = rx, tx, netOK

	// Streams: counts, stalled, viewers, ingest bytes per *Stream.
	var live []*media.Stream
	if c.src.Registry != nil {
		live = c.src.Registry.All()
	}
	seen := make(map[*media.Stream]bool, len(live))
	var liveBytes int64
	for _, st := range live {
		seen[st] = true
		b := st.Bus.BytesIn()
		c.streamBytes[st] = b
		liveBytes += b
		s.Streams.Viewers += st.Viewers.Count()
		if now.Sub(st.StartedAt) > stalledAfter {
			if last := st.Bus.LastMediaAt(); last.IsZero() || now.Sub(last) > stalledAfter {
				s.Streams.Stalled++
			}
		}
	}
	for st, b := range c.streamBytes {
		if !seen[st] {
			c.endedBytes += b // the stream ended: keep what it received
			delete(c.streamBytes, st)
		}
	}
	s.Streams.Active = len(live)
	totalIn := c.endedBytes + liveBytes
	if !first && elapsed > 0 && totalIn >= c.prevBytesIn {
		s.Network.IngestBps = float64(totalIn-c.prevBytesIn) * 8 / elapsed
	}

	// Sockets: connections and listener health.
	if t, ok := readSockets(); ok {
		httpN := t.established[port(c.cfg.HTTPListen)] + t.established[port(c.cfg.HTTPSListen)]
		rtmpN := t.established[port(c.cfg.RTMPListen)] + t.established[port(c.cfg.RTMPSListen)]
		s.Connections = &struct {
			HTTP  int `json:"http"`
			RTMP  int `json:"rtmp"`
			Total int `json:"total"`
		}{httpN, rtmpN, httpN + rtmpN}
		s.Services = c.services(&t)
	} else {
		s.Services = c.services(nil)
	}

	// Event counters.
	var pub, end, att, fail int64
	if c.src.Registry != nil {
		pub, end = c.src.Registry.EventCounts()
	}
	if c.src.PullStats != nil {
		att, fail = c.src.PullStats()
	}
	s.Totals.Since = c.start.UnixMilli()
	s.Totals.BytesIn = totalIn
	s.Totals.Published, s.Totals.Ended, s.Totals.PullAttempts, s.Totals.PullFailures = pub, end, att, fail

	pt := Sample{
		T: s.Time, NetInBps: s.Network.InBps, NetOutBps: s.Network.OutBps, IngestBps: s.Network.IngestBps,
		CPUPct: s.CPUPct, ProcCPUPct: s.Process.CPUPct,
		Streams: float64(s.Streams.Active), Viewers: float64(s.Streams.Viewers),
		Published: pub - c.prevPub, Ended: end - c.prevEnd, PullAttempts: att - c.prevAtt, PullFailures: fail - c.prevFail,
	}
	if s.Memory != nil {
		pt.MemPct = f64(s.Memory.Pct)
	}
	c.prevAt, c.prevBytesIn = now, totalIn
	c.prevPub, c.prevEnd, c.prevAtt, c.prevFail = pub, end, att, fail

	c.mu.Lock()
	s.DVRBytes = c.dvrBytes
	c.snap = s
	if !first {
		c.fine.push(pt)
		c.pending = append(c.pending, pt)
		if len(c.pending) >= int(coarseEvery/fineEvery) {
			c.coarse.push(average(c.pending))
			c.pending = c.pending[:0]
		}
	}
	c.mu.Unlock()
}

func (c *Collector) services(t *sockTable) []Service {
	svc := func(name, proto, listen string, enabled bool) Service {
		s := Service{Name: name, Proto: proto, Listen: listen, Enabled: enabled, Status: "disabled"}
		if !enabled {
			return s
		}
		p := port(listen)
		switch {
		case t == nil || p == 0:
			s.Status = "unknown"
		case proto == "udp" && t.udpBound[p], proto == "tcp" && t.listening[p]:
			s.Status = "up"
		default:
			s.Status = "down"
		}
		return s
	}
	out := []Service{
		svc("HTTP (HLS/DASH/API)", "tcp", c.cfg.HTTPListen, c.cfg.HTTPListen != ""),
		svc("RTMP", "tcp", c.cfg.RTMPListen, c.cfg.RTMPListen != ""),
		svc("SRT", "udp", c.cfg.SRTListen, c.cfg.SRTListen != ""),
	}
	if c.cfg.HTTPSListen != "" {
		out = append(out, svc("HTTPS", "tcp", c.cfg.HTTPSListen, true))
	}
	if c.cfg.RTMPSListen != "" {
		out = append(out, svc("RTMPS", "tcp", c.cfg.RTMPSListen, true))
	}
	return out
}

// Snapshot returns the latest reading.
func (c *Collector) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snap
}

// History returns the samples within the last d: 5 s points up to an hour,
// 1-minute averages beyond. step is the spacing in seconds.
func (c *Collector) History(d time.Duration) (samples []Sample, step int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, step := &c.fine, int(fineEvery/time.Second)
	if d > fineSlots*fineEvery {
		r, step = &c.coarse, int(coarseEvery/time.Second)
	}
	cutoff := time.Now().Add(-d).UnixMilli()
	for _, s := range r.items() {
		if s.T >= cutoff {
			samples = append(samples, s)
		}
	}
	return samples, step
}

// average folds fine samples into one coarse point: means of the rates and
// percentages (over the samples that have them), sums of the events.
func average(ps []Sample) Sample {
	out := Sample{T: ps[len(ps)-1].T}
	mean := func(get func(Sample) *float64) *float64 {
		var sum float64
		n := 0
		for _, p := range ps {
			if v := get(p); v != nil {
				sum += *v
				n++
			}
		}
		if n == 0 {
			return nil
		}
		return f64(sum / float64(n))
	}
	out.NetInBps = mean(func(s Sample) *float64 { return s.NetInBps })
	out.NetOutBps = mean(func(s Sample) *float64 { return s.NetOutBps })
	out.CPUPct = mean(func(s Sample) *float64 { return s.CPUPct })
	out.ProcCPUPct = mean(func(s Sample) *float64 { return s.ProcCPUPct })
	out.MemPct = mean(func(s Sample) *float64 { return s.MemPct })
	for _, p := range ps {
		out.IngestBps += p.IngestBps / float64(len(ps))
		out.Streams += p.Streams / float64(len(ps))
		out.Viewers += p.Viewers / float64(len(ps))
		out.Published += p.Published
		out.Ended += p.Ended
		out.PullAttempts += p.PullAttempts
		out.PullFailures += p.PullFailures
	}
	return out
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func statfs(path string) *Usage {
	if path == "" {
		return nil
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return nil
	}
	// Same numbers as df: used = total - free, and the percentage is of
	// the space normal users can have (used + available), so root-reserved
	// blocks don't make a full disk look 95%.
	bs := int64(st.Bsize)
	total := int64(st.Blocks) * bs
	used := total - int64(st.Bfree)*bs
	avail := int64(st.Bavail) * bs
	if total <= 0 || used+avail <= 0 {
		return nil
	}
	return &Usage{Path: path, Total: used + avail, Used: used, Pct: 100 * float64(used) / float64(used+avail)}
}

// port extracts the port from a listen address like "0.0.0.0:1935" or ":8080".
func port(listen string) int {
	if listen == "" {
		return 0
	}
	_, p, err := net.SplitHostPort(listen)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

// ring is a fixed-size circular buffer of samples.
type ring struct {
	buf  []Sample
	next int
	full bool
}

func newRing(n int) ring { return ring{buf: make([]Sample, n)} }

func (r *ring) push(s Sample) {
	r.buf[r.next] = s
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
}

// items returns the samples oldest first.
func (r *ring) items() []Sample {
	if !r.full {
		return append([]Sample(nil), r.buf[:r.next]...)
	}
	return append(append([]Sample(nil), r.buf[r.next:]...), r.buf[:r.next]...)
}
