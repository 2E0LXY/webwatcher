package app

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestRateLimiterStateIsBounded(t *testing.T) {
	r := NewRateLimiter(3, time.Hour)
	r.maxKeys = 100
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5000; i++ {
		r.Allow(fmt.Sprintf("k%d", i), now.Add(time.Duration(i)*time.Millisecond))
		if r.Len() > 100 {
			t.Fatalf("map grew to %d", r.Len())
		}
	}
	if len(r.order) > 100 {
		t.Fatalf("order grew to %d", len(r.order))
	}
	// the oldest keys were evicted, the newest are still limited
	for i := 0; i < 3; i++ {
		r.Allow("k4999", now.Add(10*time.Second))
	}
	if r.Allow("k4999", now.Add(11*time.Second)) {
		t.Fatal("recent key not limited")
	}
	// per-key timestamp lists never exceed the limit
	r2 := NewRateLimiter(3, time.Hour)
	for i := 0; i < 1000; i++ {
		r2.Allow("same", now)
	}
	if n := len(r2.hits["same"]); n != 3 {
		t.Fatalf("%d timestamps", n)
	}
}

func TestRateLimiterPrefersExpiredKeysWhenFull(t *testing.T) {
	r := NewRateLimiter(1, time.Minute)
	r.maxKeys = 10
	now := time.Now()
	for i := 0; i < 10; i++ {
		r.Allow(fmt.Sprintf("old%d", i), now)
	}
	r.Allow("fresh", now.Add(2*time.Minute)) // all old ones have expired
	if r.Len() != 1 {
		t.Fatalf("expired keys not evicted first: %d", r.Len())
	}
}

func TestRateLimiterStillLimits(t *testing.T) {
	r := NewRateLimiter(2, time.Minute)
	now := time.Now()
	if !r.Allow("a", now) || !r.Allow("a", now) || r.Allow("a", now) {
		t.Fatal("limit")
	}
	if !r.Allow("a", now.Add(61*time.Second)) {
		t.Fatal("window did not slide")
	}
}

func TestLoginsFlowThroughGlobalBudgetWithoutStarvingAlerts(t *testing.T) {
	s, clk := testStore(t)
	m := &fakeMessenger{}
	a := NewAuth(s, m, clk, "https://x")
	a.SetSendBudget(6) // logins may use 3 of the 6
	for i := 0; i < 10; i++ {
		a.RequestLogin(context.Background(), fmt.Sprintf("+4477009001%02d", i), fmt.Sprintf("10.0.0.%d", i), true)
	}
	if n := len(m.Sent()); n != 3 {
		t.Fatalf("logins sent %d, want 3 (half the budget)", n)
	}
	// alerts still get the other half
	uid := mustUser(t, s, "+447700900999")
	id, _ := s.AddWatch(Watch{UserID: uid, URL: "https://a.example.com/", IntervalMin: 60})
	for i := 0; i < 5; i++ {
		s.RecordSuccess(id, int64(100+i), "GBP", "m", 0, nil, qa(AlertChanged, int64(100+i)))
	}
	am := &fakeMessenger{}
	snd := NewSender(s, am, clk, 6)
	snd.RunOnce(context.Background())
	snd.RunOnce(context.Background())
	if n := len(am.Sent()); n != 3 {
		t.Fatalf("alerts sent %d, want the remaining 3 of the shared budget", n)
	}
	// budget frees up an hour later
	clk.Advance(61 * time.Minute)
	a.RequestLogin(context.Background(), "+447700900188", "10.9.9.9", true)
	if len(m.Sent()) != 4 {
		t.Fatal("budget did not reset")
	}
}

func TestLoginLimiterMapsAreBoundedInAuth(t *testing.T) {
	s, clk := testStore(t)
	a := NewAuth(s, &fakeMessenger{}, clk, "https://x")
	a.byIP.maxKeys, a.byPhone.maxKeys = 50, 50
	a.SetSendBudget(1_000_000)
	for i := 0; i < 500; i++ {
		a.RequestLogin(context.Background(), fmt.Sprintf("+44770090%04d", i), fmt.Sprintf("192.0.%d.%d", i/200, i%200), true)
	}
	if a.byIP.Len() > 50 || a.byPhone.Len() > 50 {
		t.Fatalf("%d %d", a.byIP.Len(), a.byPhone.Len())
	}
}
