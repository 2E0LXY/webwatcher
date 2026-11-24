package app

import (
	"strings"
	"testing"
)

func p(v int64) *int64 { return &v }

func TestEvaluateAlert(t *testing.T) {
	cases := []struct {
		name string
		in   AlertInput
		want string
	}{
		{"first check no target", AlertInput{Cur: 10000}, ""},
		{"first check below target", AlertInput{Cur: 9000, Target: p(10000)}, AlertBelow},
		{"first check above target", AlertInput{Cur: 11000, Target: p(10000)}, ""},
		{"first check exactly at target", AlertInput{Cur: 10000, Target: p(10000)}, AlertBelow},
		{"unchanged", AlertInput{Prev: p(10000), Cur: 10000}, ""},
		{"one penny up", AlertInput{Prev: p(10000), Cur: 10001}, AlertChanged},
		{"one penny down", AlertInput{Prev: p(10000), Cur: 9999}, AlertChanged},
		{"drop crossing target", AlertInput{Prev: p(12000), Cur: 9900, Target: p(10000)}, AlertBelow},
		{"drop but still above target", AlertInput{Prev: p(12000), Cur: 11000, Target: p(10000)}, AlertChanged},
		{"already below, falls further", AlertInput{Prev: p(9900), Cur: 9500, Target: p(10000)}, AlertChanged},
		{"already below, unchanged", AlertInput{Prev: p(9900), Cur: 9900, Target: p(10000)}, ""},
		{"rise above target", AlertInput{Prev: p(9900), Cur: 10500, Target: p(10000)}, AlertChanged},
		{"dedupe below same price", AlertInput{Cur: 9000, Target: p(10000), LastBelow: p(9000)}, ""},
		{"below different price than last below", AlertInput{Prev: p(12000), Cur: 9000, Target: p(10000), LastBelow: p(9500)}, AlertBelow},
		{"dedupe changed same price", AlertInput{Prev: p(9000), Cur: 10000, LastChanged: p(10000)}, ""},
		{"below dedupe does not fall through to changed", AlertInput{Prev: p(12000), Cur: 9000, Target: p(10000), LastBelow: p(9000)}, ""},
	}
	for _, c := range cases {
		if got := EvaluateAlert(c.in); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestFormatPence(t *testing.T) {
	cases := map[int64]string{5: "£0.05", 9900: "£99.00", 124950: "£1,249.50", 123456789: "£1,234,567.89"}
	for in, want := range cases {
		if got := FormatPence(in, "GBP"); got != want {
			t.Errorf("%d: %s want %s", in, got, want)
		}
	}
	if FormatPence(1000, "EUR") != "€10.00" || FormatPence(1000, "CHF") != "CHF 10.00" {
		t.Error("currency symbols")
	}
}

func TestBuildAlertMessage(t *testing.T) {
	w := &Watch{URL: "https://www.jet2holidays.com/x?a=1", Label: "Majorca\nweek  in July", Currency: "GBP", TargetPence: p(100000), Phone: "+447700900123"}
	m := BuildAlertMessage(AlertBelow, w, p(120000), 99000, "", "https://ww.example/")
	if m.To != "+447700900123" || m.Kind != KindAlert || len(m.Params) != 4 {
		t.Fatalf("%+v", m)
	}
	for _, s := range []string{"Majorca week in July", "£990.00", "£1,000.00", "£1,200.00", "below target", "https://www.jet2holidays.com/x?a=1", "https://ww.example/"} {
		if !strings.Contains(m.Text, s) {
			t.Errorf("text lacks %q: %s", s, m.Text)
		}
	}
	for _, prm := range m.Params {
		if strings.ContainsAny(prm, "\n\t") {
			t.Errorf("param has control chars: %q", prm)
		}
	}
	c := BuildAlertMessage(AlertChanged, w, p(120000), 110000, "", "https://ww.example")
	if !strings.Contains(c.Text, "£1,200.00 → £1,100.00") {
		t.Errorf("changed text: %s", c.Text)
	}
	f := BuildAlertMessage(AlertFailed, &Watch{URL: "https://a.example.com/"}, nil, 0, "blocked by site (HTTP 403)", "https://ww.example")
	if !strings.Contains(f.Text, "a.example.com") || !strings.Contains(f.Text, "stopped") {
		t.Errorf("failed text: %s", f.Text)
	}
}
