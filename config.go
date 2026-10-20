package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sync"
)

// Config is stored in /data/adb/glovebox/config.json, outside the module directory so it survives
// module upgrades. Only fields that differ from the defaults are written, see save.
type Config struct {
	// HTTP listen addresses; 192.168.0.254 is an alias on br0 (the vendor DHCP pool ends at .253)
	Listen []string `json:"listen"`
	// HTTPS listen addresses (PWA and service workers need a secure context); empty disables HTTPS
	TLSListen []string `json:"tls_listen"`
	// custom certificate and key (PEM); when empty, a generated local CA issues certificates
	TLSCert string `json:"tls_cert"`
	TLSKey  string `json:"tls_key"`
	// console hostnames, included in issued certificates; the first one is used for the HTTPS URL.
	// DNS resolution is up to you (e.g. mihomo hosts)
	Hostnames []string `json:"hostnames"`
	// alias address added to AliasDev; empty disables it
	AliasAddr string `json:"alias_addr"`
	AliasDev  string `json:"alias_dev"`
	// only these source networks may connect (hotspot LAN and loopback)
	AllowCIDRs []string `json:"allow_cidrs"`

	PasswordHash  string `json:"password_hash"`
	SessionSecret string `json:"session_secret"`

	ZTEBase string `json:"zte_base"`
	ZTEDB   string `json:"zte_db"`

	MihomoAPI    string `json:"mihomo_api"`
	MihomoSecret string `json:"mihomo_secret"`
	// main proxy group shown on the overview; empty picks the first visible Selector
	MainGroup string `json:"main_group"`

	BoxDir string `json:"box_dir"`
	// periodically fetch TailscaleProbe through the mihomo outbound TailscaleProxy to check the tailnet;
	// enabled only when both are set
	TailscaleProxy string `json:"tailscale_proxy"`
	TailscaleProbe string `json:"tailscale_probe"`
	DelayURL       string `json:"delay_url"`

	// name prefixes of cellular WAN interfaces
	WANPrefixes []string `json:"wan_prefixes"`
	LANDev      string   `json:"lan_dev"`

	// health checks and reporting (health.go, notify.go, ship.go). DeviceName titles alerts and is
	// the "host" label of shipped logs
	DeviceName string `json:"device_name"`
	// HTTP or SOCKS5 proxy for every outbound report, e.g. mihomo's mixed port: Box lets the
	// device's own traffic to private networks bypass mihomo, so a home server is only reachable
	// through an explicit proxy
	ReportProxy string `json:"report_proxy"`
	// language of alert messages, one of the web/i18n locales
	ReportLang string `json:"report_lang"`
	// Telegram alerts, enabled when both are set
	TelegramToken string `json:"telegram_token"`
	TelegramChat  string `json:"telegram_chat"`
	// every health event is POSTed here as JSON
	WebhookURL string `json:"webhook_url"`
	// Loki push endpoint (Grafana Loki, VictoriaLogs, Grafana Alloy); user:password in the URL is
	// sent as basic auth
	LogPushURL string `json:"log_push_url"`
	// mihomo DNS listener queried by the dns check; empty skips the listener part of the check
	DNSProbe string `json:"dns_probe"`

	// remote upgrade (update.go): the signed manifest to poll, empty disables; it is fetched
	// through report_proxy like the reports
	UpdateURL string `json:"update_url"`
	// auto installs a newer version, notify only reports it, off stops checking
	UpdateMode string `json:"update_mode"`
	// how often to check, a Go duration of at least 5m
	UpdateInterval string `json:"update_interval"`
	// ed25519 public keys (base64) trusted besides the built-in ones, for self-built releases
	UpdatePubkeys []string `json:"update_pubkeys"`
}

