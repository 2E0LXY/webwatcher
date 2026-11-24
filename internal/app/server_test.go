package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

type tenv struct {
	t    *testing.T
	srv  *Server
	h    http.Handler
	st   *Store
	clk  *fakeClock
	msgr *fakeMessenger
	cfg  Config
}

func newTenv(t *testing.T, mut func(*Config)) *tenv {
	st, clk := testStore(t)
	cfg := Config{Addr: "127.0.0.1:0", BaseURL: "http://ww.test", Messenger: "log"}
	if mut != nil {
		mut(&cfg)
	}
	m := &fakeMessenger{}
	a := NewAuth(st, m, clk, cfg.BaseURL)
	srv, err := NewServer(cfg, st, a, m, clk)
	if err != nil {
		t.Fatal(err)
	}
	return &tenv{t, srv, srv.Handler(), st, clk, m, cfg}
}

type browser struct {
	e       *tenv
	cookies map[string]*http.Cookie
	ip      string
}

func (e *tenv) browser() *browser {
	return &browser{e: e, cookies: map[string]*http.Cookie{}, ip: "203.0.113.5"}
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (b *browser) do(method, path string, form url.Values) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, body)
	r.RemoteAddr = b.ip + ":4000"
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range b.cookies {
		r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	w := httptest.NewRecorder()
	b.e.h.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 || c.Value == "" {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return w
}

func (b *browser) csrf(path string) string {
	w := b.do("GET", path, nil)
	m := csrfRe.FindStringSubmatch(w.Body.String())
	if m == nil {
		b.e.t.Fatalf("no csrf on %s: %d %s", path, w.Code, w.Body.String())
	}
	return m[1]
}

// login performs the full magic-link flow and returns the session csrf.
func (b *browser) login(phone string) string {
	t := b.e.t
	c := b.csrf("/login")
	w := b.do("POST", "/login", url.Values{"csrf": {c}, "phone": {phone}, "consent": {"1"}})
	if w.Code != 200 {
		t.Fatalf("login post: %d", w.Code)
	}
	b.e.srv.Wait()
	sent := b.e.msgr.Sent()
	link := sent[len(sent)-1].Params[0]
	path := strings.TrimPrefix(link, b.e.cfg.BaseURL)
	c = b.csrf(path)
	w = b.do("POST", path, url.Values{"csrf": {c}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("auth post: %d %s", w.Code, w.Body.String())
	}
	return b.csrf("/")
}

func TestLoginFlowEndToEnd(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	if w := b.do("GET", "/", nil); w.Code != 303 || w.Header().Get("Location") != "/login" {
		t.Fatalf("anon home: %d %s", w.Code, w.Header().Get("Location"))
	}
	b.login("07700 900123")
	w := b.do("GET", "/", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "447700900123") {
		t.Fatalf("dashboard: %d", w.Code)
	}
	// message sent through the messenger to the normalised number
	if s := e.msgr.Sent(); len(s) != 1 || s[0].To != "+447700900123" || s[0].Kind != KindLogin {
		t.Fatalf("%+v", s)
	}
	// returning user: same user row after a second login on another browser
	b2 := e.browser()
	b2.login("+44 7700 900123")
	var n int
	e.st.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	if n != 1 {
		t.Fatalf("users %d", n)
	}
}

func TestMagicLinkGETDoesNotConsume(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	c := b.csrf("/login")
	b.do("POST", "/login", url.Values{"csrf": {c}, "phone": {"07700900123"}, "consent": {"1"}})
	e.srv.Wait()
	path := strings.TrimPrefix(e.msgr.Sent()[0].Params[0], e.cfg.BaseURL)
	for i := 0; i < 3; i++ { // link-preview bots
		if w := httptest.NewRecorder(); true {
			r := httptest.NewRequest("GET", path, nil)
			e.h.ServeHTTP(w, r)
			if w.Code != 200 || len(w.Result().Cookies()) > 1 {
				t.Fatal("GET should only render a confirm page")
			}
			for _, ck := range w.Result().Cookies() {
				if strings.Contains(ck.Name, "session") {
					t.Fatal("session set on GET")
				}
			}
		}
	}
	c = b.csrf(path)
	if w := b.do("POST", path, url.Values{"csrf": {c}}); w.Code != 303 {
		t.Fatalf("token burned by GET: %d", w.Code)
	}
	// single use
	c = b.csrf(path)
	if w := b.do("POST", path, url.Values{"csrf": {c}}); w.Code != 401 {
		t.Fatalf("reuse: %d", w.Code)
	}
	// POST without the pre-session CSRF cookie/field is refused
	b3 := e.browser()
	if w := b3.do("POST", path, url.Values{"csrf": {"x"}}); w.Code != 403 {
		t.Fatalf("csrf on auth: %d", w.Code)
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	e := newTenv(t, func(c *Config) { c.BaseURL = "https://ww.example" })
	b := e.browser()
	c := b.csrf("/login")
	b.do("POST", "/login", url.Values{"csrf": {c}, "phone": {"07700900123"}, "consent": {"1"}})
	e.srv.Wait()
	path := strings.TrimPrefix(e.msgr.Sent()[0].Params[0], "https://ww.example")
	c = b.csrf(path)
	w := b.do("POST", path, url.Values{"csrf": {c}})
	var sc *http.Cookie
	for _, ck := range w.Result().Cookies() {
		if ck.Name == "__Host-ww_session" {
			sc = ck
		}
	}
	if sc == nil {
		t.Fatalf("no session cookie: %v", w.Result().Cookies())
	}
	if !sc.HttpOnly || !sc.Secure || sc.SameSite != http.SameSiteLaxMode || sc.MaxAge != 30*24*3600 || sc.Path != "/" {
		t.Fatalf("%+v", sc)
	}
	if hsts := w.Header().Get("Strict-Transport-Security"); hsts == "" {
		t.Error("no HSTS over https")
	}
	// the cookie value is not what is stored
	var cnt int
	e.st.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash=?`, sc.Value).Scan(&cnt)
	if cnt != 0 {
		t.Fatal("raw session token in DB")
	}
}

func TestNoEnumerationResponsesIdentical(t *testing.T) {
	e := newTenv(t, nil)
	// make +447700900111 a known user
	mustUser(t, e.st, "+447700900111")
	post := func(phone, ip string) (int, string) {
		b := e.browser()
		b.ip = ip
		c := b.csrf("/login")
		w := b.do("POST", "/login", url.Values{"csrf": {c}, "phone": {phone}, "consent": {"1"}})
		return w.Code, w.Body.String()
	}
	cKnown, bKnown := post("07700900111", "198.51.100.1")
	cUnknown, bUnknown := post("07700900222", "198.51.100.2")
	if cKnown != 200 || cKnown != cUnknown || bKnown != bUnknown {
		t.Fatalf("known/unknown differ: %d %d\n%s\n---\n%s", cKnown, cUnknown, bKnown, bUnknown)
	}
	// rate-limited request: hammer one number, response must stay identical
	var cLimited int
	var bLimited string
	for i := 0; i < 8; i++ {
		cLimited, bLimited = post("07700900111", fmt.Sprintf("198.51.100.%d", 10+i))
	}
	e.srv.Wait()
	if cLimited != cKnown || bLimited != bKnown {
		t.Fatal("rate-limited response differs")
	}
	if n := len(e.msgr.Sent()); n != 5+1 { // 5 allowed for the known number, 1 for the unknown
		t.Fatalf("sent %d messages", n)
	}
	// junk numbers are rejected with a validation error (format only, says nothing about accounts)
	if c, _ := post("not a number", "198.51.100.99"); c != 400 {
		t.Fatalf("junk: %d", c)
	}
}

func TestLoginCSRFRequired(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	if w := b.do("POST", "/login", url.Values{"phone": {"07700900123"}}); w.Code != 403 {
		t.Fatalf("no csrf: %d", w.Code)
	}
	b.csrf("/login")
	if w := b.do("POST", "/login", url.Values{"phone": {"07700900123"}, "csrf": {"wrong"}}); w.Code != 403 {
		t.Fatalf("wrong csrf: %d", w.Code)
	}
	e.srv.Wait()
	if len(e.msgr.Sent()) != 0 {
		t.Fatal("message sent despite CSRF failure")
	}
}

func TestSessionCSRFOnPosts(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	csrf := b.login("07700900123")
	good := url.Values{"url": {"https://www.jet2.com/x"}, "interval": {"60"}}
	good.Set("csrf", csrf)

	for _, f := range []url.Values{{"url": {"https://www.jet2.com/x"}}, {"url": {"https://www.jet2.com/x"}, "csrf": {"nope"}}} {
		if w := b.do("POST", "/watches", f); w.Code != 403 {
			t.Fatalf("expected 403, got %d", w.Code)
		}
	}
	if w := b.do("POST", "/watches", good); w.Code != 303 {
		t.Fatalf("good add: %d", w.Code)
	}
	for _, p := range []string{"/watches/1/edit", "/watches/1/pause", "/watches/1/resume", "/watches/1/delete", "/logout", "/account/delete"} {
		if w := b.do("POST", p, url.Values{"csrf": {"bad"}}); w.Code != 403 {
			t.Errorf("%s without valid csrf: %d", p, w.Code)
		}
	}
	// unauthenticated POSTs go to login, not through
	anon := e.browser()
	if w := anon.do("POST", "/watches", good); w.Code != 303 || w.Header().Get("Location") != "/login" {
		t.Fatalf("anon post: %d", w.Code)
	}
}

func TestAddWatchValidation(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	csrf := b.login("07700900123")
	try := func(f url.Values) *httptest.ResponseRecorder {
		f.Set("csrf", csrf)
		return b.do("POST", "/watches", f)
	}
	bad := []url.Values{
		{"url": {"http://127.0.0.1/"}}, {"url": {"http://localhost/"}}, {"url": {"https://user:pw@a.example.com/"}},
		{"url": {"https://a.example.com:8443/"}}, {"url": {"ftp://a.example.com/"}}, {"url": {""}},
		{"url": {"https://a.example.com/"}, "selector": {"div:hover"}},
		{"url": {"https://a.example.com/"}, "target": {"abc"}},
		{"url": {"https://a.example.com/"}, "label": {strings.Repeat("x", 81)}},
	}
	for _, f := range bad {
		if w := try(f); w.Code != 400 {
			t.Errorf("%v: got %d", f, w.Code)
		}
	}
	if n, _ := e.st.CountWatches(1); n != 0 {
		t.Fatalf("%d watches stored from bad input", n)
	}
	w := try(url.Values{"url": {"https://www.tui.co.uk/p"}, "label": {"<b>Trip</b>"}, "target": {"£1,099.50"}, "interval": {"180"}, "selector": {".price > span"}})
	if w.Code != 303 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	ws, _ := e.st.ListWatches(1)
	if len(ws) != 1 || *ws[0].TargetPence != 109950 || ws[0].IntervalMin != 180 || ws[0].Selector != ".price > span" {
		t.Fatalf("%+v", ws[0])
	}
	// output is escaped
	page := b.do("GET", "/", nil).Body.String()
	if strings.Contains(page, "<b>Trip</b>") || !strings.Contains(page, "&lt;b&gt;Trip&lt;/b&gt;") {
		t.Fatal("label not escaped")
	}
}

func TestPerUserWatchCapViaHTTP(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	csrf := b.login("07700900123")
	for i := 0; i < MaxWatchesPerUser+1; i++ {
		w := b.do("POST", "/watches", url.Values{"csrf": {csrf}, "url": {fmt.Sprintf("https://a.example.com/%d", i)}})
		if i < MaxWatchesPerUser && w.Code != 303 {
			t.Fatalf("add %d: %d", i, w.Code)
		}
		if i == MaxWatchesPerUser && (w.Code != 400 || !strings.Contains(w.Body.String(), "at most 30")) {
			t.Fatalf("cap not enforced: %d", w.Code)
		}
	}
	// another user is unaffected
	b2 := e.browser()
	c2 := b2.login("07700900456")
	if w := b2.do("POST", "/watches", url.Values{"csrf": {c2}, "url": {"https://a.example.com/x"}}); w.Code != 303 {
		t.Fatalf("other user: %d", w.Code)
	}
}

func TestWatchOwnershipAndActions(t *testing.T) {
	e := newTenv(t, nil)
	a, bb := e.browser(), e.browser()
	ca := a.login("07700900123")
	cb := bb.login("07700900456")
	a.do("POST", "/watches", url.Values{"csrf": {ca}, "url": {"https://a.example.com/x"}, "target": {"100"}})
	if w := a.do("GET", "/watches/1", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Price history") {
		t.Fatalf("own watch: %d", w.Code)
	}
	for _, p := range []string{"/watches/1"} {
		if w := bb.do("GET", p, nil); w.Code != 404 {
			t.Errorf("other user GET: %d", w.Code)
		}
	}
	for _, p := range []string{"/watches/1/edit", "/watches/1/pause", "/watches/1/delete"} {
		if w := bb.do("POST", p, url.Values{"csrf": {cb}}); w.Code != 404 {
			t.Errorf("other user POST %s: %d", p, w.Code)
		}
	}
	if _, err := e.st.WatchByID(1, 1); err != nil {
		t.Fatal("watch damaged")
	}
	// edit target, pause, resume, delete as owner
	a.do("POST", "/watches/1/edit", url.Values{"csrf": {ca}, "target": {"80.50"}, "interval": {"360"}})
	w, _ := e.st.WatchByID(1, 1)
	if *w.TargetPence != 8050 || w.IntervalMin != 360 {
		t.Fatalf("%+v", w)
	}
	a.do("POST", "/watches/1/edit", url.Values{"csrf": {ca}, "target": {""}, "interval": {"360"}})
	if w, _ = e.st.WatchByID(1, 1); w.TargetPence != nil {
		t.Fatal("target not cleared")
	}
	a.do("POST", "/watches/1/pause", url.Values{"csrf": {ca}})
	if w, _ = e.st.WatchByID(1, 1); !w.Paused {
		t.Fatal("not paused")
	}
	if !strings.Contains(a.do("GET", "/", nil).Body.String(), "Paused") {
		t.Fatal("dashboard lacks paused status")
	}
	a.do("POST", "/watches/1/resume", url.Values{"csrf": {ca}})
	if w, _ = e.st.WatchByID(1, 1); w.Paused {
		t.Fatal("not resumed")
	}
	a.do("POST", "/watches/1/delete", url.Values{"csrf": {ca}})
	if _, err := e.st.WatchByID(1, 1); err == nil {
		t.Fatal("not deleted")
	}
}

func TestDashboardShowsBlockedNotice(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	csrf := b.login("07700900123")
	b.do("POST", "/watches", url.Values{"csrf": {csrf}, "url": {"https://www.jet2.com/x"}})
	e.st.RecordFailure(1, "blocked", "blocked by site (HTTP 403)", 1, e.clk.Now().Unix()+3600)
	page := b.do("GET", "/", nil).Body.String()
	for _, s := range []string{"Blocked by site", "blocking automated checks", "does not try to get round bot protection", "blocked by site (HTTP 403)"} {
		if !strings.Contains(page, s) {
			t.Errorf("missing %q", s)
		}
	}
}

func TestDeleteAccountViaHTTP(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	csrf := b.login("07700900123")
	b.do("POST", "/watches", url.Values{"csrf": {csrf}, "url": {"https://a.example.com/x"}})
	e.st.RecordSuccess(1, 1000, "GBP", "regex", 0, nil)
	if w := b.do("POST", "/account/delete", url.Values{"csrf": {csrf}, "confirm": {"nope"}}); w.Code != 400 {
		t.Fatalf("no confirmation: %d", w.Code)
	}
	if w := b.do("POST", "/account/delete", url.Values{"csrf": {csrf}, "confirm": {"DELETE"}}); w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
	for _, q := range []string{`SELECT COUNT(*) FROM users`, `SELECT COUNT(*) FROM sessions`, `SELECT COUNT(*) FROM watches`,
		`SELECT COUNT(*) FROM price_history`, `SELECT COUNT(*) FROM alerts_sent`, `SELECT COUNT(*) FROM login_tokens`} {
		var n int
		e.st.db.QueryRow(q).Scan(&n)
		if n != 0 {
			t.Errorf("%s = %d", q, n)
		}
	}
	if w := b.do("GET", "/", nil); w.Code != 303 {
		t.Fatalf("session survived: %d", w.Code)
	}
}

func TestLogoutKillsSession(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	csrf := b.login("07700900123")
	old := map[string]*http.Cookie{}
	for k, v := range b.cookies {
		old[k] = v
	}
	b.do("POST", "/logout", url.Values{"csrf": {csrf}})
	b.cookies = old // replay the stolen cookie
	if w := b.do("GET", "/", nil); w.Code != 303 {
		t.Fatalf("old session still valid: %d", w.Code)
	}
}

func TestSecurityHeadersAndLimits(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	w := b.do("GET", "/login", nil)
	h := w.Header()
	if !strings.HasPrefix(h.Get("Content-Security-Policy"), "default-src 'self'") || h.Get("X-Frame-Options") != "DENY" ||
		h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("%v", h)
	}
	if strings.Contains(w.Body.String(), "<script") || strings.Contains(w.Body.String(), "style=") {
		t.Fatal("inline script/style in page")
	}
	// 64 KB body limit
	big := url.Values{"csrf": {"x"}, "phone": {strings.Repeat("1", 100<<10)}}
	if w := b.do("POST", "/login", big); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("big body: %d", w.Code)
	}
	if w := b.do("GET", "/static/style.css", nil); w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "css") {
		t.Fatalf("css: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if w := b.do("GET", "/static/../layout.html", nil); w.Code == 200 {
		t.Fatal("static traversal")
	}
	if w := b.do("GET", "/nope", nil); w.Code != 404 {
		t.Fatalf("404: %d", w.Code)
	}
}

func TestHealthzAdminOnly(t *testing.T) {
	e := newTenv(t, nil)
	if w := e.browser().do("GET", "/healthz", nil); w.Code != 404 {
		t.Fatalf("no admin token configured: %d", w.Code)
	}
	e2 := newTenv(t, func(c *Config) { c.AdminToken = "s3cret-admin" })
	b := e2.browser()
	if w := b.do("GET", "/healthz", nil); w.Code != 401 {
		t.Fatalf("anon: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	e2.h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("wrong: %d", w.Code)
	}
	r = httptest.NewRequest("GET", "/healthz", nil)
	r.Header.Set("Authorization", "Bearer s3cret-admin")
	w = httptest.NewRecorder()
	e2.h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestDevPageGated(t *testing.T) {
	// dev off
	e := newTenv(t, nil)
	if w := e.browser().do("GET", "/dev", nil); w.Code != 404 {
		t.Fatalf("dev off: %d", w.Code)
	}
	// dev on but non-log messenger
	e = newTenv(t, func(c *Config) { c.Dev = true })
	if w := e.browser().do("GET", "/dev", nil); w.Code != 404 {
		t.Fatalf("fake messenger: %d", w.Code)
	}
	// dev on with the log messenger
	st, clk := testStore(t)
	cfg := Config{BaseURL: "http://ww.test", Messenger: "log", Dev: true}
	lm := &LogMessenger{}
	a := NewAuth(st, lm, clk, cfg.BaseURL)
	srv, _ := NewServer(cfg, st, a, lm, clk)
	a.RequestLogin(context.Background(), "+447700900123", "1.1.1.1", true)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/dev", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "/auth/") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestClientIPOnlyTrustsLoopbackProxy(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.9:1"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if clientIP(r) != "203.0.113.9" {
		t.Fatal("spoofed XFF trusted from non-proxy")
	}
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("X-Forwarded-For", "9.9.9.9, 198.51.100.7")
	if clientIP(r) != "198.51.100.7" {
		t.Fatalf("got %s", clientIP(r))
	}
}

var _ = time.Second
