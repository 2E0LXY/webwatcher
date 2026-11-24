package app

import (
	"net/http"
	"testing"
	"time"
)

func headerTimeout() error {
	return &FetchError{Kind: "network", Class: ClassHeaderTimeout, Msg: "network error: net/http: timeout awaiting response headers"}
}

func gapOf(e *schedEnv, id int64) time.Duration {
	w, _ := e.st.WatchByID(id, e.uid)
	return time.Duration(w.NextCheckAt-e.clk.Now().Unix()) * time.Second
}

func TestQuickRetriesThenNormalIntervalThenBackoff(t *testing.T) {
	e := newSchedEnv(t)
	const u = "https://www.jet2holidays.com/p"
	id := e.add(t, u, nil, 360) // 6 hour interval, as in the live failure
	e.f.errs[u] = headerTimeout()

	// One failure must not cost 12 hours: the first quick retry is 2 minutes away.
	e.run(t, 0)
	w, _ := e.st.WatchByID(id, e.uid)
	if w.Status != "error" || w.Failures != 1 || gapOf(e, id) != 2*time.Minute {
		t.Fatalf("after first failure: %+v gap %v", w, gapOf(e, id))
	}
	steps := []struct {
		wait     time.Duration
		failures int
		nextGap  time.Duration
	}{
		{2 * time.Minute, 2, 10 * time.Minute},  // quick retry 1
		{10 * time.Minute, 3, 30 * time.Minute}, // quick retry 2
		{30 * time.Minute, 3, 6 * time.Hour},    // quick retry 3: no strike beyond the third, then the normal interval
		{6 * time.Hour, 4, 6 * time.Hour},       // normal-interval failures 1..3: no backoff
		{6 * time.Hour, 5, 6 * time.Hour},
		{6 * time.Hour, 6, 6 * time.Hour},
		{6 * time.Hour, 7, 12 * time.Hour}, // 4th normal failure: exponential backoff starts
		{12 * time.Hour, 8, 24 * time.Hour},
	}
	calls := e.f.calls[u]
	for i, s := range steps {
		e.run(t, s.wait-time.Second)
		if e.f.calls[u] != calls {
			t.Fatalf("step %d: checked before its time", i)
		}
		e.run(t, time.Second)
		calls++
		if e.f.calls[u] != calls {
			t.Fatalf("step %d: not checked on time", i)
		}
		w, _ = e.st.WatchByID(id, e.uid)
		if w.Failures != s.failures || gapOf(e, id) != s.nextGap {
			t.Fatalf("step %d: failures %d (want %d), next in %v (want %v)", i, w.Failures, s.failures, gapOf(e, id), s.nextGap)
		}
	}
	// every attempt is in the check log; the quick retries are numbered
	cs, _ := e.st.RecentChecks(e.uid, id, 50)
	if len(cs) != 1+len(steps) {
		t.Fatalf("%d log rows", len(cs))
	}
	var retries []int
	for i := len(cs) - 1; i >= 0; i-- {
		retries = append(retries, cs[i].Retry)
		if cs[i].Class != ClassHeaderTimeout {
			t.Fatalf("class %q", cs[i].Class)
		}
	}
	want := []int{0, 1, 2, 3, 0, 0, 0, 0, 0}
	for i := range want {
		if retries[i] != want[i] {
			t.Fatalf("retry numbers %v, want %v", retries, want)
		}
	}
	// success resets everything
	e.f.set(u, pricePage(50000))
	e.run(t, 24*time.Hour)
	w, _ = e.st.WatchByID(id, e.uid)
	if w.Status != "ok" || w.Failures != 0 || w.QuickRetry != 0 || w.QuickPending {
		t.Fatalf("not reset: %+v", w)
	}
	// and a fresh failure starts the quick retries again
	e.f.errs[u] = headerTimeout()
	e.run(t, 6*time.Hour)
	if g := gapOf(e, id); g != 2*time.Minute {
		t.Fatalf("fresh failure next in %v", g)
	}
}

