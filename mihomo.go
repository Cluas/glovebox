package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// mihomoClient talks to mihomo's RESTful API (external-controller, bound to 127.0.0.1)
type mihomoClient struct {
	base   string
	secret string
	hc     *http.Client
}

func newMihomo(base, secret string) *mihomoClient {
	return &mihomoClient{
		base:   strings.TrimRight(base, "/"),
		secret: secret,
		hc:     &http.Client{Timeout: 20 * time.Second},
	}
}

type mihomoError struct {
	Status  int
	Message string
}

func (e *mihomoError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("mihomo HTTP %d", e.Status)
}

// call expects names in path to be url.PathEscape'd (node names contain emoji and spaces)
func (m *mihomoClient) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if m.secret != "" {
		req.Header.Set("Authorization", "Bearer "+m.secret)
	}
	resp, err := m.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		e := &mihomoError{Status: resp.StatusCode}
		var msg struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &msg) == nil {
			e.Message = msg.Message
		}
		return e
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

type mihomoProxy struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Now     string   `json:"now"`
	All     []string `json:"all"`
	Hidden  bool     `json:"hidden"`
	Fixed   string   `json:"fixed"`
	Alive   bool     `json:"alive"`
	History []struct {
		Delay int `json:"delay"`
	} `json:"history"`
}

func (p mihomoProxy) lastDelay() int {
	if len(p.History) == 0 {
		return -1
	}
	return p.History[len(p.History)-1].Delay
}

func (m *mihomoClient) version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	err := m.call(ctx, http.MethodGet, "/version", nil, &v)
	return v.Version, err
}

func (m *mihomoClient) proxies(ctx context.Context) (map[string]mihomoProxy, error) {
	var v struct {
		Proxies map[string]mihomoProxy `json:"proxies"`
	}
	err := m.call(ctx, http.MethodGet, "/proxies", nil, &v)
	return v.Proxies, err
}

func (m *mihomoClient) selectProxy(ctx context.Context, group, name string) error {
	return m.call(ctx, http.MethodPut, "/proxies/"+url.PathEscape(group), map[string]string{"name": name}, nil)
}

func delayQuery(testURL string, timeoutMs int) string {
	return "?" + url.Values{"url": {testURL}, "timeout": {fmt.Sprint(timeoutMs)}}.Encode()
}

// groupDelay tests every node in a group and returns node → delay in ms (unreachable nodes are omitted)
func (m *mihomoClient) groupDelay(ctx context.Context, group, testURL string, timeoutMs int) (map[string]int, error) {
	out := map[string]int{}
	err := m.call(ctx, http.MethodGet, "/group/"+url.PathEscape(group)+"/delay"+delayQuery(testURL, timeoutMs), nil, &out)
	return out, err
}

func (m *mihomoClient) proxyDelay(ctx context.Context, name, testURL string, timeoutMs int) (int, error) {
	var v struct {
		Delay int `json:"delay"`
	}
	err := m.call(ctx, http.MethodGet, "/proxies/"+url.PathEscape(name)+"/delay"+delayQuery(testURL, timeoutMs), nil, &v)
	return v.Delay, err
}

func (m *mihomoClient) configs(ctx context.Context) (map[string]any, error) {
	var v map[string]any
	err := m.call(ctx, http.MethodGet, "/configs", nil, &v)
	return v, err
}

func (m *mihomoClient) patchConfigs(ctx context.Context, patch map[string]any) error {
	return m.call(ctx, http.MethodPatch, "/configs", patch, nil)
}

type mihomoProvider struct {
	Name        string        `json:"name"`
	VehicleType string        `json:"vehicleType"`
	UpdatedAt   time.Time     `json:"updatedAt"`
	Proxies     []mihomoProxy `json:"proxies"`
	RuleCount   int           `json:"ruleCount"`
}

// providers returns only remote providers (vehicleType=HTTP); kind is proxies or rules
func (m *mihomoClient) providers(ctx context.Context, kind string) ([]mihomoProvider, error) {
	var v struct {
		Providers map[string]mihomoProvider `json:"providers"`
	}
	if err := m.call(ctx, http.MethodGet, "/providers/"+kind, nil, &v); err != nil {
		return nil, err
	}
	var out []mihomoProvider
	for name, p := range v.Providers {
		if p.VehicleType != "HTTP" {
			continue
		}
		p.Name = name
		out = append(out, p)
	}
	return out, nil
}

// providerProxies returns every node from all proxy providers (name → node)
func (m *mihomoClient) providerProxies(ctx context.Context) (map[string]mihomoProxy, error) {
	var v struct {
		Providers map[string]mihomoProvider `json:"providers"`
	}
	if err := m.call(ctx, http.MethodGet, "/providers/proxies", nil, &v); err != nil {
		return nil, err
	}
	out := map[string]mihomoProxy{}
	for _, p := range v.Providers {
		for _, n := range p.Proxies {
			out[n.Name] = n
		}
	}
	return out, nil
}

func (m *mihomoClient) updateProvider(ctx context.Context, kind, name string) error {
	return m.call(ctx, http.MethodPut, "/providers/"+kind+"/"+url.PathEscape(name), nil, nil)
}

type mihomoConns struct {
	DownloadTotal int64 `json:"downloadTotal"`
	UploadTotal   int64 `json:"uploadTotal"`
	Connections   []struct {
		Metadata struct {
			Network   string `json:"network"`
			SourceIP  string `json:"sourceIP"`
			Host      string `json:"host"`
			DestIP    string `json:"destinationIP"`
			DestPort  string `json:"destinationPort"`
			SniffHost string `json:"sniffHost"`
		} `json:"metadata"`
		Upload   int64     `json:"upload"`
		Download int64     `json:"download"`
		Start    time.Time `json:"start"`
		Chains   []string  `json:"chains"`
		Rule     string    `json:"rule"`
		Payload  string    `json:"rulePayload"`
	} `json:"connections"`
	Memory int64 `json:"memory"`
}

func (m *mihomoClient) connections(ctx context.Context) (*mihomoConns, error) {
	var v mihomoConns
	err := m.call(ctx, http.MethodGet, "/connections", nil, &v)
	return &v, err
}

func (m *mihomoClient) closeConnections(ctx context.Context) error {
	return m.call(ctx, http.MethodDelete, "/connections", nil, nil)
}
