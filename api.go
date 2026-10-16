package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
)

// apiError carries a stable code that the web UI translates; Msg is the English fallback.
type apiError struct {
	Code string
	Msg  string
	Args map[string]any
}

func (e *apiError) Error() string { return e.Msg }

// apiErr builds an apiError; kv are alternating argument names and values for the UI's message template.
func apiErr(code, msg string, kv ...any) *apiError {
	e := &apiError{Code: code, Msg: msg}
	for i := 0; i+1 < len(kv); i += 2 {
		if e.Args == nil {
			e.Args = map[string]any{}
		}
		e.Args[fmt.Sprint(kv[i])] = kv[i+1]
	}
	return e
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// writeErr responds with {"error": message, "code": ..., "args": {...}}; code and args only for apiError
func writeErr(w http.ResponseWriter, code int, err error) {
	out := map[string]any{"error": err.Error()}
	var ae *apiError
	if errors.As(err, &ae) {
		out["code"] = ae.Code
		if len(ae.Args) > 0 {
			out["args"] = ae.Args
		}
	}
	writeJSON(w, code, out)
}

func readBody(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10))
	if err := dec.Decode(v); err != nil {
		return apiErr("bad_request", "invalid request body: "+err.Error())
	}
	return nil
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if err := readBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.auth.login(in.Password); err != nil {
		log.Printf("login failed from %s: %v", r.RemoteAddr, err)
		code := http.StatusUnauthorized
		var ae *apiError
		if errors.As(err, &ae) && ae.Code == "locked" {
			code = http.StatusTooManyRequests
		}
		writeErr(w, code, err)
		return
	}
	log.Printf("login from %s", r.RemoteAddr)
	s.auth.issue(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.clear(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"logged_in": s.auth.valid(r)})
}

func (s *server) handleInfo(w http.ResponseWriter, r *http.Request) {
	c := s.cfg.get()
	out := map[string]any{
		"version":   version,
		"hostnames": c.Hostnames,
		"tailscale": c.tailscaleEnabled(),
		"demo":      s.demo,
	}
	if s.certs != nil {
		out["https"] = s.certs.info(c)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCA serves the local root certificate (public data, no login needed).
// Once a phone trusts it, the HTTPS address works without warnings.
func (s *server) handleCA(w http.ResponseWriter, r *http.Request) {
	var b []byte
	if s.certs != nil {
		b = s.certs.caPEM()
	}
	if b == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="glovebox-ca.crt"`)
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}
