package app

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

const dashPhone = "+447700900123"

// dashEnv is a scheduler environment on the dashboard messenger. Its user was
// created the way adduser creates one under that messenger: no WhatsApp consent.
func dashEnv(t *testing.T) *schedEnv {
	e := newSchedEnv(t)
	e.st.UseDashboardAlerts(true)
	e.snd = NewSender(e.st, &DashboardMessenger{}, e.clk, 200)
	e.uid = dashUser(t, e.st, "+447700900999")
	return e
}

func dashUser(t *testing.T, st *Store, phone string) int64 {
	t.Helper()
	_, h := newToken()
	if err := st.CreateDashboardLoginToken(phone, h, time.Minute); err != nil {
		t.Fatal(err)
	}
	uid, _, err := st.ConsumeLoginToken(h)
	if err != nil {
		t.Fatal(err)
	}
	return uid
}

func notifs(t *testing.T, st *Store, uid int64) []*Notification {
	t.Helper()
	ns, err := st.Notifications(uid, 500)
	if err != nil {
		t.Fatal(err)
	}
	return ns
}

func TestDashboardNotificationOnChangeAndBelowTarget(t *testing.T) {
	e := dashEnv(t)
	const u = "https://shop.example.com/p?token=SECRETTOKEN"
	id := e.add(t, u, p(10000), 60)
	e.f.set(u, pricePage(12000), pricePage(11000), pricePage(9900), pricePage(9900))
	e.run(t, 0)
	if len(notifs(t, e.st, e.uid)) != 0 {
		t.Fatal("notification on first check above target")
	}
	e.run(t, time.Hour) // 12000 -> 11000
	e.run(t, time.Hour) // 11000 -> 9900: crosses target
	e.run(t, time.Hour) // unchanged
	ns := notifs(t, e.st, e.uid)
	if len(ns) != 2 {
		t.Fatalf("got %d notifications", len(ns))
	}
	below, changed := ns[0], ns[1] // newest first
	if below.Kind != NotifBelow || *below.Price != 9900 || *below.Old != 11000 || *below.Target != 10000 || below.Currency != "GBP" {
		t.Fatalf("below: %+v", below)
	}
	if changed.Kind != NotifChanged || *changed.Price != 11000 || *changed.Old != 12000 || changed.WatchID == nil || *changed.WatchID != id {
		t.Fatalf("changed: %+v", changed)
	}
	for _, n := range ns {
		if strings.Contains(n.Title+n.Body, "SECRETTOKEN") || strings.Contains(n.Title+n.Body, "http") || n.ReadAt != nil {
			t.Fatalf("body leaks or is read: %+v", n)
		}
	}
	if len(e.m.Sent()) != 0 {
		t.Fatal("something was sent over a network messenger")
	}
	var st string
	e.st.db.QueryRow(`SELECT GROUP_CONCAT(DISTINCT status) FROM outbox`).Scan(&st)
	var logged int
	e.st.db.QueryRow(`SELECT COUNT(*) FROM send_log`).Scan(&logged)
	if st != OutAccepted || logged != 0 {
		t.Fatalf("outbox %q send_log %d", st, logged)
	}
}

func TestDashboardNoNotificationForStaleOrDeletedWatch(t *testing.T) {
	cases := map[string]func(e *schedEnv, id int64){
		"paused":  func(e *schedEnv, id int64) { e.st.SetPaused(e.uid, id, true) },
		"edited":  func(e *schedEnv, id int64) { e.st.SetTarget(e.uid, id, p(5000)) },
		"deleted": func(e *schedEnv, id int64) { e.st.DeleteWatch(e.uid, id) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := dashEnv(t)
			id := e.add(t, "https://shop.example.com/p", nil, 60)
			if err := e.st.RecordSuccess(id, 9000, "GBP", "meta", 0, nil, qa(AlertChanged, 9000)); err != nil {
				t.Fatal(err)
			}
			mutate(e, id)
			e.snd.RunOnce(context.Background())
			if n := len(notifs(t, e.st, e.uid)); n != 0 {
				t.Fatalf("%d notifications for a %s watch", n, name)
			}
		})
	}
}

func TestDashboardDuplicateEventIsDeduped(t *testing.T) {
	e := dashEnv(t)
	id := e.add(t, "https://shop.example.com/p", nil, 60)
	for i := 0; i < 3; i++ {
		e.st.RecordSuccess(id, 9000, "GBP", "meta", 0, nil, qa(AlertChanged, 9000))
	}
	e.snd.RunOnce(context.Background())
	if n := len(notifs(t, e.st, e.uid)); n != 1 {
		t.Fatalf("%d notifications", n)
	}
}

