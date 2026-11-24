package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "127.9.9.9", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1", "169.254.169.254",
		"100.64.0.1", "100.127.255.255", "0.0.0.0", "224.0.0.1", "255.255.255.255", "198.18.0.1", "192.0.2.1",
		"::1", "::", "fe80::1", "fd00:ec2::254", "fc00::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "64:ff9b::7f00:1", "2002:7f00:1::1", "ff02::1"}
	for _, s := range blocked {
		if !IsBlockedIP(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "93.184.216.34", "100.63.255.255", "172.32.0.1", "2606:4700:4700::1111"} {
		if IsBlockedIP(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestValidateWatchURL(t *testing.T) {
	good := map[string]string{
		"https://www.jet2.com/holidays/x?a=1#frag": "https://www.jet2.com/holidays/x?a=1",
		"http://shop.example.co.uk:80/p":           "http://shop.example.co.uk:80/p",
		"https://tui.co.uk:443/":                   "https://tui.co.uk:443/",
	}
	for in, want := range good {
		got, err := ValidateWatchURL(in)
		if err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	bad := []string{"", "ftp://a.example.com/", "file:///etc/passwd", "javascript:alert(1)", "https://user:pw@a.example.com/",
		"https://a.example.com:8080/", "http://127.0.0.1/", "http://[::1]/", "http://2130706433/", "http://0x7f.0.0.1/",
		"http://10.0.0.1/", "http://localhost/", "http://intranet/", "http://x.local/", "https://a.example.com:22/", "//a.example.com/", "http://", "http://169.254.169.254/latest",
		"http://foo.localhost/", "http://[::ffff:127.0.0.1]/", "http://1.2.3.4.5/"}
	for _, in := range bad {
		if got, err := ValidateWatchURL(in); err == nil {
			t.Errorf("%q should be rejected, got %q", in, got)
		}
	}
	if _, err := ValidateWatchURL("https://a.example.com/" + strings.Repeat("x", 3000)); err == nil {
		t.Error("long URL accepted")
	}
}

// loopbackOnly is a test guard: everything except 127.0.0.1 is blocked, so a
// local httptest server stands in for a "public" site.
func loopbackOnly(a netip.Addr) bool { return a != netip.MustParseAddr("127.0.0.1") }

func testLookup(m map[string]string) lookupFunc {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		if ip, ok := m[host]; ok {
			return []netip.Addr{netip.MustParseAddr(ip)}, nil
		}
		return nil, errors.New("no such host")
	}
}

func fetcherFor(srv *httptest.Server, hosts map[string]string) *HTTPFetcher {
	addr := strings.TrimPrefix(srv.URL, "http://")
	return newHTTPFetcher(testLookup(hosts), loopbackOnly, func(string) string { return addr })
}

func TestFetchOK(t *testing.T) {
	var ua atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua.Store(r.Header.Get("User-Agent"))
		w.Write([]byte("<html>hi</html>"))
	}))
	defer srv.Close()
	f := fetcherFor(srv, map[string]string{"shop.test": "127.0.0.1"})
	p, err := f.Fetch(context.Background(), "http://shop.test/p")
	if err != nil || p.Status != 200 || string(p.Body) != "<html>hi</html>" {
		t.Fatalf("%v %+v", err, p)
	}
	if u, _ := ua.Load().(string); !strings.HasPrefix(u, "webwatcher/") || !strings.Contains(u, "github.com/2E0LXY/webwatcher") {
		t.Fatalf("UA %q", u)
	}
}

func TestFetchBodyCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("a", 3<<20)))
	}))
	defer srv.Close()
	f := fetcherFor(srv, map[string]string{"shop.test": "127.0.0.1"})
	p, err := f.Fetch(context.Background(), "http://shop.test/")
	if err != nil || len(p.Body) != MaxBodyBytes || !p.Truncated {
		t.Fatalf("%v len=%d trunc=%v", err, len(p.Body), p != nil && p.Truncated)
	}
}

