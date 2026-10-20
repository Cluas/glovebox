package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLoki records the lines of every accepted push by source
type fakeLoki struct {
	mu    sync.Mutex
	fail  bool
	lines map[string][]string
	auth  string
	label map[string]string
}

func (f *fakeLoki) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		http.Error(w, "storage is full", http.StatusServiceUnavailable)
		return
	}
	if r.URL.Path != "/insert/loki/api/v1/push" || r.Header.Get("Content-Encoding") != "gzip" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	zr, err := gzip.NewReader(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var push struct{ Streams []lokiStream }
	if err := json.NewDecoder(zr).Decode(&push); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if u, p, ok := r.BasicAuth(); ok {
		f.auth = u + ":" + p
	}
	for _, s := range push.Streams {
		f.label = s.Stream
		for _, v := range s.Values {
			f.lines[s.Stream["source"]] = append(f.lines[s.Stream["source"]], v[1])
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// take returns and forgets the lines received for a source
func (f *fakeLoki) take(src string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := f.lines[src]
	delete(f.lines, src)
	return l
}

type shipEnv struct {
	t       *testing.T
	loki    *fakeLoki
	cfg     *configStore
	h       *health
	dataDir string
	logDir  string
	clash   string
}

func newShipEnv(t *testing.T) *shipEnv {
	loki := &fakeLoki{lines: map[string][]string{}}
	srv := httptest.NewServer(loki)
	t.Cleanup(srv.Close)
	e := &shipEnv{t: t, loki: loki, dataDir: t.TempDir(), logDir: t.TempDir()}
	h, _ := newTestHealth(t, "")
	e.h = h
	c := h.s.cfg.get()
	c.DeviceName = "car"
	c.LogPushURL = strings.Replace(srv.URL, "http://", "http://ship:secret@", 1) + "/insert/loki/api/v1/push"
	e.cfg = &configStore{c: c}
	os.MkdirAll(filepath.Join(c.BoxDir, "run"), 0o700)
	e.clash = filepath.Join(c.BoxDir, "run/clash.log")
	return e
}

func (e *shipEnv) shipper() *shipper {
	return newShipper(e.cfg, e.h.events, e.h, &outbound{}, e.dataDir, e.logDir)
}

func (e *shipEnv) round(sh *shipper) {
	e.t.Helper()
	if _, err := sh.round(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(s)
	f.Close()
}

func wantLines(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("shipped %q, want %q", got, want)
	}
}

func TestShipLogs(t *testing.T) {
	e := newShipEnv(t)
	sh := e.shipper()
	appendFile(t, e.clash, "line1\nline2\npart")
	e.round(sh)
	wantLines(t, e.loki.take("mihomo"), "line1", "line2")
	if e.loki.auth != "ship:secret" || e.loki.label["job"] != "glovebox" || e.loki.label["host"] != "car" {
		t.Fatalf("auth %q, labels %v", e.loki.auth, e.loki.label)
	}

	// a line is shipped once it is complete
	appendFile(t, e.clash, "ial\n")
	e.round(sh)
	wantLines(t, e.loki.take("mihomo"), "partial")

	// a failed push keeps the position
	appendFile(t, e.clash, "line3\n")
	e.loki.fail = true
	if _, err := sh.round(context.Background()); err == nil || !strings.Contains(sh.status().Error, "storage is full") {
		t.Fatalf("push error: %v, status %+v", err, sh.status())
	}
	if st := sh.status(); st.Pending != int64(len("line3\n")) {
		t.Fatalf("pending %d", st.Pending)
	}
	e.loki.fail = false
	e.round(sh)
	wantLines(t, e.loki.take("mihomo"), "line3")

	// NUL bytes left by a power cut are dropped
	appendFile(t, e.clash, "\x00\x00\x00line4\n\x00\n")
	e.round(sh)
	wantLines(t, e.loki.take("mihomo"), "line4")

	// truncated: read from the start
	os.WriteFile(e.clash, []byte("new1\n"), 0o600)
	e.round(sh)
	wantLines(t, e.loki.take("mihomo"), "new1")

	// renamed (Box keeps the previous log as a backup): finish the old file, then the new one
	appendFile(t, e.clash, "old-tail\n")
	os.Rename(e.clash, e.clash+".bak")
	os.WriteFile(e.clash, []byte("fresh\n"), 0o600)
	e.round(sh)
	e.round(sh)
	wantLines(t, e.loki.take("mihomo"), "old-tail", "fresh")

	// the position survives a restart
	appendFile(t, e.clash, "after-restart\n")
	sh2 := e.shipper()
	e.round(sh2)
	wantLines(t, e.loki.take("mihomo"), "after-restart")

	// events are shipped once, as JSON
	e.h.events.add(event{Check: "dns", State: "fail", Msg: "timeout"})
	e.round(sh2)
	e.round(sh2)
	got := e.loki.take("events")
	if len(got) != 1 || !strings.Contains(got[0], `"_msg":"dns fail: timeout"`) || !strings.Contains(got[0], `"check":"dns"`) {
		t.Fatalf("events shipped: %q", got)
	}

	// nothing is read while the clock is wrong
	appendFile(t, e.clash, "later\n")
	e.h.clockBad = true
	e.round(sh2)
	wantLines(t, e.loki.take("mihomo"))
	e.h.clockBad = false
	e.round(sh2)
	wantLines(t, e.loki.take("mihomo"), "later")
}

func TestShipStartsNearTheEnd(t *testing.T) {
	e := newShipEnv(t)
	big := strings.Repeat(strings.Repeat("x", 99)+"\n", 2000) // 200 KB
	appendFile(t, e.clash, big+"last\n")
	sh := e.shipper()
	for range 3 {
		e.round(sh)
	}
	got := e.loki.take("mihomo")
	if n := len(got); n == 0 || n > shipFirstTail/100+1 || got[n-1] != "last" {
		t.Fatalf("first run shipped %d lines ending %q", n, got[n-1])
	}
}

func TestShipBatchLimit(t *testing.T) {
	e := newShipEnv(t)
	sh := e.shipper()
	e.round(sh)                                                                 // starts at the end of the (missing) file
	appendFile(t, e.clash, strings.Repeat(strings.Repeat("y", 1023)+"\n", 600)) // 600 KB
	more, err := sh.round(context.Background())
	if err != nil || !more {
		t.Fatalf("first batch: more %v, err %v", more, err)
	}
	for more {
		if more, err = sh.round(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(e.loki.take("mihomo")); n != 600 {
		t.Fatalf("shipped %d lines, want 600", n)
	}
}

func TestLogValues(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	data := []byte(
		`time="2026-10-20T19:58:00.5+08:00" level=warning msg="a"` + "\n" +
			"  continuation of a\n" +
			`time="2025-01-20T00:00:09+08:00" level=info msg="before the clock was set"` + "\n" +
			strings.Repeat("z", shipLineMax+10) + "\n")
	v := logValues(data, now)
	if len(v) != 4 {
		t.Fatalf("%d values", len(v))
	}
	// lines without a plausible time of their own get the one of the line before
	at := strconv.FormatInt(time.Date(2026, 10, 20, 11, 58, 0, 5e8, time.UTC).UnixNano(), 10)
	for i := range v {
		if v[i][0] != at {
			t.Errorf("line %d stamped %s, want %s", i, v[i][0], at)
		}
	}
	if len(v[3][1]) != shipLineMax {
		t.Fatalf("long line kept %d bytes", len(v[3][1]))
	}
}
