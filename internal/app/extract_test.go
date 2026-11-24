package app

import (
	"errors"
	"os"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseAmount(t *testing.T) {
	cases := map[string]int64{
		"£1,249.50": 124950, "99": 9900, "99.9": 9990, "1.299,50": 129950, "12,5": 1250,
		"1,299": 129900, "0.99": 99, "1299.999": 130000, "19.99 GBP": 1999, "1,234,567.89": 123456789,
	}
	for in, want := range cases {
		if got, ok := parseAmount(in); !ok || got != want {
			t.Errorf("%q: got %d,%v want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "abc", "0", "0.00", "free", "99999999999999"} {
		if _, ok := parseAmount(in); ok {
			t.Errorf("%q should fail", in)
		}
	}
}

func TestExtractMethods(t *testing.T) {
	cases := []struct {
		file, sel   string
		pence       int64
		cur, method string
	}{
		{"jsonld.html", "", 124950, "GBP", "jsonld"}, // beats og:price and body text
		{"jsonld_aggregate.html", "", 8999, "EUR", "jsonld"},
		{"meta_itemprop.html", "", 4210, "GBP", "meta"}, // itemprop beats og
		{"meta_og.html", "", 19999, "GBP", "meta"},
		{"meta_product.html", "", 1250, "USD", "meta"},
		{"regex.html", "", 89900, "GBP", "regex"}, // "from £899", not Was/Save/Deposit/£50/script
		{"selector.html", "div#main section.card .row > span.amt", 24999, "GBP", "selector"},
		{"selector.html", "span[data-price]", 30000, "GBP", "selector"},
		{"selector.html", ".card.promo .amt", 1000, "GBP", "selector"},
		{"selector.html", `[data-currency="GBP"]`, 24999, "GBP", "selector"},
	}
	for _, c := range cases {
		r, err := Extract(fixture(t, c.file), c.sel)
		if err != nil {
			t.Errorf("%s %q: %v", c.file, c.sel, err)
			continue
		}
		if r.PricePence != c.pence || r.Currency != c.cur || r.Method != c.method {
			t.Errorf("%s %q: got %+v want %d %s %s", c.file, c.sel, *r, c.pence, c.cur, c.method)
		}
	}
}

func TestSelectorPrecedenceAndNoFallback(t *testing.T) {
	// A selector is used even when JSON-LD exists...
	r, err := Extract(fixture(t, "selector.html"), ".alt")
	if err != nil || r.Method != "selector" || r.PricePence != 30000 {
		t.Fatalf("%+v %v", r, err)
	}
	// ...and a failing selector does not fall back to JSON-LD.
	_, err = Extract(fixture(t, "selector.html"), ".nope")
	var ee *ExtractError
	if !errors.As(err, &ee) || ee.Reason != "selector matched nothing" {
		t.Fatalf("got %v", err)
	}
	if _, err = Extract(fixture(t, "selector.html"), "div:hover"); err == nil {
		t.Fatal("pseudo-class accepted")
	}
}

func TestExtractFailures(t *testing.T) {
	_, err := Extract(fixture(t, "noprice.html"), "")
	var ee *ExtractError
	if !errors.As(err, &ee) || ee.Reason != "no price found" || ee.BrowserNeeded {
		t.Fatalf("noprice: %v", err)
	}
	_, err = Extract(fixture(t, "spa.html"), "")
	if !errors.As(err, &ee) || !ee.BrowserNeeded {
		t.Fatalf("spa: %v", err)
	}
}

func TestParseSelector(t *testing.T) {
	good := []string{"div", ".a", "#b", "div.a.b#c", "a > b c", "[x]", "[x=y]", `[x^="a b"]`, "*", "span[data-a$=z].q"}
	for _, s := range good {
		if _, err := ParseSelector(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	bad := []string{"", "  ", ">", "a >", "a > > b", "a,b", "a:hover", "a::before", "[x", "[x=", ".", "#", "a+b", "a~b", `[x="y]`}
	for _, s := range bad {
		if _, err := ParseSelector(s); err == nil {
			t.Errorf("%q should fail", s)
		}
	}
}

func TestCheckPageClassification(t *testing.T) {
	hdr := func() map[string][]string { return map[string][]string{} }
	cases := []struct {
		name   string
		page   Page
		status string
		reason string
	}{
		{"403", Page{Status: 403, Header: hdr()}, "blocked", "blocked by site (HTTP 403)"},
		{"429", Page{Status: 429, Header: hdr()}, "blocked", "blocked by site (HTTP 429)"},
		{"cloudflare 200", Page{Status: 200, Header: hdr(), Body: fixture(t, "cloudflare.html")}, "blocked", "blocked by site (bot challenge page)"},
		{"spa", Page{Status: 200, Header: hdr(), Body: fixture(t, "spa.html")}, "noprice", "needs browser rendering (the page builds its prices with JavaScript)"},
		{"no price", Page{Status: 200, Header: hdr(), Body: fixture(t, "noprice.html")}, "noprice", "no price found"},
		{"500", Page{Status: 500, Header: hdr()}, "error", "site returned HTTP 500"},
		{"404", Page{Status: 404, Header: hdr()}, "error", "site returned HTTP 404"},
	}
	for _, c := range cases {
		p := c.page
		_, f := CheckPage(&p, "")
		if f == nil || f.Status != c.status || f.Reason != c.reason {
			t.Errorf("%s: got %+v", c.name, f)
		}
	}
	p := Page{Status: 200, Header: hdr(), Body: fixture(t, "jsonld.html")}
	if r, f := CheckPage(&p, ""); f != nil || r.Method != "jsonld" {
		t.Fatalf("%+v %+v", r, f)
	}
}
