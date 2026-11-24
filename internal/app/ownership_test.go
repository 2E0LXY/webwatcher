package app

import (
	"net/url"
	"strings"
	"testing"
)

func TestLogoutEverywhere(t *testing.T) {
	e := newTenv(t, nil)
	laptop, phone, other := e.browser(), e.browser(), e.browser()
	cl := laptop.login("07700900123")
	phone.login("07700900123")
	other.login("07700900456")
	if n := countRows(e, `SELECT COUNT(*) FROM sessions WHERE user_id=1`); n != 2 {
		t.Fatalf("sessions %d", n)
	}
	if !strings.Contains(laptop.do("GET", "/account", nil).Body.String(), "Log out everywhere") {
		t.Fatal("button missing")
	}
	if w := laptop.do("POST", "/account/logout-everywhere", url.Values{}); w.Code != 403 {
		t.Fatalf("csrf not required: %d", w.Code)
	}
	if w := laptop.do("POST", "/account/logout-everywhere", url.Values{"csrf": {cl}}); w.Code != 303 {
		t.Fatalf("%d", w.Code)
	}
	for name, b := range map[string]*browser{"laptop": laptop, "phone": phone} {
		if w := b.do("GET", "/", nil); w.Code != 303 || w.Header().Get("Location") != "/login" {
			t.Errorf("%s session survived: %d", name, w.Code)
		}
	}
	if w := other.do("GET", "/", nil); w.Code != 200 {
		t.Fatalf("another account's session was revoked: %d", w.Code)
	}
	if n := countRows(e, `SELECT COUNT(*) FROM sessions WHERE user_id=1`); n != 0 {
		t.Fatalf("sessions left: %d", n)
	}
}

func countRows(e *tenv, q string) int {
	var n int
	e.st.db.QueryRow(q).Scan(&n)
	return n
}

// Every handler that takes a watch id must answer 404 for another account's
// watch, for a missing one and for a malformed id, and must change nothing.
func TestCrossAccountEveryWatchHandler(t *testing.T) {
	e := newTenv(t, nil)
	owner, intruder := e.browser(), e.browser()
	co := owner.login("07700900123")
	ci := intruder.login("07700900456")
	owner.do("POST", "/watches", url.Values{"csrf": {co}, "url": {"https://a.example.com/x"}, "target": {"100"}, "label": {"mine"}})
	e.st.RecordObservation(1, Observation{Price: 100000, Currency: "GBP", Method: "m", Basis: BasisPerPerson}, 0, nil, nil)
	e.st.RecordBasisChange(1, Observation{Price: 200000, Currency: "GBP", Method: "m", Basis: BasisTotal}, 0)
	before, _ := e.st.WatchByID(1, 1)

	type req struct{ method, path string }
	handlers := []req{
		{"GET", "/watches/%s"},
		{"POST", "/watches/%s/edit"},
		{"POST", "/watches/%s/pause"},
		{"POST", "/watches/%s/resume"},
		{"POST", "/watches/%s/confirm-basis"},
		{"POST", "/watches/%s/delete"},
	}
	for _, id := range []string{"1", "999", "abc", "-1", "1x"} {
		for _, h := range handlers {
			path := strings.Replace(h.path, "%s", id, 1)
			var form url.Values
			if h.method == "POST" {
				form = url.Values{"csrf": {ci}, "target": {"1"}, "interval": {"30"}}
			}
			w := intruder.do(h.method, path, form)
			if w.Code != 404 {
				t.Errorf("%s %s as other account: %d, want 404", h.method, path, w.Code)
			}
			if strings.Contains(w.Body.String(), "mine") || strings.Contains(w.Body.String(), "a.example.com") {
				t.Errorf("%s %s leaked watch data", h.method, path)
			}
		}
	}
	// nothing changed
	after, err := e.st.WatchByID(1, 1)
	if err != nil {
		t.Fatal("watch deleted by another account")
	}
	if *after.TargetPence != *before.TargetPence || after.IntervalMin != before.IntervalMin || after.Paused ||
		!after.BasisChanged || after.Version != before.Version {
		t.Fatalf("watch modified: %+v", after)
	}
	// the owner still can
	for _, h := range handlers[:5] {
		path := strings.Replace(h.path, "%s", "1", 1)
		var form url.Values
		if h.method == "POST" {
			form = url.Values{"csrf": {co}, "target": {"90"}, "interval": {"60"}}
		}
		if w := owner.do(h.method, path, form); w.Code == 404 || w.Code >= 500 {
			t.Errorf("owner %s %s: %d", h.method, path, w.Code)
		}
	}
	// logged-out requests never reach a handler
	anon := e.browser()
	for _, h := range handlers {
		if w := anon.do(h.method, strings.Replace(h.path, "%s", "1", 1), url.Values{"csrf": {"x"}}); w.Code != 303 {
			t.Errorf("anonymous %s %s: %d", h.method, h.path, w.Code)
		}
	}
}

func TestStoreLevelOwnership(t *testing.T) {
	st, _ := testStore(t)
	a := mustUser(t, st, "+447700900123")
	b := mustUser(t, st, "+447700900456")
	id, _ := st.AddWatch(Watch{UserID: a, URL: "https://a.example.com/", IntervalMin: 60})
	st.RecordSuccess(id, 100, "GBP", "m", 0, nil, nil)
	if _, err := st.WatchByID(id, b); err != ErrNotFound {
		t.Error("WatchByID")
	}
	if h, _ := st.History(b, id, 10); len(h) != 0 {
		t.Error("History leaked another account's rows")
	}
	if h, _ := st.History(a, id, 10); len(h) != 1 {
		t.Error("owner history")
	}
	for name, err := range map[string]error{
		"SetTarget":    st.SetTarget(b, id, nil),
		"SetPaused":    st.SetPaused(b, id, true),
		"SetInterval":  st.SetInterval(b, id, 30),
		"DeleteWatch":  st.DeleteWatch(b, id),
		"ConfirmBasis": st.ConfirmBasis(b, id),
	} {
		if err != ErrNotFound {
			t.Errorf("%s by another account: %v", name, err)
		}
	}
	if ws, _ := st.ListWatches(b); len(ws) != 0 {
		t.Error("ListWatches")
	}
}

func TestAlertToggleOnlyAffectsOwnAccount(t *testing.T) {
	e := newTenv(t, nil)
	a, b := e.browser(), e.browser()
	ca := a.login("07700900123")
	b.login("07700900456")
	a.do("POST", "/account/alerts", url.Values{"csrf": {ca}, "enabled": {"0"}})
	if alertsOn(e, 1) || !alertsOn(e, 2) {
		t.Fatal("toggle crossed accounts")
	}
}
