package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrInvalidToken = errors.New("invalid or expired token")
	ErrWatchCap     = errors.New("watch limit reached")
	ErrNotFound     = errors.New("not found")
	ErrNoConsent    = errors.New("no recorded consent")
	ErrStale        = errors.New("watch changed, paused or deleted since the check began")
)

const (
	MaxWatchesPerUser = 30
	HistoryCap        = 500
	MaxFailures       = 10
)

// Store wraps the database. All SQL is parameterised.
type Store struct {
	db        *sql.DB
	clock     Clock
	dashboard bool // WW_MESSENGER=dashboard: alerts need no WhatsApp consent
}

// UseDashboardAlerts selects the dashboard messenger's rules: alerts are
// in-app notifications, so they need no WhatsApp consent. Call it before the
// store is shared between goroutines.
func (s *Store) UseDashboardAlerts(on bool) { s.dashboard = on }

// allowedSQL is the SQL condition (over users u) under which a user may be
// sent alerts. WhatsApp needs a recorded opt-in; the dashboard does not.
func (s *Store) allowedSQL() string {
	if s.dashboard {
		return `(u.alerts_enabled=1)`
	}
	return `(u.alerts_enabled=1 AND u.consent_at IS NOT NULL)`
}

func NewStore(db *sql.DB, clock Clock) *Store { return &Store{db: db, clock: clock} }

func (s *Store) now() int64 { return s.clock.Now().Unix() }

type Session struct {
	UserID        int64
	Phone         string
	CSRF          string
	AlertsEnabled bool
	ConsentAt     *int64
	IsAdmin       bool
}

// CreateLoginToken stores only the hash of a single-use login token.
// When consent is true the explicit WhatsApp opt-in tick is recorded with the
// current time and copied to the user when the token is consumed.
func (s *Store) CreateLoginToken(phone, hash string, ttl time.Duration, consent bool) error {
	source := ""
	if consent {
		source = "login-form"
	}
	return s.createLoginToken(phone, hash, ttl, source, source != "")
}

// ConsentAdminCreated is the consent source recorded for users added with the
// local `webwatcher adduser` command.
const ConsentAdminCreated = "admin-created"

// CreateAdminLoginToken is CreateLoginToken for the operator CLI: consent is
// recorded as admin-created.
func (s *Store) CreateAdminLoginToken(phone, hash string, ttl time.Duration) error {
	return s.createLoginToken(phone, hash, ttl, ConsentAdminCreated, true)
}

// ConsentDashboard is the consent source recorded for users created while the
// dashboard messenger is in use. It records no WhatsApp consent (consent_at
// stays empty), so switching to WhatsApp later sends such a user nothing
// until they have ticked the opt-in.
const ConsentDashboard = "dashboard"

// CreateDashboardLoginToken is CreateLoginToken for the operator CLI under the
// dashboard messenger.
func (s *Store) CreateDashboardLoginToken(phone, hash string, ttl time.Duration) error {
	return s.createLoginToken(phone, hash, ttl, ConsentDashboard, false)
}

