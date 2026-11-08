package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// health runs the checks every 30 seconds and records state changes as events. A check counts as
// failing after two failed rounds (wan: after 5 minutes, since a car passes through tunnels and car
// parks; proxy: after three tests, since a cross-border request over cellular stalls now and then)
// and recovers after one good round. Checks that depend on a failing one are skipped, so an
// alert names the root cause: no DNS, proxy or Tailscale alerts while mihomo is down, and no proxy,
// Tailscale or upstream DNS alerts while the cellular link is down.

const (
	healthEvery = 30 * time.Second
	// right after boot Box is still starting mihomo
	healthBootGrace = 2 * time.Minute
	memLowMB        = 80
	// the wall clock is wrong when it is this far behind the last time it was seen
	clockSlack = time.Hour
	// give up waiting for the clock to be corrected (it may have been set back on purpose)
	clockGiveUp    = 6 * time.Hour
	clockSaveEvery = 5 * time.Minute
	// one proxy delay test; the health check tries twice
	proxyTestMs = 5000
)

// healthChecks is the display order
var healthChecks = []string{"mihomo", "dns", "proxy", "tailscale", "wan", "wifi", "clock", "memory"}

// probeOrder runs the checks that others depend on first
var probeOrder = []string{"wan", "wifi", "mihomo", "memory", "dns", "proxy", "tailscale"}

// healthDeps lists the checks a check is skipped for when they fail in the same round
var healthDeps = map[string][]string{
	"dns":       {"mihomo"},
	"proxy":     {"mihomo", "wan"},
	"tailscale": {"mihomo", "wan"},
}

// failing rounds before a check counts as failing
func failRounds(name string) int {
	switch name {
	case "wan":
		return int(5 * time.Minute / healthEvery)
	case "proxy":
		return 3
	}
	return 2
}

// checks whose failure alerts carry the latest mihomo warnings
var withMihomoLog = map[string]bool{"mihomo": true, "dns": true, "proxy": true, "tailscale": true}

var (
	errOff    = errors.New("not configured")
	errSkip   = errors.New("skipped")     // a dependency is down or the data is unavailable
	errNoData = errors.New("no new data") // nothing observed since the last round
)

func isFail(err error) bool {
	return err != nil && err != errOff && err != errSkip && err != errNoData
}

// probe returns nil when healthy; down reports checks that failed earlier in the same round
type probe func(ctx context.Context, down func(string) bool) error

type checkState struct {
	Name  string    `json:"name"`
	State string    `json:"state"` // ok, fail, skip, off, or empty before the first round
	Msg   string    `json:"msg,omitempty"`
	Since time.Time `json:"since,omitzero"` // start of the current failure, for display
	fails int
	first time.Time // start of the current failure with a monotonic reading, for durations
}

type health struct {
	s         *server
	events    *eventLog
	clockPath string
	probes    map[string]probe

	mu     sync.Mutex
	checks map[string]*checkState
	round  int
	tsAt   time.Time

	lastSeen time.Time // latest trusted wall clock, persisted
	savedAt  time.Time
	clockBad bool
	start    time.Time     // with a monotonic reading, for clockNow
	badWall  time.Time     // wall clock when it was found wrong
	badMono  time.Duration // clockNow's monotonic time at that moment
	badSeq   int64         // first event recorded with the wrong clock

	dnsmasq *dnsmasqWatch // nil in the demo
}

func newHealth(s *server, events *eventLog, dataDir string) *health {
	h := &health{s: s, events: events, checks: map[string]*checkState{}, start: time.Now()}
	for _, n := range healthChecks {
		h.checks[n] = &checkState{Name: n}
	}
	if !s.demo {
		h.dnsmasq = newDNSMasqWatch("/proc")
	}
	if dataDir != "" {
		h.clockPath = filepath.Join(dataDir, "clock")
		if b, err := os.ReadFile(h.clockPath); err == nil {
			h.lastSeen, _ = time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
		}
	}
	h.probes = map[string]probe{
		"wan":       h.probeWAN,
		"wifi":      h.probeWiFi,
		"mihomo":    h.probeMihomo,
		"memory":    h.probeMemory,
		"dns":       h.probeDNS,
		"proxy":     h.probeProxy,
		"tailscale": h.probeTailscale,
	}
	return h
}

