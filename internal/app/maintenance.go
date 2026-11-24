package app

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// SystemClock returns the wall clock.
func SystemClock() Clock { return realClock{} }

// RunMaintenance takes a daily backup (keeping 7) and clears expired rows.
func RunMaintenance(ctx context.Context, db *sql.DB, store *Store, dir string, clock Clock) {
	tick := func() {
		if err := store.Cleanup(); err != nil {
			slog.Error("cleanup", "err", err)
		}
		today := filepath.Join(dir, "backups", "webwatcher-"+clock.Now().UTC().Format("20060102")+".db")
		if _, err := os.Stat(today); err != nil {
			if _, err := BackupDB(db, dir, clock.Now()); err != nil {
				slog.Error("backup", "err", err)
			} else {
				slog.Info("backup written", "file", today)
			}
		}
	}
	tick()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
