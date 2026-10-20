package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Remote upgrade. The device sits behind cellular CGNAT and cannot be reached from outside, so it
// polls a signed manifest (update.json) instead: publishing a newer manifest is how a version is
// pushed. An upgrade downloads the module zip the manifest points to, checks it against the signed
// sha256, swaps the module files and restarts through the service.sh loop.
//
// The files it replaces are kept in update/prev. That copy of the old binary watches the new one
// (update-guard) and puts itself back unless the new one stays up for updateCommitAfter; service.sh
// does the same when the new binary keeps crashing after a power cut. Whether a version works is
// decided on the device alone, never by reaching the network, so a car out of coverage does not
// roll back a good version.

// versionCode orders builds (the build time in Unix seconds, set by build.sh); 0 for dev builds
var versionCode = "0"

func buildCode() int64 {
	n, _ := strconv.ParseInt(versionCode, 10, 64)
	return n
}

const (
	defaultUpdateURL      = "https://github.com/Cluas/glovebox/releases/latest/download/update.json"
	updateSigContext      = "glovebox-update-v1"
	updateManifestMax     = 64 << 10
	updateZipMax          = 64 << 20 // also the limit for each extracted file
	updateFirstCheck      = 2 * time.Minute
	updateMinInterval     = 5 * time.Minute
	updateCommitAfter     = time.Minute
	updateGuardTimeout    = 3 * time.Minute
	updateDownloadTimeout = 10 * time.Minute
	updateFailedKeep      = 10
)

// trustedUpdateKeys are the ed25519 public keys (base64) that sign official releases;
// update_pubkeys adds more, for self-built ones
var trustedUpdateKeys = []string{"Eur/3YRcuBNPA5NUyXF+prx3MEqSUeGbZaR4PfooTx4="}

// moduleFiles are taken from an update zip. system/ (the overlay) is left alone: it only applies
// after a reboot, and deploy.sh or a Magisk install update it.
var moduleFiles = []string{"module.prop", "service.sh", "uninstall.sh", "customize.sh", "bin/glovebox"}

// manifest is update.json. The field names follow Magisk's updateJson.
type manifest struct {
	Version     string `json:"version"`
	VersionCode int64  `json:"versionCode"`
	ZipURL      string `json:"zipUrl"` // may be relative to the manifest URL
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	Notes       string `json:"notes,omitempty"`
	Sig         string `json:"sig,omitempty"` // base64 ed25519 signature of signedText
}

var versionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)

// signedText is what the signature covers: everything but the zip location, so a release can be
// mirrored without signing it again
func (m manifest) signedText() []byte {
	return fmt.Appendf(nil, "%s\nversion=%s\nversionCode=%d\nsha256=%s\nsize=%d\nnotes=%q\n",
		updateSigContext, m.Version, m.VersionCode, m.SHA256, m.Size, m.Notes)
}

func (m manifest) verify(keys []ed25519.PublicKey) error {
	if !versionRe.MatchString(m.Version) {
		return fmt.Errorf("bad version %q", m.Version)
	}
	if m.VersionCode <= 0 {
		return errors.New("missing versionCode")
	}
	if b, err := hex.DecodeString(m.SHA256); err != nil || len(b) != sha256.Size || strings.ToLower(m.SHA256) != m.SHA256 {
		return errors.New("bad sha256")
	}
	if m.Size <= 0 || m.Size > updateZipMax {
		return fmt.Errorf("bad size %d", m.Size)
	}
	if m.ZipURL == "" {
		return errors.New("missing zipUrl")
	}
	sig, err := base64.StdEncoding.DecodeString(m.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("bad signature encoding")
	}
	msg := m.signedText()
	for _, k := range keys {
		if ed25519.Verify(k, msg, sig) {
			return nil
		}
	}
	return errors.New("signature does not match a trusted key")
}

func parseUpdateKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("update key %q: want a base64 ed25519 public key", s)
	}
	return ed25519.PublicKey(b), nil
}

