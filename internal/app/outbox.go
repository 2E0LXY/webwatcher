package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"time"
)

const (
	OutboxMaxAttempts = 5
	outboxBatch       = 20
)

// Outbox statuses.
const (
	OutQueued     = "queued"
	OutAccepted   = "accepted"   // the provider accepted the message (not necessarily delivered)
	OutFailed     = "failed"     // gave up after OutboxMaxAttempts
	OutSuppressed = "suppressed" // user opted out or has no consent at send time
)

type outboxRow struct {
	ID       int64
	UserID   int64
	Kind     string
	Text     string
	Params   []string
	Attempts int
	Phone    string
	Allowed  bool
	Fresh    bool // the watch still exists, is not paused and has not been edited since the alert was queued
}

func (s *Store) dueOutbox(limit int) ([]outboxRow, error) {
	rows, err := s.db.Query(`SELECT o.id,o.user_id,o.kind,o.text,o.params,o.attempts,u.phone,(u.alerts_enabled=1 AND u.consent_at IS NOT NULL), (w.paused=0 AND w.version=o.watch_version)
		FROM outbox o JOIN users u ON u.id=o.user_id JOIN watches w ON w.id=o.watch_id
		WHERE o.status='queued' AND o.next_attempt_at<=? ORDER BY o.id LIMIT ?`, s.now(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		var params string
		var allowed, fresh int
		if err := rows.Scan(&r.ID, &r.UserID, &r.Kind, &r.Text, &params, &r.Attempts, &r.Phone, &allowed, &fresh); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(params), &r.Params)
		r.Allowed, r.Fresh = allowed != 0, fresh != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) finishOutbox(id int64, status string, attempts int, next int64, lastErr string) error {
	var done *int64
	if status != OutQueued {
		n := s.now()
		done = &n
	}
	_, err := s.db.Exec(`UPDATE outbox SET status=?, attempts=?, next_attempt_at=?, last_error=?, done_at=? WHERE id=?`,
		status, attempts, next, lastErr, done, id)
	return err
}

// TryReserveSend claims one outbound message in the rolling-hour global budget.
// It fails if the hour's total has reached total, or the hour's count for this
// kind has reached kindLimit (so login links cannot use up the alert budget).
// Check and claim happen in one transaction.
func (s *Store) TryReserveSend(kind string, kindLimit, total int) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	since := s.now() - 3600
	var all, mine int
	if err := tx.QueryRow(`SELECT COUNT(*), COALESCE(SUM(kind=?),0) FROM send_log WHERE at>?`, kind, since).Scan(&all, &mine); err != nil {
		return false, err
	}
	if all >= total || mine >= kindLimit {
		return false, nil
	}
	if _, err := tx.Exec(`INSERT INTO send_log(at, kind) VALUES(?,?)`, s.now(), kind); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// OutboxCounts returns the number of rows per status (for tests and diagnostics).
func (s *Store) OutboxCounts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM outbox GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// Sender delivers queued alerts. All state lives in the database, so queued
// alerts survive a restart. Delivery is at least once: a crash between the
// provider accepting a message and the row being updated resends it.
type Sender struct {
	store  *Store
	msgr   Messenger
	clock  Clock
	budget int // attempts per rolling hour, across all users
	tick   time.Duration
}

func NewSender(store *Store, msgr Messenger, clock Clock, budgetPerHour int) *Sender {
	if budgetPerHour < 1 {
		budgetPerHour = 200
	}
	return &Sender{store: store, msgr: msgr, clock: clock, budget: budgetPerHour, tick: 10 * time.Second}
}

func (s *Sender) Run(ctx context.Context) {
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		s.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// backoff after the nth failed attempt: 30s, 2m, 8m, 32m.
func outboxBackoff(attempts int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < attempts; i++ {
		d *= 4
	}
	return d
}

// RunOnce attempts every due queued alert once and returns the number of
// provider calls made.
func (s *Sender) RunOnce(ctx context.Context) int {
	rows, err := s.store.dueOutbox(outboxBatch)
	if err != nil {
		slog.Error("outbox: list due", "err", err)
		return 0
	}
	calls := 0
	for _, r := range rows {
		if ctx.Err() != nil {
			break
		}
		if !r.Allowed {
			s.store.finishOutbox(r.ID, OutSuppressed, r.Attempts, 0, "user opted out or has not consented")
			continue
		}
		if !r.Fresh {
			s.store.finishOutbox(r.ID, OutSuppressed, r.Attempts, 0, "watch was paused or edited after the alert was queued")
			continue
		}
		ok, err := s.store.TryReserveSend("alert", s.budget, s.budget)
		if err != nil {
			slog.Error("outbox: budget", "err", RedactPII(err.Error()))
			break
		}
		if !ok {
			slog.Warn("outbox: hourly send budget reached; alerts stay queued", "budget", s.budget)
			break
		}
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = s.msgr.Send(sctx, Message{Kind: KindAlert, To: r.Phone, Text: r.Text, Params: r.Params})
		cancel()
		calls++
		attempts := r.Attempts + 1
		switch {
		case err == nil:
			s.store.finishOutbox(r.ID, OutAccepted, attempts, 0, "")
		case attempts >= OutboxMaxAttempts:
			slog.Error("alert failed permanently", "outbox", r.ID, "number", MaskPhone(r.Phone), "err", RedactPII(err.Error()))
			s.store.finishOutbox(r.ID, OutFailed, attempts, 0, RedactPII(err.Error()))
		default:
			slog.Warn("alert send failed; will retry", "outbox", r.ID, "attempt", attempts, "err", RedactPII(err.Error()))
			next := s.clock.Now().Add(outboxBackoff(attempts)).Unix()
			s.store.finishOutbox(r.ID, OutQueued, attempts, next, RedactPII(err.Error()))
		}
	}
	return calls
}

var (
	urlRe    = regexp.MustCompile(`https?://([^/\s?#"'<>]+)[^\s"'<>]*`)
	digitsRe = regexp.MustCompile(`\+?\d[\d ]{5,}\d`)
)

// RedactPII removes what must never be logged: URLs are reduced to their host
// (no path, no query string) and long digit runs (phone numbers) to their last
// three digits.
func RedactPII(s string) string {
	s = urlRe.ReplaceAllString(s, "$1")
	return digitsRe.ReplaceAllStringFunc(s, MaskPhone)
}
