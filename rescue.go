package main

import (
	"bytes"
	"context"
	_ "embed"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// When the power supply sags while the device boots, Magisk can end up in safe mode and disable
// every module, which keeps Glovebox and Box off until someone removes the disable files. The
// boot script in module/rescue.sh undoes that; Glovebox installs it into Magisk's service.d,
// which runs while the module is disabled, and reports what it did.

//go:embed module/rescue.sh
var rescueScript []byte

const (
	rescueScriptName = "glovebox-rescue.sh"
	rescueNote       = "rescued" // in the data folder, written by the script
	rescueEvery      = 30 * time.Second
)

// rescueModule matches the module IDs the note may name
var rescueModule = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// installRescue puts the boot script into service.d next to the modules folder of mod, the
// Magisk module Glovebox runs from, unless it is there already
func installRescue(mod string) error {
	dir := filepath.Join(filepath.Dir(filepath.Dir(mod)), "service.d")
	path := filepath.Join(dir, rescueScriptName)
	if b, err := os.ReadFile(path); err == nil && bytes.Equal(b, rescueScript) {
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() == 0o755 {
			return nil
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(path, rescueScript, 0o755); err != nil {
		return err
	}
	// Magisk only runs executable scripts, whatever the umask
	return os.Chmod(path, 0o755)
}

// rescueLoop reports the notes the boot script leaves. It also runs after Glovebox started:
// when only Box was disabled, Glovebox was already up before the script ran.
func (s *server) rescueLoop(ctx context.Context) {
	if s.dataDir == "" {
		return
	}
	if mod := moduleDir(); mod != "" {
		if err := installRescue(mod); err != nil {
			log.Printf("rescue script: %v", err)
		}
	}
	t := time.NewTicker(rescueEvery)
	defer t.Stop()
	for {
		s.noteRescue()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// noteRescue turns the script's note into an event and removes it
func (s *server) noteRescue() {
	path := filepath.Join(s.dataDir, rescueNote)
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	os.Remove(path)
	var mods []string
	seen := map[string]bool{}
	for _, m := range strings.Fields(string(b)) {
		if rescueModule.MatchString(m) && !seen[m] {
			seen[m] = true
			mods = append(mods, m)
		}
	}
	if len(mods) == 0 {
		return
	}
	msg := strings.Join(mods, ", ")
	log.Printf("Magisk safe mode had disabled %s; turned back on", msg)
	s.events.add(event{Check: "rescue", State: "info", Msg: msg})
}
