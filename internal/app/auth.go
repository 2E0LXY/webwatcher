package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	LoginTokenTTL = 15 * time.Minute
	SessionTTL    = 30 * 24 * time.Hour
)

// newToken returns a random URL-safe token and its SHA-256 hex hash.
// Only the hash is ever stored.
func newToken() (raw, hash string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, hashToken(raw)
}

func hashToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

func ctEqual(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// RateLimiter is a small in-memory sliding-window limiter.
type RateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
}

func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{hits: map[string][]time.Time{}, limit: limit, window: window}
}

func (r *RateLimiter) Allow(key string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	cut := now.Add(-r.window)
	h := r.hits[key][:0]
	for _, t := range r.hits[key] {
		if t.After(cut) {
			h = append(h, t)
		}
	}
	if len(h) >= r.limit {
		r.hits[key] = h
		return false
	}
	r.hits[key] = append(h, now)
	if len(r.hits) > 20000 { // crude bound on memory
		for k, v := range r.hits {
			if len(v) == 0 || !v[len(v)-1].After(cut) {
				delete(r.hits, k)
			}
		}
	}
	return true
}

// Auth implements the magic-link flow.
type Auth struct {
	store   *Store
	msgr    Messenger
	clock   Clock
	baseURL string
	byPhone *RateLimiter
	byIP    *RateLimiter
}

func NewAuth(store *Store, msgr Messenger, clock Clock, baseURL string) *Auth {
	return &Auth{
		store: store, msgr: msgr, clock: clock, baseURL: strings.TrimRight(baseURL, "/"),
		byPhone: NewRateLimiter(5, time.Hour),
		byIP:    NewRateLimiter(20, time.Hour),
	}
}

// RequestLogin creates and sends a magic link. It never reveals whether the
// number is known or the request was rate limited; callers show one response.
func (a *Auth) RequestLogin(ctx context.Context, phone, ip string, consent bool) {
	now := a.clock.Now()
	if !a.byIP.Allow(ip, now) || !a.byPhone.Allow(phone, now) {
		slog.Warn("login rate limited")
		return
	}
	raw, hash := newToken()
	if err := a.store.CreateLoginToken(phone, hash, LoginTokenTTL, consent); err != nil {
		slog.Error("create login token", "err", err)
		return
	}
	link := a.baseURL + "/auth/" + raw
	text := "Your Webwatcher login link: " + link + "\nIt works once and expires in 15 minutes."
	err := a.msgr.Send(ctx, Message{Kind: KindLogin, To: phone, Text: text, Params: []string{link}})
	if err != nil {
		slog.Error("send login message", "messenger", a.msgr.Name(), "err", err)
	}
}

// Consume validates a login token and starts a session, returning the raw
// session token and its CSRF token.
func (a *Auth) Consume(rawToken string) (sessionRaw, csrf string, err error) {
	uid, _, err := a.store.ConsumeLoginToken(hashToken(rawToken))
	if err != nil {
		return "", "", err
	}
	sessionRaw, sh := newToken()
	csrf, _ = newToken()
	if err := a.store.CreateSession(uid, sh, csrf, SessionTTL); err != nil {
		return "", "", err
	}
	return sessionRaw, csrf, nil
}

func (a *Auth) Session(raw string) (*Session, error) {
	if raw == "" || len(raw) > 128 {
		return nil, ErrNotFound
	}
	return a.store.SessionByHash(hashToken(raw))
}
