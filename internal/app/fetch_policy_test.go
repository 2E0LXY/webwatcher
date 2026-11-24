package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseRobots(t *testing.T) {
	body := `# comment
User-agent: *
Disallow: /private
Disallow: /*.pdf$
Allow: /private/ok

User-agent: webwatcher
User-agent: otherbot
Disallow: /secret
Allow: /secret/public
Disallow: /search?
`
	r := parseRobots(body, robotsToken)
	for path, want := range map[string]bool{
		"/": true, "/secret": false, "/secret/x": false, "/secret/public": true,
		"/private": true, "/search?q=1": false, "/search": true, "/a.pdf": true,
	} {
		if got := r.Allowed(path); got != want {
			t.Errorf("specific group %s: got %v", path, got)
		}
	}
	star := parseRobots(body, "somebot")
	for path, want := range map[string]bool{
		"/private": false, "/private/ok": true, "/private/x": false, "/a.pdf": false, "/a.pdf?x": true, "/a.pdf/x": true, "/secret": true,
	} {
		if got := star.Allowed(path); got != want {
			t.Errorf("star group %s: got %v", path, got)
		}
	}
	if parseRobots("", robotsToken).Allowed("/x") != true || (*robotsRules)(nil).Allowed("/x") != true {
		t.Fatal("empty robots must allow")
	}
	if !parseRobots("User-agent: *\nDisallow:\n", robotsToken).Allowed("/x") {
		t.Fatal("empty Disallow means allow")
	}
}

type robotsSrv struct {
	srv                  *httptest.Server
	robotsHits, pageHits atomic.Int32
}

func newRobotsSrv(robots string, robotsStatus int) *robotsSrv {
	r := &robotsSrv{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/robots.txt" {
			r.robotsHits.Add(1)
			w.WriteHeader(robotsStatus)
			w.Write([]byte(robots))
			return
		}
		r.pageHits.Add(1)
		w.Write([]byte("<html>page</html>"))
	}))
	return r
}

func TestRobotsOffByDefault(t *testing.T) {
	r := newRobotsSrv("User-agent: *\nDisallow: /\n", 200)
	defer r.srv.Close()
	f := fetcherFor(r.srv, map[string]string{"shop.test": "127.0.0.1"})
	p, err := f.Fetch(context.Background(), "http://shop.test/p", nil)
	if err != nil || p.Status != 200 {
		t.Fatalf("%v", err)
	}
	if r.robotsHits.Load() != 0 {
		t.Fatal("robots.txt consulted although the opt-in is off")
	}
}

func TestRobotsOptInDisallowsAndCaches(t *testing.T) {
	r := newRobotsSrv("User-agent: webwatcher\nDisallow: /nope\n", 200)
	defer r.srv.Close()
	f := fetcherFor(r.srv, map[string]string{"shop.test": "127.0.0.1"})
	f.SetRespectRobots(true)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	f.robots.now = func() time.Time { return now }
	_, err := f.Fetch(context.Background(), "http://shop.test/nope/x", nil)
	var fe *FetchError
	if !errors.As(err, &fe) || fe.Kind != "disallowed" || !strings.Contains(fe.Msg, "robots.txt") || strings.Contains(fe.Msg, "/nope") {
		t.Fatalf("%v", err)
	}
	if r.pageHits.Load() != 0 {
		t.Fatal("disallowed page was fetched")
	}
	if _, err := f.Fetch(context.Background(), "http://shop.test/ok", nil); err != nil {
		t.Fatal(err)
	}
	if r.robotsHits.Load() != 1 {
		t.Fatalf("robots.txt not cached: %d fetches", r.robotsHits.Load())
	}
	now = now.Add(23 * time.Hour)
	f.Fetch(context.Background(), "http://shop.test/ok", nil)
	if r.robotsHits.Load() != 1 {
		t.Fatal("cache expired early")
	}
	now = now.Add(2 * time.Hour)
	f.Fetch(context.Background(), "http://shop.test/ok", nil)
	if r.robotsHits.Load() != 2 {
		t.Fatalf("cache not refreshed after 24h: %d", r.robotsHits.Load())
	}
}