// updateState is update/state.json: the last upgrade and its outcome
type updateState struct {
	State    string    `json:"state,omitempty"` // pending, committed or rolled_back
	From     string    `json:"from,omitempty"`
	FromCode int64     `json:"from_code,omitempty"`
	To       string    `json:"to,omitempty"`
	ToCode   int64     `json:"to_code,omitempty"`
	At       time.Time `json:"at,omitzero"`
	PID      int       `json:"pid,omitempty"` // the new binary, once it started
	Reason   string    `json:"reason,omitempty"`
	Noted    bool      `json:"noted,omitempty"` // the outcome is in the event log
	// versions that were rolled back: automatic upgrades skip them, an install by hand may retry
	Failed []int64 `json:"failed,omitempty"`
	// the last version announced in notify mode
	Notified int64 `json:"notified,omitempty"`
}

func readUpdateState(dir string) (updateState, error) {
	var st updateState
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

func writeUpdateState(dir string, st updateState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "state.json"), append(b, '\n'), 0o600)
}

func (st *updateState) rolledBack(reason string) {
	st.State, st.Reason, st.PID, st.Noted = "rolled_back", reason, 0, false
	if !slices.Contains(st.Failed, st.ToCode) {
		st.Failed = append(st.Failed, st.ToCode)
	}
	if over := len(st.Failed) - updateFailedKeep; over > 0 {
		st.Failed = st.Failed[over:]
	}
}

// The service.sh loop reads these marker files: pending while an upgrade is unconfirmed, and
// rolled_back (holding the reason) after it put the previous version back itself
const (
	pendingMarker  = "pending"
	rollbackMarker = "rolled_back"
)

type updater struct {
	cfg     *configStore
	events  *eventLog
	health  *health
	out     *outbound
	dir     string // <data dir>/update; empty keeps no state and cannot install
	cfgPath string
	demo    bool
	mod     string // the Magisk module this binary runs from, empty when it does not
	// demo key, trusted besides the configured ones
	extraKeys   []ed25519.PublicKey
	restart     func() // ends this process; service.sh starts the new binary
	commitAfter time.Duration

	stateMu sync.Mutex // read-modify-write of state.json within this process

	mu        sync.Mutex
	ctx       context.Context // the server's, for installs started by a request
	phase     string          // checking, downloading, verifying, installing, restarting; empty when idle
	got       int64           // download progress
	checkedAt time.Time
	err       string
	noRelease bool
	latest    *manifest // a verified version newer than this one
	skipped   string    // demo: the version that was verified but not installed
	errNoted  int64     // version whose failed install was already reported
}

func newUpdater(cs *configStore, events *eventLog, h *health, out *outbound, dataDir string) *updater {
	u := &updater{cfg: cs, events: events, health: h, out: out, cfgPath: cs.path, commitAfter: updateCommitAfter,
		restart: func() {
			time.Sleep(time.Second) // let the response go out
			syscall.Kill(os.Getpid(), syscall.SIGTERM)
		}}
	if dataDir != "" {
		u.dir = filepath.Join(dataDir, "update")
	}
	u.mod = moduleDir()
	return u
}

// moduleDir returns the Magisk module directory this binary runs from, or "" when it is not
// installed as one (go run, the demo, tests)
func moduleDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	bin := filepath.Dir(exe)
	if filepath.Base(exe) != "glovebox" || filepath.Base(bin) != "bin" {
		return ""
	}
	mod := filepath.Dir(bin)
	if _, err := os.Stat(filepath.Join(mod, "module.prop")); err != nil {
		return ""
	}
	return mod
}

func (u *updater) mode(c Config) string {
	switch c.UpdateMode {
	case "auto", "notify", "off":
		return c.UpdateMode
	}
	return "off"
}

func (u *updater) interval(c Config) time.Duration {
	d, err := time.ParseDuration(c.UpdateInterval)
	if err != nil {
		d = time.Hour
	}
	d = max(d, updateMinInterval)
	// spread the checks of many devices
	return d - d/10 + rand.N(d/5)
}

