package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalisePhone(t *testing.T) {
	ok := map[string]string{
		"07700 900123":           "+447700900123",
		"+44 7700 900123":        "+447700900123",
		"+44 (0)7700 900123":     "+447700900123",
		"447700900123":           "+447700900123",
		"0044 7700-900-123":      "+447700900123",
		"7700900123":             "+447700900123",
		"whatsapp:+447700900123": "+447700900123",
		"+1 415 555 2671":        "+14155552671",
		"+353 87 123 4567":       "+353871234567",
	}
	for in, want := range ok {
		got, err := NormalisePhone(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "abc", "0770090012x", "+", "123", "+0123456789", "07700 900123 4567 890 123", "+44 12", "<script>", "+44 7700 900123; DROP", "++447700900123", "+4477009001234567"} {
		if got, err := NormalisePhone(in); err == nil {
			t.Errorf("%q should be rejected, got %q", in, got)
		}
	}
}

func TestTokenSingleUseExpiryAndHashAtRest(t *testing.T) {
	s, clk := testStore(t)
	msgr := &fakeMessenger{}
	a := NewAuth(s, msgr, clk, "https://ww.example")
	a.RequestLogin(context.Background(), "+447700900123", "1.2.3.4", true)
	sent := msgr.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d", len(sent))
	}
	link := sent[0].Params[0]
	if !strings.HasPrefix(link, "https://ww.example/auth/") {
		t.Fatal(link)
	}
	raw := strings.TrimPrefix(link, "https://ww.example/auth/")

	// Hash at rest: raw token must not appear anywhere in the DB.
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM login_tokens WHERE token_hash=?`, raw).Scan(&n)
	if n != 0 {
		t.Fatal("raw token stored")
	}
	s.db.QueryRow(`SELECT COUNT(*) FROM login_tokens WHERE token_hash=?`, hashToken(raw)).Scan(&n)
	if n != 1 {
		t.Fatal("hash not stored")
	}

	sess, csrf, err := a.Consume(raw)
	if err != nil || sess == "" || csrf == "" {
		t.Fatalf("consume: %v", err)
	}
	if _, _, err := a.Consume(raw); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("second use should fail, got %v", err)
	}
	// session stored hashed
	s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash=?`, sess).Scan(&n)
	if n != 0 {
		t.Fatal("raw session token stored")
	}
	got, err := a.Session(sess)
	if err != nil || got.Phone != "+447700900123" || got.CSRF != csrf {
		t.Fatalf("session: %+v %v", got, err)
	}
	// session expiry
	clk.Advance(SessionTTL + time.Second)
	if _, err := a.Session(sess); err == nil {
		t.Fatal("session should have expired")
	}

	// token expiry
	a.RequestLogin(context.Background(), "+447700900124", "1.2.3.4", true)
	raw2 := strings.TrimPrefix(msgr.Sent()[1].Params[0], "https://ww.example/auth/")
	clk.Advance(LoginTokenTTL + time.Second)
	if _, _, err := a.Consume(raw2); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired token accepted: %v", err)
	}
	if _, _, err := a.Consume("garbage"); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("garbage accepted")
	}
}

func TestReturningUserKeepsRow(t *testing.T) {
	s, _ := testStore(t)
	u1 := mustUser(t, s, "+447700900123")
	u2 := mustUser(t, s, "+447700900123")
	if u1 != u2 {
		t.Fatalf("user row changed: %d vs %d", u1, u2)
	}
}

func TestLoginRateLimits(t *testing.T) {
	s, clk := testStore(t)
	msgr := &fakeMessenger{}
	a := NewAuth(s, msgr, clk, "https://x")
	for i := 0; i < 10; i++ {
		a.RequestLogin(context.Background(), "+447700900123", "9.9.9.9", true)
	}
	if n := len(msgr.Sent()); n != 5 {
		t.Fatalf("per-number limit: sent %d, want 5", n)
	}
	for i := 0; i < 30; i++ {
		a.RequestLogin(context.Background(), "+4477009002"+string(rune('0'+i%10))+"0", "8.8.8.8", true)
	}
	// per-IP limit is 20 per hour across numbers
	if n := len(msgr.Sent()) - 5; n > 20 {
		t.Fatalf("per-IP limit exceeded: %d", n)
	}
	clk.Advance(time.Hour + time.Second)
	before := len(msgr.Sent())
	a.RequestLogin(context.Background(), "+447700900123", "9.9.9.9", true)
	if len(msgr.Sent()) != before+1 {
		t.Fatal("limit should reset after window")
	}
}