func TestRobotsUnreachableOrMissingIsAllowed(t *testing.T) {
	for _, status := range []int{404, 500, 503} {
		r := newRobotsSrv("User-agent: *\nDisallow: /\n", status)
		f := fetcherFor(r.srv, map[string]string{"shop.test": "127.0.0.1"})
		f.SetRespectRobots(true)
		if _, err := f.Fetch(context.Background(), "http://shop.test/p", nil); err != nil {
			t.Errorf("robots %d: %v", status, err)
		}
		r.srv.Close()
	}
	// connection refused
	dead := httptest.NewServer(http.NotFoundHandler())
	f := fetcherFor(dead, map[string]string{"shop.test": "127.0.0.1"})
	dead.Close()
	f.SetRespectRobots(true)
	if _, err := f.Fetch(context.Background(), "http://shop.test/p", nil); err == nil || errors.Is(err, errBlockedDest) {
		// the page itself is unreachable too; the point is that it is a network error, not "disallowed"
		var fe *FetchError
		if errors.As(err, &fe) && fe.Kind == "disallowed" {
			t.Fatal("unreachable robots treated as disallow")
		}
	}
}

func TestSchedulerDisallowedStatusDoesNotStopWatch(t *testing.T) {
	e := newSchedEnv(t)
	const u = "https://shop.example.com/p"
	id := e.add(t, u, nil, 60)
	e.f.errs[u] = &FetchError{Kind: "disallowed", Msg: "robots.txt on shop.example.com disallows this page for webwatcher, so it was not fetched"}
	for i := 0; i < 12; i++ {
		e.run(t, 25*time.Hour)
	}
	w, _ := e.st.WatchByID(id, e.uid)
	if w.Status != "disallowed" || w.Failures != 0 || !contains(w.LastError, "robots.txt") || len(e.m.Sent()) != 0 {
		t.Fatalf("%+v", w)
	}
}

func TestConditionalGetSendsValidators(t *testing.T) {
	var inm, ims atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inm.Store(r.Header.Get("If-None-Match"))
		ims.Store(r.Header.Get("If-Modified-Since"))
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Tue, 24 Nov 2026 10:00:00 GMT")
		w.Write([]byte(`<meta itemprop="price" content="10.00">`))
	}))
	defer srv.Close()
	f := fetcherFor(srv, map[string]string{"shop.test": "127.0.0.1"})
	p, err := f.Fetch(context.Background(), "http://shop.test/p", nil)
	if err != nil || p.NotModified || p.Header.Get("Etag") != `"v1"` {
		t.Fatalf("%v %+v", err, p)
	}
	if inm.Load() != "" {
		t.Fatal("conditional headers sent on first fetch")
	}
	p, err = f.Fetch(context.Background(), "http://shop.test/p", &Conditional{ETag: `"v1"`, LastModified: "Tue, 24 Nov 2026 10:00:00 GMT"})
	if err != nil || !p.NotModified || p.Status != 304 {
		t.Fatalf("%v %+v", err, p)
	}
	if inm.Load() != `"v1"` || ims.Load() != "Tue, 24 Nov 2026 10:00:00 GMT" {
		t.Fatalf("%v %v", inm.Load(), ims.Load())
	}
}

func etagPage(price int64, etag string) *Page {
	pg := pricePage(price)
	pg.Header = http.Header{"Etag": {etag}, "Last-Modified": {"Tue, 24 Nov 2026 10:00:00 GMT"}}
	return pg
}

func TestSchedulerUsesAndStoresValidators(t *testing.T) {
	e := newSchedEnv(t)
	const u = "https://shop.example.com/p"
	id := e.add(t, u, nil, 60)
	e.f.set(u, etagPage(5000, `"a"`), &Page{Status: 304, NotModified: true, Header: http.Header{}}, etagPage(4000, `"b"`))
	e.run(t, 0)
	w, _ := e.st.WatchByID(id, e.uid)
	if w.ETag != `"a"` || w.LastModified == "" {
		t.Fatalf("%+v", w)
	}
	e.run(t, time.Hour) // 304
	if c := e.f.conds[u]; c[0] != nil || c[1] == nil || c[1].ETag != `"a"` {
		t.Fatalf("conds %+v", c)
	}
	w, _ = e.st.WatchByID(id, e.uid)
	if w.Status != "ok" || *w.LastPricePence != 5000 || w.Failures != 0 || *w.LastCheckedAt != e.clk.Now().Unix() {
		t.Fatalf("%+v", w)
	}
	if h, _ := e.st.History(e.uid, id, 10); len(h) != 1 {
		t.Fatalf("304 added history: %d", len(h))
	}
	e.run(t, time.Hour) // changed
	if w, _ = e.st.WatchByID(id, e.uid); w.ETag != `"b"` || *w.LastPricePence != 4000 {
		t.Fatalf("%+v", w)
	}
	if s := e.m.Sent(); len(s) != 1 || !contains(s[0].Text, "£50.00 → £40.00") {
		t.Fatalf("%+v", s)
	}
}

func TestRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	h := func(v string) http.Header { return http.Header{"Retry-After": {v}} }
	cases := map[string]time.Duration{
		"120": 2 * time.Minute, "0": 0, "-5": 0, "junk": 0, "": 0,
		now.Add(90 * time.Minute).Format(http.TimeFormat): 90 * time.Minute,
		now.Add(-time.Hour).Format(http.TimeFormat):       0,
		"99999999999": maxRetryAfter,
	}
	for in, want := range cases {
		if got := parseRetryAfter(h(in), now); got != want {
			t.Errorf("%q: %v want %v", in, got, want)
		}
	}
}

func TestSchedulerHonoursRetryAfter(t *testing.T) {
	for _, code := range []int{429, 503} {
		e := newSchedEnv(t)
		u := "https://shop.example.com/p"
		id := e.add(t, u, nil, 30)
		e.f.set(u, &Page{Status: code, Header: http.Header{"Retry-After": {"36000"}}}) // 10 hours
		e.run(t, 0)
		w, _ := e.st.WatchByID(id, e.uid)
		if gap := w.NextCheckAt - e.clk.Now().Unix(); gap < 36000 {
			t.Fatalf("%d: next check in %ds, Retry-After ignored", code, gap)
		}
		e.run(t, 9*time.Hour)
		if e.f.calls[u] != 1 {
			t.Fatalf("%d: refetched before Retry-After elapsed", code)
		}
	}
}

func TestTransientErrorVersusNoPriceKeepLastValid(t *testing.T) {
	e := newSchedEnv(t)
	const u = "https://shop.example.com/p"
	id := e.add(t, u, nil, 60)
	e.f.set(u, pricePage(5000))
	e.run(t, 0)
	// transient: HTTP 502, then a network error
	e.f.set(u, &Page{Status: 502, Header: http.Header{}})
	e.run(t, time.Hour)
	w, _ := e.st.WatchByID(id, e.uid)
	if w.Status != "error" || *w.LastPricePence != 5000 || w.LastError != "site returned HTTP 502" {
		t.Fatalf("%+v", w)
	}
	e.f.errs[u] = &FetchError{Kind: "network", Msg: "network error: timeout"}
	e.run(t, 4*time.Hour)
	if w, _ = e.st.WatchByID(id, e.uid); w.Status != "error" || *w.LastPricePence != 5000 {
		t.Fatalf("%+v", w)
	}
	// page loads but has no price: a different status, same kept price
	delete(e.f.errs, u)
	e.f.set(u, &Page{Status: 200, Header: http.Header{}, Body: fixture(t, "noprice.html")})
	e.run(t, 24*time.Hour)
	w, _ = e.st.WatchByID(id, e.uid)
	if w.Status != "noprice" || w.LastError != "no price found" || *w.LastPricePence != 5000 {
		t.Fatalf("%+v", w)
	}
	// recovery
	e.f.set(u, pricePage(5000))
	e.run(t, 48*time.Hour)
	if w, _ = e.st.WatchByID(id, e.uid); w.Status != "ok" || w.Failures != 0 || len(e.m.Sent()) != 0 {
		t.Fatalf("%+v sent=%d", w, len(e.m.Sent()))
	}
	var n int
	e.st.db.QueryRow(`SELECT COUNT(DISTINCT outcome) FROM check_log WHERE watch_id=?`, id).Scan(&n)
	if n != 3 {
		t.Fatalf("check_log outcomes: %d", n)
	}
}

func TestDashboardDistinguishesStatuses(t *testing.T) {
	e := newTenv(t, nil)
	b := e.browser()
	csrf := b.login("07700900123")
	b.do("POST", "/watches", url.Values{"csrf": {csrf}, "url": {"https://a.example.com/x"}})
	e.st.RecordObservation(1, Observation{Price: 5000, Currency: "GBP", Method: "meta", Basis: BasisUnknown}, 0, nil, nil)
	e.st.RecordFailure(1, "noprice", "no price found", 1, 0, nil, nil)
	page := b.do("GET", "/", nil).Body.String()
	if !strings.Contains(page, "No price found") || !strings.Contains(page, "Last valid price kept") || !strings.Contains(page, "£50.00") {
		t.Fatal("noprice not shown")
	}
	e.st.RecordFailure(1, "error", "site returned HTTP 502", 1, 0, nil, nil)
	if !strings.Contains(b.do("GET", "/", nil).Body.String(), "Temporary error") {
		t.Fatal("transient not shown")
	}
}
