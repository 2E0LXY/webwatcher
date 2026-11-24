package app

import (
	"database/sql"
	"errors"
)

// MaxNotificationsPerUser is how many notifications a user keeps; older ones
// are dropped as new ones arrive.
const MaxNotificationsPerUser = 200

// Notification kinds.
const (
	NotifChanged = "changed"
	NotifBelow   = "below_target"
	NotifSystem  = "system"
)

// Notification is one in-dashboard alert.
type Notification struct {
	ID        int64
	UserID    int64
	WatchID   *int64 // nil once the watch has been deleted
	Kind      string
	Title     string
	Body      string
	Price     *int64
	Old       *int64
	Target    *int64
	Currency  string
	CreatedAt int64
	ReadAt    *int64
}

func notifKind(alertKind string) string {
	switch alertKind {
	case AlertBelow:
		return NotifBelow
	case AlertChanged:
		return NotifChanged
	}
	return NotifSystem
}

// deliverNotification is delivery for the dashboard messenger: the
// notification row is inserted and the outbox row marked accepted in one
// transaction, so a notification is never duplicated or lost on a crash.
// Title and body come from the alert's template parameters (label, summary),
// never from the page URL, and are redacted like log lines.
func (s *Store) deliverNotification(r outboxRow) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now()
	title, body := "Webwatcher", ""
	if len(r.Params) > 0 {
		title = RedactPII(r.Params[0])
	}
	if len(r.Params) > 1 {
		body = RedactPII(r.Params[1])
	}
	var price *int64
	if r.Kind != AlertFailed {
		p := r.Price
		price = &p
	}
	if _, err := tx.Exec(`INSERT INTO notifications(user_id,watch_id,kind,title,body,price_pence,old_pence,target_pence,currency,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, r.UserID, r.WatchID, notifKind(r.Kind), title, body, price, r.Old, r.Target, r.Currency, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM notifications WHERE user_id=? AND id NOT IN
		(SELECT id FROM notifications WHERE user_id=? ORDER BY id DESC LIMIT ?)`, r.UserID, r.UserID, MaxNotificationsPerUser); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE outbox SET status=?, attempts=?, next_attempt_at=0, last_error='', done_at=? WHERE id=?`,
		OutAccepted, r.Attempts+1, now, r.ID); err != nil {
		return err
	}
	return tx.Commit()
}

const notifCols = `id,user_id,watch_id,kind,title,body,price_pence,old_pence,target_pence,currency,created_at,read_at`

func scanNotification(r rowScanner) (*Notification, error) {
	var n Notification
	err := r.Scan(&n.ID, &n.UserID, &n.WatchID, &n.Kind, &n.Title, &n.Body, &n.Price, &n.Old, &n.Target, &n.Currency, &n.CreatedAt, &n.ReadAt)
	return &n, err
}

// Notifications returns the user's notifications, newest first.
func (s *Store) Notifications(uid int64, limit int) ([]*Notification, error) {
	rows, err := s.db.Query(`SELECT `+notifCols+` FROM notifications WHERE user_id=? ORDER BY id DESC LIMIT ?`, uid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Notification
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// LatestUnread returns the newest unread notification, or ErrNotFound.
func (s *Store) LatestUnread(uid int64) (*Notification, error) {
	n, err := scanNotification(s.db.QueryRow(`SELECT `+notifCols+` FROM notifications WHERE user_id=? AND read_at IS NULL ORDER BY id DESC LIMIT 1`, uid))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

// UnreadCount is the number of unread notifications.
func (s *Store) UnreadCount(uid int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM notifications WHERE user_id=? AND read_at IS NULL`, uid).Scan(&n)
	return n, err
}

// MarkNotificationRead marks one notification read, only if uid owns it
// (ErrNotFound otherwise, whether it is missing or someone else's).
func (s *Store) MarkNotificationRead(uid, id int64) error {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM notifications WHERE id=? AND user_id=?`, id, uid).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE notifications SET read_at=? WHERE id=? AND user_id=? AND read_at IS NULL`, s.now(), id, uid)
	return err
}

// MarkAllNotificationsRead marks every unread notification of the user read.
func (s *Store) MarkAllNotificationsRead(uid int64) error {
	_, err := s.db.Exec(`UPDATE notifications SET read_at=? WHERE user_id=? AND read_at IS NULL`, s.now(), uid)
	return err
}
