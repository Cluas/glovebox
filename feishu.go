package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Feishu (Lark outside China) custom bot: a webhook that posts into the group chat the bot was
// added to. Alerts go out as cards, several to a card like Telegram's messages; an SMS gets a card
// of its own, so that its code shows in the notification.

const (
	// a request may carry 20 KB; text blocks are cut and alerts share a card up to this much JSON
	feishuTextRunes = 1500
	feishuLogRunes  = 3000
	feishuCardBytes = 12 << 10
)

func (c Config) feishuEnabled() bool {
	return c.FeishuWebhook != ""
}

// feishuSign is the signature a bot with signature verification wants for a request made at ts
// (Unix seconds): HMAC-SHA256 of nothing, keyed with "<ts>\n<secret>", in base64
func feishuSign(secret string, ts int64) string {
	h := hmac.New(sha256.New, []byte(strconv.FormatInt(ts, 10)+"\n"+secret))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// feishuJSON encodes without escaping <, > and &, which would triple the size of log lines
func feishuJSON(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// feishuText is a plain text block: card markdown would need every special character escaped
func feishuText(s, size, color string) map[string]any {
	t := map[string]any{"tag": "plain_text", "content": s}
	if size != "" {
		t["text_size"] = size
	}
	if color != "" {
		t["text_color"] = color
	}
	return map[string]any{"tag": "div", "text": t}
}

// cut shortens s to n runes; tail keeps the end instead, where the newest log lines are
func cut(s string, n int, tail bool) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if tail {
		return "…" + string(r[len(r)-n:])
	}
	return string(r[:n]) + "…"
}

// feishuTitle is the headline of an event and the color of a card that holds only it. An SMS
// with a code leads with the code: the chat list and notifications only show the summary.
func feishuTitle(lang, device string, e event) (title, subtitle, template string) {
	text := eventText(lang, device, e)
	switch {
	case e.SMS != nil && e.SMS.Code != "":
		return "\U0001F510 " + e.SMS.Code, "\U0001F4E9 " + text, "wathet"
	case e.SMS != nil:
		return "\U0001F4E9 " + text, "", "wathet"
	case e.Check == "rescue":
		return eventMark(e) + " " + text, "", "orange"
	case e.Check == "update" && e.State == "info":
		return eventMark(e) + " " + text, "", "turquoise"
	}
	template = map[string]string{"fail": "red", "ok": "green", "test": "blue"}[e.State]
	if template == "" {
		template = "grey"
	}
	return eventMark(e) + " " + text, "", template
}

// feishuDetails are the blocks under an event's headline; headline adds it, for a card that holds
// several events, and withLog the mihomo warnings
func feishuDetails(c Config, e event, headline, withLog bool) []any {
	at := e.Time.In(time.Local).Format("01-02 15:04:05")
	if e.SMS != nil {
		at = smsStamp(e)
	}
	var out []any
	if headline {
		title, _, _ := feishuTitle(c.ReportLang, c.DeviceName, e)
		out = append(out, feishuText(title+" · "+at, "heading-4", ""))
	} else {
		out = append(out, feishuText(c.DeviceName+" · "+at, "notation", "grey"))
	}
	if e.SMS != nil {
		out = append(out, feishuText(cut(e.SMS.Text, feishuTextRunes, false), "", ""))
	}
	if e.Msg != "" && e.State == "fail" {
		out = append(out, feishuText(cut(e.Msg, feishuTextRunes, false), "", "red"))
	}
	if withLog && len(e.Log) > 0 {
		out = append(out,
			feishuText(tr(c.ReportLang, "alert.log", nil), "notation", "grey"),
			feishuText(cut(strings.Join(e.Log, "\n"), feishuLogRunes, true), "notation", ""))
	}
	return out
}

func feishuCard(title, subtitle, template, summary string, body []any) map[string]any {
	header := map[string]any{"title": map[string]string{"tag": "plain_text", "content": title}, "template": template}
	if subtitle != "" {
		header["subtitle"] = map[string]string{"tag": "plain_text", "content": subtitle}
	}
	return map[string]any{
		"schema": "2.0", // Feishu 7.20 and later
		"config": map[string]any{"update_multi": true, "summary": map[string]string{"content": summary}},
		"header": header,
		"body":   map[string]any{"elements": body},
	}
}

// feishuNext renders the card for the front of evs and returns how many events it holds: an SMS,
// or the alerts up to the next SMS that fit. dropped notes alerts lost before them.
func feishuNext(c Config, evs []event, dropped int64) (map[string]any, int) {
	lang := c.ReportLang
	var body []any
	if dropped > 0 {
		body = append(body, feishuText(tr(lang, "alert.dropped", map[string]string{"n": fmt.Sprint(dropped)}), "notation", "orange"))
	}
	single := func(e event) map[string]any {
		title, subtitle, template := feishuTitle(lang, c.DeviceName, e)
		summary := title
		if subtitle != "" {
			summary += " · " + subtitle
		}
		details := feishuDetails(c, e, false, true)
		if len(feishuJSON(details)) > feishuCardBytes {
			details = feishuDetails(c, e, false, false)
		}
		return feishuCard(title, subtitle, template, summary, append(body, details...))
	}
	if evs[0].SMS != nil {
		return single(evs[0]), 1
	}
	used, size := 0, len(feishuJSON(body))
	var titles []string
	template := "grey"
	alerts := append(body, feishuText(c.DeviceName, "notation", "grey"))
	for _, e := range evs {
		if e.SMS != nil {
			break
		}
		blk := feishuDetails(c, e, true, true)
		if n := len(feishuJSON(blk)); size+n > feishuCardBytes {
			blk = feishuDetails(c, e, true, false)
		}
		n := len(feishuJSON(blk))
		if used > 0 && size+n > feishuCardBytes {
			break
		}
		alerts, size, used = append(alerts, blk...), size+n, used+1
		title, _, _ := feishuTitle(lang, c.DeviceName, e)
		titles = append(titles, title)
		switch {
		case e.State == "fail":
			template = "red"
		case e.State == "ok" && template != "red":
			template = "green"
		}
	}
	if used == 1 {
		return single(evs[0]), 1
	}
	title := tr(lang, "alert.batch", map[string]string{"n": fmt.Sprint(used)})
	return feishuCard(title, "", template, cut(strings.Join(titles, "; "), 120, false), alerts), used
}

// sendFeishu posts one card and returns how many events it held; flush calls it again for the rest
func sendFeishu(ctx context.Context, hc *http.Client, c Config, evs []event, dropped int64) (int, error) {
	card, used := feishuNext(c, evs, dropped)
	msg := map[string]any{"msg_type": "interactive", "card": card}
	if c.FeishuSecret != "" {
		ts := time.Now().Unix()
		msg["timestamp"], msg["sign"] = strconv.FormatInt(ts, 10), feishuSign(c.FeishuSecret, ts)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.FeishuWebhook, bytes.NewReader(feishuJSON(msg)))
	if err != nil {
		return 0, scrub(err, c.FeishuWebhook)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, scrub(err, c.FeishuWebhook)
	}
	defer resp.Body.Close()
	// StatusCode is the older form of code, still sent alongside it
	var r struct {
		Code       *int   `json:"code"`
		StatusCode *int   `json:"StatusCode"`
		Msg        string `json:"msg"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&r)
	if r.Code == nil {
		r.Code = r.StatusCode
	}
	if resp.StatusCode >= 300 || r.Code == nil || *r.Code != 0 {
		detail := r.Msg
		if detail == "" {
			detail = resp.Status
		}
		if r.Code != nil {
			detail += fmt.Sprintf(" (code %d)", *r.Code)
		}
		return 0, scrub(fmt.Errorf("feishu: %s", detail), c.FeishuWebhook)
	}
	return used, nil
}
