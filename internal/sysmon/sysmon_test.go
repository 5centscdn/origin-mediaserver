package sysmon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// fakeProc writes /proc fixtures under a temp dir and points procRoot at it.
type fakeProc struct {
	t    *testing.T
	root string
}

func newFakeProc(t *testing.T) *fakeProc {
	root := t.TempDir()
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
	os.MkdirAll(filepath.Join(root, "self"), 0o755)
	os.MkdirAll(filepath.Join(root, "net"), 0o755)
	return &fakeProc{t, root}
}

func (f *fakeProc) write(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, name), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// netDev builds /proc/net/dev with eth0 and lo (lo must be ignored).
func netDev(rx, tx uint64) string {
	return "Inter-|   Receive                                                |  Transmit\n" +
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n" +
		"    lo: 999999 10 0 0 0 0 0 0 999999 10 0 0 0 0 0 0\n" +
		"  eth0: " + itoa(rx) + " 100 0 0 0 0 0 0 " + itoa(tx) + " 100 0 0 0 0 0 0\n" +
		"veth12: 555555 10 0 0 0 0 0 0 555555 10 0 0 0 0 0 0\n"
}

func itoa(v uint64) string { return fmtUint(v) }

func fmtUint(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

const tcpHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

func TestSample_RatesCPUMemoryNetworkSockets(t *testing.T) {
	f := newFakeProc(t)
	// 4 cores; second reading adds 1000 jiffies of which 750 idle → 25% busy.
	f.write("stat", "cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 1 0 0 0\ncpu1 1 0 0 0\ncpu2 1 0 0 0\ncpu3 1 0 0 0\n")
	f.write("self/stat", "1234 (origin media) S 1 1 1 0 -1 0 0 0 0 0 100 50 0 0 20 0 10 0 1 1 1\n")
	f.write("self/status", "Name:\torigin\nVmRSS:\t  51200 kB\n")
	f.write("meminfo", "MemTotal:       1000000 kB\nMemFree:  100000 kB\nMemAvailable:    250000 kB\n")
	f.write("uptime", "12345.67 999.0\n")
	f.write("net/dev", netDev(1000, 2000))
	// :1F90 = 8080 LISTEN + 2 established, :078F = 1935 LISTEN + 1 established.
	f.write("net/tcp", tcpHeader+
		"   0: 00000000:1F90 00000000:0000 0A 0 0 0 0 0 0 0\n"+
		"   1: 0100007F:1F90 0100007F:D431 01 0 0 0 0 0 0 0\n"+
		"   2: 0100007F:1F90 0100007F:D432 01 0 0 0 0 0 0 0\n"+
		"   3: 00000000:078F 00000000:0000 0A 0 0 0 0 0 0 0\n"+
		"   4: 0100007F:078F 0100007F:D433 01 0 0 0 0 0 0 0\n"+
		"   5: 0100007F:D434 0A000001:01BB 01 0 0 0 0 0 0 0\n") // outbound, not ours
	f.write("net/tcp6", tcpHeader)
	f.write("net/udp", tcpHeader+"   0: 00000000:0FA0 00000000:0000 07 0 0 0 0 0 0 0\n") // :4000 bound
	f.write("net/udp6", tcpHeader)

	c := New(Config{HTTPListen: "0.0.0.0:8080", RTMPListen: ":1935", SRTListen: ":4000"}, Sources{})
	t0 := time.Now()
	c.sample(t0)
	if h, _ := c.History(time.Hour); len(h) != 0 {
		t.Fatalf("first sample (baseline only) added %d history points", len(h))
	}

	// 5 s later: +1000 total/+750 idle jiffies, process +200 jiffies (2 s
	// CPU over 5 s on 4 cores = 10%), NIC +625000 B in (1 Mbps), +1250000 B out.
	f.write("stat", "cpu  250 0 200 1550 0 0 0 0 0 0\ncpu0 1 0 0 0\ncpu1 1 0 0 0\ncpu2 1 0 0 0\ncpu3 1 0 0 0\n")
	f.write("self/stat", "1234 (origin media) S 1 1 1 0 -1 0 0 0 0 0 250 100 0 0 20 0 10 0 1 1 1\n")
	f.write("net/dev", netDev(1000+625000, 2000+1250000))
	c.sample(t0.Add(5 * time.Second))

	s := c.Snapshot()
	near := func(name string, got *float64, want float64) {
		t.Helper()
		if got == nil || *got < want-0.01 || *got > want+0.01 {
			t.Errorf("%s = %v, want %v", name, deref(got), want)
		}
	}
	near("cpu", s.CPUPct, 25)
	near("process cpu", s.Process.CPUPct, 10)
	near("net in", s.Network.InBps, 1_000_000)
	near("net out", s.Network.OutBps, 2_000_000)
	if s.Memory == nil || s.Memory.Pct != 75 {
		t.Errorf("memory = %+v, want 75%%", s.Memory)
	}
	if s.Host.Cores != 4 || s.Process.RSS == nil || *s.Process.RSS != 51200*1024 {
		t.Errorf("cores %d rss %v", s.Host.Cores, s.Process.RSS)
	}
	if s.Connections == nil || s.Connections.HTTP != 2 || s.Connections.RTMP != 1 {
		t.Errorf("connections = %+v, want http 2 rtmp 1", s.Connections)
	}
	want := map[string]string{"HTTP (HLS/DASH/API)": "up", "RTMP": "up", "SRT": "up"}
	for _, svc := range s.Services {
		if svc.Status != want[svc.Name] {
			t.Errorf("service %s = %s, want %s", svc.Name, svc.Status, want[svc.Name])
		}
	}

	// A NIC counter going backwards (interface reset) gives no rate for
	// that interval, not a huge spike, and the next interval recovers.
	f.write("net/dev", netDev(10, 20))
	c.sample(t0.Add(10 * time.Second))
	if s := c.Snapshot(); s.Network.InBps != nil {
		t.Errorf("rate after counter reset = %v, want nil", *s.Network.InBps)
	}
	f.write("net/dev", netDev(10+62500, 20))
	c.sample(t0.Add(15 * time.Second))
	near("net in after reset", c.Snapshot().Network.InBps, 100_000)

	// RTMP stops listening → down.
	f.write("net/tcp", tcpHeader+"   0: 00000000:1F90 00000000:0000 0A 0 0 0 0 0 0 0\n")
	c.sample(t0.Add(20 * time.Second))
	for _, svc := range c.Snapshot().Services {
		if svc.Name == "RTMP" && svc.Status != "down" {
			t.Errorf("RTMP with no listener = %s, want down", svc.Status)
		}
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// Ingest totals survive a stream ending, and events are counted per
// interval from the registry.
func TestSample_IngestTotalsAndEvents(t *testing.T) {
	newFakeProc(t) // empty: system metrics unavailable, which must not break anything
	reg := media.NewRegistry()
	c := New(Config{}, Sources{Registry: reg})
	t0 := time.Now()
	c.sample(t0)

	st := &media.Stream{Key: media.StreamKey{App: "live", Name: "a"}}
	reg.Register(st)
	st.Bus.AddBytesIn(500_000)
	c.sample(t0.Add(5 * time.Second))
	s := c.Snapshot()
	if s.Totals.BytesIn != 500_000 || s.Network.IngestBps != 800_000 || s.Streams.Active != 1 {
		t.Fatalf("after 500 kB in 5 s: totals %d, ingest %v bps, active %d", s.Totals.BytesIn, s.Network.IngestBps, s.Streams.Active)
	}
	if s.CPUPct != nil || s.Memory != nil || s.Network.InBps != nil {
		t.Error("system metrics reported without /proc data")
	}

	reg.Unregister(st)
	c.sample(t0.Add(10 * time.Second))
	s = c.Snapshot()
	if s.Totals.BytesIn != 500_000 || s.Streams.Active != 0 || s.Network.IngestBps != 0 {
		t.Errorf("after the stream ended: totals %d (want 500000), active %d, ingest %v", s.Totals.BytesIn, s.Streams.Active, s.Network.IngestBps)
	}
	h, step := c.History(5 * time.Minute)
	if step != 5 || len(h) != 2 || h[0].Published != 1 || h[1].Ended != 1 {
		t.Errorf("history step %d, points %+v", step, h)
	}
}

func TestHistory_RingsAndAverages(t *testing.T) {
	c := New(Config{}, Sources{})
	now := time.Now()
	// 13 fine points: 12 fold into one coarse average, one is pending.
	for i := 0; i < 13; i++ {
		v := float64(i)
		c.fine.push(Sample{T: now.Add(time.Duration(i-13) * 5 * time.Second).UnixMilli(), CPUPct: &v, Published: 1})
		c.pending = append(c.pending, Sample{CPUPct: &v, Published: 1, T: now.UnixMilli()})
		if len(c.pending) == 12 {
			c.coarse.push(average(c.pending))
			c.pending = c.pending[:0]
		}
	}
	h, step := c.History(24 * time.Hour)
	if step != 60 || len(h) != 1 || *h[0].CPUPct != 5.5 || h[0].Published != 12 {
		t.Errorf("coarse: step %d %+v", step, h)
	}
	if h, step := c.History(time.Minute); step != 5 || len(h) != 12 {
		t.Errorf("fine 1 min: step %d, %d points, want 12", step, len(h))
	}
	// The fine ring keeps only the last hour.
	r := newRing(3)
	for i := 1; i <= 5; i++ {
		r.push(Sample{T: int64(i)})
	}
	if it := r.items(); len(it) != 3 || it[0].T != 3 || it[2].T != 5 {
		t.Errorf("ring items = %+v", it)
	}
}

func TestPort(t *testing.T) {
	for in, want := range map[string]int{"0.0.0.0:1935": 1935, ":8080": 8080, "[::]:443": 443, "": 0, "bad": 0} {
		if got := port(in); got != want {
			t.Errorf("port(%q) = %d, want %d", in, got, want)
		}
	}
}
