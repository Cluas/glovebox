package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// shipper pushes the mihomo, Box and Glovebox logs and the health events to a Loki-compatible
// endpoint (Grafana Loki, VictoriaLogs, Grafana Alloy). The log files are the buffer: a read
// position only advances after the server accepted the batch and is persisted in ship.json, so
// nothing is lost while offline or across a power cut as long as the files are kept.

const (
	shipEvery     = 30 * time.Second
	shipBatchMax  = 256 << 10 // bytes read per source and push
	shipLineMax   = 8 << 10
	shipFirstTail = 64 << 10 // a source seen for the first time starts this far before its end
)

type shipCursor struct {
	Ino uint64 `json:"ino"`
	Off int64  `json:"off"`
}

type shipState struct {
	Sources  map[string]shipCursor `json:"sources"`
	EventSeq int64                 `json:"event_seq"`
}

type shipSource struct {
	name, path string
}

type lokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

type shipper struct {
	cfg     *configStore
	events  *eventLog
	health  *health
	out     *outbound
	path    string // ship.json
	sources []shipSource
	wake    chan struct{}

	mu      sync.Mutex
	st      shipState
	lastOK  time.Time
	err     string
	pending int64
}

func newShipper(cfg *configStore, events *eventLog, h *health, out *outbound, dataDir, logDir string) *shipper {
	box := cfg.get().BoxDir
	sh := &shipper{cfg: cfg, events: events, health: h, out: out, sources: []shipSource{
		{"mihomo", filepath.Join(box, "run/clash.log")},
		{"box", filepath.Join(box, "run/runs.log")},
		{"glovebox", filepath.Join(logDir, logFileName)},
	}, wake: make(chan struct{}, 1)}
	if dataDir != "" {
		sh.sources = append(sh.sources, shipSource{"diag", filepath.Join(dataDir, diagLogFile)})
		sh.path = filepath.Join(dataDir, "ship.json")
		if b, err := os.ReadFile(sh.path); err == nil {
			json.Unmarshal(b, &sh.st)
		}
	}
	if sh.st.Sources == nil {
		sh.st.Sources = map[string]shipCursor{}
	}
	return sh
}

func (sh *shipper) loop(ctx context.Context) {
	var backoff time.Duration
	for {
		wait := shipEvery
		more, err := sh.round(ctx)
		switch {
		case err != nil:
			backoff = min(max(2*backoff, 30*time.Second), 10*time.Minute)
			wait = backoff
		case more:
			backoff, wait = 0, 2*time.Second
		default:
			backoff = 0
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-sh.wake:
			t.Stop()
		}
	}
}

// soon ships without waiting for the next round, e.g. the results of a diagnostics job
func (sh *shipper) soon() {
	select {
	case sh.wake <- struct{}{}:
	default:
	}
}

// round pushes one batch; more reports that a source has data beyond it
func (sh *shipper) round(ctx context.Context) (more bool, err error) {
	c := sh.cfg.get()
	// lines read now would be stamped with the wrong time
	if c.LogPushURL == "" || !sh.health.clockOK() {
		return false, nil
	}
	sh.mu.Lock()
	st := shipState{Sources: map[string]shipCursor{}, EventSeq: sh.st.EventSeq}
	for k, v := range sh.st.Sources {
		st.Sources[k] = v
	}
	sh.mu.Unlock()

	now := time.Now()
	var streams []lokiStream
	var behind, unsent int64 // bytes left after this batch, and including it
	for _, src := range sh.sources {
		cur, known := st.Sources[src.name]
		data, next, left, capped, err := readSource(src.path, cur, known)
		if err != nil {
			log.Printf("log shipping: %s: %v", src.name, err)
			continue
		}
		st.Sources[src.name] = next
		behind += left
		unsent += left + int64(len(data))
		more = more || capped
		if vals := logValues(data, now); len(vals) > 0 {
			streams = append(streams, lokiStream{Stream: sh.labels(c, src.name), Values: vals})
		}
	}
	if evs := sh.events.since(st.EventSeq); len(evs) > 0 {
		vals := make([][2]string, 0, len(evs))
		for _, e := range evs {
			e.SMS = nil // the text of a message, which may be a verification code, stays on the device
			b, _ := json.Marshal(shippedEvent{Line: eventLine(e), event: e})
			vals = append(vals, [2]string{strconv.FormatInt(e.Time.UnixNano(), 10), string(b)})
		}
		streams = append(streams, lokiStream{Stream: sh.labels(c, "events"), Values: vals})
		st.EventSeq = evs[len(evs)-1].Seq
	}
	if len(streams) > 0 {
		hc, err := sh.out.client(c.ReportProxy)
		if err == nil {
			err = pushLoki(ctx, hc, c.LogPushURL, streams)
		}
		if err != nil {
			sh.fail(err, unsent)
			return false, err
		}
	}
	sh.commit(st, behind, len(streams) > 0)
	return more, nil
}

