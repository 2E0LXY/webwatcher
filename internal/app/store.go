package app

import (
	"database/sql"
	"errors"
	"time"
)

var (
	ErrInvalidToken = errors.New("invalid or expired token")
	ErrWatchCap     = errors.New("watch limit reached")
	ErrNotFound     = errors.New("not found")
	ErrNoConsent    = errors.New("no recorded consent")
)

const (
	MaxWatchesPerUser = 30
	HistoryCap        = 500
	MaxFailures       = 10
)

// Store wraps the database. All SQL is parameterised.
type Store struct {
	db    *sql.DB
	clock Clock
}

func NewStore(db *sql.DB, clock Clock) *Store { return &Store{db: db, clock: clock} }

func (s *Store) now() int64 { return s.clock.Now().Unix() }

type Session struct {
	UserID        int64
	Phone         string
	CSRF          string
	AlertsEnabled bool
	ConsentAt     *int64
}

// CreateLoginToken stores only the hash of a single-use login token.
// When consent is true the explicit WhatsApp opt-in tick is recorded with the
// current time and copied to the user when the token is consumed.
func (s *Store) CreateLoginToken(phone, hash string, ttl time.Duration, consent bool) error {
	now := s.clock.Now()
	var c *int64
	if consent {
		n := now.Unix()
		c = &n
	}
	_, err := s.db.Exec(`INSERT INTO login_tokens(phone, token_hash, created_at, expires_at, consent_at) VALUES(?,?,?,?,?)`,
		phone, hash, now.Unix(), now.Add(ttl).Unix(), c)
	return err
}

// ConsumeLoginToken atomically marks the token used and returns the user id
// (creating the user on first login). A second call fails.
func (s *Store) ConsumeLoginToken(hash string) (int64, string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback()
	now := s.now()
	var phone string
	var consent *int64
	err = tx.QueryRow(`UPDATE login_tokens SET used_at=? WHERE token_hash=? AND used_at IS NULL AND expires_at>? RETURNING phone, consent_at`,
		now, hash, now).Scan(&phone, &consent)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrInvalidToken
	}
	if err != nil {
		return 0, "", err
	}
	var uid int64
	err = tx.QueryRow(`INSERT INTO users(phone, created_at, last_login_at, consent_at) VALUES(?,?,?,?)
		ON CONFLICT(phone) DO UPDATE SET last_login_at=excluded.last_login_at,
			consent_at=COALESCE(users.consent_at, excluded.consent_at) RETURNING id`,
		phone, now, now, consent).Scan(&uid)
	if err != nil {
		return 0, "", err
	}
	return uid, phone, tx.Commit()
}

func (s *Store) CreateSession(uid int64, hash, csrf string, ttl time.Duration) error {
	now := s.clock.Now()
	_, err := s.db.Exec(`INSERT INTO sessions(user_id, token_hash, csrf, created_at, expires_at) VALUES(?,?,?,?,?)`,
		uid, hash, csrf, now.Unix(), now.Add(ttl).Unix())
	return err
}

func (s *Store) SessionByHash(hash string) (*Session, error) {
	var x Session
	var ae int
	err := s.db.QueryRow(`SELECT s.user_id, u.phone, s.csrf, u.alerts_enabled, u.consent_at FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.token_hash=? AND s.expires_at>?`, hash, s.now()).Scan(&x.UserID, &x.Phone, &x.CSRF, &ae, &x.ConsentAt)
	x.AlertsEnabled = ae != 0
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &x, nil
}

