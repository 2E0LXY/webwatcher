package app

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

type adminEnv struct {
	e                  *tenv
	admin, other       *browser
	adminID, otherID   int64
	adminTok, otherTok string
}

func newAdminEnv(t *testing.T, mut func(*Config)) *adminEnv {
	e := newTenv(t, mut)
	a := &adminEnv{e: e}
	a.adminTok, a.adminID = plink(t, e, "+447700900001")
	a.otherTok, a.otherID = plink(t, e, "+447700900002")
	if err := e.st.SetAdmin(a.adminID, true); err != nil {
		t.Fatal(err)
	}
	a.admin, a.other = e.browser(), e.browser()
	a.admin.plogin(a.adminTok)
	a.other.plogin(a.otherTok)
	return a
}

func TestAdminRoutesNotFoundForNonAdmins(t *testing.T) {
	a := newAdminEnv(t, nil)
	c := a.other.csrf("/")
	routes := []struct{ m, p string }{
		{"GET", "/admin/users"}, {"POST", "/admin/users/add"}, {"POST", "/admin/users/1/link"}, {"POST", "/admin/users/1/revoke-link"},
		{"POST", "/admin/users/1/disable"}, {"POST", "/admin/users/1/enable"}, {"POST", "/admin/users/1/delete"},
	}
	anon := a.e.browser()
	unk := anon.do("GET", "/nope", nil)
	for _, r := range routes {
		for name, b := range map[string]*browser{"user": a.other, "anon": anon} {
			form := url.Values{"csrf": {c}, "phone": {"+447700900099"}, "confirm": {"1"}}
			w := b.do(r.m, r.p, form)
			if w.Code != 404 || w.Body.String() != unk.Body.String() {
				t.Errorf("%s %s %s: %d", name, r.m, r.p, w.Code)
			}
		}
	}
	// nothing changed
	if n, _ := a.e.st.AdminUsers(); len(n) != 2 {
		t.Fatal("accounts changed")
	}
	if info, _ := a.e.st.PersonalLinkStatus(a.adminID); !info.Exists {
		t.Fatal("admin link touched")
	}
	// the page does not leak other accounts to normal users
	if body := a.other.do("GET", "/", nil).Body.String(); strings.Contains(body, "447700900001") || strings.Contains(body, "/admin/users") {
		t.Fatal("dashboard leaks admin info")
	}
}

func TestAdminCSRFEnforced(t *testing.T) {
	a := newAdminEnv(t, nil)
	for _, p := range []string{"/admin/users/add", "/admin/users/2/link", "/admin/users/2/revoke-link", "/admin/users/2/disable", "/admin/users/2/enable", "/admin/users/2/delete"} {
		w := a.admin.do("POST", p, url.Values{"phone": {"+447700900099"}, "confirm": {"1"}})
		if w.Code != 403 {
			t.Errorf("%s without csrf: %d", p, w.Code)
		}
		w = a.admin.do("POST", p, url.Values{"csrf": {"wrong"}, "phone": {"+447700900099"}, "confirm": {"1"}})
		if w.Code != 403 {
			t.Errorf("%s bad csrf: %d", p, w.Code)
		}
	}
	if us, _ := a.e.st.AdminUsers(); len(us) != 2 || us[1].Disabled {
		t.Fatal("state changed")
	}
}