func TestDashboardAlertsOffSuppressed(t *testing.T) {
	e := dashEnv(t)
	id := e.add(t, "https://shop.example.com/p", nil, 60)
	e.st.RecordSuccess(id, 9000, "GBP", "meta", 0, nil, qa(AlertChanged, 9000))
	e.st.db.Exec(`UPDATE users SET alerts_enabled=0 WHERE id=?`, e.uid)
	e.snd.RunOnce(context.Background())
	if len(notifs(t, e.st, e.uid)) != 0 {
		t.Fatal("notification for a user with alerts off")
	}
}

func TestNotificationsCapAndPurge(t *testing.T) {
	e := dashEnv(t)
	other := dashUser(t, e.st, "+447700900888")
	id := e.add(t, "https://shop.example.com/p", nil, 60)
	for i := 0; i < MaxNotificationsPerUser+15; i++ {
		e.st.RecordSuccess(id, int64(1000+i), "GBP", "meta", 0, nil, qa(AlertChanged, int64(1000+i)))
		e.snd.RunOnce(context.Background())
	}
	ns := notifs(t, e.st, e.uid)
	if len(ns) != MaxNotificationsPerUser || *ns[0].Price != int64(1000+MaxNotificationsPerUser+14) {
		t.Fatalf("len %d newest %v", len(ns), *ns[0].Price)
	}
	e.st.db.Exec(`INSERT INTO notifications(user_id,kind,title,body,created_at) VALUES(?,?,?,?,?)`, other, NotifSystem, "t", "b", e.clk.Now().Unix())
	// age everything of the first user past the retention
	e.clk.Advance(100 * 24 * time.Hour)
	e.st.db.Exec(`INSERT INTO notifications(user_id,kind,title,body,created_at) VALUES(?,?,?,?,?)`, e.uid, NotifSystem, "fresh", "b", e.clk.Now().Unix())
	if err := e.st.Purge(90, 30); err != nil {
		t.Fatal(err)
	}
	if ns := notifs(t, e.st, e.uid); len(ns) != 1 || ns[0].Title != "fresh" {
		t.Fatalf("after purge: %d", len(ns))
	}
	if len(notifs(t, e.st, other)) != 0 {
		t.Fatal("old notification of the other user kept")
	}
}

func TestDeleteWatchKeepsNotificationAndDeleteUserPurges(t *testing.T) {
	e := dashEnv(t)
	id := e.add(t, "https://shop.example.com/p", nil, 60)
	e.st.RecordSuccess(id, 9000, "GBP", "meta", 0, nil, qa(AlertChanged, 9000))
	e.snd.RunOnce(context.Background())
	e.st.DeleteWatch(e.uid, id)
	ns := notifs(t, e.st, e.uid)
	if len(ns) != 1 || ns[0].WatchID != nil {
		t.Fatalf("%+v", ns)
	}
	if err := e.st.DeleteUser(e.uid); err != nil {
		t.Fatal(err)
	}
	var n int
	e.st.db.QueryRow(`SELECT COUNT(*) FROM notifications`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d notifications survived the account", n)
	}
}

func TestSwitchingMessengerKeepsNotificationsAndNeedsWhatsAppConsent(t *testing.T) {
	e := dashEnv(t)
	id := e.add(t, "https://shop.example.com/p", nil, 60)
	e.st.RecordSuccess(id, 9000, "GBP", "meta", 0, nil, qa(AlertChanged, 9000))
	e.snd.RunOnce(context.Background())
	// Switch to a WhatsApp messenger: same database, WhatsApp rules.
	e.st.UseDashboardAlerts(false)
	if n := len(notifs(t, e.st, e.uid)); n != 1 {
		t.Fatalf("lost notifications: %d", n)
	}
	w, _ := e.st.WatchByID(id, e.uid)
	if w.AlertsOn {
		t.Fatal("a dashboard-only user would be sent WhatsApp messages without consent")
	}
	var src string
	e.st.db.QueryRow(`SELECT consent_source FROM users WHERE id=?`, e.uid).Scan(&src)
	if src != ConsentDashboard {
		t.Fatalf("consent_source %q", src)
	}
}

// ---- HTTP ----

func dashTenv(t *testing.T) *tenv {
	e := newTenv(t, func(c *Config) { c.Messenger = "dashboard"; c.BaseURL = "https://ww.test" })
	e.st.UseDashboardAlerts(true)
	return e
}

