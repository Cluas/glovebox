package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Remote diagnostics: the device polls diag_url for a job signed with the release key, runs its
// probes and writes one JSON line per result to diag.log, which the log shipper sends home with
// source="diag". As with the remote upgrade nothing has to reach the device. The probes only
// look: the one thing a job can change is mihomo's log level, for at most diagLogMaxMinutes.

const (
	diagSigContext    = "glovebox-diag-v1"
	diagJobMax        = 64 << 10
	diagProbesMax     = 40
	diagMaxAhead      = 24 * time.Hour
	diagJobTimeout    = 3 * time.Minute
	diagProbeTimeout  = 15 * time.Second
	diagFirstPoll     = 30 * time.Second
	diagMinInterval   = 15 * time.Second
	diagFastFor       = 10 * time.Minute // after a job, poll every diagMinInterval for follow-ups
	diagDoneKeep      = 50
	diagLogFile       = "diag.log"
	diagLogMax        = 256 << 10
	diagLineMax       = 7 << 10 // below shipLineMax, so a shipped line stays valid JSON
	diagLogMaxMinutes = 30
	diagBodyMax       = 512
	diagRedirectsMax  = 5
	diagConnsMax      = 200
)

// diagJob is diag.json
type diagJob struct {
	ID       string      `json:"id"`
	Device   string      `json:"device"`    // device_name of the one device that runs it
	NotAfter string      `json:"not_after"` // RFC 3339; at most diagMaxAhead after the device's clock
	Probes   []diagProbe `json:"probes"`
	Sig      string      `json:"sig,omitempty"` // base64 ed25519 signature of signedText
}

type diagProbe struct {
	Kind    string `json:"kind"`              // dns, http, delay, proxies, connections, health, log
	Name    string `json:"name,omitempty"`    // dns: the name to resolve
	Type    string `json:"type,omitempty"`    // dns: A (default), AAAA or HTTPS
	URL     string `json:"url,omitempty"`     // http; delay (default delay_url)
	Follow  bool   `json:"follow,omitempty"`  // http: follow up to diagRedirectsMax redirects
	UA      string `json:"ua,omitempty"`      // http: User-Agent
	Body    bool   `json:"body,omitempty"`    // http: include the start of the response body
	Proxy   string `json:"proxy,omitempty"`   // delay: proxy or group to test
	Match   string `json:"match,omitempty"`   // connections: keep those containing this text
	Level   string `json:"level,omitempty"`   // log: info or debug
	Minutes int    `json:"minutes,omitempty"` // log: how long, default 10
}

var diagIDRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._:-]{0,63}$`)

// signedText is what the signature covers: the job without its signature. Signer and device
// marshal the same struct, so the bytes agree; unknown fields are dropped on both sides.
func (j diagJob) signedText() []byte {
	j.Sig = ""
	b, _ := json.Marshal(j)
	return append([]byte(diagSigContext+"\n"), b...)
}

func (j diagJob) verify(keys []ed25519.PublicKey, device string, now time.Time) error {
	sig, err := base64.StdEncoding.DecodeString(j.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("bad signature encoding")
	}
	msg := j.signedText()
	if !slices.ContainsFunc(keys, func(k ed25519.PublicKey) bool { return ed25519.Verify(k, msg, sig) }) {
		return errors.New("signature does not match a trusted key")
	}
	return j.check(device, now)
}

// check validates everything but the signature
func (j diagJob) check(device string, now time.Time) error {
	if !diagIDRe.MatchString(j.ID) {
		return fmt.Errorf("bad id %q", j.ID)
	}
	if j.Device != device {
		return fmt.Errorf("job is for %q, this device is %q", j.Device, device)
	}
	until, err := time.Parse(time.RFC3339, j.NotAfter)
	if err != nil {
		return fmt.Errorf("bad not_after %q", j.NotAfter)
	}
	if now.After(until) {
		return fmt.Errorf("job expired at %s", j.NotAfter)
	}
	if until.Sub(now) > diagMaxAhead {
		return fmt.Errorf("not_after %s is more than %s ahead", j.NotAfter, diagMaxAhead)
	}
	if len(j.Probes) == 0 || len(j.Probes) > diagProbesMax {
		return fmt.Errorf("want 1 to %d probes, got %d", diagProbesMax, len(j.Probes))
	}
	for i, p := range j.Probes {
		if err := p.check(); err != nil {
			return fmt.Errorf("probe %d (%s): %w", i+1, p.Kind, err)
		}
	}
	return nil
}

func (p diagProbe) check() error {
	switch p.Kind {
	case "dns":
		if p.Name == "" || len(p.Name) > 253 || strings.ContainsAny(p.Name, " /:") {
			return fmt.Errorf("bad name %q", p.Name)
		}
		if t := p.qtype(); t != "A" && t != "AAAA" && t != "HTTPS" {
			return fmt.Errorf("type %q: want A, AAAA or HTTPS", p.Type)
		}
	case "http":
		return checkProbeURL(p.URL)
	case "delay":
		if p.Proxy == "" {
			return errors.New("missing proxy")
		}
		if p.URL != "" {
			return checkProbeURL(p.URL)
		}
	case "proxies", "connections", "health":
	case "log":
		if p.Level != "info" && p.Level != "debug" {
			return fmt.Errorf("level %q: want info or debug", p.Level)
		}
		if p.Minutes < 0 || p.Minutes > diagLogMaxMinutes {
			return fmt.Errorf("minutes %d: want at most %d", p.Minutes, diagLogMaxMinutes)
		}
	default:
		return errors.New("unknown kind")
	}
	return nil
}

func (p diagProbe) qtype() string {
	if p.Type == "" {
		return "A"
	}
	return strings.ToUpper(p.Type)
}

// checkProbeURL keeps requests off the device itself: its own APIs (mihomo, the vendor web
// interface) answer on loopback and link-local addresses
func checkProbeURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("url %q: want an http(s) URL", raw)
	}
	h := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return fmt.Errorf("url %q: the device itself is off limits", raw)
	}
	if ip := net.ParseIP(h); ip != nil && (ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()) {
		return fmt.Errorf("url %q: the device itself is off limits", raw)
	}
	return nil
}

// diagResult is one line of diag.log. time comes first: the shipper stamps lines with it.
type diagResult struct {
	Time  time.Time `json:"time"`
	Msg   string    `json:"_msg"`   // the message field of VictoriaLogs
	Job   string    `json:"job_id"` // not "job": that is a stream label of the shipped logs
	N     int       `json:"n"`      // probe number, 0 for the job summary
	Kind  string    `json:"kind"`
	OK    bool      `json:"ok"`
	Ms    int64     `json:"ms"`
	Error string    `json:"error,omitempty"`
	Data  any       `json:"data,omitempty"`
}

type diagJobInfo struct {
	ID      string       `json:"id"`
	At      time.Time    `json:"at"`
	Ms      int64        `json:"ms"`
	OK      int          `json:"ok"`
	Failed  int          `json:"failed"`
	Results []diagResult `json:"results"`
}

// diagState is diag/state.json
type diagState struct {
	Done     []string  `json:"done,omitempty"`      // job ids that ran, newest last
	Modified string    `json:"modified,omitempty"`  // Last-Modified of the last diag.json seen
	LogLevel string    `json:"log_level,omitempty"` // set by a log probe until LogUntil
	LogWas   string    `json:"log_was,omitempty"`   // mihomo's own level, restored then
	LogUntil time.Time `json:"log_until,omitzero"`
}

type diagnoser struct {
	cfg     *configStore
	health  *health
	mihomo  *mihomoClient
	out     *outbound
	keys    func(Config) []ed25519.PublicKey
	dir     string // <data dir>/diag; empty keeps no state
	logw    io.Writer
	shipNow func()

	mu        sync.Mutex
	st        diagState
	polledAt  time.Time
	err       string
	running   string // job id
	fastUntil time.Time
	last      *diagJobInfo
	logTimer  *time.Timer
}

func newDiagnoser(cs *configStore, h *health, mc *mihomoClient, out *outbound, keys func(Config) []ed25519.PublicKey, dataDir string) *diagnoser {
	d := &diagnoser{cfg: cs, health: h, mihomo: mc, out: out, keys: keys, logw: io.Discard, shipNow: func() {}}
	if dataDir == "" {
		return d
	}
	d.dir = filepath.Join(dataDir, "diag")
	if b, err := os.ReadFile(filepath.Join(d.dir, "state.json")); err == nil {
		json.Unmarshal(b, &d.st)
	}
	if b, err := os.ReadFile(filepath.Join(d.dir, "last.json")); err == nil {
		var last diagJobInfo
		if json.Unmarshal(b, &last) == nil {
			d.last = &last
		}
	}
	if w, err := newRotatingLog(filepath.Join(dataDir, diagLogFile), diagLogMax); err == nil {
		d.logw = w
	} else {
		log.Printf("diagnostics: %v", err)
	}
	return d
}

// diagURL resolves diag_url: "auto" is diag/<device_name>.json next to a self-hosted update_url.
// Releases on GitHub are not per device, so there it stays off.
func diagURL(c Config) string {
	if c.DiagURL != "auto" {
		return c.DiagURL
	}
	base, err := url.Parse(c.UpdateURL)
	if c.UpdateURL == "" || err != nil || base.Host == "" {
		return ""
	}
	if h := strings.ToLower(base.Hostname()); h == "github.com" || strings.HasSuffix(h, ".github.com") ||
		strings.HasSuffix(h, ".githubusercontent.com") {
		return ""
	}
	name := c.DeviceName + ".json"
	return base.ResolveReference(&url.URL{Path: "diag/" + name, RawPath: "diag/" + url.PathEscape(name)}).String()
}

func (d *diagnoser) interval(c Config) time.Duration {
	iv, err := time.ParseDuration(c.DiagInterval)
	if err != nil {
		iv = time.Minute
	}
	iv = max(iv, diagMinInterval)
	d.mu.Lock()
	fast := time.Now().Before(d.fastUntil)
	d.mu.Unlock()
	if fast {
		iv = diagMinInterval
	}
	return iv - iv/10 + rand.N(iv/5)
}

func (d *diagnoser) loop(ctx context.Context) {
	d.resumeLog(ctx)
	wait := diagFirstPoll
	for {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		c := d.cfg.get()
		d.poll(ctx, c)
		wait = d.interval(c)
	}
}

// poll fetches diag.json and runs it when it holds a job that did not run yet
func (d *diagnoser) poll(ctx context.Context, c Config) {
	u := diagURL(c)
	if u == "" || (d.health != nil && !d.health.clockOK()) {
		return
	}
	j, err := d.fetch(ctx, c, u)
	d.mu.Lock()
	d.polledAt = time.Now()
	if err != nil {
		if d.err != err.Error() {
			log.Printf("diagnostics: %v", err)
		}
		d.err = err.Error()
	} else {
		d.err = ""
	}
	d.mu.Unlock()
	if j != nil {
		d.run(ctx, c, *j)
	}
}

func (d *diagnoser) fetch(ctx context.Context, c Config, u string) (*diagJob, error) {
	hc, err := d.out.client(c.ReportProxy)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "glovebox/"+version)
	d.mu.Lock()
	if d.st.Modified != "" {
		req.Header.Set("If-Modified-Since", d.st.Modified)
	}
	d.mu.Unlock()
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified, http.StatusNotFound: // nothing new, no job
		return nil, nil
	case http.StatusOK:
	default:
		return nil, fmt.Errorf("diag.json: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, diagJobMax+1))
	if err != nil {
		return nil, err
	}
	// a job that is refused is not fetched again until it changes
	d.change(func(st *diagState) { st.Modified = resp.Header.Get("Last-Modified") })
	if len(b) > diagJobMax {
		return nil, errors.New("diag.json: too large")
	}
	var j diagJob
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("diag.json: %w", err)
	}
	if err := j.verify(d.keys(c), c.DeviceName, time.Now()); err != nil {
		return nil, fmt.Errorf("diag.json: %w", err)
	}
	d.mu.Lock()
	done := slices.Contains(d.st.Done, j.ID)
	d.mu.Unlock()
	if done {
		return nil, nil
	}
	return &j, nil
}

func (d *diagnoser) change(fn func(st *diagState)) {
	d.mu.Lock()
	fn(&d.st)
	b, _ := json.MarshalIndent(d.st, "", "  ")
	d.mu.Unlock()
	if d.dir == "" {
		return
	}
	if err := os.MkdirAll(d.dir, 0o700); err == nil {
		err = writeFileAtomic(filepath.Join(d.dir, "state.json"), append(b, '\n'), 0o600)
	}
}

// run executes the probes in order and records every result
func (d *diagnoser) run(ctx context.Context, c Config, j diagJob) {
	// marked first: a job that crashes the console must not run again after the restart
	d.change(func(st *diagState) {
		st.Done = append(st.Done, j.ID)
		if over := len(st.Done) - diagDoneKeep; over > 0 {
			st.Done = st.Done[over:]
		}
	})
	d.mu.Lock()
	d.running = j.ID
	d.mu.Unlock()
	log.Printf("diagnostics: running job %s (%d probes)", j.ID, len(j.Probes))
	ctx, cancel := context.WithTimeout(ctx, diagJobTimeout)
	defer cancel()
	start := time.Now()
	info := &diagJobInfo{ID: j.ID, At: start}
	for i, p := range j.Probes {
		t0 := time.Now()
		pctx, pcancel := context.WithTimeout(ctx, diagProbeTimeout)
		rs := d.probe(pctx, c, p)
		pcancel()
		for k := range rs {
			rs[k].Job, rs[k].N = j.ID, i+1
			if rs[k].Kind == "" {
				rs[k].Kind = p.Kind
			}
			if rs[k].Time.IsZero() {
				rs[k].Time = time.Now()
			}
		}
		rs[0].Ms = time.Since(t0).Milliseconds()
		if rs[0].OK {
			info.OK++
		} else {
			info.Failed++
		}
		for _, r := range rs {
			d.write(r)
		}
		info.Results = append(info.Results, rs...)
	}
	info.Ms = time.Since(start).Milliseconds()
	sum := diagResult{Time: time.Now(), Job: j.ID, Kind: "job", OK: info.Failed == 0, Ms: info.Ms,
		Msg: fmt.Sprintf("job %s done: %d ok, %d failed in %s", j.ID, info.OK, info.Failed, roundDur(time.Since(start)))}
	d.write(sum)
	info.Results = append(info.Results, sum)
	log.Print("diagnostics: " + sum.Msg)

	d.mu.Lock()
	d.running, d.last, d.fastUntil = "", info, time.Now().Add(diagFastFor)
	d.mu.Unlock()
	if d.dir != "" {
		if b, err := json.Marshal(info); err == nil {
			writeFileAtomic(filepath.Join(d.dir, "last.json"), b, 0o600)
		}
	}
	d.shipNow()
}

// write appends r to diag.log; a result too large for one shipped line loses its data
func (d *diagnoser) write(r diagResult) {
	b, _ := json.Marshal(r)
	if len(b) > diagLineMax {
		r.Data, r.Error = nil, strings.TrimSpace(r.Error+" (details too large to ship)")
		b, _ = json.Marshal(r)
	}
	d.logw.Write(append(b, '\n'))
}

func roundDur(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(time.Millisecond)
	}
	return d.Round(100 * time.Millisecond)
}

// probe runs one probe; the first result describes it, kinds such as connections add lines
func (d *diagnoser) probe(ctx context.Context, c Config, p diagProbe) []diagResult {
	r := diagResult{Kind: p.Kind}
	var more []diagResult
	var err error
	switch p.Kind {
	case "dns":
		err = d.probeDNS(ctx, c, p, &r)
	case "http":
		err = d.probeHTTP(ctx, p, &r)
	case "delay":
		err = d.probeDelay(ctx, c, p, &r)
	case "proxies":
		err = d.probeProxies(ctx, &r)
	case "connections":
		more, err = d.probeConns(ctx, p, &r)
	case "health":
		d.probeHealth(&r)
	case "log":
		err = d.probeLog(ctx, p, &r)
	}
	if err != nil {
		r.OK, r.Error = false, err.Error()
		if r.Msg == "" {
			r.Msg = p.Kind + " " + diagTarget(p) + ": " + err.Error()
		}
	}
	return append([]diagResult{r}, more...)
}

func diagTarget(p diagProbe) string {
	switch p.Kind {
	case "dns":
		return p.Name + " " + p.qtype()
	case "http":
		return p.URL
	case "delay":
		return p.Proxy
	}
	return ""
}

type diagDNSSide struct {
	Status string   `json:"status,omitempty"`
	Addrs  []string `json:"addrs,omitempty"`
	Error  string   `json:"error,omitempty"`
	Ms     int64    `json:"ms"`
}

// probeDNS asks the listener hotspot clients use (with fake-ip that answers from memory) and
// mihomo's resolver, which asks the upstream servers
func (d *diagnoser) probeDNS(ctx context.Context, c Config, p diagProbe, r *diagResult) error {
	qt := p.qtype()
	data := map[string]diagDNSSide{}
	var parts []string
	ok := true
	if c.DNSProbe != "" && qt != "HTTPS" {
		res := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dl net.Dialer
			return dl.DialContext(ctx, "udp", c.DNSProbe)
		}}
		network := "ip4"
		if qt == "AAAA" {
			network = "ip6"
		}
		t0 := time.Now()
		ips, err := res.LookupIP(ctx, network, p.Name)
		side := diagDNSSide{Ms: time.Since(t0).Milliseconds()}
		var de *net.DNSError
		switch {
		case err == nil:
			for _, ip := range ips {
				side.Addrs = append(side.Addrs, ip.String())
			}
			parts = append(parts, fmt.Sprintf("listener %s (%dms)", strings.Join(side.Addrs, " "), side.Ms))
		case errors.As(err, &de) && de.IsNotFound && qt == "AAAA":
			side.Status = "no address" // ipv6: false answers AAAA with nothing
			parts = append(parts, fmt.Sprintf("listener no address (%dms)", side.Ms))
		default:
			side.Error, ok = err.Error(), false
			parts = append(parts, "listener "+err.Error())
		}
		data["listener"] = side
	}
	t0 := time.Now()
	rcode, answers, err := d.mihomo.dnsLookup(ctx, p.Name, qt)
	side := diagDNSSide{Ms: time.Since(t0).Milliseconds(), Addrs: answers}
	if err != nil {
		side.Error, ok = err.Error(), false
		parts = append(parts, "upstream "+err.Error())
	} else {
		side.Status = rcodeName(rcode)
		ok = ok && rcode == 0
		parts = append(parts, strings.TrimSpace(fmt.Sprintf("upstream %s %s", side.Status, strings.Join(answers, " ")))+fmt.Sprintf(" (%dms)", side.Ms))
	}
	data["upstream"] = side
	r.OK, r.Data = ok, data
	r.Msg = "dns " + p.Name + " " + qt + ": " + strings.Join(parts, ", ")
	return nil
}

func rcodeName(rc int) string {
	switch rc {
	case 0:
		return "NOERROR"
	case 2:
		return "SERVFAIL"
	case 3:
		return "NXDOMAIN"
	case 5:
		return "REFUSED"
	}
	return "RCODE" + strconv.Itoa(rc)
}

type diagHop struct {
	URL      string   `json:"url"`
	Status   int      `json:"status,omitempty"`
	Location string   `json:"location,omitempty"`
	Server   string   `json:"server,omitempty"`
	TLS      string   `json:"tls,omitempty"`
	Cert     string   `json:"cert,omitempty"` // subject / issuer of the server certificate
	TLSMs    int64    `json:"tls_ms,omitempty"`
	TTFBMs   int64    `json:"ttfb_ms,omitempty"`
	Bytes    int64    `json:"bytes,omitempty"`
	Body     string   `json:"body,omitempty"`
	Rule     string   `json:"rule,omitempty"`
	Payload  string   `json:"payload,omitempty"`
	Chains   []string `json:"chains,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// probeHTTP requests the URL through mihomo's mixed port, so it takes the rules a hotspot client