func TestAdminAddAccountAndCap(t *testing.T) {
	a := newAdminEnv(t, func(c *Config) { c.MaxUsers = 3 })
	c := a.admin.csrf("/admin/users")
	if w := a.admin.do("POST", "/admin/users/add", url.Values{"csrf": {c}, "phone": {"nonsense"}}); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := a.admin.do("POST", "/admin/users/add", url.Values{"csrf": {c}, "phone": {"07700 900123"}, "name": {"  Dad\x00  "}}); w.Code != 303 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	us, _ := a.e.st.AdminUsers()
	last := us[len(us)-1]
	if last.Phone != "+447700900123" || last.Name != "Dad" || last.IsAdmin {
		t.Fatalf("%+v", last)
	}
	if w := a.admin.do("POST", "/admin/users/add", url.Values{"csrf": {c}, "phone": {"+447700900123"}}); w.Code != 409 {
		t.Fatal("duplicate", w.Code)
	}
	w := a.admin.do("POST", "/admin/users/add", url.Values{"csrf": {c}, "phone": {"+447700900124"}})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "limit of 3") {
		t.Fatalf("cap: %d", w.Code)
	}
	if n, _ := a.e.st.AdminUsers(); len(n) != 3 {
		t.Fatal(len(n))
	}
	page := a.admin.do("GET", "/admin/users", nil).Body.String()
	if strings.Contains(page, "447700900123") || !strings.Contains(page, "Dad") {
		t.Fatal("number not masked or name missing", page)
	}
}

