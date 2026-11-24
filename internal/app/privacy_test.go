package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSensitiveQueryKeys(t *testing.T) {
	cases := map[string][]string{
		"https://a.example.com/p":                                 nil,
		"https://a.example.com/p?color=red&size=9":                nil,
		"https://a.example.com/p?q=tent&keyword=big":              nil,
		"https://a.example.com/p?token=abc":                       {"token"},
		"https://a.example.com/p?access_key=1&SessionID=2":        {"SessionID", "access_key"},
		"https://a.example.com/p?booking=ABC123&ref=9&auth=1":     {"auth", "booking", "ref"},
		"https://a.example.com/p?x=" + strings.Repeat("a1B2", 10): {"x"},
		"https://a.example.com/p?bookingRef=Z":                    {"bookingRef"},
	}
	for in, want := range cases {
		if got := SensitiveQueryKeys(in); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v want %v", in, got, want)
		}
	}
}

func TestAddURLWithTokenNeedsConfirmation(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	csrf := b.login("07700900123")
	const u = "https://a.example.com/x?bookingRef=ZXC987&adults=2"
	w := b.do("POST", "/watches", url.Values{"csrf": {csrf}, "url": {u}})
	body := w.Body.String()
	if w.Code != 400 || !strings.Contains(body, "bookingRef") || !strings.Contains(body, `name="confirm_query"`) {
		t.Fatalf("%d", w.Code)
	}
	if strings.Count(body, "ZXC987") != 1 { // only the pre-filled URL field, not the warning text
		t.Fatalf("secret echoed %d times", strings.Count(body, "ZXC987"))
	}
	if n, _ := e.st.CountWatches(1); n != 0 {
		t.Fatal("watch saved without confirmation")
	}
	if w := b.do("POST", "/watches", url.Values{"csrf": {csrf}, "url": {u}, "confirm_query": {"1"}}); w.Code != 303 {
		t.Fatalf("%d", w.Code)
	}
	if n, _ := e.st.CountWatches(1); n != 1 {
		t.Fatal("not saved after confirmation")
	}
	// harmless query strings need no tick
	if w := b.do("POST", "/watches", url.Values{"csrf": {csrf}, "url": {"https://b.example.com/x?size=9"}}); w.Code != 303 {
		t.Fatalf("%d", w.Code)
	}
}

func TestLogsNeverHoldURLsQueriesOrNumbers(t *testing.T) {
	buf := captureLogs(t)
	e := newSchedEnv(t)
	const secretURL = "https://shop.example.com/p?token=SECRETTOKEN&ref=BOOK123"
	id := e.add(t, secretURL, nil, 60)
	e.f.errs[secretURL] = &FetchError{Kind: "network", Msg: "boom"}
	e.run(t, 0)
	// provider errors that echo the number and URL
	e.f.set(secretURL, pricePage(5000), pricePage(4000))
	e.run(t, 24*time.Hour)
	e.run(t, time.Hour)
	e.m.err = fmt.Errorf("meta: HTTP 400: cannot send to +447700900123 about %s", secretURL)
	e.snd.RunOnce(context.Background())
	// failed login message
	a := NewAuth(e.st, e.m, e.clk, "https://ww.example")
	a.RequestLogin(context.Background(), "+447700900555", "1.2.3.4", true)
	_ = id
	// inbound webhook and panic path
	srv := newTenv(t, func(c *Config) { c.WAAppSecret = "s" })
	body := metaBody("m1", "447700900123", "STOP")
	postMeta(srv, body, metaSignature("s", []byte(body)))
	logs := buf.String()
	if logs == "" {
		t.Fatal("no logs captured")
	}
	for _, bad := range []string{"SECRETTOKEN", "BOOK123", "token=", "/p?", "447700900123", "7700900123", "07700900123", "447700900555", "7700900555", "https://shop.example.com"} {
		if strings.Contains(logs, bad) {
			t.Errorf("log contains %q:\n%s", bad, logs)
		}
	}
	if !strings.Contains(logs, "***123") {
		t.Errorf("masked number not logged:\n%s", logs)
	}
}

func TestLogMessengerMasksNumber(t *testing.T) {
	buf := captureLogs(t)
	l := &LogMessenger{}
	l.Send(context.Background(), Message{Kind: KindAlert, To: "+447700900123", Text: "see https://x.example.com/p?token=Z"})
	l.Send(context.Background(), Message{Kind: KindLogin, To: "+447700900123", Text: "link", Params: []string{"l"}})
	if s := buf.String(); strings.Contains(s, "7700900") || strings.Contains(s, "token=Z") {
		t.Fatal(s)
	}
}

