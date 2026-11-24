package app

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func basisPage(price, basis, cur string) *Page {
	body := `<html><head><script type="application/ld+json">{"@type":"Product","offers":{"@type":"Offer","price":"` + price +
		`","priceCurrency":"` + cur + `","description":"` + basis + `"}}</script></head></html>`
	return &Page{Status: 200, Header: http.Header{}, Body: []byte(body)}
}

func TestBasisChangeFlagsAndSendsNoAlert(t *testing.T) {
	e := newSchedEnv(t)
	const u = "https://holiday.example.com/p"
	id := e.add(t, u, nil, 60)
	e.f.set(u, basisPage("1000", "per person", "GBP"), basisPage("2000", "total price", "GBP"), basisPage("2000", "total price", "GBP"))
	e.run(t, 0)
	e.run(t, time.Hour) // per person 1000 -> total 2000: unlike things
	w, _ := e.st.WatchByID(id, e.uid)
	if !w.BasisChanged || *w.LastPricePence != 100000 || w.Basis != BasisPerPerson || *w.ObsPence != 200000 || w.ObsBasis != BasisTotal {
		t.Fatalf("%+v", w)
	}
	if len(e.m.Sent()) != 0 || outboxCount(e.st, ``) != 0 {
		t.Fatal("alert sent across a basis change")
	}
	h, _ := e.st.History(e.uid, id, 10)
	if len(h) != 2 || h[0].Basis != BasisTotal || h[1].Basis != BasisPerPerson {
		t.Fatalf("observation not recorded with its basis: %+v", h)
	}
	e.run(t, time.Hour) // still different: still flagged, still silent
	if w, _ = e.st.WatchByID(id, e.uid); !w.BasisChanged || len(e.m.Sent()) != 0 {
		t.Fatal("flag lost or alert sent")
	}
	// confirm: baseline reset to the new observation, then comparisons resume
	if err := e.st.ConfirmBasis(e.uid, id); err != nil {
		t.Fatal(err)
	}
	w, _ = e.st.WatchByID(id, e.uid)
	if w.BasisChanged || w.Basis != BasisTotal || *w.LastPricePence != 200000 || w.AlertBase != nil {
		t.Fatalf("%+v", w)
	}
	if err := e.st.ConfirmBasis(e.uid, id); err != ErrNotFound {
		t.Fatalf("confirm without flag: %v", err)
	}
	e.f.set(u, basisPage("2000", "total price", "GBP"), basisPage("1800", "total price", "GBP"))
	e.run(t, time.Hour) // first check after the reset: becomes the baseline, no alert
	if len(e.m.Sent()) != 0 {
		t.Fatal("alert on the reset check")
	}
	e.run(t, time.Hour)
	if s := e.m.Sent(); len(s) != 1 || !contains(s[0].Text, "£2,000.00 → £1,800.00") {
		t.Fatalf("%+v", s)
	}
}

func TestCurrencyChangeFlagged(t *testing.T) {
	e := newSchedEnv(t)
	const u = "https://shop.example.com/p"
	id := e.add(t, u, nil, 60)
	e.f.set(u, basisPage("100", "", "GBP"), basisPage("120", "", "EUR"), basisPage("100", "", "GBP"))
	e.run(t, 0)
	e.run(t, time.Hour)
	w, _ := e.st.WatchByID(id, e.uid)
	if !w.BasisChanged || w.ObsCurrency != "EUR" || len(e.m.Sent()) != 0 {
		t.Fatalf("%+v", w)
	}
	e.run(t, time.Hour) // site reverts to GBP: flag clears by itself, same price, no alert
	w, _ = e.st.WatchByID(id, e.uid)
	if w.BasisChanged || len(e.m.Sent()) != 0 {
		t.Fatalf("%+v", w)
	}
}

func TestBasisChangeBelowTargetNoAlert(t *testing.T) {
	e := newSchedEnv(t)
	const u = "https://holiday.example.com/p"
	e.add(t, u, p(150000), 60)
	e.f.set(u, basisPage("2000", "total price", "GBP"), basisPage("900", "per person", "GBP"))
	e.run(t, 0)
	e.run(t, time.Hour) // 900 pp is under the 1500 target, but is not comparable to a total
	if len(e.m.Sent()) != 0 {
		t.Fatalf("%+v", e.m.Sent())
	}
}

func TestSoldOutKeepsLastPriceAndStaysSilent(t *testing.T) {
	e := newSchedEnv(t)
	const u = "https://shop.example.com/p"
	id := e.add(t, u, nil, 60)
	e.f.set(u, pricePage(5000), &Page{Status: 200, Header: http.Header{}, Body: fixture(t, "soldout.html")}, pricePage(4000))
	e.run(t, 0)
	e.run(t, time.Hour)
	w, _ := e.st.WatchByID(id, e.uid)
	if !w.SoldOut || *w.LastPricePence != 5000 || w.Failures != 0 || len(e.m.Sent()) != 0 {
		t.Fatalf("%+v", w)
	}
	e.run(t, time.Hour) // back in stock at a lower price: now it alerts against the kept baseline
	w, _ = e.st.WatchByID(id, e.uid)
	if w.SoldOut || len(e.m.Sent()) != 1 || !contains(e.m.Sent()[0].Text, "£50.00 → £40.00") {
		t.Fatalf("%+v %+v", w, e.m.Sent())
	}
}

func TestConfirmBasisHandlerAndDashboard(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	csrf := b.login("07700900123")
	b.do("POST", "/watches", url.Values{"csrf": {csrf}, "url": {"https://a.example.com/x"}})
	e.st.RecordObservation(1, Observation{Price: 100000, Currency: "GBP", Method: "jsonld", Basis: BasisPerPerson}, 0, nil, nil)
	e.st.RecordBasisChange(1, Observation{Price: 200000, Currency: "GBP", Method: "jsonld", Basis: BasisTotal}, 0)
	page := b.do("GET", "/", nil).Body.String()
	for _, s := range []string{"Basis changed", "Confirm new basis", "per person", "total"} {
		if !strings.Contains(page, s) {
			t.Errorf("dashboard missing %q", s)
		}
	}
	if w := b.do("POST", "/watches/1/confirm-basis", url.Values{"csrf": {csrf}}); w.Code != 303 {
		t.Fatalf("%d", w.Code)
	}
	w, _ := e.st.WatchByID(1, 1)
	if w.BasisChanged || w.Basis != BasisTotal {
		t.Fatalf("%+v", w)
	}
	if strings.Contains(b.do("GET", "/", nil).Body.String(), "Confirm new basis") {
		t.Fatal("button still shown")
	}
}
