package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	MaxBodyBytes  = 2 << 20
	FetchTimeout  = 20 * time.Second
	MaxRedirects  = 5
	HostMinGap    = 60 * time.Second
	MaxConcurrent = 4
)

// Page is the result of fetching a URL. Non-2xx statuses are returned as
// pages, not errors, so the caller can classify them.
type Page struct {
	NotModified bool // 304 in answer to a conditional request
	URL         string
	Status      int
	ContentType string
	Header      http.Header
	Body        []byte
	Truncated   bool
}

// Fetcher retrieves a page. The interface exists so a headless-browser
// implementation can be added later.
type Fetcher interface {
	// cond, if not nil, makes the request conditional (If-None-Match / If-Modified-Since).
	Fetch(ctx context.Context, rawURL string, cond *Conditional) (*Page, error)
}

// Conditional holds the validators from the last good fetch.
type Conditional struct{ ETag, LastModified string }

// FetchError is a transport-level failure.
type FetchError struct {
	Kind string // invalid, ssrf, network, truncated, disallowed, ratelimit
	Msg  string
	Wait time.Duration // for ratelimit: how long until the host may be used again
}

func (e *FetchError) Error() string { return e.Msg }

var userAgent = "webwatcher/dev (+https://github.com/2E0LXY/webwatcher; personal price monitor)"

// SetUserAgentVersion sets the version string used in the User-Agent.
func SetUserAgentVersion(v string) {
	userAgent = "webwatcher/" + v + " (+https://github.com/2E0LXY/webwatcher; personal price monitor)"
}

// HTTPFetcher is the plain-HTTP, SSRF-guarded fetcher.
type HTTPFetcher struct {
	client  *http.Client
	lookup  lookupFunc
	blocked func(netip.Addr) bool
	// test hooks (nil in production)
	rewriteDial func(addr string) string

	robots *robotsCache // nil unless WW_RESPECT_ROBOTS=1
	hosts  *HostLimiter // shared per-host rate budget, also applied to redirect destinations (nil in some tests)
}

// SetHostLimiter makes redirects share the scheduler's per-host rate budget.
func (f *HTTPFetcher) SetHostLimiter(l *HostLimiter) { f.hosts = l }

// SetRespectRobots turns on the opt-in robots.txt check. It is off by default.
func (f *HTTPFetcher) SetRespectRobots(on bool) {
	if on {
		f.robots = newRobotsCache()
	} else {
		f.robots = nil
	}
}

func NewHTTPFetcher() *HTTPFetcher {
	return newHTTPFetcher(defaultLookup, IsBlockedIP, nil)
}

func newHTTPFetcher(lookup lookupFunc, blocked func(netip.Addr) bool, rewrite func(string) string) *HTTPFetcher {
	f := &HTTPFetcher{lookup: lookup, blocked: blocked, rewriteDial: rewrite}
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: guardControl(blocked), // final check on the actual connect address
	}
	tr := &http.Transport{
		Proxy: nil, // never honour proxy env vars: the guard must see the real destination
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			// Resolve again at dial time; the answers used to connect are the ones checked.
			addrs, err := resolveChecked(ctx, f.lookup, f.blocked, host)
			if err != nil {
				return nil, err
			}
			var last error
			for _, a := range addrs {
				target := net.JoinHostPort(a.String(), port)
				if f.rewriteDial != nil {
					target = f.rewriteDial(target)
				}
				c, err := dialer.DialContext(ctx, network, target)
				if err == nil {
					return c, nil
				}
				last = err
			}
			return nil, last
		},
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  15 * time.Second,
		MaxIdleConns:           4,
		IdleConnTimeout:        30 * time.Second,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 64 << 10,
	}
	f.client = &http.Client{
		Transport: tr,
		Timeout:   FetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > MaxRedirects {
				return fmt.Errorf("stopped after %d redirects", MaxRedirects)
			}
			if err := f.precheck(req.Context(), req.URL); err != nil {
				return err
			}
			// A redirect to another host is a request to that host: it draws
			// on that host's budget like any other check would.
			if f.hosts != nil && !seenHost(via, req.URL) {
				if ok, wait := f.hosts.Reserve(req.URL.Hostname()); !ok {
					return &FetchError{Kind: "ratelimit", Wait: wait,
						Msg: "the page redirects to " + strings.ToLower(req.URL.Hostname()) + ", which was contacted too recently; will retry later"}
				}
			}
			return f.robotsCheck(req.Context(), req.URL)
		},
	}
	return f
}

