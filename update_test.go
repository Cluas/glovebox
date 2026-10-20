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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// moduleZip builds a module zip whose bin/glovebox is a shell script printing binVersion
func moduleZip(t *testing.T, ver string, code int64, binVersion string, extra map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := map[string]string{
		"module.prop":  fmt.Sprintf("id=glovebox\nname=Glovebox\nversion=%s\nversionCode=%d\n", ver, code),
		"bin/glovebox": "#!/bin/sh\necho " + binVersion + "\n",
		"service.sh":   "#!/system/bin/sh\n# " + ver + "\n",
	}
	for k, v := range extra {
		files[k] = v
	}
	for name, body := range files {
		if body == "" {
			continue
		}
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func signed(priv ed25519.PrivateKey, m manifest) manifest {
	m.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, m.signedText()))
	return m
}

func manifestFor(priv ed25519.PrivateKey, ver string, code int64, zipName string, z []byte) manifest {
	sum := sha256.Sum256(z)
	return signed(priv, manifest{Version: ver, VersionCode: code, ZipURL: zipName, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(z)), Notes: "fixes"})
}

func TestManifestVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	other, _, _ := ed25519.GenerateKey(nil)
	keys := []ed25519.PublicKey{other, pub}
	good := manifestFor(priv, "1.2.3", 1790000000, "g.zip", []byte("zip"))
	if err := good.verify(keys); err != nil {
		t.Fatalf("good manifest: %v", err)
	}
	if err := good.verify([]ed25519.PublicKey{other}); err == nil {
		t.Fatal("verified with an untrusted key")
	}
	// the zip location is not signed, so a release can be mirrored
	moved := good
	moved.ZipURL = "https://mirror.example/g.zip"
	if err := moved.verify(keys); err != nil {
		t.Fatalf("mirrored zip: %v", err)
	}
	for name, edit := range map[string]func(m *manifest){
		"version":     func(m *manifest) { m.Version = "1.2.4" },
		"versionCode": func(m *manifest) { m.VersionCode++ },
		"sha256":      func(m *manifest) { m.SHA256 = strings.Repeat("0", 64) },
		"size":        func(m *manifest) { m.Size++ },
		"notes":       func(m *manifest) { m.Notes = "install me" },
		"sig":         func(m *manifest) { m.Sig = "bm9wZQ==" },
		"bad version": func(m *manifest) { m.Version = "1.0\nsha256=x" },
		"upper hex":   func(m *manifest) { m.SHA256 = strings.ToUpper(m.SHA256) },
		"huge":        func(m *manifest) { m.Size = updateZipMax + 1 },
		"no zip":      func(m *manifest) { m.ZipURL = "" },
	} {
		m := good
		edit(&m)
		if err := m.verify(keys); err == nil {
			t.Errorf("%s: tampered manifest verified", name)
		}
	}
}

func TestUpdateKeygenAndSign(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	var out bytes.Buffer
	if err := updateKeygen(key, &out); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(key); fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v, want 0600", fi.Mode().Perm())
	}
	if err := updateKeygen(key, &out); err == nil {
		t.Fatal("keygen overwrote an existing key")
	}
	pub, err := parseUpdateKey(out.String())
	if err != nil {
		t.Fatal(err)
	}
	z := filepath.Join(dir, "glovebox-2.0.zip")
	os.WriteFile(z, moduleZip(t, "2.0", 1790000001, "2.0", nil), 0o644)
	m, _, err := signUpdate(key, z, "glovebox-2.0.zip", "notes")
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "2.0" || m.VersionCode != 1790000001 || m.ZipURL != "glovebox-2.0.zip" {
		t.Fatalf("manifest %+v", m)
	}
	if err := m.verify([]ed25519.PublicKey{pub}); err != nil {
		t.Fatalf("signed manifest: %v", err)
	}
}