func TestQuickRetrySuccessMidwayResets(t *testing.T) {
	e := newSchedEnv(t)
	const u = "https://shop.example.com/p"
	id := e.add(t, u, nil, 360)
	e.f.errs[u] = &FetchError{Kind: "network", Class: ClassConnectReset, Msg: "network error: connection reset by peer"}
	e.run(t, 0)
	e.f.set(u, pricePage(1000))
	e.run(t, 2*time.Minute)
	w, _ := e.st.WatchByID(id, e.uid)
	if w.Status != "ok" || w.Failures != 0 || w.QuickRetry != 0 || w.QuickPending || *w.LastPricePence != 1000 {
		t.Fatalf("%+v", w)
	}
	if g := gapOf(e, id); g != 6*time.Hour {
		t.Fatalf("next in %v", g)
	}
}

func TestEveryTransientClassGetsQuickRetries(t *testing.T) {
	for _, class := range []string{ClassConnectRefused, ClassConnectReset, ClassConnectTimeout, ClassTLS, ClassHeaderTimeout, ClassBodyTimeout} {
		e := newSchedEnv(t)
		const u = "https://shop.example.com/p"
		id := e.add(t, u, nil, 360)
		e.f.errs[u] = &FetchError{Kind: "network", Class: class, Msg: "network error"}
		e.run(t, 0)
		if g := gapOf(e, id); g != 2*time.Minute {
			t.Errorf("%s: next in %v", class, g)
		}
	}
	// a 5xx is transient too
	e := newSchedEnv(t)
	const u = "https://shop.example.com/p"
	id := e.add(t, u, nil, 360)
	e.f.set(u, &Page{Status: 502, Header: http.Header{}})
	e.run(t, 0)
	if g := gapOf(e, id); g != 2*time.Minute {
		t.Errorf("502: next in %v", g)
	}
}

func TestBlocksAndNoPriceGetNoQuickRetries(t *testing.T) {
	cases := map[string]*Page{
		"403":       {Status: 403, Header: http.Header{}},
		"429":       {Status: 429, Header: http.Header{}},
		"challenge": {Status: 200, Header: http.Header{}, Body: fixture(t, "cloudflare.html")},
		"noprice":   {Status: 200, Header: http.Header{}, Body: fixture(t, "noprice.html")},
	}
	for name, pg := range cases {
		e := newSchedEnv(t)
		const u = "https://shop.example.com/p"
		id := e.add(t, u, nil, 60)
		e.f.set(u, pg)
		e.run(t, 0)
		w, _ := e.st.WatchByID(id, e.uid)
		if w.QuickRetry != 0 || w.QuickPending || gapOf(e, id) < 2*time.Hour {
			t.Errorf("%s: %+v next in %v", name, w, gapOf(e, id))
		}
		if name != "noprice" && w.Status != "blocked" {
			t.Errorf("%s: status %s", name, w.Status)
		}
	}
	// an unclassified network error is not retried quickly either
	e := newSchedEnv(t)
	const u = "https://shop.example.com/p"
	id := e.add(t, u, nil, 60)
	e.f.errs[u] = &FetchError{Kind: "network", Msg: "something odd"}
	e.run(t, 0)
	if g := gapOf(e, id); g < 2*time.Hour {
		t.Errorf("unclassified: next in %v", g)
	}
}

func TestQuickRetryHonoursRetryAfterAndJitter(t *testing.T) {
	e := newSchedEnv(t)
	e.s.jitter = func(d time.Duration) time.Duration { return -d / 2 }
	const u = "https://shop.example.com/p"
	id := e.add(t, u, nil, 360)
	e.f.set(u, &Page{Status: 503, Header: http.Header{"Retry-After": {"1800"}}})
	e.run(t, 0)
	if g := gapOf(e, id); g < 30*time.Minute {
		t.Fatalf("quick retry undercut Retry-After: %v", g)
	}
}
