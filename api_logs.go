package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// logTailBytes is how much of the end of a log the Logs page receives
const logTailBytes = 48 << 10

// handleLogs returns the tail of one log source: mihomo, box (Box for Root), glovebox or system (logcat)
func (s *server) handleLogs(w http.ResponseWriter, r *http.Request) {
	box := s.cfg.get().BoxDir
	var text string
	var err error
	switch r.URL.Query().Get("src") {
	case "", "mihomo":
		text, err = tailFile(filepath.Join(box, "run/clash.log"), logTailBytes)
	case "box":
		text, err = tailFile(filepath.Join(box, "run/runs.log"), logTailBytes)
	case "glovebox":
		text, err = tailFile(filepath.Join(s.logDir, logFileName), logTailBytes)
	case "system":
		if s.demo {
			text = demoLogcat()
			break
		}
		// with -t plus a priority filter logcat takes the last N lines before filtering,
		// so dump everything and trim here
		text, err = run(r.Context(), 10*time.Second, "logcat", "-d", "-v", "time", "*:W")
		if len(text) > logTailBytes {
			text = text[len(text)-logTailBytes:]
			if i := strings.IndexByte(text, '\n'); i >= 0 {
				text = text[i+1:]
			}
		}
	default:
		writeErr(w, http.StatusBadRequest, apiErr("log_unknown", "unknown log source"))
		return
	}
	if err != nil && text == "" {
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusOK, map[string]any{"text": "", "missing": true})
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": text})
}
