package app

import (
	"errors"
	"strings"
)

// ErrBadPhone is returned for numbers that cannot be normalised to E.164.
var ErrBadPhone = errors.New("that does not look like a valid phone number")

// NormalisePhone converts user input to E.164. Numbers without a country
// prefix are treated as UK numbers (+44).
func NormalisePhone(in string) (string, error) {
	s := strings.TrimSpace(in)
	s = strings.TrimPrefix(strings.ToLower(s), "whatsapp:")
	if len(s) > 40 {
		return "", ErrBadPhone
	}
	var b strings.Builder
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && i == 0:
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '(' || r == ')' || r == '.' || r == ' ':
		default:
			return "", ErrBadPhone
		}
	}
	d := b.String()
	var digits string
	switch {
	case strings.HasPrefix(d, "+"):
		digits = d[1:]
	case strings.HasPrefix(d, "00"):
		digits = d[2:]
	case strings.HasPrefix(d, "0"):
		digits = "44" + d[1:]
	case strings.HasPrefix(d, "44") && len(d) >= 12:
		digits = d
	default:
		digits = "44" + d
	}
	// "+44 (0)7700 ..." style: drop the redundant trunk zero.
	if strings.HasPrefix(digits, "440") {
		digits = "44" + digits[3:]
	}
	if len(digits) < 8 || len(digits) > 15 || digits[0] == '0' {
		return "", ErrBadPhone
	}
	if strings.HasPrefix(digits, "44") {
		n := len(digits) - 2
		if n < 9 || n > 10 {
			return "", ErrBadPhone
		}
	}
	return "+" + digits, nil
}

// MaskPhone hides all but the last three digits, for logs.
func MaskPhone(p string) string {
	d := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, p)
	if len(d) <= 3 {
		return "***"
	}
	return "***" + d[len(d)-3:]
}
