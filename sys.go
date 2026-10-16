package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readInt(path string) (int64, bool) {
	v, err := strconv.ParseInt(readTrim(path), 10, 64)
	return v, err == nil
}

func run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

func getprop(key string) string {
	out, err := run(context.Background(), 3*time.Second, "getprop", key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

type battery struct {
	Present  bool    `json:"present"`
	Capacity int64   `json:"capacity"`
	Status   string  `json:"status"`
	TempC    float64 `json:"temp_c"`
	VoltageV float64 `json:"voltage_v"`
	External bool    `json:"external"` // on USB / vehicle power
}

func readBattery() battery {
	const d = "/sys/class/power_supply/"
	var b battery
	if c, ok := readInt(d + "battery/capacity"); ok {
		b.Present = true
		b.Capacity = c
	}
	b.Status = readTrim(d + "battery/status")
	if t, ok := readInt(d + "battery/temp"); ok {
		b.TempC = float64(t) / 10
	}
	if v, ok := readInt(d + "battery/voltage_now"); ok {
		b.VoltageV = float64(v) / 1e6
	}
	for _, s := range []string{"usb", "ac"} {
		if v, ok := readInt(d + s + "/online"); ok && v == 1 {
			b.External = true
		}
	}
	return b
}

type thermal struct {
	Name  string  `json:"name"`
	TempC float64 `json:"temp_c"`
}

func readThermals() []thermal {
	zones, _ := filepath.Glob("/sys/class/thermal/thermal_zone*")
	var out []thermal
	for _, z := range zones {
		t, ok := readInt(z + "/temp")
		if !ok || t <= 0 {
			continue
		}
		c := float64(t)
		if c > 1000 {
			c /= 1000
		}
		out = append(out, thermal{Name: readTrim(z + "/type"), TempC: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TempC > out[j].TempC })
	return out
}

type memInfo struct {
	TotalMB     int64 `json:"total_mb"`
	AvailableMB int64 `json:"available_mb"`
}

func readMem() memInfo {
	var m memInfo
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		n, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		switch k {
		case "MemTotal":
			m.TotalMB = n / 1024
		case "MemAvailable":
			m.AvailableMB = n / 1024
		}
	}
	return m
}

func readUptime() int64 {
	s := readTrim("/proc/uptime")
	f, _, _ := strings.Cut(s, " ")
	v, _ := strconv.ParseFloat(f, 64)
	return int64(v)
}

type sysInfo struct {
	Battery  battery
	Thermals []thermal
	Mem      memInfo
	Uptime   int64
	Load     string
}

func readSysInfo() sysInfo {
	return sysInfo{readBattery(), readThermals(), readMem(), readUptime(), readLoad()}
}

func readLoad() string {
	f := strings.Fields(readTrim("/proc/loadavg"))
	if len(f) < 3 {
		return ""
	}
	return strings.Join(f[:3], " ")
}

// netSampler reads /proc/net/dev and /proc/stat every second to compute WAN (sum of cellular
// interfaces) and LAN (br0) rates plus CPU usage, keeping historyLen seconds of history for charts
type netSampler struct {
	wanPrefixes []string
	lanDev      string
	synth       bool // demo mode: synthetic data
	synthT      float64
	burst       int

	mu      sync.Mutex
	cur     netRates
	prev    map[string][2]uint64
	at      time.Time
	cpuPrev [2]uint64 // total, idle
	hist    []sample  // ring buffer
	next    int
}

const historyLen = 180

type netRates struct {
	WANUp   float64  `json:"wan_up"` // bytes per second
	WANDown float64  `json:"wan_down"`
	LANUp   float64  `json:"lan_up"` // from the clients' point of view: upload = received on br0
	LANDown float64  `json:"lan_down"`
	CPU     float64  `json:"cpu"`     // 0～100
	WANIfs  []string `json:"wan_ifs"` // cellular interfaces that are up and have counters
}

type sample struct {
	wanDown, wanUp, lanDown, lanUp, cpu float64
}

// history holds per-metric arrays in time order (oldest first); rates are rounded to bytes/s
type history struct {
	WANDown []int64   `json:"wan_down"`
	WANUp   []int64   `json:"wan_up"`
	LANDown []int64   `json:"lan_down"`
	LANUp   []int64   `json:"lan_up"`
	CPU     []float64 `json:"cpu"`
}

func readNetDev() map[string][2]uint64 {
	out := map[string][2]uint64{}
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fs := strings.Fields(rest)
		if len(fs) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(fs[0], 10, 64)
		tx, _ := strconv.ParseUint(fs[8], 10, 64)
		out[strings.TrimSpace(name)] = [2]uint64{rx, tx}
	}
	return out
}

// readCPU returns total and idle (idle + iowait) jiffies from the first line of /proc/stat
func readCPU() (total, idle uint64, ok bool) {
	s := readTrim("/proc/stat")
	line, _, _ := strings.Cut(s, "\n")
	fs := strings.Fields(line)
	if len(fs) < 5 || fs[0] != "cpu" {
		return 0, 0, false
	}
	for i, f := range fs[1:] {
		v, _ := strconv.ParseUint(f, 10, 64)
		total += v
		if i == 3 || i == 4 {
			idle += v
		}
	}
	return total, idle, true
}

func (n *netSampler) isWAN(name string) bool {
	for _, p := range n.wanPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (n *netSampler) loop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		n.sample()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (n *netSampler) sample() {
	if n.synth {
		n.sampleSynth()
		return
	}
	now := time.Now()
	cur := readNetDev()
	total, idle, cpuOK := readCPU()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.prev != nil {
		dt := now.Sub(n.at).Seconds()
		var r netRates
		for name, v := range cur {
			p, ok := n.prev[name]
			if !ok || v[0] < p[0] || v[1] < p[1] {
				continue
			}
			rx, tx := float64(v[0]-p[0])/dt, float64(v[1]-p[1])/dt
			switch {
			case n.isWAN(name):
				r.WANDown += rx
				r.WANUp += tx
				if readTrim("/sys/class/net/"+name+"/operstate") != "down" {
					r.WANIfs = append(r.WANIfs, name)
				}
			case name == n.lanDev:
				r.LANUp += rx
				r.LANDown += tx
			}
		}
		if cpuOK && total > n.cpuPrev[0] {
			dTotal, dIdle := total-n.cpuPrev[0], idle-n.cpuPrev[1]
			r.CPU = 100 * float64(dTotal-min(dIdle, dTotal)) / float64(dTotal)
		}
		sort.Strings(r.WANIfs)
		n.cur = r
		n.push(sample{r.WANDown, r.WANUp, r.LANDown, r.LANUp, r.CPU})
	}
	if cpuOK {
		n.cpuPrev = [2]uint64{total, idle}
	}
	n.prev, n.at = cur, now
}

func (n *netSampler) push(s sample) {
	if len(n.hist) < historyLen {
		n.hist = append(n.hist, s)
	} else {
		n.hist[n.next] = s
	}
	n.next = (n.next + 1) % historyLen
}

func (n *netSampler) rates() netRates {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cur
}

func (n *netSampler) history() history {
	n.mu.Lock()
	defer n.mu.Unlock()
	var h history
	l := len(n.hist)
	for i := 0; i < l; i++ {
		s := n.hist[i]
		if l == historyLen {
			s = n.hist[(n.next+i)%historyLen]
		}
		h.WANDown = append(h.WANDown, int64(s.wanDown))
		h.WANUp = append(h.WANUp, int64(s.wanUp))
		h.LANDown = append(h.LANDown, int64(s.lanDown))
		h.LANUp = append(h.LANUp, int64(s.lanUp))
		h.CPU = append(h.CPU, float64(int(s.cpu*10))/10)
	}
	return h
}

// ensureAlias adds the console address to br0; the vendor stack drops it when it rebuilds the hotspot, so re-check periodically
func ensureAlias(ctx context.Context, addr, dev string) {
	if addr == "" || dev == "" {
		return
	}
	ip, _, _ := strings.Cut(addr, "/")
	check := func() {
		out, err := run(ctx, 5*time.Second, "ip", "-4", "addr", "show", "dev", dev)
		if err != nil {
			return // br0 is not up yet
		}
		if strings.Contains(out, " "+ip+"/") {
			return
		}
		if out, err := run(ctx, 5*time.Second, "ip", "addr", "add", addr, "dev", dev); err != nil {
			log.Printf("ip addr add %s dev %s: %v %s", addr, dev, err, strings.TrimSpace(out))
		} else {
			log.Printf("added %s to %s", addr, dev)
		}
	}
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		check()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// tailFile reads the last maxBytes of a file, starting at the first complete line
func tailFile(path string, maxBytes int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	off := st.Size() - maxBytes
	if off < 0 {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "", err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	if off > 0 {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return string(b), nil
}

// startDetached starts a process in its own session so it outlives the request (e.g. restarting Box)
func startDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
