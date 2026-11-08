package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeDNSMasq lays out the parts of /proc the watch reads, with a regular file standing in for
// the stdin pipe
func fakeDNSMasq(t *testing.T) (proc, stdin string, setTicks func(int)) {
	t.Helper()
	proc = t.TempDir()
	w := func(name, s string) {
		p := filepath.Join(proc, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w("1/comm", "init\n")
	w("1/stat", "1 (init) S 0 1 1 0 -1 0 0 0 0 0 5 5 0 0")
	w("2315/comm", "dnsmasq\n")
	setTicks = func(n int) {
		w("2315/stat", fmt.Sprintf("2315 (dns masq) R 601 2315 0 0 -1 0 0 0 0 0 %d %d 0 0 20 0 1 0", n/10, n-n/10))
	}
	setTicks(1000)
	fd := filepath.Join(proc, "2315/fd")
	os.MkdirAll(fd, 0o755)
	stdin = filepath.Join(t.TempDir(), "pipe:[65275]")
	os.WriteFile(stdin, nil, 0o644)
	os.Symlink(stdin, filepath.Join(fd, "0"))
	for n, ino := range map[string]string{"11": "41715", "12": "41716", "13": "41717", "16": "68413", "4": "61157"} {
		os.Symlink("socket:["+ino+"]", filepath.Join(fd, n))
	}
	// listeners on 192.168.0.1 (br0), 127.0.0.1 and, with another owner, 0.0.0.0:67
	w("net/tcp", "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"+
		"   1: 0100007F:0035 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1052        0 41717 1 0 100 0 0 10 0\n"+
		"   2: 0100A8C0:0035 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1052        0 41715 1 0 100 0 0 10 0\n")
	w("net/udp", "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode ref pointer drops\n"+
		" 936: 0100A8C0:0035 00000000:0000 07 00000000:00000000 00:00000000 00000000  1052        0 41716 2 0 0\n"+
		" 950: 00000000:0043 00000000:0000 07 00000000:00000000 00:00000000 00000000  1000        0 65265 2 0 0\n")
	return proc, stdin, setTicks
}

func TestDNSMasqRebind(t *testing.T) {
	proc, stdin, setTicks := fakeDNSMasq(t)
	w := newDNSMasqWatch(proc)
	w.ifaceOf = func(ip net.IP) string {
		if ip.Equal(net.IPv4(192, 168, 0, 1)) {
			return "br0"
		}
		return ""
	}
	sent := func() string {
		b, _ := os.ReadFile(stdin)
		os.WriteFile(stdin, nil, 0o644)
		return string(b)
	}
	t0 := time.Date(2026, 11, 9, 2, 0, 0, 0, time.UTC)
	now, ticks := t0, 1000
	// run advances the clock and adds the CPU time dnsmasq used meanwhile
	run := func(d time.Duration, share float64) string {
		now, ticks = now.Add(d), ticks+int(d.Seconds()*clockTicks*share)
		setTicks(ticks)
		w.check(now)
		return sent()
	}
	w.check(now)
	if w.pid != 2315 {
		t.Fatalf("pid %d", w.pid)
	}
	if got := run(30*time.Second, 0.01); got != "" {
		t.Fatalf("idle dnsmasq got %q", got)
	}
	if got := run(30*time.Second, 0.93); got != "update_ifaces|br0\x00" {
		t.Fatalf("sent %q", got)
	}
	fixed := now
	// still spinning: no second try within dnsmasqRetry
	for now.Add(30*time.Second).Sub(fixed) < dnsmasqRetry {
		if got := run(30*time.Second, 0.93); got != "" {
			t.Fatalf("retried after %v: %q", now.Sub(fixed), got)
		}
	}
	if got := run(30*time.Second, 0.93); got != "update_ifaces|br0\x00" {
		t.Fatalf("no retry: %q", got)
	}
	// a new dnsmasq (tethering restarted) starts a new sample
	os.WriteFile(filepath.Join(proc, "2315/comm"), []byte("other\n"), 0o644)
	w.check(t0.Add(time.Hour))
	if w.pid != 0 || w.at != (time.Time{}) {
		t.Fatalf("stale pid %d", w.pid)
	}
}

func TestDNSMasqSendNeedsPipe(t *testing.T) {
	proc, _, _ := fakeDNSMasq(t)
	w := newDNSMasqWatch(proc)
	in := filepath.Join(proc, "2315/fd/0")
	os.Remove(in)
	os.Symlink("/dev/null", in)
	if err := w.send(2315, "update_ifaces|br0"); err == nil {
		t.Fatal("wrote to a stdin that is not a pipe")
	}
}

func TestParseProcIPv4(t *testing.T) {
	for in, want := range map[string]string{"0100A8C0:0035": "192.168.0.1", "FE00A8C0:0035": "192.168.0.254", "00000000:0043": "0.0.0.0"} {
		if got := parseProcIPv4(in); got.String() != want {
			t.Errorf("%s: %v", in, got)
		}
	}
	for _, bad := range []string{"", "0100A8C0", "zz00A8C0:0035", "00000000000000000000000001000000:0035"} {
		if got := parseProcIPv4(bad); got != nil {
			t.Errorf("%q: %v", bad, got)
		}
	}
}