// would; the connection is found in /connections by its local port to name the rule and proxy
func (d *diagnoser) probeHTTP(ctx context.Context, p diagProbe, r *diagResult) error {
	cfg, err := d.mihomo.configs(ctx)
	if err != nil {
		return fmt.Errorf("mihomo: %w", err)
	}
	port := 0
	for _, k := range []string{"mixed-port", "port"} {
		if v, ok := cfg[k].(float64); ok && v > 0 {
			port = int(v)
			break
		}
	}
	if port == 0 {
		return errors.New("mihomo has no mixed-port or port to send the request through")
	}
	var local string // the client side of the connection to mihomo
	tr := &http.Transport{
		Proxy:             http.ProxyURL(&url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}),
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var dl net.Dialer
			conn, err := dl.DialContext(ctx, network, addr)
			if err == nil {
				local = conn.LocalAddr().String()
			}
			return conn, err
		},
		TLSHandshakeTimeout:   diagProbeTimeout,
		ResponseHeaderTimeout: diagProbeTimeout,
	}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	var hops []diagHop
	next := p.URL
	for i := 0; i <= diagRedirectsMax && next != ""; i++ {
		if i > 0 {
			if err := checkProbeURL(next); err != nil {
				return err
			}
		}
		hop := d.hop(ctx, hc, p, next, &local)
		hops = append(hops, hop)
		next = ""
		if p.Follow && hop.Error == "" && hop.Location != "" && hop.Status >= 300 && hop.Status < 400 {
			if u, err := url.Parse(hop.URL); err == nil {
				if lu, err := u.Parse(hop.Location); err == nil {
					next = lu.String()
				}
			}
		}
	}
	last := hops[len(hops)-1]
	r.Data = map[string]any{"hops": hops}
	host := p.URL
	if u, err := url.Parse(p.URL); err == nil {
		host = u.Host
	}
	via := ""
	if len(last.Chains) > 0 {
		via = fmt.Sprintf(" via %s[%s]", last.Chains[len(last.Chains)-1], last.Chains[0])
	}
	if last.Rule != "" {
		via += fmt.Sprintf(" (%s %s)", last.Rule, last.Payload)
	}
	redirects := ""
	if len(hops) > 1 {
		redirects = fmt.Sprintf(" after %d redirects", len(hops)-1)
	}
	if last.Error != "" {
		r.Msg = fmt.Sprintf("http %s%s: %s%s", host, redirects, last.Error, via)
		return errors.New(last.Error)
	}
	r.OK = true
	r.Msg = fmt.Sprintf("http %s%s %d%s", host, redirects, last.Status, via)
	return nil
}

