package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// CheckFailure describes why a check produced no price.
type CheckFailure struct {
	Status string // "blocked" or "error"
	Reason string
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

// CheckPage turns a fetched page into a price or a classified failure.
func CheckPage(p *Page, selector string) (*Result, *CheckFailure) {
	switch {
	case p.Status == 403 || p.Status == 429:
		return nil, &CheckFailure{"blocked", fmt.Sprintf("blocked by site (HTTP %d)", p.Status)}
	case p.Status == 503 && looksLikeBotChallenge(p):
		return nil, &CheckFailure{"blocked", "blocked by site (bot challenge, HTTP 503)"}
	case p.Status < 200 || p.Status >= 300:
		return nil, &CheckFailure{"error", fmt.Sprintf("site returned HTTP %d", p.Status)}
	}
	res, err := Extract(p.Body, selector)
	if err == nil {
		return res, nil
	}
	if looksLikeBotChallenge(p) && len(p.Body) < 100<<10 {
		return nil, &CheckFailure{"blocked", "blocked by site (bot challenge page)"}
	}
	var ee *ExtractError
	if errors.As(err, &ee) {
		return nil, &CheckFailure{"error", ee.Reason}
	}
	return nil, &CheckFailure{"error", err.Error()}
}

// CheckURL fetches and extracts in one step.
func CheckURL(ctx context.Context, f Fetcher, rawURL, selector string) (*Result, *CheckFailure) {
	p, err := f.Fetch(ctx, rawURL)
	if err != nil {
		var fe *FetchError
		if errors.As(err, &fe) {
			return nil, &CheckFailure{"error", fe.Msg}
		}
		return nil, &CheckFailure{"error", "fetch failed: " + shortErr(err)}
	}
	return CheckPage(p, selector)
}
