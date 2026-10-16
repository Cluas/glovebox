package main

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// mainGroup returns main_group when it exists, otherwise the first visible Selector in config order
func mainGroup(ps map[string]mihomoProxy, preferred string) string {
	if _, ok := ps[preferred]; ok && preferred != "" {
		return preferred
	}
	for _, name := range groupOrder(ps) {
		if p := ps[name]; p.Type == "Selector" && !p.Hidden {
			return name
		}
	}
	return ""
}

// resolveNow follows the selections down from a group and returns the chain "group → … → node"
func resolveNow(ps map[string]mihomoProxy, name string) []string {
	var chain []string
	seen := map[string]bool{}
	start := name
	for name != "" && !seen[name] {
		seen[name] = true
		if name != start {
			chain = append(chain, name)
		}
		// nodes from proxy providers are not in /proxies; treat a miss as the final node
		p, ok := ps[name]
		if !ok || len(p.All) == 0 {
			break
		}
		name = p.Now
	}
	return chain
}

// groupOrder lists groups in config file order (GLOBAL.all is the config order)
func groupOrder(ps map[string]mihomoProxy) []string {
	var out []string
	seen := map[string]bool{}
	if g, ok := ps["GLOBAL"]; ok {
		for _, n := range g.All {
			if p, ok := ps[n]; ok && len(p.All) > 0 && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	var rest []string
	for n, p := range ps {
		if len(p.All) > 0 && !seen[n] && n != "GLOBAL" {
			rest = append(rest, n)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

type proxyNode struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Delay int    `json:"delay"`
	Alive bool   `json:"alive"`
	Group bool   `json:"group"`
}

type proxyGroup struct {
	Name  string      `json:"name"`
	Type  string      `json:"type"`
	Now   string      `json:"now"`
	Fixed string      `json:"fixed,omitempty"`
	Chain []string    `json:"chain"`
	Nodes []proxyNode `json:"nodes"`
}

func proxyDown(err error) error {
	return apiErr("proxy_down", "proxy is not running: "+err.Error())
}

func (s *server) handleProxies(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	ps, err := s.mihomo.proxies(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, proxyDown(err))
		return
	}
	cf, _ := s.mihomo.configs(ctx)
	// subscription nodes only show up in /providers/proxies; merge them for type and delay
	if pp, err := s.mihomo.providerProxies(ctx); err == nil {
		for n, p := range pp {
			if _, ok := ps[n]; !ok {
				ps[n] = p
			}
		}
	}
	// main group first, the rest in config order
	order := groupOrder(ps)
	if mg := mainGroup(ps, s.cfg.get().MainGroup); mg != "" {
		rest := []string{mg}
		for _, n := range order {
			if n != mg {
				rest = append(rest, n)
			}
		}
		order = rest
	}
	var groups []proxyGroup
	for _, name := range order {
		g := ps[name]
		if g.Hidden || strings.HasPrefix(name, "__") {
			continue
		}
		pg := proxyGroup{Name: name, Type: g.Type, Now: g.Now, Fixed: g.Fixed, Chain: resolveNow(ps, name)}
		for _, n := range g.All {
			p := ps[n]
			pg.Nodes = append(pg.Nodes, proxyNode{Name: n, Type: p.Type, Delay: p.lastDelay(), Alive: p.Alive, Group: len(p.All) > 0})
		}
		groups = append(groups, pg)
	}
	var mode any
	if cf != nil {
		mode = cf["mode"]
	}
	pp, _ := s.mihomo.providers(ctx, "proxies")
	rp, _ := s.mihomo.providers(ctx, "rules")
	type prov struct {
		Name      string    `json:"name"`
		Kind      string    `json:"kind"`
		Count     int       `json:"count"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	var provs []prov
	for _, p := range pp {
		provs = append(provs, prov{p.Name, "proxies", len(p.Proxies), p.UpdatedAt})
	}
	for _, p := range rp {
		provs = append(provs, prov{p.Name, "rules", p.RuleCount, p.UpdatedAt})
	}
	sort.Slice(provs, func(i, j int) bool {
		if provs[i].Kind != provs[j].Kind {
			return provs[i].Kind < provs[j].Kind
		}
		return provs[i].Name < provs[j].Name
	})
	writeJSON(w, http.StatusOK, map[string]any{"mode": mode, "groups": groups, "providers": provs})
}

func (s *server) handleSelect(w http.ResponseWriter, r *http.Request) {
	var in struct{ Group, Name string }
	if err := readBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.mihomo.selectProxy(r.Context(), in.Group, in.Name); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	log.Printf("select %s -> %s", in.Group, in.Name)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleDelay(w http.ResponseWriter, r *http.Request) {
	var in struct{ Group, Name string }
	if err := readBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	testURL := s.cfg.get().DelayURL
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if in.Group != "" {
		res, err := s.mihomo.groupDelay(ctx, in.Group, testURL, 5000)
		if err != nil {
			// mihomo also returns an error when every node times out; report them all as dead
			writeJSON(w, http.StatusOK, map[string]any{"delays": map[string]int{}, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"delays": res})
		return
	}
	d, err := s.mihomo.proxyDelay(ctx, in.Name, testURL, 5000)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"delays": map[string]int{}, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"delays": map[string]int{in.Name: d}})
}

func (s *server) handleMode(w http.ResponseWriter, r *http.Request) {
	var in struct{ Mode string }
	if err := readBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	switch in.Mode {
	case "rule", "global", "direct":
	default:
		writeErr(w, http.StatusBadRequest, apiErr("mode_invalid", "mode must be rule, global or direct"))
		return
	}
	if err := s.mihomo.patchConfigs(r.Context(), map[string]any{"mode": in.Mode}); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	log.Printf("proxy mode -> %s", in.Mode)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleUpdateProviders refreshes subscriptions; an empty name updates every proxy and rule provider
func (s *server) handleUpdateProviders(w http.ResponseWriter, r *http.Request) {
	var in struct{ Kind, Name string }
	if err := readBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	type result struct {
		Name  string `json:"name"`
		Kind  string `json:"kind"`
		Error string `json:"error,omitempty"`
	}
	var targets []result
	if in.Name != "" {
		if in.Kind != "proxies" && in.Kind != "rules" {
			writeErr(w, http.StatusBadRequest, apiErr("kind_invalid", "kind must be proxies or rules"))
			return
		}
		targets = append(targets, result{Name: in.Name, Kind: in.Kind})
	} else {
		for _, kind := range []string{"proxies", "rules"} {
			ps, err := s.mihomo.providers(ctx, kind)
			if err != nil {
				writeErr(w, http.StatusBadGateway, proxyDown(err))
				return
			}
			for _, p := range ps {
				targets = append(targets, result{Name: p.Name, Kind: kind})
			}
		}
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range targets {
		wg.Add(1)
		go func(t *result) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := s.mihomo.updateProvider(ctx, t.Kind, t.Name); err != nil {
				t.Error = err.Error()
			}
		}(&targets[i])
	}
	wg.Wait()
	failed := 0
	for _, t := range targets {
		if t.Error != "" {
			failed++
		}
	}
	log.Printf("update providers: %d total, %d failed", len(targets), failed)
	writeJSON(w, http.StatusOK, map[string]any{"results": targets, "failed": failed})
}

func (s *server) handleCloseConns(w http.ResponseWriter, r *http.Request) {
	if err := s.mihomo.closeConnections(r.Context()); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleConnections(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	cs, err := s.mihomo.connections(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, proxyDown(err))
		return
	}
	type conn struct {
		Host     string `json:"host"`
		Source   string `json:"source"`
		Network  string `json:"network"`
		Chain    string `json:"chain"`
		Rule     string `json:"rule"`
		Upload   int64  `json:"upload"`
		Download int64  `json:"download"`
		Start    int64  `json:"start"`
	}
	var out []conn
	for _, c := range cs.Connections {
		host := c.Metadata.Host
		if host == "" {
			host = c.Metadata.SniffHost
		}
		if host == "" {
			host = c.Metadata.DestIP
		}
		host += ":" + c.Metadata.DestPort
		chain := make([]string, len(c.Chains))
		for i, n := range c.Chains {
			chain[len(c.Chains)-1-i] = n
		}
		rule := c.Rule
		if c.Payload != "" {
			rule += "," + c.Payload
		}
		out = append(out, conn{host, c.Metadata.SourceIP, c.Metadata.Network, strings.Join(chain, " → "), rule, c.Upload, c.Download, c.Start.Unix()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start > out[j].Start })
	writeJSON(w, http.StatusOK, map[string]any{"connections": out})
}

func (s *server) handleTSCheck(w http.ResponseWriter, r *http.Request) {
	s.ts.check(r.Context())
	writeJSON(w, http.StatusOK, s.ts.status())
}