func (u *updater) keys(c Config) []ed25519.PublicKey {
	var keys []ed25519.PublicKey
	for _, s := range append(slices.Clone(trustedUpdateKeys), c.UpdatePubkeys...) {
		if k, err := parseUpdateKey(s); err == nil {
			keys = append(keys, k)
		} else {
			log.Print(err)
		}
	}
	return append(keys, u.extraKeys...)
}

// loop settles the last upgrade, then checks for a new version every update_interval. It runs once
// the HTTP listeners are up: staying up from then on is what confirms a new version.
func (u *updater) loop(ctx context.Context) {
	u.mu.Lock()
	u.ctx = ctx
	u.mu.Unlock()
	u.settle(ctx)
	wait := updateFirstCheck
	for {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		c := u.cfg.get()
		wait = u.interval(c)
		u.tick(ctx, c)
	}
}

func (u *updater) tick(ctx context.Context, c Config) {
	mode := u.mode(c)
	if c.UpdateURL == "" || mode == "off" {
		return
	}
	if u.health != nil && !u.health.clockOK() {
		return // HTTPS fails until the clock is right
	}
	if st, _ := u.state(); st.State == "pending" {
		return // the last upgrade is not settled yet
	}
	m, err := u.check(ctx)
	if err != nil || m == nil {
		return
	}
	if mode == "notify" {
		u.announce(*m)
		return
	}
	if st, _ := u.state(); slices.Contains(st.Failed, m.VersionCode) {
		return
	}
	if u.begin("downloading") {
		u.install(ctx, *m)
	}
}

func (u *updater) state() (updateState, error) {
	if u.dir == "" {
		return updateState{}, nil
	}
	return readUpdateState(u.dir)
}

// change applies fn to state.json
func (u *updater) change(fn func(st *updateState)) error {
	if u.dir == "" {
		return nil
	}
	u.stateMu.Lock()
	defer u.stateMu.Unlock()
	st, err := readUpdateState(u.dir)
	if err != nil {
		log.Printf("update state: %v, starting over", err)
		st = updateState{}
	}
	fn(&st)
	if err := os.MkdirAll(u.dir, 0o700); err != nil {
		return err
	}
	return writeUpdateState(u.dir, st)
}

// settle runs at start: a new version confirms itself after updateCommitAfter, a restored one
// records why the upgrade did not take
func (u *updater) settle(ctx context.Context) {
	st, err := u.state()
	if err != nil {
		log.Printf("update state: %v", err)
		return
	}
	me := buildCode()
	switch {
	case st.State == "pending" && st.ToCode == me:
		u.change(func(st *updateState) { st.PID = os.Getpid() })
		go u.commitLater(ctx)
	case st.State == "pending":
		// this is the previous version again: service.sh put it back, or the new one never started
		reason := "the new version did not start"
		if b, err := os.ReadFile(filepath.Join(u.dir, rollbackMarker)); err == nil && len(bytes.TrimSpace(b)) > 0 {
			reason = string(bytes.TrimSpace(b))
		}
		u.change(func(st *updateState) { st.rolledBack(reason) })
		u.noteRollback()
	case st.State == "rolled_back" && !st.Noted:
		u.noteRollback()
	}
}

func (u *updater) noteRollback() {
	var st updateState
	u.change(func(s *updateState) { s.Noted = true; st = *s })
	os.Remove(filepath.Join(u.dir, pendingMarker))
	os.Remove(filepath.Join(u.dir, rollbackMarker))
	os.RemoveAll(filepath.Join(u.dir, "new"))
	u.cleanZips()
	log.Printf("update to %s was rolled back: %s", st.To, st.Reason)
	u.events.add(event{Check: "update", State: "fail", Msg: st.To + ": " + st.Reason})
}

