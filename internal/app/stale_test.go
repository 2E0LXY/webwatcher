package app

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// hookFetcher runs a callback while a fetch is "in flight".
type hookFetcher struct {
	inner Fetcher
	hook  func()
}

func (h *hookFetcher) Fetch(ctx context.Context, u string, c *Conditional) (*Page, error) {
	if h.hook != nil {
		h.hook()
	}
	return h.inner.Fetch(ctx, u, c)
}

func staleEnv(t *testing.T) (*schedEnv, *hookFetcher, int64, string) {
	e := newSchedEnv(t)
	const u = "https://shop.example.com/p"
	id := e.add(t, u, p(10000), 60)
	e.f.set(u, pricePage(12000), pricePage(9000), pricePage(9000))
	e.run(t, 0) // baseline 12000, above target
	hf := &hookFetcher{inner: e.f}
	e.s.fetcher = hf
	return e, hf, id, u
}

func TestStaleTargetEditDuringCheckDropsAlert(t *testing.T) {
	e, hf, id, _ := staleEnv(t)
	hf.hook = func() { e.st.SetTarget(e.uid, id, p(5000)) } // user lowers the target mid-fetch
	e.run(t, time.Hour)
	if outboxCount(e.st, ``) != 0 || len(e.m.Sent()) != 0 {
		t.Fatal("alert for the old target was queued or sent")
	}
	w, _ := e.st.WatchByID(id, e.uid)
	if *w.LastPricePence != 12000 || w.AlertBase != nil || *w.TargetPence != 5000 {
		t.Fatalf("stale result written: %+v", w)
	}
	hf.hook = nil
	e.run(t, time.Hour) // re-checked with the new target: 9000 is above 5000, baseline reset => no below alert
	if len(e.m.Sent()) != 0 {
		t.Fatalf("%+v", e.m.Sent())
	}
}

func TestStalePauseDuringCheck(t *testing.T) {
	e, hf, id, _ := staleEnv(t)
	hf.hook = func() { e.st.SetPaused(e.uid, id, true) }
	e.run(t, time.Hour)
	w, _ := e.st.WatchByID(id, e.uid)
	if outboxCount(e.st, ``) != 0 || *w.LastPricePence != 12000 || !w.Paused {
		t.Fatalf("%+v", w)
	}
}

func TestStaleDeleteDuringCheck(t *testing.T) {
	e, hf, id, _ := staleEnv(t)
	hf.hook = func() { e.st.DeleteWatch(e.uid, id) }
	e.run(t, time.Hour)
	if outboxCount(e.st, ``) != 0 {
		t.Fatal("outbox row for a deleted watch")
	}
	if _, err := e.st.WatchByID(id, e.uid); err == nil {
		t.Fatal("watch resurrected")
	}
}

func TestStaleAlertsDisabledDuringCheck(t *testing.T) {
	e, hf, id, _ := staleEnv(t)
	hf.hook = func() { e.st.SetAlertsEnabled(e.uid, false) }
	e.run(t, time.Hour)
	w, _ := e.st.WatchByID(id, e.uid)
	if outboxCount(e.st, ``) != 0 || len(e.m.Sent()) != 0 || *w.LastPricePence != 9000 {
		t.Fatalf("%+v", w)
	}
}

func TestStaleStoreLevelGuard(t *testing.T) {
	e := newSchedEnv(t)
	id := e.add(t, "https://shop.example.com/p", p(10000), 60)
	w, _ := e.st.WatchByID(id, e.uid)
	v := w.Version
	e.st.SetTarget(e.uid, id, p(1))
	err := e.st.RecordObservation(id, Observation{Price: 100, Currency: "GBP", Method: "m", Basis: BasisUnknown, Version: &v}, 0, nil, qa(AlertChanged, 100))
	if err != ErrStale {
		t.Fatalf("%v", err)
	}
	for name, edit := range map[string]func(){
		"pause":  func() { e.st.SetPaused(e.uid, id, true) },
		"resume": func() { e.st.SetPaused(e.uid, id, false) },
	} {
		w, _ = e.st.WatchByID(id, e.uid)
		before := w.Version
		edit()
		if w, _ = e.st.WatchByID(id, e.uid); w.Version <= before {
			t.Errorf("%s did not bump the version", name)
		}
	}
	// interval edits are not alert-relevant and must not invalidate queued alerts
	w, _ = e.st.WatchByID(id, e.uid)
	e.st.SetInterval(e.uid, id, 360)
	if w2, _ := e.st.WatchByID(id, e.uid); w2.Version != w.Version {
		t.Error("interval edit bumped the version")
	}
	// basis confirmation bumps it too
	e.st.RecordObservation(id, Observation{Price: 100, Currency: "GBP", Method: "m", Basis: BasisPerPerson}, 0, nil, nil)
	e.st.RecordBasisChange(id, Observation{Price: 200, Currency: "GBP", Method: "m", Basis: BasisTotal}, 0)
	w, _ = e.st.WatchByID(id, e.uid)
	e.st.ConfirmBasis(e.uid, id)
	if w2, _ := e.st.WatchByID(id, e.uid); w2.Version <= w.Version {
		t.Error("basis confirmation did not bump the version")
	}
}

func queueFor(e *schedEnv, t *testing.T, id int64) {
	t.Helper()
	if err := e.st.RecordSuccess(id, 9000, "GBP", "meta", 0, nil, qa(AlertChanged, 9000)); err != nil {
		t.Fatal(err)
	}
	if outboxCount(e.st, `WHERE status='queued'`) != 1 {
		t.Fatal("not queued")
	}
}

