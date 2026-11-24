package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
)

var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",       // "this" network
		"10.0.0.0/8",      // private
		"100.64.0.0/10",   // CGNAT
		"127.0.0.0/8",     // loopback
		"169.254.0.0/16",  // link-local incl. cloud metadata 169.254.169.254
		"172.16.0.0/12",   // private
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // TEST-NET-1
		"192.88.99.0/24",  // 6to4 relay
		"192.168.0.0/16",  // private
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved + broadcast
		"::/128",
		"::1/128",
		"64:ff9b::/96",  // NAT64
		"100::/64",      // discard
		"2001::/32",     // Teredo
		"2001:db8::/32", // documentation
		"2002::/16",     // 6to4
		"fc00::/7",      // unique local (incl. fd00:ec2::254 metadata)
		"fe80::/10",     // link-local
		"ff00::/8",      // multicast
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// IsBlockedIP reports whether a destination address is off limits.
func IsBlockedIP(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || a.IsUnspecified() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

var errBlockedDest = errors.New("destination address is not allowed")

// guardControl is a net.Dialer Control function: it runs on the final,
// already-resolved address immediately before connect(2), so DNS rebinding
// between a pre-check and the dial cannot bypass the guard.
func guardControl(blocked func(netip.Addr) bool) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return fmt.Errorf("%w: %v", errBlockedDest, err)
		}
		if blocked(ap.Addr()) {
			return errBlockedDest
		}
		return nil
	}
}

// ValidateWatchURL checks a user-supplied URL syntactically. It never touches
// the network. It returns the cleaned URL string.
func ValidateWatchURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 {
		return "", errors.New("enter a URL of up to 2048 characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("that is not a valid URL")
	}
	return validateParsedURL(u)
}

func validateParsedURL(u *url.URL) (string, error) {
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("only http and https URLs are allowed")
	}
	if u.User != nil {
		return "", errors.New("URLs containing a username or password are not allowed")
	}
	host := u.Hostname()
	if host == "" {
		return "", errors.New("the URL has no host name")
	}
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil || strings.ContainsAny(host, ":[]") {
		return "", errors.New("IP addresses are not allowed; use the site's host name")
	}
	if isNumericHost(host) {
		return "", errors.New("IP addresses are not allowed; use the site's host name")
	}
	h := strings.ToLower(host)
	if !strings.Contains(h, ".") || h == "localhost" || strings.HasSuffix(h, ".localhost") ||
		strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".internal") || strings.HasSuffix(h, ".lan") {
		return "", errors.New("that host name is not allowed")
	}
	if p := u.Port(); p != "" && p != "80" && p != "443" {
		return "", errors.New("only the standard ports 80 and 443 are allowed")
	}
	u.Fragment = ""
	return u.String(), nil
}

// isNumericHost catches odd IPv4 spellings such as 2130706433 or 0x7f.1.
func isNumericHost(h string) bool {
	last := h
	if i := strings.LastIndex(h, "."); i >= 0 {
		last = h[i+1:]
	}
	if last == "" {
		return false
	}
	if strings.HasPrefix(strings.ToLower(last), "0x") {
		return true
	}
	for _, r := range last {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// lookupFunc resolves a host to addresses.
type lookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)

func defaultLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// resolveChecked resolves host and fails if any answer is blocked.
func resolveChecked(ctx context.Context, lookup lookupFunc, blocked func(netip.Addr) bool, host string) ([]netip.Addr, error) {
	addrs, err := lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("could not resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("could not resolve %s", host)
	}
	for _, a := range addrs {
		if blocked(a) {
			return nil, errBlockedDest
		}
	}
	return addrs, nil
}
