package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func testDiagKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func TestDiagJobVerify(t *testing.T) {
	priv := testDiagKey(t)
	keys := []ed25519.PublicKey{priv.Public().(ed25519.PublicKey)}
	now := time.Date(2026, 10, 20, 17, 0, 0, 0, time.Local)
	probes := []diagProbe{{Kind: "dns", Name: "accounts.google.com"}, {Kind: "http", URL: "https://accounts.google.com/", Follow: true}}
	good, err := signDiag(priv, "job-1", "car", time.Hour, probes, now)
	if err != nil {
		t.Fatal(err)
	}
	// a round trip through JSON, as the device reads it, keeps the signature valid
	b, _ := json.Marshal(good)
	var read diagJob
	json.Unmarshal(b, &read)
	if err := read.verify(keys, "car", now); err != nil {
		t.Fatalf("good job: %v", err)
	}

	resign := func(j diagJob) diagJob {
		j.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, j.signedText()))
		return j
	}
	tampered := good
	tampered.Probes = []diagProbe{{Kind: "dns", Name: "example.com"}}
	other := testDiagKey(t)
	foreign := good
	foreign.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(other, good.signedText()))
	// an upgrade manifest signature must not pass as a job signature
	crossed := good
	crossed.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, append([]byte(updateSigContext+"\n"), good.signedText()[len(diagSigContext)+1:]...)))
	many := good
	many.Probes = make([]diagProbe, diagProbesMax+1)
	for i := range many.Probes {
		many.Probes[i] = diagProbe{Kind: "health"}
	}
	with := func(f func(*diagJob)) diagJob {
		j := good
		j.Probes = append([]diagProbe(nil), good.Probes...)
		f(&j)
		return resign(j)
	}
	for name, tc := range map[string]struct {
		j    diagJob
		want string
	}{
		"tampered":        {tampered, "signature"},
		"foreign key":     {foreign, "signature"},
		"update sig":      {crossed, "signature"},
		"no sig":          {with(func(j *diagJob) {}), ""},
		"other device":    {with(func(j *diagJob) { j.Device = "home" }), `for "home"`},
		"expired":         {with(func(j *diagJob) { j.NotAfter = now.Add(-time.Second).Format(time.RFC3339) }), "expired"},
		"too far ahead":   {with(func(j *diagJob) { j.NotAfter = now.Add(25 * time.Hour).Format(time.RFC3339) }), "ahead"},
		"bad time":        {with(func(j *diagJob) { j.NotAfter = "tomorrow" }), "not_after"},
		"bad id":          {with(func(j *diagJob) { j.ID = "../x" }), "bad id"},
		"too many":        {resign(many), "probes"},
		"none":            {with(func(j *diagJob) { j.Probes = nil }), "probes"},
		"unknown kind":    {with(func(j *diagJob) { j.Probes[0] = diagProbe{Kind: "exec"} }), "unknown kind"},
		"loopback":        {with(func(j *diagJob) { j.Probes[1].URL = "http://127.0.0.1:9091/configs" }), "off limits"},
		"localhost":       {with(func(j *diagJob) { j.Probes[1].URL = "http://localhost/" }), "off limits"},
		"link-local":      {with(func(j *diagJob) { j.Probes[1].URL = "http://[fe80::1]/" }), "off limits"},
		"not http":        {with(func(j *diagJob) { j.Probes[1].URL = "file:///etc/passwd" }), "http(s)"},
		"dns type":        {with(func(j *diagJob) { j.Probes[0].Type = "TXT" }), "type"},
		"log level":       {with(func(j *diagJob) { j.Probes[0] = diagProbe{Kind: "log", Level: "silent"} }), "level"},
		"log too long":    {with(func(j *diagJob) { j.Probes[0] = diagProbe{Kind: "log", Level: "debug", Minutes: 31} }), "minutes"},
		"delay, no proxy": {with(func(j *diagJob) { j.Probes[0] = diagProbe{Kind: "delay"} }), "proxy"},
	} {
		if name == "no sig" {
			tc.j.Sig = ""
			tc.want = "signature"
		}
		err := tc.j.verify(keys, "car", now)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error with %q", name, err, tc.want)
		}
	}
}

