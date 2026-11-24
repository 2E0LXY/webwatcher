package app

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// plink creates (or rotates) the permanent link for phone and returns its raw token.
func plink(t *testing.T, e *tenv, phone string) (tok string, uid int64) {
	t.Helper()
	uid, err := e.st.EnsureUser(phone, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, h := newToken()
	if err := e.st.SetPersonalLink(uid, h); err != nil {
		t.Fatal(err)
	}
	return raw, uid
}

func (b *browser) plogin(tok string) *httptest.ResponseRecorder { return b.do("GET", "/p/"+tok, nil) }

func TestPersonalLinkHashAtRest(t *testing.T) {
	e := newTenv(t, nil)
	tok, uid := plink(t, e, "+447700900123")
	var h string
	e.st.db.QueryRow(`SELECT token_hash FROM personal_links WHERE user_id=?`, uid).Scan(&h)
	if h == tok || h != hashToken(tok) || len(tok) != 43 {
		t.Fatalf("hash %q tok %q", h, tok)
	}
	var n int
	e.st.db.QueryRow(`SELECT COUNT(*) FROM personal_links WHERE token_hash=?`, tok).Scan(&n)
	if n != 0 {
		t.Fatal("raw token stored")
	}
}

func TestPersonalLinkReusableAndCookieFlags(t *testing.T) {
	e := newTenv(t, func(c *Config) { c.BaseURL = "https://ww.test"; c.SessionDays = 7 })
	tok, uid := plink(t, e, "+447700900123")
	for i := 0; i < 3; i++ {
		b := e.browser()
		w := b.plogin(tok)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
			t.Fatalf("use %d: %d %s", i, w.Code, w.Header().Get("Location"))
		}
		if w.Header().Get("Referrer-Policy") != "no-referrer" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("headers %v", w.Header())
		}
		cs := w.Result().Cookies()
		if len(cs) != 1 {
			t.Fatalf("cookies %v", cs)
		}
		c := cs[0]
		if c.Name != "__Host-ww_session" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge != 7*86400 {
			t.Fatalf("cookie %+v", c)
		}
		if w := b.do("GET", "/", nil); w.Code != 200 {
			t.Fatalf("dashboard %d", w.Code)
		}
	}
	e.clk.Advance(time.Hour)
	info, _ := e.st.PersonalLinkStatus(uid)
	if info.LastUsedAt == nil || !info.Exists {
		t.Fatalf("%+v", info)
	}
}

func TestPersonalLinkHeadDoesNothing(t *testing.T) {
	e := newTenv(t, nil)
	tok, uid := plink(t, e, "+447700900123")
	w := e.browser().do("HEAD", "/p/"+tok, nil)
	if w.Code != 404 || len(w.Result().Cookies()) != 0 {
		t.Fatalf("%d", w.Code)
	}
	if info, _ := e.st.PersonalLinkStatus(uid); info.LastUsedAt != nil {
		t.Fatal("HEAD recorded use")
	}
}

func TestPersonalLinkRotationAndRevoke(t *testing.T) {
	e := newTenv(t, nil)
	old, uid := plink(t, e, "+447700900123")
	nw, h := newToken()
	if err := e.st.SetPersonalLink(uid, h); err != nil {
		t.Fatal(err)
	}
	if w := e.browser().plogin(old); w.Code != 404 {
		t.Fatalf("old link %d", w.Code)
	}
	if w := e.browser().plogin(nw); w.Code != 303 {
		t.Fatalf("new link %d", w.Code)
	}
	if ok, err := e.st.RevokePersonalLink(uid); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if w := e.browser().plogin(nw); w.Code != 404 {
		t.Fatalf("revoked link %d", w.Code)
	}
	// rotating after revoke restores access with a new token only
	n2, h2 := newToken()
	e.st.SetPersonalLink(uid, h2)
	if e.browser().plogin(nw).Code != 404 || e.browser().plogin(n2).Code != 303 {
		t.Fatal("rotate after revoke")
	}
}

func TestPersonalLinkInvalidIdenticalTo404AndRateLimit(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	unk := b.do("GET", "/nothing-here", nil)
	for i := 0; i < 20; i++ {
		w := b.plogin("bogus" + strings.Repeat("x", i))
		if w.Code != unk.Code || w.Body.String() != unk.Body.String() {
			t.Fatalf("attempt %d: %d %q vs %d %q", i, w.Code, w.Body.String(), unk.Code, unk.Body.String())
		}
		for _, h := range []string{"Content-Type", "Cache-Control", "Referrer-Policy"} {
			if w.Header().Get(h) != unk.Header().Get(h) {
				t.Fatalf("header %s differs: %q vs %q", h, w.Header().Get(h), unk.Header().Get(h))
			}
		}
	}
	tok, _ := plink(t, e, "+447700900123")
	if w := b.plogin("again"); w.Code != 429 {
		t.Fatalf("21st failure: %d", w.Code)
	}
	// blocked even with the right token (so guessing cannot continue), other IPs unaffected
	if w := b.plogin(tok); w.Code != 429 {
		t.Fatalf("valid token while blocked: %d", w.Code)
	}
	b2 := e.browser()
	b2.ip = "198.51.100.7"
	if w := b2.plogin(tok); w.Code != 303 {
		t.Fatalf("other IP: %d", w.Code)
	}
	e.clk.Advance(61 * time.Minute)
	if w := b.plogin(tok); w.Code != 303 {
		t.Fatalf("after window: %d", w.Code)
	}
	if n := e.srv.plLimit.Len(); n > DefaultLimiterKeys {
		t.Fatal(n)
	}
}