func (h *health) loop(ctx context.Context) {
	h.checkClock(h.clockNow())
	up := h.s.sys().Uptime
	if !h.s.demo { // the demo comes with its own history
		h.events.add(event{Check: "started", State: "info", Msg: version, Dur: up})
	}
	if wait := healthBootGrace - time.Duration(up)*time.Second; wait > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
	tk := time.NewTicker(healthEvery)
	defer tk.Stop()
	for {
		h.runRound(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
	}
}

func (h *health) runRound(ctx context.Context) {
	h.checkClock(h.clockNow())
	h.mu.Lock()
	h.round++
	h.mu.Unlock()
	if h.dnsmasq != nil {
		h.dnsmasq.check(time.Now())
	}
	raw := map[string]error{}
	down := func(n string) bool { return isFail(raw[n]) }
	for _, name := range probeOrder {
		p := h.probes[name]
		if p == nil {
			continue
		}
		var err error
		for _, d := range healthDeps[name] {
			if down(d) {
				err = errSkip
			}
		}
		if err == nil {
			err = p(ctx, down)
		}
		raw[name] = err
		h.observe(name, err, time.Now())
	}
}

// observe applies one probe result and records an event when the state changes
func (h *health) observe(name string, err error, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cs := h.checks[name]
	switch {
	case err == errNoData:
	case err == errOff:
		cs.State, cs.Msg, cs.fails = "off", "", 0
	case err == errSkip:
		// a failing check stays failing until it is seen healthy again
		if cs.State != "fail" {
			cs.State, cs.Msg = "skip", ""
		}
		cs.fails = 0
	case err == nil:
		if cs.State == "fail" {
			h.events.add(event{Check: name, State: "ok", Dur: int64(now.Sub(cs.first).Seconds())})
		}
		cs.State, cs.Msg, cs.fails, cs.Since = "ok", "", 0, time.Time{}
	default:
		cs.fails++
		if cs.fails == 1 {
			cs.first = now
		}
		if cs.State == "fail" {
			cs.Msg = err.Error()
			break
		}
		if cs.State == "" {
			cs.State = "ok"
		}
		if cs.fails >= failRounds(name) {
			cs.State, cs.Msg, cs.Since = "fail", err.Error(), cs.first.Round(0)
			e := event{Check: name, State: "fail", Msg: err.Error()}
			if withMihomoLog[name] {
				e.Log = mihomoWarnings(filepath.Join(h.s.cfg.get().BoxDir, "run/clash.log"), 10)
			}
			h.events.add(e)
		}
	}
}

// checkClock tells whether the wall clock is usable. The F50 has no RTC battery: after a power
// cut it boots at an old date until the cellular network provides the time. Events recorded in
// the meantime are restamped once the clock jumps forward.
func (h *health) checkClock(wall time.Time, mono time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cs := h.checks["clock"]
	if h.clockBad {
		if wall.Before(h.lastSeen.Add(-clockSlack)) && mono-h.badMono < clockGiveUp {
			return
		}
		expected := h.badWall.Add(mono - h.badMono)
		skew := wall.Sub(expected).Round(time.Second)
		h.events.restamp(h.badSeq, skew)
		for _, c := range h.checks {
			if !c.Since.IsZero() && !c.Since.Before(h.badWall) {
				c.Since = c.Since.Add(skew)
			}
		}
		h.clockBad = false
		cs.State, cs.Msg, cs.Since = "ok", "", time.Time{}
		h.events.add(event{Check: "clock_fixed", State: "info", Dur: int64(skew.Seconds())})
		h.saveClock(wall)
		return
	}
	if !h.lastSeen.IsZero() && wall.Before(h.lastSeen.Add(-clockSlack)) {
		h.clockBad, h.badWall, h.badMono, h.badSeq = true, wall, mono, h.events.lastSeq()+1
		cs.State, cs.Since = "fail", wall
		cs.Msg = "clock is behind the last known time " + h.lastSeen.In(time.Local).Format("2006-01-02 15:04")
		return
	}
	cs.State = "ok"
	if wall.After(h.lastSeen) && wall.Sub(h.savedAt) >= clockSaveEvery {
		h.saveClock(wall)
	}
}

// clockNow returns the wall clock and the monotonic time since start, which setting the clock
// does not affect
func (h *health) clockNow() (time.Time, time.Duration) {
	return time.Now().Round(0), time.Since(h.start)
}

func (h *health) saveClock(wall time.Time) {
	h.lastSeen, h.savedAt = wall, wall
	if h.clockPath == "" {
		return
	}
	if err := writeFileAtomic(h.clockPath, []byte(wall.UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		log.Printf("health: %v", err)
	}
}

// clockOK is false while timestamps would be wrong; alerts and log shipping wait for it
func (h *health) clockOK() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.clockBad
}

func (h *health) snapshot() []checkState {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]checkState, 0, len(healthChecks))
	for _, n := range healthChecks {
		out = append(out, *h.checks[n])
	}
	return out
}

