// Package agent is the Webwatcher Home Agent: it loads pages in the owner's
// own browser from the owner's own connection and reports what it rendered to
// the Webwatcher server. It never tries to get round bot protection: when it
// meets a challenge it reports it and backs off.
package agent

import (
	"net/url"
	"strings"
)

// strongMarkers appear on interstitial and block pages, not on ordinary pages,
// so they are safe to look for in a page that loaded with a 2xx status.
var strongMarkers = []string{
	"cf-browser-verification", "cf-chl-", "challenge-platform", "just a moment...",
	"attention required! | cloudflare", "_incapsula_resource", "incapsula incident",
	"px-captcha", "captcha-delivery.com", "pardon our interruption", "errors.edgesuite.net",
	"/_sec/cp_challenge", "are you a robot", "verify you are human", "cf-turnstile",
	"press & hold to confirm", "access to this page has been denied", "please verify you are a human",
}

// titleMarkers are matched only against the page title.
var titleMarkers = []string{"access denied", "attention required", "just a moment", "pardon our interruption", "robot check", "are you a human"}

// maxChallengeBody is the size above which a 2xx page is taken to be a real page.
// Interstitials are small; a large page that merely mentions a vendor is not one.
const maxChallengeBody = 200 << 10

// DetectChallenge reports whether a loaded page is a bot challenge or an access
// denial. status is the HTTP status of the main document (0 if unknown).
func DetectChallenge(status int, html string) bool {
	if status == 403 || status == 429 {
		return true
	}
	if status == 503 {
		return anyMarker(strings.ToLower(html)) || titleMatches(html)
	}
	if status >= 400 {
		return false // other errors are reported as errors, not as a challenge
	}
	if len(html) > maxChallengeBody {
		return false
	}
	return anyMarker(strings.ToLower(html)) || titleMatches(html)
}

func anyMarker(lower string) bool {
	for _, m := range strongMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

func titleMatches(html string) bool {
	l := strings.ToLower(html)
	i := strings.Index(l, "<title")
	if i < 0 {
		return false
	}
	j := strings.Index(l[i:], ">")
	if j < 0 {
		return false
	}
	rest := l[i+j+1:]
	k := strings.Index(rest, "</title>")
	if k < 0 || k > 300 {
		return false
	}
	title := strings.TrimSpace(rest[:k])
	for _, m := range titleMarkers {
		if strings.Contains(title, m) {
			return true
		}
	}
	return false
}

// AllowedURL reports whether the agent may load raw: http and https only, with a host.
// file:, chrome:, javascript:, data:, about:, blob:, ftp: and the rest are refused.
func AllowedURL(raw string) bool {
	if strings.TrimSpace(raw) != raw || raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// HostOf returns the lower-case hostname of a URL, or "".
func HostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// SafeURL is a URL fit for a log line: scheme, host and path, with no query,
// fragment or credentials.
func SafeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "[unparseable]"
	}
	p := u.EscapedPath()
	if len(p) > 80 {
		p = p[:80] + "..."
	}
	return u.Scheme + "://" + u.Hostname() + p
}

// ClassifyNavError maps a browser navigation error to a failure_class the server understands.
func ClassifyNavError(msg string) string {
	m := strings.ToUpper(msg)
	switch {
	case strings.Contains(m, "ERR_NAME_NOT_RESOLVED"), strings.Contains(m, "ERR_NAME_RESOLUTION_FAILED"), strings.Contains(m, "ERR_DNS"):
		return "dns"
	case strings.Contains(m, "ERR_CERT"), strings.Contains(m, "ERR_SSL"), strings.Contains(m, "ERR_BAD_SSL"), strings.Contains(m, "ERR_TLS"):
		return "tls"
	case strings.Contains(m, "ERR_CONNECTION"), strings.Contains(m, "ERR_ADDRESS_UNREACHABLE"), strings.Contains(m, "ERR_INTERNET_DISCONNECTED"),
		strings.Contains(m, "ERR_NETWORK_CHANGED"), strings.Contains(m, "ERR_PROXY"), strings.Contains(m, "ERR_TUNNEL"):
		return "connect"
	case strings.Contains(m, "ERR_TIMED_OUT"), strings.Contains(m, "DEADLINE EXCEEDED"), strings.Contains(m, "TIMEOUT"):
		return "timeout"
	}
	return "nav_error"
}
