package app

import "testing"

func TestExtractFixtures(t *testing.T) {
	cases := []struct {
		name, file string
		pence      int64
		cur, basis string
		method     string
		sold       bool
	}{
		{"european decimals and thousands", "eu_decimals.html", 129950, "EUR", BasisTotal, "regex", false},
		{"non-GBP dollars with commas", "usd_text.html", 124999, "USD", BasisUnknown, "regex", false},
		{"suffix euro", "suffix_eur.html", 89900, "EUR", BasisUnknown, "regex", false},
		{"was then now prefers now", "was_now.html", 79900, "GBP", BasisUnknown, "regex", false},
		{"now then (was) prefers now", "now_was_after.html", 79900, "GBP", BasisUnknown, "regex", false},
		{"from beats was and save", "from_was_save.html", 84900, "GBP", BasisPerPerson, "regex", false},
		{"multiple offers: cheapest in stock, same currency, own product only", "offers_multi.html", 1950, "GBP", BasisUnknown, "jsonld", false},
		{"all offers sold out", "soldout.html", 4900, "GBP", BasisUnknown, "jsonld", true},
		{"malformed JSON-LD falls back to meta", "jsonld_malformed.html", 5200, "GBP", BasisUnknown, "meta", false},
		{"malformed block then valid block", "jsonld_malformed_then_ok.html", 6125, "GBP", BasisUnknown, "jsonld", false},
		{"sale price beats list price, per person detected", "jsonld_pp.html", 99900, "GBP", BasisPerPerson, "jsonld", false},
		{"total wording", "total_text.html", 259800, "GBP", BasisTotal, "regex", false},
		{"pp wording", "pp_text.html", 129900, "GBP", BasisPerPerson, "regex", false},
		{"jsonld with comma price", "jsonld.html", 124950, "GBP", BasisUnknown, "jsonld", false},
	}
	for _, c := range cases {
		r, err := Extract(fixture(t, c.file), "")
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if r.PricePence != c.pence || r.Currency != c.cur || r.Basis != c.basis || r.Method != c.method || r.SoldOut != c.sold {
			t.Errorf("%s: got %+v want %d %s %s %s sold=%v", c.name, *r, c.pence, c.cur, c.basis, c.method, c.sold)
		}
	}
}

func TestParseAmountSeparators(t *testing.T) {
	cases := map[string]int64{"1.299": 129900, "€1.299,50": 129950, "12.345,6": 1234560, "0.999": 100, "1,299.00": 129900, "7,5": 750}
	for in, want := range cases {
		if got, ok := parseAmount(in); !ok || got != want {
			t.Errorf("%q: got %d,%v want %d", in, got, ok, want)
		}
	}
}

func TestSelectorBasis(t *testing.T) {
	r, err := Extract([]byte(`<div class="x">£500 per person</div>`), ".x")
	if err != nil || r.Basis != BasisPerPerson {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestBasisFromText(t *testing.T) {
	for in, want := range map[string]string{
		"per person": BasisPerPerson, "PP": BasisPerPerson, "Total price": BasisTotal, "for 4 adults": BasisTotal,
		"nothing": BasisUnknown, "£999 pp (£1,998 total)": BasisPerPerson, "total £1,998 (£999 per person)": BasisTotal,
	} {
		if got := basisFromText(in); got != want {
			t.Errorf("%q: %s want %s", in, got, want)
		}
	}
}
