package app

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
)

var sensitiveKeyRe = regexp.MustCompile(`(?i)(token|key|session|booking|ref|auth|secret|passw|signature|sig$|sid$|jwt|otp|pnr|reservation|credential)`)
var opaqueValueRe = regexp.MustCompile(`^[A-Za-z0-9_\-+/=.]{32,}$`)

// SensitiveQueryKeys returns the names (never the values) of query parameters
// that look like tokens, session ids or booking references. Webwatcher stores
// the URL and sends it to the site, so the user is asked to confirm first.
func SensitiveQueryKeys(rawURL string) []string {
	u, err := url.Parse(rawURL)
	if err != nil || u.RawQuery == "" {
		return nil
	}
	seen := map[string]bool{}
	for k, vals := range u.Query() {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "keyword") {
			continue
		}
		hit := sensitiveKeyRe.MatchString(k)
		for _, v := range vals {
			if opaqueValueRe.MatchString(v) {
				hit = true
			}
		}
		if hit {
			seen[k] = true
		}
	}
	var out []string
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
