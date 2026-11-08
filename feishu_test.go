package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const feishuPath = "/open-apis/bot/v2/hook/fs-token"

// fakeFeishu is a custom bot with signature verification; reply replaces the success response
type fakeFeishu struct {
	mu     sync.Mutex
	secret string
	cards  []map[string]any
	reply  string
}

func (f *fakeFeishu) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var in struct {
		Timestamp string         `json:"timestamp"`
		Sign      string         `json:"sign"`
		MsgType   string         `json:"msg_type"`
		Card      map[string]any `json:"card"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	ts, _ := strconv.ParseInt(in.Timestamp, 10, 64)
	switch {
	case f.reply != "":
		io.WriteString(w, f.reply)
	case r.URL.Path != feishuPath:
		w.WriteHeader(http.StatusNotFound)
	case f.secret != "" && (in.Sign != feishuSign(f.secret, ts) || time.Since(time.Unix(ts, 0)).Abs() > time.Hour):
		io.WriteString(w, `{"code":19021,"msg":"sign match fail or timestamp is not within one hour from current time"}`)
	case in.MsgType != "interactive" || in.Card["schema"] != "2.0":
		io.WriteString(w, `{"code":9499,"msg":"Bad Request","data":{}}`)
	default:
		f.cards = append(f.cards, in.Card)
		io.WriteString(w, `{"StatusCode":0,"StatusMessage":"success","code":0,"data":{},"msg":"success"}`)
	}
}

func (f *fakeFeishu) take() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cards
	f.cards = nil
	return c
}

// cardText collects the title, subtitle, summary and every text block of a card, one per line
func cardText(card map[string]any) (template, text string) {
	var lines []string
	h := card["header"].(map[string]any)
	lines = append(lines, h["title"].(map[string]any)["content"].(string))
	if s, ok := h["subtitle"].(map[string]any); ok {
		lines = append(lines, s["content"].(string))
	}
	lines = append(lines, card["config"].(map[string]any)["summary"].(map[string]any)["content"].(string))
	for _, el := range card["body"].(map[string]any)["elements"].([]any) {
		t := el.(map[string]any)["text"].(map[string]any)
		lines = append(lines, t["content"].(string))
	}
	return h["template"].(string), strings.Join(lines, "\n")
}

func feishuChannel(t *testing.T, n *notifier) *channel {
	t.Helper()
	for _, ch := range n.chans {
		if ch.name == "feishu" {
			return ch
		}
	}
	t.Fatal("no feishu channel")
	return nil
}

func TestFeishu(t *testing.T) {
	bot := &fakeFeishu{secret: "fs-secret"}
	srv := httptest.NewServer(bot)
	t.Cleanup(srv.Close)
	n, _ := newTestNotifier(t, "", func(c *Config) {
		c.FeishuWebhook, c.FeishuSecret = srv.URL+feishuPath, "fs-secret"
	})
	ch := feishuChannel(t, n)
	ctx := context.Background()
	at := time.Date(2026, 11, 7, 10, 30, 5, 0, time.Local)

	n.events.add(event{Check: "started", State: "info"})
	n.events.add(event{Check: "dns", State: "fail", Msg: "<b>x</b> & y", Log: []string{`msg="dial a<b"`}})
	n.events.add(event{Check: "sms", State: "info", Msg: "1065500",
		SMS: &smsEvent{ID: 6, From: "1065500", Text: "Your WhatsApp code: 615-204 <b>", Code: "615204", At: at}})
	n.events.add(event{Check: "rescue", State: "info", Msg: "glovebox"})
	if err := n.flush(ctx, ch); err != nil {
		t.Fatal(err)
	}
	cards := bot.take()
	if len(cards) != 3 {
		t.Fatalf("%d cards, want dns, sms and rescue", len(cards))
	}

	tpl, text := cardText(cards[0])
	for _, want := range []string{"\U0001F534 DNS failed", "car · ", "<b>x</b> & y", `msg="dial a<b"`} {
		if !strings.Contains(text, want) {
			t.Errorf("alert card lacks %q:\n%s", want, text)
		}
	}
	if tpl != "red" {
		t.Errorf("alert template %q", tpl)
	}

	// the code leads the title, which is also the summary the chat list and notifications show
	tpl, text = cardText(cards[1])
	if !strings.HasPrefix(text, "\U0001F510 615204\n\U0001F4E9 SMS from 1065500\n\U0001F510 615204 · \U0001F4E9 SMS from 1065500\ncar · 11-07 10:30:05\nYour WhatsApp code: 615-204 <b>") || tpl != "wathet" {
		t.Errorf("SMS card %s:\n%s", tpl, text)
	}
	if tpl, text = cardText(cards[2]); tpl != "orange" || !strings.Contains(text, "glovebox") {
		t.Errorf("rescue card %s:\n%s", tpl, text)
	}

	// sms_forward picks whether SMS go to Feishu; alerts always do
	c := n.cfg.get()
	c.SMSForward = []string{"telegram"}
	if ch.wants(c, event{Check: "sms", State: "info"}) || !ch.wants(c, event{Check: "dns", State: "fail"}) || ch.wants(c, event{Check: "started", State: "info"}) {
		t.Error("Feishu takes the wrong events")
	}

	// alerts that pile up share a card, an SMS breaks them up; lost alerts are noted once
	n.events.add(event{Check: "dns", State: "ok", Dur: 95})
	n.events.add(event{Check: "proxy", State: "ok", Dur: 30})
	n.events.add(event{Check: "sms", State: "info", Msg: "10086", SMS: &smsEvent{From: "10086", Text: "hi"}})
	n.events.add(event{Check: "proxy", State: "fail", Msg: "Timeout"})
	ch.dropped = 3
	if err := n.flush(ctx, ch); err != nil {
		t.Fatal(err)
	}
	cards = bot.take()
	if len(cards) != 3 {
		t.Fatalf("%d cards, want 2 alerts, the SMS, 1 alert", len(cards))
	}
	note := tr("en", "alert.dropped", map[string]string{"n": "3"})
	tpl, text = cardText(cards[0])
	if tpl != "green" || !strings.HasPrefix(text, "2 alerts\n") || !strings.Contains(text, note) ||
		!strings.Contains(text, "DNS recovered after 1m") || !strings.Contains(text, "Proxy recovered after 30s") {
		t.Errorf("batched card %s:\n%s", tpl, text)
	}
	if _, text = cardText(cards[1]); !strings.HasPrefix(text, "\U0001F4E9 SMS from 10086") || strings.Contains(text, note) {
		t.Errorf("SMS card:\n%s", text)
	}
	if tpl, text = cardText(cards[2]); tpl != "red" || !strings.Contains(text, "Proxy failed") {
		t.Errorf("last card %s:\n%s", tpl, text)
	}
}

func TestFeishuErrors(t *testing.T) {
	bot := &fakeFeishu{secret: "fs-secret"}
	srv := httptest.NewServer(bot)
	t.Cleanup(srv.Close)
	c := defaultConfig()
	c.FeishuWebhook = srv.URL + feishuPath
	evs := []event{{Check: "dns", State: "fail", Time: time.Now()}}
	ctx := context.Background()

	// the bot checks signatures but no secret is set
	sent, err := sendFeishu(ctx, http.DefaultClient, c, evs, 0)
	if sent != 0 || err == nil || !strings.Contains(err.Error(), "code 19021") || strings.Contains(err.Error(), "fs-token") {
		t.Errorf("unsigned: %d, %v", sent, err)
	}
	c.FeishuSecret = "fs-secret"
	if sent, err = sendFeishu(ctx, http.DefaultClient, c, evs, 0); sent != 1 || err != nil {
		t.Errorf("signed: %d, %v", sent, err)
	}
	// rate limited, a proxy's error page and the bare old response
	for reply, ok := range map[string]bool{
		`{"code":11232,"msg":"frequency limited"}`:   false,
		`<html>502 Bad Gateway</html>`:               false,
		`{"StatusCode":0,"StatusMessage":"success"}`: true,
	} {
		bot.reply = reply
		_, err := sendFeishu(ctx, http.DefaultClient, c, evs, 0)
		if (err == nil) != ok {
			t.Errorf("reply %s: %v", reply, err)
		}
	}
	// the token in the webhook stays out of transport errors
	srv.Close()
	if _, err := sendFeishu(ctx, http.DefaultClient, c, evs, 0); err == nil || strings.Contains(err.Error(), "fs-token") {
		t.Errorf("transport error %v", err)
	}
}

func TestFeishuSign(t *testing.T) {
	// computed independently: base64(HMAC-SHA256(key "1599360473\ns3cret", message ""))
	if got := feishuSign("s3cret", 1599360473); got != "QQHOl6fWr0/7EF26jw8goDL6Cc0vHLHZYLDituMJB4s=" {
		t.Errorf("sign %q", got)
	}
}

func TestFeishuCardCutsLongText(t *testing.T) {
	c := defaultConfig()
	long := strings.Repeat("\u9a8c", 5000) // three bytes in UTF-8
	evs := []event{
		{Check: "sms", State: "info", SMS: &smsEvent{From: "1", Text: long}},
		{Check: "dns", State: "fail", Msg: long, Log: []string{long}},
		{Check: "dns", State: "fail", Msg: long, Log: []string{strings.Repeat("<", 9000)}},
		{Check: "proxy", State: "fail", Msg: long, Log: []string{long}},
	}
	for len(evs) > 0 {
		card, used := feishuNext(c, evs, 7)
		if b := feishuJSON(map[string]any{"msg_type": "interactive", "card": card}); len(b) > 20<<10 || used < 1 {
			t.Fatalf("request is %d bytes for %d events, Feishu takes 20 KB", len(b), used)
		}
		evs = evs[used:]
	}
}