func TestExtractModule(t *testing.T) {
	dir := t.TempDir()
	z := filepath.Join(dir, "m.zip")
	os.WriteFile(z, moduleZip(t, "2.0", 2, "2.0", map[string]string{
		"../bin/glovebox": "evil", "system/vendor/x.apk": "apk", "META-INF/x": "x", "uninstall.sh": "#!/bin/sh\n",
	}), 0o644)
	out := filepath.Join(dir, "new")
	if err := extractModule(z, out); err != nil {
		t.Fatal(err)
	}
	var got []string
	filepath.Walk(out, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			rel, _ := filepath.Rel(out, p)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	slices.Sort(got)
	if want := []string{"bin/glovebox", "module.prop", "service.sh", "uninstall.sh"}; !slices.Equal(got, want) {
		t.Fatalf("extracted %v, want %v", got, want)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "bin/glovebox")); bytes.Contains(b, []byte("evil")) {
		t.Fatal("../bin/glovebox overwrote the binary")
	}
	if fi, _ := os.Stat(filepath.Join(out, "bin/glovebox")); fi.Mode().Perm() != 0o755 {
		t.Errorf("binary mode %v", fi.Mode().Perm())
	}
	os.WriteFile(z, moduleZip(t, "2.0", 2, "", map[string]string{"bin/glovebox": ""}), 0o644)
	if err := extractModule(z, out); err == nil || !strings.Contains(err.Error(), "bin/glovebox") {
		t.Fatalf("zip without a binary: %v", err)
	}
}

func TestCheckModule(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runs a shell script")
	}
	dir := t.TempDir()
	z := filepath.Join(dir, "m.zip")
	m := manifest{Version: "2.0", VersionCode: 2}
	for _, c := range []struct {
		ver, bin string
		code     int64
		ok       bool
	}{
		{"2.0", "2.0", 2, true},
		{"2.0", "1.9", 2, false}, // the binary is not what module.prop says
		{"2.1", "2.0", 2, false}, // module.prop is not what the manifest says
		{"2.0", "2.0", 3, false},
	} {
		os.WriteFile(z, moduleZip(t, c.ver, c.code, c.bin, nil), 0o644)
		out := filepath.Join(dir, "new")
		if err := extractModule(z, out); err != nil {
			t.Fatal(err)
		}
		if err := checkModule(context.Background(), out, m, true); (err == nil) != c.ok {
			t.Errorf("%+v: %v", c, err)
		}
	}
}

// testUpgrade is an installed module plus a release server
type testUpgrade struct {
	t       *testing.T
	data    string // config directory
	mod     string
	priv    ed25519.PrivateKey
	pub     ed25519.PublicKey
	srv     *httptest.Server
	release atomic.Pointer[[2][]byte] // manifest, zip
	events  *eventLog
}