func TestAdminLinkShownOnceNotLogged(t *testing.T) {
	a := newAdminEnv(t, nil)
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	c := a.admin.csrf("/admin/users")
	w := a.admin.do("POST", "/admin/users/"+itoa(a.otherID)+"/link", url.Values{"csrf": {c}})
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	i := strings.Index(w.Body.String(), "http://ww.test/p/")
	if i < 0 {
		t.Fatal(w.Body.String())
	}
	tok := w.Body.String()[i+len("http://ww.test/p/"):]
	tok = tok[:43]
	// the new link works for the other account; the old one is rotated out
	if a.e.browser().plogin(a.otherTok).Code != 404 {
		t.Fatal("old link still valid")
	}
	b := a.e.browser()
	if b.plogin(tok).Code != 303 {
		t.Fatal("new link invalid")
	}
	// never again on the list page or in logs
	if strings.Contains(a.admin.do("GET", "/admin/users", nil).Body.String(), tok) {
		t.Fatal("token shown again")
	}
	if strings.Contains(buf.String(), tok) {
		t.Fatal("token logged")
	}
	// the stored value is a hash
	var n int
	a.e.st.db.QueryRow(`SELECT COUNT(*) FROM personal_links WHERE token_hash=?`, tok).Scan(&n)
	if n != 0 {
		t.Fatal("raw token stored")
	}
	// cannot mint one's own link
	if w := a.admin.do("POST", "/admin/users/"+itoa(a.adminID)+"/link", url.Values{"csrf": {c}}); w.Code != 403 {
		t.Fatal("self link", w.Code)
	}
	if a.e.browser().plogin(a.adminTok).Code != 303 {
		t.Fatal("admin link was changed")
	}
	// revoke
	if w := a.admin.do("POST", "/admin/users/"+itoa(a.otherID)+"/revoke-link", url.Values{"csrf": {c}}); w.Code != 303 {
		t.Fatal(w.Code)
	}
	if a.e.browser().plogin(tok).Code != 404 {
		t.Fatal("revoked link works")
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

func TestAdminDisableBlocksLoginAndScheduler(t *testing.T) {
	a := newAdminEnv(t, nil)
	if _, err := a.e.st.AddWatch(Watch{UserID: a.otherID, URL: "https://a.example/x", IntervalMin: 60}); err != nil {
		t.Fatal(err)
	}
	a.e.clk.Advance(time.Hour)
	if d, _ := a.e.st.DueWatches(10); len(d) != 1 {
		t.Fatalf("due %d", len(d))
	}
	c := a.admin.csrf("/admin/users")
	if w := a.admin.do("POST", "/admin/users/"+itoa(a.otherID)+"/disable", url.Values{"csrf": {c}}); w.Code != 303 {
		t.Fatal(w.Code)
	}
	if a.other.do("GET", "/", nil).Code != 303 {
		t.Fatal("session survived disable")
	}
	if a.e.browser().plogin(a.otherTok).Code != 404 {
		t.Fatal("permanent link works while disabled")
	}
	// one-time link too
	raw, h := newToken()
	a.e.st.CreateAdminLoginToken("+447700900002", h, time.Hour)
	if _, _, err := a.e.srv.auth.Consume(raw); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("magic link on disabled account: %v", err)
	}
	if d, _ := a.e.st.DueWatches(10); len(d) != 0 {
		t.Fatal("scheduler would check a disabled account's watch")
	}
	// enable restores everything
	a.admin.do("POST", "/admin/users/"+itoa(a.otherID)+"/enable", url.Values{"csrf": {c}})
	if a.e.browser().plogin(a.otherTok).Code != 303 {
		t.Fatal("link dead after enable")
	}
	if d, _ := a.e.st.DueWatches(10); len(d) != 1 {
		t.Fatal("watch not due after enable")
	}
}

func TestAdminSelfAndLastAdminProtected(t *testing.T) {
	a := newAdminEnv(t, nil)
	c := a.admin.csrf("/admin/users")
	id := itoa(a.adminID)
	if w := a.admin.do("POST", "/admin/users/"+id+"/disable", url.Values{"csrf": {c}}); w.Code != 403 {
		t.Fatal("self disable", w.Code)
	}
	if w := a.admin.do("POST", "/admin/users/"+id+"/delete", url.Values{"csrf": {c}, "confirm": {"1"}}); w.Code != 403 {
		t.Fatal("self delete", w.Code)
	}
	if a.admin.do("GET", "/admin/users", nil).Code != 200 {
		t.Fatal("admin lost access")
	}
	// store-level guard for the last enabled admin
	if err := a.e.st.SetDisabled(a.adminID, true); !errors.Is(err, ErrLastAdmin) {
		t.Fatal(err)
	}
	if err := a.e.st.DeleteUserGuarded(a.adminID); !errors.Is(err, ErrLastAdmin) {
		t.Fatal(err)
	}
	if err := a.e.st.SetAdmin(a.adminID, false); !errors.Is(err, ErrLastAdmin) {
		t.Fatal(err)
	}
	// with two admins one may go, but then the other is the last
	a.e.st.SetAdmin(a.otherID, true)
	if err := a.e.st.SetDisabled(a.otherID, true); err != nil {
		t.Fatal(err)
	}
	if err := a.e.st.SetDisabled(a.adminID, true); !errors.Is(err, ErrLastAdmin) {
		t.Fatal("disabled admins must not count", err)
	}
}

func TestAdminDeletePurgesAndAudit(t *testing.T) {
	a := newAdminEnv(t, nil)
	a.e.st.AddWatch(Watch{UserID: a.otherID, URL: "https://a.example/x", IntervalMin: 60})
	c := a.admin.csrf("/admin/users")
	id := itoa(a.otherID)
	if w := a.admin.do("POST", "/admin/users/"+id+"/delete", url.Values{"csrf": {c}}); w.Code != 400 {
		t.Fatal("no confirm", w.Code)
	}
	if w := a.admin.do("POST", "/admin/users/"+id+"/delete", url.Values{"csrf": {c}, "confirm": {"1"}}); w.Code != 303 {
		t.Fatal(w.Code)
	}
	for _, q := range []string{`SELECT COUNT(*) FROM users WHERE id=?`, `SELECT COUNT(*) FROM watches WHERE user_id=?`, `SELECT COUNT(*) FROM sessions WHERE user_id=?`, `SELECT COUNT(*) FROM personal_links WHERE user_id=?`} {
		var n int
		a.e.st.db.QueryRow(q, a.otherID).Scan(&n)
		if n != 0 {
			t.Errorf("%s left %d", q, n)
		}
	}
	if a.e.browser().plogin(a.otherTok).Code != 404 {
		t.Fatal("link survived")
	}
	// unknown ids give 404
	if w := a.admin.do("POST", "/admin/users/9999/delete", url.Values{"csrf": {c}, "confirm": {"1"}}); w.Code != 404 {
		t.Fatal(w.Code)
	}
	// audit
	a.admin.do("POST", "/admin/users/add", url.Values{"csrf": {c}, "phone": {"+447700900300"}})
	evs, _ := a.e.st.AdminEvents(50)
	if len(evs) != 2 || evs[0].Action != ActAddAccount || evs[1].Action != ActDeleteAccount || evs[1].Target != MaskPhone("+447700900002") || evs[0].Actor != MaskPhone("+447700900001") {
		t.Fatalf("%+v", evs)
	}
	page := a.admin.do("GET", "/admin/users", nil).Body.String()
	if !strings.Contains(page, "delete-account") || !strings.Contains(page, "add-account") || strings.Contains(page, "7700900300") {
		t.Fatal("audit log not shown or unmasked")
	}
}

func TestAdminIsolationBetweenAccounts(t *testing.T) {
	a := newAdminEnv(t, nil)
	idA, _ := a.e.st.AddWatch(Watch{UserID: a.adminID, URL: "https://a.example/x", IntervalMin: 60})
	if w := a.other.do("GET", "/watches/"+itoa(idA), nil); w.Code != 404 {
		t.Fatal("other user can view admin's watch", w.Code)
	}
	body := a.other.do("GET", "/", nil).Body.String()
	if strings.Contains(body, "a.example") || strings.Contains(body, "7700900001") {
		t.Fatal("dashboard leaks another account")
	}
	// being admin is read from the database each request: demoted admins lose access at once
	a.e.st.SetAdmin(a.otherID, true)
	if a.other.do("GET", "/admin/users", nil).Code != 200 {
		t.Fatal("role is read live: promotion should apply to the existing session")
	}
	a.e.st.SetAdmin(a.otherID, false)
	if a.other.do("GET", "/admin/users", nil).Code != 404 {
		t.Fatal("demoted admin kept access")
	}
}

func TestSetAdminCLI(t *testing.T) {
	env, dir := adduserEnv(t)
	var out bytes.Buffer
	if err := SetAdmin(envMap(env), "07700 900123", false, &out); err != nil || !strings.Contains(out.String(), "is an admin") {
		t.Fatal(err, out.String())
	}
	db, _ := OpenDB(dir)
	defer db.Close()
	st := NewStore(db, SystemClock())
	uid, err := st.UserID("+447700900123")
	if err != nil || !st.IsAdmin(uid) {
		t.Fatal("not admin / not created", err)
	}
	var src string
	db.QueryRow(`SELECT consent_source FROM users WHERE id=?`, uid).Scan(&src)
	if src != ConsentAdminCreated {
		t.Fatal(src)
	}
	// last admin cannot be revoked
	if err := SetAdmin(envMap(env), "+447700900123", true, &out); !errors.Is(err, ErrLastAdmin) {
		t.Fatal(err)
	}
	if !st.IsAdmin(uid) {
		t.Fatal("last admin removed")
	}
	// with a second admin it can
	if err := SetAdmin(envMap(env), "+447700900124", false, &out); err != nil {
		t.Fatal(err)
	}
	if err := SetAdmin(envMap(env), "+447700900123", true, &out); err != nil || st.IsAdmin(uid) {
		t.Fatal("revoke failed", err)
	}
	// revoke for an unknown number
	if err := SetAdmin(envMap(env), "+447700900999", true, &out); err == nil {
		t.Fatal("revoke for unknown user succeeded")
	}
	// --admin on permalink and adduser
	if err := Permalink(envMap(env), "+447700900125", PermalinkOpts{Admin: true}, &out); err != nil {
		t.Fatal(err)
	}
	u3, _ := st.UserID("+447700900125")
	if !st.IsAdmin(u3) {
		t.Fatal("permalink --admin")
	}
	if err := AddUserAdmin(envMap(env), "+447700900126", &out); err != nil {
		t.Fatal(err)
	}
	u4, _ := st.UserID("+447700900126")
	if !st.IsAdmin(u4) {
		t.Fatal("adduser --admin")
	}
	_ = http.StatusOK
}