func TestDiagURL(t *testing.T) {
	for _, tc := range []struct{ diag, update, device, want string }{
		{"auto", "http://192.168.1.10:8088/update.json", "car", "http://192.168.1.10:8088/diag/car.json"},
		{"auto", "https://example.com/glovebox/update.json", "a b", "https://example.com/glovebox/diag/a%20b.json"},
		{"auto", "http://h/update.json", "../x/y", "http://h/diag/..%2Fx%2Fy.json"},
		{"auto", defaultUpdateURL, "car", ""},
		{"auto", "https://raw.githubusercontent.com/x/y/main/update.json", "car", ""},
		{"auto", "", "car", ""},
		{"https://example.com/job.json", defaultUpdateURL, "car", "https://example.com/job.json"},
		{"", "http://192.168.1.10:8088/update.json", "car", ""},
	} {
		c := defaultConfig()
		c.DiagURL, c.UpdateURL, c.DeviceName = tc.diag, tc.update, tc.device
		if got := diagURL(c); got != tc.want {
			t.Errorf("diag_url %q, update_url %q: %q, want %q", tc.diag, tc.update, got, tc.want)
		}
	}
}

func TestParseProbes(t *testing.T) {
	for _, in := range []string{`[{"kind":"health"}]`, `{"probes":[{"kind":"health"}]}`} {
		ps, err := parseProbes([]byte(in))
		if err != nil || len(ps) != 1 || ps[0].Kind != "health" {
			t.Errorf("%s: %v %v", in, ps, err)
		}
	}
	if _, err := parseProbes([]byte(`[{"kind":"http","url":"https://x/","folow":true}]`)); err == nil {
		t.Error("a misspelled field was accepted")
	}
}

// diagEnv is a diagnoser wired to the demo's fake mihomo, with its mixed port, and a job server
type diagEnv struct {
	t       *testing.T
	d       *diagnoser
	mh      *fakeMihomo
	cfg     *configStore
	priv    ed25519.PrivateKey
	dataDir string
	shipped int

	mu       sync.Mutex
	job      []byte
	jobAt    time.Time
	requests int
	ims      string // If-Modified-Since of the last request
}

func newDiagEnv(t *testing.T) *diagEnv {
	t.Helper()
	e := &diagEnv{t: t, mh: newFakeMihomo(), priv: testDiagKey(t), dataDir: t.TempDir()}
	mhSrv := httptest.NewServer(e.mh)
	t.Cleanup(mhSrv.Close)
	mixed := httptest.NewServer(http.HandlerFunc(e.mh.proxyHTTP))
	t.Cleanup(mixed.Close)
	_, port, _ := net.SplitHostPort(mixed.Listener.Addr().String())
	e.mh.mixedPort, _ = strconv.Atoi(port)
	jobs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.requests++
		e.ims = r.Header.Get("If-Modified-Since")
		if r.URL.Path != "/diag/car.json" || e.job == nil {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "", e.jobAt, bytes.NewReader(e.job))
	}))
	t.Cleanup(jobs.Close)

	c := defaultConfig()
	c.DeviceName = "car"
	c.UpdateURL = jobs.URL + "/update.json"
	c.MihomoAPI = mhSrv.URL
	c.DNSProbe = ""
	e.cfg = &configStore{c: c}
	keys := func(Config) []ed25519.PublicKey { return []ed25519.PublicKey{e.priv.Public().(ed25519.PublicKey)} }
	e.d = e.diagnoser(keys)
	return e
}

// diagnoser starts the console again on the same data directory
func (e *diagEnv) diagnoser(keys func(Config) []ed25519.PublicKey) *diagnoser {
	d := newDiagnoser(e.cfg, nil, newMihomo(e.cfg.get().MihomoAPI, ""), &outbound{}, keys, e.dataDir)
	d.shipNow = func() { e.shipped++ }
	return d
}