func TestRetentionPurge(t *testing.T) {
	st, clk := testStore(t)
	uid := mustUser(t, st, "+447700900123")
	id, _ := st.AddWatch(Watch{UserID: uid, URL: "https://a.example.com/", IntervalMin: 60})
	day := int64(86400)
	now := clk.Now().Unix()
	for _, age := range []int64{1, 29, 31, 89, 91, 200} {
		st.db.Exec(`INSERT INTO price_history(watch_id,price_pence,currency,method,checked_at) VALUES(?,?,?,?,?)`, id, 100, "GBP", "m", now-age*day)
		st.db.Exec(`INSERT INTO check_log(watch_id,checked_at,outcome) VALUES(?,?,?)`, id, now-age*day, "ok")
	}
	st.db.Exec(`INSERT INTO outbox(user_id,watch_id,kind,price_pence,event_key,text,params,status,next_attempt_at,created_at,done_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		uid, id, "changed", 1, "k1", "t", "[]", "accepted", 0, now-40*day, now-40*day)
	st.db.Exec(`INSERT INTO outbox(user_id,watch_id,kind,price_pence,event_key,text,params,status,next_attempt_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		uid, id, "changed", 2, "k2", "t", "[]", "queued", 0, now-40*day)
	// two old alert records of one kind: only the newest survives, even though both are old
	st.db.Exec(`INSERT INTO alerts_sent(watch_id,kind,price_pence,sent_at) VALUES(?,?,?,?)`, id, "changed", 1, now-300*day)
	st.db.Exec(`INSERT INTO alerts_sent(watch_id,kind,price_pence,sent_at) VALUES(?,?,?,?)`, id, "changed", 2, now-200*day)
	if err := st.Purge(90, 30); err != nil {
		t.Fatal(err)
	}
	count := func(q string) int {
		var n int
		st.db.QueryRow(q).Scan(&n)
		return n
	}
	if n := count(`SELECT COUNT(*) FROM price_history`); n != 4 { // 1, 29, 31, 89 days
		t.Errorf("history rows %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM check_log`); n != 2 { // 1, 29 days
		t.Errorf("check_log rows %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM outbox WHERE status='accepted'`); n != 0 {
		t.Error("old finished outbox row kept")
	}
	if n := count(`SELECT COUNT(*) FROM outbox WHERE status='queued'`); n != 1 {
		t.Error("queued outbox row purged")
	}
	if n := count(`SELECT COUNT(*) FROM alerts_sent`); n != 1 {
		t.Errorf("alerts_sent rows %d", n)
	}
	// configurable
	st.Purge(2, 2)
	if n := count(`SELECT COUNT(*) FROM price_history`); n != 1 {
		t.Errorf("history rows %d after 2-day retention", n)
	}
}

func TestMaintenancePurgesDailyAndBacksUp(t *testing.T) {
	st, clk := testStore(t)
	dir := t.TempDir()
	uid := mustUser(t, st, "+447700900123")
	id, _ := st.AddWatch(Watch{UserID: uid, URL: "https://a.example.com/", IntervalMin: 60})
	st.db.Exec(`INSERT INTO price_history(watch_id,price_pence,currency,method,checked_at) VALUES(?,?,?,?,?)`, id, 100, "GBP", "m", clk.Now().Unix()-100*86400)
	cfg := MaintenanceConfig{Dir: dir, RetainHistoryDays: 90, RetainDiagDays: 30}
	RunMaintenanceOnce(st.db, st, clk, cfg)
	var n int
	st.db.QueryRow(`SELECT COUNT(*) FROM price_history`).Scan(&n)
	if n != 0 {
		t.Fatal("history not purged by maintenance")
	}
	if !BackupExists(dir, clk.Now()) {
		t.Fatal("no backup")
	}
}

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func TestEncryptedBackup(t *testing.T) {
	st, clk := testStore(t)
	dir := t.TempDir()
	mustUser(t, st, "+447700900123")
	name, err := BackupDBKey(st.db, dir, clk.Now(), testKey())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(name, ".db.enc") {
		t.Fatal(name)
	}
	fi, _ := os.Stat(name)
	di, _ := os.Stat(filepath.Dir(name))
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("modes %v %v", fi.Mode().Perm(), di.Mode().Perm())
	}
	ents, _ := os.ReadDir(filepath.Dir(name))
	if len(ents) != 1 {
		t.Fatalf("stray files: %v", ents)
	}
	raw, _ := os.ReadFile(name)
	if bytes.Contains(raw, []byte("SQLite format")) || bytes.Contains(raw, []byte("+447700900123")) {
		t.Fatal("backup is not encrypted")
	}
	plain, err := DecryptBackup(testKey(), raw)
	if err != nil || !bytes.HasPrefix(plain, []byte("SQLite format 3\x00")) || !bytes.Contains(plain, []byte("+447700900123")) {
		t.Fatalf("decrypt: %v", err)
	}
	wrong := testKey()
	wrong[0] ^= 1
	if _, err := DecryptBackup(wrong, raw); err == nil {
		t.Fatal("wrong key accepted")
	}
	raw[len(raw)-1] ^= 1
	if _, err := DecryptBackup(testKey(), raw); err == nil {
		t.Fatal("tampered backup accepted")
	}
	if _, err := DecryptBackup(testKey(), []byte("junk")); err == nil {
		t.Fatal("junk accepted")
	}
	if _, err := EncryptBackup([]byte("short"), nil); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestPlainBackupModeAndExpiry(t *testing.T) {
	st, clk := testStore(t)
	dir := t.TempDir()
	for i := 0; i < 12; i++ {
		name, err := BackupDBKey(st.db, dir, clk.Now().AddDate(0, 0, i), nil)
		if err != nil {
			t.Fatal(err)
		}
		if fi, _ := os.Stat(name); fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", fi.Mode().Perm())
		}
	}
	ents, _ := os.ReadDir(filepath.Join(dir, "backups"))
	if len(ents) != 7 {
		t.Fatalf("%d backups survive; expiry is 7 days", len(ents))
	}
	// the oldest survivor is 6 days older than the newest: nothing 7+ days old remains
	if ents[0].Name() != "webwatcher-20261129.db" {
		t.Fatal(ents[0].Name())
	}
	// a wider-than-wanted directory is tightened
	os.Chmod(filepath.Join(dir, "backups"), 0o755)
	BackupDBKey(st.db, dir, clk.Now().AddDate(0, 0, 12), nil)
	if di, _ := os.Stat(filepath.Join(dir, "backups")); di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", di.Mode().Perm())
	}
}

func TestEncryptedBackupExpiryCoversEncFiles(t *testing.T) {
	st, clk := testStore(t)
	dir := t.TempDir()
	for i := 0; i < 9; i++ {
		BackupDBKey(st.db, dir, clk.Now().AddDate(0, 0, i), testKey())
	}
	ents, _ := os.ReadDir(filepath.Join(dir, "backups"))
	if len(ents) != 7 || !strings.HasSuffix(ents[0].Name(), ".db.enc") {
		t.Fatalf("%d %v", len(ents), ents[0].Name())
	}
}

func TestDatabaseFileAndDirModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "webwatcher.db"), nil, 0o644) // pre-existing, too open
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Exec(`CREATE TABLE IF NOT EXISTS x(a)`)
	db.Exec(`INSERT INTO x VALUES(1)`)
	for _, f := range []string{"webwatcher.db", "webwatcher.db-wal", "webwatcher.db-shm"} {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is %v", f, fi.Mode().Perm())
		}
	}
	if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
		t.Errorf("dir %v", di.Mode().Perm())
	}
}

