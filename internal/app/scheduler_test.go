package app

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeFetcher struct {
	mu     sync.Mutex
	pages  map[string][]*Page // queue per URL; last element repeats
	errs   map[string]error
	calls  map[string]int
	hold   chan struct{}
	cur    atomic.Int32
	maxCur atomic.Int32
}

func newFakeFetcher() *fakeFetcher {
	return &fakeFetcher{pages: map[string][]*Page{}, errs: map[string]error{}, calls: map[string]int{}}
}

func pricePage(pence int64) *Page {
	return &Page{Status: 200, Header: http.Header{}, Body: []byte(fmt.Sprintf(`<html><meta itemprop="price" content="%d.%02d"></html>`, pence/100, pence%100))}
}

func (f *fakeFetcher) set(u string, pages ...*Page) {
	f.mu.Lock()
	f.pages[u] = pages
	delete(f.errs, u)
	f.mu.Unlock()
}

func (f *fakeFetcher) Fetch(ctx context.Context, u string) (*Page, error) {
	n := f.cur.Add(1)
	defer f.cur.Add(-1)
	for {
		m := f.maxCur.Load()
		if n <= m || f.maxCur.CompareAndSwap(m, n) {
			break
		}
	}
	if f.hold != nil {
		<-f.hold
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[u]++
	if err := f.errs[u]; err != nil {
		return nil, err
	}
	q := f.pages[u]
	if len(q) == 0 {
		return nil, &FetchError{Kind: "network", Msg: "no fixture"}
	}
	pg := q[0]
	if len(q) > 1 {
		f.pages[u] = q[1:]
	}
	c := *pg
	return &c, nil
}

type schedEnv struct {
	s   *Scheduler
	st  *Store
	clk *fakeClock
	f   *fakeFetcher
	m   *fakeMessenger
	snd *Sender
	uid int64
}

func newSchedEnv(t *testing.T) *schedEnv {
	st, clk := testStore(t)
	f, m := newFakeFetcher(), &fakeMessenger{}
	s := NewScheduler(st, f, clk, "https://ww.example")
	s.jitter = func(time.Duration) time.Duration { return 0 }
	return &schedEnv{s, st, clk, f, m, NewSender(st, m, clk, 200), mustUser(t, st, "+447700900123")}
}

func (e *schedEnv) add(t *testing.T, url string, target *int64, interval int) int64 {
	id, err := e.st.AddWatch(Watch{UserID: e.uid, URL: url, Label: "L", TargetPence: target, IntervalMin: interval})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *schedEnv) run(t *testing.T, advance time.Duration) int {
	e.clk.Advance(advance)
	n := e.s.RunDue(context.Background())
	e.snd.RunOnce(context.Background())
	return n
}

func TestSchedulerChangeAndBelowTarget(t *testing.T) {
	e := newSchedEnv(t)
	const u = "https://shop.example.com/p"
	id := e.add(t, u, p(10000), 60)
	e.f.set(u, pricePage(12000), pricePage(12000), pricePage(11000), pricePage(9900), pricePage(9500), pricePage(10500), pricePage(9900))

	// 1: first check, above target: no alert, history recorded
	if e.run(t, 0) != 1 {
		t.Fatal("first check not run")
	}
	w, _ := e.st.WatchByID(id, e.uid)
	if w.Status != "ok" || *w.LastPricePence != 12000 || w.LastMethod != "meta" || len(e.m.Sent()) != 0 {
		t.Fatalf("%+v sent=%d", w, len(e.m.Sent()))
	}
	// not due again until the interval passes
	if e.run(t, 10*time.Minute) != 0 {
		t.Fatal("ran early")
	}
	// 2: unchanged
	e.run(t, time.Hour)
	if len(e.m.Sent()) != 0 {
		t.Fatal("alert on unchanged price")
	}
	// 3: 12000 -> 11000 : changed
	e.run(t, time.Hour)
	sent := e.m.Sent()
	if len(sent) != 1 || sent[0].To != "+447700900123" || sent[0].Kind != KindAlert || !contains(sent[0].Text, "£120.00 → £110.00") {
		t.Fatalf("%+v", sent)
	}
	// 4: 11000 -> 9900: below target (single message, not also "changed")
	e.run(t, time.Hour)
	sent = e.m.Sent()
	if len(sent) != 2 || !contains(sent[1].Text, "below target") || !contains(sent[1].Text, "£99.00") {
		t.Fatalf("%+v", sent)
	}
	// 5: 9900 -> 9500: changed
	e.run(t, time.Hour)
	if len(e.m.Sent()) != 3 || !contains(e.m.Sent()[2].Text, "price changed") {
		t.Fatalf("%+v", e.m.Sent())
	}
	// 6: 9500 -> 10500 : changed ; 7: -> 9900 : below, but same price as the last below alert -> suppressed
	e.run(t, time.Hour)
	e.run(t, time.Hour)
	if n := len(e.m.Sent()); n != 4 {
		t.Fatalf("sent %d, want 4 (below repeat for same price suppressed)", n)
	}
	h, _ := e.st.History(id, 100)
	if len(h) != 7 {
		t.Fatalf("history %d", len(h))
	}
}

func TestSchedulerFirstCheckBelowTarget(t *testing.T) {
	e := newSchedEnv(t)
	const u = "https://shop.example.com/p"
	e.add(t, u, p(10000), 60)
	e.f.set(u, pricePage(9000))
	e.run(t, 0)
	if s := e.m.Sent(); len(s) != 1 || !contains(s[0].Text, "below target") {
		t.Fatalf("%+v", s)
	}
}

func TestSchedulerBlockedBackoffAndStop(t *testing.T) {
	e := newSchedEnv(t)
	const u = "https://www.jet2.com/p"
	id := e.add(t, u, nil, 60)
	e.f.set(u, &Page{Status: 403, Header: http.Header{}})

	e.run(t, 0)
	w, _ := e.st.WatchByID(id, e.uid)
	if w.Status != "blocked" || w.LastError != "blocked by site (HTTP 403)" || w.Failures != 1 {
		t.Fatalf("%+v", w)
	}
	// backoff: first retry no sooner than 2h
	gap1 := w.NextCheckAt - e.clk.Now().Unix()
	if gap1 < 2*3600 {
		t.Fatalf("backoff too short: %ds", gap1)
	}
	e.run(t, 59*time.Minute)
	if e.f.calls[u] != 1 {
		t.Fatal("retried before backoff elapsed")
	}
	prevGap := gap1
	for i := 2; i <= MaxFailures; i++ {
		e.run(t, 48*time.Hour)
		w, _ = e.st.WatchByID(id, e.uid)
		if w.Failures != i {
			t.Fatalf("failures %d want %d", w.Failures, i)
		}
		g := w.NextCheckAt - e.clk.Now().Unix()
		if g < prevGap {
			t.Fatalf("backoff shrank: %d < %d", g, prevGap)
		}
		prevGap = g
	}
	if prevGap > 24*3600+1 {
		t.Fatalf("backoff above cap: %d", prevGap)
	}
	sent := e.m.Sent()
	if len(sent) != 1 || !contains(sent[0].Text, "stopped") {
		t.Fatalf("want one stop notice, got %+v", sent)
	}
	calls := e.f.calls[u]
	e.run(t, 100*time.Hour)
	if e.f.calls[u] != calls || len(e.m.Sent()) != 1 {
		t.Fatal("stopped watch kept running or re-notified")
	}
	// resume restarts it
	if err := e.st.SetPaused(e.uid, id, false); err != nil {
		t.Fatal(err)
	}
	e.f.set(u, pricePage(5000))
	e.run(t, 0)
	w, _ = e.st.WatchByID(id, e.uid)
	if w.Status != "ok" || w.Failures != 0 {
		t.Fatalf("%+v", w)
	}
}

func TestSchedulerFailureReasons(t *testing.T) {
	e := newSchedEnv(t)
	cases := map[string]struct {
		pg     *Page
		status string
		reason string
	}{
		"https://a.example.com/": {&Page{Status: 429, Header: http.Header{}}, "blocked", "blocked by site (HTTP 429)"},
		"https://b.example.com/": {&Page{Status: 200, Header: http.Header{}, Body: fixture(t, "spa.html")}, "error", "needs browser rendering (the page builds its prices with JavaScript)"},
		"https://c.example.com/": {&Page{Status: 200, Header: http.Header{}, Body: fixture(t, "noprice.html")}, "error", "no price found"},
	}
	ids := map[string]int64{}
	for u, c := range cases {
		ids[u] = e.add(t, u, nil, 60)
		e.f.set(u, c.pg)
	}
	e.run(t, 0)
	for u, c := range cases {
		w, _ := e.st.WatchByID(ids[u], e.uid)
		if w.Status != c.status || w.LastError != c.reason {
			t.Errorf("%s: %s / %s", u, w.Status, w.LastError)
		}
	}
}

func TestSchedulerPerHostGap(t *testing.T) {
	e := newSchedEnv(t)
	a := e.add(t, "https://www.tui.co.uk/a", nil, 60)
	b := e.add(t, "https://www.tui.co.uk/b", nil, 60)
	e.f.set("https://www.tui.co.uk/a", pricePage(1000))
	e.f.set("https://www.tui.co.uk/b", pricePage(2000))
	if n := e.run(t, 0); n != 1 {
		t.Fatalf("expected one fetch for same host, got %d", n)
	}
	wb, _ := e.st.WatchByID(b, e.uid)
	if wb.Failures != 0 || wb.Status != "pending" {
		t.Fatalf("deferral must not count as failure: %+v", wb)
	}
	if n := e.run(t, 70*time.Second); n != 1 {
		t.Fatalf("deferred watch should run after gap, got %d", n)
	}
	wa, _ := e.st.WatchByID(a, e.uid)
	wb, _ = e.st.WatchByID(b, e.uid)
	if wa.Status != "ok" || wb.Status != "ok" {
		t.Fatalf("%s %s", wa.Status, wb.Status)
	}
}

func TestSchedulerConcurrencyLimitAndPause(t *testing.T) {
	e := newSchedEnv(t)
	e.f.hold = make(chan struct{})
	for i := 0; i < 10; i++ {
		u := fmt.Sprintf("https://h%d.example.com/p", i)
		e.add(t, u, nil, 60)
		e.f.set(u, pricePage(1000))
	}
	paused := e.add(t, "https://paused.example.com/p", nil, 60)
	e.st.SetPaused(e.uid, paused, true)
	done := make(chan int)
	go func() { done <- e.s.RunDue(context.Background()) }()
	time.Sleep(200 * time.Millisecond)
	if c := e.f.cur.Load(); c != MaxConcurrent {
		t.Errorf("in flight %d, want %d", c, MaxConcurrent)
	}
	for i := 0; i < 10; i++ {
		e.f.hold <- struct{}{}
	}
	if n := <-done; n != 10 {
		t.Fatalf("ran %d", n)
	}
	if e.f.maxCur.Load() > MaxConcurrent {
		t.Fatalf("max concurrency %d", e.f.maxCur.Load())
	}
	if e.f.calls["https://paused.example.com/p"] != 0 {
		t.Fatal("paused watch fetched")
	}
}

func TestSetTargetResetsBaseline(t *testing.T) {
	e := newSchedEnv(t)
	const u = "https://shop.example.com/p"
	id := e.add(t, u, nil, 60)
	e.f.set(u, pricePage(9000))
	e.run(t, 0)
	if len(e.m.Sent()) != 0 {
		t.Fatal("unexpected alert")
	}
	e.st.SetTarget(e.uid, id, p(10000))
	e.run(t, time.Hour)
	if s := e.m.Sent(); len(s) != 1 || !contains(s[0].Text, "below target") {
		t.Fatalf("%+v", s)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
