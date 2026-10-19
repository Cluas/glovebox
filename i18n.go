package main

import (
	"encoding/json"
	"io/fs"
	"regexp"
	"sync"
)

var (
	localesMu sync.Mutex
	locales   = map[string]map[string]string{}
	varRe     = regexp.MustCompile(`\{(\w+)\}`)
)

func locale(lang string) map[string]string {
	localesMu.Lock()
	defer localesMu.Unlock()
	if m, ok := locales[lang]; ok {
		return m
	}
	var m map[string]string
	if b, err := fs.ReadFile(webFS, "web/i18n/"+lang+".json"); err == nil {
		json.Unmarshal(b, &m)
	}
	locales[lang] = m
	return m
}

// tr translates a web/i18n key for text the server sends itself (alerts), with the UI's {name}
// placeholders; a missing key falls back to English and then to the key
func tr(lang, key string, vars map[string]string) string {
	s, ok := locale(lang)[key]
	if !ok {
		if s, ok = locale("en")[key]; !ok {
			s = key
		}
	}
	return varRe.ReplaceAllStringFunc(s, func(m string) string { return vars[m[1:len(m)-1]] })
}