func (d *diagnoser) hop(ctx context.Context, hc *http.Client, p diagProbe, target string, local *string) diagHop {
	h := diagHop{URL: target}
	var start, tlsStart time.Time
	trace := &httptrace.ClientTrace{
		TLSHandshakeStart: func() { tlsStart = time.Now() },
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			if !tlsStart.IsZero() {
				h.TLSMs = time.Since(tlsStart).Milliseconds()
			}
		},
		GotFirstResponseByte: func() { h.TTFBMs = time.Since(start).Milliseconds() },
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, target, nil)
	if err != nil {
		h.Error = err.Error()
		return h
	}
	ua := p.UA
	if ua == "" {
		ua = "glovebox/" + version + " (diagnostics)"
	}
	req.Header.Set("User-Agent", ua)
	*local = ""
	start = time.Now()
	resp, err := hc.Do(req)
	// the connection is still open: look it up before reading the body closes it
	d.ruleOf(ctx, *local, &h)
	if err != nil {
		h.Error = cleanNetErr(err)
		return h
	}
	defer resp.Body.Close()
	h.Status = resp.StatusCode
	h.Location = resp.Header.Get("Location")
	h.Server = resp.Header.Get("Server")
	if cs := resp.TLS; cs != nil {
		h.TLS = tls.VersionName(cs.Version)
		if len(cs.PeerCertificates) > 0 {
			crt := cs.PeerCertificates[0]
			h.Cert = crt.Subject.CommonName + " / " + strings.TrimSpace(crt.Issuer.CommonName+" "+strings.Join(crt.Issuer.Organization, " "))
		}
	}
	buf := make([]byte, diagBodyMax)
	n, _ := io.ReadFull(resp.Body, buf)
	rest, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	h.Bytes = int64(n) + rest
	if p.Body && n > 0 {
		h.Body = strings.ToValidUTF8(string(buf[:n]), "")
	}
	return h
}

