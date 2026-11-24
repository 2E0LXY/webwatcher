package app

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"time"
)

// SystemClock returns the wall clock.
func SystemClock() Clock { return realClock{} }

// MaintenanceConfig holds what RunMaintenance needs.
type MaintenanceConfig struct {
	Dir               string
	BackupKey         []byte
	RetainHistoryDays int
	RetainDiagDays    int
}

type maintenance struct {
	db        *sql.DB
	store     *Store
	clock     Clock
	cfg       MaintenanceConfig
	lastPurge time.Time
}

// tick clears expired rows, purges data past its retention (at most daily) and
// takes the daily backup if today's is missing.
func (m *maintenance) tick() {
	if err := m.store.Cleanup(); err != nil {
		slog.Error("cleanup", "err", RedactPII(err.Error()))
	}
	now := m.clock.Now()
	if m.lastPurge.IsZero() || now.Sub(m.lastPurge) >= 24*time.Hour {
		if err := m.store.Purge(m.cfg.RetainHistoryDays, m.cfg.RetainDiagDays); err != nil {
			slog.Error("retention purge", "err", RedactPII(err.Error()))
		} else {
			m.lastPurge = now
		}
	}
	if !BackupExists(m.cfg.Dir, now) {
		if f, err := BackupDBKey(m.db, m.cfg.Dir, now, m.cfg.BackupKey); err != nil {
			slog.Error("backup", "err", RedactPII(err.Error()))
		} else {
			slog.Info("backup written", "file", filepath.Base(f), "encrypted", m.cfg.BackupKey != nil)
		}
	}
}

// RunMaintenanceOnce performs one maintenance pass (used by tests).
func RunMaintenanceOnce(db *sql.DB, store *Store, clock Clock, cfg MaintenanceConfig) {
	(&maintenance{db: db, store: store, clock: clock, cfg: cfg}).tick()
}

// RunMaintenance runs a pass now and then hourly until ctx is cancelled.
func RunMaintenance(ctx context.Context, db *sql.DB, store *Store, clock Clock, cfg MaintenanceConfig) {
	m := &maintenance{db: db, store: store, clock: clock, cfg: cfg}
	m.tick()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.tick()
		}
	}
}
