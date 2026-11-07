package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runRescue runs module/rescue.sh against a scratch /data/adb and waits for it
func runRescue(t *testing.T, adb string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	script := filepath.Join(t.TempDir(), rescueScriptName)
	if err := os.WriteFile(script, rescueScript, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", script)
	cmd.Env = append(os.Environ(), "GLOVEBOX_ADB="+adb, "GLOVEBOX_NOW=1", "GLOVEBOX_BOX_WAIT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
}

func writeFile(t *testing.T, path, s string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestRescueScript(t *testing.T) {
	adb := t.TempDir()
	mods := filepath.Join(adb, "modules")
	// left by Magisk's safe mode: mode 000
	writeFile(t, filepath.Join(mods, "glovebox", "disable"), "", 0)
	writeFile(t, filepath.Join(mods, "glovebox", "service.sh"), "touch \"${0%/*}/started\"\n", 0o755)
	// turned off in the Magisk app
	writeFile(t, filepath.Join(mods, "box_for_root", "disable"), "", 0o644)
	// another module is not touched
	writeFile(t, filepath.Join(mods, "other", "disable"), "", 0)
	runRescue(t, adb)

	if exists(filepath.Join(mods, "glovebox", "disable")) {
		t.Error("glovebox is still disabled")
	}
	if !exists(filepath.Join(mods, "glovebox", "started")) {
		t.Error("glovebox's service.sh did not run")
	}
	if !exists(filepath.Join(mods, "box_for_root", "disable")) {
		t.Error("a module turned off by hand was turned back on")
	}
	if !exists(filepath.Join(mods, "other", "disable")) {
		t.Error("another module was turned back on")
	}
	b, err := os.ReadFile(filepath.Join(adb, "glovebox", rescueNote))
	if err != nil || strings.TrimSpace(string(b)) != "glovebox" {
		t.Errorf("note %q, %v", b, err)
	}

	// nothing to do: no note, service.sh not run again
	os.Remove(filepath.Join(adb, "glovebox", rescueNote))
	os.Remove(filepath.Join(mods, "glovebox", "started"))
	runRescue(t, adb)
	if exists(filepath.Join(adb, "glovebox", rescueNote)) || exists(filepath.Join(mods, "glovebox", "started")) {
		t.Error("acted without a safe mode disable file")
	}
}

func TestRescueScriptStartsBox(t *testing.T) {
	adb := t.TempDir()
	writeFile(t, filepath.Join(adb, "modules", "glovebox", "module.prop"), "", 0o644)
	writeFile(t, filepath.Join(adb, "modules", "box_for_root", "disable"), "", 0)
	// stands in for Box's scripts; box.pid names no running process, so Box is not running
	writeFile(t, filepath.Join(adb, "box", "scripts", "box.service"), "echo \"$1\" >> \"${0%/*}/../calls\"\n", 0o755)
	writeFile(t, filepath.Join(adb, "box", "scripts", "box.iptables"), "echo \"iptables $1\" >> \"${0%/*}/../calls\"\n", 0o755)
	writeFile(t, filepath.Join(adb, "box", "run", "box.pid"), "999999\n", 0o644)
	runRescue(t, adb)

	if exists(filepath.Join(adb, "modules", "box_for_root", "disable")) {
		t.Error("Box is still disabled")
	}
	b, _ := os.ReadFile(filepath.Join(adb, "box", "calls"))
	if got := strings.Fields(string(b)); strings.Join(got, " ") != "start iptables enable" {
		t.Errorf("Box calls %q", got)
	}
	n, _ := os.ReadFile(filepath.Join(adb, "glovebox", rescueNote))
	if strings.TrimSpace(string(n)) != "box_for_root" {
		t.Errorf("note %q", n)
	}
}

func TestRescueScriptRemovesItselfAfterUninstall(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	adb := t.TempDir()
	script := filepath.Join(adb, "service.d", rescueScriptName)
	writeFile(t, script, string(rescueScript), 0o755)
	cmd := exec.Command("sh", script)
	cmd.Env = append(os.Environ(), "GLOVEBOX_ADB="+adb, "GLOVEBOX_NOW=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if exists(script) {
		t.Error("the script stayed after Glovebox was uninstalled")
	}
}

func TestInstallRescue(t *testing.T) {
	adb := t.TempDir()
	mod := filepath.Join(adb, "modules", "glovebox")
	path := filepath.Join(adb, "service.d", rescueScriptName)
	if err := installRescue(mod); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != string(rescueScript) {
		t.Fatalf("installed %d bytes, %v", len(b), err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", fi.Mode())
	}
	// an outdated or non-executable copy is replaced
	writeFile(t, path, "old", 0o644)
	if err := installRescue(mod); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	fi, _ := os.Stat(path)
	if string(b) != string(rescueScript) || fi.Mode().Perm() != 0o755 {
		t.Errorf("not replaced: %d bytes, mode %v", len(b), fi.Mode())
	}
}

func TestNoteRescue(t *testing.T) {
	dir := t.TempDir()
	s := &server{dataDir: dir, events: openEventLog("")}
	// two boots before Glovebox read the note, and a line that is not a module ID
	writeFile(t, filepath.Join(dir, rescueNote), " glovebox box_for_root\n box_for_root\n<b>\n", 0o644)
	s.noteRescue()
	evs := s.events.since(0)
	if len(evs) != 1 || evs[0].Check != "rescue" || evs[0].State != "info" || evs[0].Msg != "glovebox, box_for_root" {
		t.Fatalf("events %+v", evs)
	}
	if exists(filepath.Join(dir, rescueNote)) {
		t.Error("note not removed")
	}
	s.noteRescue()
	if n := len(s.events.since(0)); n != 1 {
		t.Errorf("%d events after the note was gone", n)
	}

	c := defaultConfig()
	if !notifyWants(t, c, "telegram", evs[0]) {
		t.Error("Telegram does not announce it")
	}
	if got := eventText("en", "car", evs[0]); !strings.Contains(got, "glovebox, box_for_root") || !strings.Contains(got, "safe mode") {
		t.Errorf("text %q", got)
	}
}

// notifyWants reports whether the named channel takes e
func notifyWants(t *testing.T, c Config, name string, e event) bool {
	t.Helper()
	n := newNotifier(&configStore{c: c}, openEventLog(""), nil, &outbound{}, "")
	for _, ch := range n.chans {
		if ch.name == name {
			return ch.wants(c, e)
		}
	}
	t.Fatalf("no channel %s", name)
	return false
}