// ruleOf fills the rule and proxy chain of the mihomo connection whose source is local
func (d *diagnoser) ruleOf(ctx context.Context, local string, h *diagHop) {
	_, port, err := net.SplitHostPort(local)
	if err != nil {
		return
	}
	cs, err := d.mihomo.connections(ctx)
	if err != nil {
		return
	}
	for _, cn := range cs.Connections {
		if cn.Metadata.SourcePort == port && (cn.Metadata.SourceIP == "127.0.0.1" || cn.Metadata.SourceIP == "::1") {
			h.Rule, h.Payload, h.Chains = cn.Rule, cn.Payload, cn.Chains
			return
		}
	}
}

// cleanNetErr drops the request line Go puts in front of the cause
func cleanNetErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return err.Error()
}

func (d *diagnoser) probeDelay(ctx context.Context, c Config, p diagProbe, r *diagResult) error {
	u := p.URL
	if u == "" {
		u = c.DelayURL
	}
	ms, err := d.mihomo.proxyDelay(ctx, p.Proxy, u, int(diagProbeTimeout/time.Millisecond)-1000)
	if err != nil {
		return err
	}
	if ms <= 0 {
		return errors.New("no response")
	}
	r.OK, r.Data = true, map[string]any{"proxy": p.Proxy, "url": u, "delay": ms}
	r.Msg = fmt.Sprintf("delay %s %s: %dms", p.Proxy, u, ms)
	return nil
}

