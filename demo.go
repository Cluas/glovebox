package main

// Demo mode: `glovebox demo` runs the full console on any computer without a device.
// The backends are in-process fakes of the ZTE goform and mihomo APIs (exercised through the same
// client code as on a device); rates, signal and system info are synthetic, and actions such as
// reboot or setting the clock are only logged. Used for frontend work, screenshots and tests.

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// demoPassword is the console password in demo mode
const demoPassword = "glovebox"

// newDemoServer starts the fake backends and returns a server wired to them; dir holds fake files
func newDemoServer(ctx context.Context, dir, listen string) (*server, error) {
	zte := newFakeZTE()
	zteAddr, err := serveLocal(ctx, zte)
	if err != nil {
		return nil, err
	}
	mh := newFakeMihomo()
	mhAddr, err := serveLocal(ctx, mh)
	if err != nil {
		return nil, err
	}
	mixedAddr, err := serveLocal(ctx, http.HandlerFunc(mh.proxyHTTP))
	if err != nil {
		return nil, err
	}
	_, mixedPort, _ := net.SplitHostPort(mixedAddr)
	mh.mixedPort, _ = strconv.Atoi(mixedPort)
	// stands in for the Telegram Bot API and a Loki server
	sinkAddr, err := serveLocal(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	if err != nil {
		return nil, err
	}
	// the vendor web_settings.db stores login_pwd immediately followed by SHA256U(admin password)
	db := filepath.Join(dir, "web_settings.db")
	if err := os.WriteFile(db, []byte("SQLite format 3\x00...login_pwd"+sha256U("admin")+"..."), 0o600); err != nil {
		return nil, err
	}
	box := filepath.Join(dir, "box")
	if err := os.MkdirAll(filepath.Join(box, "run"), 0o700); err != nil {
		return nil, err
	}
	os.WriteFile(filepath.Join(box, "run/runs.log"), []byte(demoBoxLog()), 0o600)
	clashLog := filepath.Join(box, "run/clash.log")
	go mh.writeLog(ctx, clashLog)

	h, err := hashPassword(demoPassword)
	if err != nil {
		return nil, err
	}
	c := defaultConfig()
	c.Listen = []string{listen}
	c.TLSListen = nil
	c.AliasAddr = ""
	c.AllowCIDRs = []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}
	c.PasswordHash = h
	c.SessionSecret = hex.EncodeToString([]byte(time.Now().String()))
	c.ZTEBase = "http://" + zteAddr
	c.ZTEDB = db
	c.MihomoAPI = "http://" + mhAddr
	c.BoxDir = box
	c.TailscaleProxy = "Tailscale"
	c.TailscaleProbe = "http://100.100.100.100/"
	// there is no DNS listener to ask; the check only queries the fake mihomo
	c.DNSProbe = ""
	c.DeviceName = "demo"
	c.TelegramToken, c.TelegramChat = "demo", "demo"
	c.WhatsAppURL, c.WhatsAppChat = "http://"+sinkAddr, "15555550142"
	c.LogPushURL = "http://" + sinkAddr + "/insert/loki/api/v1/push"
	rel, err := newFakeRelease(time.Now(), c.DeviceName)
	if err != nil {
		return nil, err
	}
	relAddr, err := serveLocal(ctx, rel)
	if err != nil {
		return nil, err
	}
	c.UpdateURL = "http://" + relAddr + "/update.json"
	c.UpdateMode = "notify" // shows the install button rather than skipping an install
	cs := &configStore{c: c}

	s := newServer(cs, dir, dir)
	s.notify.telegramAPI = "http://" + sinkAddr
	s.demo = true
	s.update.demo = true
	s.update.extraKeys = []ed25519.PublicKey{rel.pub}
	s.net.synth = true
	start := time.Now()
	s.sys = func() sysInfo { return demoSys(start) }
	// prefill the histories so charts are full on first load
	for range historyLen {
		s.net.sampleSynth()
	}
	for i := sigHistoryLen; i > 0; i-- {
		rsrp, sinr := demoSignal(float64(-5 * i))
		s.sigHist = append(s.sigHist, sigSample{&rsrp, &sinr})
	}
	demoEvents(s.events, start)
	// the update panel starts out with a newer version found
	s.update.check(ctx)
	// a verification code arrives a little later, so the SMS panel and its forwarding have news
	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(20 * time.Second):
			zte.smsArrive("1065500", "Your WhatsApp code: 615-204. Don't share this code with others.")
		}
	}()
	// and the diagnostics panel with the results of a job, once the health checks ran
	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
			s.diag.poll(ctx, cs.get())
		}
	}()
	return s, nil
}

// fakeRelease serves a signed update.json and module zip, newer than any dev build, and a
// diagnostics job for device
type fakeRelease struct {
	pub      ed25519.PublicKey
	manifest []byte
	zip      []byte
	zipName  string
	diag     []byte
	diagPath string
	diagAt   time.Time
}

