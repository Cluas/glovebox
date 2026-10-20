package main

import (
	"errors"
	"net/http"
)

func (s *server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.update.info())
}

// handleUpdateCheck checks right away; a failed check is reported in the returned status
func (s *server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if s.cfg.get().UpdateURL == "" {
		writeErr(w, http.StatusBadRequest, apiErr("update_off", "remote upgrade is turned off (update_url is empty)"))
		return
	}
	if _, err := s.update.check(r.Context()); err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			writeErr(w, http.StatusConflict, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, s.update.info())
}

// handleUpdateInstall starts installing the version the last check found; the progress is in
// GET /api/update, and the console restarts when it is done
func (s *server) handleUpdateInstall(w http.ResponseWriter, r *http.Request) {
	var in struct {
		VersionCode int64 `json:"versionCode"`
	}
	if err := readBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !s.update.info().Managed {
		writeErr(w, http.StatusBadRequest, apiErr("update_unmanaged", "glovebox is not running from its Magisk module"))
		return
	}
	if err := s.update.installLatest(in.VersionCode); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
