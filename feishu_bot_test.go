package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testAppID     = "cli_test"
	testAppSecret = "app-secret-123"
)

// sentCard is a message the bot sent or updated through the Open Platform
type sentCard struct {
	Method, To, IDType string
	Card               map[string]any
}

// fakeLark is the Feishu Open Platform and its long connection
type fakeLark struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	tokens int
	// refuse the next message call with an expired token
	expire bool
	// answer the token request with this error message instead
	tokenErr string
	sent     chan sentCard
	peers    chan *wsPeer
	pings    chan *feishuFrame
	acks     chan *feishuFrame
	nextID   int
}

func newFakeLark(t *testing.T) *fakeLark {
	f := &fakeLark{t: t, sent: make(chan sentCard, 20), peers: make(chan *wsPeer, 4),
		pings: make(chan *feishuFrame, 20), acks: make(chan *feishuFrame, 20)}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLark) reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (f *fakeLark) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&in)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
		if f.tokenErr != "" {
			f.reply(w, map[string]any{"code": 10014, "msg": f.tokenErr})
			return
		}
		if in["app_id"] != testAppID || in["app_secret"] != testAppSecret {
			f.reply(w, map[string]any{"code": 10014, "msg": "app secret invalid"})
			return
		}
		f.tokens++
		f.reply(w, map[string]any{"code": 0, "tenant_access_token": fmt.Sprintf("t-%d", f.tokens), "expire": 7200})
	case r.URL.Path == "/callback/ws/endpoint":
		if in["AppID"] != testAppID || in["AppSecret"] != testAppSecret {
			f.reply(w, map[string]any{"code": 1000040343, "msg": "bad app"})
			return
		}
		u := "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/ws?device_id=d1&service_id=7"
		f.reply(w, map[string]any{"code": 0, "data": map[string]any{"URL": u, "ClientConfig": map[string]int{"PingInterval": 120}}})
	case r.URL.Path == "/ws":
		f.mu.Unlock()
		p, err := wsAccept(w, r)
		f.mu.Lock()
		if err == nil {
			go f.readPeer(p)
			f.peers <- p
		}
	case strings.HasPrefix(r.URL.Path, "/open-apis/im/v1/messages"):
		if r.Header.Get("Authorization") != fmt.Sprintf("Bearer t-%d", f.tokens) || f.expire {
			f.expire = false
			f.reply(w, map[string]any{"code": 99991663, "msg": "Invalid access token for authorization"})
			return
		}
		var card map[string]any
		json.Unmarshal([]byte(in["content"]), &card)
		sc := sentCard{Method: r.Method, To: in["receive_id"], IDType: r.URL.Query().Get("receive_id_type"), Card: card}
		if r.Method == http.MethodPatch {
			sc.To = strings.TrimPrefix(r.URL.Path, "/open-apis/im/v1/messages/")
		}
		f.sent <- sc
		f.nextID++
		f.reply(w, map[string]any{"code": 0, "data": map[string]any{"message_id": fmt.Sprintf("om_%d", f.nextID)}})
	default:
		http.NotFound(w, r)
	}
}

// readPeer answers pings with a pong and collects the acknowledgements
func (f *fakeLark) readPeer(p *wsPeer) {
	for {
		op, data, err := p.readFrame()
		if err != nil || op == wsClose {
			return
		}
		fr, err := unmarshalFeishuFrame(data)
		if err != nil {
			f.t.Errorf("bad frame from the bot: %v", err)
			return
		}
		if fr.Method == feishuControl {
			f.pings <- fr
			pong := feishuFrame{Service: fr.Service, Method: feishuControl, Headers: []feishuHeader{{"type", "pong"}}, Payload: []byte(`{"PingInterval":90}`)}
			p.writeFrame(true, wsBinary, pong.marshal())
			continue
		}
		f.acks <- fr
	}
}

func (f *fakeLark) url() string { return f.srv.URL }

var eventSeq int