// shippedEvent is an event as a JSON log line. VictoriaLogs takes the message from _msg and the
// other keys as fields; Loki keeps the whole line.
type shippedEvent struct {
	Line string `json:"_msg"`
	event
}

// eventLine is a short summary such as "dns fail: timeout"
func eventLine(e event) string {
	s := e.Check + " " + e.State
	if e.Msg != "" {
		s += ": " + e.Msg
	}
	return s
}

func (sh *shipper) labels(c Config, source string) map[string]string {
	return map[string]string{"job": "glovebox", "host": c.DeviceName, "source": source}
}

func (sh *shipper) commit(st shipState, behind int64, pushed bool) {
	sh.mu.Lock()
	changed := st.EventSeq != sh.st.EventSeq || len(st.Sources) != len(sh.st.Sources)
	for k, v := range st.Sources {
		changed = changed || sh.st.Sources[k] != v
	}
	sh.st, sh.pending = st, behind
	if pushed {
		if sh.err != "" {
			log.Printf("log shipping works again")
		}
		sh.err, sh.lastOK = "", time.Now()
	}
	b, _ := json.Marshal(st)
	sh.mu.Unlock()
	if changed && sh.path != "" {
		if err := writeFileAtomic(sh.path, b, 0o600); err != nil {
			log.Printf("log shipping: %v", err)
		}
	}
}

func (sh *shipper) fail(err error, unsent int64) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	// logged when the reason changes, not on every retry
	if sh.err != err.Error() {
		log.Printf("log shipping failing: %v", err)
	}
	sh.err, sh.pending = err.Error(), unsent
}

func (sh *shipper) status() channelStatus {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return channelStatus{Enabled: sh.cfg.get().LogPushURL != "", LastOK: sh.lastOK, Error: sh.err, Pending: sh.pending}
}

// test pushes one line with source="test"
func (sh *shipper) test(ctx context.Context) error {
	c := sh.cfg.get()
	hc, err := sh.out.client(c.ReportProxy)
	if err != nil {
		return err
	}
	line := eventText(c.ReportLang, c.DeviceName, event{State: "test"})
	err = pushLoki(ctx, hc, c.LogPushURL, []lokiStream{{
		Stream: sh.labels(c, "test"),
		Values: [][2]string{{strconv.FormatInt(time.Now().UnixNano(), 10), line}},
	}})
	if err == nil {
		sh.mu.Lock()
		sh.err, sh.lastOK = "", time.Now()
		sh.mu.Unlock()
	}
	return err
}