// probeProxies records what every group has selected
func (d *diagnoser) probeProxies(ctx context.Context, r *diagResult) error {
	ps, err := d.mihomo.proxies(ctx)
	if err != nil {
		return err
	}
	now := map[string]string{}
	for name, p := range ps {
		if len(p.All) > 0 && p.Now != "" && name != "GLOBAL" {
			now[name] = p.Now
		}
	}
	r.OK, r.Data = true, now
	r.Msg = fmt.Sprintf("proxies: %d groups", len(now))
	return nil
}

type diagConn struct {
	Network string    `json:"network"`
	Source  string    `json:"source"`
	Host    string    `json:"host"`
	Dest    string    `json:"dest"`
	Rule    string    `json:"rule"`
	Payload string    `json:"payload,omitempty"`
	Chains  []string  `json:"chains"`
	Up      int64     `json:"up"`
	Down    int64     `json:"down"`
	Start   time.Time `json:"start"`
}

// probeConns snapshots mihomo's connections, one line each: what the hotspot clients are doing
func (d *diagnoser) probeConns(ctx context.Context, p diagProbe, r *diagResult) ([]diagResult, error) {
	cs, err := d.mihomo.connections(ctx)
	if err != nil {
		return nil, err
	}
	var conns []diagConn
	match := strings.ToLower(p.Match)
	for _, cn := range cs.Connections {
		md := cn.Metadata
		if md.SourceIP == "127.0.0.1" || md.SourceIP == "::1" {
			continue // Glovebox's own, such as this job's requests
		}
		host := md.Host
		if host == "" {
			host = md.SniffHost
		}
		dc := diagConn{Network: md.Network, Source: md.SourceIP, Host: host, Dest: net.JoinHostPort(md.DestIP, md.DestPort),
			Rule: cn.Rule, Payload: cn.Payload, Chains: cn.Chains, Up: cn.Upload, Down: cn.Download, Start: cn.Start}
		if match != "" && !strings.Contains(strings.ToLower(strings.Join(append([]string{dc.Host, dc.Dest, dc.Source, dc.Rule, dc.Payload}, dc.Chains...), " ")), match) {
			continue
		}
		conns = append(conns, dc)
	}
	sort.Slice(conns, func(i, j int) bool { return conns[i].Start.After(conns[j].Start) })
	total := len(conns)
	conns = conns[:min(len(conns), diagConnsMax)]
	r.OK, r.Data = true, map[string]int{"total": len(cs.Connections), "shown": len(conns)}
	r.Msg = fmt.Sprintf("connections: %d", total)
	if match != "" {
		r.Msg = fmt.Sprintf("connections matching %q: %d of %d", p.Match, total, len(cs.Connections))
	}
	var lines []diagResult
	for _, dc := range conns {
		target := dc.Host
		if target == "" {
			target = dc.Dest
		} else if _, port, err := net.SplitHostPort(dc.Dest); err == nil {
			target = net.JoinHostPort(dc.Host, port)
		}
		via := ""
		if len(dc.Chains) > 0 {
			via = fmt.Sprintf(" %s[%s]", dc.Chains[len(dc.Chains)-1], dc.Chains[0])
		}
		lines = append(lines, diagResult{Kind: "connection", OK: true, Data: dc,
			Msg: strings.TrimSpace(fmt.Sprintf("%s %s → %s %s(%s)%s", dc.Network, dc.Source, target, dc.Rule, dc.Payload, via))})
	}
	return lines, nil
}

