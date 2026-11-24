package agent

import (
	"strings"
	"testing"
)

func TestDetectChallenge(t *testing.T) {
	big := "<html>" + strings.Repeat("x", maxChallengeBody) + " px-captcha</html>"
	cases := []struct {
		name   string
		status int
		html   string
		want   bool
	}{
		{"403 is a block", 403, "<html>nothing</html>", true},
		{"429 is a block", 429, "", true},
		{"503 plain is an error not a challenge", 503, "<html>maintenance</html>", false},
		{"503 with cloudflare marker", 503, `<title>Just a moment...</title>`, true},
		{"500 is an error", 500, "<html>oops</html>", false},
		{"404 is an error", 404, "<html>gone</html>", false},
		{"200 ordinary page", 200, `<html><title>Majorca holidays</title><body>From £499 pp</body></html>`, false},
		{"200 cloudflare interstitial", 200, `<html><title>Just a moment...</title><div id="cf-chl-widget"></div></html>`, true},
		{"200 cf turnstile", 200, `<div class="cf-turnstile"></div>`, true},
		{"200 akamai access denied title", 200, `<html><head><title>Access Denied</title></head><body>You don't have permission</body></html>`, true},
		{"200 akamai errors.edgesuite", 200, `<html>Reference #18.1234 https://errors.edgesuite.net/18.1234</html>`, true},
		{"200 akamai bot manager sec challenge", 200, `<script src="/_sec/cp_challenge/ak-challenge-3-4.htm"></script>`, true},
		{"200 perimeterx", 200, `<div id="px-captcha"></div>`, true},
		{"200 perimeterx press and hold", 200, `<p>Press & Hold to confirm you are a human</p>`, true},
		{"200 datadome", 200, `<script src="https://geo.captcha-delivery.com/captcha/"></script>`, true},
		{"200 incapsula", 200, `<iframe src="/_Incapsula_Resource?SWUDNSAI=31"></iframe>`, true},
		{"200 imperva interruption", 200, `<title>Pardon Our Interruption</title>`, true},
		{"200 robot wording", 200, `<h1>Are you a robot?</h1>`, true},
		{"200 big page mentioning a vendor is real", 200, big, false},
		{"200 page title mentioning access denied far from title is fine", 200, `<html><title>Holiday</title><body>Your card was denied: access denied for 3DS</body></html>`, false},
		{"unknown status 0 with real page", 0, `<html><body>£100</body></html>`, false},
		{"unknown status 0 with marker", 0, `<html>px-captcha</html>`, true},
	}
	for _, c := range cases {
		if got := DetectChallenge(c.status, c.html); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestAllowedURL(t *testing.T) {
	ok := []string{"https://www.jet2.com/holidays", "http://example.com/", "https://www.tui.co.uk/x?a=1#f", "HTTPS://EXAMPLE.COM/"}
	bad := []string{"", "file:///etc/passwd", "file://localhost/c:/windows", "chrome://settings", "chrome-extension://abc/x", "javascript:alert(1)", "data:text/html,<p>x</p>",
		"about:blank", "blob:https://x/uuid", "ftp://example.com/", "view-source:https://example.com", "//example.com/", "example.com", "https://", "https://user:pw@example.com/", " https://example.com/", "https://example.com/ ", "ws://example.com/", "mailto:a@b.c", "devtools://devtools/bundled/x.html"}
	for _, u := range ok {
		if !AllowedURL(u) {
			t.Errorf("refused %q", u)
		}
	}
	for _, u := range bad {
		if AllowedURL(u) {
			t.Errorf("allowed %q", u)
		}
	}
}

func TestClassifyNavError(t *testing.T) {
	cases := map[string]string{
		"page load error net::ERR_NAME_NOT_RESOLVED":            "dns",
		"net::ERR_CONNECTION_REFUSED":                           "connect",
		"net::ERR_CONNECTION_RESET":                             "connect",
		"net::ERR_ADDRESS_UNREACHABLE":                          "connect",
		"net::ERR_INTERNET_DISCONNECTED":                        "connect",
		"net::ERR_CERT_AUTHORITY_INVALID":                       "tls",
		"net::ERR_SSL_PROTOCOL_ERROR":                           "tls",
		"net::ERR_TIMED_OUT":                                    "timeout",
		"context deadline exceeded":                             "timeout",
		"net::ERR_ABORTED":                                      "nav_error",
		"net::ERR_TOO_MANY_REDIRECTS":                           "nav_error",
		"something unexpected":                                  "nav_error",
		"net::ERR_CONNECTION_CLOSED":                            "connect",
		"page load error net::ERR_BLOCKED_BY_RESPONSE.NOT_SAME": "nav_error",
	}
	for in, want := range cases {
		if got := ClassifyNavError(in); got != want {
			t.Errorf("%q: %s want %s", in, got, want)
		}
	}
}

func TestSafeURLAndSafeFinal(t *testing.T) {
	cases := map[string]string{
		"https://www.jet2.com/holiday/x?token=SECRET&a=1#frag": "https://www.jet2.com/holiday/x",
		"https://user:pass@www.jet2.com/p?x=1":                 "https://www.jet2.com/p",
		"https://www.jet2.com":                                 "https://www.jet2.com",
		"%%%":                                                  "[unparseable]",
		"https://www.jet2.com/" + strings.Repeat("a", 200):     "https://www.jet2.com/" + strings.Repeat("a", 79) + "...",
	}
	for in, want := range cases {
		got := SafeURL(in)
		if got != want {
			t.Errorf("SafeURL(%q) = %q want %q", in, got, want)
		}
		if strings.Contains(got, "SECRET") || strings.Contains(got, "pass") {
			t.Errorf("leak: %q", got)
		}
	}
	if SafeFinal("https://a.example/p?x=1#y") != "https://a.example/p" {
		t.Fatal("SafeFinal")
	}
}

func TestSlimHTML(t *testing.T) {
	src := `<html><head><style>.a{}</style><script>var big="` + strings.Repeat("z", 1000) + `"</script>
<script type="application/ld+json">{"@type":"Product","offers":{"price":"99.00","priceCurrency":"GBP"}}</script>
<meta property="og:price:amount" content="99.00"></head><body><!-- c --><svg><path d="M0"/></svg><noscript>x</noscript><p>From £99</p></body></html>`
	out := SlimHTML(src)
	for _, gone := range []string{".a{}", "zzzz", "<svg", "<!--", "noscript"} {
		if strings.Contains(out, gone) {
			t.Errorf("%q survived", gone)
		}
	}
	for _, kept := range []string{`"price":"99.00"`, "og:price:amount", "From £99"} {
		if !strings.Contains(out, kept) {
			t.Errorf("%q lost", kept)
		}
	}
}
