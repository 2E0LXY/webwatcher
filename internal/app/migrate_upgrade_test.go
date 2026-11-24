package app

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// prodMigrations is the number of migrations in fix/review-findings, which is
// what production has applied. Migrations 1..prodMigrations must never change.
const prodMigrations = 14

func TestUpgradeFromReviewFindingsSchema(t *testing.T) {
	if len(migrations) <= prodMigrations {
		t.Fatalf("expected new migrations after %d, have %d", prodMigrations, len(migrations))
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "p.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for i := 0; i < prodMigrations; i++ {
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("prod migration %d: %v", i+1, err)
		}
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, prodMigrations)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users(phone,created_at,last_login_at) VALUES('+447700900123',1,1)`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO watches(user_id,url,interval_min,status,next_check_at,created_at) VALUES(1,'https://a.example/x',60,'ok',1,1)`); err != nil {
		t.Fatalf("seed watch: %v", err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	var v int
	db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != len(migrations) {
		t.Fatalf("user_version %d, want %d", v, len(migrations))
	}
	var mode string
	if err := db.QueryRow(`SELECT fetch_mode FROM watches WHERE id=1`).Scan(&mode); err != nil || mode != "server" {
		t.Fatalf("existing watch fetch_mode = %q, %v", mode, err)
	}
	if _, err := db.Exec(`SELECT id,user_id,token_hash,label,created_at,last_seen_at,revoked_at FROM agent_tokens`); err != nil {
		t.Fatalf("agent_tokens: %v", err)
	}
	// Running again is a no-op.
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
}

func TestAddWatchAgentModeInSameInsert(t *testing.T) {
	st, _ := testStore(t)
	uid := mustUser(t, st, "+447700900123")
	id, err := st.AddWatch(Watch{UserID: uid, URL: "https://a.example/x", IntervalMin: 60, FetchMode: FetchAgent})
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := st.FetchModeOf(id); m != FetchAgent {
		t.Fatalf("mode %q", m)
	}
	if _, err := st.AddWatch(Watch{UserID: uid, URL: "https://a.example/y", IntervalMin: 60, FetchMode: "bogus"}); err == nil {
		t.Fatal("bogus mode accepted")
	}
}

func TestAgentBlockBackoffAtLeastSixHours(t *testing.T) {
	// The scheduler never undercuts Retry-After, and jitter is at most -10%,
	// so even the worst case stays at or above 6 hours.
	if worst := agentBlockBack - agentBlockBack/10; worst < 6*60*60*1e9 {
		t.Fatalf("worst-case back-off %v < 6h", worst)
	}
	if agentBlockBack > maxRetryAfter {
		t.Fatal("agentBlockBack would be clamped")
	}
}
