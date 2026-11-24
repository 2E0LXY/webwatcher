package app

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func qa(kind string, price int64) *QueuedAlert {
	return &QueuedAlert{Kind: kind, Price: price, Msg: Message{Kind: KindAlert, Text: "t", Params: []string{"a", "b"}}}
}

func outboxCount(st *Store, where string) int {
	var n int
	st.db.QueryRow(`SELECT COUNT(*) FROM outbox ` + where).Scan(&n)
	return n
}

func TestAlertQueuedInSameTransactionAsPriceUpdate(t *testing.T) {
	e := newSchedEnv(t)
	const u = "https://shop.example.com/p"
	id := e.add(t, u, nil, 60)
	e.f.set(u, pricePage(10000), pricePage(9000))
	e.clk.Advance(0)
	e.s.RunDue(context.Background())
	e.clk.Advance(time.Hour)
	e.s.RunDue(context.Background())
	// Before the sender runs: price and queued alert are both visible.
	w, _ := e.st.WatchByID(id, e.uid)
	if *w.LastPricePence != 9000 || outboxCount(e.st, `WHERE status='queued'`) != 1 {
		t.Fatalf("price=%d queued=%d", *w.LastPricePence, outboxCount(e.st, `WHERE status='queued'`))
	}
	if len(e.m.Sent()) != 0 {
		t.Fatal("scheduler sent directly")
	}
	// A failing transaction must roll back both: break the outbox insert via a duplicate-proof trigger.
	e.st.db.Exec(`CREATE TRIGGER boom BEFORE INSERT ON outbox BEGIN SELECT RAISE(ABORT,'boom'); END`)
	before, _ := e.st.WatchByID(id, e.uid)
	err := e.st.RecordSuccess(id, 5000, "GBP", "meta", 0, nil, qa(AlertChanged, 5000))
	if err == nil {
		t.Fatal("expected error")
	}
	after, _ := e.st.WatchByID(id, e.uid)
	if *after.LastPricePence != *before.LastPricePence {
		t.Fatal("price update not rolled back with the failed outbox insert")
	}
}

func TestOutboxEventKeyUnique(t *testing.T) {
	e := newSchedEnv(t)
	id := e.add(t, "https://shop.example.com/p", nil, 60)
	for i := 0; i < 3; i++ {
		if err := e.st.RecordSuccess(id, 9000, "GBP", "meta", 0, nil, qa(AlertChanged, 9000)); err != nil {
			t.Fatal(err)
		}
	}
	if n := outboxCount(e.st, ``); n != 1 {
		t.Fatalf("duplicate event queued: %d", n)
	}
	e.st.RecordSuccess(id, 9000, "GBP", "meta", 0, nil, qa(AlertBelow, 9000))
	e.st.RecordSuccess(id, 9500, "GBP", "meta", 0, nil, qa(AlertChanged, 9500))
	if n := outboxCount(e.st, ``); n != 3 {
		t.Fatalf("distinct events: %d", n)
	}
}

func TestOutboxOscillatingPriceCanAlertAgain(t *testing.T) {
	e := newSchedEnv(t)
	id := e.add(t, "https://shop.example.com/p", nil, 60)
	for _, p := range []int64{9900, 10500, 9900, 10500} {
		e.st.RecordSuccess(id, p, "GBP", "meta", 0, nil, qa(AlertChanged, p))
		e.snd.RunOnce(context.Background())
	}
	if n := len(e.m.Sent()); n != 4 {
		t.Fatalf("sent %d, want 4", n)
	}
}