func (u *updater) commitLater(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(u.commitAfter):
	}
	var st updateState
	committed := false
	u.change(func(s *updateState) {
		// the guard may have given up on this version in the meantime
		if s.State == "pending" && s.ToCode == buildCode() {
			s.State, s.PID, s.Noted, committed = "committed", 0, true, true
		}
		st = *s
	})
	if !committed {
		return
	}
	os.Remove(filepath.Join(u.dir, pendingMarker))
	os.RemoveAll(filepath.Join(u.dir, "new"))
	u.cleanZips()
	log.Printf("upgrade from %s to %s confirmed", st.From, st.To)
	u.events.add(event{Check: "update", State: "ok", Msg: st.From + " → " + st.To})
}

// announce records a new version once, in notify mode
func (u *updater) announce(m manifest) {
	fresh := false
	u.change(func(st *updateState) {
		if st.Notified != m.VersionCode {
			st.Notified, fresh = m.VersionCode, true
		}
	})
	if fresh {
		u.events.add(event{Check: "update", State: "info", Msg: m.Version})
	}
}

// begin claims the updater for one operation
func (u *updater) begin(phase string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.phase != "" {
		return false
	}
	u.phase, u.got, u.skipped = phase, 0, ""
	return true
}

func (u *updater) setPhase(phase string) {
	u.mu.Lock()
	u.phase = phase
	u.mu.Unlock()
}

var errNoRelease = errors.New("no release published")

// check fetches and verifies the manifest; it returns the manifest when it is newer than this build
func (u *updater) check(ctx context.Context) (*manifest, error) {
	if !u.begin("checking") {
		return nil, apiErr("update_busy", "an update is already in progress")
	}
	c := u.cfg.get()
	m, err := u.fetchManifest(ctx, c)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.phase = ""
	u.checkedAt = time.Now()
	u.noRelease = errors.Is(err, errNoRelease)
	u.err = ""
	u.latest = nil
	if err != nil {
		if u.noRelease {
			return nil, nil
		}
		u.err = err.Error()
		log.Printf("update check: %v", err)
		return nil, err
	}
	if m.VersionCode > buildCode() {
		u.latest = m
	}
	return u.latest, nil
}

func (u *updater) fetchManifest(ctx context.Context, c Config) (*manifest, error) {
	base, err := url.Parse(c.UpdateURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, fmt.Errorf("update_url %q: want an http(s) URL", c.UpdateURL)
	}
	hc, err := u.out.client(c.ReportProxy)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.UpdateURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "glovebox/"+version)
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNoRelease
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, updateManifestMax+1))
	if err != nil {
		return nil, err
	}
	if len(b) > updateManifestMax {
		return nil, errors.New("manifest: too large")
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if err := m.verify(u.keys(c)); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	// relative to the configured URL, not to where a redirect (GitHub) ended up
	zu, err := base.Parse(m.ZipURL)
	if err != nil || (zu.Scheme != "http" && zu.Scheme != "https") {
		return nil, fmt.Errorf("manifest: bad zipUrl %q", m.ZipURL)
	}
	m.ZipURL = zu.String()
	return &m, nil
}

// install downloads, verifies and installs m; the caller holds the updater (begin). On success the
// process restarts into the new version.
func (u *updater) install(ctx context.Context, m manifest) error {
	err := u.doInstall(ctx, m)
	if err == nil {
		return nil
	}
	u.mu.Lock()
	u.phase, u.err = "", err.Error()
	report := u.errNoted != m.VersionCode
	u.errNoted = m.VersionCode
	u.mu.Unlock()
	log.Printf("update to %s: %v", m.Version, err)
	if report {
		u.events.add(event{Check: "update", State: "fail", Msg: m.Version + ": " + err.Error()})
	}
	return err
}

