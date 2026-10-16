package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

// staticHandler serves the embedded web UI. The UI ships with the binary, so everything except fonts
// is revalidated on each load (ETags are content hashes, unchanged files get a 304). __BUILD__ in
// sw.js is replaced with a hash of all files so the service worker refreshes its cache on upgrade.
func staticHandler() http.Handler {
	sub, _ := fs.Sub(webFS, "web")
	etags := map[string]string{}
	all := sha256.New()
	fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(sub, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		etags["/"+p] = `"` + hex.EncodeToString(sum[:8]) + `"`
		if p != "sw.js" {
			all.Write([]byte(p))
			all.Write(sum[:])
		}
		return nil
	})
	build := hex.EncodeToString(all.Sum(nil)[:6])
	swSrc, _ := fs.ReadFile(sub, "sw.js")
	sw := bytes.ReplaceAll(swSrc, []byte("__BUILD__"), []byte(build))
	fsrv := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/sw.js" {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("ETag", `"`+build+`"`)
			http.ServeContent(w, r, "sw.js", time.Time{}, bytes.NewReader(sw))
			return
		}
		if strings.HasPrefix(p, "/fonts/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		if strings.HasSuffix(p, ".webmanifest") {
			w.Header().Set("Content-Type", "application/manifest+json")
		}
		key := p
		if strings.HasSuffix(key, "/") {
			key += "index.html"
		}
		if e, ok := etags[key]; ok {
			w.Header().Set("ETag", e)
		}
		fsrv.ServeHTTP(w, r)
	})
}