func (h *health) bad() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []string{}
	for _, n := range healthChecks {
		if h.checks[n].State == "fail" {
			out = append(out, n)
		}
	}
	return out
}

// ---------- probes ----------

func (h *health) probeWAN(ctx context.Context, _ func(string) bool) error {
	z, err := h.s.zteStatus(ctx)
	if err != nil {
		// the vendor service being unreachable says nothing about the link
		return errSkip
	}
	if ppp := str(z["ppp_status"]); !pppConnected(ppp) {
		return fmt.Errorf("cellular data is not connected (%s)", ppp)
	}
	return nil
}

func (h *health) probeWiFi(ctx context.Context, _ func(string) bool) error {
	z, err := h.s.zteStatus(ctx)
	if err != nil {
		return errSkip
	}
	if str(z["wifi_onoff_state"]) != "1" {
		return errors.New("the hotspot is off")
	}
	return nil
}

func (h *health) probeMihomo(ctx context.Context, _ func(string) bool) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := h.s.mihomo.version(ctx)
	return err
}

func (h *health) probeMemory(context.Context, func(string) bool) error {
	m := h.s.sys().Mem
	if m.TotalMB == 0 {
		return errOff
	}
	if m.AvailableMB < memLowMB {
		return fmt.Errorf("%d MB of %d MB available", m.AvailableMB, m.TotalMB)
	}
	return nil
}

// probeDNS asks mihomo's DNS listener, the one hotspot clients are redirected to; with fake-ip that
// is answered from memory, so a random name is also resolved through mihomo's upstream servers
func (h *health) probeDNS(ctx context.Context, down func(string) bool) error {
	c := h.s.cfg.get()
	host := "www.gstatic.com"
	if u, err := url.Parse(c.DelayURL); err == nil && u.Hostname() != "" && net.ParseIP(u.Hostname()) == nil {
		host = u.Hostname()
	}
	if c.DNSProbe != "" {
		r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", c.DNSProbe)
		}}
		lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err := r.LookupHost(lctx, host)
		cancel()
		if err != nil {
			return fmt.Errorf("listener %s: %w", c.DNSProbe, err)
		}
	}
	if down("wan") {
		return nil
	}
	b := make([]byte, 4)
	rand.Read(b)
	qctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	rcode, err := h.s.mihomo.dnsQuery(qctx, "glovebox-"+hex.EncodeToString(b)+"."+host, "A")
	if err != nil {
		return fmt.Errorf("upstream: %w", err)
	}
	// NXDOMAIN is an answer too
	if rcode != 0 && rcode != 3 {
		return fmt.Errorf("upstream: response code %d", rcode)
	}
	return nil
}

// probeProxy measures the main group every other round; each test is a request over cellular data.
// A cross-border request over cellular stalls for a few seconds now and then, so a failed test is
// retried once before the round counts as failed.
func (h *health) probeProxy(ctx context.Context, _ func(string) bool) error {
	h.mu.Lock()
	skip := h.round%2 == 0
	h.mu.Unlock()
	if skip {
		return errNoData
	}
	c := h.s.cfg.get()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ps, err := h.s.mihomo.proxies(ctx)
	if err != nil {
		return err
	}
	g := mainGroup(ps, c.MainGroup)
	if g == "" {
		return errOff
	}
	test := func() error {
		d, err := h.s.mihomo.proxyDelay(ctx, g, c.DelayURL, proxyTestMs)
		if err == nil && d <= 0 {
			err = errors.New("no response")
		}
		return err
	}
	if err = test(); err != nil && ctx.Err() == nil {
		err = test()
	}
	if err != nil {
		return fmt.Errorf("%s: %w", g, err)
	}
	return nil
}

// probeTailscale reads the tsProbe result, which is refreshed every minute
func (h *health) probeTailscale(context.Context, func(string) bool) error {
	if !h.s.cfg.get().tailscaleEnabled() {
		return errOff
	}
	at, online, msg := h.s.ts.last()
	h.mu.Lock()
	seen := at.IsZero() || at.Equal(h.tsAt)
	h.tsAt = at
	h.mu.Unlock()
	if seen {
		return errNoData
	}
	if !online {
		if msg == "" {
			msg = "the tailnet is unreachable"
		}
		return errors.New(msg)
	}
	return nil
}

// mihomoWarnings returns the last n warning and error lines of mihomo's log
func mihomoWarnings(path string, n int) []string {
	text, err := tailFile(path, 32<<10)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, "level=warning") || strings.Contains(l, "level=error") || strings.Contains(l, "level=fatal") {
			if len(l) > 300 {
				l = l[:300] + "..."
			}
			out = append(out, strings.ToValidUTF8(l, ""))
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}