func TestSenderDropsStaleQueuedAlerts(t *testing.T) {
	cases := map[string]func(e *schedEnv, id int64){
		"target edited": func(e *schedEnv, id int64) { e.st.SetTarget(e.uid, id, p(1)) },
		"paused":        func(e *schedEnv, id int64) { e.st.SetPaused(e.uid, id, true) },
		"paused and resumed": func(e *schedEnv, id int64) {
			e.st.SetPaused(e.uid, id, true)
			e.st.SetPaused(e.uid, id, false)
		},
		"alerts off": func(e *schedEnv, id int64) { e.st.SetAlertsEnabled(e.uid, false) },
	}
	for name, mutate := range cases {
		e := newSchedEnv(t)
		id := e.add(t, "https://shop.example.com/p", nil, 60)
		queueFor(e, t, id)
		mutate(e, id)
		e.snd.RunOnce(context.Background())
		if len(e.m.Sent()) != 0 {
			t.Errorf("%s: stale alert sent", name)
		}
	}
	// control: an untouched queued alert is sent, and an interval edit does not stop it
	e := newSchedEnv(t)
	id := e.add(t, "https://shop.example.com/p", nil, 60)
	queueFor(e, t, id)
	e.st.SetInterval(e.uid, id, 360)
	e.snd.RunOnce(context.Background())
	if len(e.m.Sent()) != 1 {
		t.Fatal("fresh alert not sent")
	}
}

func TestDeleteWatchOrAccountRemovesQueuedRows(t *testing.T) {
	e := newSchedEnv(t)
	a := e.add(t, "https://a.example.com/p", nil, 60)
	b := e.add(t, "https://b.example.com/p", nil, 60)
	queueFor2 := func(id int64) { e.st.RecordSuccess(id, 9000, "GBP", "m", 0, nil, qa(AlertChanged, 9000)) }
	queueFor2(a)
	queueFor2(b)
	e.st.DeleteWatch(e.uid, a)
	if outboxCount(e.st, `WHERE watch_id=`+"1") != 0 || outboxCount(e.st, ``) != 1 {
		t.Fatal("watch delete")
	}
	e.st.DeleteUser(e.uid)
	if outboxCount(e.st, ``) != 0 {
		t.Fatal("account delete")
	}
}

// ---- transitions ----

func simulate(target *int64, prices []int64) []string {
	var prev *int64
	var out []string
	for _, c := range prices {
		out = append(out, EvaluateAlert(AlertInput{Prev: prev, Cur: c, Target: target}))
		c := c
		prev = &c
	}
	return out
}

func TestAlertTransitions(t *testing.T) {
	B, C, N := AlertBelow, AlertChanged, ""
	cases := []struct {
		name   string
		target *int64
		prices []int64
		want   []string
	}{
		{"cross down alerts once", p(10000), []int64{12000, 9000, 9000, 9000}, []string{N, B, N, N}},
		{"falling further below is a plain change", p(10000), []int64{12000, 9000, 8000}, []string{N, B, C}},
		{"recovery above re-arms, same price alerts again", p(10000), []int64{9000, 11000, 9000}, []string{B, C, B}},
		{"oscillation below/above alerts every crossing", p(10000), []int64{9000, 11000, 9000, 11000, 9000}, []string{B, C, B, C, B}},
		{"staying above never alerts below", p(10000), []int64{12000, 11000, 10001}, []string{N, C, C}},
		{"exactly at target counts as below", p(10000), []int64{10001, 10000}, []string{N, B}},
		{"no target: changes only", nil, []int64{100, 100, 90, 90, 100}, []string{N, N, C, N, C}},
		{"first check already below", p(10000), []int64{9000, 9000}, []string{B, N}},
	}
	for _, c := range cases {
		if got := simulate(c.target, c.prices); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestTransitionsThroughSchedulerAndOutbox(t *testing.T) {
	e := newSchedEnv(t)
	const u = "https://shop.example.com/p"
	e.add(t, u, p(10000), 60)
	e.f.set(u, pricePage(9000), pricePage(11000), pricePage(9000), pricePage(9000))
	for i := 0; i < 4; i++ {
		e.run(t, time.Hour)
	}
	var kinds []string
	for _, m := range e.m.Sent() {
		if contains(m.Text, "below target") {
			kinds = append(kinds, "below")
		} else {
			kinds = append(kinds, "changed")
		}
	}
	if !reflect.DeepEqual(kinds, []string{"below", "changed", "below"}) {
		t.Fatalf("%v", kinds)
	}
}

func TestEditingTargetReArms(t *testing.T) {
	e := newSchedEnv(t)
	const u = "https://shop.example.com/p"
	id := e.add(t, u, p(10000), 60)
	e.f.set(u, pricePage(9000))
	e.run(t, 0)
	e.run(t, time.Hour)
	if len(e.m.Sent()) != 1 {
		t.Fatalf("%d", len(e.m.Sent()))
	}
	e.st.SetTarget(e.uid, id, p(9500)) // price 9000 is below the new target as well: armed again
	e.run(t, time.Hour)
	if s := e.m.Sent(); len(s) != 2 || !contains(s[1].Text, "below target") || !contains(s[1].Text, "£95.00") {
		t.Fatalf("%+v", s)
	}
	e.run(t, time.Hour)
	if len(e.m.Sent()) != 2 {
		t.Fatal("repeat alert")
	}
}
