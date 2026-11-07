package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// SMS goes through the vendor goform API, like the web UI on 192.168.0.1. The watcher turns each
// message that arrives into an "sms" event, so the alert channels forward it with the same queue
// and retries as health alerts.

const (
	smsEvery    = 15 * time.Second
	smsPage     = 100 // messages read per list, newest first
	smsTextMax  = 500 // characters per message sent
	smsBatchMax = 100 // ids per delete or mark-read request
)

// smsMessage is a message in the vendor store
type smsMessage struct {
	ID     int64     `json:"id"`
	Number string    `json:"number"`
	Text   string    `json:"text"`
	Code   string    `json:"code,omitempty"` // verification code found in Text
	State  string    `json:"state"`          // unread, read, sent, failed or draft
	Time   time.Time `json:"time,omitzero"`
}

// smsEvent is the message an "sms" event carries
type smsEvent struct {
	ID   int64     `json:"id"`
	From string    `json:"from"`
	Text string    `json:"text"`
	Code string    `json:"code,omitempty"`
	At   time.Time `json:"at,omitzero"`
}

var smsStates = map[string]string{"0": "read", "1": "unread", "2": "sent", "3": "failed", "4": "draft"}

// smsList reads the newest messages of the device store
func (z *zteClient) smsList(ctx context.Context, n int) ([]smsMessage, error) {
	if err := z.ensureLogin(ctx); err != nil {
		return nil, err
	}
	f := url.Values{
		"isTest": {"false"}, "multi_data": {"1"}, "cmd": {"sms_data_total"},
		"page": {"0"}, "data_per_page": {strconv.Itoa(n)}, "mem_store": {"1"}, "tags": {"100"},
		"order_by": {"order by id desc"},
	}
	m, err := z.do(ctx, http.MethodGet, "/goform/goform_get_cmd_process", f)
	if err != nil {
		return nil, err
	}
	raw, ok := m["messages"].([]any)
	if !ok {
		if _, has := m["messages"]; !has {
			return nil, errors.New("sms_data_total: no messages in the reply")
		}
		return nil, nil // an empty store answers "messages":""
	}
	var out []smsMessage
	for _, r := range raw {
		o, _ := r.(map[string]any)
		id, err := strconv.ParseInt(str(o["id"]), 10, 64)
		if err != nil {
			continue
		}
		text := decodeSMSText(str(o["content"]))
		st := smsStates[str(o["tag"])]
		if st == "" {
			st = "read"
		}
		msg := smsMessage{ID: id, Number: str(o["number"]), Text: text, State: st, Time: parseSMSDate(str(o["date"]))}
		if st == "unread" || st == "read" {
			msg.Code = smsCode(text)
		}
		out = append(out, msg)
	}
	return out, nil
}

// smsSend queues a message; the list shows it as sent or failed once the modem is done
func (z *zteClient) smsSend(ctx context.Context, number, text string) error {
	_, err := z.set(ctx, "SEND_SMS", url.Values{"Number": {number}, "MessageBody": {encodeUCS2(text)}})
	return err
}

// smsEach runs goformID (DELETE_SMS, SET_MSG_READ) for each id: the vendor takes one at a time
func (z *zteClient) smsEach(ctx context.Context, goformID string, ids []int64) error {
	for _, id := range ids {
		if _, err := z.set(ctx, goformID, url.Values{"msg_id": {strconv.FormatInt(id, 10)}, "notCallback": {"true"}}); err != nil {
			return err
		}
	}
	return nil
}

// decodeSMSText decodes the content field: base64 UTF-8 on the F50, UCS-2 hex on older ZTE
// firmware. Anything else is shown as it came.
func decodeSMSText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s)%4 == 0 && isHex(s) {
		if t, ok := decodeUCS2(s); ok {
			return t
		}
	}
	b64 := s
	if pad := len(b64) % 4; pad != 0 {
		b64 += strings.Repeat("=", 4-pad)
	}
	if b, err := base64.StdEncoding.DecodeString(b64); err == nil && utf8.Valid(b) {
		return string(b)
	}
	return s
}

func isHex(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

func decodeUCS2(s string) (string, bool) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return "", false
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
	}
	t := string(utf16.Decode(u))
	for _, r := range t {
		if r == utf8.RuneError || (unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t') {
			return "", false
		}
	}
	return t, true
}

// encodeUCS2 is the MessageBody of SEND_SMS: UTF-16 big endian, hex
func encodeUCS2(s string) string {
	var b strings.Builder
	for _, u := range utf16.Encode([]rune(s)) {
		fmt.Fprintf(&b, "%04X", u)
	}
	return b.String()
}

// parseSMSDate parses "26,11,07,10,30,05,+32": year, month, day, time and the zone in quarter
// hours. A date without a zone is local time.
func parseSMSDate(s string) time.Time {
	p := strings.Split(s, ",")
	if len(p) < 6 {
		return time.Time{}
	}
	var n [6]int
	for i := range n {
		v, err := strconv.Atoi(strings.TrimSpace(p[i]))
		if err != nil {
			return time.Time{}
		}
		n[i] = v
	}
	if n[0] < 100 {
		n[0] += 2000
	}
	loc := time.Local
	if len(p) > 6 {
		if q, err := strconv.Atoi(strings.TrimSpace(p[6])); err == nil {
			loc = time.FixedZone("", q*15*60)
		}
	}
	return time.Date(n[0], time.Month(n[1]), n[2], n[3], n[4], n[5], 0, loc)
}

