package app

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

// rawServer accepts connections and hands each to h after the request headers
// have been read.
func rawServer(t *testing.T, h func(c net.Conn)) *httptest.Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil || line == "\r\n" {
						break
					}
				}
				h(c)
			}()
		}
	}()
	return &httptest.Server{URL: "http://" + ln.Addr().String()}
}

func fetchClass(t *testing.T, f *HTTPFetcher) (string, *FetchError) {
	t.Helper()
	_, err := f.Fetch(context.Background(), "http://shop.test/p", nil)
	var fe *FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("expected a FetchError, got %v", err)
	}
	return fe.Class, fe
}

func TestFailureClassification(t *testing.T) {
	hosts := map[string]string{"shop.test": "127.0.0.1"}
	tr := func(f *HTTPFetcher) *http.Transport { return f.client.Transport.(*http.Transport) }

	t.Run("connect_refused", func(t *testing.T) {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		addr := ln.Addr().String()
		ln.Close()
		f := fetcherFor(&httptest.Server{URL: "http://" + addr}, hosts)
		if c, _ := fetchClass(t, f); c != ClassConnectRefused {
			t.Fatalf("%s", c)
		}
	})
	t.Run("connect_reset", func(t *testing.T) {
		srv := rawServer(t, func(c net.Conn) {}) // reads the request, closes without answering
		f := fetcherFor(srv, hosts)
		if c, _ := fetchClass(t, f); c != ClassConnectReset {
			t.Fatalf("%s", c)
		}
	})
	t.Run("header_timeout", func(t *testing.T) {
		srv := rawServer(t, func(c net.Conn) { time.Sleep(2 * time.Second) })
		f := fetcherFor(srv, hosts)
		tr(f).ResponseHeaderTimeout = 150 * time.Millisecond
		c, fe := fetchClass(t, f)
		if c != ClassHeaderTimeout {
			t.Fatalf("%s (%s)", c, fe.Msg)
		}
		if fe.Timing.Total < 100*time.Millisecond || fe.Timing.TTFB != 0 {
			t.Fatalf("timing %+v", fe.Timing)
		}
	})
	t.Run("body_timeout", func(t *testing.T) {
		srv := rawServer(t, func(c net.Conn) {
			c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\n<p>half"))
			time.Sleep(2 * time.Second)
		})
		f := fetcherFor(srv, hosts)
		f.client.Timeout = 300 * time.Millisecond
		c, fe := fetchClass(t, f)
		if c != ClassBodyTimeout {
			t.Fatalf("%s (%s)", c, fe.Msg)
		}
	})
	t.Run("tls", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer srv.Close()
		addr := strings.TrimPrefix(srv.URL, "https://")
		f := newHTTPFetcher(testLookup(hosts), loopbackOnly, func(string) string { return addr })
		_, err := f.Fetch(context.Background(), "https://shop.test/p", nil) // certificate is not trusted
		var fe *FetchError
		if !errors.As(err, &fe) || fe.Class != ClassTLS {
			t.Fatalf("%v %+v", err, fe)
		}
	})
	t.Run("dns", func(t *testing.T) {
		lookup := func(context.Context, string) ([]netip.Addr, error) {
			return nil, &net.DNSError{Err: "no such host", Name: "shop.test", IsNotFound: true}
		}
		f := newHTTPFetcher(lookup, loopbackOnly, nil)
		if c, _ := fetchClass(t, f); c != ClassDNS {
			t.Fatalf("%s", c)
		}
	})
}

func TestHTTPClassification(t *testing.T) {
	cases := []struct {
		name string
		p    *Page
		want string
	}{
		{"403", &Page{Status: 403, Header: http.Header{}}, ClassHTTP403},
		{"429", &Page{Status: 429, Header: http.Header{}}, ClassHTTP429},
		{"503 challenge", &Page{Status: 503, Header: http.Header{}, Body: []byte("<html>Just a moment...</html>")}, ClassChallenge},
		{"challenge body", &Page{Status: 200, Header: http.Header{}, Body: fixture(t, "cloudflare.html")}, ClassChallenge},
		{"502", &Page{Status: 502, Header: http.Header{}}, ClassHTTP5xx},
		{"no price", &Page{Status: 200, Header: http.Header{}, Body: fixture(t, "noprice.html")}, ClassNoPrice},
	}
	for _, c := range cases {
		_, fail := CheckPage(c.p, "")
		if fail == nil || fail.Class != c.want {
			t.Errorf("%s: %+v, want %s", c.name, fail, c.want)
		}
	}
	if !transientClass(ClassHTTP5xx) || transientClass(ClassHTTP403) || transientClass(ClassChallenge) || transientClass(ClassNoPrice) || transientClass(ClassHTTP429) {
		t.Fatal("transient set wrong")
	}
}