func (s *Store) DeleteSession(hash string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash=?`, hash)
	return err
}

// DeleteUser erases the user and everything hanging off them (sessions,
// watches, history, alerts) plus any outstanding login tokens for the number.
func (s *Store) DeleteUser(uid int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM login_tokens WHERE phone=(SELECT phone FROM users WHERE id=?)`, uid); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM users WHERE id=?`, uid); err != nil {
		return err
	}
	return tx.Commit()
}

// SetAlertsEnabled turns WhatsApp alerts on or off for a user. Turning them
// on needs a recorded consent; it returns ErrNoConsent otherwise.
func (s *Store) SetAlertsEnabled(uid int64, on bool) error {
	return s.setAlerts(`id=?`, uid, on)
}

// SetAlertsByPhone is used by the inbound STOP/START webhooks. It reports
// whether a user with that number exists.
func (s *Store) SetAlertsByPhone(phone string, on bool) (bool, error) {
	err := s.setAlerts(`phone=?`, phone, on)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) setAlerts(where string, arg any, on bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := setAlertsTx(tx, where, arg, on); err != nil {
		return err
	}
	return tx.Commit()
}

func setAlertsTx(tx *sql.Tx, where string, arg any, on bool) error {
	var uid int64
	var consent *int64
	err := tx.QueryRow(`SELECT id, consent_at FROM users WHERE `+where, arg).Scan(&uid, &consent)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if on && consent == nil {
		return ErrNoConsent
	}
	v := 0
	if on {
		v = 1
	}
	_, err = tx.Exec(`UPDATE users SET alerts_enabled=? WHERE id=?`, v, uid)
	return err
}

// ApplyInbound records an inbound webhook message id (replay protection) and,
// if it is new, applies the STOP/START command to the user with that number.
// It returns replay=true if the id had been seen before.
func (s *Store) ApplyInbound(provider, msgID, phone string, on bool) (replay bool, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT OR IGNORE INTO webhook_seen(provider,msg_id,seen_at) VALUES(?,?,?)`, provider, msgID, s.now())
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return true, nil
	}
	if err := setAlertsTx(tx, `phone=?`, phone, on); err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrNoConsent) {
		return false, err
	}
	return false, tx.Commit()
}

// Cleanup removes expired sessions and old login tokens.
func (s *Store) Cleanup() error {
	now := s.now()
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at<?`, now); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM webhook_seen WHERE seen_at<?`, now-7*86400); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM login_tokens WHERE expires_at<?`, now-86400)
	return err
}

// ---- watches ----

type Watch struct {
	ID             int64
	UserID         int64
	URL            string
	Label          string
	Selector       string
	TargetPence    *int64
	IntervalMin    int
	Paused         bool
	Status         string
	LastPricePence *int64
	AlertBase      *int64
	Currency       string
	LastMethod     string
	LastError      string
	LastCheckedAt  *int64
	NextCheckAt    int64
	Failures       int
	FailureNotif   bool
	CreatedAt      int64
	Phone          string // joined from users; set by DueWatches/WatchByID
	AlertsOn       bool   // user has consented and not opted out
}

const watchCols = `w.id,w.user_id,w.url,w.label,w.selector,w.target_pence,w.interval_min,w.paused,w.status,
	w.last_price_pence,w.alert_base_pence,w.currency,w.last_method,w.last_error,w.last_checked_at,
	w.next_check_at,w.failures,w.failure_notified,w.created_at,u.phone,(u.alerts_enabled=1 AND u.consent_at IS NOT NULL)`

type rowScanner interface{ Scan(...any) error }

func scanWatch(r rowScanner) (*Watch, error) {
	var w Watch
	var paused, fn, ao int
	err := r.Scan(&w.ID, &w.UserID, &w.URL, &w.Label, &w.Selector, &w.TargetPence, &w.IntervalMin, &paused, &w.Status,
		&w.LastPricePence, &w.AlertBase, &w.Currency, &w.LastMethod, &w.LastError, &w.LastCheckedAt,
		&w.NextCheckAt, &w.Failures, &fn, &w.CreatedAt, &w.Phone, &ao)
	w.Paused, w.FailureNotif, w.AlertsOn = paused != 0, fn != 0, ao != 0
	return &w, err
}

func (s *Store) CountWatches(uid int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM watches WHERE user_id=?`, uid).Scan(&n)
	return n, err
}

