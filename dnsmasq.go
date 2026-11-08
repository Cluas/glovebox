package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The hotspot's DNS forwarder is the dnsmasq netd starts with tethering. netd turns IPv6 on for a
// tethered interface and sends dnsmasq the interface list on its stdin; dnsmasq binds every
// address the interfaces have at that moment, IPv6 link-local ones included. Box turns IPv6 off
// when it starts (ipv6="false"), the addresses go away and netd destroys the sockets bound to
// them. The destroyed TCP listeners report a hangup on every poll, which dnsmasq never handles,
// so it spins at a full core from then on: after every boot, and after every Box restart that
// follows a tethering restart. Sending it the interface list again, as netd does when tethering
// changes, makes it close those listeners and bind the addresses that exist.

const (
	// share of a core over a health round that counts as spinning
	dnsmasqBusy = 0.5
	// after a fix, wait this long before trying again
	dnsmasqRetry = 10 * time.Minute
	// USER_HZ, the unit of the CPU times in /proc/<pid>/stat
	clockTicks = 100
)

type dnsmasqWatch struct {
	proc    string
	ifaceOf func(net.IP) string

	pid     int
	ticks   uint64
	at      time.Time
	fixedAt time.Time
}

func newDNSMasqWatch(proc string) *dnsmasqWatch {
	return &dnsmasqWatch{proc: proc, ifaceOf: ifaceOf}
}

// check samples dnsmasq's CPU time and makes it rebind its listeners when it spins
func (w *dnsmasqWatch) check(now time.Time) {
	if w.pid == 0 || w.comm(w.pid) != "dnsmasq" {
		w.pid, w.at = w.find(), time.Time{}
		if w.pid == 0 {
			return
		}
	}
	ticks, err := w.cpuTicks(w.pid)
	if err != nil {
		w.pid = 0
		return
	}
	prevTicks, prevAt := w.ticks, w.at
	w.ticks, w.at = ticks, now
	if prevAt.IsZero() || ticks < prevTicks || now.Sub(prevAt) < 10*time.Second {
		return
	}
	busy := float64(ticks-prevTicks) / clockTicks / now.Sub(prevAt).Seconds()
	if busy < dnsmasqBusy || (!w.fixedAt.IsZero() && now.Sub(w.fixedAt) < dnsmasqRetry) {
		return
	}
	w.fixedAt = now
	ifaces := w.ifaces(w.pid)
	if len(ifaces) == 0 {
		log.Printf("dnsmasq: %.0f%% of a core, but it listens on no tethered interface", busy*100)
		return
	}
	cmd := "update_ifaces|" + strings.Join(ifaces, "|")
	if err := w.send(w.pid, cmd); err != nil {
		log.Printf("dnsmasq: %.0f%% of a core, rebind failed: %v", busy*100, err)
		return
	}
	log.Printf("dnsmasq: %.0f%% of a core, sent %q to rebind its listeners", busy*100, cmd)
}

func (w *dnsmasqWatch) comm(pid int) string {
	b, _ := os.ReadFile(filepath.Join(w.proc, strconv.Itoa(pid), "comm"))
	return strings.TrimSpace(string(b))
}

func (w *dnsmasqWatch) find() int {
	des, _ := os.ReadDir(w.proc)
	for _, de := range des {
		if pid, err := strconv.Atoi(de.Name()); err == nil && w.comm(pid) == "dnsmasq" {
			return pid
		}
	}
	return 0
}

// cpuTicks returns utime+stime
func (w *dnsmasqWatch) cpuTicks(pid int) (uint64, error) {
	b, err := os.ReadFile(filepath.Join(w.proc, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	// the command name in parentheses may contain spaces
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(f) < 13 {
		return 0, errors.New("short stat")
	}
	u, err1 := strconv.ParseUint(f[11], 10, 64)
	k, err2 := strconv.ParseUint(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, errors.New("bad stat")
	}
	return u + k, nil
}

// ifaces names the non-loopback interfaces that hold the IPv4 addresses dnsmasq listens on
func (w *dnsmasqWatch) ifaces(pid int) []string {
	fds := filepath.Join(w.proc, strconv.Itoa(pid), "fd")
	des, _ := os.ReadDir(fds)
	mine := map[string]bool{}
	for _, de := range des {
		l, _ := os.Readlink(filepath.Join(fds, de.Name()))
		if ino, ok := strings.CutPrefix(l, "socket:["); ok {
			mine[strings.TrimSuffix(ino, "]")] = true
		}
	}
	var out []string
	for _, table := range []string{"tcp", "udp"} {
		b, _ := os.ReadFile(filepath.Join(w.proc, "net", table))
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) < 10 || !mine[f[9]] {
				continue
			}
			ip := parseProcIPv4(f[1])
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if name := w.ifaceOf(ip); name != "" && !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	slices.Sort(out)
	return out
}

// parseProcIPv4 reads the address of "0100A8C0:0035", which the kernel prints in host order
func parseProcIPv4(s string) net.IP {
	h, _, ok := strings.Cut(s, ":")
	b, err := hex.DecodeString(h)
	if !ok || err != nil || len(b) != 4 {
		return nil
	}
	return net.IPv4(b[3], b[2], b[1], b[0])
}

func ifaceOf(ip net.IP) string {
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
				return i.Name
			}
		}
	}
	return ""
}

// send writes a command to dnsmasq's stdin, the pipe netd keeps for the same purpose
func (w *dnsmasqWatch) send(pid int, cmd string) error {
	in := filepath.Join(w.proc, strconv.Itoa(pid), "fd", "0")
	if l, err := os.Readlink(in); err != nil || !strings.HasPrefix(filepath.Base(l), "pipe:") {
		return fmt.Errorf("stdin is not a pipe (%s)", l)
	}
	f, err := os.OpenFile(in, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append([]byte(cmd), 0))
	return err
}
