package main

import (
	"context"
	"sync"
	"time"
)

// tsProbe periodically fetches an address inside the tailnet (e.g. a home router) through mihomo's
// Tailscale outbound to tell whether the tailnet is reachable
type tsProbe struct {
	m      *mihomoClient
	cfg    *configStore
	mu     sync.Mutex
	online bool
	delay  int
	err    string
	at     time.Time
	run    sync.Mutex // one probe at a time
}

type tsStatus struct {
	Enabled bool   `json:"enabled"`
	Name    string `json:"name,omitempty"`
	Online  bool   `json:"online"`
	Delay   int    `json:"delay"`
	Error   string `json:"error,omitempty"`
	Checked string `json:"checked,omitempty"`
	Target  string `json:"target"`
}

func (t *tsProbe) check(ctx context.Context) {
	if !t.run.TryLock() {
		return
	}
	defer t.run.Unlock()
	c := t.cfg.get()
	if !c.tailscaleEnabled() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	d, err := t.m.proxyDelay(ctx, c.TailscaleProxy, c.TailscaleProbe, 8000)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.at = time.Now()
	t.online, t.delay, t.err = err == nil && d > 0, d, ""
	if err != nil {
		t.err = err.Error()
	}
}

func (t *tsProbe) status() tsStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.cfg.get()
	s := tsStatus{Enabled: c.tailscaleEnabled(), Name: c.TailscaleProxy, Online: t.online, Delay: t.delay, Error: t.err, Target: c.TailscaleProbe}
	if !t.at.IsZero() {
		s.Checked = t.at.Format("15:04:05")
	}
	return s
}

func (t *tsProbe) loop(ctx context.Context) {
	tk := time.NewTicker(time.Minute)
	defer tk.Stop()
	for {
		t.check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
	}
}
