package main

import (
	"encoding/json"
	"io/fs"
	"regexp"
	"sort"
	"sync"
)

var (
	localesMu  sync.Mutex
	locales    = map[string]map[string]string{}
	localeList []string
	varRe      = regexp.MustCompile(`\{(\w+)\}`)
)

// localeNames lists the languages of web/i18n/index.json
func localeNames() []string {
	localesMu.Lock()
	defer localesMu.Unlock()
	if localeList == nil {
		var idx map[string]string
		if b, err := fs.ReadFile(webFS, "web/i18n/index.json"); err == nil {
			json.Unmarshal(b, &idx)
		}
		localeList = []string{}
		for k := range idx {
			localeList = append(localeList, k)
		}
		sort.Strings(localeList)
	}
	return localeList
}

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
