package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestHealth returns a health whose probes report the errors in res (nil = healthy)
func newTestHealth(t *testing.T, dataDir string) (*health, map[string]error) {
	t.Helper()
	c := defaultConfig()
	c.BoxDir = t.TempDir()
	s := &server{cfg: &configStore{c: c}, sys: func() sysInfo { return sysInfo{Uptime: 3600} }}
	evPath := ""
	if dataDir != "" {
		evPath = filepath.Join(dataDir, "events.jsonl")
	}
	s.events = openEventLog(evPath)
	h := newHealth(s, s.events, dataDir)
	h.dnsmasq = nil
	res := map[string]error{}
	for _, n := range probeOrder {
		h.probes[n] = func(context.Context, func(string) bool) error { return res[n] }
	}
	return h, res
}

func eventKeys(evs []event) []string {
	var out []string
	for i := len(evs) - 1; i >= 0; i-- {
		out = append(out, evs[i].Check+":"+evs[i].State)
	}
	return out
}

func TestHealthDebounceAndDependencies(t *testing.T) {
	h, res := newTestHealth(t, "")
	ctx := context.Background()
	down := errors.New("connection refused")

	res["mihomo"] = down
	res["dns"] = errors.New("must be skipped")
	h.runRound(ctx)
	if evs := h.events.recent(10); len(evs) != 0 {
		t.Fatalf("a single failure raised %v", eventKeys(evs))
	}
	h.runRound(ctx)
	if got := eventKeys(h.events.recent(10)); strings.Join(got, ",") != "mihomo:fail" {
		t.Fatalf("after two failures: %v", got)
	}
	if st := h.checks["dns"].State; st != "skip" {
		t.Fatalf("dns while mihomo is down: %q, want skip", st)
	}
	if b := h.bad(); len(b) != 1 || b[0] != "mihomo" {
		t.Fatalf("bad = %v", b)
	}

	// mihomo is back but its DNS is not: one failure is not enough
	res["mihomo"] = nil
	h.runRound(ctx)
	h.runRound(ctx)
	got := eventKeys(h.events.recent(10))
	if strings.Join(got, ",") != "mihomo:fail,mihomo:ok,dns:fail" {
		t.Fatalf("events: %v", got)
	}
	if e := h.events.recent(1)[0]; e.Msg != "must be skipped" {
		t.Fatalf("dns failure message %q", e.Msg)
	}
	res["dns"] = nil
	h.runRound(ctx)
	if e := h.events.recent(1)[0]; e.Check != "dns" || e.State != "ok" {
		t.Fatalf("dns did not recover: %+v", e)
	}

	// while the cellular link is down the proxy is not checked; short outages are not reported
	res["wan"] = errors.New("cellular data is not connected (ppp_disconnected)")
	res["proxy"] = errors.New("must be skipped")
	before := h.events.lastSeq()
	for range failRounds("wan") - 1 {
		h.runRound(ctx)
	}
	if h.events.lastSeq() != before {
		t.Fatalf("short wan outage raised %v", eventKeys(h.events.since(before)))
	}
	h.runRound(ctx)
	if got := eventKeys(h.events.since(before)); strings.Join(got, ",") != "wan:fail" {
		t.Fatalf("long wan outage: %v", got)
	}
}

func TestHealthDurationsAndLog(t *testing.T) {
	h, _ := newTestHealth(t, "")
	box := h.s.cfg.get().BoxDir
	os.MkdirAll(filepath.Join(box, "run"), 0o700)
	os.WriteFile(filepath.Join(box, "run/clash.log"), []byte(
		"time=\"x\" level=info msg=\"noise\"\n"+
			"time=\"x\" level=warning msg=\"[TCP] dial Proxy error: i/o timeout\"\n"+
			"time=\"x\" level=error msg=\"fatal-ish\"\n"), 0o600)
	t0 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	fail := errors.New("Timeout")
	for i := range failRounds("proxy") {
		h.observe("proxy", fail, t0.Add(time.Duration(i)*time.Minute))
	}
	e := h.events.recent(1)[0]
	if e.State != "fail" || len(e.Log) != 2 || !strings.Contains(e.Log[0], "i/o timeout") {
		t.Fatalf("failure event: %+v", e)
	}
	if st := h.snapshot()[2]; st.Name != "proxy" || st.State != "fail" || !st.Since.Equal(t0) {
		t.Fatalf("proxy state: %+v", st)
	}
	h.observe("proxy", nil, t0.Add(4*time.Minute))
	if e := h.events.recent(1)[0]; e.State != "ok" || e.Dur != 240 {
		t.Fatalf("recovery event: %+v", e)
	}
	// probes without new data or configuration change nothing
	h.observe("tailscale", errOff, t0)
	h.observe("proxy", errNoData, t0)
	if s := h.snapshot(); s[3].State != "off" || s[2].State != "ok" {
		t.Fatalf("states: %+v", s)
	}
}

