package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	defaultTelegramAPI = "https://api.telegram.org"
	notifyQueueMax     = 50 // undelivered alerts kept per channel; older ones are dropped
	// Telegram allows 4096 characters per message after parsing the HTML; the markup is counted
	// too, which leaves a margin
	telegramMaxRunes = 3900
)

// outbound builds the HTTP client for every report (alerts and log shipping), through
// report_proxy when set
type outbound struct {
	mu    sync.Mutex
	proxy string
	hc    *http.Client
	err   error
}

func (o *outbound) client(proxy string) (*http.Client, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if (o.hc == nil && o.err == nil) || o.proxy != proxy {
		o.proxy = proxy
		o.hc, o.err = newOutboundClient(proxy)
	}
	return o.hc, o.err
}

func newOutboundClient(proxy string) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.MaxIdleConns = 4
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("report_proxy %q: want http://host:port or socks5://host:port", proxy)
		}
		switch u.Scheme {
		case "http", "https", "socks5":
		default:
			return nil, fmt.Errorf("report_proxy %q: unsupported scheme %q", proxy, u.Scheme)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: tr, Timeout: 20 * time.Second}, nil
}

type channelStatus struct {
	Enabled bool      `json:"enabled"`
	LastOK  time.Time `json:"last_ok,omitzero"`
	Error   string    `json:"error,omitempty"`
	Pending int64     `json:"pending"` // events for alerts, bytes for log shipping
	Dropped int64     `json:"dropped,omitempty"`
}

// channel delivers events from a cursor into the event log, so alerts raised while offline or
// right before a power cut go out once the device is back online
type channel struct {
	name    string
	enabled func(Config) bool
	wants   func(event) bool
	// send delivers a prefix of evs and returns its length
	send func(ctx context.Context, hc *http.Client, c Config, evs []event, dropped int64) (int, error)

	mu      sync.Mutex
	cursor  int64
	lastOK  time.Time
	err     string
	dropped int64 // reported with the next message
	lost    int64 // total, for the UI
}

type notifier struct {
	cfg    *configStore
	events *eventLog
	health *health
	out    *outbound
	path   string     // notify.json: the cursor of each channel
	saveMu sync.Mutex // the channels save from their own goroutines and share the temp file
	chans  []*channel
	// Bot API base URL; the demo and the tests point it at a fake server
	telegramAPI string
}

func newNotifier(cfg *configStore, events *eventLog, h *health, out *outbound, dataDir string) *notifier {
	n := &notifier{cfg: cfg, events: events, health: h, out: out, telegramAPI: defaultTelegramAPI}
	n.chans = []*channel{
		{
			name:    "telegram",
			enabled: Config.telegramEnabled,
			// started and clock_fixed are only logged: the device starts with every drive
			wants: func(e event) bool { return e.State != "info" },
			send: func(ctx context.Context, hc *http.Client, c Config, evs []event, dropped int64) (int, error) {
				return sendTelegram(ctx, hc, n.telegramAPI, c, evs, dropped)
			},
		},
		{
			name:    "webhook",
			enabled: func(c Config) bool { return c.WebhookURL != "" },
			wants:   func(event) bool { return true },
			send:    sendWebhook,
		},
	}
	last := events.lastSeq()
	saved := map[string]int64{}
	if dataDir != "" {
		n.path = filepath.Join(dataDir, "notify.json")
		if b, err := os.ReadFile(n.path); err == nil {
			json.Unmarshal(b, &saved)
		}
	}
	for _, ch := range n.chans {
		cur, ok := saved[ch.name]
		// without a saved cursor start from now instead of sending the whole history
		if !ok || cur > last {
			cur = last
		}
		ch.cursor = cur
	}
	return n
}

func (n *notifier) save() {
	if n.path == "" {
		return
	}
	n.saveMu.Lock()
	defer n.saveMu.Unlock()
	m := map[string]int64{}
	for _, ch := range n.chans {
		ch.mu.Lock()
		m[ch.name] = ch.cursor
		ch.mu.Unlock()
	}
	b, _ := json.Marshal(m)
	if err := writeFileAtomic(n.path, b, 0o600); err != nil {
		log.Printf("notify: %v", err)
	}
}

func (n *notifier) loop(ctx context.Context) {
	for _, ch := range n.chans {
		go n.run(ctx, ch)
	}
}

