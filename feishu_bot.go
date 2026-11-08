package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The Feishu bot: a self-built app in the owner's Feishu team. In a private chat it answers
// commands with cards whose buttons act on the device; alerts and SMS reach the owner the same
// way. Only the bound owners (feishu_bot_users) are obeyed.

const (
	feishuEventMaxAge  = 5 * time.Minute  // older events are dropped: sent while the device was off
	feishuActionMaxAge = time.Minute      // older clicks do not change anything
	feishuConfirmFor   = 2 * time.Minute  // a confirmation card's buttons work this long
	feishuSMSShown     = 5                // messages on the SMS card
	feishuNodesShown   = 100              // nodes in the switch menu
	feishuSMSRunes     = 300              // text of a message on the SMS card
	feishuSeenMax      = 200              // event ids remembered against redelivery
	feishuBindTries    = 5                // wrong codes before the binding code changes
	feishuBoxWait      = 60 * time.Second // how long a Box restart is followed
)

// the old mihomo may still answer for a moment after a Box restart; a variable for the tests
var feishuBoxSettle = 5 * time.Second

type feishuBot struct {
	s   *server
	hc  *http.Client // Open Platform calls
	wsc *http.Client // the long connection: no timeout, HTTP/1.1

	mu        sync.Mutex
	token     string
	tokenExp  time.Time
	connected bool
	err       string
	bindCode  string
	bindFails int
	seen      map[string]bool
	seenOrder []string
}

func newFeishuBot(s *server) *feishuBot {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	ws := newWSTransport()
	ws.ResponseHeaderTimeout = 15 * time.Second
	return &feishuBot{
		s:    s,
		hc:   &http.Client{Transport: tr, Timeout: 20 * time.Second},
		wsc:  &http.Client{Transport: ws},
		seen: map[string]bool{},
	}
}

func (c Config) feishuBotEnabled() bool {
	return c.FeishuAppID != "" && c.FeishuAppSecret != ""
}

// ready reports whether alerts go through the bot: it is configured and someone is bound
func (b *feishuBot) ready(c Config) bool {
	return b != nil && c.feishuBotEnabled() && len(c.FeishuBotUsers) > 0
}

// status is the bot as the console shows it; bind is the code to send while nobody is bound
func (b *feishuBot) status() map[string]any {
	if b == nil {
		return map[string]any{"enabled": false}
	}
	c := b.s.cfg.get()
	if !c.feishuBotEnabled() {
		return map[string]any{"enabled": false}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]any{"enabled": true, "connected": b.connected, "owners": len(c.FeishuBotUsers)}
	if b.err != "" {
		out["error"] = b.err
	}
	if len(c.FeishuBotUsers) == 0 {
		if b.bindCode == "" {
			b.bindCode = newBindCode()
		}
		out["bind"] = b.bindCode
	}
	return out
}

func newBindCode() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1e6))
	return fmt.Sprintf("%06d", n)
}

// fresh records an event id and reports whether it is new
func (b *feishuBot) fresh(id string) bool {
	if id == "" {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen[id] {
		return false
	}
	b.seen[id] = true
	b.seenOrder = append(b.seenOrder, id)
	if len(b.seenOrder) > feishuSeenMax {
		delete(b.seen, b.seenOrder[0])
		b.seenOrder = b.seenOrder[1:]
	}
	return true
}

func (b *feishuBot) isOwner(openID string) bool {
	return openID != "" && slices.Contains(b.s.cfg.get().FeishuBotUsers, openID)
}

// feishuTime reads an event's create_time: milliseconds, or microseconds for card actions
func feishuTime(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	if len(s) >= 16 {
		return time.UnixMicro(n)
	}
	return time.UnixMilli(n)
}

// ---------- events ----------

type feishuToast struct {
	Type    string `json:"type"` // info, success, error, warning
	Content string `json:"content"`
}

// cardAnswer answers a card action: a toast, a new card, or both. messageID is the card to update
// when the answer comes too late for the action's reply.
type cardAnswer struct {
	Toast     *feishuToast
	Card      map[string]any
	messageID string
}

func (a cardAnswer) MarshalJSON() ([]byte, error) {
	m := map[string]any{}
	if a.Toast != nil {
		m["toast"] = a.Toast
	}
	if a.Card != nil {
		m["card"] = map[string]any{"type": "raw", "data": a.Card}
	}
	return json.Marshal(m)
}

func (b *feishuBot) lang() string { return b.s.cfg.get().ReportLang }

func (b *feishuBot) t(key string, kv ...string) string {
	vars := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		vars[kv[i]] = kv[i+1]
	}
	return tr(b.lang(), key, vars)
}