var (
	// "verification code" and similar words in Chinese (simplified and traditional), and English
	smsCodeWord = regexp.MustCompile(`(?i)\x{9a8c}\x{8bc1}\x{7801}|\x{6821}\x{9a8c}\x{7801}|\x{52a8}\x{6001}\x{7801}|\x{786e}\x{8ba4}\x{7801}|\x{6fc0}\x{6d3b}\x{7801}|\x{5b89}\x{5168}\x{7801}|\x{52a8}\x{6001}\x{5bc6}\x{7801}|\x{9a57}\x{8b49}\x{78bc}|\x{8a8d}\x{8b49}\x{78bc}|code|otp|verif|passcode`)
	// 4 to 8 digits, or two groups of three such as WhatsApp's 123-456
	smsCodeNum = regexp.MustCompile(`\b\d{3}[- ]\d{3}\b|\b\d{4,8}\b`)
)

// smsCode finds the verification code in a message: the number nearest to a word such as
// "code". Messages without such a word have none.
func smsCode(text string) string {
	words := smsCodeWord.FindAllStringIndex(text, -1)
	if len(words) == 0 {
		return ""
	}
	best, bestDist := "", -1
	for _, m := range smsCodeNum.FindAllStringIndex(text, -1) {
		dist := -1
		for _, w := range words {
			d := max(w[0]-m[1], m[0]-w[1], 0)
			if dist < 0 || d < dist {
				dist = d
			}
		}
		if bestDist < 0 || dist < bestDist {
			best, bestDist = text[m[0]:m[1]], dist
		}
	}
	return strings.NewReplacer("-", "", " ", "").Replace(best)
}

// smsWatcher raises an "sms" event for every message that arrives
type smsWatcher struct {
	cfg    *configStore
	zte    *zteClient
	events *eventLog
	health *health
	path   string // sms.json

	mu      sync.Mutex
	st      smsState
	err     string
	checked time.Time
}

type smsState struct {
	// LastID is the highest message id seen; Seen is false until the first read, which only takes
	// note of the store so that old messages are not forwarded
	LastID int64 `json:"last_id"`
	Seen   bool  `json:"seen"`
}

func newSMSWatcher(cfg *configStore, z *zteClient, events *eventLog, h *health, dataDir string) *smsWatcher {
	w := &smsWatcher{cfg: cfg, zte: z, events: events, health: h}
	if dataDir != "" {
		w.path = filepath.Join(dataDir, "sms.json")
		if b, err := os.ReadFile(w.path); err == nil {
			json.Unmarshal(b, &w.st)
		}
	}
	return w
}

func (w *smsWatcher) loop(ctx context.Context) {
	t := time.NewTicker(smsEvery)
	defer t.Stop()
	for {
		w.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *smsWatcher) tick(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	msgs, err := w.zte.smsList(ctx, smsPage)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.checked = time.Now()
	if err != nil {
		if w.err != err.Error() {
			log.Printf("sms: %v", err)
		}
		w.err = err.Error()
		return
	}
	if w.err != "" {
		log.Print("sms: reading messages works again")
	}
	w.err = ""
	var top int64
	for _, m := range msgs {
		top = max(top, m.ID)
	}
	switch {
	case !w.st.Seen:
		w.st = smsState{LastID: top, Seen: true}
		w.save()
		return
	case top < w.st.LastID:
		// the store was reset (new SIM, factory reset): start over without forwarding
		w.st.LastID = top
		w.save()
		return
	case top == w.st.LastID:
		return
	}
	var fresh []smsMessage
	for _, m := range msgs {
		if m.ID > w.st.LastID && (m.State == "unread" || m.State == "read") {
			fresh = append(fresh, m)
		}
	}
	slices.SortFunc(fresh, func(a, b smsMessage) int { return int(a.ID - b.ID) })
	for _, m := range fresh {
		at := m.Time
		if at.IsZero() {
			at = time.Now()
		}
		w.events.add(event{Time: time.Now(), Check: "sms", State: "info", Msg: m.Number,
			SMS: &smsEvent{ID: m.ID, From: m.Number, Text: m.Text, Code: m.Code, At: at}})
		// the text stays out of the log, which is shipped
		log.Printf("sms: message %d from %s (%d characters)", m.ID, m.Number, utf8.RuneCountInString(m.Text))
	}
	w.st.LastID = top
	w.save()
}

func (w *smsWatcher) save() {
	if w.path == "" {
		return
	}
	b, _ := json.Marshal(w.st)
	if err := writeFileAtomic(w.path, b, 0o600); err != nil {
		log.Printf("sms: %v", err)
	}
}

// status is what the SMS panel shows about forwarding
func (w *smsWatcher) status() (checked time.Time, err string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.checked, w.err
}

// smsForwards reports whether new messages go to the named channel
func smsForwards(c Config, channel string) bool {
	return slices.Contains(c.SMSForward, channel)
}