func TestFetchBlocksPrivateStartHost(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	f := fetcherFor(srv, map[string]string{"internal.test": "10.0.0.5", "meta.test": "169.254.169.254"})
	for _, h := range []string{"internal.test", "meta.test"} {
		_, err := f.Fetch(context.Background(), "http://"+h+"/")
		var fe *FetchError
		if !errors.As(err, &fe) || fe.Kind != "ssrf" {
			t.Errorf("%s: %v", h, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("server was contacted")
	}
}

func TestFetchRedirects(t *testing.T) {
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/to-private", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://internal.test/secret", http.StatusFound)
	})
	mux.HandleFunc("/to-ip", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1/secret", http.StatusFound)
	})
	mux.HandleFunc("/to-port", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://shop.test:8080/x", http.StatusFound)
	})
	mux.HandleFunc("/to-file", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/loop", http.StatusFound)
	})
	mux.HandleFunc("/hop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("done")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	f := fetcherFor(srv, map[string]string{"shop.test": "127.0.0.1", "internal.test": "192.168.0.10"})

	for _, path := range []string{"/to-private", "/to-ip", "/to-port", "/to-file"} {
		p, err := f.Fetch(context.Background(), "http://shop.test"+path)
		if err == nil {
			t.Errorf("%s: redirect followed, status %d", path, p.Status)
		}
	}
	_, err := f.Fetch(context.Background(), "http://shop.test/to-private")
	var fe *FetchError
	if !errors.As(err, &fe) || fe.Kind != "ssrf" {
		t.Errorf("redirect to private: want ssrf FetchError, got %v", err)
	}
	if _, err := f.Fetch(context.Background(), "http://shop.test/loop"); err == nil || hits.Load() > MaxRedirects+1 {
		t.Errorf("redirect loop: err=%v hits=%d", err, hits.Load())
	}
	if p, err := f.Fetch(context.Background(), "http://shop.test/hop"); err != nil || string(p.Body) != "done" {
		t.Errorf("benign redirect: %v", err)
	}
}

// Rebinding: the pre-check sees a public address, the dial-time lookup returns
// loopback. The connection must be refused and the server never reached.
func TestFetchDNSRebinding(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	var calls atomic.Int32
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		if calls.Add(1) == 1 {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	addr := strings.TrimPrefix(srv.URL, "http://")
	f := newHTTPFetcher(lookup, IsBlockedIP, func(string) string { return addr })
	_, err := f.Fetch(context.Background(), "http://rebind.example.com/")
	var fe *FetchError
	if !errors.As(err, &fe) || fe.Kind != "ssrf" {
		t.Fatalf("want ssrf, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("server reached despite rebinding")
	}
}

// The dialer Control hook must refuse a blocked address even when no resolver
// check ran first (belt and braces against rebinding between check and dial).
func TestDialControlRefusesLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	d := &net.Dialer{Timeout: time.Second, Control: guardControl(IsBlockedIP)}
	c, err := d.Dial("tcp", ln.Addr().String())
	if err == nil {
		c.Close()
		t.Fatal("dial to loopback succeeded")
	}
	if !errors.Is(err, errBlockedDest) {
		t.Fatalf("unexpected error: %v", err)
	}
	// and the allowing guard connects, proving the failure above was the guard
	d2 := &net.Dialer{Timeout: time.Second, Control: guardControl(func(netip.Addr) bool { return false })}
	c2, err := d2.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c2.Close()
}

func TestHostLimiter(t *testing.T) {
	clk := newFakeClock()
	h := NewHostLimiter(HostMinGap, clk)
	if ok, _ := h.Reserve("A.example.com"); !ok {
		t.Fatal("first should pass")
	}
	ok, wait := h.Reserve("a.example.com")
	if ok || wait != HostMinGap {
		t.Fatalf("second should wait: %v %v", ok, wait)
	}
	if ok, _ := h.Reserve("b.example.com"); !ok {
		t.Fatal("other host should pass")
	}
	clk.Advance(61 * time.Second)
	if ok, _ := h.Reserve("a.example.com"); !ok {
		t.Fatal("should pass after gap")
	}
}