func toast(kind, text string) *cardAnswer {
	return &cardAnswer{Toast: &feishuToast{kind, text}}
}

func (b *feishuBot) working() *cardAnswer { return toast("info", b.t("bot.working")) }

// errText translates an API error the way the console does
func (b *feishuBot) errText(err error) string {
	var ae *apiError
	if errors.As(err, &ae) {
		vars := map[string]string{}
		for k, v := range ae.Args {
			vars[k] = fmt.Sprint(v)
		}
		if s := tr(b.lang(), "err."+ae.Code, vars); s != "err."+ae.Code {
			return s
		}
	}
	return err.Error()
}

func (b *feishuBot) failed(err error) *cardAnswer {
	return toast("error", b.t("bot.failed", "err", b.errText(err)))
}

// handleEvent handles an event from the long connection; only card actions have an answer
func (b *feishuBot) handleEvent(ctx context.Context, payload []byte) *cardAnswer {
	var ev struct {
		Header struct {
			EventID    string `json:"event_id"`
			EventType  string `json:"event_type"`
			CreateTime string `json:"create_time"`
		} `json:"header"`
		Event json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		log.Printf("feishu: bad event: %v", err)
		return nil
	}
	if !b.fresh(ev.Header.EventID) {
		return nil
	}
	created := feishuTime(ev.Header.CreateTime)
	if !created.IsZero() && time.Since(created) > feishuEventMaxAge {
		log.Printf("feishu: dropped a %s from %s ago", ev.Header.EventType, time.Since(created).Round(time.Second))
		return nil
	}
	switch ev.Header.EventType {
	case "im.message.receive_v1":
		// acknowledged right away; the reply is a message of its own
		go b.onMessage(context.WithoutCancel(ctx), ev.Event)
	case "card.action.trigger":
		return b.onAction(ctx, created, ev.Event)
	}
	return nil
}

