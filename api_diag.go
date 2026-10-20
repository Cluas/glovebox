package main

import "net/http"

func (s *server) handleDiag(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.diag.info())
}
