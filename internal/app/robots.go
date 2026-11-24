package app

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// robots.txt support is opt-in (WW_RESPECT_ROBOTS=1) and off by default.
// When on, the file is fetched once per host, cached for 24 hours, and a
// matching Disallow stops the fetch with a "disallowed" status. An unreachable
// robots.txt (network error, 5xx) counts as allowed.

const (
	robotsToken    = "webwatcher"
	robotsTTL      = 24 * time.Hour
	robotsFailTTL  = time.Hour // retry sooner when robots.txt could not be fetched
	robotsMaxBytes = 512 << 10
)

type robotsRule struct {
	allow   bool
	pattern string
}

type robotsRules struct{ rules []robotsRule }

type robotsEntry struct {
	rules   *robotsRules // nil = allow everything
	expires time.Time
}

type robotsCache struct {
	mu  sync.Mutex
	m   map[string]robotsEntry
	now func() time.Time
}

func newRobotsCache() *robotsCache { return &robotsCache{m: map[string]robotsEntry{}, now: time.Now} }

// parseRobots extracts the rules that apply to token (the specific group if
// there is one, otherwise the "*" group), following RFC 9309 group rules.
func parseRobots(body, token string) *robotsRules {
	type group struct {
		agents []string
		rules  []robotsRule
	}
	var groups []*group
	var cur *group
	lastWasAgent := false
	for _, line := range strings.Split(body, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		switch k {
		case "user-agent":
			if cur == nil || !lastWasAgent {
				cur = &group{}
				groups = append(groups, cur)
			}
			cur.agents = append(cur.agents, strings.ToLower(v))
			lastWasAgent = true
		case "allow", "disallow":
			lastWasAgent = false
			if cur != nil && v != "" {
				cur.rules = append(cur.rules, robotsRule{allow: k == "allow", pattern: v})
			}
		default:
			lastWasAgent = false
		}
	}
	pick := func(match func(string) bool) *robotsRules {
		var out *robotsRules
		for _, g := range groups {
			for _, a := range g.agents {
				if match(a) {
					if out == nil {
						out = &robotsRules{}
					}
					out.rules = append(out.rules, g.rules...)
					break
				}
			}
		}
		return out
	}
	token = strings.ToLower(token)
	if r := pick(func(a string) bool { return a == token }); r != nil {
		return r
	}
	return pick(func(a string) bool { return a == "*" })
}

// robotsMatch reports whether pattern (with * wildcards and an optional
// trailing $ anchor) matches the start of path.
func robotsMatch(pattern, path string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	pattern = strings.TrimSuffix(pattern, "$")
	parts := strings.Split(pattern, "*")
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	expr := "^" + strings.Join(parts, ".*")
	if anchored {
		expr += "$"
	}
	re, err := regexp.Compile(expr)
	return err == nil && re.MatchString(path)
}

// Allowed applies the longest-match rule; Allow wins ties.
func (r *robotsRules) Allowed(path string) bool {
	if r == nil {
		return true
	}
	bestLen, allowed := -1, true
	for _, rule := range r.rules {
		if !robotsMatch(rule.pattern, path) {
			continue
		}
		l := len(rule.pattern)
		if l > bestLen || (l == bestLen && rule.allow) {
			bestLen, allowed = l, rule.allow
		}
	}
	return allowed
}

func (f *HTTPFetcher) fetchRobots(ctx context.Context, u *url.URL) (*robotsRules, bool) {
	ru := &url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/robots.txt"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ru.String(), nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, false // unreachable: treated as allowed
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 200:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, robotsMaxBytes))
		return parseRobots(string(b), robotsToken), true
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return nil, true // no robots.txt: everything allowed
	}
	return nil, false
}

// robotsCheck returns a FetchError of Kind "disallowed" if robots.txt forbids
// the URL. It is a no-op unless the opt-in is on.
func (f *HTTPFetcher) robotsCheck(ctx context.Context, u *url.URL) error {
	rc := f.robots
	if rc == nil {
		return nil
	}
	key := strings.ToLower(u.Scheme + "://" + u.Host)
	rc.mu.Lock()
	e, ok := rc.m[key]
	rc.mu.Unlock()
	if !ok || !rc.now().Before(e.expires) {
		rules, good := f.fetchRobots(ctx, u)
		ttl := robotsTTL
		if !good {
			ttl = robotsFailTTL
		}
		e = robotsEntry{rules: rules, expires: rc.now().Add(ttl)}
		rc.mu.Lock()
		rc.m[key] = e
		rc.mu.Unlock()
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	if !e.rules.Allowed(path) {
		return &FetchError{Kind: "disallowed", Msg: "robots.txt on " + strings.ToLower(u.Hostname()) + " disallows this page for webwatcher, so it was not fetched"}
	}
	return nil
}
