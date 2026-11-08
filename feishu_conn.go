package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Feishu's long connection (the protocol of the official SDKs, ws/ in oapi-sdk-go): the device
// opens a WebSocket to Feishu and events come down it, so the bot works without a public address.
// The bot connects directly, not through report_proxy: it has to work when mihomo is down, to
// restart it.

const (
	feishuPingDefault = 2 * time.Minute
	feishuBackoffMax  = 2 * time.Minute
)

// variables for the tests
var (
	feishuBackoffMin = 5 * time.Second
	// Feishu sends an event again when it is not answered within 3 seconds
	feishuAnswerBudget = 2500 * time.Millisecond
	// how often the connection is checked
	feishuWatch = 5 * time.Second
)

type feishuClientConfig struct {
	ReconnectCount    int `json:"ReconnectCount"`
	ReconnectInterval int `json:"ReconnectInterval"`
	ReconnectNonce    int `json:"ReconnectNonce"`
	PingInterval      int `json:"PingInterval"`
}

// endpoint asks for the WebSocket URL of a new connection
func (b *feishuBot) endpoint(ctx context.Context) (string, feishuClientConfig, error) {
	c := b.s.cfg.get()
	var r struct {
		Data struct {
			URL          string             `json:"URL"`
			ClientConfig feishuClientConfig `json:"ClientConfig"`
		} `json:"data"`
	}
	err := b.post(ctx, http.MethodPost, "/callback/ws/endpoint", "",
		map[string]string{"AppID": c.FeishuAppID, "AppSecret": c.FeishuAppSecret}, &r)
	if err != nil {
		return "", feishuClientConfig{}, b.scrub(err)
	}
	if r.Data.URL == "" {
		return "", feishuClientConfig{}, errors.New("feishu: no connection URL in the answer")
	}
	return r.Data.URL, r.Data.ClientConfig, nil
}