// demoDiagProbes is the job the demo runs: what the YouTube sign-in needs, with an upstream that
// fails HTTPS records
var demoDiagProbes = []diagProbe{
	{Kind: "dns", Name: "accounts.google.com"},
	{Kind: "dns", Name: "accounts.google.com", Type: "AAAA"},
	{Kind: "dns", Name: "accounts.google.com", Type: "HTTPS"},
	{Kind: "http", URL: "http://accounts.google.com/", Follow: true},
	{Kind: "http", URL: "http://www.youtube.com/generate_204"},
	{Kind: "delay", Proxy: "Proxy"},
	{Kind: "proxies"},
	{Kind: "connections", Match: "youtube"},
	{Kind: "health"},
	{Kind: "log", Level: "info", Minutes: 10},
}

func newFakeRelease(now time.Time, device string) (*fakeRelease, error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	ver := now.Add(24 * time.Hour).Format("20060102.1504")
	code := now.Unix()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"module.prop":  fmt.Sprintf("id=glovebox\nname=Glovebox\nversion=%s\nversionCode=%d\n", ver, code),
		"bin/glovebox": "#!/bin/sh\necho " + ver + "\n",
	} {
		w, err := zw.Create(name)
		if err != nil {
			return nil, err
		}
		io.WriteString(w, body)
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	r := &fakeRelease{pub: pub, zip: buf.Bytes(), zipName: "glovebox-" + ver + ".zip"}
	sum := sha256.Sum256(r.zip)
	m := manifest{Version: ver, VersionCode: code, ZipURL: r.zipName, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(r.zip)),
		Notes: "Demo release: faster proxy page, clearer health alerts."}
	m.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, m.signedText()))
	if r.manifest, err = json.Marshal(m); err != nil {
		return nil, err
	}
	j, err := signDiag(priv, "demo-"+now.Format("20060102-1504")+"-youtube", device, 12*time.Hour, demoDiagProbes, now)
	if err != nil {
		return nil, err
	}
	r.diagPath, r.diagAt = "/diag/"+device+".json", now.Truncate(time.Second)
	r.diag, err = json.Marshal(j)
	return r, err
}

func (r *fakeRelease) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/update.json":
		w.Header().Set("Content-Type", "application/json")
		w.Write(r.manifest)
	case "/" + r.zipName:
		w.Header().Set("Content-Type", "application/zip")
		w.Write(r.zip)
	case r.diagPath:
		w.Header().Set("Content-Type", "application/json")
		http.ServeContent(w, req, "", r.diagAt, bytes.NewReader(r.diag))
	default:
		http.NotFound(w, req)
	}
}

// demoEvents is the history of a drive: a cold start with the clock fixed by the network, a
// proxy outage in a tunnel, and a short Tailscale failure
func demoEvents(l *eventLog, now time.Time) {
	boot := now.Add(-3*time.Hour - 17*time.Minute)
	warn := func(at time.Time, msg string) string {
		return fmt.Sprintf("time=%q level=warning msg=%q", at.Format(time.RFC3339Nano), msg)
	}
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	for _, e := range []event{
		{Time: boot, Check: "started", State: "info", Msg: version, Dur: 41},
		{Time: boot.Add(38 * time.Second), Check: "clock_fixed", State: "info", Dur: 54_903_211},
		{Time: ago(2*time.Hour + 4*time.Minute), Check: "tailscale", State: "fail", Msg: "Timeout"},
		{Time: ago(2*time.Hour + time.Minute), Check: "tailscale", State: "ok", Dur: 180},
		{Time: ago(47 * time.Minute), Check: "proxy", State: "fail", Msg: "Proxy: Timeout", Log: []string{
			warn(ago(48*time.Minute), "[TCP] dial Proxy (match Match/) 192.168.0.143:51734 --> www.youtube.com:443 error: i/o timeout"),
			warn(ago(47*time.Minute+30*time.Second), "[UDP] dial Proxy (match Match/) 192.168.0.143:50012 --> 142.250.72.14:443 error: i/o timeout"),
		}},
		{Time: ago(44 * time.Minute), Check: "proxy", State: "ok", Dur: 190},
	} {
		l.add(e)
	}
}

func serveLocal(ctx context.Context, h http.Handler) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	go func() { <-ctx.Done(); srv.Close() }()
	return ln.Addr().String(), nil
}

// ---------- synthetic data ----------

func (n *netSampler) sampleSynth() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.synthT++
	t := n.synthT
	const mbps = 125000 // 1 Mbps = 125000 bytes/s
	down := 16 + 11*math.Sin(t/23) + 6*math.Sin(t/7.3) + 3*rand.Float64()
	if n.burst > 0 {
		n.burst--
		down += 55 + 25*rand.Float64()
	} else if rand.IntN(70) == 0 {
		n.burst = 4 + rand.IntN(8)
	}
	down = math.Max(down, 0.4)
	up := math.Max(1.4+0.9*math.Sin(t/11)+0.6*rand.Float64()+down*0.03, 0.1)
	r := netRates{
		WANDown: down * mbps,
		WANUp:   up * mbps,
		LANDown: down * mbps * 0.97,
		LANUp:   up * mbps * 1.03,
		CPU:     math.Min(8+down*0.25+4*rand.Float64(), 100),
		WANIfs:  []string{"sipa_eth0"},
	}
	n.cur = r
	n.push(sample{r.WANDown, r.WANUp, r.LANDown, r.LANUp, r.CPU})
}

