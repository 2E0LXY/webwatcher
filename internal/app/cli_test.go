package app

import (
	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	secTwilio = "SECRET-twilio-token-9f8e7d"
	secMeta   = "SECRET-meta-token-1a2b3c"
	secApp    = "SECRET-app-secret-4d5e6f"
	secVerify = "SECRET-verify-7g8h9i"
	secAdmin  = "SECRET-admin-0j1k2l"
	secBackup = "SECRET-backup-not-base64"
)

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func goodDir(t *testing.T) string {
	d := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

func twilioEnv(t *testing.T) map[string]string {
	return map[string]string{
		"WW_DIR": goodDir(t), "WW_BASE_URL": "https://watch.mydomain.uk", "WW_MESSENGER": "twilio",
		"WW_TWILIO_SID": "ACsid", "WW_TWILIO_TOKEN": secTwilio, "WW_TWILIO_FROM": "+14155238886",
		"WW_ADMIN_TOKEN": secAdmin,
	}
}

func runDoctor(env map[string]string) (string, int) {
	var b bytes.Buffer
	n := Doctor(envMap(env), &b)
	return b.String(), n
}

func TestDoctorTwilioOK(t *testing.T) {
	out, n := runDoctor(twilioEnv(t))
	if n != 0 {
		t.Fatalf("fails=%d\n%s", n, out)
	}
	for _, want := range []string{"OK   WW_BASE_URL is https", "https://watch.mydomain.uk/hook/twilio", "would start", "WW_TWILIO_TOKEN is set", "All checks passed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "FAIL") {
		t.Errorf("unexpected FAIL\n%s", out)
	}
}

func TestDoctorMetaOK(t *testing.T) {
	d := goodDir(t)
	out, n := runDoctor(map[string]string{
		"WW_DIR": d, "WW_BASE_URL": "https://watch.mydomain.uk", "WW_MESSENGER": "meta",
		"WW_WA_PHONE_ID": "123", "WW_WA_TOKEN": secMeta, "WW_WA_APP_SECRET": secApp, "WW_WA_VERIFY_TOKEN": secVerify,
	})
	if n != 0 {
		t.Fatalf("fails=%d\n%s", n, out)
	}
	for _, want := range []string{"https://watch.mydomain.uk/hook/meta", "login=webwatcher_login alert=webwatcher_alert language=en_GB"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestDoctorFailures(t *testing.T) {
	cases := []struct {
		name string
		mut  func(map[string]string)
		want string
	}{
		{"http base", func(m map[string]string) { m["WW_BASE_URL"] = "http://watch.mydomain.uk" }, "FAIL WW_BASE_URL must be https"},
		{"placeholder", func(m map[string]string) { m["WW_BASE_URL"] = "https://watch.example.com" }, "example.com placeholder"},
		{"no base", func(m map[string]string) { delete(m, "WW_BASE_URL") }, "FAIL WW_BASE_URL is not set"},
		{"missing twilio var", func(m map[string]string) { delete(m, "WW_TWILIO_FROM") }, "FAIL WW_TWILIO_FROM is missing"},
		{"log messenger", func(m map[string]string) { delete(m, "WW_MESSENGER") }, "FAIL WW_MESSENGER is not set to twilio or meta"},
		{"unknown messenger", func(m map[string]string) { m["WW_MESSENGER"] = "carrier-pigeon" }, "unknown value"},
		{"meta missing", func(m map[string]string) { m["WW_MESSENGER"] = "meta" }, "FAIL WW_WA_PHONE_ID is missing"},
		{"bad backup key", func(m map[string]string) { m["WW_BACKUP_KEY"] = secBackup }, "FAIL WW_BACKUP_KEY is not 32 random bytes"},
		{"loose dir", func(m map[string]string) { os.Chmod(m["WW_DIR"], 0o755) }, "must be 0700"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := twilioEnv(t)
			c.mut(env)
			out, n := runDoctor(env)
			if n == 0 || !strings.Contains(out, c.want) {
				t.Fatalf("fails=%d, want %q in\n%s", n, c.want, out)
			}
			if !strings.Contains(out, "FAIL the service would NOT start") && c.name != "placeholder" && c.name != "loose dir" {
				t.Errorf("expected a would-not-start verdict\n%s", out)
			}
		})
	}
}

func TestDoctorDevMode(t *testing.T) {
	out, n := runDoctor(map[string]string{"WW_DIR": goodDir(t), "WW_BASE_URL": "http://127.0.0.1:8780", "WW_DEV": "1", "WW_MESSENGER": "log"})
	if n != 0 || !strings.Contains(out, "WARN") {
		t.Fatalf("fails=%d\n%s", n, out)
	}
}

func TestDoctorMissingDirIsWarning(t *testing.T) {
	env := twilioEnv(t)
	env["WW_DIR"] = filepath.Join(t.TempDir(), "nope")
	out, n := runDoctor(env)
	if n != 0 || !strings.Contains(out, "WARN data directory") {
		t.Fatalf("fails=%d\n%s", n, out)
	}
}

// Whatever the configuration, no secret value may appear in the output.
func TestDoctorNeverLeaksSecrets(t *testing.T) {
	secrets := []string{secTwilio, secMeta, secApp, secVerify, secAdmin, secBackup}
	base := func() map[string]string {
		return map[string]string{
			"WW_DIR": goodDir(t), "WW_BASE_URL": "https://watch.mydomain.uk",
			"WW_TWILIO_SID": "ACsid", "WW_TWILIO_TOKEN": secTwilio, "WW_TWILIO_FROM": "+1",
			"WW_WA_PHONE_ID": "1", "WW_WA_TOKEN": secMeta, "WW_WA_APP_SECRET": secApp, "WW_WA_VERIFY_TOKEN": secVerify,
			"WW_ADMIN_TOKEN": secAdmin, "WW_BACKUP_KEY": secBackup,
		}
	}
	for _, msgr := range []string{"twilio", "meta", "log", "", "bogus"} {
		for _, dev := range []string{"", "1"} {
			env := base()
			env["WW_MESSENGER"], env["WW_DEV"] = msgr, dev
			out, _ := runDoctor(env)
			for _, s := range secrets {
				if strings.Contains(out, s) {
					t.Errorf("messenger=%q dev=%q leaked %q:\n%s", msgr, dev, s, out)
				}
			}
		}
	}
	// secrets in a half-configured env too
	env := base()
	env["WW_MESSENGER"] = "meta"
	delete(env, "WW_WA_PHONE_ID")
	out, _ := runDoctor(env)
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("leaked %q:\n%s", s, out)
		}
	}
}

func TestParseEnvFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "env")
	os.WriteFile(p, []byte("# c\nA=1\n  B = \"two words\" \n#C=3\nD='x'\nbadline\nE=a=b\n"), 0o600)
	m, err := ParseEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if m["A"] != "1" || m["B"] != "two words" || m["D"] != "x" || m["E"] != "a=b" {
		t.Fatalf("%v", m)
	}
	if _, ok := m["C"]; ok {
		t.Fatal("comment parsed")
	}
	g, err := EnvLookup(p, func(string) string { return "" })
	if err != nil || g("A") != "1" {
		t.Fatalf("lookup: %v", err)
	}
}

func adduserEnv(t *testing.T) (map[string]string, string) {
	t.Setenv("WW_ALLOW_ROOT", "1") // tests may run as root
	d := filepath.Join(t.TempDir(), "state")
	return map[string]string{"WW_DIR": d, "WW_BASE_URL": "https://watch.mydomain.uk/"}, d
}

func TestAddUserPrintsOneTimeLink(t *testing.T) {
	env, dir := adduserEnv(t)
	var out bytes.Buffer
	if err := AddUser(envMap(env), "07700 900123", &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	i := strings.Index(s, "https://watch.mydomain.uk/auth/")
	if i < 0 {
		t.Fatalf("no link in %q", s)
	}
	raw := strings.Fields(s[i:])[0][len("https://watch.mydomain.uk/auth/"):]
	if strings.Contains(s, "7700900123") {
		t.Error("full phone number printed")
	}

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var phone, hash, src string
	var created, expires int64
	if err := db.QueryRow(`SELECT phone, token_hash, consent_source, created_at, expires_at FROM login_tokens`).Scan(&phone, &hash, &src, &created, &expires); err != nil {
		t.Fatal(err)
	}
	if phone != "+447700900123" || src != "admin-created" || expires-created != 900 {
		t.Fatalf("phone=%s src=%s ttl=%d", phone, src, expires-created)
	}
	if hash == raw || hash != hashToken(raw) {
		t.Fatal("token not hashed at rest")
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM login_tokens WHERE token_hash=?`, raw).Scan(&n)
	if n != 0 {
		t.Fatal("raw token stored")
	}

	// The link logs in once, records consent and is then spent.
	st := NewStore(db, SystemClock())
	a := NewAuth(st, &fakeMessenger{}, SystemClock(), "https://watch.mydomain.uk")
	if _, _, err := a.Consume(raw); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if _, _, err := a.Consume(raw); err == nil {
		t.Fatal("link worked twice")
	}
	var consentAt sql.NullInt64
	var usrc string
	var alerts int
	db.QueryRow(`SELECT consent_at, consent_source, alerts_enabled FROM users WHERE phone=?`, phone).Scan(&consentAt, &usrc, &alerts)
	if !consentAt.Valid || usrc != "admin-created" || alerts != 1 {
		t.Fatalf("user consent: %v %q %d", consentAt, usrc, alerts)
	}
}

func TestAddUserErrors(t *testing.T) {
	env, _ := adduserEnv(t)
	var out bytes.Buffer
	if err := AddUser(envMap(env), "not a number", &out); err == nil {
		t.Error("bad phone accepted")
	}
	delete(env, "WW_BASE_URL")
	if err := AddUser(envMap(env), "+447700900123", &out); err == nil {
		t.Error("missing base URL accepted")
	}
	if out.Len() != 0 {
		t.Errorf("printed on error: %q", out.String())
	}
}

func TestAddUserNotAvailableOverHTTP(t *testing.T) {
	e := newTenv(t, nil)
	for _, c := range []struct{ m, p string }{
		{"POST", "/adduser"}, {"GET", "/adduser"}, {"POST", "/admin/adduser"}, {"GET", "/admin/users"}, {"POST", "/api/adduser"},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(c.m, c.p, strings.NewReader("phone=%2B447700900123"))
		r.RemoteAddr = "203.0.113.9:4000"
		e.h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d", c.m, c.p, w.Code)
		}
	}
}
