package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func loadLocale(t *testing.T, name string) map[string]string {
	t.Helper()
	b, err := fs.ReadFile(webFS, "web/i18n/"+name+".json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s.json: %v", name, err)
	}
	return m
}

// Every locale listed in index.json must have exactly the keys of en.json.
func TestLocalesComplete(t *testing.T) {
	b, err := fs.ReadFile(webFS, "web/i18n/index.json")
	if err != nil {
		t.Fatal(err)
	}
	var index map[string]string
	if err := json.Unmarshal(b, &index); err != nil {
		t.Fatal(err)
	}
	en := loadLocale(t, "en")
	for code := range index {
		loc := loadLocale(t, code)
		for k := range en {
			if _, ok := loc[k]; !ok {
				t.Errorf("%s: missing key %q", code, k)
			}
		}
		for k := range loc {
			if _, ok := en[k]; !ok {
				t.Errorf("%s: unknown key %q", code, k)
			}
		}
	}
}

// Every static translation key used by the UI and every API error code must exist in en.json.
func TestTranslationKeysExist(t *testing.T) {
	en := loadLocale(t, "en")
	var used []string
	html, _ := fs.ReadFile(webFS, "web/index.html")
	for _, m := range regexp.MustCompile(`data-i18n(?:-ph)?="([\w.]+)"`).FindAllSubmatch(html, -1) {
		used = append(used, string(m[1]))
	}
	js, _ := fs.ReadFile(webFS, "web/app.js")
	for _, m := range regexp.MustCompile(`\bt\('([\w.]+)'`).FindAllSubmatch(js, -1) {
		used = append(used, string(m[1]))
	}
	for _, f := range goSources(t) {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range regexp.MustCompile(`apiErr\("(\w+)"`).FindAllSubmatch(src, -1) {
			used = append(used, "err."+string(m[1]))
		}
	}
	for _, k := range slices.Compact(slices.Sorted(slices.Values(used))) {
		if strings.HasSuffix(k, ".") {
			continue // dynamic key such as t('err.' + code), resolved with a fallback at runtime
		}
		if _, ok := en[k]; !ok {
			t.Errorf("key %q is used but missing from en.json", k)
		}
	}
	if len(used) < 50 {
		t.Fatalf("only found %d keys, extraction is broken", len(used))
	}
}

// Every key in en.json must be used, either literally or through a dynamic prefix such as
// t('err.' + code) or tEnum('sim', state).
func TestNoUnusedKeys(t *testing.T) {
	en := loadLocale(t, "en")
	html, _ := fs.ReadFile(webFS, "web/index.html")
	js, _ := fs.ReadFile(webFS, "web/app.js")
	src := string(html) + string(js)
	var prefixes []string
	for _, re := range []string{
		`\bt\('([\w.]+\.)'\s*\+`,     // t('err.' + code)
		"\\bt\\(`([\\w.]+\\.)\\$\\{", // t(`https.${k}`)
		`\btEnum\('(\w+)'`,           // tEnum('sim', v) -> sim.*
	} {
		for _, m := range regexp.MustCompile(re).FindAllStringSubmatch(src, -1) {
			p := m[1]
			if !strings.HasSuffix(p, ".") {
				p += "."
			}
			prefixes = append(prefixes, p)
		}
	}
	for k := range en {
		quoted := strings.Contains(src, "'"+k+"'") || strings.Contains(src, `"`+k+`"`)
		dynamic := slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(k, p) })
		if !quoted && !dynamic {
			t.Errorf("key %q is not used by the UI", k)
		}
	}
}

func goSources(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(files, func(f string) bool { return strings.HasSuffix(f, "_test.go") })
}

// Source files are English-only; translations live in web/i18n.
func TestNoCJKInSource(t *testing.T) {
	han := regexp.MustCompile(`\p{Han}`)
	err := fs.WalkDir(os.DirFS("."), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch p {
			case ".git", "dist", "web/i18n", "web/fonts":
				return fs.SkipDir
			}
			return nil
		}
		if !slices.Contains([]string{".go", ".js", ".css", ".html", ".sh", ".xml", ".prop", ".webmanifest", ".yml"}, extOf(p)) {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			if han.MatchString(line) {
				t.Errorf("%s:%d: non-English text: %s", p, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func extOf(p string) string {
	if i := strings.LastIndexByte(p, '.'); i >= 0 {
		return p[i:]
	}
	return ""
}
