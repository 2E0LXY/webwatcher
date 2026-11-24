package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http/httptrace"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Failure classes recorded in the check log. They say what went wrong as
// precisely as the evidence allows; none of them contains a URL.
const (
	ClassDNS            = "dns"
	ClassConnectRefused = "connect_refused"
	ClassConnectReset   = "connect_reset"
	ClassConnectTimeout = "connect_timeout"
	ClassTLS            = "tls"
	ClassHeaderTimeout  = "header_timeout"
	ClassBodyTimeout    = "body_timeout"
	ClassHTTP403        = "http_403"
	ClassHTTP429        = "http_429"
	ClassHTTP5xx        = "http_5xx"
	ClassHTTPOther      = "http_other"
	ClassChallenge      = "challenge"
	ClassNoPrice        = "no_price"
	ClassTooLarge       = "too_large"
	ClassRobots         = "robots"
	ClassNetworkOther   = "network_other"
)

// transientClass reports whether a failure class is a transient network error
// that deserves the quick retries: timeouts, connection reset or refused, TLS
// handshake failures and 5xx answers. Blocks (403, 429, challenge pages) and
// pages without a price never are.
func transientClass(c string) bool {
	switch c {
	case ClassConnectRefused, ClassConnectReset, ClassConnectTimeout, ClassTLS,
		ClassHeaderTimeout, ClassBodyTimeout, ClassHTTP5xx:
		return true
	}
	return false
}

// confirmedBlock is a refusal the site actually sent, as opposed to a timeout,
// which only suggests a block.
func confirmedBlock(c string) bool {
	return c == ClassHTTP403 || c == ClassHTTP429 || c == ClassChallenge
}

// Timing holds how long each phase of one fetch took. A phase that did not
// happen (for example TLS on a reused or plain connection) is zero.
type Timing struct {
	DNS, Connect, TLS, TTFB, Total time.Duration
}

// CheckMeta is the diagnostic detail stored with one check log row.
type CheckMeta struct {
	Class  string
	Timing Timing
	Retry  int // 0 for a normal check, 1..3 for the quick retries
}

func firstMeta(m []CheckMeta) CheckMeta {
	if len(m) > 0 {
		return m[0]
	}
	return CheckMeta{}
}

// traceState records phase timings through net/http/httptrace, and how far the
// request got, which is what tells a header timeout from a body timeout.
type traceState struct {
	mu       sync.Mutex
	start    time.Time
	dnsAt    time.Time
	connAt   time.Time
	tlsAt    time.Time
	t        Timing
	connOK   bool
	tlsBegan bool
	tlsDone  bool
	tlsErr   error
	wrote    bool
	gotFirst bool
}

func newTraceState() *traceState { return &traceState{start: time.Now()} }

func (ts *traceState) attach(ctx context.Context) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { ts.lock(func() { ts.dnsAt = time.Now() }) },
		DNSDone: func(httptrace.DNSDoneInfo) {
			ts.lock(func() {
				if !ts.dnsAt.IsZero() {
					ts.t.DNS += time.Since(ts.dnsAt)
				}
			})
		},
		ConnectStart: func(_, _ string) {
			ts.lock(func() {
				if ts.connAt.IsZero() {
					ts.connAt = time.Now()
				}
			})
		},
		ConnectDone: func(_, _ string, err error) {
			ts.lock(func() {
				if err == nil {
					ts.connOK = true
					ts.t.Connect = time.Since(ts.connAt)
				}
			})
		},
		TLSHandshakeStart: func() { ts.lock(func() { ts.tlsBegan, ts.tlsAt = true, time.Now() }) },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			ts.lock(func() { ts.tlsDone, ts.tlsErr, ts.t.TLS = true, err, time.Since(ts.tlsAt) })
		},
		WroteRequest:         func(httptrace.WroteRequestInfo) { ts.lock(func() { ts.wrote = true }) },
		GotFirstResponseByte: func() { ts.lock(func() { ts.gotFirst, ts.t.TTFB = true, time.Since(ts.start) }) },
	})
}

func (ts *traceState) lock(f func()) {
	ts.mu.Lock()
	f()
	ts.mu.Unlock()
}

// finish returns the timings with the total filled in.
func (ts *traceState) finish() Timing {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	t := ts.t
	t.Total = time.Since(ts.start)
	return t
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return strings.Contains(err.Error(), "timeout awaiting response headers") ||
		strings.Contains(err.Error(), "TLS handshake timeout")
}

