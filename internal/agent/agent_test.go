package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeBrowser struct {
	mu    sync.Mutex
	pages map[string]*Rendered // by URL
	errs  map[string]error
	loads []string
	at    []time.Time
	clk   *fakeClock
}

func (b *fakeBrowser) Load(ctx context.Context, u string) (*Rendered, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.loads = append(b.loads, u)
	b.at = append(b.at, b.clk.Now())
	if err := b.errs[u]; err != nil {
		return nil, err
	}
	if p := b.pages[u]; p != nil {
		c := *p
		return &c, nil
	}
	return &Rendered{HTML: "<html>£100</html>", FinalURL: u, Status: 200}, nil
}
func (b *fakeBrowser) Close() error { return nil }

type fakeServer struct {
	mu        sync.Mutex
	jobs      []Job
	jobsErr   error
	submitErr error
	results   []*Result
	polls     int
}

func (s *fakeServer) Jobs(context.Context) (*JobList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.polls++
	if s.jobsErr != nil {
		return nil, s.jobsErr
	}
	return &JobList{PollAfterSec: 60, Jobs: append([]Job(nil), s.jobs...)}, nil
}
func (s *fakeServer) Submit(_ context.Context, r *Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.submitErr != nil {
		return s.submitErr
	}
	c := *r
	s.results = append(s.results, &c)
	return nil
}

type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	sleeps []time.Duration
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.sleeps = append(c.sleeps, d)
	c.mu.Unlock()
	return nil
}

type env struct {
	a   *Agent
	srv *fakeServer
	br  *fakeBrowser
	clk *fakeClock
	log *bytes.Buffer
}

func newEnv(t *testing.T, jobs ...Job) *env {
	clk := &fakeClock{t: time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)}
	srv := &fakeServer{jobs: jobs}
	br := &fakeBrowser{pages: map[string]*Rendered{}, errs: map[string]error{}, clk: clk}
	var buf bytes.Buffer
	a := New(srv, br, Options{Now: clk.Now, Sleep: clk.Sleep, Rand: func(n int64) int64 { return n / 2 },
		Logger: slog.New(slog.NewTextHandler(&buf, nil))})
	return &env{a, srv, br, clk, &buf}
}

func job(id int64, u string, minDelay int) Job {
	return Job{WatchID: id, URL: u, Version: 3, IntervalMin: minDelay / 60, MinDelaySec: minDelay}
}

func TestAgentLoadsAndReports(t *testing.T) {
	e := newEnv(t, job(1, "https://www.jet2.com/a", 3600))
	e.br.pages["https://www.jet2.com/a"] = &Rendered{HTML: "<html>£499</html>", FinalURL: "https://www.jet2.com/a?x=1", Status: 200, LoadMS: 900, IdleMS: 100}
	e.a.Cycle(context.Background())
	if len(e.srv.results) != 1 {
		t.Fatalf("results %d", len(e.srv.results))
	}
	r := e.srv.results[0]
	if r.Status != "ok" || r.WatchID != 1 || r.Version != 3 || r.RenderedHTML != "<html>£499</html>" || r.HTTPStatus != 200 || r.Timings.LoadMS != 900 {
		t.Fatalf("%+v", r)
	}
}

func TestAgentJitterBeforeEachLoad(t *testing.T) {
	e := newEnv(t, job(1, "https://a.example/x", 3600), job(2, "https://b.example/y", 3600), job(3, "https://c.example/z", 3600))
	e.a.Cycle(context.Background())
	if len(e.br.loads) != 3 || len(e.clk.sleeps) != 3 {
		t.Fatalf("loads %d sleeps %v", len(e.br.loads), e.clk.sleeps)
	}
	for _, s := range e.clk.sleeps {
		if s < DefaultJitterMin || s > DefaultJitterMax {
			t.Errorf("jitter %v outside 5-60 s", s)
		}
	}
	// one at a time: loads are spaced by at least the minimum jitter
	for i := 1; i < len(e.br.at); i++ {
		if e.br.at[i].Sub(e.br.at[i-1]) < DefaultJitterMin {
			t.Errorf("loads %d and %d too close", i-1, i)
		}
	}
	// extremes of the random source stay inside the range
	for _, r := range []int64{0, 1<<62 - 1} {
		e2 := newEnv(t, job(1, "https://a.example/x", 3600))
		e2.a.o.Rand = func(n int64) int64 { return min(r, n-1) }
		e2.a.Cycle(context.Background())
		if s := e2.clk.sleeps[0]; s < DefaultJitterMin || s > DefaultJitterMax {
			t.Errorf("jitter %v", s)
		}
	}
}

