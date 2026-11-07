package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDecodeSMSText(t *testing.T) {
	for in, want := range map[string]string{
		base64.StdEncoding.EncodeToString([]byte("G-482913 is your Google verification code.")): "G-482913 is your Google verification code.",
		// base64 without padding, and UTF-8 beyond ASCII
		strings.TrimRight(base64.StdEncoding.EncodeToString([]byte("\u9a8c\u8bc1\u7801 1234")), "="): "\u9a8c\u8bc1\u7801 1234",
		// UCS-2 hex, as older ZTE firmware sends it
		"9A8C8BC17801003A003600350034003300320031": "\u9a8c\u8bc1\u7801:654321",
		"not base64!": "not base64!",
		"":            "",
	} {
		if got := decodeSMSText(in); got != want {
			t.Errorf("decodeSMSText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEncodeUCS2(t *testing.T) {
	if got := encodeUCS2("Hi \U0001F600"); got != "004800690020D83DDE00" {
		t.Errorf("encodeUCS2 = %s", got)
	}
	if got, ok := decodeUCS2(encodeUCS2("Hi \U0001F600 \u4f60\u597d")); !ok || got != "Hi \U0001F600 \u4f60\u597d" {
		t.Errorf("round trip: %q %v", got, ok)
	}
}

func TestParseSMSDate(t *testing.T) {
	got := parseSMSDate("26,11,07,10,30,05,+32")
	if want := time.Date(2026, 11, 7, 10, 30, 5, 0, time.FixedZone("", 8*3600)); !got.Equal(want) {
		t.Errorf("with zone: %v, want %v", got, want)
	}
	if got := parseSMSDate("26,11,07,10,30,05"); !got.Equal(time.Date(2026, 11, 7, 10, 30, 5, 0, time.Local)) {
		t.Errorf("without zone: %v", got)
	}
	// what the F50 sends
	for _, z := range []string{"+0800", "+08:00", "+800"} {
		got := parseSMSDate("26,11,08,02,38,38," + z)
		if want := time.Date(2026, 11, 8, 2, 38, 38, 0, time.FixedZone("", 8*3600)); !got.Equal(want) {
			t.Errorf("%s: %v, want %v", z, got, want)
		}
		if _, err := json.Marshal(got); err != nil {
			t.Errorf("%s: %v", z, err)
		}
	}
	if got := parseSMSDate("26,11,08,02,38,38,-0530"); !got.Equal(time.Date(2026, 11, 8, 2, 38, 38, 0, time.FixedZone("", -(5*3600+30*60)))) {
		t.Errorf("-0530: %v", got)
	}
	// a zone no place uses falls back to local time instead of a time JSON cannot encode
	for _, z := range []string{"+2500", "+99", "+08:75", "x"} {
		got := parseSMSDate("26,11,08,02,38,38," + z)
		if !got.Equal(time.Date(2026, 11, 8, 2, 38, 38, 0, time.Local)) {
			t.Errorf("%s: %v", z, got)
		}
		if _, err := json.Marshal(got); err != nil {
			t.Errorf("%s: %v", z, err)
		}
	}
	for _, bad := range []string{"", "26,11,07", "a,b,c,d,e,f,+32"} {
		if !parseSMSDate(bad).IsZero() {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestSMSCode(t *testing.T) {
	for text, want := range map[string]string{
		"\u3010\u94f6\u884c\u3011\u9a8c\u8bc1\u7801\uff1a654321\uff0c5\u5206\u949f\u5185\u6709\u6548\u3002\u5982\u6709\u7591\u95ee\u8bf7\u81f4\u753595588": "654321",
		"\u60a8\u7684\u9a8c\u8bc1\u7801\u662f 12345678":                           "12345678",
		"G-482913 is your Google verification code.":                              "482913",
		"Your WhatsApp code: 615-204. Don't share this code with others.":         "615204",
		"Telegram code: 52814":                                                    "52814",
		"Your OTP for login is 7731. Valid for 10 min.":                           "7731",
		"Data balance: 18.4 GB of 30 GB left this month. Reply 1234 for details.": "",
		"Meeting moved to 1530 tomorrow":                                          "",
		"Your verification link: https://example.com/v?id=9":                      "",
	} {
		if got := smsCode(text); got != want {
			t.Errorf("smsCode(%q) = %q, want %q", text, got, want)
		}
	}
}

// newFakeVendor serves the demo's fake vendor API with a login hash file next to it
func newFakeVendor(t *testing.T) (*fakeZTE, *zteClient) {
	t.Helper()
	f := newFakeZTE()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	db := filepath.Join(t.TempDir(), "web_settings.db")
	os.WriteFile(db, []byte("SQLite format 3\x00...login_pwd"+sha256U("admin")+"..."), 0o600)
	return f, newZTE(srv.URL, db)
}

func smsEvents(l *eventLog) []event {
	var out []event
	for _, e := range l.since(0) {
		if e.Check == "sms" {
			out = append(out, e)
		}
	}
	return out
}

func TestSMSWatcher(t *testing.T) {
	f, z := newFakeVendor(t)
	dir := t.TempDir()
	events := openEventLog("")
	w := newSMSWatcher(&configStore{c: defaultConfig()}, z, events, nil, dir)
	ctx := context.Background()

	// the first read only takes note of what is there: old messages are not forwarded
	w.tick(ctx)
	if evs := smsEvents(events); len(evs) != 0 {
		t.Fatalf("old messages forwarded: %+v", evs)
	}
	f.smsArrive("1065500", "Your WhatsApp code: 615-204. Don't share this code with others.")
	w.tick(ctx)
	evs := smsEvents(events)
	if len(evs) != 1 {
		t.Fatalf("%d events", len(evs))
	}
	if e := evs[0]; e.Msg != "1065500" || e.State != "info" || e.SMS == nil || e.SMS.Code != "615204" ||
		!strings.HasPrefix(e.SMS.Text, "Your WhatsApp code") || e.SMS.At.IsZero() {
		t.Errorf("event %+v %+v", e, e.SMS)
	}
	w.tick(ctx)
	if len(smsEvents(events)) != 1 {
		t.Error("a message was forwarded twice")
	}

	// a message sent from the device is not forwarded
	if err := z.smsSend(ctx, "15555550123", "hello"); err != nil {
		t.Fatal(err)
	}
	w.tick(ctx)
	if len(smsEvents(events)) != 1 {
		t.Error("a sent message was forwarded")
	}

	// the position survives a restart
	f.smsArrive("10690000", "first")
	f.smsArrive("10690000", "second")
	w2 := newSMSWatcher(w.cfg, z, events, nil, dir)
	w2.tick(ctx)
	evs = smsEvents(events)
	if len(evs) != 3 || evs[1].SMS.Text != "first" || evs[2].SMS.Text != "second" {
		t.Fatalf("after restart: %d events", len(evs))
	}

	// an emptied store (new SIM, factory reset) starts over without forwarding
	f.mu.Lock()
	f.sms, f.smsID = nil, 1
	f.sms = append(f.sms, fakeSMS{id: 1, number: "10086", tag: "1", text: "welcome", at: time.Now()})
	f.mu.Unlock()
	w2.tick(ctx)
	if len(smsEvents(events)) != 3 {
		t.Error("forwarded after the store was reset")
	}
	if w2.st.LastID != 1 {
		t.Errorf("LastID %d after reset", w2.st.LastID)
	}
}

// fakeWAHA records sendText calls
type fakeWAHA struct {
	mu     sync.Mutex
	bodies []map[string]string
	key    string
	fail   bool
}

func (f *fakeWAHA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail || r.URL.Path != "/api/sendText" || r.Header.Get("X-Api-Key") != "waha-key" {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"message":"bad key waha-key"}`)
		return
	}
	var in map[string]string
	json.NewDecoder(r.Body).Decode(&in)
	f.bodies = append(f.bodies, in)
	w.WriteHeader(http.StatusCreated)
	io.WriteString(w, `{"id":"true_15555550142@c.us_ABC"}`)
}

func TestSMSForwarding(t *testing.T) {
	waha := &fakeWAHA{}
	wsrv := httptest.NewServer(waha)
	t.Cleanup(wsrv.Close)
	var hooks []map[string]any
	var hookMu sync.Mutex
	hsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		hookMu.Lock()
		hooks = append(hooks, in)
		hookMu.Unlock()
	}))
	t.Cleanup(hsrv.Close)
	n, tg := newTestNotifier(t, "", func(c *Config) {
		c.WhatsAppURL, c.WhatsAppKey, c.WhatsAppChat = wsrv.URL+"/", "waha-key", "+1 555 555 0142"
		c.WebhookURL = hsrv.URL
	})
	ctx := context.Background()
	at := time.Date(2026, 11, 7, 10, 30, 5, 0, time.Local)
	n.events.add(event{Check: "dns", State: "fail", Msg: "timeout"})
	n.events.add(event{Check: "sms", State: "info", Msg: "1065500",
		SMS: &smsEvent{ID: 6, From: "1065500", Text: "Your WhatsApp code: 615-204 <b>", Code: "615204", At: at}})
	for _, ch := range n.chans {
		if err := n.flush(ctx, ch); err != nil {
			t.Fatalf("%s: %v", ch.name, err)
		}
	}

	texts := tg.take()
	if len(texts) != 1 {
		t.Fatalf("%d Telegram messages", len(texts))
	}
	for _, want := range []string{"SMS from 1065500", "11-07 10:30:05", "\n<code>615204</code>", "<blockquote>Your WhatsApp code: 615-204 &lt;b&gt;</blockquote>", "DNS failed"} {
		if !strings.Contains(texts[0], want) {
			t.Errorf("Telegram message lacks %q:\n%s", want, texts[0])
		}
	}

	// WhatsApp only gets the SMS, code first
	if len(waha.bodies) != 1 {
		t.Fatalf("%d WhatsApp messages", len(waha.bodies))
	}
	b := waha.bodies[0]
	if b["chatId"] != "15555550142@c.us" || b["session"] != "default" || !strings.HasPrefix(b["text"], "\U0001F510 615204\n") ||
		!strings.Contains(b["text"], "Your WhatsApp code: 615-204 <b>") || !strings.Contains(b["text"], "car") {
		t.Errorf("WhatsApp body %q", b)
	}

	hookMu.Lock()
	if len(hooks) != 2 || hooks[1]["check"] != "sms" || hooks[1]["sms"].(map[string]any)["code"] != "615204" {
		t.Errorf("webhook %v", hooks)
	}
	hookMu.Unlock()

	// sms_forward picks the channels
	c := n.cfg.get()
	c.SMSForward = []string{"whatsapp"}
	sms := event{Check: "sms", State: "info"}
	if n.chans[0].wants(c, sms) || !n.chans[2].wants(c, sms) || n.chans[1].wants(c, sms) || !n.chans[1].wants(c, event{Check: "dns"}) {
		t.Error("sms_forward is not followed")
	}
	if n.chans[2].wants(c, event{Check: "dns", State: "fail"}) {
		t.Error("WhatsApp gets health alerts")
	}

	// the key stays out of errors
	waha.fail = true
	_, err := sendWhatsApp(ctx, http.DefaultClient, n.cfg.get(), []event{{Check: "sms", SMS: &smsEvent{Text: "x"}}}, 0)
	if err == nil || strings.Contains(err.Error(), "waha-key") || !strings.Contains(err.Error(), "401") {
		t.Errorf("error %v", err)
	}
}

func TestWhatsAppChatID(t *testing.T) {
	for in, want := range map[string]string{
		"+86 138 0000 0000": "8613800000000@c.us",
		"15555550142":       "15555550142@c.us",
		"120363000000@g.us": "120363000000@g.us",
		"15555550142@c.us":  "15555550142@c.us",
	} {
		if got := whatsAppChatID(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestShipLeavesSMSTextOut(t *testing.T) {
	e := newShipEnv(t)
	sh := e.shipper()
	e.h.events.add(event{Check: "sms", State: "info", Msg: "1065500", SMS: &smsEvent{From: "1065500", Text: "code 615204", Code: "615204"}})
	e.round(sh)
	lines := e.loki.take("events")
	if len(lines) != 1 || strings.Contains(lines[0], "615204") || !strings.Contains(lines[0], `"_msg":"sms info: 1065500"`) {
		t.Fatalf("shipped %q", lines)
	}
	// the event on the device keeps the text
	if evs := smsEvents(e.h.events); len(evs) != 1 || evs[0].SMS == nil || evs[0].SMS.Code != "615204" {
		t.Error("shipping changed the stored event")
	}
}

func TestHealthEventsLeaveSMSOut(t *testing.T) {
	evs := healthEvents([]event{{Check: "dns"}, {Check: "sms"}, {Check: "proxy"}})
	if len(evs) != 2 || evs[0].Check != "dns" || evs[1].Check != "proxy" {
		t.Errorf("%+v", evs)
	}
}

func TestSMSAPI(t *testing.T) {
	c := newTestClient(t)
	if code, _ := c.do("POST", "/api/login", `{"password":"`+demoPassword+`"}`, true); code != http.StatusOK {
		t.Fatal("login")
	}
	code, st := c.do("GET", "/api/sms", "", false)
	msgs, _ := st["messages"].([]any)
	if code != http.StatusOK || len(msgs) == 0 || st["number"] != "15555550142" || st["unread"].(float64) != 1 {
		t.Fatalf("list: %d %v", code, st)
	}
	for body, want := range map[string]string{
		`{"number":"abc","text":"hi"}`:                                                "sms_number",
		`{"number":"15555550123","text":"  "}`:                                        "sms_text",
		`{"number":"15555550123","text":"` + strings.Repeat("x", smsTextMax+1) + `"}`: "sms_text",
	} {
		if code, out := c.do("POST", "/api/sms/send", body, true); code != http.StatusBadRequest || out["code"] != want {
			t.Errorf("%.60s: %d %v", body, code, out)
		}
	}
	if code, out := c.do("POST", "/api/sms/send", `{"number":"+1 555 555 0123","text":"On my way"}`, true); code != http.StatusOK {
		t.Fatalf("send: %d %v", code, out)
	}
	_, st = c.do("GET", "/api/sms", "", false)
	first := st["messages"].([]any)[0].(map[string]any)
	if first["state"] != "sent" || first["number"] != "+15555550123" || first["text"] != "On my way" {
		t.Errorf("sent message %v", first)
	}
	// mark the unread message read, then delete the sent one
	var unread float64
	for _, m := range st["messages"].([]any) {
		if mm := m.(map[string]any); mm["state"] == "unread" {
			unread = mm["id"].(float64)
		}
	}
	if code, out := c.do("POST", "/api/sms/read", `{"ids":[`+jsonNum(unread)+`]}`, true); code != http.StatusOK {
		t.Fatalf("read: %d %v", code, out)
	}
	if code, out := c.do("POST", "/api/sms/delete", `{"ids":[`+jsonNum(first["id"].(float64))+`]}`, true); code != http.StatusOK {
		t.Fatalf("delete: %d %v", code, out)
	}
	_, st = c.do("GET", "/api/sms", "", false)
	if st["unread"].(float64) != 0 || st["messages"].([]any)[0].(map[string]any)["id"] == first["id"] {
		t.Errorf("after read and delete: %v", st)
	}
	if code, out := c.do("POST", "/api/sms/delete", `{"ids":[]}`, true); code != http.StatusBadRequest || out["code"] != "sms_ids" {
		t.Errorf("no ids: %d %v", code, out)
	}
}

func jsonNum(f float64) string {
	b, _ := json.Marshal(int64(f))
	return string(b)
}
