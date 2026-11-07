package main

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"
)

// handleHealth returns the check states, the latest events and the state of each report channel
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ch := s.notify.status()
	ch["logs"] = s.ship.status()
	writeJSON(w, http.StatusOK, map[string]any{
		"checks":   s.health.snapshot(),
		"events":   healthEvents(s.events.recent(50)),
		"channels": ch,
		"seq":      s.events.lastSeq(),
	})
}

// healthEvents leaves out SMS, which has its own panel
func healthEvents(evs []event) []event {
	return slices.DeleteFunc(evs, func(e event) bool { return e.Check == "sms" })
}

// handleReportTest sends a test message through every enabled channel and waits for the results
func (s *server) handleReportTest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	res := s.notify.test(ctx)
	if s.cfg.get().LogPushURL != "" {
		res["logs"] = s.ship.test(ctx)
	}
	if len(res) == 0 {
		writeErr(w, http.StatusBadRequest, apiErr("report_off", "no report channel is configured"))
		return
	}
	var sent, failed []string
	for name, err := range res {
		if err != nil {
			failed = append(failed, name+": "+err.Error())
		} else {
			sent = append(sent, name)
		}
	}
	slices.Sort(sent)
	slices.Sort(failed)
	if len(failed) > 0 {
		msg := strings.Join(failed, "; ")
		writeErr(w, http.StatusBadGateway, apiErr("report_failed", "sending failed: "+msg, "detail", msg))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": sent})
}
