package app

import (
	"fmt"
	"net/url"
	"strings"
)

const (
	AlertChanged = "changed"
	AlertBelow   = "below"
	AlertFailed  = "failed"
)

// AlertInput is everything the alert rules need.
type AlertInput struct {
	Prev   *int64 // price at the alert baseline (nil on the first check, or after the target or basis was reset)
	Cur    int64
	Target *int64
}

// EvaluateAlert applies the alert rules. It returns "", AlertChanged or AlertBelow.
// Alerts fire on transitions, never because of a price seen before:
//
//   - below: a target is set and the price crosses from above it to at or
//     under it (or this is the first check after the baseline was reset, which
//     happens on the first check ever and whenever the target is edited). It
//     supersedes "changed", so one price move produces one message. A later
//     recovery above the target re-arms it.
//   - changed: the price differs from the baseline by at least a penny.
//
// The baseline moves on every check, so an unchanged price never repeats an alert.
func EvaluateAlert(in AlertInput) string {
	if in.Target != nil && in.Cur <= *in.Target && (in.Prev == nil || *in.Prev > *in.Target) {
		return AlertBelow
	}
	if in.Prev != nil && in.Cur != *in.Prev {
		return AlertChanged
	}
	return ""
}

// FormatPence renders pence as a currency string, e.g. £1,249.50.
func FormatPence(p int64, cur string) string {
	sym := map[string]string{"GBP": "£", "EUR": "€", "USD": "$"}[cur]
	if sym == "" {
		sym = cur + " "
	}
	neg := ""
	if p < 0 {
		neg, p = "-", -p
	}
	pounds := fmt.Sprintf("%d", p/100)
	var parts []string
	for len(pounds) > 3 {
		parts = append([]string{pounds[len(pounds)-3:]}, parts...)
		pounds = pounds[:len(pounds)-3]
	}
	parts = append([]string{pounds}, parts...)
	return fmt.Sprintf("%s%s%s.%02d", neg, sym, strings.Join(parts, ","), p%100)
}

// plainParam makes a string safe as a WhatsApp template parameter (no
// newlines, tabs or runs of spaces) and bounds its length.
func plainParam(s string, max int) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, s)), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

func watchLabel(w *Watch) string {
	if w.Label != "" {
		return plainParam(w.Label, 60)
	}
	if u, err := url.Parse(w.URL); err == nil && u.Hostname() != "" {
		return plainParam(u.Hostname(), 60)
	}
	return "your watch"
}

// BuildAlertMessage builds the short plain-text message and template params.
func BuildAlertMessage(kind string, w *Watch, old *int64, cur int64, reason, baseURL string) Message {
	label := watchLabel(w)
	var summary string
	switch kind {
	case AlertBelow:
		summary = fmt.Sprintf("below target! Now %s (target %s)", FormatPence(cur, w.Currency), FormatPence(*w.TargetPence, w.Currency))
		if old != nil {
			summary += fmt.Sprintf(", was %s", FormatPence(*old, w.Currency))
		}
	case AlertChanged:
		summary = fmt.Sprintf("price changed %s → %s", FormatPence(*old, w.Currency), FormatPence(cur, w.Currency))
	case AlertFailed:
		summary = fmt.Sprintf("checking stopped after %d failed attempts (%s)", MaxFailures, plainParam(reason, 100))
	}
	page := plainParam(w.URL, 300)
	dash := strings.TrimRight(baseURL, "/") + "/"
	text := fmt.Sprintf("Webwatcher: %s: %s\n%s\nDashboard: %s", label, summary, page, dash)
	return Message{Kind: KindAlert, To: w.Phone, Text: text, Params: []string{label, summary, page, dash}}
}