// publish signs a job and serves it, as last modified at
func (e *diagEnv) publish(id string, at time.Time, probes ...diagProbe) {
	e.t.Helper()
	j, err := signDiag(e.priv, id, "car", time.Hour, probes, time.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := json.Marshal(j)
	e.mu.Lock()
	e.job, e.jobAt = b, at
	e.mu.Unlock()
}

func (e *diagEnv) poll() {
	e.d.poll(context.Background(), e.cfg.get())
}

// results reads diag.log
func (e *diagEnv) results() []diagResult {
	e.t.Helper()
	f, err := os.Open(filepath.Join(e.dataDir, diagLogFile))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []diagResult
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if !strings.HasPrefix(sc.Text(), `{"time":"`) {
			e.t.Fatalf("line does not start with its time: %s", sc.Text())
		}
		var r diagResult
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestDiagPollRunsEachJobOnce(t *testing.T) {
	e := newDiagEnv(t)
	e.poll() // no job yet: a 404 is not an error
	if in := e.d.info(); in.Error != "" || in.Last != nil || !in.Enabled {
		t.Fatalf("before any job: %+v", in)
	}
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	e.publish("job-1", at, diagProbe{Kind: "dns", Name: "accounts.google.com"}, diagProbe{Kind: "dns", Name: "accounts.google.com", Type: "HTTPS"},
		diagProbe{Kind: "proxies"})
	e.poll()
	rs := e.results()
	if len(rs) != 4 {
		t.Fatalf("%d results: %+v", len(rs), rs)
	}
	if r := rs[0]; !r.OK || r.Job != "job-1" || r.N != 1 || r.Kind != "dns" || !strings.Contains(r.Msg, "upstream NOERROR 203.0.113.") {
		t.Errorf("dns: %+v", r)
	}
	if r := rs[1]; r.OK || !strings.Contains(r.Msg, "SERVFAIL") {
		t.Errorf("failing dns: %+v", r)
	}
	if r := rs[3]; r.Kind != "job" || r.N != 0 || r.OK || r.Msg == "" || !strings.Contains(r.Msg, "2 ok, 1 failed") {
		t.Errorf("summary: %+v", r)
	}
	if e.shipped != 1 {
		t.Errorf("shipped %d times", e.shipped)
	}
	if in := e.d.info(); in.Last == nil || in.Last.ID != "job-1" || in.Last.OK != 2 || in.Last.Failed != 1 {
		t.Errorf("info: %+v", in.Last)
	}
	// polls every diagMinInterval for a while after a job
	if iv := e.d.interval(e.cfg.get()); iv > diagMinInterval*11/10 {
		t.Errorf("interval after a job: %s", iv)
	}

	e.poll() // unchanged: 304
	e.mu.Lock()
	ims := e.ims
	e.mu.Unlock()
	if ims == "" {
		t.Error("no If-Modified-Since")
	}
	// the same id again, changed and in a console started anew, does not run again
	e.publish("job-1", at.Add(time.Second), diagProbe{Kind: "health"})
	e.d = e.diagnoser(e.d.keys)
	if e.d.info().Last == nil {
		t.Error("the last job was not kept")
	}
	e.poll()
	if n := len(e.results()); n != 4 {
		t.Fatalf("job-1 ran again: %d results", n)
	}
	e.publish("job-2", at.Add(2*time.Second), diagProbe{Kind: "proxies"})
	e.poll()
	if rs := e.results(); len(rs) != 6 || rs[4].Job != "job-2" {
		t.Fatalf("job-2: %+v", rs[4:])
	}

	// a job signed with another key is refused and reported
	e.d = e.diagnoser(func(Config) []ed25519.PublicKey {
		return []ed25519.PublicKey{testDiagKey(t).Public().(ed25519.PublicKey)}
	})
	e.publish("job-3", at.Add(3*time.Second), diagProbe{Kind: "proxies"})
	e.poll()
	if in := e.d.info(); !strings.Contains(in.Error, "signature") || len(e.results()) != 6 {
		t.Errorf("foreign job: %q", in.Error)
	}
}

func TestDiagHTTPProbe(t *testing.T) {
	e := newDiagEnv(t)
	r := e.d.probe(context.Background(), e.cfg.get(), diagProbe{Kind: "http", URL: "http://accounts.google.com/", Follow: true})[0]
	hops := r.Data.(map[string]any)["hops"].([]diagHop)
	if !r.OK || len(hops) != 2 || hops[0].Status != http.StatusFound || hops[1].Status != http.StatusOK {
		t.Fatalf("%+v %+v", r, hops)
	}
	// the request is matched to its mihomo connection by the local port
	if h := hops[1]; h.Rule != "RuleSet" || h.Payload != "proxy-domains" || len(h.Chains) != 3 || h.Server != "demo" {
		t.Errorf("hop: %+v", h)
	}
	if want := "http accounts.google.com after 1 redirects 200 via Proxy[HK 01] (RuleSet proxy-domains)"; r.Msg != want {
		t.Errorf("msg %q, want %q", r.Msg, want)
	}
	// without follow it stops at the redirect, and the body is only included when asked for
	r = e.d.probe(context.Background(), e.cfg.get(), diagProbe{Kind: "http", URL: "http://accounts.google.com/x", Body: true})[0]
	if h := r.Data.(map[string]any)["hops"].([]diagHop); len(h) != 1 || !strings.Contains(h[0].Body, "Sign in") {
		t.Errorf("body: %+v", h)
	}
	r = e.d.probe(context.Background(), e.cfg.get(), diagProbe{Kind: "http", URL: "https://accounts.google.com/"})[0]
	if r.OK || r.Error == "" || !strings.HasPrefix(r.Msg, "http accounts.google.com: ") {
		t.Errorf("failed request: %+v", r)
	}
}

func TestDiagConnections(t *testing.T) {
	e := newDiagEnv(t)
	rs := e.d.probe(context.Background(), e.cfg.get(), diagProbe{Kind: "connections", Match: "YOUTUBE"})
	if len(rs) != 2 || !rs[0].OK || rs[1].Kind != "connection" {
		t.Fatalf("%+v", rs)
	}
	if want := "tcp "; !strings.HasPrefix(rs[1].Msg, want) || !strings.Contains(rs[1].Msg, "www.youtube.com:443 RuleSet(proxy-domains) Streaming[JP 01]") {
		t.Errorf("connection line %q", rs[1].Msg)
	}
	if all := e.d.probe(context.Background(), e.cfg.get(), diagProbe{Kind: "connections"}); len(all) != 1+len(demoConns()) {
		t.Errorf("%d lines for %d connections", len(all), len(demoConns()))
	}
}

func TestDiagLogLevelComesBack(t *testing.T) {
	e := newDiagEnv(t)
	level := func() string {
		e.mh.mu.Lock()
		defer e.mh.mu.Unlock()
		return e.mh.logLevel
	}
	r := e.d.probe(context.Background(), e.cfg.get(), diagProbe{Kind: "log", Level: "debug", Minutes: 5})[0]
	if !r.OK || level() != "debug" {
		t.Fatalf("%+v, mihomo at %s", r, level())
	}
	if in := e.d.info(); in.LogLevel != "debug" || time.Until(in.LogUntil) < 4*time.Minute {
		t.Errorf("info: %s until %s", in.LogLevel, in.LogUntil)
	}
	// a second log probe extends the time but keeps the level to go back to
	e.d.probe(context.Background(), e.cfg.get(), diagProbe{Kind: "log", Level: "info", Minutes: 5})
	e.d.logTimer.Stop()

	// the console restarts after the time is up: the level is restored on start
	b, _ := os.ReadFile(filepath.Join(e.dataDir, "diag", "state.json"))
	var st diagState
	json.Unmarshal(b, &st)
	if st.LogWas != "warning" || st.LogLevel != "info" {
		t.Fatalf("state: %+v", st)
	}
	st.LogUntil = time.Now().Add(-time.Minute)
	b, _ = json.Marshal(st)
	os.WriteFile(filepath.Join(e.dataDir, "diag", "state.json"), b, 0o600)
	d := e.diagnoser(e.d.keys)
	d.resumeLog(context.Background())
	waitFor(t, "the log level to come back", func() bool { return level() == "warning" })
	waitFor(t, "the state to be cleared", func() bool { return d.info().LogLevel == "" })
}

func TestDiagLineLimit(t *testing.T) {
	var buf bytes.Buffer
	d := &diagnoser{logw: &buf}
	d.write(diagResult{Time: time.Now(), Msg: "big", Job: "j", Kind: "proxies", OK: true, Data: strings.Repeat("x", diagLineMax)})
	line := strings.TrimSuffix(buf.String(), "\n")
	var r diagResult
	if len(line) > diagLineMax || json.Unmarshal([]byte(line), &r) != nil || r.Data != nil || !strings.Contains(r.Error, "too large") {
		t.Fatalf("%d bytes: %.200s", len(line), line)
	}
}

func TestShipDiagResultsSoon(t *testing.T) {
	e := newShipEnv(t)
	sh := e.shipper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sh.loop(ctx)
	time.Sleep(100 * time.Millisecond) // the first round found nothing
	at := time.Date(2026, 10, 20, 17, 3, 0, 0, time.UTC)
	b, _ := json.Marshal(diagResult{Time: at, Msg: "dns x A: ok", Job: "j", N: 1, Kind: "dns", OK: true})
	appendFile(t, filepath.Join(e.dataDir, diagLogFile), string(b)+"\n")
	sh.soon()
	waitFor(t, "the diag line", func() bool {
		e.loki.mu.Lock()
		defer e.loki.mu.Unlock()
		return len(e.loki.lines["diag"]) == 1
	})
	if ts, ok := lineTime(string(b)); !ok || !ts.Equal(at) {
		t.Errorf("line time %v %v", ts, ok)
	}
}

func TestDiagAPI(t *testing.T) {
	c := newTestClient(t)
	if code, _ := c.do("POST", "/api/login", `{"password":"`+demoPassword+`"}`, true); code != http.StatusOK {
		t.Fatal("login")
	}
	// the demo runs its job a few seconds after the start
	var st map[string]any
	for range 100 {
		if _, st = c.do("GET", "/api/diag", "", false); st["last"] != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st["last"] == nil {
		t.Fatalf("no job ran: %v", st)
	}
	last := st["last"].(map[string]any)
	if st["enabled"] != true || last["failed"].(float64) != 1 || len(last["results"].([]any)) < len(demoDiagProbes)+1 {
		t.Errorf("%v", st)
	}
}