func TestAgentPerHostSpacing(t *testing.T) {
	e := newEnv(t, job(1, "https://www.jet2.com/a", 600), job(2, "https://www.jet2.com/b", 600), job(3, "https://www.easyjet.com/c", 600))
	e.a.Cycle(context.Background())
	// the second jet2 page waits: same host within five minutes
	if got := strings.Join(e.br.loads, " "); got != "https://www.jet2.com/a https://www.easyjet.com/c" {
		t.Fatalf("loads: %s", got)
	}
	e.clk.Sleep(context.Background(), 4*time.Minute) // still under five minutes since the jet2 load
	e.a.Cycle(context.Background())
	if len(e.br.loads) != 2 {
		t.Fatalf("host loaded again after 4 min: %v", e.br.loads)
	}
	e.clk.Sleep(context.Background(), 2*time.Minute)
	e.a.Cycle(context.Background())
	// 6 min after the first jet2 load: /b may now load
	if len(e.br.loads) != 3 || e.br.loads[2] != "https://www.jet2.com/b" {
		t.Fatalf("loads: %v", e.br.loads)
	}
	// never closer than five minutes on one host
	var jet []time.Time
	for i, u := range e.br.loads {
		if strings.Contains(u, "jet2") {
			jet = append(jet, e.br.at[i])
		}
	}
	if len(jet) != 2 || jet[1].Sub(jet[0]) < DefaultHostGap {
		t.Fatalf("jet2 spacing %v", jet)
	}
}

func TestAgentHonoursInterval(t *testing.T) {
	// the server keeps offering the job (for instance because it is slow to record); the agent loads it once per interval
	e := newEnv(t, job(1, "https://shop.example/p", 1800))
	for i := 0; i < 5; i++ {
		e.a.Cycle(context.Background())
		e.clk.Sleep(context.Background(), 6*time.Minute) // past the host gap, inside the interval
	}
	if len(e.br.loads) != 1 {
		t.Fatalf("loaded %d times inside the interval", len(e.br.loads))
	}
	e.clk.Sleep(context.Background(), 31*time.Minute)
	e.a.Cycle(context.Background())
	if len(e.br.loads) != 2 {
		t.Fatalf("not loaded after the interval: %d", len(e.br.loads))
	}
}

func TestAgentChallengeBacksOffSixHours(t *testing.T) {
	e := newEnv(t, job(1, "https://www.easyjet.com/a", 1800), job(2, "https://www.easyjet.com/b", 1800), job(3, "https://www.tui.co.uk/c", 1800))
	e.br.pages["https://www.easyjet.com/a"] = &Rendered{HTML: `<title>Access Denied</title>`, Status: 403, FinalURL: "https://www.easyjet.com/a?token=SECRET"}
	e.a.Cycle(context.Background())
	if len(e.srv.results) != 2 {
		t.Fatalf("results %d", len(e.srv.results))
	}
	r := e.srv.results[0]
	if r.Status != "blocked" || r.FailureClass != "challenge" || r.RenderedHTML != "" || strings.Contains(r.FinalURL, "SECRET") {
		t.Fatalf("%+v", r)
	}
	if e.srv.results[1].WatchID != 3 {
		t.Fatalf("other host skipped: %+v", e.srv.results[1])
	}
	loads := len(e.br.loads)
	// five hours later the host is still left alone, other watches on it too
	for i := 0; i < 5; i++ {
		e.clk.Sleep(context.Background(), time.Hour)
		e.a.Cycle(context.Background())
	}
	for _, u := range e.br.loads[loads:] {
		if strings.Contains(u, "easyjet") {
			t.Fatalf("easyjet loaded during the back-off: %v", e.br.loads)
		}
	}
	e.clk.Sleep(context.Background(), 70*time.Minute)
	e.br.pages["https://www.easyjet.com/a"] = &Rendered{HTML: "<html>£100</html>", Status: 200, FinalURL: "https://www.easyjet.com/a"}
	e.a.Cycle(context.Background())
	found := false
	for _, u := range e.br.loads[loads:] {
		found = found || strings.Contains(u, "easyjet")
	}
	if !found {
		t.Fatal("host not retried after the back-off")
	}
}