// loop keeps the connection up while the bot is configured
func (b *feishuBot) loop(ctx context.Context) {
	backoff := feishuBackoffMin
	var lastErr string
	for ctx.Err() == nil {
		if !b.s.cfg.get().feishuBotEnabled() {
			if !sleepCtx(ctx, time.Minute) {
				return
			}
			continue
		}
		start := time.Now()
		err := b.session(ctx)
		b.mu.Lock()
		if b.connected {
			lastErr = "" // a connection that came up is always reported when it ends
		}
		b.mu.Unlock()
		b.setConn(false, err)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > 2*time.Minute {
			backoff = feishuBackoffMin
		}
		// the same failure every retry is logged once
		if msg := b.scrub(err).Error(); msg != lastErr {
			log.Printf("feishu: disconnected: %s", msg)
			lastErr = msg
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = min(2*backoff, feishuBackoffMax)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (b *feishuBot) setConn(up bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.connected = up
	b.err = ""
	if err != nil && !errors.Is(err, context.Canceled) {
		b.err = b.scrubLocked(err).Error()
	}
}

// scrubLocked is scrub for callers holding b.mu
func (b *feishuBot) scrubLocked(err error) error {
	return scrub(scrub(err, b.s.cfg.get().FeishuAppSecret), b.token)
}

// session runs one connection until it fails
func (b *feishuBot) session(ctx context.Context) error {
	rawURL, conf, err := b.endpoint(ctx)
	if err != nil {
		return err
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	service, _ := strconv.ParseInt(u.Query().Get("service_id"), 10, 32)
	conn, err := wsDial(ctx, b.wsc, rawURL)
	if err != nil {
		var he *wsHandshakeError
		if errors.As(err, &he) && he.Header.Get("Handshake-Msg") != "" {
			return fmt.Errorf("feishu: %s (%s)", he.Header.Get("Handshake-Msg"), he.Header.Get("Handshake-Status"))
		}
		// the URL carries a ticket
		return errors.New(strings.ReplaceAll(err.Error(), rawURL, "<endpoint>"))
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-sctx.Done()
		conn.Close()
	}()
	log.Printf("feishu: connected")
	b.setConn(true, nil)

	var mu sync.Mutex
	ping := feishuPingDefault
	if conf.PingInterval > 0 {
		ping = time.Duration(conf.PingInterval) * time.Second
	}
	lastRecv := time.Now()
	// sends a ping now and then at the interval Feishu asks for; a connection silent for two
	// intervals is dead
	go func() {
		var lastPing time.Time
		for {
			mu.Lock()
			every, quiet := ping, time.Since(lastRecv)
			mu.Unlock()
			if quiet > 2*every+5*time.Second {
				log.Printf("feishu: no answer for %s", quiet.Round(time.Second))
				cancel()
				return
			}
			if time.Since(lastPing) >= every {
				f := feishuFrame{Service: int32(service), Method: feishuControl, Headers: []feishuHeader{{"type", "ping"}}}
				if err := conn.WriteMessage(wsBinary, f.marshal()); err != nil {
					cancel()
					return
				}
				lastPing = time.Now()
			}
			if !sleepCtx(sctx, feishuWatch) {
				return
			}
		}
	}()

	parts := map[string][][]byte{}
	for {
		op, msg, err := conn.ReadMessage()
		if err != nil {
			if sctx.Err() != nil && ctx.Err() == nil {
				return errors.New("feishu: connection timed out")
			}
			return err
		}
		mu.Lock()
		lastRecv = time.Now()
		mu.Unlock()
		if op != wsBinary {
			continue
		}
		f, err := unmarshalFeishuFrame(msg)
		if err != nil {
			log.Printf("%v", err)
			continue
		}
		if f.Method == feishuControl {
			if f.header("type") == "pong" && len(f.Payload) > 0 {
				var cc feishuClientConfig
				if json.Unmarshal(f.Payload, &cc) == nil && cc.PingInterval > 0 {
					mu.Lock()
					ping = time.Duration(cc.PingInterval) * time.Second
					mu.Unlock()
				}
			}
			continue
		}
		payload := f.Payload
		if sum, _ := strconv.Atoi(f.header("sum")); sum > 1 {
			seq, _ := strconv.Atoi(f.header("seq"))
			id := f.header("message_id")
			if seq < 0 || seq >= sum || sum > 64 {
				continue
			}
			if len(parts) > 32 {
				clear(parts)
			}
			if parts[id] == nil {
				parts[id] = make([][]byte, sum)
			}
			parts[id][seq] = payload
			var whole []byte
			for _, p := range parts[id] {
				if p == nil {
					whole = nil
					break
				}
				whole = append(whole, p...)
			}
			if whole == nil {
				continue
			}
			delete(parts, id)
			payload = whole
		}
		switch f.header("type") {
		case "event", "card":
			go b.answer(sctx, conn, f, payload)
		}
	}
}

// answer handles an event and acknowledges it on its frame. A card action must be answered
// within the budget: when it takes longer, the answer is a toast and the card is updated later.
func (b *feishuBot) answer(ctx context.Context, conn *wsConn, f *feishuFrame, payload []byte) {
	start := time.Now()
	done := make(chan *cardAnswer, 1)
	// the work outlives this connection: restarting Box may drop it
	work := context.WithoutCancel(ctx)
	go func() { done <- b.handleEvent(work, payload) }()
	var ans *cardAnswer
	select {
	case ans = <-done:
	case <-time.After(feishuAnswerBudget):
		ans = b.working()
		go func() {
			late := <-done
			if late == nil || late.messageID == "" {
				return
			}
			if late.Card == nil && late.Toast != nil && late.Toast.Type == "error" {
				// the toast is gone with the reply; say it on the card
				late.Card = b.card(late.Toast.Content, "red", b.navRow(""))
			}
			if late.Card == nil {
				return
			}
			uctx, cancel := context.WithTimeout(work, 30*time.Second)
			defer cancel()
			if err := b.updateCard(uctx, late.messageID, late.Card); err != nil {
				log.Printf("feishu: update card: %v", err)
			}
		}()
	}
	resp := map[string]any{"code": http.StatusOK}
	if ans != nil && (ans.Toast != nil || ans.Card != nil) {
		data, _ := json.Marshal(ans)
		resp["data"] = data // []byte: base64 in JSON, as the SDKs send it
	}
	out := *f
	out.Headers = append(append([]feishuHeader{}, f.Headers...), feishuHeader{"biz_rt", strconv.FormatInt(time.Since(start).Milliseconds(), 10)})
	out.Payload, _ = json.Marshal(resp)
	if err := conn.WriteMessage(wsBinary, out.marshal()); err != nil {
		log.Printf("feishu: answer: %v", err)
	}
}
