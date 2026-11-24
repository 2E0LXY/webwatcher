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

// DefaultLimiterKeys bounds the memory a RateLimiter may use.
const DefaultLimiterKeys = 10000

// RateLimiter is a small in-memory sliding-window limiter. Its state is hard
// bounded: at most maxKeys keys, each with at most limit timestamps. When the
// key table is full, expired keys go first and then the oldest keys, so a
// flood of distinct keys cannot grow memory (it can only push old keys out).
type RateLimiter struct {
	mu      sync.Mutex
	hits    map[string][]time.Time
	order   []string // keys in order of first appearance; each key at most once
	limit   int
	window  time.Duration
	maxKeys int
}

func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{hits: map[string][]time.Time{}, limit: limit, window: window, maxKeys: DefaultLimiterKeys}
}

// Len reports how many keys are tracked.
func (r *RateLimiter) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.hits)
}

func (r *RateLimiter) evict(cut time.Time) {
	// first pass: drop keys whose hits have all expired
	kept := r.order[:0]
	for _, k := range r.order {
		h, ok := r.hits[k]
		if !ok {
			continue
		}
		if len(h) == 0 || !h[len(h)-1].After(cut) {
			delete(r.hits, k)
			continue
		}
		kept = append(kept, k)
	}
	r.order = kept
	// still full: drop the oldest keys
	for len(r.hits) >= r.maxKeys && len(r.order) > 0 {
		delete(r.hits, r.order[0])
		r.order = r.order[1:]
	}
}

func (r *RateLimiter) Allow(key string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	cut := now.Add(-r.window)
	prev, known := r.hits[key]
	if !known && len(r.hits) >= r.maxKeys {
		r.evict(cut)
	}
	h := prev[:0]
	for _, t := range prev {
		if t.After(cut) {
			h = append(h, t)
		}
	}
	if len(h) >= r.limit {
		r.hits[key] = h
		return false
	}
	if !known {
		r.order = append(r.order, key)
	}
	r.hits[key] = append(h, now)
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
	budget  int // global outbound messages per hour; login links may use half of it
}

func NewAuth(store *Store, msgr Messenger, clock Clock, baseURL string) *Auth {
	return &Auth{
		store: store, msgr: msgr, clock: clock, baseURL: strings.TrimRight(baseURL, "/"),
		byPhone: NewRateLimiter(5, time.Hour),
		byIP:    NewRateLimiter(20, time.Hour),
		budget:  200,
	}
}

// SetSendBudget sets the global hourly message budget shared with alerts.
func (a *Auth) SetSendBudget(n int) {
	if n > 0 {
		a.budget = n
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
	ok, err := a.store.TryReserveSend("login", max(1, a.budget/2), a.budget)
	if err != nil {
		slog.Error("login send budget", "err", RedactPII(err.Error()))
		return
	}
	if !ok {
		slog.Warn("login send budget reached; login request dropped")
		return
	}
	raw, hash := newToken()
	if err := a.store.CreateLoginToken(phone, hash, LoginTokenTTL, consent); err != nil {
		slog.Error("create login token", "err", RedactPII(err.Error()))
		return
	}
	link := a.baseURL + "/auth/" + raw
	text := "Your Webwatcher login link: " + link + "\nIt works once and expires in 15 minutes."
	err = a.msgr.Send(ctx, Message{Kind: KindLogin, To: phone, Text: text, Params: []string{link}})
	if err != nil {
		slog.Error("send login message", "messenger", a.msgr.Name(), "err", RedactPII(err.Error()))
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
