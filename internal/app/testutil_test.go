package app

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)} }
func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}
func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

type fakeMessenger struct {
	mu   sync.Mutex
	sent []Message
	err  error
}

func (f *fakeMessenger) Name() string { return "fake" }
func (f *fakeMessenger) Send(_ context.Context, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return f.err
}
func (f *fakeMessenger) Sent() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Message(nil), f.sent...)
}

func testStore(t *testing.T) (*Store, *fakeClock) {
	t.Helper()
	db, err := OpenDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clk := newFakeClock()
	return NewStore(db, clk), clk
}

func mustUser(t *testing.T, s *Store, phone string) int64 {
	t.Helper()
	raw, h := newToken()
	if err := s.CreateLoginToken(phone, h, time.Minute); err != nil {
		t.Fatal(err)
	}
	_ = raw
	uid, _, err := s.ConsumeLoginToken(h)
	if err != nil {
		t.Fatal(err)
	}
	return uid
}