// UserExists reports whether a user with this number exists.
func (s *Store) UserExists(phone string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM users WHERE phone=?`, phone).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) createLoginToken(phone, hash string, ttl time.Duration, source string, withConsent bool) error {
	now := s.clock.Now()
	var c *int64
	var src *string
	if source != "" {
		src = &source
		if withConsent {
			n := now.Unix()
			c = &n
		}
	}
	_, err := s.db.Exec(`INSERT INTO login_tokens(phone, token_hash, created_at, expires_at, consent_at, consent_source) VALUES(?,?,?,?,?,?)`,
		phone, hash, now.Unix(), now.Add(ttl).Unix(), c, src)
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
	var source *string
	err = tx.QueryRow(`UPDATE login_tokens SET used_at=? WHERE token_hash=? AND used_at IS NULL AND expires_at>? RETURNING phone, consent_at, consent_source`,
		now, hash, now).Scan(&phone, &consent, &source)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrInvalidToken
	}
	if err != nil {
		return 0, "", err
	}
	var uid int64
	err = tx.QueryRow(`INSERT INTO users(phone, created_at, last_login_at, consent_at, consent_source) VALUES(?,?,?,?,?)
		ON CONFLICT(phone) DO UPDATE SET last_login_at=excluded.last_login_at,
			consent_source=CASE WHEN users.consent_at IS NULL THEN excluded.consent_source ELSE users.consent_source END,
			consent_at=COALESCE(users.consent_at, excluded.consent_at) RETURNING id`,
		phone, now, now, consent, source).Scan(&uid)
	if err != nil {
		return 0, "", err
	}
	var disabled *int64
	if err := tx.QueryRow(`SELECT disabled_at FROM users WHERE id=?`, uid).Scan(&disabled); err != nil {
		return 0, "", err
	}
	if disabled != nil {
		return 0, "", ErrInvalidToken
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
	var ae, adm int
	err := s.db.QueryRow(`SELECT s.user_id, u.phone, s.csrf, u.alerts_enabled, u.consent_at, u.is_admin FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.token_hash=? AND s.expires_at>? AND u.disabled_at IS NULL`, hash, s.now()).Scan(&x.UserID, &x.Phone, &x.CSRF, &ae, &x.ConsentAt, &adm)
	x.AlertsEnabled, x.IsAdmin = ae != 0, adm != 0
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
// watches, history, alerts, notifications) plus any outstanding login tokens for the number.
func (s *Store) DeleteUser(uid int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM login_tokens WHERE phone=(SELECT phone FROM users WHERE id=?)`, uid); err != nil {
		return err
	}
	for _, q := range []string{`DELETE FROM outbox WHERE user_id=?`, `DELETE FROM notifications WHERE user_id=?`, `DELETE FROM sessions WHERE user_id=?`, `DELETE FROM personal_links WHERE user_id=?`} {
		if _, err := tx.Exec(q, uid); err != nil {
			return err
		}
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
	if _, err = tx.Exec(`UPDATE users SET alerts_enabled=? WHERE id=?`, v, uid); err != nil {
		return err
	}
	if !on {
		_, err = tx.Exec(`DELETE FROM outbox WHERE user_id=? AND status='queued'`, uid)
	}
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

// Purge applies the retention rules: price history older than historyDays, and
// check logs, finished outbox rows and old send records older than diagDays.
// Notifications follow the history retention.
// The newest alert record per watch and kind is kept, as it drives de-duplication.
func (s *Store) Purge(historyDays, diagDays int) error {
	now := s.now()
	h, d := now-int64(historyDays)*86400, now-int64(diagDays)*86400
	for _, q := range []struct {
		sql string
		arg int64
	}{
		{`DELETE FROM price_history WHERE checked_at<?`, h},
		{`DELETE FROM alerts_sent WHERE sent_at<? AND id NOT IN (SELECT MAX(id) FROM alerts_sent GROUP BY watch_id, kind)`, h},
		{`DELETE FROM check_log WHERE checked_at<?`, d},
		{`DELETE FROM outbox WHERE status<>'queued' AND COALESCE(done_at, created_at)<?`, d},
		{`DELETE FROM send_log WHERE at<?`, now - 7200},
		{`DELETE FROM notifications WHERE created_at<?`, h},
	} {
		if _, err := s.db.Exec(q.sql, q.arg); err != nil {
			return err
		}
	}
	return nil
}

// DeleteUserSessions revokes every session of the user ("log out everywhere").
func (s *Store) DeleteUserSessions(uid int64) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE user_id=?`, uid)
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
	Basis          string // per_person, total or unknown: what LastPricePence is a price of
	BasisChanged   bool   // a later observation had a different currency or basis; awaiting confirmation
	ObsPence       *int64 // that observation
	ObsCurrency    string
	ObsBasis       string
	SoldOut        bool
	ETag           string
	LastModified   string
	Version        int64 // bumped on target, pause and basis edits; guards queued alerts against stale snapshots
}

const watchColsTpl = `w.id,w.user_id,w.url,w.label,w.selector,w.target_pence,w.interval_min,w.paused,w.status,
	w.last_price_pence,w.alert_base_pence,w.currency,w.last_method,w.last_error,w.last_checked_at,
	w.next_check_at,w.failures,w.failure_notified,w.created_at,u.phone,{ALLOWED},
	w.basis,w.basis_changed,w.obs_price_pence,w.obs_currency,w.obs_basis,w.sold_out,w.etag,w.last_modified,w.version`

func (s *Store) watchCols() string {
	return strings.Replace(watchColsTpl, "{ALLOWED}", s.allowedSQL(), 1)
}

type rowScanner interface{ Scan(...any) error }

func scanWatch(r rowScanner) (*Watch, error) {
	var w Watch
	var paused, fn, ao, bc, so int
	err := r.Scan(&w.ID, &w.UserID, &w.URL, &w.Label, &w.Selector, &w.TargetPence, &w.IntervalMin, &paused, &w.Status,
		&w.LastPricePence, &w.AlertBase, &w.Currency, &w.LastMethod, &w.LastError, &w.LastCheckedAt,
		&w.NextCheckAt, &w.Failures, &fn, &w.CreatedAt, &w.Phone, &ao,
		&w.Basis, &bc, &w.ObsPence, &w.ObsCurrency, &w.ObsBasis, &so, &w.ETag, &w.LastModified, &w.Version)
	w.Paused, w.FailureNotif, w.AlertsOn, w.BasisChanged, w.SoldOut = paused != 0, fn != 0, ao != 0, bc != 0, so != 0
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
	rows, err := s.db.Query(`SELECT `+s.watchCols()+` FROM watches w JOIN users u ON u.id=w.user_id WHERE w.user_id=? ORDER BY w.id`, uid)
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
	q := `SELECT ` + s.watchCols() + ` FROM watches w JOIN users u ON u.id=w.user_id WHERE w.id=?`
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
	rows, err := s.db.Query(`SELECT `+s.watchCols()+` FROM watches w JOIN users u ON u.id=w.user_id
		WHERE u.disabled_at IS NULL AND w.fetch_mode='server' AND w.paused=0 AND w.failures<? AND w.next_check_at<=? ORDER BY w.next_check_at LIMIT ?`, MaxFailures, s.now(), limit)
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
	return s.exec1(uid, id, `UPDATE watches SET target_pence=?, alert_base_pence=NULL, version=version+1`, target)
}

func (s *Store) SetInterval(uid, id int64, mins int) error {
	return s.exec1(uid, id, `UPDATE watches SET interval_min=?`, mins)
}

func (s *Store) SetPaused(uid, id int64, paused bool) error {
	if paused {
		return s.exec1(uid, id, `UPDATE watches SET paused=1, version=version+1`)
	}
	if err := s.exec1(uid, id, `UPDATE watches SET paused=0, version=version+1, failures=0, failure_notified=0,
		status=CASE WHEN status IN ('blocked','error','noprice','disallowed') THEN 'pending' ELSE status END, next_check_at=?`, s.now()); err != nil {
		return err
	}
	// A fresh run of failures may send a fresh stop notice.
	_, err := s.db.Exec(`DELETE FROM outbox WHERE watch_id=? AND kind=? AND status<>'queued'`, id, AlertFailed)
	return err
}

func (s *Store) DeleteWatch(uid, id int64) error {
	return s.exec1(uid, id, `DELETE FROM watches`)
}

// ---- results ----

// guard re-reads the watch inside the transaction that is about to record a
// check result. It returns ErrStale if the watch is gone, paused or edited
// since the snapshot (ver) was taken, and whether the owner may still receive
// alerts.
func (s *Store) guard(tx *sql.Tx, id int64, ver *int64) (alertsOK bool, err error) {
	var version int64
	var paused, ok int
	err = tx.QueryRow(`SELECT w.version, w.paused, `+s.allowedSQL()+`
		FROM watches w JOIN users u ON u.id=w.user_id WHERE w.id=?`, id).Scan(&version, &paused, &ok)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrStale
	}
	if err != nil {
		return false, err
	}
	if paused != 0 || (ver != nil && *ver != version) {
		return false, ErrStale
	}
	return ok != 0, nil
}

// QueuedAlert is an alert to be written to the outbox in the same transaction
// as the check result that caused it.
type QueuedAlert struct {
	Kind     string
	Price    int64
	Msg      Message
	Old      *int64 // previous price, for the dashboard notification
	Target   *int64
	Currency string
}

// enqueueTx writes an alert to the outbox. The unique event key
// (watch_id:kind:price_pence) makes a repeat of the same event a no-op. Older
// finished alerts of the same kind at other prices are forgotten first, so a
// price that oscillates (99 -> 105 -> 99 -> 105) can alert again.
func enqueueTx(tx *sql.Tx, watchID int64, a *QueuedAlert, now int64) error {
	// Alerts fire on transitions (see EvaluateAlert), so a finished alert of
	// this kind never blocks a new one. The unique key only stops the same
	// event being queued twice at once.
	if _, err := tx.Exec(`DELETE FROM outbox WHERE watch_id=? AND kind=? AND status<>'queued'`, watchID, a.Kind); err != nil {
		return err
	}
	params, _ := json.Marshal(a.Msg.Params)
	key := fmt.Sprintf("%d:%s:%d", watchID, a.Kind, a.Price)
	if _, err := tx.Exec(`INSERT OR IGNORE INTO outbox(user_id,watch_id,kind,price_pence,event_key,text,params,next_attempt_at,created_at,watch_version,old_pence,target_pence,currency)
		SELECT user_id,?,?,?,?,?,?,?,?,version,?,?,? FROM watches WHERE id=?`,
		watchID, a.Kind, a.Price, key, a.Msg.Text, string(params), now, now, a.Old, a.Target, a.Currency, watchID); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO alerts_sent(watch_id,kind,price_pence,sent_at) VALUES(?,?,?,?)`, watchID, a.Kind, a.Price, now)
	return err
}

