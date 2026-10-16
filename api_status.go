package main

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

var zteStatusFields = []string{
	"network_type", "signalbar", "network_provider", "network_provider_fullname", "spn_name_data",
	"ppp_status", "modem_main_state", "simcard_roam",
	"realtime_tx_thrpt", "realtime_rx_thrpt", "monthly_tx_bytes", "monthly_rx_bytes",
	"SSID1", "wifi_onoff_state",
	"rssi", "lte_rsrp", "lte_snr", "Z5g_rsrp", "Z5g_SINR", "Z5g_snr",
	"wan_active_band", "nr5g_action_band", "lte_ca_pcell_band", "wan_ipaddr",
	"cr_version", "wa_inner_version",
}

// zteStatus is cached for 2 seconds so several open pages don't overload the vendor service
func (s *server) zteStatus(ctx context.Context) (map[string]any, error) {
	s.zteMu.Lock()
	defer s.zteMu.Unlock()
	if time.Since(s.zteAt) < 2*time.Second {
		return s.zteCache, s.zteErr
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	m, err := s.zte.get(ctx, zteStatusFields...)
	if err == nil {
		for _, k := range []string{"station_list", "lan_station_list"} {
			if l, err2 := s.zte.get(ctx, k); err2 == nil {
				m[k] = l[k]
			}
		}
	}
	s.zteCache, s.zteErr, s.zteAt = m, err, time.Now()
	return m, err
}

// sigSample is recorded every 5 seconds; fields are nil (JSON null, a gap in the chart) without signal
type sigSample struct {
	RSRP *float64 `json:"rsrp"`
	SINR *float64 `json:"sinr"`
}

const sigHistoryLen = 120 // 10 minutes

func optNum(s string) *float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return nil
	}
	return &v
}

// sigLoop samples the vendor signal fields so the UI can show signal changes along a drive
func (s *server) sigLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		var smp sigSample
		if z, err := s.zteStatus(ctx); err == nil {
			rsrp, sinr := cellSignal(z)
			smp = sigSample{optNum(rsrp), optNum(sinr)}
		}
		s.sigMu.Lock()
		s.sigHist = append(s.sigHist, smp)
		if len(s.sigHist) > sigHistoryLen {
			s.sigHist = s.sigHist[len(s.sigHist)-sigHistoryLen:]
		}
		s.sigMu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *server) signalHistory() []sigSample {
	s.sigMu.Lock()
	defer s.sigMu.Unlock()
	return append([]sigSample(nil), s.sigHist...)
}

// cellSignal prefers 5G readings and falls back to LTE
func cellSignal(z map[string]any) (rsrp, sinr string) {
	rsrp, sinr = str(z["Z5g_rsrp"]), str(z["Z5g_SINR"])
	if rsrp == "" {
		rsrp, sinr = str(z["lte_rsrp"]), str(z["lte_snr"])
	}
	return
}

// netLabel maps the vendor network_type to a display generation or a code the UI translates
func netLabel(t string) string {
	switch strings.ToUpper(t) {
	case "SA", "5G":
		return "5G SA"
	case "ENDC", "LTE-NSA":
		return "5G NSA"
	case "LTE", "LTE_CA", "LTE_A", "EHRPD":
		return "4G"
	case "UMTS", "HSDPA", "HSUPA", "HSPA", "HSPA+", "DC", "DC-HSPA", "DC-HSPA+", "DC-HSDPA":
		return "3G"
	case "GSM", "GPRS", "EDGE":
		return "2G"
	case "", "NO SERVICE", "NO_SERVICE":
		return "NO_SERVICE"
	case "LIMITED_SERVICE", "LIMITED SERVICE":
		return "LIMITED"
	}
	return t
}

// simState maps modem_main_state to a code the UI translates; unknown states pass through
func simState(s string) string {
	switch s {
	case "modem_init_complete":
		return "READY"
	case "modem_sim_undetected":
		return "NO_SIM"
	case "modem_waitpin":
		return "PIN"
	case "modem_waitpuk":
		return "PUK"
	case "modem_sim_destroy":
		return "INVALID"
	case "modem_imsi_waitnck":
		return "NET_LOCK"
	}
	return s
}