func (u *updater) doInstall(ctx context.Context, m manifest) error {
	if u.dir == "" {
		return errors.New("no data directory")
	}
	if u.mod == "" && !u.demo {
		return errors.New("glovebox is not running from its Magisk module; install it with deploy.sh")
	}
	if err := os.MkdirAll(u.dir, 0o700); err != nil {
		return err
	}
	zipPath, err := u.download(ctx, m)
	if err != nil {
		return err
	}
	u.setPhase("verifying")
	newDir := filepath.Join(u.dir, "new")
	if err := extractModule(zipPath, newDir); err != nil {
		return err
	}
	if err := checkModule(ctx, newDir, m, !u.demo); err != nil {
		return err
	}
	if u.demo {
		log.Printf("demo: skipping the install of %s", m.Version)
		u.mu.Lock()
		u.phase, u.skipped, u.err = "", m.Version, ""
		u.mu.Unlock()
		return nil
	}
	u.setPhase("installing")
	if err := u.swap(m, newDir); err != nil {
		return err
	}
	log.Printf("installed %s, restarting", m.Version)
	u.setPhase("restarting")
	go u.restart()
	return nil
}

// download fetches the zip into the update directory and checks its size and sha256
func (u *updater) download(ctx context.Context, m manifest) (string, error) {
	c := u.cfg.get()
	hc, err := u.out.client(c.ReportProxy)
	if err != nil {
		return "", err
	}
	dl := *hc
	dl.Timeout = updateDownloadTimeout
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.ZipURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "glovebox/"+version)
	resp, err := dl.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	u.cleanZips()
	final := filepath.Join(u.dir, fmt.Sprintf("%d.zip", m.VersionCode))
	part := final + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h, progress{u}), io.LimitReader(resp.Body, m.Size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
	case n != m.Size:
		err = fmt.Errorf("download: got %d bytes, the manifest says %d", n, m.Size)
	case hex.EncodeToString(h.Sum(nil)) != m.SHA256:
		err = errors.New("download: sha256 does not match the manifest")
	}
	if err != nil {
		os.Remove(part)
		return "", err
	}
	return final, os.Rename(part, final)
}

type progress struct{ u *updater }

func (p progress) Write(b []byte) (int, error) {
	p.u.mu.Lock()
	p.u.got += int64(len(b))
	p.u.mu.Unlock()
	return len(b), nil
}

// cleanZips removes downloaded zips
func (u *updater) cleanZips() {
	old, _ := filepath.Glob(filepath.Join(u.dir, "*.zip*"))
	for _, p := range old {
		os.Remove(p)
	}
}

// extractModule writes the files in moduleFiles from the zip into dir. Only those exact names are
// taken, so entries such as ../x cannot escape.
func extractModule(zipPath, dir string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("zip: %w", err)
	}
	defer zr.Close()
	os.RemoveAll(dir)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o700); err != nil {
		return err
	}
	found := map[string]bool{}
	for _, f := range zr.File {
		name := strings.TrimPrefix(f.Name, "./")
		if !slices.Contains(moduleFiles, name) || found[name] {
			continue
		}
		mode := os.FileMode(0o644)
		if name == "bin/glovebox" || strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		if err := extractFile(f, filepath.Join(dir, filepath.FromSlash(name)), mode); err != nil {
			return fmt.Errorf("zip: %s: %w", name, err)
		}
		found[name] = true
	}
	for _, need := range []string{"bin/glovebox", "module.prop"} {
		if !found[need] {
			return fmt.Errorf("zip: no %s, not a glovebox module", need)
		}
	}
	return nil
}

func extractFile(f *zip.File, dst string, mode os.FileMode) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	w, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	n, err := io.Copy(w, io.LimitReader(rc, updateZipMax+1))
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > updateZipMax {
		err = errors.New("too large")
	}
	return err
}

// checkModule makes sure the extracted module is the version the manifest promises and, when run
// is set, that the new binary starts on this device at all
func checkModule(ctx context.Context, dir string, m manifest, run bool) error {
	b, err := os.ReadFile(filepath.Join(dir, "module.prop"))
	if err != nil {
		return err
	}
	props := parseProps(b)
	if props["id"] != "glovebox" {
		return fmt.Errorf("module.prop: id %q is not glovebox", props["id"])
	}
	if props["version"] != m.Version || props["versionCode"] != strconv.FormatInt(m.VersionCode, 10) {
		return fmt.Errorf("module.prop: %s (%s) does not match the manifest", props["version"], props["versionCode"])
	}
	if !run {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, filepath.Join(dir, "bin/glovebox"), "version").Output()
	if err != nil {
		return fmt.Errorf("the new binary does not run here: %w", err)
	}
	if got := strings.TrimSpace(string(out)); got != m.Version {
		return fmt.Errorf("the new binary reports version %q", got)
	}
	return nil
}

