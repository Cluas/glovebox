package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// readmeShape is the structure a README translation has to keep in step with README.md.
type readmeShape struct {
	headings []string // heading levels in order, e.g. "#", "##"
	fences   int
	tableRow int
	images   []string // screenshot file names in order
}

var readmeImg = regexp.MustCompile(`docs/screenshots/([\w-]+)/([\w.-]+\.png)`)

func parseReadme(t *testing.T, path string) (readmeShape, string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s readmeShape
	inCode := false
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inCode = !inCode
			s.fences++
			continue
		}
		if inCode {
			continue
		}
		if level, _, ok := strings.Cut(line, " "); ok && strings.Trim(level, "#") == "" && level != "" {
			s.headings = append(s.headings, level)
		}
		if strings.HasPrefix(trimmed, "|") {
			s.tableRow++
		}
	}
	for _, m := range readmeImg.FindAllStringSubmatch(string(b), -1) {
		s.images = append(s.images, m[1]+"/"+m[2])
	}
	return s, string(b)
}

// README.<lang>.md translations keep the sections, tables, code blocks and screenshots of README.md,
// use screenshots in their own language, and are linked from README.md.
func TestReadmeTranslationsInSync(t *testing.T) {
	en, enText := parseReadme(t, "README.md")
	if len(en.headings) < 10 || len(en.images) == 0 {
		t.Fatalf("README.md parsed as %+v, parser is broken", en)
	}
	translations, _ := filepath.Glob("README.*.md")
	if len(translations) == 0 {
		t.Fatal("no README translations found")
	}
	for _, path := range translations {
		lang := strings.TrimSuffix(strings.TrimPrefix(path, "README."), ".md")
		tr, _ := parseReadme(t, path)
		if !slices.Equal(tr.headings, en.headings) {
			t.Errorf("%s: headings %v, README.md has %v", path, tr.headings, en.headings)
		}
		if tr.fences != en.fences {
			t.Errorf("%s: %d code fences, README.md has %d", path, tr.fences, en.fences)
		}
		if tr.tableRow != en.tableRow {
			t.Errorf("%s: %d table rows, README.md has %d", path, tr.tableRow, en.tableRow)
		}
		var want []string
		for _, img := range en.images {
			want = append(want, lang+"/"+strings.TrimPrefix(img, "en/"))
		}
		if !slices.Equal(tr.images, want) {
			t.Errorf("%s: screenshots %v, want %v", path, tr.images, want)
		}
		if !strings.Contains(enText, "("+path+")") {
			t.Errorf("README.md does not link to %s", path)
		}
	}
	for _, path := range append(translations, "README.md") {
		s, _ := parseReadme(t, path)
		for _, img := range s.images {
			if _, err := os.Stat(filepath.Join("docs/screenshots", img)); err != nil {
				t.Errorf("%s: %v", path, err)
			}
		}
	}
}