func num(v any) float64 {
	f, _ := strconv.ParseFloat(str(v), 64)
	return f
}

type client struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
	MAC  string `json:"mac"`
	Via  string `json:"via"`
}

func parseClients(m map[string]any) []client {
	var out []client
	for key, via := range map[string]string{"station_list": "Wi-Fi", "lan_station_list": "USB"} {
		l, _ := m[key].([]any)
		for _, it := range l {
			e, _ := it.(map[string]any)
			if e == nil {
				continue
			}
			out = append(out, client{Name: str(e["hostname"]), IP: str(e["ip_addr"]), MAC: str(e["mac_addr"]), Via: via})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c := s.cfg.get()
	out := map[string]any{"time": time.Now().Format("2006-01-02 15:04:05"), "unix": time.Now().Unix()}
	errs := map[string]string{}

	z, err := s.zteStatus(ctx)
	if err != nil {
		errs["zte"] = err.Error()
	} else {
		band := str(z["nr5g_action_band"])
		if band == "" {
			band = str(z["wan_active_band"])
		}
		if band == "" {
			band = str(z["lte_ca_pcell_band"])
		}
		rsrp, sinr := cellSignal(z)
		operator := str(z["network_provider_fullname"])
		if operator == "" {
			operator = str(z["network_provider"])
		}
		ppp := str(z["ppp_status"])
		out["cell"] = map[string]any{
			"operator":     operator,
			"network_type": str(z["network_type"]),
			"label":        netLabel(str(z["network_type"])),
			"signalbar":    num(z["signalbar"]),
			"rsrp":         rsrp,
			"sinr":         sinr,
			"band":         band,
			"connected":    strings.Contains(ppp, "connected") && !strings.Contains(ppp, "disconnected"),
			"ppp_status":   ppp,
			"sim":          simState(str(z["modem_main_state"])),
			"roaming":      str(z["simcard_roam"]),
			"wan_ip":       str(z["wan_ipaddr"]),
		}
		out["month"] = map[string]any{"rx": num(z["monthly_rx_bytes"]), "tx": num(z["monthly_tx_bytes"])}
		cl := parseClients(z)
		out["clients"] = len(cl)
		out["client_list"] = cl
		out["wifi"] = map[string]any{"on": str(z["wifi_onoff_state"]) == "1", "ssid": str(z["SSID1"])}
		out["firmware"] = str(z["wa_inner_version"])
	}

	out["rates"] = s.net.rates()
	out["history"] = s.net.history()
	out["signal_history"] = s.signalHistory()
	si := s.sys()
	if len(si.Thermals) > 6 {
		si.Thermals = si.Thermals[:6]
	}
	out["battery"] = si.Battery
	out["thermals"] = si.Thermals
	out["mem"] = si.Mem
	out["uptime"] = map[string]any{"seconds": si.Uptime}
	out["load"] = si.Load

	proxy := map[string]any{"running": false}
	mctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if v, err := s.mihomo.version(mctx); err != nil {
		errs["mihomo"] = err.Error()
	} else {
		proxy["running"] = true
		proxy["version"] = v
		if cf, err := s.mihomo.configs(mctx); err == nil {
			proxy["mode"] = cf["mode"]
		}
		if ps, err := s.mihomo.proxies(mctx); err == nil {
			if g, ok := ps[mainGroup(ps, c.MainGroup)]; ok {
				proxy["group"] = g.Name
				proxy["now"] = resolveNow(ps, g.Name)
			}
		}
		if cs, err := s.mihomo.connections(mctx); err == nil {
			proxy["connections"] = len(cs.Connections)
			proxy["upload_total"] = cs.UploadTotal
			proxy["download_total"] = cs.DownloadTotal
			proxy["memory"] = cs.Memory
		}
	}
	out["proxy"] = proxy
	out["tailscale"] = s.ts.status()
	out["errors"] = errs
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleClients(w http.ResponseWriter, r *http.Request) {
	z, err := s.zteStatus(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"clients": parseClients(z)})
}