// AddWatch inserts a watch, enforcing the per-user cap atomically.
func (s *Store) AddWatch(w Watch) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM watches WHERE user_id=?`, w.UserID).Scan(&n); err != nil {
		return 0, err
	}
	if n >= MaxWatchesPerUser {
		return 0, ErrWatchCap
	}
	now := s.now()
	res, err := tx.Exec(`INSERT INTO watches(user_id,url,label,selector,target_pence,interval_min,status,next_check_at,created_at)
		VALUES(?,?,?,?,?,?, 'pending', ?, ?)`, w.UserID, w.URL, w.Label, w.Selector, w.TargetPence, w.IntervalMin, now, now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, tx.Commit()
}

func (s *Store) ListWatches(uid int64) ([]*Watch, error) {
	rows, err := s.db.Query(`SELECT `+watchCols+` FROM watches w JOIN users u ON u.id=w.user_id WHERE w.user_id=? ORDER BY w.id`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Watch
	for rows.Next() {
		w, err := scanWatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// WatchByID returns the watch only if it belongs to uid (pass 0 to skip the check).
func (s *Store) WatchByID(id, uid int64) (*Watch, error) {
	q := `SELECT ` + watchCols + ` FROM watches w JOIN users u ON u.id=w.user_id WHERE w.id=?`
	args := []any{id}
	if uid != 0 {
		q += ` AND w.user_id=?`
		args = append(args, uid)
	}
	w, err := scanWatch(s.db.QueryRow(q, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return w, err
}

func (s *Store) DueWatches(limit int) ([]*Watch, error) {
	rows, err := s.db.Query(`SELECT `+watchCols+` FROM watches w JOIN users u ON u.id=w.user_id
		WHERE w.paused=0 AND w.failures<? AND w.next_check_at<=? ORDER BY w.next_check_at LIMIT ?`, MaxFailures, s.now(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Watch
	for rows.Next() {
		w, err := scanWatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) exec1(uid, id int64, q string, args ...any) error {
	args = append(args, id, uid)
	res, err := s.db.Exec(q+` WHERE id=? AND user_id=?`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetTarget changes the target and resets the alert baseline so the next
// check can raise a "below target" alert against the new target.
func (s *Store) SetTarget(uid, id int64, target *int64) error {
	return s.exec1(uid, id, `UPDATE watches SET target_pence=?, alert_base_pence=NULL`, target)
}

func (s *Store) SetInterval(uid, id int64, mins int) error {
	return s.exec1(uid, id, `UPDATE watches SET interval_min=?`, mins)
}

func (s *Store) SetPaused(uid, id int64, paused bool) error {
	if paused {
		return s.exec1(uid, id, `UPDATE watches SET paused=1`)
	}
	return s.exec1(uid, id, `UPDATE watches SET paused=0, failures=0, failure_notified=0,
		status=CASE WHEN status IN ('blocked','error') THEN 'pending' ELSE status END, next_check_at=?`, s.now())
}

func (s *Store) DeleteWatch(uid, id int64) error {
	return s.exec1(uid, id, `DELETE FROM watches`)
}

// ---- results ----

// RecordSuccess stores a successful check, trims history and resets failures.
func (s *Store) RecordSuccess(id int64, price int64, cur, method string, next int64, base *int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now()
	if _, err := tx.Exec(`UPDATE watches SET status='ok', last_price_pence=?, currency=?, last_method=?, last_error='',
		last_checked_at=?, next_check_at=?, failures=0, failure_notified=0, alert_base_pence=? WHERE id=?`,
		price, cur, method, now, next, base, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO price_history(watch_id, price_pence, currency, method, checked_at) VALUES(?,?,?,?,?)`,
		id, price, cur, method, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM price_history WHERE watch_id=? AND id NOT IN
		(SELECT id FROM price_history WHERE watch_id=? ORDER BY id DESC LIMIT ?)`, id, id, HistoryCap); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordFailure stores a failed check. status is one of error, blocked, stopped.
func (s *Store) RecordFailure(id int64, status, reason string, failures int, next int64) error {
	_, err := s.db.Exec(`UPDATE watches SET status=?, last_error=?, failures=?, last_checked_at=?, next_check_at=? WHERE id=?`,
		status, reason, failures, s.now(), next, id)
	return err
}

func (s *Store) Defer(id int64, next int64) error {
	_, err := s.db.Exec(`UPDATE watches SET next_check_at=? WHERE id=?`, next, id)
	return err
}

func (s *Store) MarkFailureNotified(id int64) error {
	_, err := s.db.Exec(`UPDATE watches SET failure_notified=1 WHERE id=?`, id)
	return err
}

type HistoryRow struct {
	PricePence int64
	Currency   string
	Method     string
	CheckedAt  int64
}

func (s *Store) History(watchID int64, limit int) ([]HistoryRow, error) {
	rows, err := s.db.Query(`SELECT price_pence,currency,method,checked_at FROM price_history WHERE watch_id=? ORDER BY id DESC LIMIT ?`, watchID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HistoryRow
	for rows.Next() {
		var h HistoryRow
		if err := rows.Scan(&h.PricePence, &h.Currency, &h.Method, &h.CheckedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) RecordAlert(watchID int64, kind string, price int64) error {
	_, err := s.db.Exec(`INSERT INTO alerts_sent(watch_id,kind,price_pence,sent_at) VALUES(?,?,?,?)`, watchID, kind, price, s.now())
	return err
}

// LastAlertPrice returns the price of the most recent alert of kind, if any.
func (s *Store) LastAlertPrice(watchID int64, kind string) (*int64, error) {
	var p int64
	err := s.db.QueryRow(`SELECT price_pence FROM alerts_sent WHERE watch_id=? AND kind=? ORDER BY id DESC LIMIT 1`, watchID, kind).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}