// precheck validates a URL and resolves its host against the guard. It is
// run for the initial URL and again for every redirect target.
func (f *HTTPFetcher) precheck(ctx context.Context, u *url.URL) error {
	if _, err := validateParsedURL(cloneURL(u)); err != nil {
		return &FetchError{Kind: "invalid", Msg: err.Error()}
	}
	if _, err := resolveChecked(ctx, f.lookup, f.blocked, u.Hostname()); err != nil {
		if errors.Is(err, errBlockedDest) {
			return &FetchError{Kind: "ssrf", Msg: "the site resolves to an address that is not allowed"}
		}
		return &FetchError{Kind: "network", Msg: err.Error()}
	}
	return nil
}

func seenHost(via []*http.Request, u *url.URL) bool {
	for _, r := range via {
		if strings.EqualFold(r.URL.Hostname(), u.Hostname()) {
			return true
		}
	}
	return false
}

func cloneURL(u *url.URL) *url.URL { c := *u; return &c }

func (f *HTTPFetcher) Fetch(ctx context.Context, rawURL string, cond *Conditional) (*Page, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, &FetchError{Kind: "invalid", Msg: "invalid URL"}
	}
	if err := f.precheck(ctx, u); err != nil {
		return nil, err
	}
	if err := f.robotsCheck(ctx, u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, &FetchError{Kind: "invalid", Msg: "invalid URL"}
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.5")
	req.Header.Set("Accept-Language", "en-GB,en;q=0.8")
	if cond != nil {
		if cond.ETag != "" {
			req.Header.Set("If-None-Match", cond.ETag)
		}
		if cond.LastModified != "" {
			req.Header.Set("If-Modified-Since", cond.LastModified)
		}
	}
	resp, err := f.client.Do(req)
	if err != nil {
		var fe *FetchError
		if errors.As(err, &fe) {
			return nil, fe
		}
		if errors.Is(err, errBlockedDest) {
			return nil, &FetchError{Kind: "ssrf", Msg: "the site resolves to an address that is not allowed"}
		}
		return nil, &FetchError{Kind: "network", Msg: "network error: " + shortErr(err)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		// A partial body is never parsed: half a page can hold a wrong price.
		return nil, &FetchError{Kind: "truncated", Msg: "the connection ended before the whole page arrived; it was not parsed (" + shortErr(err) + ")"}
	}
	if cl := resp.ContentLength; cl >= 0 && len(body) <= MaxBodyBytes && int64(len(body)) != cl &&
		resp.StatusCode != http.StatusNotModified && resp.StatusCode != http.StatusNoContent {
		return nil, &FetchError{Kind: "truncated", Msg: "the page length did not match what the site announced; it was not parsed"}
	}
	p := &Page{NotModified: resp.StatusCode == http.StatusNotModified && cond != nil, URL: resp.Request.URL.String(), Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Header: resp.Header}
	if len(body) > MaxBodyBytes {
		body, p.Truncated = body[:MaxBodyBytes], true
	}
	p.Body = body
	return p, nil
}

func shortErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	s := err.Error()
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// HostLimiter enforces a minimum gap between requests to the same host.
type HostLimiter struct {
	mu    sync.Mutex
	last  map[string]time.Time
	gap   time.Duration
	clock Clock
}

func NewHostLimiter(gap time.Duration, clock Clock) *HostLimiter {
	return &HostLimiter{last: map[string]time.Time{}, gap: gap, clock: clock}
}

// Reserve claims a slot for host. If one is not yet free it returns false and
// how long to wait.
func (h *HostLimiter) Reserve(host string) (bool, time.Duration) {
	host = strings.ToLower(host)
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.clock.Now()
	if t, ok := h.last[host]; ok && now.Sub(t) < h.gap {
		return false, h.gap - now.Sub(t)
	}
	h.last[host] = now
	if len(h.last) > 5000 {
		for k, t := range h.last {
			if now.Sub(t) > h.gap {
				delete(h.last, k)
			}
		}
	}
	return true, 0
}