// dashLogin logs in with an operator-created link, as the owner would hand out.
func dashLogin(b *browser, phone string) (csrf string, uid int64) {
	t := b.e.t
	raw, h := newToken()
	if err := b.e.st.CreateDashboardLoginToken(phone, h, time.Minute); err != nil {
		t.Fatal(err)
	}
	path := "/auth/" + raw
	c := b.csrf(path)
	if w := b.do("POST", path, url.Values{"csrf": {c}}); w.Code != 303 {
		t.Fatalf("auth: %d", w.Code)
	}
	b.e.st.db.QueryRow(`SELECT id FROM users WHERE phone=?`, phone).Scan(&uid)
	return b.csrf("/"), uid
}

func addNotif(t *testing.T, st *Store, uid int64, watch *int64, title string) int64 {
	t.Helper()
	res, err := st.db.Exec(`INSERT INTO notifications(user_id,watch_id,kind,title,body,price_pence,currency,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		uid, watch, NotifChanged, title, "price changed £10.00 → £9.00", 900, "GBP", st.now())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestNotificationsPageBadgeBannerAndMarkRead(t *testing.T) {
	e := dashTenv(t)
	b := e.browser()
	csrf, uid := dashLogin(b, dashPhone)
	wid, _ := e.st.AddWatch(Watch{UserID: uid, URL: "https://shop.example.com/p", Label: "Kettle", IntervalMin: 60})
	n1 := addNotif(t, e.st, uid, &wid, "Kettle")
	e.clk.Advance(time.Minute)
	n2 := addNotif(t, e.st, uid, &wid, "Toaster")

	home := b.do("GET", "/", nil).Body.String()
	if !strings.Contains(home, `id="unread">2<`) || !strings.Contains(home, "Toaster") || !strings.Contains(home, "All notifications") {
		t.Fatalf("badge or banner missing:\n%s", home)
	}
	if strings.Contains(home, "WhatsApp alerts") {
		t.Error("WhatsApp card shown in dashboard mode")
	}
	page := b.do("GET", "/notifications", nil).Body.String()
	if i, j := strings.Index(page, "Toaster"), strings.Index(page, "Kettle"); i < 0 || j < 0 || i > j {
		t.Fatalf("not newest first")
	}
	if !strings.Contains(page, fmt.Sprintf(`href="/watches/%d"`, wid)) {
		t.Fatal("no link to the watch")
	}
	if w := b.do("GET", "/notifications/count", nil); w.Body.String() != "2" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("count %q %q", w.Body.String(), w.Header().Get("Content-Type"))
	}
	// CSRF is required
	if w := b.do("POST", fmt.Sprintf("/notifications/%d/read", n1), url.Values{"csrf": {"wrong"}}); w.Code != 403 {
		t.Fatalf("csrf: %d", w.Code)
	}
	if w := b.do("POST", fmt.Sprintf("/notifications/%d/read", n1), url.Values{"csrf": {csrf}}); w.Code != 303 {
		t.Fatalf("mark read: %d", w.Code)
	}
	if n, _ := e.st.UnreadCount(uid); n != 1 {
		t.Fatalf("unread %d", n)
	}
	if w := b.do("POST", "/notifications/read-all", url.Values{"csrf": {"x"}}); w.Code != 403 {
		t.Fatalf("read-all csrf: %d", w.Code)
	}
	b.do("POST", "/notifications/read-all", url.Values{"csrf": {csrf}})
	if n, _ := e.st.UnreadCount(uid); n != 0 {
		t.Fatalf("unread %d", n)
	}
	_ = n2
	if home := b.do("GET", "/", nil).Body.String(); strings.Contains(home, "All notifications") || !strings.Contains(home, `id="unread" hidden`) {
		t.Fatal("banner or badge still shown with nothing unread")
	}
}

func TestNotificationOwnershipCrossAccount404(t *testing.T) {
	e := dashTenv(t)
	a, bb := e.browser(), e.browser()
	_, ua := dashLogin(a, dashPhone)
	csrfB, ub := dashLogin(bb, "+447700900555")
	na := addNotif(t, e.st, ua, nil, "A's alert")
	nb := addNotif(t, e.st, ub, nil, "B's alert")
	if w := bb.do("POST", fmt.Sprintf("/notifications/%d/read", na), url.Values{"csrf": {csrfB}}); w.Code != 404 {
		t.Fatalf("cross-account mark read: %d", w.Code)
	}
	if n, _ := e.st.UnreadCount(ua); n != 1 {
		t.Fatal("another account's notification was marked read")
	}
	if w := bb.do("POST", "/notifications/99999/read", url.Values{"csrf": {csrfB}}); w.Code != 404 {
		t.Fatalf("missing: %d", w.Code)
	}
	if w := bb.do("POST", "/notifications/abc/read", url.Values{"csrf": {csrfB}}); w.Code != 404 {
		t.Fatalf("bad id: %d", w.Code)
	}
	if strings.Contains(bb.do("GET", "/notifications", nil).Body.String(), "A's alert") {
		t.Fatal("list shows another account's notification")
	}
	// read-all touches only the caller's
	bb.do("POST", "/notifications/read-all", url.Values{"csrf": {csrfB}})
	if n, _ := e.st.UnreadCount(ua); n != 1 {
		t.Fatal("read-all crossed accounts")
	}
	if n, _ := e.st.UnreadCount(ub); n != 0 {
		t.Fatal("own notification not read")
	}
	_ = nb
	// anonymous
	if w := e.browser().do("GET", "/notifications", nil); w.Code != 303 {
		t.Fatalf("anon: %d", w.Code)
	}
	if w := e.browser().do("GET", "/notifications/count", nil); w.Code != 303 {
		t.Fatalf("anon count: %d", w.Code)
	}
}

func TestDashboardLoginIdenticalForKnownAndUnknown(t *testing.T) {
	e := dashTenv(t)
	dashUser(t, e.st, dashPhone) // a known user
	get := e.browser().do("GET", "/login", nil)
	if get.Code != 200 || !strings.Contains(get.Body.String(), "Ask the administrator for a login link.") {
		t.Fatalf("login page: %d %s", get.Code, get.Body.String())
	}
	if strings.Contains(get.Body.String(), "<form method=\"post\" action=\"/login\"") || strings.Contains(strings.ToLower(get.Body.String()), "whatsapp") {
		t.Fatal("login page promises WhatsApp or offers a send form")
	}
	var first string
	for i, phone := range []string{dashPhone, "07700 900123", "+447911123456", "garbage", ""} {
		w := e.browser().do("POST", "/login", url.Values{"phone": {phone}})
		if w.Code != 200 {
			t.Fatalf("%q: %d", phone, w.Code)
		}
		if i == 0 {
			first = w.Body.String()
		} else if w.Body.String() != first {
			t.Fatalf("response differs for %q", phone)
		}
	}
	if first != get.Body.String() {
		t.Fatal("POST response differs from the page")
	}
	e.srv.Wait()
	if len(e.msgr.Sent()) != 0 {
		t.Fatal("a message was sent")
	}
	var n int
	e.st.db.QueryRow(`SELECT COUNT(*) FROM login_tokens`).Scan(&n)
	if n != 1 { // only the one created by dashUser
		t.Fatalf("login tokens: %d", n)
	}
}

func TestWhatsAppLoginFormUnchanged(t *testing.T) {
	e := newTenv(t, nil)
	body := e.browser().do("GET", "/login", nil).Body.String()
	if !strings.Contains(body, `name="consent"`) || !strings.Contains(body, "WhatsApp number") {
		t.Fatal("WhatsApp login form changed")
	}
}

func TestDeleteAccountViaHTTPPurgesNotifications(t *testing.T) {
	e := dashTenv(t)
	b := e.browser()
	csrf, uid := dashLogin(b, dashPhone)
	addNotif(t, e.st, uid, nil, "x")
	if w := b.do("POST", "/account/delete", url.Values{"csrf": {csrf}, "confirm": {"DELETE"}}); w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
	var n int
	e.st.db.QueryRow(`SELECT COUNT(*) FROM notifications`).Scan(&n)
	if n != 0 {
		t.Fatal("notifications survived")
	}
}

// ---- CLI, config, doctor ----

func TestLoginLinkCLI(t *testing.T) {
	env, dir := adduserEnv(t)
	env["WW_MESSENGER"] = "dashboard"
	var out bytes.Buffer
	if err := LoginLink(envMap(env), "07700 900123", &out); err == nil || out.Len() != 0 {
		t.Fatalf("unknown user accepted: %v %q", err, out.String())
	}
	if err := AddUser(envMap(env), "07700 900123", &out); err != nil {
		t.Fatal(err)
	}
	// the adduser link logs the user in; then loginlink issues another
	raw := strings.Fields(out.String()[strings.Index(out.String(), "https://"):])[0]
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := NewStore(db, SystemClock())
	a := NewAuth(st, &DashboardMessenger{}, SystemClock(), "https://watch.mydomain.uk")
	if _, _, err := a.Consume(strings.TrimPrefix(raw, "https://watch.mydomain.uk/auth/")); err != nil {
		t.Fatal(err)
	}
	db.Close()
	out.Reset()
	if err := LoginLink(envMap(env), "+44 7700 900123", &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "https://watch.mydomain.uk/auth/") || strings.Contains(s, "7700900123") || !strings.Contains(s, "15 minutes") {
		t.Fatalf("output: %q", s)
	}
	raw2 := strings.TrimPrefix(strings.Fields(s[strings.Index(s, "https://"):])[0], "https://watch.mydomain.uk/auth/")
	db, _ = OpenDB(dir)
	defer db.Close()
	st = NewStore(db, SystemClock())
	a = NewAuth(st, &DashboardMessenger{}, SystemClock(), "https://watch.mydomain.uk")
	if _, _, err := a.Consume(raw2); err != nil {
		t.Fatalf("link does not work: %v", err)
	}
	if _, _, err := a.Consume(raw2); err == nil {
		t.Fatal("link worked twice")
	}
	var src string
	var consent *int64
	db.QueryRow(`SELECT consent_source, consent_at FROM users`).Scan(&src, &consent)
	if src != ConsentDashboard || consent != nil {
		t.Fatalf("source %q consent %v", src, consent)
	}
	// Outside dashboard mode the alias records admin-created like adduser.
	env2, _ := adduserEnv(t)
	out.Reset()
	AddUser(envMap(env2), "07700 900123", &out)
	{
		db2, _ := OpenDB(env2["WW_DIR"])
		st2 := NewStore(db2, SystemClock())
		raw3 := strings.TrimPrefix(strings.Fields(out.String()[strings.Index(out.String(), "https://"):])[0], "https://watch.mydomain.uk/auth/")
		if _, _, err := NewAuth(st2, &fakeMessenger{}, SystemClock(), "https://x").Consume(raw3); err != nil {
			t.Fatal(err)
		}
		db2.Close()
	}
	var o2 bytes.Buffer
	if err := LoginLink(envMap(env2), "07700 900123", &o2); err != nil || !strings.Contains(o2.String(), ConsentAdminCreated) {
		t.Fatalf("%v %q", err, o2.String())
	}
}

func TestConfigDashboardMessenger(t *testing.T) {
	env := func(m map[string]string) func(string) string { return envMap(m) }
	c, err := LoadConfig(env(map[string]string{"WW_MESSENGER": "dashboard", "WW_BASE_URL": "https://watch.mydomain.uk"}))
	if err != nil || c.Messenger != "dashboard" || NewMessenger(c).Name() != "dashboard" {
		t.Fatalf("%v %+v", err, c)
	}
	if _, err := LoadConfig(env(map[string]string{"WW_MESSENGER": "dashboard", "WW_BASE_URL": "http://watch.mydomain.uk"})); err == nil {
		t.Error("dashboard over plain http accepted without WW_DEV=1")
	}
	// log is still development only
	if _, err := LoadConfig(env(map[string]string{"WW_MESSENGER": "log", "WW_BASE_URL": "https://watch.mydomain.uk"})); err == nil {
		t.Error("log accepted in production")
	}
	if _, err := LoadConfig(env(map[string]string{"WW_MESSENGER": "log", "WW_BASE_URL": "http://127.0.0.1:8780"})); err == nil {
		t.Error("log accepted without WW_DEV=1")
	}
	if _, err := LoadConfig(env(map[string]string{"WW_MESSENGER": "log", "WW_BASE_URL": "http://127.0.0.1:8780", "WW_DEV": "1"})); err != nil {
		t.Error(err)
	}
}

func TestDoctorDashboard(t *testing.T) {
	env := map[string]string{"WW_DIR": goodDir(t), "WW_BASE_URL": "https://watch.mydomain.uk", "WW_MESSENGER": "dashboard",
		"WW_ADMIN_TOKEN": secAdmin, "WW_TWILIO_TOKEN": secTwilio, "WW_WA_TOKEN": secMeta}
	out, fails := runDoctor(env)
	if !strings.Contains(out, "OK   messenger: dashboard") || !strings.Contains(out, "WARN alerts are visible only inside the dashboard until a WhatsApp provider is configured") {
		t.Fatalf("output:\n%s", out)
	}
	if !strings.Contains(out, "the service would start") {
		t.Fatalf("service would not start:\n%s", out)
	}
	_ = fails
	for _, s := range []string{secAdmin, secTwilio, secMeta} {
		if strings.Contains(out, s) {
			t.Errorf("leaked %q", s)
		}
	}
	if strings.Contains(out, "webhook") {
		t.Error("webhook URLs printed for the dashboard messenger")
	}
}
