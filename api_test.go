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
		{"/api/wifi", `{"chip":"1","enabled":true,"ssid":"x","password":"12345678","max_clients":999}`, "max_clients_range"},
		{"/api/wifi", `{"chip":"9","enabled":true}`, "wifi_chip_unknown"},
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