func TestPersonalLinkNotLogged(t *testing.T) {
	e := newTenv(t, nil)
	tok, _ := plink(t, e, "+447700900123")
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)
	b := e.browser()
	b.plogin(tok)
	b.plogin("wrong-" + tok)
	if strings.Contains(buf.String(), tok) || strings.Contains(buf.String(), "wrong-") {
		t.Fatalf("token in log: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "/p/[redacted]") {
		t.Fatalf("expected redacted path in debug log: %s", buf.String())
	}
	r := httptest.NewRequest("GET", "/p/"+tok, nil)
	if logPath(r) != "/p/[redacted]" {
		t.Fatal(logPath(r))
	}
}

func TestDoctorNoTokenAndSessionDays(t *testing.T) {
	env := twilioEnv(t)
	env["WW_SESSION_DAYS"] = "14"
	out, n := runDoctor(env)
	if n != 0 || !strings.Contains(out, "sessions last 14 days") {
		t.Fatalf("%d\n%s", n, out)
	}
	env["WW_SESSION_DAYS"] = "0"
	if _, n := runDoctor(env); n == 0 {
		t.Fatal("bad WW_SESSION_DAYS accepted")
	}
}

func TestPermalinkCLI(t *testing.T) {
	env, dir := adduserEnv(t)
	var out bytes.Buffer
	if err := Permalink(envMap(env), "07700 900123", PermalinkOpts{}, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	i := strings.Index(s, "https://watch.mydomain.uk/p/")
	if i < 0 || !strings.Contains(s, "treat it like a password") {
		t.Fatal(s)
	}
	tok := strings.Fields(s[i:])[0][len("https://watch.mydomain.uk/p/"):]
	db, _ := OpenDB(dir)
	defer db.Close()
	st := NewStore(db, SystemClock())
	uid, err := st.UserID("+447700900123")
	if err != nil {
		t.Fatal("user not created")
	}
	var src string
	db.QueryRow(`SELECT consent_source FROM users WHERE id=?`, uid).Scan(&src)
	if src != ConsentAdminCreated {
		t.Fatal(src)
	}
	if got, err := st.UsePersonalLink(hashToken(tok)); err != nil || got != uid {
		t.Fatal(got, err)
	}
	// status never prints the token
	var st2 bytes.Buffer
	if err := Permalink(envMap(env), "+447700900123", PermalinkOpts{Status: true}, &st2); err != nil || strings.Contains(st2.String(), tok) || !strings.Contains(st2.String(), "active") {
		t.Fatalf("%v %q", err, st2.String())
	}
	// rotate replaces
	var out2 bytes.Buffer
	Permalink(envMap(env), "+447700900123", PermalinkOpts{}, &out2)
	tok2 := strings.Fields(out2.String()[strings.Index(out2.String(), "/p/"):])[0][3:]
	if tok2 == tok {
		t.Fatal("not rotated")
	}
	if _, err := st.UsePersonalLink(hashToken(tok)); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("old token still valid")
	}
	// revoke
	if err := Permalink(envMap(env), "+447700900123", PermalinkOpts{Revoke: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UsePersonalLink(hashToken(tok2)); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("revoked token valid")
	}
	if err := Permalink(envMap(env), "+447700900123", PermalinkOpts{Revoke: true}, io.Discard); err == nil {
		t.Fatal("second revoke should report no active link")
	}
	// dashboard messenger records no consent
	env["WW_MESSENGER"] = "dashboard"
	Permalink(envMap(env), "+447700900555", PermalinkOpts{}, io.Discard)
	u2, _ := st.UserID("+447700900555")
	var cat *int64
	db.QueryRow(`SELECT consent_at, consent_source FROM users WHERE id=?`, u2).Scan(&cat, &src)
	if cat != nil || src != ConsentDashboard {
		t.Fatal(cat, src)
	}
}

func TestCLIRefusesRoot(t *testing.T) {
	env, _ := adduserEnv(t)
	t.Setenv("WW_ALLOW_ROOT", "")
	old := geteuid
	geteuid = func() int { return 0 }
	defer func() { geteuid = old }()
	var out bytes.Buffer
	for name, f := range map[string]func() error{
		"permalink": func() error { return Permalink(envMap(env), "+447700900123", PermalinkOpts{}, &out) },
		"revoke":    func() error { return Permalink(envMap(env), "+447700900123", PermalinkOpts{Revoke: true}, &out) },
		"status":    func() error { return Permalink(envMap(env), "+447700900123", PermalinkOpts{Status: true}, &out) },
		"setadmin":  func() error { return SetAdmin(envMap(env), "+447700900123", false, &out) },
		"adduser":   func() error { return AddUser(envMap(env), "+447700900123", &out) },
	} {
		if err := f(); err == nil || !strings.Contains(err.Error(), "as root") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if out.Len() != 0 {
		t.Fatal(out.String())
	}
}

func TestDeleteAccountPurgesLink(t *testing.T) {
	e := newTenv(t, nil)
	tok, uid := plink(t, e, "+447700900123")
	b := e.browser()
	b.plogin(tok)
	c := b.csrf("/account")
	if w := b.do("POST", "/account/delete", url.Values{"csrf": {c}, "confirm": {"DELETE"}}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	var n int
	e.st.db.QueryRow(`SELECT COUNT(*) FROM personal_links WHERE user_id=?`, uid).Scan(&n)
	if n != 0 || e.browser().plogin(tok).Code != 404 {
		t.Fatal("link survived delete")
	}
}

func TestDashboardRevokeOwnershipAndCSRF(t *testing.T) {
	e := newTenv(t, nil)
	tokA, uidA := plink(t, e, "+447700900001")
	tokB, uidB := plink(t, e, "+447700900002")
	a := e.browser()
	a.plogin(tokA)
	page := a.do("GET", "/", nil).Body.String()
	if !strings.Contains(page, "Personal login link") || !strings.Contains(page, "last used") || strings.Contains(page, tokA) {
		t.Fatal("section missing or token shown")
	}
	// no CSRF: refused, nothing revoked
	if w := a.do("POST", "/account/personal-link/revoke", url.Values{}); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if info, _ := e.st.PersonalLinkStatus(uidA); !info.Exists {
		t.Fatal("revoked without CSRF")
	}
	// revoke only touches the caller's own link
	c := a.csrf("/")
	if w := a.do("POST", "/account/personal-link/revoke", url.Values{"csrf": {c}, "user_id": {"2"}}); w.Code != 303 {
		t.Fatal(w.Code)
	}
	if info, _ := e.st.PersonalLinkStatus(uidA); info.Exists {
		t.Fatal("own link not revoked")
	}
	if info, _ := e.st.PersonalLinkStatus(uidB); !info.Exists {
		t.Fatal("other user's link revoked")
	}
	if e.browser().plogin(tokB).Code != 303 {
		t.Fatal("B link broken")
	}
	// anonymous POST
	if w := e.browser().do("POST", "/account/personal-link/revoke", url.Values{"csrf": {"x"}}); w.Code != 303 {
		t.Fatal(w.Code)
	}
	// there is no HTTP route that creates a link
	for _, p := range []string{"/account/personal-link", "/account/personal-link/create", "/account/personal-link/rotate"} {
		if w := a.do("POST", p, url.Values{"csrf": {c}}); w.Code != 404 && w.Code != 405 {
			t.Errorf("%s %d", p, w.Code)
		}
	}
}

func TestSessionDaysConfig(t *testing.T) {
	c, err := LoadConfig(envMap(map[string]string{"WW_BASE_URL": "https://a.uk", "WW_MESSENGER": "dashboard", "WW_SESSION_DAYS": "3"}))
	if err != nil || c.SessionLifetime() != 72*time.Hour {
		t.Fatal(err, c.SessionLifetime())
	}
	c, _ = LoadConfig(envMap(map[string]string{"WW_BASE_URL": "https://a.uk", "WW_MESSENGER": "dashboard"}))
	if c.SessionLifetime() != 30*24*time.Hour {
		t.Fatal(c.SessionLifetime())
	}
	for _, bad := range []string{"0", "366", "x"} {
		if _, err := LoadConfig(envMap(map[string]string{"WW_BASE_URL": "https://a.uk", "WW_MESSENGER": "dashboard", "WW_SESSION_DAYS": bad})); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	e := newTenv(t, func(c *Config) { c.SessionDays = 2 })
	e.srv.auth.SetSessionTTL(e.cfg.SessionLifetime())
	tok, _ := plink(t, e, "+447700900123")
	b := e.browser()
	b.plogin(tok)
	e.clk.Advance(47 * time.Hour)
	if b.do("GET", "/", nil).Code != 200 {
		t.Fatal("expired early")
	}
	e.clk.Advance(2 * time.Hour)
	if b.do("GET", "/", nil).Code != 303 {
		t.Fatal("did not expire")
	}
	// log out everywhere still works
	b2 := e.browser()
	b2.plogin(tok)
	c2 := b2.csrf("/account")
	b2.do("POST", "/account/logout-everywhere", url.Values{"csrf": {c2}})
	if b2.do("GET", "/", nil).Code != 303 {
		t.Fatal("still logged in")
	}
}