// event sends an event down the connection, in parts when sum > 1, and returns its id
func (f *fakeLark) event(p *wsPeer, typ string, ev any, created time.Time, sum int) string {
	eventSeq++
	id := fmt.Sprintf("ev_%d", eventSeq)
	ct := strconv.FormatInt(created.UnixMilli(), 10)
	if typ == "card.action.trigger" {
		ct = strconv.FormatInt(created.UnixMicro(), 10)
	}
	payload, _ := json.Marshal(map[string]any{"schema": "2.0",
		"header": map[string]string{"event_id": id, "event_type": typ, "create_time": ct}, "event": ev})
	size := (len(payload) + sum - 1) / sum
	for i := 0; i < sum; i++ {
		part := payload[i*size : min((i+1)*size, len(payload))]
		fr := feishuFrame{SeqID: uint64(eventSeq), Service: 7, Method: feishuData, Payload: part,
			Headers: []feishuHeader{{"type", "event"}, {"message_id", "m" + id}, {"sum", strconv.Itoa(sum)}, {"seq", strconv.Itoa(i)}, {"trace_id", "tr"}}}
		if err := p.writeFrame(true, wsBinary, fr.marshal()); err != nil {
			f.t.Fatal(err)
		}
	}
	return id
}

func (f *fakeLark) message(p *wsPeer, from, text string) string {
	content, _ := json.Marshal(map[string]string{"text": text})
	return f.event(p, "im.message.receive_v1", map[string]any{
		"sender":  map[string]any{"sender_id": map[string]string{"open_id": from}, "sender_type": "user"},
		"message": map[string]any{"message_id": "om_in", "chat_id": "oc_" + from, "chat_type": "p2p", "message_type": "text", "content": string(content)},
	}, time.Now(), 1)
}

func (f *fakeLark) action(p *wsPeer, from string, value map[string]string, option string, form map[string]string, created time.Time) {
	f.event(p, "card.action.trigger", map[string]any{
		"operator": map[string]string{"open_id": from},
		"action":   map[string]any{"value": value, "tag": "button", "option": option, "form_value": form},
		"context":  map[string]string{"open_message_id": "om_card", "open_chat_id": "oc_" + from},
	}, created, 1)
}

func (f *fakeLark) nextSent(t *testing.T) sentCard {
	t.Helper()
	select {
	case s := <-f.sent:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("the bot sent nothing")
	}
	return sentCard{}
}

// ack waits for the acknowledgement of an event and returns the answer it carries
func (f *fakeLark) ack(t *testing.T) map[string]any {
	t.Helper()
	select {
	case fr := <-f.acks:
		if fr.header("biz_rt") == "" || fr.header("trace_id") != "tr" {
			t.Errorf("ack headers %+v", fr.Headers)
		}
		var resp struct {
			Code int    `json:"code"`
			Data []byte `json:"data"`
		}
		if err := json.Unmarshal(fr.Payload, &resp); err != nil || resp.Code != 200 {
			t.Fatalf("ack %s: %v", fr.Payload, err)
		}
		var ans map[string]any
		if len(resp.Data) > 0 {
			json.Unmarshal(resp.Data, &ans)
		}
		return ans
	case <-time.After(5 * time.Second):
		t.Fatal("no acknowledgement")
	}
	return nil
}

func title(card map[string]any) string {
	h, _ := card["header"].(map[string]any)
	tl, _ := h["title"].(map[string]any)
	s, _ := tl["content"].(string)
	return s
}

func cardJSON(card map[string]any) string {
	b, _ := json.Marshal(card)
	return string(b)
}

func ansCard(ans map[string]any) map[string]any {
	c, _ := ans["card"].(map[string]any)
	d, _ := c["data"].(map[string]any)
	return d
}

func ansToast(ans map[string]any) (string, string) {
	tt, _ := ans["toast"].(map[string]any)
	k, _ := tt["type"].(string)
	s, _ := tt["content"].(string)
	return k, s
}

// findValue returns the callback value of the first button or menu whose value has a=action
func findValue(card map[string]any, action string) map[string]string {
	// cards built in Go hold typed maps and slices; look at them as JSON does
	b, _ := json.Marshal(card)
	card = nil
	json.Unmarshal(b, &card)
	var found map[string]string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if x["type"] == "callback" {
				if val, ok := x["value"].(map[string]any); ok && val["a"] == action && found == nil {
					found = map[string]string{}
					for k, v := range val {
						found[k], _ = v.(string)
					}
				}
			}
			for _, v := range x {
				walk(v)
			}
		case []any:
			for _, v := range x {
				walk(v)
			}
		}
	}
	walk(card)
	return found
}