func demoSys(start time.Time) sysInfo {
	t := time.Since(start).Seconds()
	capacity := min(int64(76+t/120), 100)
	status := "Charging"
	if capacity == 100 {
		status = "Full"
	}
	wave := math.Sin(t / 60)
	return sysInfo{
		Battery: battery{Present: true, Capacity: capacity, Status: status, TempC: 33.8 + wave, VoltageV: 4.12, External: true},
		Thermals: []thermal{
			{"soc-thermal", 47.2 + 2*wave + rand.Float64()},
			{"modem-thermal", 44.6 + 1.5*wave},
			{"board-thermal", 39.4 + wave},
			{"battery", 33.8 + wave},
		},
		Mem:    memInfo{TotalMB: 1858, AvailableMB: 900 + int64(40*wave)},
		Uptime: int64(t) + 3*3600 + 17*60,
		Load:   fmt.Sprintf("%.2f %.2f %.2f", 0.42+0.2*wave, 0.38, 0.31),
	}
}

func demoBoxLog() string {
	now := time.Now().Add(-3*time.Hour - 17*time.Minute)
	lines := []string{
		"[Info]: Box for Root starting",
		"[Info]: core: mihomo, network mode: tproxy",
		"[Info]: tproxy port 9898, dns port 1053",
		"[Info]: mihomo started, pid 2481",
		"[Info]: applying iptables rules",
		"[Info]: done",
	}
	var b strings.Builder
	for i, l := range lines {
		fmt.Fprintf(&b, "%s %s\n", now.Add(time.Duration(i)*time.Second).Format("15:04:05"), l)
	}
	return b.String()
}

func demoLogcat() string {
	now := time.Now()
	lines := []string{
		"W/WifiService( 1203): softap client count changed: 4",
		"W/ConnectivityService( 1203): NetworkAgentInfo [MOBILE (NR) - 101] validation passed",
		"E/ztemodem(  812): rssi poll timeout, retry",
		"W/hostapd( 2210): wlan1: STA aa:bb:cc:00:00:31 IEEE 802.11: disassociated",
		"W/hostapd( 2210): wlan1: STA aa:bb:cc:00:00:31 IEEE 802.11: associated",
		"W/BatteryService( 1203): charging at 9V input",
	}
	var b strings.Builder
	for i, l := range lines {
		fmt.Fprintf(&b, "%s %s\n", now.Add(time.Duration(i-len(lines))*41*time.Second).Format("01-02 15:04:05.000"), l)
	}
	return b.String()
}

// ---------- fake ZTE goform ----------

type fakeZTE struct {
	mu       sync.Mutex
	start    time.Time
	loggedIn bool
	aps      []map[string]any
	wifiOn   bool // queryWiFiModuleSwitch; one band is on at a time, like the F50
	sms      []fakeSMS
	smsID    int64
}

// fakeSMS is a message of the vendor store; tag 0 read, 1 unread, 2 sent, 3 failed
type fakeSMS struct {
	id          int64
	number, tag string
	text        string
	at          time.Time
}

func newFakeZTE() *fakeZTE {
	ap := func(chip, on, ssid string) map[string]any {
		return map[string]any{
			"ChipIndex": chip, "AccessPointIndex": "0", "AccessPointSwitchStatus": on,
			"SSID": ssid, "Password": base64.StdEncoding.EncodeToString([]byte("glovebox-demo")),
			"AuthMode": "WPA2PSK", "EncrypType": "", "ApBroadcastDisabled": "0",
			"ApMaxStationNumber": "10", "ApIsolate": "0", "QrImageShow": "1",
		}
	}
	f := &fakeZTE{start: time.Now(), wifiOn: true, aps: []map[string]any{ap("0", "0", "Glovebox"), ap("1", "1", "Glovebox-5G")}}
	now := time.Now()
	for _, m := range []fakeSMS{
		{number: "10099", tag: "0", text: "Data balance: 18.4 GB of 30 GB left this month. Reply CX for details.", at: now.Add(-50 * time.Hour)},
		{number: "15555550123", tag: "2", text: "On my way, see you at 7.", at: now.Add(-26 * time.Hour)},
		{number: "15555550123", tag: "0", text: "Great, I'll wait at the north entrance.", at: now.Add(-25 * time.Hour)},
		{number: "15555550188", tag: "3", text: "Parked at level B2, spot 114.", at: now.Add(-3 * time.Hour)},
		{number: "10690000", tag: "1", text: "G-482913 is your Google verification code.", at: now.Add(-14 * time.Minute)},
	} {
		f.smsID++
		m.id = f.smsID
		f.sms = append(f.sms, m)
	}
	return f
}