func (d *diagnoser) probeHealth(r *diagResult) {
	if d.health == nil {
		r.Msg = "health: not running"
		return
	}
	checks := d.health.snapshot()
	var bad []string
	for _, c := range checks {
		if c.State == "fail" {
			bad = append(bad, c.Name)
		}
	}
	r.OK, r.Data = len(bad) == 0, checks
	r.Msg = "health: all ok"
	if len(bad) > 0 {
		r.Msg = "health: failing " + strings.Join(bad, ", ")
	}
}

// probeLog raises mihomo's log level for a while, so its connection log (rule and proxy of every
// connection, with errors) is shipped home; the level goes back on its own
func (d *diagnoser) probeLog(ctx context.Context, p diagProbe, r *diagResult) error {
	minutes := p.Minutes
	if minutes == 0 {
		minutes = 10
	}
	d.mu.Lock()
	orig := d.st.LogWas
	d.mu.Unlock()
	if orig == "" { // not raised already: remember mihomo's own level
		cfg, err := d.mihomo.configs(ctx)
		if err != nil {
			return fmt.Errorf("mihomo: %w", err)
		}
		orig, _ = cfg["log-level"].(string)
		if orig == "" {
			orig = "info"
		}
	}
	if err := d.mihomo.patchConfigs(ctx, map[string]any{"log-level": p.Level}); err != nil {
		return fmt.Errorf("mihomo: %w", err)
	}
	until := time.Now().Add(time.Duration(minutes) * time.Minute)
	d.change(func(st *diagState) { st.LogLevel, st.LogWas, st.LogUntil = p.Level, orig, until })
	d.scheduleLogRestore(time.Until(until))
	r.OK, r.Data = true, map[string]any{"level": p.Level, "was": orig, "until": until}
	r.Msg = fmt.Sprintf("log: mihomo logs at %s for %dm (was %s)", p.Level, minutes, orig)
	return nil
}

