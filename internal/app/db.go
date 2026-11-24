package app

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var migrations = []string{
	`CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		phone TEXT NOT NULL UNIQUE,
		created_at INTEGER NOT NULL,
		last_login_at INTEGER NOT NULL
	);
	CREATE TABLE sessions (
		id INTEGER PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		token_hash TEXT NOT NULL UNIQUE,
		csrf TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL
	);
	CREATE TABLE login_tokens (
		id INTEGER PRIMARY KEY,
		phone TEXT NOT NULL,
		token_hash TEXT NOT NULL UNIQUE,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		used_at INTEGER
	);
	CREATE INDEX login_tokens_phone ON login_tokens(phone);
	CREATE TABLE watches (
		id INTEGER PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		url TEXT NOT NULL,
		label TEXT NOT NULL DEFAULT '',
		selector TEXT NOT NULL DEFAULT '',
		target_pence INTEGER,
		interval_min INTEGER NOT NULL DEFAULT 60,
		paused INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'pending',
		last_price_pence INTEGER,
		alert_base_pence INTEGER,
		currency TEXT NOT NULL DEFAULT 'GBP',
		last_method TEXT NOT NULL DEFAULT '',
		last_error TEXT NOT NULL DEFAULT '',
		last_checked_at INTEGER,
		next_check_at INTEGER NOT NULL,
		failures INTEGER NOT NULL DEFAULT 0,
		failure_notified INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);
	CREATE INDEX watches_user ON watches(user_id);
	CREATE INDEX watches_due ON watches(paused, next_check_at);
	CREATE TABLE price_history (
		id INTEGER PRIMARY KEY,
		watch_id INTEGER NOT NULL REFERENCES watches(id) ON DELETE CASCADE,
		price_pence INTEGER NOT NULL,
		currency TEXT NOT NULL,
		method TEXT NOT NULL,
		checked_at INTEGER NOT NULL
	);
	CREATE INDEX price_history_watch ON price_history(watch_id, id);
	CREATE TABLE alerts_sent (
		id INTEGER PRIMARY KEY,
		watch_id INTEGER NOT NULL REFERENCES watches(id) ON DELETE CASCADE,
		kind TEXT NOT NULL,
		price_pence INTEGER NOT NULL,
		sent_at INTEGER NOT NULL
	);
	CREATE INDEX alerts_sent_watch ON alerts_sent(watch_id, kind, id);`,
	`ALTER TABLE users ADD COLUMN alerts_enabled INTEGER NOT NULL DEFAULT 1;
	ALTER TABLE users ADD COLUMN consent_at INTEGER;
	ALTER TABLE login_tokens ADD COLUMN consent_at INTEGER;
	CREATE TABLE webhook_seen (
		provider TEXT NOT NULL,
		msg_id TEXT NOT NULL,
		seen_at INTEGER NOT NULL,
		PRIMARY KEY (provider, msg_id)
	);`,
	`CREATE TABLE outbox (
		id INTEGER PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		watch_id INTEGER NOT NULL REFERENCES watches(id) ON DELETE CASCADE,
		kind TEXT NOT NULL,
		price_pence INTEGER NOT NULL,
		event_key TEXT NOT NULL UNIQUE,
		text TEXT NOT NULL,
		params TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'queued',
		attempts INTEGER NOT NULL DEFAULT 0,
		next_attempt_at INTEGER NOT NULL,
		last_error TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		done_at INTEGER
	);
	CREATE INDEX outbox_due ON outbox(status, next_attempt_at);
	CREATE INDEX outbox_user ON outbox(user_id);
	CREATE TABLE send_log (at INTEGER NOT NULL);
	CREATE INDEX send_log_at ON send_log(at);`,
	`ALTER TABLE watches ADD COLUMN basis TEXT NOT NULL DEFAULT 'unknown';
	ALTER TABLE watches ADD COLUMN basis_changed INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE watches ADD COLUMN obs_price_pence INTEGER;
	ALTER TABLE watches ADD COLUMN obs_currency TEXT NOT NULL DEFAULT '';
	ALTER TABLE watches ADD COLUMN obs_basis TEXT NOT NULL DEFAULT '';
	ALTER TABLE price_history ADD COLUMN basis TEXT NOT NULL DEFAULT 'unknown';
	ALTER TABLE watches ADD COLUMN sold_out INTEGER NOT NULL DEFAULT 0;`,
	`ALTER TABLE watches ADD COLUMN etag TEXT NOT NULL DEFAULT '';
	ALTER TABLE watches ADD COLUMN last_modified TEXT NOT NULL DEFAULT '';
	CREATE TABLE check_log (
		id INTEGER PRIMARY KEY,
		watch_id INTEGER NOT NULL REFERENCES watches(id) ON DELETE CASCADE,
		checked_at INTEGER NOT NULL,
		outcome TEXT NOT NULL,
		detail TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX check_log_at ON check_log(checked_at);
	CREATE INDEX check_log_watch ON check_log(watch_id, id);`,
	`ALTER TABLE watches ADD COLUMN version INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE outbox ADD COLUMN watch_version INTEGER NOT NULL DEFAULT 0;`,
	`ALTER TABLE send_log ADD COLUMN kind TEXT NOT NULL DEFAULT 'alert';`,
	`ALTER TABLE users ADD COLUMN consent_source TEXT;
	ALTER TABLE login_tokens ADD COLUMN consent_source TEXT;`,
	`ALTER TABLE outbox ADD COLUMN old_pence INTEGER;
	ALTER TABLE outbox ADD COLUMN target_pence INTEGER;
	ALTER TABLE outbox ADD COLUMN currency TEXT NOT NULL DEFAULT '';
	CREATE TABLE notifications (
		id INTEGER PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		watch_id INTEGER REFERENCES watches(id) ON DELETE SET NULL,
		kind TEXT NOT NULL CHECK (kind IN ('changed','below_target','system')),
		title TEXT NOT NULL,
		body TEXT NOT NULL,
		price_pence INTEGER,
		old_pence INTEGER,
		target_pence INTEGER,
		currency TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		read_at INTEGER
	);
	CREATE INDEX notifications_user ON notifications(user_id, id);`,
	`CREATE TABLE personal_links (
		id INTEGER PRIMARY KEY,
		user_id INTEGER NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
		token_hash TEXT NOT NULL UNIQUE,
		created_at INTEGER NOT NULL,
		last_used_at INTEGER,
		revoked_at INTEGER
	);`,
	`ALTER TABLE users ADD COLUMN is_admin INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE users ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
	ALTER TABLE users ADD COLUMN disabled_at INTEGER;
	CREATE TABLE admin_events (
		id INTEGER PRIMARY KEY,
		at INTEGER NOT NULL,
		actor_id INTEGER NOT NULL,
		action TEXT NOT NULL,
		target_id INTEGER,
		target_label TEXT NOT NULL DEFAULT ''
	);`,
	`ALTER TABLE watches ADD COLUMN target_currency TEXT NOT NULL DEFAULT 'GBP';
	ALTER TABLE watches ADD COLUMN currency_mismatch INTEGER NOT NULL DEFAULT 0;`,
	`ALTER TABLE check_log ADD COLUMN class TEXT NOT NULL DEFAULT '';
	ALTER TABLE check_log ADD COLUMN dns_ms INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE check_log ADD COLUMN connect_ms INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE check_log ADD COLUMN tls_ms INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE check_log ADD COLUMN ttfb_ms INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE check_log ADD COLUMN total_ms INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE check_log ADD COLUMN retry INTEGER NOT NULL DEFAULT 0;`,
}