func TestDeleteAccountPurgesOutboxAndSessions(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	csrf := b.login("07700900123")
	other := e.browser()
	other.login("07700900456")
	b.do("POST", "/watches", url.Values{"csrf": {csrf}, "url": {"https://a.example.com/x"}})
	e.st.RecordSuccess(1, 1000, "GBP", "m", 0, nil, qa(AlertChanged, 1000))
	other.do("POST", "/watches", url.Values{"csrf": {other.csrf("/")}, "url": {"https://b.example.com/x"}})
	e.st.RecordSuccess(2, 1000, "GBP", "m", 0, nil, qa(AlertChanged, 1000))
	b.do("POST", "/account/delete", url.Values{"csrf": {csrf}, "confirm": {"DELETE"}})
	var o, s, w int
	e.st.db.QueryRow(`SELECT COUNT(*) FROM outbox`).Scan(&o)
	e.st.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&s)
	e.st.db.QueryRow(`SELECT COUNT(*) FROM check_log WHERE watch_id=1`).Scan(&w)
	if o != 1 || s != 1 || w != 0 {
		t.Fatalf("outbox=%d sessions=%d checklog=%d (other user's data must stay)", o, s, w)
	}
}

func TestPrivacyConfig(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, err := LoadConfig(env(nil))
	if err != nil || c.RetainHistoryDays != 90 || c.RetainDiagDays != 30 || c.BackupKey != nil || c.RespectRobots {
		t.Fatalf("%+v %v", c, err)
	}
	key := base64.StdEncoding.EncodeToString(testKey())
	c, err = LoadConfig(env(map[string]string{"WW_BACKUP_KEY": key, "WW_RETAIN_HISTORY_DAYS": "7", "WW_RETAIN_DIAG_DAYS": "3", "WW_RESPECT_ROBOTS": "1"}))
	if err != nil || !bytes.Equal(c.BackupKey, testKey()) || c.RetainHistoryDays != 7 || c.RetainDiagDays != 3 || !c.RespectRobots {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []map[string]string{
		{"WW_BACKUP_KEY": base64.StdEncoding.EncodeToString([]byte("too short"))},
		{"WW_BACKUP_KEY": "not base64 !!!"},
		{"WW_RETAIN_HISTORY_DAYS": "0"},
		{"WW_RETAIN_DIAG_DAYS": "x"},
	} {
		if _, err := LoadConfig(env(bad)); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
