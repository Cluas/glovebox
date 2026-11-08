package main

import (
	"context"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var smsNumberRe = regexp.MustCompile(`^\+?[0-9]{3,20}$`)

// handleSMS lists the newest messages, the device's own number and where new ones are forwarded
func (s *server) handleSMS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	msgs, err := s.zte.smsList(ctx, smsPage)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if msgs == nil {
		msgs = []smsMessage{}
	}
	unread := 0
	for _, m := range msgs {
		if m.State == "unread" {
			unread++
		}
	}
	number := ""
	if z, err := s.zteStatus(ctx); err == nil {
		number = str(z["sim_msisdn"])
	}
	c := s.cfg.get()
	var forward []string
	for _, ch := range []struct {
		name string
		on   bool
	}{{"telegram", c.telegramEnabled()}, {"whatsapp", c.whatsAppEnabled()}, {"webhook", c.WebhookURL != ""}} {
		if ch.on && smsForwards(c, ch.name) {
			forward = append(forward, ch.name)
		}
	}
	checked, werr := s.sms.status()
	writeJSON(w, http.StatusOK, map[string]any{
		"messages": msgs, "unread": unread, "number": number, "forward": forward,
		"checked": checked, "error": werr,
	})
}

func (s *server) handleSMSSend(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Number string `json:"number"`
		Text   string `json:"text"`
	}
	if err := readBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	number, text, err := checkSMS(in.Number, in.Text)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.sendSMS(r.Context(), number, text); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// checkSMS normalizes a message to send: the number without spaces, the text trimmed
func checkSMS(number, text string) (string, string, error) {
	n := strings.Join(strings.Fields(number), "")
	text = strings.TrimSpace(text)
	if !smsNumberRe.MatchString(n) {
		return "", "", apiErr("sms_number", "not a phone number: "+number)
	}
	if l := utf8.RuneCountInString(text); l == 0 || l > smsTextMax {
		return "", "", apiErr("sms_text", "the text must have 1 to "+strconv.Itoa(smsTextMax)+" characters", "max", smsTextMax)
	}
	return n, text, nil
}

// sendSMS sends a message checked by checkSMS
func (s *server) sendSMS(ctx context.Context, number, text string) error {
	if err := s.zte.smsSend(ctx, number, text); err != nil {
		return err
	}
	// the text stays out of the log, which is shipped
	log.Printf("sms: sent to %s (%d characters)", number, utf8.RuneCountInString(text))
	return nil
}

func (s *server) handleSMSDelete(w http.ResponseWriter, r *http.Request) {
	s.smsBatch(w, r, "DELETE_SMS", "deleted")
}

func (s *server) handleSMSRead(w http.ResponseWriter, r *http.Request) {
	s.smsBatch(w, r, "SET_MSG_READ", "")
}

// smsBatch runs a per-message vendor action on a list of ids
func (s *server) smsBatch(w http.ResponseWriter, r *http.Request, goformID, logVerb string) {
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if err := readBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(in.IDs) == 0 || len(in.IDs) > smsBatchMax {
		writeErr(w, http.StatusBadRequest, apiErr("sms_ids", "send 1 to "+strconv.Itoa(smsBatchMax)+" message ids", "max", smsBatchMax))
		return
	}
	if err := s.zte.smsEach(r.Context(), goformID, in.IDs); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if logVerb != "" {
		log.Printf("sms: %s %d messages", logVerb, len(in.IDs))
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