func TestAgentTransientErrorsAreClassified(t *testing.T) {
	for _, class := range []string{"dns", "connect", "tls", "timeout", "nav_error"} {
		e := newEnv(t, job(1, "https://shop.example/p", 1800))
		e.br.errs["https://shop.example/p"] = &LoadError{Class: class, Err: errors.New("boom https://shop.example/p?secret=1")}
		e.a.Cycle(context.Background())
		r := e.srv.results[0]
		if r.Status != "error" || r.FailureClass != class || r.RenderedHTML != "" {
			t.Errorf("%s: %+v", class, r)
		}
		// no immediate retry: the same job is not loaded again in the next poll
		e.a.Cycle(context.Background())
		if len(e.br.loads) != 1 {
			t.Errorf("%s: retried immediately", class)
		}
		if strings.Contains(e.log.String(), "secret=1") {
			t.Errorf("%s: query string in the log: %s", class, e.log.String())
		}
	}
	// an unclassified browser error is "other"
	e := newEnv(t, job(1, "https://shop.example/p", 1800))
	e.br.errs["https://shop.example/p"] = errors.New("browser died")
	e.a.Cycle(context.Background())
	if r := e.srv.results[0]; r.Status != "error" || r.FailureClass != "other" {
		t.Fatalf("%+v", r)
	}
}

func TestAgentRefusesNonHTTPURLs(t *testing.T) {
	for _, u := range []string{"file:///etc/passwd", "chrome://settings", "javascript:alert(1)", "data:text/html,x", "about:blank", "ftp://x.example/"} {
		e := newEnv(t, job(1, u, 1800))
		e.a.Cycle(context.Background())
		if len(e.br.loads) != 0 {
			t.Errorf("%s: browser was asked to load it", u)
		}
		if len(e.srv.results) != 1 || e.srv.results[0].Status != "error" {
			t.Errorf("%s: %+v", u, e.srv.results)
		}
	}
}

func TestChromeRefusesNonHTTPURLs(t *testing.T) {
	c := NewChrome(ChromeOptions{Path: "/nonexistent"})
	for _, u := range []string{"file:///etc/passwd", "chrome://version", "javascript:1", "data:text/html,x"} {
		_, err := c.Load(context.Background(), u)
		var le *LoadError
		if !errors.As(err, &le) || !errors.Is(err, errRefused) {
			t.Errorf("%s: %v", u, err)
		}
	}
}

func TestAgentSubmitFailureIsRetried(t *testing.T) {
	e := newEnv(t, job(1, "https://shop.example/p", 3600))
	e.srv.submitErr = errors.New("server down")
	e.a.Cycle(context.Background())
	if len(e.srv.results) != 0 || len(e.br.loads) != 1 {
		t.Fatal("setup")
	}
	e.srv.submitErr = nil
	e.clk.Sleep(context.Background(), 6*time.Minute) // past the host gap
	e.a.Cycle(context.Background())
	if len(e.srv.results) != 1 {
		t.Fatalf("result lost: loads %d results %d", len(e.br.loads), len(e.srv.results))
	}
}