func TestDeleteUserErasesEverything(t *testing.T) {
	s, _ := testStore(t)
	uid := mustUser(t, s, "+447700900123")
	other := mustUser(t, s, "+447700900999")
	id, err := s.AddWatch(Watch{UserID: uid, URL: "https://a.example/x", IntervalMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	otherW, _ := s.AddWatch(Watch{UserID: other, URL: "https://b.example/x", IntervalMin: 60})
	s.RecordSuccess(id, 1000, "GBP", "jsonld", 0, nil, nil)
	s.RecordAlert(id, "changed", 1000)
	_, h := newToken()
	s.CreateSession(uid, h, "c", time.Hour)
	_, h2 := newToken()
	s.CreateLoginToken("+447700900123", h2, time.Hour, true)

	if err := s.DeleteUser(uid); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"sessions", "watches", "price_history", "alerts_sent"} {
		var n int
		s.db.QueryRow(`SELECT COUNT(*) FROM `+tbl+` WHERE `+map[string]string{
			"sessions": "user_id", "watches": "user_id", "price_history": "watch_id", "alerts_sent": "watch_id"}[tbl]+` IN (?,?)`, uid, id).Scan(&n)
		if n != 0 {
			t.Errorf("%s still has rows", tbl)
		}
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE id=?`, uid).Scan(&n)
	if n != 0 {
		t.Error("user remains")
	}
	var lt int
	s.db.QueryRow(`SELECT COUNT(*) FROM login_tokens WHERE phone='+447700900123'`).Scan(&lt)
	if lt != 0 {
		t.Error("login tokens remain")
	}
	if _, err := s.WatchByID(otherW, other); err != nil {
		t.Errorf("other user's data affected: %v", err)
	}
}

func TestWatchCap(t *testing.T) {
	s, _ := testStore(t)
	uid := mustUser(t, s, "+447700900123")
	for i := 0; i < MaxWatchesPerUser; i++ {
		if _, err := s.AddWatch(Watch{UserID: uid, URL: "https://a.example/x", IntervalMin: 60}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddWatch(Watch{UserID: uid, URL: "https://a.example/x", IntervalMin: 60}); !errors.Is(err, ErrWatchCap) {
		t.Fatalf("want cap error, got %v", err)
	}
}

func TestHistoryCap(t *testing.T) {
	s, _ := testStore(t)
	uid := mustUser(t, s, "+447700900123")
	id, _ := s.AddWatch(Watch{UserID: uid, URL: "https://a.example/x", IntervalMin: 60})
	for i := 0; i < HistoryCap+25; i++ {
		if err := s.RecordSuccess(id, int64(100+i), "GBP", "regex", 0, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	h, _ := s.History(id, 10000)
	if len(h) != HistoryCap {
		t.Fatalf("history %d want %d", len(h), HistoryCap)
	}
	if h[0].PricePence != int64(100+HistoryCap+24) {
		t.Fatal("newest row missing")
	}
}

func TestBackupAndPrune(t *testing.T) {
	s, clk := testStore(t)
	dir := t.TempDir()
	for i := 0; i < 10; i++ {
		if _, err := BackupDB(s.db, dir, clk.Now().AddDate(0, 0, i)); err != nil {
			t.Fatal(err)
		}
	}
	ents, _ := os.ReadDir(filepath.Join(dir, "backups"))
	if len(ents) != 7 {
		t.Fatalf("kept %d backups", len(ents))
	}
	if ents[0].Name() != "webwatcher-20261127.db" {
		t.Fatalf("oldest kept: %s", ents[0].Name())
	}
	// backup is a valid DB
	fi, _ := os.Stat(filepath.Join(dir, "backups", ents[6].Name()))
	if fi.Size() == 0 {
		t.Fatal("empty backup")
	}
}

func TestConfig(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, err := LoadConfig(env(map[string]string{}))
	if err != nil || c.Addr != "127.0.0.1:8780" || c.Messenger != "log" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := LoadConfig(env(map[string]string{"WW_MESSENGER": "meta"})); err == nil {
		t.Fatal("meta without creds accepted")
	}
	if _, err := LoadConfig(env(map[string]string{"WW_MESSENGER": "pigeon"})); err == nil {
		t.Fatal("unknown messenger accepted")
	}
	c, _ = LoadConfig(env(map[string]string{"WW_BASE_URL": "https://ww.example/"}))
	if c.BaseURL != "https://ww.example" || !c.SecureCookies() {
		t.Fatal("base url")
	}
}