func newTestUpgrade(t *testing.T) *testUpgrade {
	if runtime.GOOS == "windows" {
		t.Skip("runs shell scripts")
	}
	tu := &testUpgrade{t: t, data: t.TempDir(), mod: filepath.Join(t.TempDir(), "glovebox")}
	tu.pub, tu.priv, _ = ed25519.GenerateKey(nil)
	os.MkdirAll(filepath.Join(tu.mod, "bin"), 0o755)
	os.WriteFile(filepath.Join(tu.mod, "module.prop"), []byte("id=glovebox\nversion=1.0\nversionCode=1\n"), 0o644)
	os.WriteFile(filepath.Join(tu.mod, "service.sh"), []byte("#!/system/bin/sh\n# 1.0\n"), 0o755)
	os.WriteFile(filepath.Join(tu.mod, "bin/glovebox"), []byte("#!/bin/sh\necho 1.0\n"), 0o755)
	tu.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := tu.release.Load()
		switch {
		case rel == nil:
			http.NotFound(w, r)
		case r.URL.Path == "/update.json":
			w.Write(rel[0])
		case strings.HasSuffix(r.URL.Path, ".zip"):
			w.Write(rel[1])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(tu.srv.Close)
	tu.events = openEventLog("")
	setBuild(t, "1.0", 1)
	return tu
}

func setBuild(t *testing.T, ver string, code int64) {
	oldV, oldC := version, versionCode
	version, versionCode = ver, strconv.FormatInt(code, 10)
	t.Cleanup(func() { version, versionCode = oldV, oldC })
}

func (tu *testUpgrade) publish(ver string, code int64, binVersion string) manifest {
	z := moduleZip(tu.t, ver, code, binVersion, nil)
	m := manifestFor(tu.priv, ver, code, "glovebox-"+ver+".zip", z)
	b, _ := json.Marshal(m)
	tu.release.Store(&[2][]byte{b, z})
	return m
}

// updater is the updater of the running process
func (tu *testUpgrade) updater(mode string) (*updater, *int32) {
	c := defaultConfig()
	c.UpdateURL = tu.srv.URL + "/update.json"
	c.UpdateMode = mode
	cs := &configStore{path: filepath.Join(tu.data, "config.json"), c: c}
	u := newUpdater(cs, tu.events, nil, &outbound{}, tu.data)
	u.mod = tu.mod
	u.extraKeys = []ed25519.PublicKey{tu.pub}
	u.commitAfter = 20 * time.Millisecond
	var restarts int32
	u.restart = func() { atomic.AddInt32(&restarts, 1) }
	return u, &restarts
}

func (tu *testUpgrade) installed() string {
	b, _ := os.ReadFile(filepath.Join(tu.mod, "bin/glovebox"))
	return strings.TrimPrefix(strings.TrimSpace(string(b)), "#!/bin/sh\necho ")
}

func (tu *testUpgrade) lastEvent() event {
	evs := tu.events.recent(1)
	if len(evs) == 0 {
		return event{}
	}
	return evs[0]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestUpgradeAndCommit(t *testing.T) {
	tu := newTestUpgrade(t)
	u, restarts := tu.updater("auto")
	ctx := context.Background()

	// nothing published yet: not an error
	if m, err := u.check(ctx); m != nil || err != nil || !u.info().NoRelease {
		t.Fatalf("no release: %v %v %+v", m, err, u.info())
	}
	tu.publish("2.0", 2, "2.0")
	u.tick(ctx, u.cfg.get())
	if got := tu.installed(); got != "2.0" {
		t.Fatalf("installed %q, want 2.0 (status %+v)", got, u.info())
	}
	waitFor(t, "the restart", func() bool { return atomic.LoadInt32(restarts) == 1 })
	if b, _ := os.ReadFile(filepath.Join(tu.mod, "service.sh")); !strings.Contains(string(b), "2.0") {
		t.Error("service.sh was not replaced")
	}
	if b, _ := os.ReadFile(filepath.Join(tu.data, "update/prev/bin/glovebox")); !strings.Contains(string(b), "1.0") {
		t.Error("the previous binary was not kept")
	}
	st, _ := readUpdateState(u.dir)
	if st.State != "pending" || st.From != "1.0" || st.To != "2.0" || st.ToCode != 2 {
		t.Fatalf("state %+v", st)
	}
	if _, err := os.Stat(filepath.Join(u.dir, pendingMarker)); err != nil {
		t.Fatal("no pending marker for service.sh")
	}

	// the new version starts and stays up
	setBuild(t, "2.0", 2)
	u2, _ := tu.updater("auto")
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	u2.settle(cctx)
	waitFor(t, "the commit", func() bool { st, _ := readUpdateState(u.dir); return st.State == "committed" })
	if _, err := os.Stat(filepath.Join(u.dir, pendingMarker)); err == nil {
		t.Error("pending marker left behind")
	}
	if e := tu.lastEvent(); e.Check != "update" || e.State != "ok" || e.Msg != "1.0 → 2.0" {
		t.Errorf("event %+v", e)
	}
	// up to date now
	if m, _ := u2.check(ctx); m != nil {
		t.Errorf("offered %s again", m.Version)
	}
	// a guard that wakes up late leaves a confirmed version alone
	if err := guardUpgrade(u.dir, tu.mod, 0, time.Millisecond); err != nil || tu.installed() != "2.0" {
		t.Fatalf("guard after commit: %v, installed %s", err, tu.installed())
	}
}

func TestUpgradeRollsBack(t *testing.T) {
	tu := newTestUpgrade(t)
	u, _ := tu.updater("auto")
	ctx := context.Background()
	m := tu.publish("2.0", 2, "2.0")
	u.tick(ctx, u.cfg.get())
	if tu.installed() != "2.0" {
		t.Fatalf("not installed: %+v", u.info())
	}
	// the new version never confirms itself
	if err := guardUpgrade(u.dir, tu.mod, 30*time.Millisecond, 5*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if got := tu.installed(); got != "1.0" {
		t.Fatalf("after the guard: %q, want 1.0", got)
	}
	if b, _ := os.ReadFile(filepath.Join(tu.mod, "service.sh")); !strings.Contains(string(b), "1.0") {
		t.Error("service.sh was not restored")
	}
	st, _ := readUpdateState(u.dir)
	if st.State != "rolled_back" || !slices.Contains(st.Failed, int64(2)) || st.Noted {
		t.Fatalf("state %+v", st)
	}

	// the restored version reports it once
	u2, _ := tu.updater("auto")
	u2.settle(ctx)
	if e := tu.lastEvent(); e.State != "fail" || !strings.HasPrefix(e.Msg, "2.0: ") {
		t.Errorf("event %+v", e)
	}
	if left, _ := filepath.Glob(filepath.Join(u.dir, "*.zip*")); len(left) > 0 {
		t.Errorf("the failed download was kept: %v", left)
	}
	if _, err := os.Stat(filepath.Join(u.dir, "new")); err == nil {
		t.Error("the extracted module was kept")
	}
	n := tu.events.lastSeq()
	u2.settle(ctx)
	if tu.events.lastSeq() != n {
		t.Error("the rollback was reported twice")
	}
	// automatic upgrades skip the failed version, a manual install may retry it
	u2.tick(ctx, u2.cfg.get())
	if tu.installed() != "1.0" {
		t.Fatal("reinstalled a version that was rolled back")
	}
	if in := u2.info(); in.Latest == nil || !in.Failed {
		t.Errorf("status should offer the failed version: %+v", in)
	}
	if err := u2.installLatest(m.VersionCode); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the manual install", func() bool { return tu.installed() == "2.0" })
}

// service.sh undid the upgrade after a power cut: the restored version picks up its reason
func TestUpgradeUndoneByServiceScript(t *testing.T) {
	tu := newTestUpgrade(t)
	u, _ := tu.updater("auto")
	ctx := context.Background()
	tu.publish("2.0", 2, "2.0")
	u.tick(ctx, u.cfg.get())
	restoreModule(filepath.Join(u.dir, "prev"), tu.mod)
	os.WriteFile(filepath.Join(u.dir, rollbackMarker), []byte("the new version crashed 3 times at start\n"), 0o600)
	os.Remove(filepath.Join(u.dir, pendingMarker))

	u2, _ := tu.updater("auto")
	u2.settle(ctx)
	st, _ := readUpdateState(u.dir)
	if st.State != "rolled_back" || st.Reason != "the new version crashed 3 times at start" || !st.Noted {
		t.Fatalf("state %+v", st)
	}
	if e := tu.lastEvent(); e.State != "fail" || !strings.Contains(e.Msg, "crashed") {
		t.Errorf("event %+v", e)
	}
}

func TestUpgradeRefusesBadReleases(t *testing.T) {
	tu := newTestUpgrade(t)
	ctx := context.Background()

	// signed by someone else
	_, stranger, _ := ed25519.GenerateKey(nil)
	z := moduleZip(t, "2.0", 2, "2.0", nil)
	b, _ := json.Marshal(manifestFor(stranger, "2.0", 2, "g.zip", z))
	tu.release.Store(&[2][]byte{b, z})
	u, _ := tu.updater("auto")
	if _, err := u.check(ctx); err == nil || !strings.Contains(u.info().Error, "signature") {
		t.Fatalf("foreign signature: %v", err)
	}

	// the zip is not the one that was signed
	m := manifestFor(tu.priv, "2.0", 2, "g.zip", z)
	b, _ = json.Marshal(m)
	tu.release.Store(&[2][]byte{b, moduleZip(t, "2.0", 2, "evil", nil)})
	u.tick(ctx, u.cfg.get())
	if tu.installed() != "1.0" || !strings.Contains(u.info().Error, "download") {
		t.Fatal("installed a zip that does not match the manifest")
	}
	if e := tu.lastEvent(); e.State != "fail" || !strings.HasPrefix(e.Msg, "2.0: ") {
		t.Errorf("event %+v", e)
	}
	n := tu.events.lastSeq()
	u.tick(ctx, u.cfg.get())
	if tu.events.lastSeq() != n {
		t.Error("the same failure was reported twice")
	}

	// a binary that does not run here
	tu.publish("3.0", 3, "garbage")
	u.tick(ctx, u.cfg.get())
	if tu.installed() != "1.0" || !strings.Contains(u.info().Error, "reports version") {
		t.Fatalf("installed a binary that reports the wrong version: %+v", u.info())
	}

	// older than this build
	setBuild(t, "5.0", 5)
	tu.publish("4.0", 4, "4.0")
	if m, err := u.check(ctx); m != nil || err != nil {
		t.Fatalf("offered an older version: %v %v", m, err)
	}
}

func TestUpgradeNotifyMode(t *testing.T) {
	tu := newTestUpgrade(t)
	u, _ := tu.updater("notify")
	ctx := context.Background()
	tu.publish("2.0", 2, "2.0")
	u.tick(ctx, u.cfg.get())
	u.tick(ctx, u.cfg.get())
	if tu.installed() != "1.0" {
		t.Fatal("notify mode installed")
	}
	var n int
	for _, e := range tu.events.recent(10) {
		if e.Check == "update" && e.State == "info" && e.Msg == "2.0" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("announced %d times, want once", n)
	}
}

func TestUpdateEventText(t *testing.T) {
	cases := map[string]event{
		"Update 2.0 is available": {Check: "update", State: "info", Msg: "2.0"},
		"Updated 1.0 → 2.0":       {Check: "update", State: "ok", Msg: "1.0 → 2.0"},
		"Update failed":           {Check: "update", State: "fail", Msg: "2.0: crashed"},
	}
	for want, e := range cases {
		if got := eventText("en", "car", e); got != want {
			t.Errorf("%+v: %q, want %q", e, got, want)
		}
	}
	n := newNotifier(&configStore{c: defaultConfig()}, openEventLog(""), nil, &outbound{}, "")
	tg := n.chans[0]
	if !tg.wants(event{Check: "update", State: "info"}) || tg.wants(event{Check: "started", State: "info"}) {
		t.Error("Telegram should announce new versions but not starts")
	}
}

func TestUpdateAPI(t *testing.T) {
	c := newTestClient(t)
	if code, _ := c.do("POST", "/api/login", `{"password":"`+demoPassword+`"}`, true); code != http.StatusOK {
		t.Fatal("login")
	}
	code, st := c.do("GET", "/api/update", "", false)
	latest, _ := st["latest"].(map[string]any)
	if code != http.StatusOK || latest == nil || latest["sig"] != nil {
		t.Fatalf("update status: %d %v", code, st)
	}
	if code, out := c.do("POST", "/api/update/install", `{"versionCode":1}`, true); code != http.StatusConflict || out["code"] != "update_stale" {
		t.Fatalf("stale install: %d %v", code, out)
	}
	body := fmt.Sprintf(`{"versionCode":%d}`, int64(latest["versionCode"].(float64)))
	if code, out := c.do("POST", "/api/update/install", body, true); code != http.StatusOK {
		t.Fatalf("install: %d %v", code, out)
	}
	waitFor(t, "the demo install", func() bool {
		_, st := c.do("GET", "/api/update", "", false)
		return st["skipped"] == latest["version"]
	})
	if code, st := c.do("POST", "/api/update/check", "", true); code != http.StatusOK || st["latest"] == nil {
		t.Fatalf("check: %d %v", code, st)
	}
	if _, st := c.do("GET", "/api/status", "", false); st["update"] != latest["version"] {
		t.Errorf("status bar update: %v", st["update"])
	}
}