// OpenDB opens (creating if needed) the SQLite database in dir and migrates it.
// The directory is 0700 and the database file 0600.
func OpenDB(dir string) (*sql.DB, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "webwatcher.db")
	// Create the file ourselves so it never exists with wider permissions.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection keeps SQLite semantics simple; the workload is tiny.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		os.Chmod(path+suffix, 0o600) // best effort: SQLite normally inherits 0600 already
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// BackupRetention is how long backups are kept. Older ones are deleted.
const BackupRetention = 7 * 24 * time.Hour

const backupMagic = "WWBK1"

// BackupDB writes an unencrypted snapshot (mode 0600). See BackupDBKey.
func BackupDB(db *sql.DB, dir string, now time.Time) (string, error) {
	return BackupDBKey(db, dir, now, nil)
}

// BackupDBKey writes a consistent snapshot to dir/backups/webwatcher-YYYYMMDD.db
// (replacing any earlier file for the same day). With a 32-byte key the file is
// AES-256-GCM encrypted and named .db.enc. The backups directory is 0700, files
// are 0600, and backups older than seven days are deleted.
func BackupDBKey(db *sql.DB, dir string, now time.Time, key []byte) (string, error) {
	bdir := filepath.Join(dir, "backups")
	if err := os.MkdirAll(bdir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(bdir, 0o700); err != nil {
		return "", err
	}
	base := filepath.Join(bdir, "webwatcher-"+now.UTC().Format("20060102")+".db")
	tmp := base + ".tmp"
	os.Remove(tmp)
	// The path is generated here, never user input; VACUUM INTO cannot bind it.
	if _, err := db.Exec(`VACUUM INTO ?`, tmp); err != nil {
		return "", err
	}
	defer os.Remove(tmp) // never leave a plaintext copy behind when encrypting
	if err := os.Chmod(tmp, 0o600); err != nil {
		return "", err
	}
	name := base
	if key != nil {
		plain, err := os.ReadFile(tmp)
		if err != nil {
			return "", err
		}
		enc, err := EncryptBackup(key, plain)
		if err != nil {
			return "", err
		}
		name = base + ".enc"
		etmp := name + ".tmp"
		if err := os.WriteFile(etmp, enc, 0o600); err != nil {
			return "", err
		}
		if err := os.Rename(etmp, name); err != nil {
			return "", err
		}
		os.Remove(base) // a plain backup from earlier today, if the key was added since
	} else if err := os.Rename(tmp, name); err != nil {
		return "", err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return "", err
	}
	return name, pruneBackups(bdir, now)
}

// EncryptBackup returns magic || nonce || AES-256-GCM(plaintext).
func EncryptBackup(key, plain []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("backup key must be 32 bytes")
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte(backupMagic), nonce...)
	return gcm.Seal(out, nonce, plain, []byte(backupMagic)), nil
}