// RecordSuccess stores a successful check, trims history, resets failures and,
// if alert is non-nil, queues it in the outbox, all in one transaction.
func (s *Store) RecordSuccess(id int64, price int64, cur, method string, next int64, base *int64, alert *QueuedAlert) error {
	return s.RecordObservation(id, Observation{Price: price, Currency: cur, Method: method, Basis: BasisUnknown}, next, base, alert)
}

// Observation is one extracted price with what it is a price of.
type Observation struct {
	Price        int64
	Currency     string
	Method       string
	Basis        string
	ETag         string
	LastModified string
	// Version is the watch version the check started from. If the watch has
	// been edited, paused or deleted since, the result is dropped (ErrStale).
	Version *int64
}

// logCheck appends a diagnostics row. detail never holds URLs or numbers.
func logCheck(tx *sql.Tx, id, now int64, outcome, detail string) error {
	_, err := tx.Exec(`INSERT INTO check_log(watch_id, checked_at, outcome, detail) VALUES(?,?,?,?)`, id, now, outcome, RedactPII(detail))
	return err
}

// RecordObservation is RecordSuccess for a comparable observation.
func (s *Store) RecordObservation(id int64, o Observation, next int64, base *int64, alert *QueuedAlert) error {
	price, cur, method := o.Price, o.Currency, o.Method
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now()
	alertsOK, err := s.guard(tx, id, o.Version)
	if err != nil {
		return err
	}
	if !alertsOK {
		alert = nil
	}
	if _, err := tx.Exec(`UPDATE watches SET status='ok', last_price_pence=?, currency=?, basis=?, last_method=?, last_error='',
		last_checked_at=?, next_check_at=?, failures=0, failure_notified=0, alert_base_pence=?, sold_out=0,
		basis_changed=0, obs_price_pence=NULL, obs_currency='', obs_basis='', etag=?, last_modified=? WHERE id=?`,
		price, cur, o.Basis, method, now, next, base, o.ETag, o.LastModified, id); err != nil {
		return err
	}
	if err := logCheck(tx, id, now, "ok", method); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO price_history(watch_id, price_pence, currency, basis, method, checked_at) VALUES(?,?,?,?,?,?)`,
		id, price, cur, o.Basis, method, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM price_history WHERE watch_id=? AND id NOT IN
		(SELECT id FROM price_history WHERE watch_id=? ORDER BY id DESC LIMIT ?)`, id, id, HistoryCap); err != nil {
		return err
	}
	if alert != nil {
		if err := enqueueTx(tx, id, alert, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RecordBasisChange stores an observation whose currency or basis differs from
// the watch's baseline. The observation goes into the history and is shown on
// the dashboard, but the baseline, last price and alert state are untouched, so
// no change alert can be raised by comparing unlike things.
func (s *Store) RecordBasisChange(id int64, o Observation, next int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now()
	if _, err := s.guard(tx, id, o.Version); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE watches SET status='ok', last_error='', last_method=?, last_checked_at=?, next_check_at=?,
		failures=0, failure_notified=0, sold_out=0, basis_changed=1, obs_price_pence=?, obs_currency=?, obs_basis=?, etag=?, last_modified=? WHERE id=?`,
		o.Method, now, next, o.Price, o.Currency, o.Basis, o.ETag, o.LastModified, id); err != nil {
		return err
	}
	if err := logCheck(tx, id, now, "basis_changed", o.Method); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO price_history(watch_id, price_pence, currency, basis, method, checked_at) VALUES(?,?,?,?,?,?)`,
		id, o.Price, o.Currency, o.Basis, o.Method, now); err != nil {
		return err
	}
	return tx.Commit()
}

// ConfirmBasis accepts the flagged observation as the new baseline. The alert
// baseline is reset, as for a new target, so the next check starts afresh.
func (s *Store) ConfirmBasis(uid, id int64) error {
	res, err := s.db.Exec(`UPDATE watches SET last_price_pence=obs_price_pence, currency=obs_currency, basis=obs_basis,
		alert_base_pence=NULL, basis_changed=0, obs_price_pence=NULL, obs_currency='', obs_basis='', version=version+1
		WHERE id=? AND user_id=? AND basis_changed=1`, id, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordSoldOut notes that the page says the item is sold out. The last valid
// price, baseline and alert state are kept and no price is recorded.
func (s *Store) RecordSoldOut(id int64, next int64, ver *int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now()
	if _, err := s.guard(tx, id, ver); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE watches SET status='ok', sold_out=1, last_error='', last_checked_at=?, next_check_at=?, failures=0, failure_notified=0 WHERE id=?`,
		now, next, id); err != nil {
		return err
	}
	if err := logCheck(tx, id, now, "sold_out", ""); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordNotModified notes a successful conditional GET answered with 304: the
// page is unchanged, so the last valid observation stands.
func (s *Store) RecordNotModified(id int64, next int64, ver *int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now()
	if _, err := s.guard(tx, id, ver); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE watches SET status='ok', last_error='', last_checked_at=?, next_check_at=?, failures=0, failure_notified=0 WHERE id=?`,
		now, next, id); err != nil {
		return err
	}
	if err := logCheck(tx, id, now, "not_modified", ""); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordFailure stores a failed check. status is one of error, blocked, stopped.
// A non-nil notice is queued and the watch marked as notified, atomically.
func (s *Store) RecordFailure(id int64, status, reason string, failures int, next int64, notice *QueuedAlert, ver *int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now()
	alertsOK, err := s.guard(tx, id, ver)
	if err != nil {
		return err
	}
	if !alertsOK {
		notice = nil
	}
	if _, err := tx.Exec(`UPDATE watches SET status=?, last_error=?, failures=?, last_checked_at=?, next_check_at=? WHERE id=?`,
		status, reason, failures, now, next, id); err != nil {
		return err
	}
	if err := logCheck(tx, id, now, status, reason); err != nil {
		return err
	}
	if notice != nil {
		if err := enqueueTx(tx, id, notice, now); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE watches SET failure_notified=1 WHERE id=?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Defer(id int64, next int64) error {
	_, err := s.db.Exec(`UPDATE watches SET next_check_at=? WHERE id=?`, next, id)
	return err
}

type HistoryRow struct {
	PricePence int64
	Currency   string
	Basis      string
	Method     string
	CheckedAt  int64
}

// History returns the newest observations of a watch, only if uid owns it.
func (s *Store) History(uid, watchID int64, limit int) ([]HistoryRow, error) {
	rows, err := s.db.Query(`SELECT h.price_pence,h.currency,h.basis,h.method,h.checked_at FROM price_history h
		JOIN watches w ON w.id=h.watch_id WHERE h.watch_id=? AND w.user_id=? ORDER BY h.id DESC LIMIT ?`, watchID, uid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HistoryRow
	for rows.Next() {
		var h HistoryRow
		if err := rows.Scan(&h.PricePence, &h.Currency, &h.Basis, &h.Method, &h.CheckedAt); err != nil {
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

// ---- personal (permanent) login links ----

// PersonalLinkInfo describes a user's permanent link. It never carries the token.
type PersonalLinkInfo struct {
	Exists     bool // a link was created and has not been revoked
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// EnsureUser returns the id of the user with this number, creating the user
// when absent. A new user gets the consent the operator CLI records: admin-created
// consent normally, or the dashboard source (no WhatsApp consent) under the
// dashboard messenger. An existing user is left as it is.
func (s *Store) EnsureUser(phone string, dashboard bool) (int64, error) {
	now := s.now()
	src, consent := ConsentAdminCreated, any(now)
	if dashboard {
		src, consent = ConsentDashboard, nil
	}
	var uid int64
	err := s.db.QueryRow(`INSERT INTO users(phone, created_at, last_login_at, consent_at, consent_source) VALUES(?,?,?,?,?)
		ON CONFLICT(phone) DO UPDATE SET phone=phone RETURNING id`, phone, now, now, consent, src).Scan(&uid)
	return uid, err
}

// UserID looks a user up by number.
func (s *Store) UserID(phone string) (int64, error) {
	var uid int64
	err := s.db.QueryRow(`SELECT id FROM users WHERE phone=?`, phone).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return uid, err
}

// SetPersonalLink creates the user's permanent link or rotates it, replacing
// the previous hash (the old link stops working at once) and clearing any
// revocation. Only the hash is stored.
func (s *Store) SetPersonalLink(uid int64, hash string) error {
	_, err := s.db.Exec(`INSERT INTO personal_links(user_id, token_hash, created_at) VALUES(?,?,?)
		ON CONFLICT(user_id) DO UPDATE SET token_hash=excluded.token_hash, created_at=excluded.created_at,
			last_used_at=NULL, revoked_at=NULL`, uid, hash, s.now())
	return err
}

// RevokePersonalLink revokes the user's permanent link. It reports whether an
// active link existed.
func (s *Store) RevokePersonalLink(uid int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE personal_links SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, s.now(), uid)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// PersonalLinkStatus reports the user's link state (never the token).
func (s *Store) PersonalLinkStatus(uid int64) (PersonalLinkInfo, error) {
	var created int64
	var used, revoked *int64
	err := s.db.QueryRow(`SELECT created_at, last_used_at, revoked_at FROM personal_links WHERE user_id=?`, uid).Scan(&created, &used, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return PersonalLinkInfo{}, nil
	}
	if err != nil {
		return PersonalLinkInfo{}, err
	}
	info := PersonalLinkInfo{Exists: revoked == nil, CreatedAt: time.Unix(created, 0)}
	if used != nil {
		t := time.Unix(*used, 0)
		info.LastUsedAt = &t
	}
	if revoked != nil {
		t := time.Unix(*revoked, 0)
		info.RevokedAt = &t
	}
	return info, nil
}

// UsePersonalLink looks a link up by the hash of its token. When it is valid
// and not revoked it records the use and returns the user id; otherwise
// ErrInvalidToken. The token is not consumed. The stored hash is compared in
// constant time as well as by index.
func (s *Store) UsePersonalLink(hash string) (int64, error) {
	var uid int64
	var stored string
	err := s.db.QueryRow(`SELECT p.user_id, p.token_hash FROM personal_links p JOIN users u ON u.id=p.user_id WHERE p.token_hash=? AND p.revoked_at IS NULL AND u.disabled_at IS NULL`, hash).Scan(&uid, &stored)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !ctEqual(stored, hash)) {
		return 0, ErrInvalidToken
	}
	if err != nil {
		return 0, err
	}
	if _, err := s.db.Exec(`UPDATE personal_links SET last_used_at=? WHERE user_id=?`, s.now(), uid); err != nil {
		return 0, err
	}
	return uid, nil
}

// IsAdmin reports whether the user is an admin.
func (s *Store) IsAdmin(uid int64) bool {
	var a int
	return s.db.QueryRow(`SELECT is_admin FROM users WHERE id=?`, uid).Scan(&a) == nil && a != 0
}
