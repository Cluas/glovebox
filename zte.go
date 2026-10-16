package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// zteClient calls the vendor goform API of com.zte.web (the same one the 192.168.0.1 web UI uses).
//
// Login: GET cmd=LD for a nonce, then password = SHA256U(login_pwd + LD), where login_pwd is the
// SHA256U(admin password) stored in web_settings.db. Write requests also need
// AD = SHA256U(SHA256U(wa_inner_version + cr_version) + RD).
// The vendor stack counts failed logins and locks out, so a hash that failed once is never retried.
type zteClient struct {
	base   string
	dbPath string
	hc     *http.Client

	mu      sync.Mutex
	verHash string
	badHash string
}

func newZTE(base, dbPath string) *zteClient {
	return &zteClient{
		base:   strings.TrimRight(base, "/"),
		dbPath: dbPath,
		hc:     &http.Client{Timeout: 8 * time.Second},
	}
}

func sha256U(s string) string {
	h := sha256.Sum256([]byte(s))
	return strings.ToUpper(hex.EncodeToString(h[:]))
}

func (z *zteClient) do(ctx context.Context, method, path string, form url.Values) (map[string]any, error) {
	var body io.Reader
	u := z.base + path
	if method == http.MethodGet {
		u += "?" + form.Encode()
	} else {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	// the vendor stack rejects requests without a Referer
	req.Header.Set("Referer", "http://192.168.0.1/index.html")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	}
	resp, err := z.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("goform %s: HTTP %d", path, resp.StatusCode)
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("goform %s: %v", path, err)
	}
	return m, nil
}

// get reads a set of fields, using multi_data=1 when there is more than one
func (z *zteClient) get(ctx context.Context, cmds ...string) (map[string]any, error) {
	f := url.Values{"isTest": {"false"}, "cmd": {strings.Join(cmds, ",")}}
	if len(cmds) > 1 {
		f.Set("multi_data", "1")
	}
	return z.do(ctx, http.MethodGet, "/goform/goform_get_cmd_process", f)
}

var loginPwdRe = regexp.MustCompile(`login_pwd([0-9A-F]{64})`)

// loginHash reads login_pwd from web_settings.db, an SQLite file (journal_mode=delete) with a
// setting(key, value) table. There is no sqlite3 on the device, so it scans for "login_pwd"
// immediately followed by 64 hex digits (key and value are stored adjacently in the record).
func (z *zteClient) loginHash() (string, error) {
	b, err := os.ReadFile(z.dbPath)
	if err != nil {
		return "", err
	}
	ms := loginPwdRe.FindAllSubmatch(b, -1)
	if len(ms) == 0 {
		return "", errors.New("login_pwd not found in web_settings.db")
	}
	seen := map[string]bool{}
	var uniq []string
	for _, m := range ms {
		s := string(m[1])
		if !seen[s] {
			seen[s] = true
			uniq = append(uniq, s)
		}
	}
	if len(uniq) > 1 {
		return "", fmt.Errorf("web_settings.db has %d different login_pwd values, refusing to guess", len(uniq))
	}
	return uniq[0], nil
}

func (z *zteClient) ensureLogin(ctx context.Context) error {
	z.mu.Lock()
	defer z.mu.Unlock()
	m, err := z.get(ctx, "loginfo")
	if err != nil {
		return err
	}
	if str(m["loginfo"]) == "ok" {
		return nil
	}
	h, err := z.loginHash()
	if err != nil {
		return err
	}
	if h == z.badHash {
		return errors.New("vendor login failed before; not retrying to avoid a lockout (restart glovebox after changing the admin password)")
	}
	ld, err := z.get(ctx, "LD")
	if err != nil {
		return err
	}
	f := url.Values{
		"isTest":   {"false"},
		"goformId": {"LOGIN"},
		"password": {sha256U(h + str(ld["LD"]))},
	}
	r, err := z.do(ctx, http.MethodPost, "/goform/goform_set_cmd_process", f)
	if err != nil {
		return err
	}
	if res := str(r["result"]); res != "0" {
		z.badHash = h
		return fmt.Errorf("vendor login failed (result=%s)", res)
	}
	return nil
}

func (z *zteClient) adToken(ctx context.Context) (string, error) {
	z.mu.Lock()
	vh := z.verHash
	z.mu.Unlock()
	if vh == "" {
		m, err := z.get(ctx, "wa_inner_version", "cr_version")
		if err != nil {
			return "", err
		}
		vh = sha256U(str(m["wa_inner_version"]) + str(m["cr_version"]))
		z.mu.Lock()
		z.verHash = vh
		z.mu.Unlock()
	}
	rd, err := z.get(ctx, "RD")
	if err != nil {
		return "", err
	}
	return sha256U(vh + str(rd["RD"])), nil
}

// getAuthed reads fields that require a session (e.g. queryAccessPointInfo)
func (z *zteClient) getAuthed(ctx context.Context, cmds ...string) (map[string]any, error) {
	if err := z.ensureLogin(ctx); err != nil {
		return nil, err
	}
	return z.get(ctx, cmds...)
}

func (z *zteClient) set(ctx context.Context, goformID string, params url.Values) (map[string]any, error) {
	if err := z.ensureLogin(ctx); err != nil {
		return nil, err
	}
	ad, err := z.adToken(ctx)
	if err != nil {
		return nil, err
	}
	f := url.Values{}
	for k, v := range params {
		f[k] = v
	}
	f.Set("isTest", "false")
	f.Set("goformId", goformID)
	f.Set("AD", ad)
	r, err := z.do(ctx, http.MethodPost, "/goform/goform_set_cmd_process", f)
	if err != nil {
		return nil, err
	}
	if res := str(r["result"]); res != "success" && res != "0" {
		return r, fmt.Errorf("%s failed (result=%s)", goformID, res)
	}
	return r, nil
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	default:
		return fmt.Sprint(t)
	}
}
