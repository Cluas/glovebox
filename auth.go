package main

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pbkdf2Iter     = 60000 // about 0.1 s on a ZTE F50
	sessionCookie  = "glovebox_session"
	sessionTTL     = 30 * 24 * time.Hour // long-lived: in-car screens shouldn't keep asking
	minPasswordLen = 6
)

func errPasswordShort() error {
	return apiErr("password_short", fmt.Sprintf("password must be at least %d characters", minPasswordLen), "min", minPasswordLen)
}

// format: pbkdf2-sha256$iterations$salt(base64)$key(base64)
func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

func checkPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[2])
	want, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

type auth struct {
	cfg *configStore

	mu        sync.Mutex
	failures  int
	lockUntil time.Time
}

// session cookie = expiry.HMAC(secret, expiry|password hash); changing the password invalidates all sessions
func (a *auth) sign(exp int64, c Config) string {
	secret, _ := hex.DecodeString(c.SessionSecret)
	m := hmac.New(sha256.New, secret)
	fmt.Fprintf(m, "v1|%d|%s", exp, c.PasswordHash)
	return hex.EncodeToString(m.Sum(nil))
}

func (a *auth) issue(w http.ResponseWriter) {
	c := a.cfg.get()
	exp := time.Now().Add(sessionTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    strconv.FormatInt(exp, 10) + "." + a.sign(exp, c),
		Path:     "/",
		MaxAge:   int(sessionTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *auth) clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
}

func (a *auth) valid(r *http.Request) bool {
	ck, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	expStr, sig, ok := strings.Cut(ck.Value, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	c := a.cfg.get()
	if c.PasswordHash == "" {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(a.sign(exp, c)))
}

// login locks for 1 minute after 5 consecutive failures, doubling with each further failure (max 30 minutes)
func (a *auth) login(pw string) error {
	a.mu.Lock()
	if wait := time.Until(a.lockUntil); wait > 0 {
		a.mu.Unlock()
		sec := int(wait.Seconds()) + 1
		return apiErr("locked", fmt.Sprintf("too many attempts, try again in %d s", sec), "seconds", sec)
	}
	a.mu.Unlock()

	c := a.cfg.get()
	ok := c.PasswordHash != "" && checkPassword(c.PasswordHash, pw)

	a.mu.Lock()
	defer a.mu.Unlock()
	if ok {
		a.failures = 0
		return nil
	}
	a.failures++
	if a.failures >= 5 {
		d := time.Minute << min(a.failures-5, 5)
		a.lockUntil = time.Now().Add(min(d, 30*time.Minute))
	}
	return apiErr("password_wrong", "wrong password")
}
