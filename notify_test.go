package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

const testToken = "123456:secret-token"

// fakeTelegram records sendMessage calls; reply is the description of an error to return
type fakeTelegram struct {
	mu    sync.Mutex
	texts []string
	reply string
}

func (f *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var in struct {
		ChatID    string `json:"chat_id"`
		Text      string `json:"text"`
		ParseMode string `json:"parse_mode"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	switch {
	case r.URL.Path != "/bot"+testToken+"/sendMessage":
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "Not Found"})
	case f.reply != "":
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": f.reply})
	case in.ChatID != "42" || in.ParseMode != "HTML":
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "bad request"})
	default:
		f.texts = append(f.texts, in.Text)
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}

func (f *fakeTelegram) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.texts
	f.texts = nil
	return t
}

func newTestNotifier(t *testing.T, dataDir string, edit func(*Config)) (*notifier, *fakeTelegram) {
	t.Helper()
	tg := &fakeTelegram{}
	srv := httptest.NewServer(tg)
	t.Cleanup(srv.Close)
	h, _ := newTestHealth(t, "")
	c := h.s.cfg.get()
	c.DeviceName = "car"
	c.TelegramToken, c.TelegramChat = testToken, "42"
	if edit != nil {
		edit(&c)
	}
	n := newNotifier(&configStore{c: c}, h.events, h, &outbound{}, dataDir)
	n.telegramAPI = srv.URL
	return n, tg
}

func TestTelegramAlerts(t *testing.T) {
	dir := t.TempDir()
	n, tg := newTestNotifier(t, dir, nil)
	ch := n.chans[0]
	ctx := context.Background()

	n.events.add(event{Check: "started", State: "info"})
	n.events.add(event{Check: "dns", State: "fail", Msg: "<b>x</b> & y", Log: []string{`msg="dial a<b"`}})
	if err := n.flush(ctx, ch); err != nil {
		t.Fatal(err)
	}
	texts := tg.take()
	if len(texts) != 1 {
		t.Fatalf("%d messages", len(texts))
	}
	for _, want := range []string{"<b>car</b>", "DNS failed", "<code>&lt;b&gt;x&lt;/b&gt; &amp; y</code>", "<pre>msg=&#34;dial a&lt;b&#34;</pre>"} {
		if !strings.Contains(texts[0], want) {
			t.Errorf("message lacks %q:\n%s", want, texts[0])
		}
	}
	if strings.Contains(texts[0], "started") {
		t.Errorf("lifecycle event sent to Telegram:\n%s", texts[0])
	}

	// undelivered alerts are kept and sent together once Telegram works again
	tg.reply = "Bad Gateway"
	n.events.add(event{Check: "dns", State: "ok", Dur: 95})
	if err := n.flush(ctx, ch); err == nil || !strings.Contains(err.Error(), "Bad Gateway") {
		t.Fatalf("flush error %v", err)
	}
	if st := n.status()["telegram"]; st.Pending != 1 || st.Error == "" {
		t.Fatalf("status %+v", st)
	}
	n.events.add(event{Check: "proxy", State: "fail", Msg: "Timeout"})
	tg.reply = ""
	if err := n.flush(ctx, ch); err != nil {
		t.Fatal(err)
	}
	texts = tg.take()
	if len(texts) != 1 || !strings.Contains(texts[0], "DNS recovered after 1m") || !strings.Contains(texts[0], "Proxy failed") {
		t.Fatalf("catch-up messages: %q", texts)
	}
	if st := n.status()["telegram"]; st.Pending != 0 || st.Error != "" || st.LastOK.IsZero() {
		t.Fatalf("status after delivery %+v", st)
	}

	// the cursor is persisted: after a restart only new alerts are sent
	n2 := newNotifier(n.cfg, n.events, n.health, &outbound{}, dir)
	n2.telegramAPI = n.telegramAPI
	n.events.add(event{Check: "proxy", State: "ok", Dur: 30})
	if err := n2.flush(ctx, n2.chans[0]); err != nil {
		t.Fatal(err)
	}
	if texts = tg.take(); len(texts) != 1 || strings.Contains(texts[0], "DNS") || !strings.Contains(texts[0], "Proxy recovered") {
		t.Fatalf("after restart: %q", texts)
	}
}

func TestTelegramTranslatedAndScrubbed(t *testing.T) {
	n, tg := newTestNotifier(t, "", func(c *Config) { c.ReportLang = "zh-CN" })
	n.events.add(event{Check: "wan", State: "ok", Dur: 3700})
	if err := n.flush(context.Background(), n.chans[0]); err != nil {
		t.Fatal(err)
	}
	if texts := tg.take(); len(texts) != 1 || !strings.Contains(texts[0], tr("zh-CN", "hc.wan", nil)) || !strings.Contains(texts[0], "1h 1m") {
		t.Fatalf("translated message: %q", texts)
	}

	// the token is part of the URL and must not show up in errors
	n.telegramAPI = "http://127.0.0.1:1"
	n.events.add(event{Check: "dns", State: "fail"})
	err := n.flush(context.Background(), n.chans[0])
	if err == nil || strings.Contains(err.Error(), "secret-token") || strings.Contains(n.status()["telegram"].Error, "secret-token") {
		t.Fatalf("error leaks the token: %v", err)
	}
}

func TestTelegramBatching(t *testing.T) {
	n, tg := newTestNotifier(t, "", nil)
	long := strings.Repeat("timeout ", 40)
	logs := []string{strings.Repeat("w", 290), strings.Repeat("v", 290)}
	for range notifyQueueMax + 10 {
		n.events.add(event{Check: "proxy", State: "fail", Msg: long, Log: logs})
	}
	if err := n.flush(context.Background(), n.chans[0]); err != nil {
		t.Fatal(err)
	}
	texts := tg.take()
	total := 0
	for _, s := range texts {
		if utf8.RuneCountInString(s) > telegramMaxRunes {
			t.Fatalf("message of %d characters", utf8.RuneCountInString(s))
		}
		total += strings.Count(s, "Proxy failed")
	}
	if total != notifyQueueMax || len(texts) < 2 {
		t.Fatalf("%d alerts in %d messages", total, len(texts))
	}
	if !strings.Contains(texts[0], "10 older alerts were dropped") || strings.Contains(texts[1], "dropped") {
		t.Fatalf("drop notice: %q / %q", texts[0][:200], texts[1][:200])
	}

	// a single huge event still fits
	text, used := telegramText(n.cfg.get(), []event{{Check: "dns", State: "fail", Msg: strings.Repeat("x", 10000)}}, 0)
	if used != 1 || utf8.RuneCountInString(text) > telegramMaxRunes {
		t.Fatalf("huge event: used %d, %d characters", used, utf8.RuneCountInString(text))
	}
}

func TestWebhook(t *testing.T) {
	var mu sync.Mutex
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	}))
	defer srv.Close()
	n, _ := newTestNotifier(t, "", func(c *Config) { c.TelegramToken, c.WebhookURL = "", srv.URL+"/hook?key=abc" })
	n.events.add(event{Check: "started", State: "info", Msg: "v1", Dur: 30})
	n.events.add(event{Check: "memory", State: "fail", Msg: "60 MB of 1400 MB available"})
	if err := n.flush(context.Background(), n.chans[1]); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0]["check"] != "started" || got[1]["state"] != "fail" || got[1]["device"] != "car" ||
		got[1]["text"] != "Memory failed" || got[0]["text"] != "Glovebox v1 started, system up 30s" {
		t.Fatalf("webhook bodies: %v", got)
	}
	if res := n.test(context.Background()); len(res) != 1 || res["webhook"] != nil || got[2]["state"] != "test" {
		t.Fatalf("test: %v %v", res, got)
	}
}

func TestOutboundProxies(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "target") }))
	defer target.Close()

	var mu sync.Mutex
	var seen []string
	// an HTTP proxy receives the absolute URL
	httpProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, "http "+r.URL.String())
		mu.Unlock()
		io.WriteString(w, "via proxy")
	}))
	defer httpProxy.Close()
	hc, err := newOutboundClient(httpProxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	if body := fetchBody(t, hc, target.URL+"/x"); body != "via proxy" || len(seen) != 1 || seen[0] != "http "+target.URL+"/x" {
		t.Fatalf("http proxy: %q %v", body, seen)
	}

	socks := socks5Server(t, func(dst string) {
		mu.Lock()
		seen = append(seen, "socks "+dst)
		mu.Unlock()
	})
	hc, err = newOutboundClient("socks5://" + socks)
	if err != nil {
		t.Fatal(err)
	}
	if body := fetchBody(t, hc, target.URL); body != "target" || seen[1] != "socks "+strings.TrimPrefix(target.URL, "http://") {
		t.Fatalf("socks5 proxy: %q %v", body, seen)
	}

	for _, bad := range []string{"ftp://x:1", "127.0.0.1:7890", "http://"} {
		if _, err := newOutboundClient(bad); err == nil {
			t.Errorf("report_proxy %q accepted", bad)
		}
	}
}

func fetchBody(t *testing.T, hc *http.Client, u string) string {
	t.Helper()
	resp, err := hc.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// socks5Server is a minimal SOCKS5 proxy (no authentication, CONNECT only)
func socks5Server(t *testing.T, connect func(dst string)) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 262)
				if _, err := io.ReadFull(c, buf[:2]); err != nil {
					return
				}
				io.ReadFull(c, buf[:buf[1]])
				c.Write([]byte{5, 0})
				if _, err := io.ReadFull(c, buf[:4]); err != nil {
					return
				}
				var host string
				switch buf[3] {
				case 1:
					io.ReadFull(c, buf[:4])
					host = net.IP(buf[:4]).String()
				case 3:
					io.ReadFull(c, buf[:1])
					l := int(buf[0])
					io.ReadFull(c, buf[:l])
					host = string(buf[:l])
				default:
					return
				}
				io.ReadFull(c, buf[:2])
				dst := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(buf[:2]))))
				connect(dst)
				up, err := net.Dial("tcp", dst)
				if err != nil {
					c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer up.Close()
				c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				go io.Copy(up, c)
				io.Copy(c, up)
			}()
		}
	}()
	return ln.Addr().String()
}