func TestTimingRecordedInCheckLogWithoutURLs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond)
		w.Write([]byte(`<meta itemprop="price" content="10.00">`))
	}))
	defer srv.Close()
	e := newSchedEnv(t)
	e.s.fetcher = fetcherFor(srv, map[string]string{"shop.example.com": "127.0.0.1"})
	id := e.add(t, "http://shop.example.com/p?ref=SECRET", nil, 60)
	e.run(t, 0)
	cs, err := e.st.RecentChecks(e.uid, id, 5)
	if err != nil || len(cs) != 1 {
		t.Fatalf("%v %v", err, cs)
	}
	tm := cs[0].Timing
	if cs[0].Outcome != "ok" || tm.TTFB < 50*time.Millisecond || tm.Total < tm.TTFB || tm.Connect < 0 {
		t.Fatalf("%+v", cs[0])
	}
	var raw string
	e.st.db.QueryRow(`SELECT outcome||detail||class FROM check_log`).Scan(&raw)
	if strings.Contains(raw, "SECRET") || strings.Contains(raw, "shop.example.com/p") {
		t.Fatalf("URL in check log: %q", raw)
	}
}

func TestFailureClassStoredAndLeakFree(t *testing.T) {
	e := newSchedEnv(t)
	const u = "https://shop.example.com/p?token=SECRET"
	id := e.add(t, u, nil, 60)
	e.f.errs[u] = &FetchError{Kind: "network", Class: ClassHeaderTimeout, Msg: "network error: Get " + u + ": net/http: timeout awaiting response headers",
		Timing: Timing{Connect: 20 * time.Millisecond, TLS: 40 * time.Millisecond, Total: 15 * time.Second}}
	e.run(t, 0)
	cs, _ := e.st.RecentChecks(e.uid, id, 5)
	if len(cs) != 1 || cs[0].Class != ClassHeaderTimeout || cs[0].Timing.Total != 15*time.Second || cs[0].Timing.TLS != 40*time.Millisecond {
		t.Fatalf("%+v", cs)
	}
	if strings.Contains(cs[0].Detail, "SECRET") {
		t.Fatalf("detail leaks the URL: %q", cs[0].Detail)
	}
}

func TestWatchPageShowsClassifiedFailureWithHint(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	c := b.login("07700900123")
	b.do("POST", "/watches", url.Values{"csrf": {c}, "url": {"https://www.jet2holidays.com/x"}})
	meta := func(class string) CheckMeta { return CheckMeta{Class: class, Timing: Timing{Total: 15 * time.Second}} }
	e.st.RecordFailure(1, "error", "network error: timeout", 1, 0, nil, nil, meta(ClassHeaderTimeout))
	body := b.do("GET", "/watches/1", nil).Body.String()
	for _, want := range []string{"header_timeout", "timeout (unconfirmed block)", "The site did not answer; many travel sites block automated requests from data-centre addresses. Webwatcher does not try to bypass this."} {
		if !strings.Contains(body, want) {
			t.Errorf("watch page lacks %q", want)
		}
	}
	if strings.Contains(body, "confirmed block)") && !strings.Contains(body, "unconfirmed block)") {
		t.Error("timeout shown as confirmed")
	}
	e.st.RecordFailure(1, "blocked", "blocked by site (HTTP 403)", 2, 0, nil, nil, meta(ClassHTTP403))
	body = b.do("GET", "/watches/1", nil).Body.String()
	if !strings.Contains(body, "http_403 (confirmed block)") || strings.Contains(body, "unconfirmed") {
		t.Errorf("403 not shown as a confirmed block")
	}
}