// pushLoki sends a gzip-compressed Loki push request; user:password in the URL becomes basic auth
func pushLoki(ctx context.Context, hc *http.Client, rawURL string, streams []lokiStream) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("log_push_url: %w", err)
	}
	user := u.User
	u.User = nil
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	json.NewEncoder(zw).Encode(map[string]any{"streams": streams})
	zw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	if user != nil {
		pw, _ := user.Password()
		req.SetBasicAuth(user.Username(), pw)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// readSource returns the next complete lines of a log and the cursor after them. A file replaced
// by a new one (Box renames the old mihomo log on start, Glovebox rotates to .1) is finished from
// its new name first; a file that got shorter was truncated and is read from the start.
func readSource(path string, cur shipCursor, known bool) (data []byte, next shipCursor, left int64, capped bool, err error) {
	fi, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, cur, 0, false, err
	}
	var ino uint64
	var size int64
	if fi != nil {
		ino, size = fileIno(fi), fi.Size()
	}
	if !known {
		cur = shipCursor{Ino: ino, Off: tailStart(path, size)}
	}
	if cur.Ino != 0 && cur.Ino != ino {
		if old := findIno(filepath.Dir(path), cur.Ino); old != "" {
			data, n, oldLeft, err := readChunk(old, cur.Off, true)
			if err == nil && n > 0 {
				return data, shipCursor{cur.Ino, cur.Off + n}, oldLeft + size, true, nil
			}
		}
		cur = shipCursor{Ino: ino}
	}
	if fi == nil {
		return nil, cur, 0, false, nil
	}
	cur.Ino = ino
	if size < cur.Off {
		cur.Off = 0
	}
	data, n, left, err := readChunk(path, cur.Off, false)
	return data, shipCursor{ino, cur.Off + n}, left, len(data) >= shipBatchMax-shipLineMax, err
}

// readChunk reads up to shipBatchMax bytes from off. Unless final (the file is no longer written)
// it stops after the last newline, leaving a line still being written for the next round.
func readChunk(path string, off int64, final bool) (data []byte, n, left int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, 0, err
	}
	want := min(fi.Size()-off, shipBatchMax)
	if want <= 0 {
		return nil, 0, 0, nil
	}
	buf := make([]byte, want)
	k, err := f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return nil, 0, 0, err
	}
	buf = buf[:k]
	if !final {
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			buf = buf[:i+1]
		} else if k < shipBatchMax {
			buf = buf[:0]
		}
	}
	return buf, int64(len(buf)), fi.Size() - off - int64(len(buf)), nil
}

// tailStart is the start of the first complete line in the last shipFirstTail bytes
func tailStart(path string, size int64) int64 {
	if size <= shipFirstTail {
		return 0
	}
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	buf := make([]byte, shipFirstTail)
	k, _ := f.ReadAt(buf, size-shipFirstTail)
	if i := bytes.IndexByte(buf[:k], '\n'); i >= 0 {
		return size - shipFirstTail + int64(i) + 1
	}
	return size
}

func findIno(dir string, ino uint64) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() && fileIno(fi) == ino {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

// logValues splits data into Loki values. A power cut can leave NUL bytes in a log, which are
// dropped. Each line is stamped with the time it carries (mihomo, Glovebox) when plausible, else
// with that of the line before it, else with now.
func logValues(data []byte, now time.Time) [][2]string {
	var out [][2]string
	last := now
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimRight(bytes.ReplaceAll(line, []byte{0}, nil), "\r")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if len(line) > shipLineMax {
			line = line[:shipLineMax]
		}
		s := strings.ToValidUTF8(string(line), "")
		if ts, ok := lineTime(s); ok && ts.After(now.AddDate(0, 0, -30)) && ts.Before(now.Add(time.Minute)) {
			last = ts
		}
		out = append(out, [2]string{strconv.FormatInt(last.UnixNano(), 10), s})
	}
	return out
}

func lineTime(s string) (time.Time, bool) {
	// mihomo: time="2026-10-20T14:02:31.123456+08:00" level=...
	// diagnostics: {"time":"2026-10-20T14:02:31.123456+08:00",...
	rest, ok := strings.CutPrefix(s, `time="`)
	if !ok {
		rest, ok = strings.CutPrefix(s, `{"time":"`)
	}
	if ok {
		if i := strings.IndexByte(rest, '"'); i > 0 {
			t, err := time.Parse(time.RFC3339Nano, rest[:i])
			return t, err == nil
		}
	}
	// Go's log package: 2026/10/20 14:02:31 ...
	const std = "2006/01/02 15:04:05"
	if len(s) >= len(std) && s[4] == '/' {
		t, err := time.ParseInLocation(std, s[:len(std)], time.Local)
		return t, err == nil
	}
	return time.Time{}, false
}
