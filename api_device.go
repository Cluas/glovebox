package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"
)

var maxClientRe = regexp.MustCompile(`MaximumSupportedClientNumber=(\d+)`)

// maxSupportedClients reads the SoftAP client limit the system allows
// (config_wifiHardwareSoftapMaxClientCount from the vendor overlays).
// dumpsys wifi is slow, so the value is cached for 10 minutes.
func (s *server) maxSupportedClients(ctx context.Context) int {
	if s.demo {
		return 32
	}
	s.capMu.Lock()
	defer s.capMu.Unlock()
	if s.capMax > 0 && time.Since(s.capAt) < 10*time.Minute {
		return s.capMax
	}
	out, _ := run(ctx, 10*time.Second, "dumpsys", "wifi")
	if m := maxClientRe.FindStringSubmatch(out); m != nil {
		s.capMax, _ = strconv.Atoi(m[1])
		s.capAt = time.Now()
	}
	if s.capMax <= 0 {
		return 10
	}
	return s.capMax
}

type apInfo struct {
	Chip      string `json:"chip"`
	Band      string `json:"band"`
	Enabled   bool   `json:"enabled"`
	SSID      string `json:"ssid"`
	Password  string `json:"password"`
	AuthMode  string `json:"auth_mode"`
	Hidden    bool   `json:"hidden"`
	MaxClient string `json:"max_clients"`
}