func parseProps(b []byte) map[string]string {
	props := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	return props
}

// swap keeps the installed files in update/prev, records the pending upgrade, replaces the module
// files (the binary last) and starts the guard from the previous binary
func (u *updater) swap(m manifest, newDir string) error {
	prev := filepath.Join(u.dir, "prev")
	tmp := prev + ".tmp"
	os.RemoveAll(tmp)
	for _, f := range moduleFiles {
		src := filepath.Join(u.mod, filepath.FromSlash(f))
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := copyFile(src, filepath.Join(tmp, filepath.FromSlash(f))); err != nil {
			return fmt.Errorf("keep the current version: %w", err)
		}
	}
	if _, err := os.Stat(filepath.Join(tmp, "bin/glovebox")); err != nil {
		return fmt.Errorf("keep the current version: %w", err)
	}
	os.RemoveAll(prev)
	if err := os.Rename(tmp, prev); err != nil {
		return err
	}
	err := u.change(func(st *updateState) {
		*st = updateState{State: "pending", From: version, FromCode: buildCode(), To: m.Version, ToCode: m.VersionCode,
			At: time.Now(), Failed: st.Failed, Notified: st.Notified}
	})
	if err != nil {
		return err
	}
	os.Remove(filepath.Join(u.dir, rollbackMarker))
	if err := os.WriteFile(filepath.Join(u.dir, pendingMarker), []byte(strconv.FormatInt(m.VersionCode, 10)+"\n"), 0o600); err != nil {
		return err
	}
	undo := func(err error) error {
		restoreModule(prev, u.mod)
		os.Remove(filepath.Join(u.dir, pendingMarker))
		u.change(func(st *updateState) { st.rolledBack(err.Error()); st.Noted = true })
		return err
	}
	for _, f := range moduleFiles {
		src := filepath.Join(newDir, filepath.FromSlash(f))
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := replaceFile(src, filepath.Join(u.mod, filepath.FromSlash(f))); err != nil {
			return undo(fmt.Errorf("replace %s: %w", f, err))
		}
	}
	if err := startDetached(filepath.Join(prev, "bin/glovebox"), "-config", u.cfgPath, "update-guard", u.mod); err != nil {
		return undo(fmt.Errorf("start the update guard: %w", err))
	}
	return nil
}