// smsArrive receives a message, as the modem would
func (f *fakeZTE) smsArrive(number, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.smsID++
	f.sms = append(f.sms, fakeSMS{id: f.smsID, number: number, tag: "1", text: text, at: time.Now()})
}

// smsJSON is the sms_data_total reply: newest first, base64 content, the date with its zone as
// the F50 writes it ("+0800")
func (f *fakeZTE) smsJSON(n int) []any {
	var l []any
	for i := len(f.sms) - 1; i >= 0 && len(l) < n; i-- {
		m := f.sms[i]
		_, off := m.at.Zone()
		sign := '+'
		if off < 0 {
			sign, off = '-', -off
		}
		l = append(l, map[string]any{
			"id": strconv.FormatInt(m.id, 10), "number": m.number, "tag": m.tag, "draft_group_id": "",
			"content": base64.StdEncoding.EncodeToString([]byte(m.text)),
			"date":    m.at.Format("06,01,02,15,04,05,") + fmt.Sprintf("%c%02d%02d", sign, off/3600, off%3600/60),
		})
	}
	return l
}

var demoStations = []map[string]any{
	{"hostname": "car-headunit", "ip_addr": "192.168.0.23", "mac_addr": "AA:BB:CC:00:00:23"},
	{"hostname": "phone", "ip_addr": "192.168.0.31", "mac_addr": "AA:BB:CC:00:00:31"},
	{"hostname": "laptop", "ip_addr": "192.168.0.39", "mac_addr": "AA:BB:CC:00:00:39"},
	{"hostname": "tablet", "ip_addr": "192.168.0.52", "mac_addr": "AA:BB:CC:00:00:52"},
}

// demoSignal returns a drifting RSRP (dBm) and SINR (dB) for t seconds after start, rounded like the vendor API
func demoSignal(t float64) (rsrp, sinr float64) {
	rsrp = -88 + 7*math.Sin(t/50) + 3*math.Sin(t/9) + 2*rand.Float64()
	sinr = 15 + 6*math.Sin(t/37) + 2*rand.Float64()
	return math.Round(rsrp), math.Round(sinr*10) / 10
}

func (f *fakeZTE) field(name string) any {
	t := time.Since(f.start).Seconds()
	rsrp, sinr := demoSignal(t)
	bar := 5
	switch {
	case rsrp < -110:
		bar = 1
	case rsrp < -100:
		bar = 2
	case rsrp < -95:
		bar = 3
	case rsrp < -85:
		bar = 4
	}
	switch name {
	case "loginfo":
		if f.loggedIn {
			return "ok"
		}
		return ""
	case "LD", "RD":
		b := make([]byte, 16)
		for i := range b {
			b[i] = byte(rand.IntN(256))
		}
		return strings.ToUpper(hex.EncodeToString(b))
	case "network_type":
		return "SA"
	case "signalbar":
		return strconv.Itoa(bar)
	case "network_provider", "network_provider_fullname", "spn_name_data":
		return "Demo Mobile"
	case "ppp_status":
		return "ppp_connected"
	case "modem_main_state":
		return "modem_init_complete"
	case "simcard_roam":
		return "Home"
	case "monthly_rx_bytes":
		return strconv.FormatInt(41_300_000_000+int64(t*2_100_000), 10)
	case "monthly_tx_bytes":
		return strconv.FormatInt(3_870_000_000+int64(t*180_000), 10)
	case "sms_unread_num":
		n := 0
		for _, m := range f.sms {
			if m.tag == "1" {
				n++
			}
		}
		return strconv.Itoa(n)
	case "sim_msisdn":
		return "15555550142"
	case "SSID1":
		return str(f.aps[1]["SSID"])
	case "wifi_onoff_state", "WiFiModuleSwitch":
		if f.wifiOn {
			return "1"
		}
		return "0"
	case "Z5g_rsrp":
		return strconv.FormatFloat(rsrp, 'f', 0, 64)
	case "Z5g_SINR", "Z5g_snr":
		return strconv.FormatFloat(sinr, 'f', 1, 64)
	case "nr5g_action_band":
		return "n78"
	case "wan_ipaddr":
		return "10.172.33.41"
	case "cr_version":
		return "DEMO_CR_1.0"
	case "wa_inner_version":
		return "DEMO_B09"
	case "station_list":
		return demoStations
	case "lan_station_list":
		return []any{}
	}
	return ""
}

