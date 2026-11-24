package app

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
}

// OpenDB opens (creating if needed) the SQLite database in dir and migrates it.
func OpenDB(dir string) (*sql.DB, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.Join(dir, "webwatcher.db") +
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

const backupKeep = 7

// BackupDB writes a consistent snapshot to dir/backups/webwatcher-YYYYMMDD.db
// (replacing any earlier file for the same day) and prunes to the newest 7.
func BackupDB(db *sql.DB, dir string, now time.Time) (string, error) {
	bdir := filepath.Join(dir, "backups")
	if err := os.MkdirAll(bdir, 0o750); err != nil {
		return "", err
	}
	name := filepath.Join(bdir, "webwatcher-"+now.UTC().Format("20060102")+".db")
	tmp := name + ".tmp"
	os.Remove(tmp)
	// The path is generated here, never user input; VACUUM INTO cannot bind it.
	if _, err := db.Exec(`VACUUM INTO ?`, tmp); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, name); err != nil {
		return "", err
	}
	return name, pruneBackups(bdir)
}

func pruneBackups(bdir string) error {
	ents, err := os.ReadDir(bdir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, "webwatcher-") && strings.HasSuffix(n, ".db") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for len(names) > backupKeep {
		if err := os.Remove(filepath.Join(bdir, names[0])); err != nil {
			return err
		}
		names = names[1:]
	}
	return nil
}
