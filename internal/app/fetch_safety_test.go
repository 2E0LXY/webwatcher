package app

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func redirectServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Host == "a.test" && r.URL.Path == "/go":
			http.Redirect(w, r, "http://b.test/p", http.StatusFound)
		case r.Host == "a.test" && r.URL.Path == "/self":
			http.Redirect(w, r, "http://a.test/p", http.StatusFound)
		default:
			w.Write([]byte("<html>ok</html>"))
		}
	}))
}

func TestRedirectDestinationUsesHostBudget(t *testing.T) {
	srv := redirectServer()
	defer srv.Close()
	clk := newFakeClock()
	lim := NewHostLimiter(time.Minute, clk)
	f := fetcherFor(srv, map[string]string{"a.test": "127.0.0.1", "b.test": "127.0.0.1"})
	f.SetHostLimiter(lim)

	// b.test was contacted a moment ago: the redirect to it must wait
	lim.Reserve("b.test")
	clk.Advance(20 * time.Second)
	_, err := f.Fetch(context.Background(), "http://a.test/go", nil)
	var fe *FetchError
	if !errors.As(err, &fe) || fe.Kind != "ratelimit" || fe.Wait <= 0 || fe.Wait > 41*time.Second {
		t.Fatalf("%v %+v", err, fe)
	}
	// once the budget allows, the redirect is followed and consumes it
	clk.Advance(time.Minute)
	if p, err := f.Fetch(context.Background(), "http://a.test/go", nil); err != nil || p.Status != 200 {
		t.Fatalf("%v", err)
	}
	if ok, _ := lim.Reserve("b.test"); ok {
		t.Fatal("redirect did not use up the destination host's budget")
	}
	// same-host redirects do not draw a second slot
	lim.Reserve("a.test")
	if _, err := f.Fetch(context.Background(), "http://a.test/self", nil); err != nil {
		t.Fatalf("same-host redirect: %v", err)
	}
}

func TestSchedulerDefersOnRedirectRateLimit(t *testing.T) {
	e := newSchedEnv(t)
	const u = "https://shop.example.com/p"
	id := e.add(t, u, nil, 60)
	e.f.errs[u] = &FetchError{Kind: "ratelimit", Wait: 45 * time.Second, Msg: "the page redirects to other.example.com, which was contacted too recently; will retry later"}
	e.run(t, 0)
	w, _ := e.st.WatchByID(id, e.uid)
	if w.Failures != 0 || w.Status != "pending" {
		t.Fatalf("deferral counted as a failure: %+v", w)
	}
	if gap := w.NextCheckAt - e.clk.Now().Unix(); gap < 47 || gap > 60 {
		t.Fatalf("next check in %ds", gap)
	}
}

func TestTruncatedBodiesAreRejected(t *testing.T) {
	// the site announces 1000 bytes, sends 12, and closes
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil || line == "\r\n" {
						break
					}
				}
				c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/html\r\nContent-Length: 1000\r\nConnection: close\r\n\r\n<p>£99.00</p>"))
				c.Close()
			}()
		}
	}()
	srv := &httptest.Server{URL: "http://" + ln.Addr().String()}
	f := fetcherFor(srv, map[string]string{"shop.test": "127.0.0.1"})
	_, err = f.Fetch(context.Background(), "http://shop.test/p", nil)
	var fe *FetchError
	if !errors.As(err, &fe) || fe.Kind != "truncated" || !strings.Contains(fe.Msg, "not parsed") {
		t.Fatalf("partial body accepted: %v", err)
	}
	// via the checker: a transient error, and no price is extracted from the stump
	res, fail := CheckURL(context.Background(), f, "http://shop.test/p", "", nil)
	if res != nil || fail == nil || fail.Status != "error" {
		t.Fatalf("%+v %+v", res, fail)
	}
}

func TestOversizeBodyIsAFailureNotParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// a price sits inside the first 2 MB; the cut-off page must still not be parsed
		w.Write([]byte(`<meta itemprop="price" content="10.00">` + strings.Repeat("a", 3<<20)))
	}))
	defer srv.Close()
	f := fetcherFor(srv, map[string]string{"shop.test": "127.0.0.1"})
	res, fail := CheckURL(context.Background(), f, "http://shop.test/p", "", nil)
	if res != nil || fail == nil || fail.Status != "error" || !strings.Contains(fail.Reason, "larger than 2 MB") {
		t.Fatalf("%+v %+v", res, fail)
	}
}

func TestCompleteBodyWithMatchingLengthStillWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "39")
		w.Write([]byte(`<meta itemprop="price" content="10.00">`))
	}))
	defer srv.Close()
	f := fetcherFor(srv, map[string]string{"shop.test": "127.0.0.1"})
	res, fail := CheckURL(context.Background(), f, "http://shop.test/p", "", nil)
	if fail != nil || res.PricePence != 1000 {
		t.Fatalf("%+v %+v", res, fail)
	}
}