func defaultConfig() Config {
	return Config{
		Listen:     []string{"192.168.0.254:80"},
		TLSListen:  []string{"192.168.0.254:443"},
		Hostnames:  []string{"glovebox.lan"},
		AliasAddr:  "192.168.0.254/32",
		AliasDev:   "br0",
		AllowCIDRs: []string{"192.168.0.0/24", "127.0.0.0/8"},
		// not 127.0.0.1: the vendor iptables rules only allow 192.168.0.0/24 to reach port 8080
		ZTEBase:     "http://192.168.0.1:8080",
		ZTEDB:       "/data/data/com.zte.web/databases/web_settings.db",
		MihomoAPI:   "http://127.0.0.1:9091",
		BoxDir:      "/data/adb/box",
		DelayURL:    "https://www.gstatic.com/generate_204",
		WANPrefixes: []string{"sipa_eth", "rmnet", "ccmni"},
		LANDev:      "br0",
		DeviceName:  "glovebox",
		ReportLang:  "en",
		// Box for Root redirects DNS to mihomo on this port
		DNSProbe:       "127.0.0.1:1053",
		UpdateURL:      defaultUpdateURL,
		UpdateMode:     "auto",
		UpdateInterval: "1h",
	}
}

func (c Config) tailscaleEnabled() bool {
	return c.TailscaleProxy != "" && c.TailscaleProbe != ""
}

func (c Config) telegramEnabled() bool {
	return c.TelegramToken != "" && c.TelegramChat != ""
}

type configStore struct {
	path string
	mu   sync.RWMutex
	c    Config
}

func loadConfig(path string) (*configStore, error) {
	c := defaultConfig()
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(b, &c); err != nil {
			return nil, err
		}
	}
	s := &configStore{path: path, c: c}
	if s.c.SessionSecret == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return nil, err
		}
		s.c.SessionSecret = hex.EncodeToString(buf)
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *configStore) get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.c
}

func (s *configStore) update(fn func(c *Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.c
	fn(&s.c)
	if err := s.save(); err != nil {
		s.c = old
		return err
	}
	return nil
}

// importJSON merges a JSON object into the configuration: present fields override, absent fields
// are kept, unknown fields are an error. Secrets cannot be changed this way.
func (s *configStore) importJSON(r io.Reader) error {
	b, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return err
	}
	for _, k := range []string{"password_hash", "session_secret"} {
		if _, ok := keys[k]; ok {
			return errors.New(k + " cannot be imported (use the passwd command)")
		}
	}
	// validate against a scratch copy first so a bad document never touches the stored config
	var scratch Config
	if err := decodeStrict(b, &scratch); err != nil {
		return err
	}
	return s.update(func(c *Config) { decodeStrict(b, c) })
}

func decodeStrict(b []byte, c *Config) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	next := *c
	if err := dec.Decode(&next); err != nil {
		return err
	}
	*c = next
	return nil
}

// redacted returns the configuration with secrets masked, for display
func (c Config) redacted() Config {
	if c.PasswordHash != "" {
		c.PasswordHash = "(set)"
	}
	if c.SessionSecret != "" {
		c.SessionSecret = "(set)"
	}
	if c.TelegramToken != "" {
		c.TelegramToken = "(set)"
	}
	// webhook URLs usually carry their secret in the path or query
	if c.WebhookURL != "" {
		c.WebhookURL = "(set)"
	}
	if u, err := url.Parse(c.LogPushURL); err == nil && u.User != nil {
		c.LogPushURL = u.Redacted()
	}
	return c
}

// save writes only fields that differ from the defaults, so changed defaults reach old installs.
// The caller holds the write lock (loadConfig runs before any concurrency). An empty path keeps the
// configuration in memory only (demo mode).
func (s *configStore) save() error {
	if s.path == "" {
		return nil
	}
	var cur, def map[string]any
	if err := roundTrip(s.c, &cur); err != nil {
		return err
	}
	if err := roundTrip(defaultConfig(), &def); err != nil {
		return err
	}
	for k, v := range cur {
		if reflect.DeepEqual(v, def[k]) {
			delete(cur, k)
		}
	}
	b, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func roundTrip(in any, out *map[string]any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
