package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Feishu Open Platform calls the bot makes: its tenant_access_token, and sending, answering and
// updating cards. They need the "send messages as the app" permission (im:message:send_as_bot).

// feishuAPIError is an error code from the Open Platform
type feishuAPIError struct {
	Code int
	Msg  string
}

func (e *feishuAPIError) Error() string { return fmt.Sprintf("feishu: %s (code %d)", e.Msg, e.Code) }

// codes of an invalid or expired tenant_access_token
func feishuTokenInvalid(err error) bool {
	var e *feishuAPIError
	return errors.As(err, &e) && (e.Code == 99991663 || e.Code == 99991661 || e.Code == 99991668)
}

func (b *feishuBot) scrub(err error) error {
	if err == nil {
		return nil
	}
	c := b.s.cfg.get()
	err = scrub(err, c.FeishuAppSecret)
	b.mu.Lock()
	tok := b.token
	b.mu.Unlock()
	return scrub(err, tok)
}

// post sends JSON to the Open Platform and decodes its {code, msg, ...} answer into out
func (b *feishuBot) post(ctx context.Context, method, path, token string, body, out any) error {
	c := b.s.cfg.get()
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.FeishuDomain, "/")+path, bytes.NewReader(feishuJSON(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := b.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var r struct {
		Code *int   `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Code == nil {
		return fmt.Errorf("feishu: %s", resp.Status)
	}
	if *r.Code != 0 {
		return &feishuAPIError{*r.Code, r.Msg}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// tenantToken returns the cached token, or a new one when it has less than 30 minutes left
func (b *feishuBot) tenantToken(ctx context.Context, fresh bool) (string, error) {
	b.mu.Lock()
	tok, exp := b.token, b.tokenExp
	b.mu.Unlock()
	if !fresh && tok != "" && time.Until(exp) > 30*time.Minute {
		return tok, nil
	}
	c := b.s.cfg.get()
	var r struct {
		Token  string `json:"tenant_access_token"`
		Expire int64  `json:"expire"`
	}
	err := b.post(ctx, http.MethodPost, "/open-apis/auth/v3/tenant_access_token/internal", "",
		map[string]string{"app_id": c.FeishuAppID, "app_secret": c.FeishuAppSecret}, &r)
	if err != nil {
		return "", b.scrub(err)
	}
	if r.Token == "" {
		return "", errors.New("feishu: no tenant_access_token in the answer")
	}
	b.mu.Lock()
	b.token, b.tokenExp = r.Token, time.Now().Add(time.Duration(r.Expire)*time.Second)
	b.mu.Unlock()
	return r.Token, nil
}

// call makes an authorized request, with a new token once when the cached one was refused
func (b *feishuBot) call(ctx context.Context, method, path string, body, out any) error {
	for attempt := 0; ; attempt++ {
		tok, err := b.tenantToken(ctx, attempt > 0)
		if err != nil {
			return err
		}
		err = b.post(ctx, method, path, tok, body, out)
		if attempt == 0 && feishuTokenInvalid(err) {
			continue
		}
		return b.scrub(err)
	}
}

func cardContent(card map[string]any) string { return string(feishuJSON(card)) }

// sendCard sends a card to a user (open_id) or a chat (chat_id) and returns its message id
func (b *feishuBot) sendCard(ctx context.Context, idType, id string, card map[string]any) (string, error) {
	var r struct {
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	err := b.call(ctx, http.MethodPost, "/open-apis/im/v1/messages?receive_id_type="+url.QueryEscape(idType),
		map[string]string{"receive_id": id, "msg_type": "interactive", "content": cardContent(card)}, &r)
	return r.Data.MessageID, err
}

// updateCard replaces a card the bot sent; both cards declare update_multi
func (b *feishuBot) updateCard(ctx context.Context, messageID string, card map[string]any) error {
	return b.call(ctx, http.MethodPatch, "/open-apis/im/v1/messages/"+url.PathEscape(messageID),
		map[string]string{"content": cardContent(card)}, nil)
}