func (n *notifier) run(ctx context.Context, ch *channel) {
	wake := n.events.subscribe()
	var backoff time.Duration
	for {
		wait := time.Minute
		if err := n.flush(ctx, ch); err != nil {
			backoff = min(max(2*backoff, 15*time.Second), 5*time.Minute)
			wait = backoff
		} else {
			backoff = 0
		}
		retry := time.NewTimer(wait)
		for waiting := true; waiting; {
			select {
			case <-ctx.Done():
				retry.Stop()
				return
			case <-wake:
				// after a failure new events wait for the retry
				waiting = backoff > 0
			case <-retry.C:
				waiting = false
			}
		}
		retry.Stop()
	}
}

// pending returns the undelivered events the channel wants and the sequence number to advance to
// when they are all delivered
func (n *notifier) pending(ch *channel) ([]event, int64) {
	ch.mu.Lock()
	cur := ch.cursor
	ch.mu.Unlock()
	all := n.events.since(cur)
	if len(all) == 0 {
		return nil, cur
	}
	var evs []event
	for _, e := range all {
		if ch.wants(e) {
			evs = append(evs, e)
		}
	}
	return evs, all[len(all)-1].Seq
}

func (n *notifier) flush(ctx context.Context, ch *channel) error {
	c := n.cfg.get()
	if !ch.enabled(c) {
		// events from while the channel was off are not sent when it is turned on
		if last := n.events.lastSeq(); last != ch.cursorValue() {
			ch.mu.Lock()
			ch.cursor = last
			ch.mu.Unlock()
			n.save()
		}
		return nil
	}
	if !n.health.clockOK() {
		return nil
	}
	evs, end := n.pending(ch)
	ch.mu.Lock()
	if over := len(evs) - notifyQueueMax; over > 0 {
		ch.dropped += int64(over)
		ch.lost += int64(over)
		evs = evs[over:]
	}
	ch.mu.Unlock()
	advance := func(seq int64) {
		ch.mu.Lock()
		ch.cursor = seq
		ch.mu.Unlock()
		n.save()
	}
	if len(evs) == 0 {
		if end != ch.cursorValue() {
			advance(end)
		}
		return nil
	}
	hc, err := n.out.client(c.ReportProxy)
	if err != nil {
		ch.fail(err)
		return err
	}
	for len(evs) > 0 {
		ch.mu.Lock()
		dropped := ch.dropped
		ch.mu.Unlock()
		sent, err := ch.send(ctx, hc, c, evs, dropped)
		if sent > 0 {
			ch.mu.Lock()
			ch.dropped = 0
			ch.mu.Unlock()
			seq := evs[sent-1].Seq
			if sent == len(evs) {
				seq = end
			}
			advance(seq)
			evs = evs[sent:]
		}
		if err != nil {
			ch.fail(err)
			return err
		}
	}
	ch.ok()
	return nil
}

func (ch *channel) cursorValue() int64 {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.cursor
}

func (ch *channel) fail(err error) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.err == "" {
		log.Printf("%s alerts failing: %v", ch.name, err)
	}
	ch.err = err.Error()
}

func (ch *channel) ok() {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.err != "" {
		log.Printf("%s alerts delivered again", ch.name)
	}
	ch.err, ch.lastOK = "", time.Now()
}

func (n *notifier) status() map[string]channelStatus {
	c := n.cfg.get()
	out := map[string]channelStatus{}
	for _, ch := range n.chans {
		evs, _ := n.pending(ch)
		ch.mu.Lock()
		out[ch.name] = channelStatus{Enabled: ch.enabled(c), LastOK: ch.lastOK, Error: ch.err,
			Pending: int64(min(len(evs), notifyQueueMax)), Dropped: ch.lost}
		ch.mu.Unlock()
	}
	return out
}

// test sends a test message through every enabled channel; the result maps channel to error
func (n *notifier) test(ctx context.Context) map[string]error {
	c := n.cfg.get()
	out := map[string]error{}
	hc, err := n.out.client(c.ReportProxy)
	for _, ch := range n.chans {
		if !ch.enabled(c) {
			continue
		}
		if err != nil {
			out[ch.name] = err
			continue
		}
		e := event{Time: time.Now(), Check: "test", State: "test"}
		_, out[ch.name] = ch.send(ctx, hc, c, []event{e}, 0)
		if out[ch.name] == nil {
			ch.ok()
		}
	}
	return out
}

// ---------- message text ----------