func TestSenderRetriesWithBackoffThenFails(t *testing.T) {
	e := newSchedEnv(t)
	id := e.add(t, "https://shop.example.com/p", nil, 60)
	e.st.RecordSuccess(id, 9000, "GBP", "meta", 0, nil, qa(AlertChanged, 9000))
	e.m.err = fmt.Errorf("provider down for +447700900123 at https://x.example/a?token=SECRET")
	ctx := context.Background()
	if e.snd.RunOnce(ctx) != 1 {
		t.Fatal("first attempt")
	}
	if e.snd.RunOnce(ctx) != 0 {
		t.Fatal("retried without waiting for backoff")
	}
	var last string
	e.st.db.QueryRow(`SELECT last_error FROM outbox`).Scan(&last)
	if contains(last, "SECRET") || contains(last, "7700900") || contains(last, "/a") {
		t.Fatalf("PII stored in last_error: %q", last)
	}
	gaps := []time.Duration{30 * time.Second, 2 * time.Minute, 8 * time.Minute}
	for i, g := range gaps {
		e.clk.Advance(g - time.Second)
		if e.snd.RunOnce(ctx) != 0 {
			t.Fatalf("attempt %d too early", i+2)
		}
		e.clk.Advance(time.Second)
		if e.snd.RunOnce(ctx) != 1 {
			t.Fatalf("attempt %d not made after %v", i+2, g)
		}
	}
	// 4 attempts so far; the fifth is the last
	e.clk.Advance(33 * time.Minute)
	e.snd.RunOnce(ctx)
	c, _ := e.st.OutboxCounts()
	if c[OutFailed] != 1 || c[OutQueued] != 0 {
		t.Fatalf("%v", c)
	}
	var att int
	e.st.db.QueryRow(`SELECT attempts FROM outbox`).Scan(&att)
	if att != OutboxMaxAttempts || len(e.m.Sent()) != OutboxMaxAttempts {
		t.Fatalf("attempts %d sent %d", att, len(e.m.Sent()))
	}
	e.clk.Advance(100 * time.Hour)
	if e.snd.RunOnce(ctx) != 0 {
		t.Fatal("failed row retried")
	}
}

func TestSenderMarksAccepted(t *testing.T) {
	e := newSchedEnv(t)
	id := e.add(t, "https://shop.example.com/p", nil, 60)
	e.st.RecordSuccess(id, 9000, "GBP", "meta", 0, nil, qa(AlertChanged, 9000))
	e.snd.RunOnce(context.Background())
	c, _ := e.st.OutboxCounts()
	if c[OutAccepted] != 1 {
		t.Fatalf("%v", c)
	}
	if s := e.m.Sent(); len(s) != 1 || s[0].To != "+447700900123" || s[0].Kind != KindAlert {
		t.Fatalf("%+v", s)
	}
	e.snd.RunOnce(context.Background())
	if len(e.m.Sent()) != 1 {
		t.Fatal("accepted row resent")
	}
}

func TestOutboxSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	clk := newFakeClock()
	st := NewStore(db, clk)
	uid := mustUser(t, st, "+447700900123")
	id, _ := st.AddWatch(Watch{UserID: uid, URL: "https://shop.example.com/p", IntervalMin: 60})
	st.RecordSuccess(id, 9000, "GBP", "meta", 0, nil, qa(AlertChanged, 9000))
	db.Close()

	db2, err := OpenDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	m := &fakeMessenger{}
	NewSender(NewStore(db2, clk), m, clk, 10).RunOnce(context.Background())
	if len(m.Sent()) != 1 {
		t.Fatal("queued alert lost across restart")
	}
}

func TestOutboxRemovedWithWatchAndAccount(t *testing.T) {
	e := newSchedEnv(t)
	a := e.add(t, "https://a.example.com/p", nil, 60)
	b := e.add(t, "https://b.example.com/p", nil, 60)
	e.st.RecordSuccess(a, 9000, "GBP", "meta", 0, nil, qa(AlertChanged, 9000))
	e.st.RecordSuccess(b, 9000, "GBP", "meta", 0, nil, qa(AlertChanged, 9000))
	if err := e.st.DeleteWatch(e.uid, a); err != nil {
		t.Fatal(err)
	}
	if n := outboxCount(e.st, ``); n != 1 {
		t.Fatalf("watch delete left %d rows", n)
	}
	e.st.DeleteUser(e.uid)
	if n := outboxCount(e.st, ``); n != 0 {
		t.Fatalf("account delete left %d rows", n)
	}
	if e.snd.RunOnce(context.Background()) != 0 {
		t.Fatal("sent for deleted account")
	}
}

func TestGlobalSendBudget(t *testing.T) {
	e := newSchedEnv(t)
	e.snd = NewSender(e.st, e.m, e.clk, 3)
	for i := 0; i < 5; i++ {
		id := e.add(t, fmt.Sprintf("https://h%d.example.com/p", i), nil, 60)
		e.st.RecordSuccess(id, 9000, "GBP", "meta", 0, nil, qa(AlertChanged, 9000))
	}
	ctx := context.Background()
	e.snd.RunOnce(ctx)
	e.snd.RunOnce(ctx)
	if n := len(e.m.Sent()); n != 3 {
		t.Fatalf("budget not enforced: %d", n)
	}
	if outboxCount(e.st, `WHERE status='queued'`) != 2 {
		t.Fatal("over-budget alerts must stay queued, not fail")
	}
	e.clk.Advance(61 * time.Minute)
	e.snd.RunOnce(ctx)
	if n := len(e.m.Sent()); n != 5 {
		t.Fatalf("queue not drained after window: %d", n)
	}
}