func isTLSError(err error) bool {
	var (
		rh  tls.RecordHeaderError
		ua  x509.UnknownAuthorityError
		hn  x509.HostnameError
		ci  x509.CertificateInvalidError
		cve *tls.CertificateVerificationError
		alt tls.AlertError
	)
	if errors.As(err, &rh) || errors.As(err, &ua) || errors.As(err, &hn) || errors.As(err, &ci) ||
		errors.As(err, &cve) || errors.As(err, &alt) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "tls:") || strings.Contains(s, "x509:") || strings.Contains(s, "TLS handshake")
}

func isReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		strings.Contains(err.Error(), "connection reset") || strings.Contains(err.Error(), "broken pipe")
}

// classifyNetErr names the cause of a failed request (before the response
// headers were fully received).
func classifyNetErr(err error, ts *traceState) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ClassDNS
	}
	ts.mu.Lock()
	connOK, tlsBegan, tlsDone, tlsErr, gotFirst := ts.connOK, ts.tlsBegan, ts.tlsDone, ts.tlsErr, ts.gotFirst
	ts.mu.Unlock()
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return ClassConnectRefused
	case tlsErr != nil || isTLSError(err) || (tlsBegan && !tlsDone && isTimeout(err)):
		return ClassTLS
	case isTimeout(err):
		switch {
		case !connOK:
			return ClassConnectTimeout
		case gotFirst:
			return ClassBodyTimeout
		}
		return ClassHeaderTimeout
	case isReset(err):
		return ClassConnectReset
	}
	return ClassNetworkOther
}

// classifyBodyErr names the cause of an error while reading the body.
func classifyBodyErr(err error) string {
	switch {
	case isTimeout(err):
		return ClassBodyTimeout
	case isReset(err):
		return ClassConnectReset
	}
	return ClassNetworkOther
}

// FailureHint is the plain-English explanation shown with a classified failure.
func FailureHint(class string) string {
	switch class {
	case ClassDNS:
		return "The site's address could not be looked up. This is usually a short-lived DNS problem; check the address is right if it keeps happening."
	case ClassConnectRefused:
		return "The site refused the connection. It may be down for maintenance, or it may refuse connections from data-centre addresses."
	case ClassConnectReset:
		return "The connection was cut off by the site. Some travel sites do this to automated requests from data-centre addresses. Webwatcher does not try to bypass this."
	case ClassConnectTimeout:
		return "The site did not accept a connection in time. It may be down, or it may silently drop connections from data-centre addresses. Webwatcher does not try to bypass this."
	case ClassTLS:
		return "The secure (TLS) connection could not be set up. This is often temporary; some sites also refuse automated clients at this stage."
	case ClassHeaderTimeout:
		return "The site did not answer; many travel sites block automated requests from data-centre addresses. Webwatcher does not try to bypass this. This is an unconfirmed block: it may just be slow."
	case ClassBodyTimeout:
		return "The site started to answer but did not finish sending the page. This is an unconfirmed block: it may just be slow."
	case ClassHTTP403:
		return "The site refused the request (HTTP 403). This is a confirmed block. Webwatcher does not try to bypass it; use 'Record a price I saw' to keep this watch useful."
	case ClassHTTP429:
		return "The site says there have been too many requests (HTTP 429). This is a confirmed block. Webwatcher backs off and will try again later."
	case ClassChallenge:
		return "The site served a bot-check page instead of the product page. This is a confirmed block. Webwatcher does not try to bypass it; use 'Record a price I saw' to keep this watch useful."
	case ClassHTTP5xx:
		return "The site reported a server error. This is usually temporary."
	case ClassHTTPOther:
		return "The site answered with an unexpected HTTP status."
	case ClassNoPrice:
		return "The page loaded but no price could be read from it. Many sites build prices with JavaScript, which Webwatcher does not run; a CSS selector may help."
	case ClassTooLarge:
		return "The page is larger than Webwatcher will read, so it was not parsed."
	case ClassRobots:
		return "robots.txt on the site disallows this page and WW_RESPECT_ROBOTS is on, so it was not fetched."
	case ClassNetworkOther:
		return "The request failed for a reason Webwatcher could not classify."
	}
	return ""
}

// FailureKind says whether a class is a confirmed block, an unconfirmed
// (timeout) block or neither.
func FailureKind(class string) string {
	switch {
	case confirmedBlock(class):
		return "confirmed block"
	case class == ClassHeaderTimeout || class == ClassBodyTimeout || class == ClassConnectTimeout:
		return "timeout (unconfirmed block)"
	}
	return ""
}