// DecryptBackup reverses EncryptBackup. A wrong key or a damaged file fails.
func DecryptBackup(key, data []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("backup key must be 32 bytes")
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	n := len(backupMagic) + gcm.NonceSize()
	if len(data) < n+gcm.Overhead() || string(data[:len(backupMagic)]) != backupMagic {
		return nil, errors.New("not a webwatcher encrypted backup")
	}
	plain, err := gcm.Open(nil, data[len(backupMagic):n], data[n:], []byte(backupMagic))
	if err != nil {
		return nil, errors.New("could not decrypt the backup (wrong key or damaged file)")
	}
	return plain, nil
}

// pruneBackups deletes backups dated seven or more days before now, and any
// stray temporary files.
func pruneBackups(bdir string, now time.Time) error {
	ents, err := os.ReadDir(bdir)
	if err != nil {
		return err
	}
	cutoff := now.UTC().Truncate(24 * time.Hour).Add(-BackupRetention)
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, "webwatcher-") || len(n) < len("webwatcher-20060102") {
			continue
		}
		d, err := time.Parse("20060102", n[len("webwatcher-"):len("webwatcher-20060102")])
		if err != nil {
			continue
		}
		if !d.After(cutoff) {
			if err := os.Remove(filepath.Join(bdir, n)); err != nil {
				return err
			}
		}
	}
	return nil
}

// BackupExists reports whether a backup for the day of now already exists.
func BackupExists(dir string, now time.Time) bool {
	base := filepath.Join(dir, "backups", "webwatcher-"+now.UTC().Format("20060102")+".db")
	for _, p := range []string{base, base + ".enc"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}