// copyFile copies src to dst with src's mode, creating dst's directory
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// replaceFile puts a copy of src at dst in one step, so the running binary is never written to
func replaceFile(src, dst string) error {
	tmp := dst + ".new"
	if err := copyFile(src, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func restoreModule(prev, mod string) error {
	var errs []error
	for _, f := range moduleFiles {
		src := filepath.Join(prev, filepath.FromSlash(f))
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := replaceFile(src, filepath.Join(mod, filepath.FromSlash(f))); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// runGuard is the update-guard command, run from update/prev/bin/glovebox right after an upgrade
func runGuard(cfgPath string, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: glovebox update-guard <module dir>")
	}
	dir := filepath.Join(filepath.Dir(cfgPath), "update")
	return guardUpgrade(dir, args[0], updateGuardTimeout, 2*time.Second)
}

// guardUpgrade waits for the new version to confirm itself and puts the previous one back when it
// does not within timeout, whether it crashed or hangs
func guardUpgrade(dir, mod string, timeout, every time.Duration) error {
	deadline := time.Now().Add(timeout)
	var st updateState
	for {
		var err error
		if st, err = readUpdateState(dir); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(dir, pendingMarker)); st.State != "pending" || err != nil {
			log.Printf("update guard: the upgrade to %s settled (%s)", st.To, st.State)
			return nil // confirmed, or service.sh already put the previous version back
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(every)
	}
	reason := fmt.Sprintf("the new version did not stay up for %s", updateCommitAfter)
	if st.PID == 0 {
		reason = "the new version did not start"
	}
	log.Printf("update guard: %s, restoring %s", reason, st.From)
	err := restoreModule(filepath.Join(dir, "prev"), mod)
	st.rolledBack(reason)
	if werr := writeUpdateState(dir, st); err == nil {
		err = werr
	}
	os.Remove(filepath.Join(dir, pendingMarker))
	// service.sh starts the restored binary once the new one is gone
	stopReplaced(filepath.Join(mod, "bin/glovebox"))
	return err
}

// stopReplaced ends processes started from bin, whose file has since been replaced: the new
// binary, not one started after the restore
func stopReplaced(bin string) {
	ents, _ := os.ReadDir("/proc")
	var pids []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		cmd, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if argv0, _, _ := bytes.Cut(cmd, []byte{0}); string(argv0) != bin {
			continue
		}
		if exe, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe")); err != nil || !strings.HasSuffix(exe, " (deleted)") {
			continue
		}
		syscall.Kill(pid, syscall.SIGTERM)
		pids = append(pids, pid)
	}
	for range 10 {
		pids = slices.DeleteFunc(pids, func(pid int) bool { return syscall.Kill(pid, 0) != nil })
		if len(pids) == 0 {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	for _, pid := range pids {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

// updateInfo is GET /api/update
type updateInfo struct {
	Version   string       `json:"version"`
	Code      int64        `json:"code"`
	Built     time.Time    `json:"built,omitzero"`
	Source    string       `json:"source,omitempty"` // host of update_url
	Mode      string       `json:"mode"`
	Interval  string       `json:"interval"`
	Managed   bool         `json:"managed"` // runs from the module, so it can install
	Phase     string       `json:"phase,omitempty"`
	Got       int64        `json:"got,omitempty"`
	CheckedAt time.Time    `json:"checked_at,omitzero"`
	Error     string       `json:"error,omitempty"`
	NoRelease bool         `json:"no_release,omitempty"`
	Latest    *manifest    `json:"latest,omitempty"`
	Failed    bool         `json:"failed,omitempty"` // latest was rolled back before
	Skipped   string       `json:"skipped,omitempty"`
	Last      *updateState `json:"last,omitempty"`
}

func (u *updater) info() updateInfo {
	c := u.cfg.get()
	in := updateInfo{Version: version, Code: buildCode(), Mode: u.mode(c), Interval: c.UpdateInterval, Managed: u.mod != "" || u.demo}
	if c.UpdateURL == "" {
		in.Mode = "off"
	}
	if in.Code > 1e9 {
		in.Built = time.Unix(in.Code, 0)
	}
	if p, err := url.Parse(c.UpdateURL); err == nil {
		in.Source = p.Host
	}
	st, _ := u.state()
	failed := st.Failed
	if st.State != "" {
		st.Failed, st.Notified, st.PID = nil, 0, 0
		in.Last = &st
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	in.Phase, in.Got, in.CheckedAt, in.Error, in.NoRelease, in.Skipped = u.phase, u.got, u.checkedAt, u.err, u.noRelease, u.skipped
	if u.latest != nil {
		m := *u.latest
		m.Sig = ""
		in.Latest = &m
		in.Failed = slices.Contains(failed, m.VersionCode)
	}
	return in
}

// available is the newer version found by the last check, for the status bar
func (u *updater) available() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.latest == nil {
		return ""
	}
	return u.latest.Version
}

// installLatest starts installing the version the last check found, if that is still want
func (u *updater) installLatest(want int64) error {
	u.mu.Lock()
	m := u.latest
	ctx := u.ctx
	u.mu.Unlock()
	if m == nil || m.VersionCode != want {
		return apiErr("update_stale", "check for updates again")
	}
	if !u.begin("downloading") {
		return apiErr("update_busy", "an update is already in progress")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	go u.install(ctx, *m)
	return nil
}
