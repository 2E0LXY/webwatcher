package app

import (
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode"
)

var (
	ErrUserExists = errors.New("an account with that number already exists")
	ErrUserCap    = errors.New("the account limit has been reached")
	ErrLastAdmin  = errors.New("that would remove the last enabled admin")
)

// DefaultMaxUsers is the account cap when WW_MAX_USERS is unset.
const DefaultMaxUsers = 25

// CleanDisplayName trims a display name, drops control characters and limits it to 40 runes.
func CleanDisplayName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 40 {
		s = strings.TrimSpace(string(r[:40]))
	}
	return s
}

// SetAdmin marks or unmarks a user as admin. Removing admin from the last
// enabled admin is refused.
func (s *Store) SetAdmin(uid int64, admin bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !admin {
		if err := guardLastAdmin(tx, uid); err != nil {
			return err
		}
	}
	v := 0
	if admin {
		v = 1
	}
	res, err := tx.Exec(`UPDATE users SET is_admin=? WHERE id=?`, v, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// guardLastAdmin returns ErrLastAdmin when uid is an enabled admin and the only one.
func guardLastAdmin(tx *sql.Tx, uid int64) error {
	var isAdmin int
	var disabled *int64
	if err := tx.QueryRow(`SELECT is_admin, disabled_at FROM users WHERE id=?`, uid).Scan(&isAdmin, &disabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if isAdmin == 0 || disabled != nil {
		return nil
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin=1 AND disabled_at IS NULL`).Scan(&n); err != nil {
		return err
	}
	if n <= 1 {
		return ErrLastAdmin
	}
	return nil
}

// CreateUserCapped adds an account for the admin page. It fails with
// ErrUserExists for a known number and ErrUserCap at max accounts.
func (s *Store) CreateUserCapped(phone, name string, dashboard bool, max int) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRow(`SELECT 1 FROM users WHERE phone=?`, phone).Scan(&one); err == nil {
		return 0, ErrUserExists
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, err
	}
	if n >= max {
		return 0, ErrUserCap
	}
	now := s.now()
	src, consent := ConsentAdminCreated, any(now)
	if dashboard {
		src, consent = ConsentDashboard, nil
	}
	var uid int64
	if err := tx.QueryRow(`INSERT INTO users(phone, created_at, last_login_at, consent_at, consent_source, display_name) VALUES(?,?,?,?,?,?) RETURNING id`,
		phone, now, now, consent, src, CleanDisplayName(name)).Scan(&uid); err != nil {
		return 0, err
	}
	return uid, tx.Commit()
}

// SetDisabled disables or enables an account. Disabling ends its sessions;
// it is refused for the last enabled admin.
func (s *Store) SetDisabled(uid int64, disabled bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if disabled {
		if err := guardLastAdmin(tx, uid); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE users SET disabled_at=COALESCE(disabled_at, ?) WHERE id=?`, s.now(), uid); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id=?`, uid); err != nil {
			return err
		}
	} else if _, err := tx.Exec(`UPDATE users SET disabled_at=NULL WHERE id=?`, uid); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteUserGuarded is DeleteUser, refused for the last enabled admin.
func (s *Store) DeleteUserGuarded(uid int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	err = guardLastAdmin(tx, uid)
	tx.Rollback()
	if err != nil {
		return err
	}
	return s.DeleteUser(uid)
}

// AdminUser is one row of the admin accounts page.
type AdminUser struct {
	ID         int64
	Phone      string
	Name       string
	Created    time.Time
	LastLogin  time.Time
	LinkExists bool
	LinkUsed   *time.Time
	Watches    int
	AlertsOn   bool
	Disabled   bool
	IsAdmin    bool
}

func (s *Store) AdminUsers() ([]AdminUser, error) {
	rows, err := s.db.Query(`SELECT u.id, u.phone, u.display_name, u.created_at, u.last_login_at, u.alerts_enabled, u.disabled_at IS NOT NULL, u.is_admin,
		p.user_id IS NOT NULL AND p.revoked_at IS NULL, p.last_used_at, (SELECT COUNT(*) FROM watches w WHERE w.user_id=u.id)
		FROM users u LEFT JOIN personal_links p ON p.user_id=u.id ORDER BY u.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminUser
	for rows.Next() {
		var a AdminUser
		var created, login int64
		var used *int64
		var alerts, dis, adm, link int
		if err := rows.Scan(&a.ID, &a.Phone, &a.Name, &created, &login, &alerts, &dis, &adm, &link, &used, &a.Watches); err != nil {
			return nil, err
		}
		a.Created, a.LastLogin = time.Unix(created, 0), time.Unix(login, 0)
		a.AlertsOn, a.Disabled, a.IsAdmin, a.LinkExists = alerts != 0, dis != 0, adm != 0, link != 0
		if used != nil {
			t := time.Unix(*used, 0)
			a.LinkUsed = &t
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Admin audit actions.
const (
	ActAddAccount    = "add-account"
	ActLinkCreate    = "link-create-rotate"
	ActLinkRevoke    = "link-revoke"
	ActDisable       = "disable"
	ActEnable        = "enable"
	ActDeleteAccount = "delete-account"
)

// LogAdminEvent records who did what to which account. It holds no secrets:
// the label is the masked number at the time.
func (s *Store) LogAdminEvent(actor int64, action string, target int64, label string) error {
	_, err := s.db.Exec(`INSERT INTO admin_events(at, actor_id, action, target_id, target_label) VALUES(?,?,?,?,?)`, s.now(), actor, action, target, label)
	return err
}

type AdminEvent struct {
	At     time.Time
	Actor  string
	Action string
	Target string
}

func (s *Store) AdminEvents(limit int) ([]AdminEvent, error) {
	rows, err := s.db.Query(`SELECT e.at, COALESCE(a.phone,''), e.action, e.target_label FROM admin_events e LEFT JOIN users a ON a.id=e.actor_id ORDER BY e.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminEvent
	for rows.Next() {
		var e AdminEvent
		var at int64
		var actor string
		if err := rows.Scan(&at, &actor, &e.Action, &e.Target); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0)
		e.Actor = "(deleted account)"
		if actor != "" {
			e.Actor = MaskPhone(actor)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// UserPhone returns a user's number.
func (s *Store) UserPhone(uid int64) (string, error) {
	var p string
	err := s.db.QueryRow(`SELECT phone FROM users WHERE id=?`, uid).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return p, err
}