func (b *feishuBot) onMessage(ctx context.Context, raw json.RawMessage) {
	var ev struct {
		Sender struct {
			SenderID struct {
				OpenID string `json:"open_id"`
			} `json:"sender_id"`
			SenderType string `json:"sender_type"`
		} `json:"sender"`
		Message struct {
			ChatID      string `json:"chat_id"`
			ChatType    string `json:"chat_type"`
			MessageType string `json:"message_type"`
			Content     string `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &ev) != nil || ev.Message.ChatType != "p2p" || ev.Sender.SenderType != "user" {
		return
	}
	var text string
	if ev.Message.MessageType == "text" {
		var content struct {
			Text string `json:"text"`
		}
		json.Unmarshal([]byte(ev.Message.Content), &content)
		text = content.Text
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd, args := parseCommand(text)
	from := ev.Sender.SenderID.OpenID
	var card map[string]any
	switch {
	case b.isOwner(from):
		log.Printf("feishu: command %s", cmd)
		card = b.command(ctx, cmd, args)
	case cmd == "bind":
		card = b.bind(from, args)
	default:
		card = b.unauthorized(from)
	}
	if _, err := b.sendCard(ctx, "chat_id", ev.Message.ChatID, card); err != nil {
		log.Printf("feishu: reply: %v", err)
	}
}

// bind makes the sender an owner when nobody is bound and the code matches the console's
func (b *feishuBot) bind(openID, code string) map[string]any {
	if len(b.s.cfg.get().FeishuBotUsers) > 0 {
		return b.unauthorized(openID)
	}
	code = strings.TrimSpace(code)
	b.mu.Lock()
	ok := b.bindCode != "" && subtle.ConstantTimeCompare([]byte(code), []byte(b.bindCode)) == 1
	if ok {
		b.bindCode, b.bindFails = "", 0
	} else if b.bindFails++; b.bindFails >= feishuBindTries {
		b.bindCode, b.bindFails = "", 0
	}
	b.mu.Unlock()
	if !ok {
		return b.card(b.t("bot.unauth.title"), "red", txt(b.t("bot.bind.wrong")), note(b.t("bot.unauth.bind")))
	}
	if err := b.s.cfg.update(func(c *Config) { c.FeishuBotUsers = append(c.FeishuBotUsers, openID) }); err != nil {
		return b.card(b.t("bot.unauth.title"), "red", txt(b.t("bot.failed", "err", err.Error())))
	}
	log.Printf("feishu: bound the owner")
	return b.menu(b.t("bot.bind.ok"))
}

func (b *feishuBot) unauthorized(openID string) map[string]any {
	els := []any{txt(b.t("bot.unauth.text", "id", openID))}
	if len(b.s.cfg.get().FeishuBotUsers) == 0 {
		els = append(els, note(b.t("bot.unauth.bind")))
	}
	return b.card(b.t("bot.unauth.title"), "red", els...)
}

// botCommands are the commands; their words in each language are the bot.cmd.* translations
var botCommands = []struct{ name, key string }{
	{"help", "bot.cmd.help"},
	{"status", "bot.cmd.status"},
	{"sms", "bot.cmd.sms"},
	{"proxy", "bot.cmd.proxy"},
	{"device", "bot.cmd.device"},
	{"bind", "bot.cmd.bind"},
}

// parseCommand splits "word rest"; an unknown word asks for help
func parseCommand(text string) (string, string) {
	text = strings.TrimPrefix(strings.TrimSpace(text), "/")
	word, rest, _ := strings.Cut(text, " ")
	if i := strings.IndexAny(word, "\t\n"); i >= 0 {
		word, rest = word[:i], word[i+1:]+" "+rest
	}
	word = strings.ToLower(word)
	for _, c := range botCommands {
		if word == c.name {
			return c.name, strings.TrimSpace(rest)
		}
		for _, lang := range localeNames() {
			for _, alias := range strings.Split(tr(lang, c.key, nil), "|") {
				if alias != "" && word == strings.ToLower(alias) {
					return c.name, strings.TrimSpace(rest)
				}
			}
		}
	}
	return "help", ""
}

// command answers a text command with a card
func (b *feishuBot) command(ctx context.Context, cmd, args string) map[string]any {
	switch cmd {
	case "status":
		return b.statusCard(ctx)
	case "sms":
		if args == "" {
			return b.smsCard(ctx)
		}
		number, text, _ := strings.Cut(args, " ")
		return b.smsConfirm(number, text)
	case "proxy":
		return b.proxyCard(ctx)
	case "device":
		return b.deviceCard()
	case "bind":
		return b.menu("")
	}
	return b.menu("")
}

// mutating are the card actions that change something; they must be recent
var mutating = map[string]bool{
	"select": true, "mode": true, "sms_send": true, "sms_del": true,
	"box_restart": true, "reboot": true, "update_install": true,
}

func (b *feishuBot) onAction(ctx context.Context, created time.Time, raw json.RawMessage) *cardAnswer {
	var ev struct {
		Operator struct {
			OpenID string `json:"open_id"`
		} `json:"operator"`
		Action struct {
			Value     map[string]any `json:"value"`
			Option    string         `json:"option"`
			FormValue map[string]any `json:"form_value"`
		} `json:"action"`
		Context struct {
			MessageID string `json:"open_message_id"`
		} `json:"context"`
	}
	if json.Unmarshal(raw, &ev) != nil {
		return nil
	}
	if !b.isOwner(ev.Operator.OpenID) {
		return toast("error", b.t("bot.unauth.title"))
	}
	val := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	v := func(k string) string { return val(ev.Action.Value, k) }
	a := v("a")
	if mutating[a] && !created.IsZero() && time.Since(created) > feishuActionMaxAge {
		return toast("warning", b.t("bot.expired"))
	}
	// a confirmation card's buttons expire
	if exp, err := strconv.ParseInt(v("exp"), 10, 64); err == nil && time.Now().Unix() > exp {
		return toast("warning", b.t("bot.expired"))
	}
	log.Printf("feishu: action %s", a)
	ans := b.act(ctx, a, v, ev.Action.Option, func(k string) string { return val(ev.Action.FormValue, k) })
	if ans != nil {
		ans.messageID = ev.Context.MessageID
	}
	return ans
}

// act runs a card action; v reads the button's value, form the submitted form
func (b *feishuBot) act(ctx context.Context, a string, v func(string) string, option string, form func(string) string) *cardAnswer {
	c := b.s.cfg.get()
	switch a {
	case "menu":
		return &cardAnswer{Card: b.menu("")}
	case "status":
		return &cardAnswer{Card: b.statusCard(ctx)}
	case "sms":
		return &cardAnswer{Card: b.smsCard(ctx)}
	case "sms_compose":
		return &cardAnswer{Card: b.smsCompose(v("n"))}
	case "sms_confirm":
		number, text, err := checkSMS(form("number"), form("text"))
		if err != nil {
			return b.failed(err)
		}
		return &cardAnswer{Card: b.smsConfirm(number, text)}
	case "sms_send":
		number, text, err := checkSMS(v("n"), v("t"))
		if err == nil {
			err = b.s.sendSMS(ctx, number, text)
		}
		if err != nil {
			return b.failed(err)
		}
		return &cardAnswer{Toast: &feishuToast{"success", b.t("sms.sent")},
			Card: b.card(b.t("bot.sms.title"), "green", txt(b.t("bot.sms.sent", "n", number)), btnRow(b.btn("bot.btn.sms", "default", "a", "sms")))}
	case "sms_del":
		id, err := strconv.ParseInt(v("id"), 10, 64)
		if err == nil {
			err = b.s.zte.smsEach(ctx, "DELETE_SMS", []int64{id})
		}
		if err != nil {
			return b.failed(err)
		}
		log.Printf("sms: deleted 1 messages")
		return &cardAnswer{Toast: &feishuToast{"success", b.t("sms.deleted")}, Card: b.smsCard(ctx)}
	case "proxy":
		return &cardAnswer{Card: b.proxyCard(ctx)}
	case "select":
		if err := b.s.mihomo.selectProxy(ctx, v("g"), option); err != nil {
			return b.failed(err)
		}
		log.Printf("select %s -> %s", v("g"), option)
		return &cardAnswer{Toast: &feishuToast{"success", b.t("bot.px.selected", "node", option)}, Card: b.proxyCard(ctx)}
	case "mode":
		m := v("m")
		if m != "rule" && m != "global" && m != "direct" {
			return b.failed(apiErr("mode_invalid", "mode must be rule, global or direct"))
		}
		if err := b.s.mihomo.patchConfigs(ctx, map[string]any{"mode": m}); err != nil {
			return b.failed(err)
		}
		log.Printf("proxy mode -> %s", m)
		return &cardAnswer{Toast: &feishuToast{"success", b.t("bot.done")}, Card: b.proxyCard(ctx)}
	case "delay":
		dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		res, err := b.s.mihomo.groupDelay(dctx, v("g"), c.DelayURL, 5000)
		if err != nil {
			// every node timing out is an error too
			res = map[string]int{}
		}
		return &cardAnswer{Toast: &feishuToast{"success", b.t("bot.px.tested")}, Card: b.proxyCardWith(ctx, res)}
	case "device":
		return &cardAnswer{Card: b.deviceCard()}
	case "update_check":
		if c.UpdateURL == "" {
			return b.failed(apiErr("update_off", "remote upgrade is turned off"))
		}
		if _, err := b.s.update.check(ctx); err != nil {
			return &cardAnswer{Toast: &feishuToast{"error", b.t("bot.failed", "err", b.errText(err))}, Card: b.deviceCard()}
		}
		return &cardAnswer{Toast: &feishuToast{"success", b.t("bot.done")}, Card: b.deviceCard()}
	case "update_install":
		code, _ := strconv.ParseInt(v("c"), 10, 64)
		if v("ok") != "1" {
			ver := b.s.update.available()
			return &cardAnswer{Card: b.confirm(b.t("bot.dev.installQ", "v", ver), "orange", "update_install", "c", v("c"))}
		}
		if !b.s.update.info().Managed {
			return b.failed(apiErr("update_unmanaged", "glovebox is not running from its Magisk module"))
		}
		ver := b.s.update.available()
		if err := b.s.update.installLatest(code); err != nil {
			return b.failed(err)
		}
		return &cardAnswer{Card: b.card(b.t("bot.dev.title", "device", c.DeviceName), "turquoise", txt(b.t("bot.dev.installing", "v", ver)))}
	case "ts_check":
		tctx, cancel := context.WithTimeout(ctx, 12*time.Second)
		defer cancel()
		b.s.ts.check(tctx)
		return &cardAnswer{Card: b.deviceCard()}
	case "report_test":
		tctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		var sent, bad []string
		for name, err := range b.s.notify.test(tctx) {
			if err != nil {
				bad = append(bad, name+": "+err.Error())
			} else {
				sent = append(sent, name)
			}
		}
		slices.Sort(sent)
		if len(bad) > 0 {
			return toast("error", strings.Join(bad, "; "))
		}
		return toast("success", b.t("rep.sent", "list", strings.Join(sent, ", ")))
	case "box_restart":
		if v("ok") != "1" {
			return &cardAnswer{Card: b.confirm(b.t("bot.dev.boxQ"), "orange", "box_restart")}
		}
		if err := b.s.restartBox(); err != nil {
			return b.failed(err)
		}
		return &cardAnswer{Card: b.boxRecovery(ctx)}
	case "reboot":
		if v("ok") != "1" {
			return &cardAnswer{Card: b.confirm(b.t("bot.dev.rebootQ"), "red", "reboot")}
		}
		log.Printf("reboot requested in Feishu")
		b.s.rebootSoon()
		return &cardAnswer{Card: b.card(b.t("bot.dev.title", "device", c.DeviceName), "red", txt(b.t("bot.dev.rebooting")))}
	}
	return &cardAnswer{Card: b.menu("")}
}

// boxRecovery waits for mihomo to answer again after a Box restart
func (b *feishuBot) boxRecovery(ctx context.Context) map[string]any {
	title := b.t("bot.dev.title", "device", b.s.cfg.get().DeviceName)
	deadline := time.Now().Add(feishuBoxWait)
	sleepCtx(ctx, feishuBoxSettle)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		vctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		v, err := b.s.mihomo.version(vctx)
		cancel()
		if err == nil {
			return b.card(title, "green", txt(b.t("bot.dev.boxOK", "v", v)), btnRow(b.btn("bot.btn.status", "default", "a", "status")))
		}
		sleepCtx(ctx, 3*time.Second)
	}
	return b.card(title, "red", txt(b.t("bot.dev.boxSlow")), btnRow(b.btn("bot.btn.status", "default", "a", "status")))
}

// ---------- cards ----------

func (b *feishuBot) card(title, template string, els ...any) map[string]any {
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true, "summary": map[string]string{"content": title}},
		"header": map[string]any{"title": map[string]string{"tag": "plain_text", "content": title}, "template": template},
		"body":   map[string]any{"elements": els},
	}
}

func txt(s string) map[string]any  { return feishuText(s, "", "") }
func note(s string) map[string]any { return feishuText(s, "notation", "grey") }

// btn is a button labelled with the translation of key; kv is its value
func (b *feishuBot) btn(key, kind string, kv ...string) map[string]any {
	return rawBtn(b.t(key), kind, kv...)
}

func rawBtn(label, kind string, kv ...string) map[string]any {
	value := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		value[kv[i]] = kv[i+1]
	}
	return map[string]any{
		"tag": "button", "type": kind, "size": "small",
		"text":      map[string]string{"tag": "plain_text", "content": label},
		"behaviors": []any{map[string]any{"type": "callback", "value": value}},
	}
}

// btnRow lays buttons out side by side, wrapping on narrow screens
func btnRow(btns ...map[string]any) map[string]any {
	var cols []any
	for _, bt := range btns {
		cols = append(cols, map[string]any{"tag": "column", "width": "auto", "elements": []any{bt}})
	}
	return map[string]any{"tag": "column_set", "flex_mode": "flow", "horizontal_spacing": "8px", "columns": cols}
}

func (b *feishuBot) navRow(skip string) map[string]any {
	var btns []map[string]any
	for _, n := range []string{"status", "sms", "proxy", "device"} {
		if n != skip {
			btns = append(btns, b.btn("bot.btn."+n, "default", "a", n))
		}
	}
	return btnRow(btns...)
}

func (b *feishuBot) menu(lead string) map[string]any {
	var els []any
	if lead != "" {
		els = append(els, txt(lead))
	}
	els = append(els, note(b.t("bot.menu.text")), btnRow(
		b.btn("bot.btn.status", "primary", "a", "status"), b.btn("bot.btn.sms", "default", "a", "sms"),
		b.btn("bot.btn.proxy", "default", "a", "proxy"), b.btn("bot.btn.device", "default", "a", "device")))
	return b.card(b.t("bot.menu.title", "device", b.s.cfg.get().DeviceName), "blue", els...)
}

// confirm asks before an action; its button carries ok=1 and an expiry
func (b *feishuBot) confirm(question, template, action string, kv ...string) map[string]any {
	kv = append(kv, "a", action, "ok", "1", "exp", strconv.FormatInt(time.Now().Add(feishuConfirmFor).Unix(), 10))
	back := "device"
	if action == "sms_send" {
		back = "sms"
	}
	return b.card(question, template, btnRow(b.btn("bot.btn.confirm", "danger", kv...), b.btn("bot.btn.cancel", "default", "a", back)))
}

func fmtBytes(n float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", n, units[i])
	}
	return fmt.Sprintf("%.1f %s", n, units[i])
}

func mmap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func (b *feishuBot) modeName(m any) string {
	s, _ := m.(string)
	if s == "" {
		return "—"
	}
	return tr(b.lang(), "mode."+s, nil)
}

func (b *feishuBot) statusCard(ctx context.Context) map[string]any {
	c := b.s.cfg.get()
	st := b.s.statusData(ctx)
	errs, _ := st["errors"].(map[string]string)
	var els []any
	if cell := mmap(st["cell"]); cell != nil {
		dash := func(k string) string {
			if s, _ := cell[k].(string); s != "" {
				return s
			}
			return "—"
		}
		els = append(els, txt("\U0001F4F6 "+b.t("bot.st.cell", "operator", dash("operator"), "net", dash("label"), "band", dash("band"), "rsrp", dash("rsrp"), "sinr", dash("sinr"))))
		month := mmap(st["month"])
		rx, _ := month["rx"].(float64)
		tx, _ := month["tx"].(float64)
		if on, _ := cell["connected"].(bool); on {
			els = append(els, txt("\U0001F310 "+b.t("bot.st.online", "rx", fmtBytes(rx), "tx", fmtBytes(tx))))
		} else {
			els = append(els, txt("\U0001F310 "+b.t("bot.st.offline")))
		}
		n, _ := st["clients"].(int)
		els = append(els, txt("\U0001F4F1 "+b.t("bot.st.clients", "n", strconv.Itoa(n))))
	} else {
		els = append(els, txt("\U0001F4F6 "+b.t("bot.st.zteDown", "err", errs["zte"])))
	}
	px := mmap(st["proxy"])
	if run, _ := px["running"].(bool); run {
		chain, _ := px["now"].([]string)
		node := "—"
		if len(chain) > 0 {
			node = chain[len(chain)-1]
		}
		els = append(els, txt("\U0001F9ED "+b.t("bot.st.proxy", "node", node, "mode", b.modeName(px["mode"]))))
	} else {
		els = append(els, txt("\U0001F9ED "+b.t("bot.st.proxyDown")))
	}
	if ts := b.s.ts.status(); ts.Enabled {
		if ts.Online {
			els = append(els, txt("\U0001F512 "+b.t("bot.st.ts", "ms", strconv.Itoa(ts.Delay))))
		} else {
			els = append(els, txt("\U0001F512 "+b.t("bot.st.tsOff")))
		}
	}
	if bad := b.s.health.bad(); len(bad) > 0 {
		var names []string
		for _, n := range bad {
			names = append(names, tr(b.lang(), "hc."+n, nil))
		}
		els = append(els, txt("\U0001F534 "+b.t("bot.st.bad", "list", strings.Join(names, ", "))))
	} else {
		els = append(els, txt("\U0001F7E2 "+b.t("bot.st.ok")))
	}
	si := b.s.sys()
	temp := "—"
	for _, th := range si.Thermals {
		if th.TempC > 0 {
			temp = fmt.Sprintf("%.0f°C", th.TempC)
			break
		}
	}
	els = append(els, note(b.t("bot.st.sys", "version", version, "up", fmtDuration(si.Uptime), "temp", temp,
		"used", strconv.FormatInt(si.Mem.TotalMB-si.Mem.AvailableMB, 10), "total", strconv.FormatInt(si.Mem.TotalMB, 10))))
	if v := b.s.update.available(); v != "" {
		els = append(els, note("\U0001F195 "+tr(b.lang(), "ev.update.info", map[string]string{"msg": v})))
	}
	els = append(els, btnRow(b.btn("bot.btn.refresh", "primary", "a", "status"), b.btn("bot.btn.proxy", "default", "a", "proxy"),
		b.btn("bot.btn.sms", "default", "a", "sms"), b.btn("bot.btn.device", "default", "a", "device")))
	return b.card(b.t("bot.st.title", "device", c.DeviceName), "blue", els...)
}

func (b *feishuBot) smsCard(ctx context.Context) map[string]any {
	title := b.t("bot.sms.title")
	msgs, err := b.s.zte.smsList(ctx, feishuSMSShown)
	if err != nil {
		return b.card(title, "red", txt(b.t("bot.failed", "err", err.Error())), b.navRow("sms"))
	}
	var els []any
	if len(msgs) == 0 {
		els = append(els, note(b.t("sms.empty")))
	}
	for i, m := range msgs {
		if i > 0 {
			els = append(els, map[string]any{"tag": "hr"})
		}
		out := m.State == "sent" || m.State == "failed" || m.State == "draft"
		who := m.Number
		if out {
			who = b.t("sms.toN", "n", m.Number)
		}
		head := who + " · " + m.Time.In(time.Local).Format("01-02 15:04") + " · " + b.t("sms.state."+m.State)
		if m.Code != "" {
			head = "\U0001F510 " + m.Code + " · " + head
		}
		els = append(els, note(head), txt(cut(m.Text, feishuSMSRunes, false)))
		id := strconv.FormatInt(m.ID, 10)
		if out {
			els = append(els, btnRow(b.btn("sms.delete", "default", "a", "sms_del", "id", id)))
		} else {
			els = append(els, btnRow(b.btn("sms.reply", "default", "a", "sms_compose", "n", m.Number), b.btn("sms.delete", "default", "a", "sms_del", "id", id)))
		}
	}
	els = append(els, btnRow(b.btn("bot.sms.compose", "primary", "a", "sms_compose"), b.btn("bot.btn.refresh", "default", "a", "sms")))
	return b.card(title, "wathet", els...)
}

func (b *feishuBot) smsCompose(number string) map[string]any {
	form := map[string]any{
		"tag": "form", "name": "sms",
		"elements": []any{
			map[string]any{"tag": "input", "name": "number", "required": true, "default_value": number, "max_length": 24,
				"label": map[string]string{"tag": "plain_text", "content": b.t("sms.to")}},
			map[string]any{"tag": "input", "name": "text", "required": true, "input_type": "multiline_text", "rows": 3, "max_length": smsTextMax,
				"label": map[string]string{"tag": "plain_text", "content": b.t("sms.text")}},
			map[string]any{"tag": "button", "name": "next", "form_action_type": "submit", "type": "primary",
				"text":      map[string]string{"tag": "plain_text", "content": b.t("bot.sms.next")},
				"behaviors": []any{map[string]any{"type": "callback", "value": map[string]string{"a": "sms_confirm"}}}},
		},
	}
	return b.card(b.t("bot.sms.compose"), "wathet", form, btnRow(b.btn("bot.btn.cancel", "default", "a", "sms")))
}

func (b *feishuBot) smsConfirm(number, text string) map[string]any {
	number, text, err := checkSMS(number, text)
	if err != nil {
		return b.card(b.t("bot.sms.title"), "red", txt(b.errText(err)), btnRow(b.btn("bot.sms.compose", "primary", "a", "sms_compose")))
	}
	c := b.confirm(b.t("bot.sms.confirm", "n", number), "orange", "sms_send", "n", number, "t", text)
	body := c["body"].(map[string]any)
	body["elements"] = append([]any{txt(text), note(b.t("bot.sms.cost"))}, body["elements"].([]any)...)
	return c
}

func (b *feishuBot) proxyCard(ctx context.Context) map[string]any {
	return b.proxyCardWith(ctx, nil)
}

// proxyCardWith shows the main group; delays, when given, are fresh test results
func (b *feishuBot) proxyCardWith(ctx context.Context, delays map[string]int) map[string]any {
	title := b.t("bot.px.title")
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	groups, mode, err := b.s.proxyGroups(pctx)
	if err != nil || len(groups) == 0 {
		msg := b.t("bot.st.proxyDown")
		if err != nil {
			msg += ": " + err.Error()
		}
		return b.card(title, "red", txt(msg), b.navRow("proxy"))
	}
	g := groups[0]
	chain := append([]string{g.Name}, g.Chain...)
	els := []any{txt(strings.Join(chain, " → ")), note(b.t("bot.px.mode", "mode", b.modeName(mode)))}
	label := func(n proxyNode) string {
		d := n.Delay
		if delays != nil {
			d = delays[n.Name]
		}
		if d <= 0 {
			return n.Name + " · " + b.t("bot.px.timeout")
		}
		return fmt.Sprintf("%s · %d ms", n.Name, d)
	}
	if g.Type == "Selector" {
		var opts []any
		for i, n := range g.Nodes {
			if i >= feishuNodesShown && n.Name != g.Now {
				continue
			}
			opts = append(opts, map[string]any{"text": map[string]string{"tag": "plain_text", "content": label(n)}, "value": n.Name})
		}
		els = append(els, map[string]any{
			"tag": "select_static", "initial_option": g.Now, "options": opts,
			"placeholder": map[string]string{"tag": "plain_text", "content": b.t("bot.px.pick")},
			"behaviors":   []any{map[string]any{"type": "callback", "value": map[string]string{"a": "select", "g": g.Name}}},
		})
	} else {
		els = append(els, note(b.t("bot.px.fixed", "group", g.Name)))
		for _, n := range g.Nodes {
			if n.Name == g.Now {
				els = append(els, txt(label(n)))
			}
		}
	}
	var modes []map[string]any
	cur, _ := mode.(string)
	for _, m := range []string{"rule", "global", "direct"} {
		kind := "default"
		if m == cur {
			kind = "primary"
		}
		modes = append(modes, rawBtn(tr(b.lang(), "mode."+m, nil), kind, "a", "mode", "m", m))
	}
	els = append(els, btnRow(append([]map[string]any{b.btn("bot.px.test", "primary", "a", "delay", "g", g.Name)}, modes...)...), b.navRow("proxy"))
	return b.card(title, "indigo", els...)
}

func (b *feishuBot) deviceCard() map[string]any {
	c := b.s.cfg.get()
	u := b.s.update.info()
	var els []any
	ver := "Glovebox " + u.Version
	switch {
	case u.Latest != nil:
		ver += " · " + tr(b.lang(), "ev.update.info", map[string]string{"msg": u.Latest.Version})
	case !u.CheckedAt.IsZero():
		ver += " · " + tr(b.lang(), "upd.upToDate", nil)
	}
	els = append(els, txt(ver))
	if u.Error != "" {
		els = append(els, feishuText(u.Error, "notation", "red"))
	}
	if ts := b.s.ts.status(); ts.Enabled {
		line := b.t("bot.st.tsOff")
		if ts.Online {
			line = b.t("bot.st.ts", "ms", strconv.Itoa(ts.Delay))
		}
		if ts.Checked != "" {
			line += " · " + ts.Checked
		}
		els = append(els, txt(line))
	}
	els = append(els, note(b.t("bot.dev.up", "up", fmtDuration(b.s.sys().Uptime))))
	var row []map[string]any
	if c.UpdateURL != "" {
		row = append(row, b.btn("bot.dev.check", "default", "a", "update_check"))
	}
	if u.Latest != nil && u.Managed {
		row = append(row, rawBtn(b.t("bot.dev.install", "v", u.Latest.Version), "primary", "a", "update_install", "c", strconv.FormatInt(u.Latest.VersionCode, 10)))
	}
	if c.tailscaleEnabled() {
		row = append(row, b.btn("bot.dev.ts", "default", "a", "ts_check"))
	}
	row = append(row, b.btn("bot.dev.test", "default", "a", "report_test"))
	els = append(els, btnRow(row...),
		btnRow(b.btn("bot.dev.box", "danger", "a", "box_restart"), b.btn("bot.dev.reboot", "danger", "a", "reboot")),
		b.navRow("device"))
	return b.card(b.t("bot.dev.title", "device", c.DeviceName), "grey", els...)
}

// ---------- alerts ----------

// push sends alerts and SMS to the owners in their chats with the bot; a card for an SMS gets
// buttons to answer or delete it
func (b *feishuBot) push(ctx context.Context, c Config, evs []event, dropped int64) (int, error) {
	card, used := feishuNext(c, evs, dropped)
	body := card["body"].(map[string]any)
	els := body["elements"].([]any)
	if e := evs[0]; used == 1 && e.SMS != nil {
		id := strconv.FormatInt(e.SMS.ID, 10)
		els = append(els, btnRow(b.btn("sms.reply", "default", "a", "sms_compose", "n", e.SMS.From),
			b.btn("sms.delete", "default", "a", "sms_del", "id", id), b.btn("bot.btn.sms", "default", "a", "sms")))
	} else if evs[0].State != "test" {
		els = append(els, btnRow(b.btn("bot.btn.status", "default", "a", "status")))
	}
	body["elements"] = els
	for _, u := range c.FeishuBotUsers {
		if _, err := b.sendCard(ctx, "open_id", u, card); err != nil {
			return 0, err
		}
	}
	return used, nil
}