func TestBudgetCountsRetries(t *testing.T) {
	e := newSchedEnv(t)
	e.snd = NewSender(e.st, e.m, e.clk, 2)
	id := e.add(t, "https://a.example.com/p", nil, 60)
	e.st.RecordSuccess(id, 9000, "GBP", "meta", 0, nil, qa(AlertChanged, 9000))
	e.m.err = fmt.Errorf("x")
	for i := 0; i < 5; i++ {
		e.clk.Advance(time.Hour / 10)
		e.snd.RunOnce(context.Background())
	}
	if n := len(e.m.Sent()); n > 3 {
		t.Fatalf("budget ignored for retries: %d", n)
	}
}

func TestNoAlertsWhenDisabledOrNoConsent(t *testing.T) {
	e := newSchedEnv(t)
	const u = "https://shop.example.com/p"
	id := e.add(t, u, p(10000), 60)
	e.f.set(u, pricePage(12000), pricePage(9000), pricePage(8000), pricePage(7000))
	e.run(t, 0)
	// opted out: price change recorded, nothing queued or sent
	e.st.SetAlertsEnabled(e.uid, false)
	e.run(t, time.Hour)
	if len(e.m.Sent()) != 0 || outboxCount(e.st, ``) != 0 {
		t.Fatalf("alert while opted out: %d %d", len(e.m.Sent()), outboxCount(e.st, ``))
	}
	w, _ := e.st.WatchByID(id, e.uid)
	if *w.LastPricePence != 9000 {
		t.Fatal("price not tracked while opted out")
	}
	// re-enable: no stale alert replay for the old move
	e.st.SetAlertsEnabled(e.uid, true)
	e.run(t, time.Hour)
	if s := e.m.Sent(); len(s) != 1 || !contains(s[0].Text, "£90.00 → £80.00") {
		t.Fatalf("%+v", s)
	}
	// STOP arriving after queueing but before sending: the row is dropped
	e.f.set(u, pricePage(7000))
	e.clk.Advance(time.Hour)
	e.s.RunDue(context.Background())
	if outboxCount(e.st, `WHERE status='queued'`) != 1 {
		t.Fatal("not queued")
	}
	e.st.SetAlertsEnabled(e.uid, false)
	e.snd.RunOnce(context.Background())
	if len(e.m.Sent()) != 1 {
		t.Fatal("queued alert sent after opt-out")
	}
	// a user without consent never gets sent anything even if rows exist
	st2, _ := testStore(t)
	st2.CreateLoginToken("+447700900555", "h", time.Minute, false)
	uid2, _, _ := st2.ConsumeLoginToken("h")
	wid, _ := st2.AddWatch(Watch{UserID: uid2, URL: "https://x.example.com/", IntervalMin: 60})
	st2.RecordSuccess(wid, 100, "GBP", "meta", 0, nil, qa(AlertChanged, 100))
	m := &fakeMessenger{}
	NewSender(st2, m, newFakeClock(), 10).RunOnce(context.Background())
	if len(m.Sent()) != 0 {
		t.Fatal("sent without consent")
	}
	c, _ := st2.OutboxCounts()
	if c[OutSuppressed] != 1 {
		t.Fatalf("%v", c)
	}
}

func TestRedactPII(t *testing.T) {
	got := RedactPII("failed for +447700900123 url https://shop.example.com/a/b?token=abc&x=1 done")
	if got != "failed for ***123 url shop.example.com done" {
		t.Fatal(got)
	}
}

func TestSendBudgetConfig(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, _ := LoadConfig(env(map[string]string{}))
	if c.SendBudget != 200 {
		t.Fatal(c.SendBudget)
	}
	c, _ = LoadConfig(env(map[string]string{"WW_SEND_BUDGET_PER_HOUR": "5"}))
	if c.SendBudget != 5 {
		t.Fatal(c.SendBudget)
	}
	if _, err := LoadConfig(env(map[string]string{"WW_SEND_BUDGET_PER_HOUR": "0"})); err == nil {
		t.Fatal("zero accepted")
	}
}
