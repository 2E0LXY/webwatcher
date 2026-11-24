package app

import (
	"net/url"
	"strings"
	"testing"
)

func TestManualPriceRecordedAndEvaluated(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	c := b.login("07700900123")
	b.do("POST", "/watches", url.Values{"csrf": {c}, "url": {"https://www.jet2holidays.com/x"}, "target": {"1000"}})
	post := func(price string) int {
		return b.do("POST", "/watches/1/price", url.Values{"csrf": {c}, "price": {price}}).Code
	}
	if !strings.Contains(b.do("GET", "/watches/1", nil).Body.String(), "Record a price I saw") {
		t.Fatal("form missing")
	}
	// first price, above target: recorded, no alert
	if code := post("1,249.50"); code != 303 {
		t.Fatalf("%d", code)
	}
	w, _ := e.st.WatchByID(1, 1)
	if *w.LastPricePence != 124950 || w.LastMethod != "manual" || *w.AlertBase != 124950 {
		t.Fatalf("%+v", w)
	}
	if outboxCount(e.st, ``) != 0 {
		t.Fatal("alert on first price above target")
	}
	// changed
	post("1199")
	if outboxCount(e.st, `WHERE kind='changed'`) != 1 {
		t.Fatalf("no changed alert: %d", outboxCount(e.st, ``))
	}
	// crosses the target
	post("950.00")
	if outboxCount(e.st, `WHERE kind='below'`) != 1 {
		t.Fatalf("no below-target alert")
	}
	hist, _ := e.st.History(1, 1, 10)
	if len(hist) != 3 || hist[0].Method != "manual" || hist[0].PricePence != 95000 {
		t.Fatalf("%+v", hist)
	}
	body := b.do("GET", "/watches/1", nil).Body.String()
	if !strings.Contains(body, "£950.00") || !strings.Contains(body, "manual") {
		t.Fatal("watch page does not show the manual price")
	}
}

func TestManualPriceValidationOwnershipAndCSRF(t *testing.T) {
	e := newTenv(t, nil)
	a, o := e.browser(), e.browser()
	ca := a.login("07700900123")
	co := o.login("07700900456")
	a.do("POST", "/watches", url.Values{"csrf": {ca}, "url": {"https://www.tui.co.uk/x"}})
	for _, bad := range []string{"", "abc", "0", "-5", "99999999999", strings.Repeat("9", 20)} {
		if code := a.do("POST", "/watches/1/price", url.Values{"csrf": {ca}, "price": {bad}}).Code; code != 400 {
			t.Errorf("%q: %d", bad, code)
		}
	}
	if code := o.do("POST", "/watches/1/price", url.Values{"csrf": {co}, "price": {"10"}}).Code; code != 404 {
		t.Errorf("other user: %d", code)
	}
	if code := a.do("POST", "/watches/1/price", url.Values{"price": {"10"}}).Code; code == 303 {
		t.Errorf("accepted without CSRF token")
	}
	if w, _ := e.st.WatchByID(1, 1); w.LastPricePence != nil {
		t.Fatal("a rejected request recorded a price")
	}
	// paused watches are not changed
	a.do("POST", "/watches/1/pause", url.Values{"csrf": {ca}})
	if code := a.do("POST", "/watches/1/price", url.Values{"csrf": {ca}, "price": {"10"}}).Code; code != 409 {
		t.Errorf("paused: %d", code)
	}
}

func TestManualPriceDoesNotHideABlock(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	c := b.login("07700900123")
	b.do("POST", "/watches", url.Values{"csrf": {c}, "url": {"https://www.jet2.com/x"}})
	e.st.RecordFailure(1, "blocked", "blocked by site (HTTP 403)", 2, e.clk.Now().Unix()+7200, nil, nil, CheckMeta{Class: ClassHTTP403})
	b.do("POST", "/watches/1/price", url.Values{"csrf": {c}, "price": {"500"}})
	w, _ := e.st.WatchByID(1, 1)
	if w.Status != "blocked" || w.Failures != 2 || w.NextCheckAt != e.clk.Now().Unix()+7200 || *w.LastPricePence != 50000 {
		t.Fatalf("%+v", w)
	}
}

func TestManualPriceInTargetCurrencyAndMismatch(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	c := b.login("07700900123")
	b.do("POST", "/watches", url.Values{"csrf": {c}, "url": {"https://www.easyjet.com/x"}, "target": {"100"}, "target_currency": {"EUR"}})
	b.do("POST", "/watches/1/price", url.Values{"csrf": {c}, "price": {"90"}})
	w, _ := e.st.WatchByID(1, 1)
	if w.Currency != "EUR" || w.CurrencyMismatch {
		t.Fatalf("%+v", w)
	}
	if outboxCount(e.st, `WHERE kind='below'`) != 1 {
		t.Fatal("EUR price against EUR target not evaluated")
	}
}
