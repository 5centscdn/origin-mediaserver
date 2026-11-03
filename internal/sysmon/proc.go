package sysmon

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procRoot is where /proc is read from; tests point it at fixtures.
var procRoot = "/proc"

func procPath(p string) string { return filepath.Join(procRoot, p) }

// clkTck is USER_HZ, the unit of /proc/*/stat CPU times. It's 100 on every
// mainstream Linux build (x86, arm64); reading it properly needs sysconf
// (cgo), which this project avoids.
const clkTck = 100

// cpuTimes is the first "cpu" line of /proc/stat: total and idle jiffies
// across all cores.
type cpuTimes struct{ total, idle uint64 }

func readCPUTimes() (cpuTimes, int, bool) {
	b, err := os.ReadFile(procPath("stat"))
	if err != nil {
		return cpuTimes{}, 0, false
	}
	var t cpuTimes
	cores := 0
	ok := false
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if f[0] == "cpu" && len(f) >= 5 {
			// user nice system idle iowait irq softirq steal (guest is
			// already included in user/nice, so it's not added again).
			for i, v := range f[1:] {
				if i >= 8 {
					break
				}
				n, _ := strconv.ParseUint(v, 10, 64)
				t.total += n
				if i == 3 || i == 4 { // idle + iowait
					t.idle += n
				}
			}
			ok = true
		} else if strings.HasPrefix(f[0], "cpu") {
			cores++
		}
	}
	return t, cores, ok
}

// readProcCPU returns this process's user+system CPU time in jiffies.
func readProcCPU() (uint64, bool) {
	b, err := os.ReadFile(procPath("self/stat"))
	if err != nil {
		return 0, false
	}
	// The command name (field 2) is in parentheses and may contain spaces;
	// fields are counted after the last ')'.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(string(b[i+1:]))
	// After ')': state(3) ppid(4) ... utime is field 14, stime 15 → indexes 11, 12.
	if len(f) < 13 {
		return 0, false
	}
	u, err1 := strconv.ParseUint(f[11], 10, 64)
	s, err2 := strconv.ParseUint(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return u + s, true
}

// readKV parses "Key: 123 kB" lines (meminfo, status).
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

// memInfo is system memory in bytes; avail counts reclaimable cache as free.
type memInfo struct{ total, avail int64 }

func readMemInfo() (memInfo, bool) {
	m := readKV(procPath("meminfo"))
	if m["MemTotal"] <= 0 {
		return memInfo{}, false
	}
	return memInfo{total: m["MemTotal"] * 1024, avail: m["MemAvailable"] * 1024}, true
}

// readRSS returns this process's resident set size in bytes.
func readRSS() (int64, bool) {
	m := readKV(procPath("self/status"))
	v, ok := m["VmRSS"]
	return v * 1024, ok
}

// netCounters sums received/transmitted bytes over the host's external
// interfaces. Loopback and virtual bridge/veth devices are left out: they
// either aren't network traffic or double-count a container's packets.
func readNetCounters() (rx, tx uint64, ok bool) {
	b, err := os.ReadFile(procPath("net/dev"))
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		if skipIface(name) {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(f[0], 10, 64)
		t, _ := strconv.ParseUint(f[8], 10, 64)
		rx += r
		tx += t
		ok = true
	}
	return rx, tx, ok
}

func skipIface(name string) bool {
	if name == "lo" {
		return true
	}
	for _, p := range []string{"veth", "docker", "br-", "virbr", "cni", "flannel", "cali", "vxlan"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// sockTable counts TCP sockets per local port from /proc/net/tcp{,6}:
// established connections and whether something is listening.
type sockTable struct {
	established map[int]int
	listening   map[int]bool
	udpBound    map[int]bool
}

func readSockets() (sockTable, bool) {
	t := sockTable{established: map[int]int{}, listening: map[int]bool{}, udpBound: map[int]bool{}}
	any := false
	for _, f := range []string{"net/tcp", "net/tcp6"} {
		if scanSockets(procPath(f), func(port int, state string) {
			switch state {
			case "01": // ESTABLISHED
				t.established[port]++
			case "0A": // LISTEN
				t.listening[port] = true
			}
		}) {
			any = true
		}
	}
	for _, f := range []string{"net/udp", "net/udp6"} {
		scanSockets(procPath(f), func(port int, state string) { t.udpBound[port] = true })
	}
	return t, any
}

// scanSockets calls fn(localPort, state) for each row of a /proc/net socket
// table. Rows look like "0: 0100007F:1F90 00000000:0000 0A ...".
func scanSockets(path string, fn func(port int, state string)) bool {
	fh, err := os.Open(path)
	if err != nil {
		return false
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Scan() // header
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 {
			continue
		}
		_, portHex, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		port, err := strconv.ParseUint(portHex, 16, 32)
		if err != nil {
			continue
		}
		fn(int(port), f[3])
	}
	return true
}

// readUptime returns the host's uptime in seconds.
func readUptime() (float64, bool) {
	b, err := os.ReadFile(procPath("uptime"))
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	return v, err == nil
}
