package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigSavesOnlyOverrides(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	cs, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.update(func(c *Config) { c.MainGroup = "Proxy" }); err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]any
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk["main_group"] != "Proxy" || onDisk["session_secret"] == nil {
		t.Fatalf("missing overrides: %v", onDisk)
	}
	if _, ok := onDisk["listen"]; ok {
		t.Fatalf("default field written: %v", onDisk)
	}
	again, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.get(); got.MainGroup != "Proxy" || got.Listen[0] != defaultConfig().Listen[0] {
		t.Fatalf("reload mismatch: %+v", got)
	}
}

func TestConfigImport(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	cs, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	before := cs.get()
	for _, bad := range []string{
		`{"password_hash": "x"}`,
		`{"session_secret": "x"}`,
		`{"no_such_field": 1}`,
		`{"listen": "not a list"}`,
	} {
		if err := cs.importJSON(strings.NewReader(bad)); err == nil {
			t.Errorf("import %s: want error", bad)
		}
	}
	if got := cs.get(); got.SessionSecret != before.SessionSecret || len(got.Listen) != len(before.Listen) {
		t.Fatal("failed import changed the config")
	}
	if err := cs.importJSON(strings.NewReader(`{"tailscale_proxy": "TS", "tailscale_probe": "http://100.64.0.1/"}`)); err != nil {
		t.Fatal(err)
	}
	if got := cs.get(); !got.tailscaleEnabled() || got.TailscaleProxy != "TS" {
		t.Fatalf("import not applied: %+v", got)
	}
}

func TestRedacted(t *testing.T) {
	c := Config{
		PasswordHash: "h", SessionSecret: "s", TelegramToken: "123:tok", TelegramChat: "42",
		WebhookURL: "https://hooks.example/abc?key=k", LogPushURL: "http://user:pw@logs.example:9428/insert/loki/api/v1/push",
		WhatsAppKey: "waha-key", FeishuWebhook: "https://open.feishu.cn/open-apis/bot/v2/hook/fs-tok", FeishuSecret: "fs-sec",
	}.redacted()
	b, _ := json.Marshal(c)
	for _, secret := range []string{`"h"`, `"s"`, "123:tok", "abc", "key=k", ":pw@", "waha-key", "fs-tok", "fs-sec"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("%s not masked: %s", secret, b)
		}
	}
	if c.TelegramChat != "42" || !strings.Contains(c.LogPushURL, "user:xxxxx@logs.example:9428") {
		t.Fatalf("masked too much: %+v", c)
	}
}