func TestHealthClock(t *testing.T) {
	dir := t.TempDir()
	h, _ := newTestHealth(t, dir)
	real := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	h.checkClock(real, 0)
	if !h.clockOK() {
		t.Fatal("first run must trust the clock")
	}

	// power cut: the device boots at the firmware date, an hour of driving later the network
	// sets the time
	h2, _ := newTestHealth(t, dir)
	if !h2.lastSeen.Equal(real) {
		t.Fatalf("persisted clock %v, want %v", h2.lastSeen, real)
	}
	boot := time.Date(2025, 1, 20, 0, 0, 5, 0, time.UTC)
	h2.checkClock(boot, 0)
	if h2.clockOK() || h2.checks["clock"].State != "fail" {
		t.Fatal("old clock not detected")
	}
	e := h2.events.add(event{Time: boot.Add(10 * time.Minute), Check: "dns", State: "fail"})
	h2.checkClock(boot.Add(30*time.Minute), 30*time.Minute)
	if h2.clockOK() {
		t.Fatal("still wrong clock accepted")
	}
	fixed := real.Add(26 * time.Hour)
	h2.checkClock(fixed, time.Hour)
	if !h2.clockOK() {
		t.Fatal("corrected clock not accepted")
	}
	evs := h2.events.since(e.Seq - 1)
	if len(evs) != 2 || evs[1].Check != "clock_fixed" {
		t.Fatalf("events: %+v", evs)
	}
	// the dns failure happened 10 minutes after boot, i.e. 50 minutes before the correction
	if want := fixed.Add(-50 * time.Minute); !evs[0].Time.Equal(want) {
		t.Fatalf("restamped to %v, want %v", evs[0].Time, want)
	}
	if skew := fixed.Sub(boot.Add(time.Hour)); evs[1].Dur != int64(skew.Seconds()) {
		t.Fatalf("clock_fixed dur %d, want %v", evs[1].Dur, skew)
	}
	// the correction is persisted with the events
	again := openEventLog(filepath.Join(dir, "events.jsonl"))
	if got := again.since(e.Seq - 1); !got[0].Time.Equal(evs[0].Time) {
		t.Fatalf("restamp not persisted: %v", got[0].Time)
	}
}

func TestEventLogPersistence(t *testing.T) {
	p := filepath.Join(t.TempDir(), "events.jsonl")
	l := openEventLog(p)
	for range 3 {
		l.add(event{Check: "dns", State: "fail", Msg: "x"})
	}
	// a power cut leaves NUL bytes after the last line
	f, _ := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	f.Write(make([]byte, 64))
	f.Close()
	l = openEventLog(p)
	if l.lastSeq() != 3 || len(l.recent(10)) != 3 {
		t.Fatalf("reload: seq %d, %d events", l.lastSeq(), len(l.recent(10)))
	}
	l.add(event{Check: "dns", State: "ok"})
	if l2 := openEventLog(p); l2.lastSeq() != 4 {
		t.Fatalf("event after a power cut lost: seq %d", l2.lastSeq())
	}

	// the file is compacted to the events kept in memory
	big := strings.Repeat("e", 2000)
	for range 150 {
		l.add(event{Check: "proxy", State: "fail", Msg: big})
	}
	if fi, _ := os.Stat(p); fi.Size() > eventFileMax {
		t.Fatalf("events.jsonl is %d bytes", fi.Size())
	}
	l2 := openEventLog(p)
	if l2.lastSeq() != 154 || len(l2.since(0)) > eventKeep {
		t.Fatalf("after compaction: seq %d, %d events", l2.lastSeq(), len(l2.since(0)))
	}
	if r := l2.recent(2); r[0].Seq != 154 || r[1].Seq != 153 {
		t.Fatalf("recent order: %d %d", r[0].Seq, r[1].Seq)
	}
}

// a cross-border test over cellular stalls now and then: one timeout is retried at once
func TestProbeProxyRetries(t *testing.T) {
	var delays []string
	mh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/proxies":
			w.Write([]byte(`{"proxies":{"Proxy":{"name":"Proxy","type":"Selector","now":"a"}}}`))
		case "/proxies/Proxy/delay":
			if len(delays) == 0 {
				t.Fatal("empty script")
			}
			d := delays[0]
			delays = delays[1:]
			if d == "" {
				w.WriteHeader(http.StatusGatewayTimeout)
				w.Write([]byte(`{"message":"Timeout"}`))
				return
			}
			w.Write([]byte(`{"delay":` + d + `}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(mh.Close)
	h, _ := newTestHealth(t, "")
	h.s.mihomo = newMihomo(mh.URL, "")
	h.s.cfg.c.MainGroup = "Proxy"
	h.round = 1
	for _, tc := range []struct {
		script []string
		want   string
	}{
		{[]string{"120"}, ""},
		{[]string{"", "130"}, ""},
		{[]string{"0", "140"}, ""},
		{[]string{"", ""}, "Proxy: Timeout"},
		{[]string{"", "0"}, "Proxy: no response"},
	} {
		delays = tc.script
		err := h.probeProxy(context.Background(), nil)
		if got := fmt.Sprint(err); (tc.want == "" && err != nil) || (tc.want != "" && got != tc.want) || len(delays) != 0 {
			t.Errorf("%q: %v, %d tests left", tc.script, err, len(delays))
		}
	}
}