func newTestBot(t *testing.T, owners ...string) (*server, *fakeLark, *wsPeer) {
	t.Helper()
	oldBackoff, oldBudget, oldSettle := feishuBackoffMin, feishuAnswerBudget, feishuBoxSettle
	feishuBackoffMin, feishuAnswerBudget, feishuBoxSettle = 50*time.Millisecond, 300*time.Millisecond, 0
	t.Cleanup(func() { feishuBackoffMin, feishuAnswerBudget, feishuBoxSettle = oldBackoff, oldBudget, oldSettle })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s, err := newDemoServer(ctx, demoDir(t), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeLark(t)
	s.cfg.update(func(c *Config) {
		c.FeishuAppID, c.FeishuAppSecret, c.FeishuDomain, c.FeishuBotUsers = testAppID, testAppSecret, f.url(), owners
	})
	go s.bot.loop(ctx)
	var p *wsPeer
	select {
	case p = <-f.peers:
	case <-time.After(5 * time.Second):
		t.Fatalf("the bot did not connect: %v", s.bot.status())
	}
	select {
	case ping := <-f.pings:
		if ping.Service != 7 || ping.header("type") != "ping" {
			t.Fatalf("ping %+v", ping)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no ping")
	}
	return s, f, p
}

func TestFeishuBotBind(t *testing.T) {
	s, f, p := newTestBot(t)
	st := s.bot.status()
	code, _ := st["bind"].(string)
	if st["connected"] != true || len(code) != 6 {
		t.Fatalf("status %v", st)
	}

	// strangers learn their id and how to bind, nothing else
	f.message(p, "ou_stranger", "status")
	f.ack(t)
	got := f.nextSent(t)
	if got.IDType != "chat_id" || got.To != "oc_ou_stranger" || title(got.Card) != "Not authorized" || !strings.Contains(cardJSON(got.Card), "ou_stranger") {
		t.Fatalf("stranger got %+v", got)
	}
	f.message(p, "ou_stranger", "bind 000000x")
	f.ack(t)
	if got = f.nextSent(t); !strings.Contains(cardJSON(got.Card), "wrong") {
		t.Fatalf("wrong code: %s", cardJSON(got.Card))
	}

	// the right code in Chinese binds the sender
	f.message(p, "ou_owner", "\u7ed1\u5b9a "+code)
	f.ack(t)
	if got = f.nextSent(t); !strings.Contains(cardJSON(got.Card), "Bound") {
		t.Fatalf("bind: %s", cardJSON(got.Card))
	}
	if c := s.cfg.get(); len(c.FeishuBotUsers) != 1 || c.FeishuBotUsers[0] != "ou_owner" {
		t.Fatalf("owners %v", c.FeishuBotUsers)
	}
	if _, ok := s.bot.status()["bind"]; ok {
		t.Error("binding code shown after binding")
	}
	// nobody else can bind now
	f.message(p, "ou_stranger", "bind "+code)
	f.ack(t)
	if got = f.nextSent(t); title(got.Card) != "Not authorized" {
		t.Fatalf("second bind: %s", cardJSON(got.Card))
	}
}

func TestFeishuBotBindCodeChanges(t *testing.T) {
	s, f, p := newTestBot(t)
	code := s.bot.status()["bind"].(string)
	for range feishuBindTries {
		f.message(p, "ou_x", "bind 1")
		f.ack(t)
		f.nextSent(t)
	}
	if again := s.bot.status()["bind"].(string); again == code {
		t.Error("the code survived wrong guesses")
	}
	f.message(p, "ou_x", "bind "+code)
	f.ack(t)
	if got := f.nextSent(t); !strings.Contains(cardJSON(got.Card), "wrong") {
		t.Errorf("old code bound: %s", cardJSON(got.Card))
	}
}

func TestFeishuBotCommands(t *testing.T) {
	s, f, p := newTestBot(t, "ou_owner")
	for text, want := range map[string]string{
		"status":         "Status · demo",
		"/STATUS":        "Status · demo",
		"\u72b6\u6001":   "Status · demo", // the Chinese word
		"proxy":          "Proxy",
		"\u8282\u70b9":   "Proxy",
		"sms":            "SMS",
		"device":         "Device · demo",
		"what":           "Glovebox · demo",
		"sms 10086 hi":   "Send to 10086?",
		"sms abc hi":     "SMS",
		"\u77ed\u4fe1 1": "SMS",
	} {
		f.message(p, "ou_owner", text)
		f.ack(t)
		got := f.nextSent(t)
		if title(got.Card) != want {
			t.Errorf("%q: %q, want %q", text, title(got.Card), want)
		}
	}
	// the status card reads the device
	f.message(p, "ou_owner", "status")
	f.ack(t)
	card := cardJSON(f.nextSent(t).Card)
	for _, want := range []string{"RSRP", "this month", "on the hotspot", "Node ", "Glovebox dev"} {
		if !strings.Contains(card, want) {
			t.Errorf("status card lacks %q: %s", want, card)
		}
	}
	// a redelivered event runs once
	content, _ := json.Marshal(map[string]string{"text": "status"})
	ev := map[string]any{
		"sender":  map[string]any{"sender_id": map[string]string{"open_id": "ou_owner"}, "sender_type": "user"},
		"message": map[string]any{"chat_id": "oc_x", "chat_type": "p2p", "message_type": "text", "content": string(content)},
	}
	payload, _ := json.Marshal(map[string]any{"header": map[string]string{"event_id": "dup", "event_type": "im.message.receive_v1",
		"create_time": strconv.FormatInt(time.Now().UnixMilli(), 10)}, "event": ev})
	for range 2 {
		fr := feishuFrame{Service: 7, Method: feishuData, Payload: payload, Headers: []feishuHeader{{"type", "event"}, {"trace_id", "tr"}}}
		p.writeFrame(true, wsBinary, fr.marshal())
		f.ack(t)
	}
	f.nextSent(t)
	select {
	case got := <-f.sent:
		t.Errorf("redelivery answered: %s", title(got.Card))
	case <-time.After(300 * time.Millisecond):
	}
	// group chats and old messages are ignored
	f.event(p, "im.message.receive_v1", map[string]any{
		"sender":  map[string]any{"sender_id": map[string]string{"open_id": "ou_owner"}, "sender_type": "user"},
		"message": map[string]any{"chat_id": "oc_g", "chat_type": "group", "message_type": "text", "content": string(content)},
	}, time.Now(), 1)
	f.ack(t)
	f.event(p, "im.message.receive_v1", ev, time.Now().Add(-10*time.Minute), 1)
	f.ack(t)
	// an event in three parts
	f.event(p, "im.message.receive_v1", ev, time.Now(), 3)
	f.ack(t)
	if got := f.nextSent(t); title(got.Card) != "Status · demo" {
		t.Errorf("reassembled event: %q", title(got.Card))
	}
	select {
	case got := <-f.sent:
		t.Errorf("ignored message answered: %s", title(got.Card))
	case <-time.After(300 * time.Millisecond):
	}
	_ = s
}

func TestFeishuBotActions(t *testing.T) {
	s, f, p := newTestBot(t, "ou_owner")
	now := time.Now()

	// switch node from the proxy card's menu, then the mode
	f.action(p, "ou_owner", map[string]string{"a": "proxy"}, "", nil, now)
	card := ansCard(f.ack(t))
	sel := findValue(card, "select")
	if title(card) != "Proxy" || sel["g"] == "" {
		t.Fatalf("proxy card %s", cardJSON(card))
	}
	groups, _, _ := s.proxyGroups(context.Background())
	target := groups[0].Nodes[len(groups[0].Nodes)-1].Name
	f.action(p, "ou_owner", sel, target, nil, now)
	ans := f.ack(t)
	if k, msg := ansToast(ans); k != "success" || !strings.Contains(msg, target) {
		t.Fatalf("select toast %s %s", k, msg)
	}
	if groups, _, _ = s.proxyGroups(context.Background()); groups[0].Now != target {
		t.Fatalf("now %q, want %q", groups[0].Now, target)
	}
	f.action(p, "ou_owner", map[string]string{"a": "mode", "m": "global"}, "", nil, now)
	f.ack(t)
	if _, mode, _ := s.proxyGroups(context.Background()); mode != "global" {
		t.Fatalf("mode %v", mode)
	}

	// a click from a minute ago changes nothing, and neither does a stranger's
	f.action(p, "ou_owner", map[string]string{"a": "mode", "m": "direct"}, "", nil, now.Add(-2*time.Minute))
	if k, _ := ansToast(f.ack(t)); k != "warning" {
		t.Errorf("stale click toast %q", k)
	}
	f.action(p, "ou_stranger", map[string]string{"a": "mode", "m": "direct"}, "", nil, now)
	if k, _ := ansToast(f.ack(t)); k != "error" {
		t.Errorf("stranger toast %q", k)
	}
	if _, mode, _ := s.proxyGroups(context.Background()); mode != "global" {
		t.Fatalf("mode changed to %v", mode)
	}

	// rebooting asks first; an expired confirmation does nothing
	f.action(p, "ou_owner", map[string]string{"a": "reboot"}, "", nil, now)
	confirm := findValue(ansCard(f.ack(t)), "reboot")
	if confirm["ok"] != "1" || confirm["exp"] == "" {
		t.Fatalf("confirmation %v", confirm)
	}
	old := map[string]string{"a": "reboot", "ok": "1", "exp": strconv.FormatInt(now.Add(-time.Second).Unix(), 10)}
	f.action(p, "ou_owner", old, "", nil, now)
	if k, _ := ansToast(f.ack(t)); k != "warning" {
		t.Errorf("expired confirmation toast %q", k)
	}
	f.action(p, "ou_owner", confirm, "", nil, now)
	if c := ansCard(f.ack(t)); !strings.Contains(cardJSON(c), "Rebooting") {
		t.Errorf("reboot card %s", cardJSON(c))
	}

	// a slow action answers with a toast and updates the card when done
	feishuBoxSettle = 3 * feishuAnswerBudget
	f.action(p, "ou_owner", map[string]string{"a": "box_restart", "ok": "1"}, "", nil, now)
	ans = f.ack(t)
	if k, _ := ansToast(ans); k != "info" || ans["card"] != nil {
		t.Fatalf("box restart answer %v", ans)
	}
	up := f.nextSent(t)
	if up.Method != http.MethodPatch || up.To != "om_card" || !strings.Contains(cardJSON(up.Card), "Box is back") {
		t.Errorf("late update %+v", up)
	}
}

// every card but the menu leads back to it
func TestFeishuBotCardsReachMenu(t *testing.T) {
	s, _, _ := newTestBot(t, "ou_owner")
	ctx := context.Background()
	for name, card := range map[string]map[string]any{
		"status": s.bot.statusCard(ctx), "sms": s.bot.smsCard(ctx), "proxy": s.bot.proxyCard(ctx),
		"device": s.bot.deviceCard(), "confirm": s.bot.smsConfirm("bad", ""),
	} {
		if findValue(card, "menu") == nil {
			t.Errorf("%s card has no menu button", name)
		}
	}
}

func TestFeishuBotSMS(t *testing.T) {
	s, f, p := newTestBot(t, "ou_owner")
	now := time.Now()
	f.action(p, "ou_owner", map[string]string{"a": "sms"}, "", nil, now)
	card := ansCard(f.ack(t))
	if title(card) != "SMS" || findValue(card, "sms_del") == nil || findValue(card, "sms_compose") == nil {
		t.Fatalf("sms card %s", cardJSON(card))
	}
	// write, confirm, send
	f.action(p, "ou_owner", map[string]string{"a": "sms_confirm"}, "", map[string]string{"number": "+1 555 0100", "text": " hello "}, now)
	confirm := findValue(ansCard(f.ack(t)), "sms_send")
	if confirm["n"] != "+15550100" || confirm["t"] != "hello" || confirm["ok"] != "1" {
		t.Fatalf("confirmation %v", confirm)
	}
	f.action(p, "ou_owner", map[string]string{"a": "sms_confirm"}, "", map[string]string{"number": "abc", "text": "x"}, now)
	if k, _ := ansToast(f.ack(t)); k != "error" {
		t.Errorf("bad number toast %q", k)
	}
	f.action(p, "ou_owner", confirm, "", nil, now)
	if k, _ := ansToast(f.ack(t)); k != "success" {
		t.Fatalf("send toast %q", k)
	}
	msgs, _ := s.zte.smsList(context.Background(), 1)
	if len(msgs) != 1 || msgs[0].Number != "+15550100" || msgs[0].Text != "hello" {
		t.Fatalf("sent %+v", msgs)
	}
	f.action(p, "ou_owner", map[string]string{"a": "sms_del", "id": strconv.FormatInt(msgs[0].ID, 10)}, "", nil, now)
	f.ack(t)
	if after, _ := s.zte.smsList(context.Background(), 1); len(after) > 0 && after[0].ID == msgs[0].ID {
		t.Error("message not deleted")
	}
}

func TestFeishuBotPush(t *testing.T) {
	s, f, _ := newTestBot(t, "ou_owner")
	f.mu.Lock()
	f.expire = true // the cached token is refused once
	f.mu.Unlock()
	n := s.notify
	var ch *channel
	for _, c := range n.chans {
		if c.name == "feishu" {
			ch = c
		}
	}
	if !ch.enabled(s.cfg.get()) {
		t.Fatal("feishu channel off with a bound bot")
	}
	s.events.add(event{Check: "sms", State: "info", Msg: "10690", SMS: &smsEvent{ID: 9, From: "10690", Text: "code 123456", Code: "123456"}})
	if err := n.flush(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	var got sentCard
	for got.Card == nil || title(got.Card) != "\U0001F510 123456" {
		got = f.nextSent(t)
	}
	if got.IDType != "open_id" || got.To != "ou_owner" || findValue(got.Card, "sms_del")["id"] != "9" || findValue(got.Card, "sms_compose")["n"] != "10690" {
		t.Errorf("pushed %+v", got)
	}
	if f.tokens < 2 {
		t.Errorf("token not refreshed: %d requests", f.tokens)
	}
}

func TestFeishuBotSecretsStayOut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := newDemoServer(ctx, demoDir(t), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeLark(t)
	f.tokenErr = "app secret " + testAppSecret + " invalid"
	s.cfg.update(func(c *Config) {
		c.FeishuAppID, c.FeishuAppSecret, c.FeishuDomain, c.FeishuBotUsers = testAppID, testAppSecret, f.url(), []string{"ou_owner"}
	})
	_, err = s.bot.sendCard(ctx, "open_id", "ou_owner", s.bot.menu(""))
	if err == nil || strings.Contains(err.Error(), testAppSecret) || !strings.Contains(err.Error(), "10014") {
		t.Errorf("error %v", err)
	}
	// a refused connection shows its reason, without the secret
	s.cfg.update(func(c *Config) { c.FeishuAppSecret = "wrong" })
	if err := s.bot.session(ctx); err == nil || !strings.Contains(err.Error(), "bad app") {
		t.Errorf("session error %v", err)
	}
	if b, _ := json.Marshal(s.cfg.get().redacted()); strings.Contains(string(b), "wrong") {
		t.Error("app secret not redacted")
	}
}

func TestParseCommand(t *testing.T) {
	for in, want := range map[string][2]string{
		"  status ":                {"status", ""},
		"/sms 10086 two words":     {"sms", "10086 two words"},
		"\u77ed\u4fe1 10086 hi":    {"sms", "10086 hi"},
		"\u4ee3\u7406":             {"proxy", ""},
		"bind\t123456":             {"bind", "123456"},
		"":                         {"help", ""},
		"hello there":              {"help", ""},
		"\u5e2e\u52a9":             {"help", ""},
		"\u8bbe\u5907":             {"device", ""},
		"MENU":                     {"help", ""},
		"\u7ed1\u5b9a  000111 ":    {"bind", "000111"},
		"\u72b6\u6001\u6765\u4e00": {"help", ""},
	} {
		cmd, args := parseCommand(in)
		if cmd != want[0] || args != want[1] {
			t.Errorf("%q: %q %q, want %q", in, cmd, args, want)
		}
	}
}

func TestFeishuTime(t *testing.T) {
	ms := time.UnixMilli(1608725989000)
	if got := feishuTime("1608725989000"); !got.Equal(ms) {
		t.Errorf("ms %v", got)
	}
	if got := feishuTime("1603977298000000"); !got.Equal(time.UnixMicro(1603977298000000)) {
		t.Errorf("us %v", got)
	}
	if !feishuTime("x").IsZero() {
		t.Error("garbage parsed")
	}
}
