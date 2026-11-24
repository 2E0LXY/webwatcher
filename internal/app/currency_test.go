package app

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func curPage(pence int64, cur string) *Page {
	return &Page{Status: 200, Header: http.Header{}, Body: []byte(fmt.Sprintf(
		`<html><div itemprop="offers"><meta itemprop="priceCurrency" content="%s"><meta itemprop="price" content="%d.%02d"></div></html>`, cur, pence/100, pence%100))}
}

func TestTargetCurrencyDefaultsToGBP(t *testing.T) {
	e := newSchedEnv(t)
	id := e.add(t, "https://shop.example.com/p", p(10000), 60)
	w, _ := e.st.WatchByID(id, e.uid)
	if w.TargetCurrency != "GBP" || w.CurrencyMismatch {
		t.Fatalf("%+v", w)
	}
}

func TestCurrencyMismatchSkipsBelowTargetAndIsFlagged(t *testing.T) {
	e := newSchedEnv(t)
	const u = "https://shop.example.com/p"
	id := e.add(t, u, p(10000), 60) // GBP target
	e.f.set(u, curPage(9000, "EUR"), curPage(8000, "EUR"))
	e.run(t, 0)
	e.run(t, time.Hour)
	for _, m := range e.m.Sent() { // a plain change alert is fine; a below-target one is not
		if contains(m.Text, "below target") {
			t.Fatalf("below-target alert sent for EUR against a GBP target: %q", m.Text)
		}
	}
	before := len(e.m.Sent())
	w, _ := e.st.WatchByID(id, e.uid)
	if !w.CurrencyMismatch {
		t.Fatal("watch not flagged")
	}
	v := (&Server{}).view(w)
	if !v.CurrencyMismatch || v.StatusTxt != "Currency mismatch" || !strings.Contains(v.MismatchNote, "GBP") || !strings.Contains(v.MismatchNote, "EUR") {
		t.Fatalf("%+v", v)
	}
	// the user sets the target in euros: the flag clears and the target is evaluated again
	if err := e.st.SetTargetCurrency(e.uid, id, "EUR"); err != nil {
		t.Fatal(err)
	}
	e.f.set(u, curPage(7000, "EUR"))
	e.run(t, time.Hour)
	w, _ = e.st.WatchByID(id, e.uid)
	if w.CurrencyMismatch {
		t.Fatal("flag not cleared")
	}
	if s := e.m.Sent(); len(s) != before+1 || !contains(s[len(s)-1].Text, "below target") {
		t.Fatalf("%+v", s)
	}
}

func TestMatchingCurrencyEvaluatesAsBefore(t *testing.T) {
	e := newSchedEnv(t)
	const u = "https://shop.example.com/p"
	e.add(t, u, p(10000), 60)
	e.f.set(u, curPage(9000, "GBP"))
	e.run(t, 0)
	if s := e.m.Sent(); len(s) != 1 || !contains(s[0].Text, "below target") {
		t.Fatalf("%+v", s)
	}
}

func TestTargetCurrencyThroughTheForms(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	c := b.login("07700900123")
	b.do("POST", "/watches", url.Values{"csrf": {c}, "url": {"https://a.example.com/x"}, "target": {"100"}, "target_currency": {"EUR"}})
	b.do("POST", "/watches", url.Values{"csrf": {c}, "url": {"https://a.example.com/y"}, "target": {"100"}, "target_currency": {"XXX"}})
	w1, _ := e.st.WatchByID(1, 1)
	w2, _ := e.st.WatchByID(2, 1)
	if w1.TargetCurrency != "EUR" || w2.TargetCurrency != "GBP" {
		t.Fatalf("%s %s", w1.TargetCurrency, w2.TargetCurrency)
	}
	b.do("POST", "/watches/2/edit", url.Values{"csrf": {c}, "target": {"100"}, "interval": {"60"}, "target_currency": {"USD"}})
	if w2, _ = e.st.WatchByID(2, 1); w2.TargetCurrency != "USD" {
		t.Fatalf("edit: %s", w2.TargetCurrency)
	}
	// the mismatch shows on the dashboard and the watch page
	e.st.db.Exec(`UPDATE watches SET currency_mismatch=1, status='ok' WHERE id=1`)
	for _, path := range []string{"/", "/watches/1"} {
		if body := b.do("GET", path, nil).Body.String(); !strings.Contains(body, "Currency mismatch") {
			t.Errorf("%s lacks the currency mismatch flag", path)
		}
	}
}
