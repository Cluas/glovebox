package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
)

type testClient struct {
	t    *testing.T
	base string
	hc   *http.Client
}

func newTestClient(t *testing.T) *testClient {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s, err := newDemoServer(ctx, t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	return &testClient{t: t, base: ts.URL, hc: &http.Client{Jar: jar}}
}

func (c *testClient) do(method, path, body string, csrf bool) (int, map[string]any) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if csrf {
		req.Header.Set("X-Glovebox", "1")
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	var out map[string]any
	json.Unmarshal(b, &out)
	return res.StatusCode, out
}

func TestAPIAuthAndErrors(t *testing.T) {
	c := newTestClient(t)

	if code, out := c.do("GET", "/api/status", "", false); code != http.StatusUnauthorized || out["code"] != "unauthorized" {
		t.Fatalf("status before login: %d %v", code, out)
	}
	if code, _ := c.do("POST", "/api/login", `{"password":"glovebox"}`, false); code != http.StatusForbidden {
		t.Fatalf("login without X-Glovebox: %d, want 403", code)
	}
	if code, out := c.do("POST", "/api/login", `{"password":"nope"}`, true); code != http.StatusUnauthorized || out["code"] != "password_wrong" {
		t.Fatalf("wrong password: %d %v", code, out)
	}
	if code, out := c.do("POST", "/api/login", `{"password":"`+demoPassword+`"}`, true); code != http.StatusOK {
		t.Fatalf("login: %d %v", code, out)
	}

	code, st := c.do("GET", "/api/status", "", false)
	if code != http.StatusOK {
		t.Fatalf("status: %d %v", code, st)
	}
	cell, _ := st["cell"].(map[string]any)
	if cell["sim"] != "READY" || cell["connected"] != true {
		t.Fatalf("unexpected cell status: %v", cell)
	}

	cases := []struct {
		path, body, code string
	}{
		{"/api/proxies/mode", `{"mode":"bogus"}`, "mode_invalid"},
		{"/api/console/password", `{"old":"glovebox","new":"abc"}`, "password_short"},
		{"/api/console/password", `{"old":"nope","new":"abcdef"}`, "old_password_wrong"},
		{"/api/wifi", `{"chip":"1","ssid":"x","password":"12345678","max_clients":999}`, "max_clients_range"},
		{"/api/wifi", `{"chip":"9"}`, "wifi_chip_unknown"},
		{"/api/wifi/band", `{"chip":"9"}`, "wifi_chip_unknown"},
		{"/api/proxies/mode", `not json`, "bad_request"},
	}
	for _, tc := range cases {
		if code, out := c.do("POST", tc.path, tc.body, true); code < 400 || out["code"] != tc.code {
			t.Errorf("%s %s: %d %v, want code %s", tc.path, tc.body, code, out, tc.code)
		}
	}

	// the demo reports to an in-process fake Telegram and Loki
	if code, out := c.do("POST", "/api/report/test", `{}`, true); code != http.StatusOK || fmt.Sprint(out["sent"]) != "[logs telegram]" {
		t.Errorf("report test: %d %v", code, out)
	}
	code, hl := c.do("GET", "/api/health", "", false)
	checks, _ := hl["checks"].([]any)
	events, _ := hl["events"].([]any)
	if code != http.StatusOK || len(checks) != len(healthChecks) || len(events) == 0 {
		t.Fatalf("health: %d %v", code, hl)
	}
	if h, _ := st["health"].(map[string]any); h == nil || h["seq"] != float64(len(events)) {
		t.Fatalf("status health: %v", st["health"])
	}

	if code, out := c.do("POST", "/api/proxies/mode", `{"mode":"global"}`, true); code != http.StatusOK {
		t.Fatalf("set mode: %d %v", code, out)
	}
	if _, px := c.do("GET", "/api/proxies", "", false); px["mode"] != "global" {
		t.Fatalf("mode not applied: %v", px["mode"])
	}
}

func TestReportTestWithoutChannels(t *testing.T) {
	s, err := newDemoServer(t.Context(), t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.c.TelegramToken, s.cfg.c.LogPushURL = "", ""
	rec := httptest.NewRecorder()
	s.handleReportTest(rec, httptest.NewRequest("POST", "/api/report/test", nil))
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusBadRequest || out["code"] != "report_off" {
		t.Fatalf("%d %v", rec.Code, out)
	}
}

// the F50 runs one band at a time: picking one turns the other off
func TestWiFiBands(t *testing.T) {
	c := newTestClient(t)
	if code, out := c.do("POST", "/api/login", `{"password":"`+demoPassword+`"}`, true); code != http.StatusOK {
		t.Fatalf("login: %d %v", code, out)
	}
	bands := func() string {
		t.Helper()
		code, out := c.do("GET", "/api/wifi", "", false)
		aps, _ := out["aps"].([]any)
		if code != http.StatusOK || len(aps) != 2 {
			t.Fatalf("wifi: %d %v", code, out)
		}
		on := ""
		for _, a := range aps {
			if ap := a.(map[string]any); ap["enabled"] == true {
				on += ap["chip"].(string)
			}
		}
		return fmt.Sprintf("active=%v on=%s", out["active"], on)
	}
	if got := bands(); got != "active=1 on=1" {
		t.Fatalf("demo starts with %s", got)
	}
	for _, tc := range []struct{ chip, want string }{
		{"0", "active=0 on=0"},
		{"", "active= on="},
		{"1", "active=1 on=1"},
	} {
		if code, out := c.do("POST", "/api/wifi/band", `{"chip":"`+tc.chip+`"}`, true); code != http.StatusOK {
			t.Fatalf("band %q: %d %v", tc.chip, code, out)
		}
		if got := bands(); got != tc.want {
			t.Fatalf("after band %q: %s, want %s", tc.chip, got, tc.want)
		}
	}
	// settings of the band that is off can be changed without turning it on
	if code, out := c.do("POST", "/api/wifi", `{"chip":"0","ssid":"Spare","password":"12345678"}`, true); code != http.StatusOK {
		t.Fatalf("save idle band: %d %v", code, out)
	}
	if got := bands(); got != "active=1 on=1" {
		t.Fatalf("saving the idle band switched bands: %s", got)
	}
}
