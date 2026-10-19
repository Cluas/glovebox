package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCertDirsFor(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.Mkdir(a, 0o700)
	os.WriteFile(b, nil, 0o600) // a file, not a directory
	if got := certDirsFor([]string{a, b, filepath.Join(dir, "missing")}); got != a {
		t.Fatalf("got %q, want %q", got, a)
	}
	if got := certDirsFor([]string{filepath.Join(dir, "missing")}); got != "" {
		t.Fatalf("got %q for no directories", got)
	}
}