func fmtDuration(sec int64) string {
	sec = max(sec, 0)
	d, h, m := sec/86400, sec%86400/3600, sec%3600/60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh", d, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%ds", sec)
}

// eventText is the one-line, translated description of an event
func eventText(lang, device string, e event) string {
	check := tr(lang, "hc."+e.Check, nil)
	switch e.State {
	case "fail":
		return tr(lang, "alert.fail", map[string]string{"check": check})
	case "ok":
		return tr(lang, "alert.ok", map[string]string{"check": check, "dur": fmtDuration(e.Dur)})
	case "test":
		return tr(lang, "alert.test", map[string]string{"device": device})
	}
	return tr(lang, "ev."+e.Check, map[string]string{"msg": e.Msg, "dur": fmtDuration(e.Dur)})
}

func telegramBlock(lang, device string, e event, withLog bool) string {
	mark := map[string]string{"fail": "\U0001F534", "ok": "\U0001F7E2", "test": "\U0001F514"}[e.State]
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>%s</b>", mark, html.EscapeString(eventText(lang, device, e)))
	if e.State != "test" {
		fmt.Fprintf(&b, " · %s", e.Time.In(time.Local).Format("01-02 15:04:05"))
	}
	if e.Msg != "" && e.State == "fail" {
		fmt.Fprintf(&b, "\n<code>%s</code>", html.EscapeString(e.Msg))
	}
	if withLog && len(e.Log) > 0 {
		fmt.Fprintf(&b, "\n%s\n<pre>%s</pre>", html.EscapeString(tr(lang, "alert.log", nil)), html.EscapeString(strings.Join(e.Log, "\n")))
	}
	return b.String()
}

// telegramText renders as many events as fit in one message and returns how many it used
func telegramText(c Config, evs []event, dropped int64) (string, int) {
	lang := c.ReportLang
	head := "<b>" + html.EscapeString(c.DeviceName) + "</b>"
	if dropped > 0 {
		head += "\n" + html.EscapeString(tr(lang, "alert.dropped", map[string]string{"n": fmt.Sprint(dropped)}))
	}
	text, used := head, 0
	for _, e := range evs {
		blk := telegramBlock(lang, c.DeviceName, e, true)
		if utf8.RuneCountInString(text)+1+utf8.RuneCountInString(blk) > telegramMaxRunes {
			blk = telegramBlock(lang, c.DeviceName, e, false)
		}
		if utf8.RuneCountInString(text)+1+utf8.RuneCountInString(blk) > telegramMaxRunes {
			if used > 0 {
				break
			}
			// a single event that does not fit even without its log: cut the error text
			e.Msg = string([]rune(e.Msg)[:min(utf8.RuneCountInString(e.Msg), 1000)])
			blk = telegramBlock(lang, c.DeviceName, e, false)
		}
		text += "\n" + blk
		used++
	}
	return text, used
}

func sendTelegram(ctx context.Context, hc *http.Client, api string, c Config, evs []event, dropped int64) (int, error) {
	text, used := telegramText(c, evs, dropped)
	body, _ := json.Marshal(map[string]any{
		"chat_id": c.TelegramChat, "text": text, "parse_mode": "HTML", "disable_web_page_preview": true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api+"/bot"+c.TelegramToken+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return 0, scrub(err, c.TelegramToken)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, scrub(err, c.TelegramToken)
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&r)
	if !r.OK {
		if r.Description == "" {
			r.Description = resp.Status
		}
		return 0, errors.New("telegram: " + r.Description)
	}
	return used, nil
}

// scrub keeps the bot token, which is part of the API URL, out of error messages
func scrub(err error, secret string) error {
	if secret == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, "***"))
}

// sendWebhook posts each event as one JSON object
func sendWebhook(ctx context.Context, hc *http.Client, c Config, evs []event, _ int64) (int, error) {
	for i, e := range evs {
		body, _ := json.Marshal(map[string]any{
			"device": c.DeviceName, "seq": e.Seq, "time": e.Time, "check": e.Check, "state": e.State,
			"msg": e.Msg, "dur": e.Dur, "log": e.Log, "text": eventText(c.ReportLang, c.DeviceName, e),
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.WebhookURL, bytes.NewReader(body))
		if err != nil {
			return i, scrub(err, c.WebhookURL)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			return i, scrub(err, c.WebhookURL)
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return i, fmt.Errorf("webhook: %s", resp.Status)
		}
	}
	return len(evs), nil
}