func TestAgentServerErrors(t *testing.T) {
	e := newEnv(t, job(1, "https://shop.example/p", 3600))
	e.srv.jobsErr = ErrRejected
	if w := e.a.Cycle(context.Background()); w != DefaultAuthBackoff || len(e.br.loads) != 0 {
		t.Fatalf("rejected: wait %v loads %d", w, len(e.br.loads))
	}
	if !strings.Contains(e.log.String(), "rejected") {
		t.Fatal("rejection not logged")
	}
	e.srv.jobsErr = &RateLimitedError{After: 10 * time.Minute}
	if w := e.a.Cycle(context.Background()); w != 10*time.Minute {
		t.Fatalf("rate limited: wait %v", w)
	}
	e.srv.jobsErr = errors.New("connection refused")
	if w := e.a.Cycle(context.Background()); w != DefaultPollInterval {
		t.Fatalf("down: wait %v", w)
	}
	e.srv.jobsErr = nil
	if w := e.a.Cycle(context.Background()); w != 60*time.Second {
		t.Fatalf("normal poll wait %v", w)
	}
}

func TestAgentOversizedPage(t *testing.T) {
	filler := strings.Repeat("<script>var a='"+strings.Repeat("z", 1000)+"'</script>", 3000) // ~3 MB of removable script
	ld := `<script type="application/ld+json">{"offers":{"price":"10.00"}}</script>`
	e := newEnv(t, job(1, "https://shop.example/p", 3600), job(2, "https://other.example/p", 3600))
	e.br.pages["https://shop.example/p"] = &Rendered{HTML: "<html><head>" + ld + "</head><body>" + filler + "</body></html>", Status: 200, FinalURL: "https://shop.example/p"}
	e.br.pages["https://other.example/p"] = &Rendered{HTML: "<html><body>" + strings.Repeat("<p>real content</p>", 200000) + "</body></html>", Status: 200, FinalURL: "https://other.example/p"}
	e.a.Cycle(context.Background())
	if len(e.srv.results) != 2 {
		t.Fatal(len(e.srv.results))
	}
	r := e.srv.results[0]
	if r.Status != "ok" || len(r.RenderedHTML) > MaxHTMLBytes || !strings.Contains(r.RenderedHTML, `"price":"10.00"`) {
		t.Fatalf("slimmed page: %s %d", r.Status, len(r.RenderedHTML))
	}
	if r := e.srv.results[1]; r.Status != "error" || r.FailureClass != "too_large" || r.RenderedHTML != "" {
		t.Fatalf("still too large: %+v", r.Status)
	}
}

func TestAgentStopsOnCancel(t *testing.T) {
	e := newEnv(t, job(1, "https://a.example/x", 3600), job(2, "https://b.example/y", 3600))
	ctx, cancel := context.WithCancel(context.Background())
	e.a.o.Sleep = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }
	if err := e.a.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(e.br.loads) != 0 || len(e.srv.results) != 0 {
		t.Fatalf("work after cancel: %d %d", len(e.br.loads), len(e.srv.results))
	}
}

func TestOnResultAndLogsHoldNoSecrets(t *testing.T) {
	e := newEnv(t, job(1, "https://www.jet2.com/holiday?booking=ABC123&email=a@b.c", 3600))
	var seen []string
	e.a.o.OnResult = func(host, status, class string, at time.Time) { seen = append(seen, host+"|"+status) }
	e.a.Cycle(context.Background())
	if len(seen) != 1 || seen[0] != "www.jet2.com|ok" {
		t.Fatalf("%v", seen)
	}
	l := e.log.String()
	for _, bad := range []string{"ABC123", "a@b.c", "booking=", "?"} {
		if strings.Contains(l, bad) {
			t.Errorf("log contains %q:\n%s", bad, l)
		}
	}
	if !strings.Contains(l, "www.jet2.com/holiday") {
		t.Fatalf("expected the host and path in the log:\n%s", l)
	}
}