func (f *fakeZTE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Referer") == "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]any{}
	switch r.URL.Path {
	case "/goform/goform_get_cmd_process":
		cmds := strings.Split(r.URL.Query().Get("cmd"), ",")
		if len(cmds) == 1 && cmds[0] == "sms_data_total" {
			if !f.loggedIn {
				out["messages"] = ""
				break
			}
			n, _ := strconv.Atoi(r.URL.Query().Get("data_per_page"))
			out["messages"] = f.smsJSON(max(n, 1))
			break
		}
		if len(cmds) == 1 && cmds[0] == "queryAccessPointInfo" {
			if !f.loggedIn {
				out["ResponseList"] = []any{}
				break
			}
			var l []any
			for _, ap := range f.aps {
				l = append(l, ap)
			}
			out["ResponseList"] = l
			break
		}
		for _, c := range cmds {
			if c == "queryWiFiModuleSwitch" {
				c = "WiFiModuleSwitch"
			}
			out[c] = f.field(c)
		}
	case "/goform/goform_set_cmd_process":
		r.ParseForm()
		switch r.PostForm.Get("goformId") {
		case "LOGIN":
			f.loggedIn = true
			out["result"] = "0"
		case "setAccessPointInfo":
			out["result"] = "failure"
			for _, ap := range f.aps {
				if str(ap["ChipIndex"]) != r.PostForm.Get("ChipIndex") {
					continue
				}
				for _, k := range []string{"SSID", "Password", "ApBroadcastDisabled", "ApMaxStationNumber"} {
					if v := r.PostForm.Get(k); v != "" {
						ap[k] = v
					}
				}
				out["result"] = "success"
			}
		case "switchWiFiChip":
			want := r.PostForm.Get("ChipEnum")
			out["result"] = "failure"
			for _, ap := range f.aps {
				if chipEnum(ap) == want {
					out["result"] = "success"
				}
			}
			if out["result"] != "success" {
				break
			}
			f.wifiOn = true
			for _, ap := range f.aps {
				ap["AccessPointSwitchStatus"] = "0"
				if chipEnum(ap) == want {
					ap["AccessPointSwitchStatus"] = "1"
				}
			}
		case "switchWiFiModule":
			f.wifiOn = r.PostForm.Get("SwitchOption") == "1"
			out["result"] = "success"
		case "SEND_SMS":
			text, ok := decodeUCS2(r.PostForm.Get("MessageBody"))
			if !ok || r.PostForm.Get("Number") == "" {
				out["result"] = "failure"
				break
			}
			f.smsID++
			// numbers starting with 000 fail, so the panel can show a failed message
			tag := "2"
			if strings.HasPrefix(r.PostForm.Get("Number"), "000") {
				tag = "3"
			}
			f.sms = append(f.sms, fakeSMS{id: f.smsID, number: r.PostForm.Get("Number"), tag: tag, text: text, at: time.Now()})
			out["result"] = "success"
		case "DELETE_SMS", "SET_MSG_READ":
			id, _ := strconv.ParseInt(r.PostForm.Get("msg_id"), 10, 64)
			out["result"] = "failure"
			for i := range f.sms {
				if f.sms[i].id != id {
					continue
				}
				if r.PostForm.Get("goformId") == "DELETE_SMS" {
					f.sms = append(f.sms[:i], f.sms[i+1:]...)
				} else if f.sms[i].tag == "1" {
					f.sms[i].tag = "0"
				}
				out["result"] = "success"
				break
			}
		default:
			out["result"] = "failure"
		}
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// ---------- fake mihomo ----------

type fakeNode struct {
	name, typ string
	base      int // typical delay (ms)
}

var demoNodes = []fakeNode{
	{"HK 01", "Vless", 62}, {"HK 02", "Hysteria2", 71}, {"JP 01", "Vless", 88}, {"JP 02", "Trojan", 97},
	{"SG 01", "Vless", 118}, {"TW 01", "Shadowsocks", 79}, {"US 01", "Hysteria2", 172},
	{"US 02", "Vless", 189}, {"DE 01", "Trojan", 236},
}

type fakeGroup struct {
	name, typ string
	all       []string
}

type fakeConn struct {
	host, src, srcPort, network, port, rule, payload string
	chains                                           []string
	up, down                                         int64
	start                                            time.Time
}

type fakeMihomo struct {
	mu      sync.Mutex
	start   time.Time
	mode    string
	groups  []fakeGroup
	now     map[string]string
	delays  map[string]int
	updated map[string]time.Time
	conns   []fakeConn
	closed  time.Time
	// mixed port: the diagnostics probes' requests, by source port
	mixedPort int
	proxied   map[string]fakeConn
	logLevel  string
}

func newFakeMihomo() *fakeMihomo {
	var nodes []string
	for _, n := range demoNodes {
		nodes = append(nodes, n.name)
	}
	m := &fakeMihomo{
		start: time.Now(),
		mode:  "rule",
		groups: []fakeGroup{
			{"Proxy", "Selector", append([]string{"Auto"}, append(nodes, "DIRECT")...)},
			{"Auto", "URLTest", nodes},
			{"Streaming", "Selector", []string{"Proxy", "HK 01", "JP 01", "SG 01", "US 01"}},
			{"AI", "Selector", []string{"US 01", "US 02", "JP 01", "Proxy"}},
			{"Tailnet", "Selector", []string{"Tailscale", "DIRECT"}},
			{"Final", "Selector", []string{"Proxy", "DIRECT"}},
		},
		now:      map[string]string{"Proxy": "Auto", "Auto": "HK 01", "Streaming": "JP 01", "AI": "US 01", "Tailnet": "Tailscale", "Final": "Proxy"},
		delays:   map[string]int{},
		updated:  map[string]time.Time{},
		proxied:  map[string]fakeConn{},
		logLevel: "warning",
	}
	for _, n := range demoNodes {
		m.delays[n.name] = n.base + rand.IntN(30)
	}
	m.delays["DE 01"] = 0
	for _, p := range []string{"demo-sub", "reject", "direct-cn", "proxy-domains"} {
		m.updated[p] = time.Now().Add(-time.Duration(2+rand.IntN(20)) * time.Hour)
	}
	return m
}

func (m *fakeMihomo) group(name string) *fakeGroup {
	for i := range m.groups {
		if m.groups[i].name == name {
			return &m.groups[i]
		}
	}
	return nil
}

func (m *fakeMihomo) nodeBase(name string) (fakeNode, bool) {
	for _, n := range demoNodes {
		if n.name == name {
			return n, true
		}
	}
	return fakeNode{}, false
}

// testDelay tests a node or group (a group uses its selected node); 0 means timeout
func (m *fakeMihomo) testDelay(name string, depth int) int {
	if depth > 5 {
		return 0
	}
	if g := m.group(name); g != nil {
		return m.testDelay(m.now[name], depth+1)
	}
	switch name {
	case "DIRECT":
		return 18 + rand.IntN(10)
	case "Tailscale":
		return 32 + rand.IntN(20)
	}
	n, ok := m.nodeBase(name)
	if !ok || rand.IntN(12) == 0 {
		m.delays[name] = 0
		return 0
	}
	d := n.base + rand.IntN(n.base/2+10)
	m.delays[name] = d
	return d
}

func delayHistory(d int) []map[string]any {
	if d < 0 {
		return []map[string]any{}
	}
	return []map[string]any{{"time": time.Now().Format(time.RFC3339), "delay": d}}
}

func (m *fakeMihomo) nodeJSON(n fakeNode) map[string]any {
	d := m.delays[n.name]
	return map[string]any{"name": n.name, "type": n.typ, "alive": d > 0, "history": delayHistory(d), "udp": true}
}

func (m *fakeMihomo) proxiesJSON() map[string]any {
	ps := map[string]any{}
	for name, typ := range map[string]string{"DIRECT": "Direct", "REJECT": "Reject"} {
		ps[name] = map[string]any{"name": name, "type": typ, "alive": true, "history": []any{}}
	}
	ps["Tailscale"] = map[string]any{"name": "Tailscale", "type": "Tailscale", "alive": true, "history": delayHistory(38)}
	var all []string
	for _, g := range m.groups {
		if g.typ == "URLTest" {
			// URLTest picks the lowest delay
			best, bd := g.all[0], math.MaxInt
			for _, n := range g.all {
				if d := m.delays[n]; d > 0 && d < bd {
					best, bd = n, d
				}
			}
			m.now[g.name] = best
		}
		ps[g.name] = map[string]any{"name": g.name, "type": g.typ, "now": m.now[g.name], "all": g.all, "alive": true, "history": []any{}}
		all = append(all, g.name)
	}
	all = append(all, "DIRECT", "REJECT", "Tailscale")
	ps["GLOBAL"] = map[string]any{"name": "GLOBAL", "type": "Selector", "now": "Proxy", "all": all, "hidden": true, "history": []any{}}
	return ps
}

func (m *fakeMihomo) connsJSON() map[string]any {
	t := time.Since(m.start).Seconds()
	if len(m.conns) == 0 && time.Since(m.closed) > 3*time.Second {
		m.conns = demoConns()
	}
	var l []map[string]any
	conns := slices.Clone(m.conns)
	for _, c := range m.proxied {
		conns = append(conns, c)
	}
	for i := range conns {
		c := &conns[i]
		if i < len(m.conns) {
			c = &m.conns[i]
			c.down += int64(rand.IntN(400_000))
			c.up += int64(rand.IntN(30_000))
		}
		l = append(l, map[string]any{
			"metadata": map[string]any{
				"network": c.network, "sourceIP": c.src, "sourcePort": c.srcPort, "host": c.host,
				"destinationIP": "203.0.113." + strconv.Itoa(10+i), "destinationPort": c.port, "sniffHost": "",
			},
			"upload": c.up, "download": c.down, "start": c.start.Format(time.RFC3339Nano),
			"chains": c.chains, "rule": c.rule, "rulePayload": c.payload,
		})
	}
	return map[string]any{
		"downloadTotal": 9_820_000_000 + int64(t*2_000_000),
		"uploadTotal":   612_000_000 + int64(t*160_000),
		"memory":        48_300_000 + rand.IntN(2_000_000),
		"connections":   l,
	}
}

func demoConns() []fakeConn {
	type spec struct {
		host, port, network, rule, payload string
		chains                             []string
	}
	specs := []spec{
		{"www.youtube.com", "443", "tcp", "RuleSet", "proxy-domains", []string{"JP 01", "Streaming"}},
		{"rr4---sn.googlevideo.com", "443", "udp", "RuleSet", "proxy-domains", []string{"JP 01", "Streaming"}},
		{"api.openai.com", "443", "tcp", "DomainSuffix", "openai.com", []string{"US 01", "AI"}},
		{"github.com", "443", "tcp", "RuleSet", "proxy-domains", []string{"HK 01", "Auto", "Proxy"}},
		{"www.google.com", "443", "tcp", "RuleSet", "proxy-domains", []string{"HK 01", "Auto", "Proxy"}},
		{"api.spotify.com", "443", "tcp", "Match", "", []string{"HK 01", "Auto", "Proxy", "Final"}},
		{"cdn.jsdelivr.net", "443", "tcp", "RuleSet", "direct-cn", []string{"DIRECT"}},
		{"weather.example.net", "443", "tcp", "RuleSet", "direct-cn", []string{"DIRECT"}},
		{"router.tailnet.example", "80", "tcp", "IPCIDR", "100.64.0.0/10", []string{"Tailscale", "Tailnet"}},
		{"news.ycombinator.com", "443", "tcp", "Match", "", []string{"HK 01", "Auto", "Proxy", "Final"}},
		{"en.wikipedia.org", "443", "tcp", "RuleSet", "proxy-domains", []string{"HK 01", "Auto", "Proxy"}},
		{"time.example.org", "123", "udp", "GeoIP", "LAN", []string{"DIRECT"}},
	}
	var out []fakeConn
	for i, s := range specs {
		out = append(out, fakeConn{
			host: s.host, src: str(demoStations[i%len(demoStations)]["ip_addr"]), srcPort: strconv.Itoa(40000 + rand.IntN(20000)),
			network: s.network, port: s.port,
			rule: s.rule, payload: s.payload, chains: s.chains,
			up: int64(rand.IntN(2_000_000)), down: int64(rand.IntN(80_000_000)),
			start: time.Now().Add(-time.Duration(rand.IntN(1800)) * time.Second),
		})
	}
	return out
}

func (m *fakeMihomo) providersJSON(kind string) map[string]any {
	ps := map[string]any{}
	if kind == "proxies" {
		var nodes []any
		for _, n := range demoNodes {
			nodes = append(nodes, m.nodeJSON(n))
		}
		ps["demo-sub"] = map[string]any{"name": "demo-sub", "type": "Proxy", "vehicleType": "HTTP", "updatedAt": m.updated["demo-sub"], "proxies": nodes}
		ps["default"] = map[string]any{"name": "default", "type": "Proxy", "vehicleType": "Compatible", "proxies": []any{}}
		return map[string]any{"providers": ps}
	}
	for name, n := range map[string]int{"reject": 148_212, "direct-cn": 113_860, "proxy-domains": 27_514} {
		ps[name] = map[string]any{"name": name, "type": "Rule", "behavior": "domain", "vehicleType": "HTTP", "ruleCount": n, "updatedAt": m.updated[name]}
	}
	return map[string]any{"providers": ps}
}

func (m *fakeMihomo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	fail := func(code int, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"message": msg})
	}
	parts := strings.Split(strings.Trim(r.URL.EscapedPath(), "/"), "/")
	for i := range parts {
		parts[i], _ = url.PathUnescape(parts[i])
	}
	switch {
	case r.Method == http.MethodGet && parts[0] == "version":
		reply(map[string]any{"meta": true, "version": "v1.19.32-demo"})
	case r.Method == http.MethodGet && parts[0] == "dns" && len(parts) == 2 && parts[1] == "query":
		reply(fakeDNSAnswer(r.URL.Query().Get("name"), r.URL.Query().Get("type")))
	case parts[0] == "configs" && r.Method == http.MethodGet:
		reply(map[string]any{"mode": m.mode, "tproxy-port": 9898, "mixed-port": m.mixedPort, "log-level": m.logLevel})
	case parts[0] == "configs" && r.Method == http.MethodPatch:
		var in struct {
			Mode     string
			LogLevel string `json:"log-level"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if in.Mode != "" {
			m.mode = in.Mode
		}
		if in.LogLevel != "" {
			m.logLevel = in.LogLevel
		}
		w.WriteHeader(http.StatusNoContent)
	case parts[0] == "proxies" && len(parts) == 1:
		reply(map[string]any{"proxies": m.proxiesJSON()})
	case parts[0] == "proxies" && len(parts) == 2 && r.Method == http.MethodPut:
		g := m.group(parts[1])
		var in struct{ Name string }
		json.NewDecoder(r.Body).Decode(&in)
		if g == nil || g.typ != "Selector" {
			fail(http.StatusBadRequest, "Must be a Selector")
			return
		}
		for _, n := range g.all {
			if n == in.Name {
				m.now[g.name] = n
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		fail(http.StatusBadRequest, "Proxy does not exist")
	case parts[0] == "proxies" && len(parts) == 3 && parts[2] == "delay":
		if d := m.testDelay(parts[1], 0); d > 0 {
			reply(map[string]int{"delay": d})
		} else {
			fail(http.StatusGatewayTimeout, "Timeout")
		}
	case parts[0] == "group" && len(parts) == 3 && parts[2] == "delay":
		g := m.group(parts[1])
		if g == nil {
			fail(http.StatusNotFound, "resource not found")
			return
		}
		out := map[string]int{}
		for _, n := range g.all {
			if d := m.testDelay(n, 0); d > 0 {
				out[n] = d
			}
		}
		reply(out)
	case parts[0] == "providers" && len(parts) == 2:
		reply(m.providersJSON(parts[1]))
	case parts[0] == "providers" && len(parts) == 3 && r.Method == http.MethodPut:
		m.updated[parts[2]] = time.Now()
		w.WriteHeader(http.StatusNoContent)
	case parts[0] == "connections" && r.Method == http.MethodGet:
		reply(m.connsJSON())
	case parts[0] == "connections" && r.Method == http.MethodDelete:
		m.conns, m.closed = nil, time.Now()
		w.WriteHeader(http.StatusNoContent)
	default:
		fail(http.StatusNotFound, "resource not found")
	}
}

// fakeDNSAnswer resolves every name but the random ones the health check asks for; HTTPS records
// fail as with an upstream that does not know the type
func fakeDNSAnswer(name, qtype string) map[string]any {
	q := []any{map[string]any{"Name": name + ".", "Qtype": 1}}
	switch {
	case strings.HasPrefix(name, "glovebox-"):
		return map[string]any{"Status": 3, "Question": q}
	case qtype == "HTTPS":
		return map[string]any{"Status": 2, "Question": q}
	}
	var ans []any
	if qtype == "" || qtype == "A" {
		h := 0
		for _, c := range name {
			h = (h*31 + int(c)) % 200
		}
		ans = append(ans, map[string]any{"name": name + ".", "type": 1, "TTL": 300, "data": "203.0.113." + strconv.Itoa(20+h%200)})
	}
	return map[string]any{"Status": 0, "Question": q, "Answer": ans}
}

// proxyHTTP stands in for mihomo's mixed port: plain http requests get a canned answer, and the
// connection shows in /connections like a real one
func (m *fakeMihomo) proxyHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect || r.URL.Host == "" {
		http.Error(w, "demo proxy: plain http only", http.StatusBadGateway)
		return
	}
	_, port, _ := net.SplitHostPort(r.RemoteAddr)
	m.mu.Lock()
	m.proxied[port] = fakeConn{host: r.URL.Hostname(), src: "127.0.0.1", srcPort: port, network: "tcp", port: "80",
		rule: "RuleSet", payload: "proxy-domains", chains: []string{"HK 01", "Auto", "Proxy"}, start: time.Now()}
	m.mu.Unlock()
	time.AfterFunc(5*time.Second, func() {
		m.mu.Lock()
		delete(m.proxied, port)
		m.mu.Unlock()
	})
	w.Header().Set("Server", "demo")
	switch r.URL.Path {
	case "/generate_204":
		w.WriteHeader(http.StatusNoContent)
	case "/":
		w.Header().Set("Location", "http://"+r.URL.Host+"/ServiceLogin")
		w.WriteHeader(http.StatusFound)
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, "<!doctype html><title>Sign in</title>")
	}
}

// writeLog simulates mihomo writing connection logs to clash.log
func (m *fakeMihomo) writeLog(ctx context.Context, path string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		log.Printf("demo log: %v", err)
		return
	}
	defer f.Close()
	line := func(at time.Time, level, msg string) {
		fmt.Fprintf(f, "time=%q level=%s msg=%q\n", at.Format(time.RFC3339Nano), level, msg)
	}
	at := time.Now().Add(-3 * time.Minute)
	line(at, "info", "Start initial configuration in progress")
	line(at, "info", "Initial configuration complete, total time: 812ms")
	line(at, "info", "RESTful API listening at: 127.0.0.1:9091")
	line(at, "info", "TProxy server listening at: [::]:9898")
	line(at, "warning", "[Provider] demo-sub not updated for a long time, force refresh")
	t := time.NewTicker(1500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			m.mu.Lock()
			conns := demoConns()
			m.mu.Unlock()
			sort.Slice(conns, func(i, j int) bool { return rand.IntN(2) == 0 })
			c := conns[0]
			switch rand.IntN(14) {
			case 0:
				line(now, "warning", fmt.Sprintf("[TCP] dial %s (match %s/%s) %s:%d --> %s:%s error: i/o timeout",
					c.chains[len(c.chains)-1], c.rule, c.payload, c.src, 40000+rand.IntN(20000), c.host, c.port))
			case 1:
				line(now, "debug", fmt.Sprintf("[DNS] %s --> [203.0.113.%d] A from fakeip", c.host, rand.IntN(200)))
			default:
				rule := c.rule
				if c.payload != "" {
					rule += "(" + c.payload + ")"
				}
				line(now, "info", fmt.Sprintf("[%s] %s:%d --> %s:%s match %s using %s",
					strings.ToUpper(c.network), c.src, 40000+rand.IntN(20000), c.host, c.port, rule, strings.Join(reverse(c.chains), "[")+strings.Repeat("]", len(c.chains)-1)))
			}
		}
	}
}

func reverse(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}