func (d *diagnoser) scheduleLogRestore(after time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.logTimer != nil {
		d.logTimer.Stop()
	}
	d.logTimer = time.AfterFunc(max(after, 0), func() { d.restoreLog(context.Background()) })
}

// resumeLog finishes a log probe that outlived a restart of the console
func (d *diagnoser) resumeLog(ctx context.Context) {
	d.mu.Lock()
	was, until := d.st.LogWas, d.st.LogUntil
	d.mu.Unlock()
	if was != "" {
		d.scheduleLogRestore(time.Until(until))
	}
}

func (d *diagnoser) restoreLog(ctx context.Context) {
	d.mu.Lock()
	level := d.st.LogWas
	d.mu.Unlock()
	if level == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := d.mihomo.patchConfigs(ctx, map[string]any{"log-level": level}); err != nil {
		log.Printf("diagnostics: restoring the mihomo log level: %v", err)
		d.scheduleLogRestore(time.Minute)
		return
	}
	d.change(func(st *diagState) { st.LogLevel, st.LogWas, st.LogUntil = "", "", time.Time{} })
	log.Printf("diagnostics: mihomo logs at %s again", level)
}

type diagInfo struct {
	Enabled  bool         `json:"enabled"`
	Source   string       `json:"source,omitempty"` // host of diag_url
	Interval string       `json:"interval,omitempty"`
	PolledAt time.Time    `json:"polled_at,omitzero"`
	Error    string       `json:"error,omitempty"`
	Running  string       `json:"running,omitempty"`
	LogLevel string       `json:"log_level,omitempty"` // raised by a log probe until log_until
	LogUntil time.Time    `json:"log_until,omitzero"`
	Last     *diagJobInfo `json:"last,omitempty"`
}

func (d *diagnoser) info() diagInfo {
	c := d.cfg.get()
	in := diagInfo{}
	if u, err := url.Parse(diagURL(c)); err == nil && u.Host != "" {
		in.Enabled, in.Source, in.Interval = true, u.Host, c.DiagInterval
		if iv, err := time.ParseDuration(c.DiagInterval); err != nil {
			in.Interval = "1m"
		} else if iv < diagMinInterval {
			in.Interval = diagMinInterval.String()
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	in.PolledAt, in.Error, in.Running, in.Last = d.polledAt, d.err, d.running, d.last
	if d.st.LogWas != "" {
		in.LogLevel, in.LogUntil = d.st.LogLevel, d.st.LogUntil
	}
	return in
}