func (s *server) queryAPs(ctx context.Context) ([]map[string]any, error) {
	m, err := s.zte.getAuthed(ctx, "queryAccessPointInfo")
	if err != nil {
		return nil, err
	}
	l, _ := m["ResponseList"].([]any)
	var out []map[string]any
	for _, it := range l {
		if e, ok := it.(map[string]any); ok && str(e["AccessPointIndex"]) == "0" {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("queryAccessPointInfo returned no access points")
	}
	return out, nil
}

func bandName(chip string) string {
	if chip == "1" {
		return "5 GHz"
	}
	return "2.4 GHz"
}

func (s *server) handleWiFi(w http.ResponseWriter, r *http.Request) {
	aps, err := s.queryAPs(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	var out []apInfo
	for _, e := range aps {
		pw, _ := base64.StdEncoding.DecodeString(str(e["Password"]))
		out = append(out, apInfo{
			Chip:      str(e["ChipIndex"]),
			Band:      bandName(str(e["ChipIndex"])),
			Enabled:   str(e["AccessPointSwitchStatus"]) == "1",
			SSID:      str(e["SSID"]),
			Password:  string(pw),
			AuthMode:  str(e["AuthMode"]),
			Hidden:    str(e["ApBroadcastDisabled"]) == "1",
			MaxClient: str(e["ApMaxStationNumber"]),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"aps": out, "max_supported": s.maxSupportedClients(r.Context())})
}

// handleSetWiFi changes one band's switch / SSID / password / limits. The request mirrors
// what the vendor web UI sends (setAccessPointInfo in its service.js).
func (s *server) handleSetWiFi(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Chip     string `json:"chip"`
		Enabled  bool   `json:"enabled"`
		SSID     string `json:"ssid"`
		Password string `json:"password"`
		Hidden   bool   `json:"hidden"`
		// 0 means unchanged
		MaxClients int `json:"max_clients"`
	}
	if err := readBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx := r.Context()
	aps, err := s.queryAPs(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	var cur map[string]any
	for _, e := range aps {
		if str(e["ChipIndex"]) == in.Chip {
			cur = e
		}
	}
	if cur == nil {
		writeErr(w, http.StatusBadRequest, apiErr("wifi_chip_unknown", "no access point with ChipIndex="+in.Chip, "chip", in.Chip))
		return
	}
	sw := "0"
	if in.Enabled {
		sw = "1"
	}
	f := url.Values{
		"ChipIndex":               {in.Chip},
		"AccessPointIndex":        {"0"},
		"QrImageShow":             {str(cur["QrImageShow"])},
		"lan_sec_ssid_control":    {""},
		"wifi_syncparas_flag":     {""},
		"AccessPointSwitchStatus": {sw},
	}
	if sw == str(cur["AccessPointSwitchStatus"]) {
		// other fields are only sent when the switch is unchanged (same as the vendor UI)
		if len(in.SSID) < 1 || len(in.SSID) > 32 {
			writeErr(w, http.StatusBadRequest, apiErr("ssid_length", "SSID must be 1-32 bytes"))
			return
		}
		auth := str(cur["AuthMode"])
		hidden := "0"
		if in.Hidden {
			hidden = "1"
		}
		f.Set("SSID", in.SSID)
		f.Set("ApIsolate", str(cur["ApIsolate"]))
		f.Set("AuthMode", auth)
		f.Set("ApBroadcastDisabled", hidden)
		maxSta := str(cur["ApMaxStationNumber"])
		if in.MaxClients != 0 {
			if limit := s.maxSupportedClients(ctx); in.MaxClients < 1 || in.MaxClients > limit {
				writeErr(w, http.StatusBadRequest, apiErr("max_clients_range", fmt.Sprintf("max clients must be 1-%d", limit), "max", limit))
				return
			}
			maxSta = strconv.Itoa(in.MaxClients)
		}
		f.Set("ApMaxStationNumber", maxSta)
		switch auth {
		case "WPAPSKWPA2PSK", "WPA2PSK", "WPA3PSK", "WPA2PSKWPA3PSK":
			if len(in.Password) < 8 || len(in.Password) > 63 {
				writeErr(w, http.StatusBadRequest, apiErr("wifi_password_length", "Wi-Fi password must be 8-63 characters"))
				return
			}
			f.Set("EncrypType", str(cur["EncrypType"]))
			f.Set("Password", base64.StdEncoding.EncodeToString([]byte(in.Password)))
		default:
			f.Set("EncrypType", "NONE")
		}
	}
	if _, err := s.zte.set(ctx, "setAccessPointInfo", f); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	log.Printf("wifi chip=%s enabled=%v ssid=%q max=%d", in.Chip, in.Enabled, in.SSID, in.MaxClients)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleBoxRestart(w http.ResponseWriter, r *http.Request) {
	script := filepath.Join(s.cfg.get().BoxDir, "scripts/box.service")
	if s.demo {
		log.Printf("demo: skipping %s restart", script)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if err := startDetached("/system/bin/sh", script, "restart"); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	log.Printf("restarting proxy (%s restart)", script)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleReboot(w http.ResponseWriter, r *http.Request) {
	log.Printf("reboot requested by %s", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	if s.demo {
		return
	}
	go func() {
		time.Sleep(time.Second)
		if err := startDetached("/system/bin/svc", "power", "reboot", "glovebox"); err != nil {
			log.Printf("svc power reboot: %v, falling back to reboot", err)
			startDetached("/system/bin/reboot")
		}
	}()
}

// handleSetTime sets the device clock from the browser's clock. Without a SIM the device boots
// with its factory date, and every TLS handshake fails until the clock is fixed.
func (s *server) handleSetTime(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Unix int64 `json:"unix"`
	}
	if err := readBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	t := time.Unix(in.Unix, 0)
	if t.Year() < 2025 || t.Year() > 2100 {
		writeErr(w, http.StatusBadRequest, apiErr("time_invalid", "implausible time "+t.Format(time.DateTime), "time", t.Format(time.DateTime)))
		return
	}
	old := time.Now()
	if s.demo {
		log.Printf("demo: skipping clock change %s -> %s", old.Format(time.DateTime), t.Format(time.DateTime))
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	tv := syscall.NsecToTimeval(t.UnixNano())
	if err := syscall.Settimeofday(&tv); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	log.Printf("clock %s -> %s (from %s)", old.Format(time.DateTime), t.Format(time.DateTime), r.RemoteAddr)
	// the session expiry was computed with the old clock; a jump forward would log the caller out
	s.auth.issue(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handlePassword(w http.ResponseWriter, r *http.Request) {
	var in struct{ Old, New string }
	if err := readBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !checkPassword(s.cfg.get().PasswordHash, in.Old) {
		writeErr(w, http.StatusBadRequest, apiErr("old_password_wrong", "current password is wrong"))
		return
	}
	if len(in.New) < minPasswordLen {
		writeErr(w, http.StatusBadRequest, errPasswordShort())
		return
	}
	h, err := hashPassword(in.New)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.cfg.update(func(c *Config) { c.PasswordHash = h }); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	log.Printf("console password changed (from %s)", r.RemoteAddr)
	s.auth.issue(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
