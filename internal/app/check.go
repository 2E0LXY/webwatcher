package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CheckFailure describes why a check produced no price.
type CheckFailure struct {
	// Status is one of:
	//   blocked    the site refused us or served a bot challenge (never evaded)
	//   error      transient: network failure, timeout, 5xx or other HTTP error
	//   noprice    the page was fetched but holds no price we can read
	//   disallowed robots.txt forbids the page (only with WW_RESPECT_ROBOTS=1)
	// None of them touches the last valid observation.
	//   deferred   a redirect target's per-host rate budget was exhausted; retry later, not a fault
	Status     string
	Reason     string
	RetryAfter time.Duration // from a Retry-After header on 429/503; 0 if none
	Class      string        // precise cause (see diag.go); empty for deferred
	Timing     Timing
}

func (c *CheckFailure) Error() string { return c.Reason }

var challengeMarkers = []string{
	"cf-browser-verification", "cf-chl-", "challenge-platform", "just a moment...",
	"attention required! | cloudflare", "_incapsula_resource", "incapsula incident",
	"px-captcha", "captcha-delivery.com", "pardon our interruption", "errors.edgesuite.net",
	"/_sec/cp_challenge", "akamai", "access denied", "are you a robot", "verify you are human",
}

// looksLikeBotChallenge recognises common bot-protection interstitials. We only
// detect and report them; we never try to get round them.
func looksLikeBotChallenge(p *Page) bool {
	if strings.EqualFold(p.Header.Get("Cf-Mitigated"), "challenge") {
		return true
	}
	if strings.Contains(strings.ToLower(p.Header.Get("Server")), "akamaighost") && p.Status >= 400 {
		return true
	}
	if len(p.Body) > 300<<10 {
		return false
	}
	lb := strings.ToLower(string(p.Body))
	for _, m := range challengeMarkers {
		if strings.Contains(lb, m) {
			return true
		}
	}
	return false
}

// maxRetryAfter bounds how far a Retry-After header can push the next check.
const maxRetryAfter = 7 * 24 * time.Hour

// parseRetryAfter reads delta-seconds or an HTTP date.
func parseRetryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	var d time.Duration
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 {
			return 0
		}
		d = time.Duration(min(n, int64(maxRetryAfter/time.Second))) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	}
	return max(0, min(d, maxRetryAfter))
}

// CheckPage turns a fetched page into a price or a classified failure.
func CheckPage(p *Page, selector string) (*Result, *CheckFailure) {
	return checkPageAt(p, selector, time.Now())
}

func checkPageAt(p *Page, selector string, now time.Time) (*Result, *CheckFailure) {
	ra := parseRetryAfter(p.Header, now)
	switch {
	case p.NotModified:
		return &Result{NotModified: true}, nil
	case p.Status == 403 || p.Status == 429:
		return nil, &CheckFailure{Status: "blocked", Reason: fmt.Sprintf("blocked by site (HTTP %d)", p.Status), RetryAfter: ra, Class: map[int]string{403: ClassHTTP403, 429: ClassHTTP429}[p.Status]}
	case p.Status == 503 && looksLikeBotChallenge(p):
		return nil, &CheckFailure{Status: "blocked", Reason: "blocked by site (bot challenge, HTTP 503)", RetryAfter: ra, Class: ClassChallenge}
	case p.Status < 200 || p.Status >= 300:
		class := ClassHTTPOther
		if p.Status >= 500 {
			class = ClassHTTP5xx
		}
		return nil, &CheckFailure{Status: "error", Reason: fmt.Sprintf("site returned HTTP %d", p.Status), RetryAfter: ra, Class: class}
	}
	if p.Truncated {
		return nil, &CheckFailure{Status: "error", Reason: fmt.Sprintf("the page is larger than %d MB; it was cut off and not parsed", MaxBodyBytes>>20), Class: ClassTooLarge}
	}
	res, err := Extract(p.Body, selector)
	if err == nil {
		res.ETag, res.LastModified = p.Header.Get("Etag"), p.Header.Get("Last-Modified")
		return res, nil
	}
	if looksLikeBotChallenge(p) && len(p.Body) < 100<<10 {
		return nil, &CheckFailure{Status: "blocked", Reason: "blocked by site (bot challenge page)", Class: ClassChallenge}
	}
	var ee *ExtractError
	if errors.As(err, &ee) {
		return nil, &CheckFailure{Status: "noprice", Reason: ee.Reason, Class: ClassNoPrice}
	}
	return nil, &CheckFailure{Status: "noprice", Reason: err.Error(), Class: ClassNoPrice}
}

// CheckURL fetches and extracts in one step.
func CheckURL(ctx context.Context, f Fetcher, rawURL, selector string, cond *Conditional) (*Result, *CheckFailure) {
	p, err := f.Fetch(ctx, rawURL, cond)
	if err != nil {
		var fe *FetchError
		if errors.As(err, &fe) {
			switch fe.Kind {
			case "disallowed":
				return nil, &CheckFailure{Status: "disallowed", Reason: fe.Msg, Class: ClassRobots, Timing: fe.Timing}
			case "ratelimit":
				return nil, &CheckFailure{Status: "deferred", Reason: fe.Msg, RetryAfter: fe.Wait}
			}
			class := fe.Class
			if class == "" && fe.Kind == "network" {
				class = ClassNetworkOther
			}
			return nil, &CheckFailure{Status: "error", Reason: fe.Msg, Class: class, Timing: fe.Timing}
		}
		return nil, &CheckFailure{Status: "error", Reason: "fetch failed: " + shortErr(err), Class: ClassNetworkOther}
	}
	res, fail := checkPageAt(p, selector, time.Now())
	if res != nil {
		res.Timing = p.Timing
	}
	if fail != nil {
		fail.Timing = p.Timing
	}
	return res, fail
}
