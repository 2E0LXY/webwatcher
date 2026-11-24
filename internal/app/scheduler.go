package app

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"strings"
	"sync"
	"time"
)

var AllowedIntervals = []int{30, 60, 180, 360, 1440}

const DefaultIntervalMin = 60

// Scheduler polls due watches. One goroutine (Run) decides what is due;
// checks run in at most MaxConcurrent worker goroutines.
type Scheduler struct {
	store   *Store
	fetcher Fetcher
	clock   Clock
	hosts   *HostLimiter
	baseURL string
	sem     chan struct{}
	jitter  func(d time.Duration) time.Duration // returns a signed offset
	tick    time.Duration
}

func NewScheduler(store *Store, f Fetcher, clock Clock, baseURL string) *Scheduler {
	return &Scheduler{
		store: store, fetcher: f, clock: clock, baseURL: baseURL,
		hosts: NewHostLimiter(HostMinGap, clock),
		sem:   make(chan struct{}, MaxConcurrent),
		jitter: func(d time.Duration) time.Duration {
			span := int64(d / 5) // +/-10%
			if span <= 0 {
				return 0
			}
			return time.Duration(rand.Int64N(span) - span/2)
		},
		tick: 15 * time.Second,
	}
}

// Run loops until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		s.RunDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunDue checks every due watch and waits for the checks to finish. It
// returns the number of watches actually fetched.
func (s *Scheduler) RunDue(ctx context.Context) int {
	due, err := s.store.DueWatches(100)
	if err != nil {
		slog.Error("scheduler: list due", "err", err)
		return 0
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	n := 0
	for _, w := range due {
		if ctx.Err() != nil {
			break
		}
		host := ""
		if u, err := url.Parse(w.URL); err == nil {
			host = strings.ToLower(u.Hostname())
		}
		select {
		case s.sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return n
		}
		if ok, wait := s.hosts.Reserve(host); !ok {
			<-s.sem
			next := s.clock.Now().Add(wait + 2*time.Second + s.jitter(10*time.Second)).Unix()
			s.store.Defer(w.ID, next)
			continue
		}
		mu.Lock()
		n++
		mu.Unlock()
		wg.Add(1)
		go func(w *Watch) {
			defer wg.Done()
			defer func() { <-s.sem }()
			s.check(ctx, w)
		}(w)
	}
	wg.Wait()
	return n
}

func (s *Scheduler) interval(w *Watch) time.Duration {
	m := w.IntervalMin
	if m <= 0 {
		m = DefaultIntervalMin
	}
	return time.Duration(m) * time.Minute
}

func (s *Scheduler) check(ctx context.Context, w *Watch) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, fail := CheckURL(cctx, s.fetcher, w.URL, w.Selector)
	if ctx.Err() != nil {
		return // shutting down; do not record a bogus failure
	}
	if fail != nil {
		s.onFailure(ctx, w, fail)
		return
	}
	s.onSuccess(ctx, w, res)
}

func (s *Scheduler) onSuccess(ctx context.Context, w *Watch, res *Result) {
	obs := Observation{Price: res.PricePence, Currency: res.Currency, Method: res.Method, Basis: res.Basis}
	if obs.Basis == "" {
		obs.Basis = BasisUnknown
	}
	iv := s.interval(w)
	next := s.clock.Now().Add(iv + s.jitter(iv)).Unix()
	if res.SoldOut {
		if err := s.store.RecordSoldOut(w.ID, next); err != nil {
			slog.Error("record sold out", "watch", w.ID, "host", hostOf(w.URL), "err", err)
		}
		return
	}
	// Never compare unlike things: a different currency or price basis is
	// recorded and flagged for the user to confirm, with no alert.
	if w.LastPricePence != nil && (w.Currency != obs.Currency || w.Basis != obs.Basis) {
		if err := s.store.RecordBasisChange(w.ID, obs, next); err != nil {
			slog.Error("record basis change", "watch", w.ID, "host", hostOf(w.URL), "err", err)
		}
		return
	}
	lastChanged, _ := s.store.LastAlertPrice(w.ID, AlertChanged)
	lastBelow, _ := s.store.LastAlertPrice(w.ID, AlertBelow)
	kind := EvaluateAlert(AlertInput{Prev: w.AlertBase, Cur: res.PricePence, Target: w.TargetPence, LastChanged: lastChanged, LastBelow: lastBelow})
	w.Currency = res.Currency
	var qa *QueuedAlert
	if kind != "" && w.AlertsOn {
		qa = &QueuedAlert{Kind: kind, Price: res.PricePence, Msg: BuildAlertMessage(kind, w, w.AlertBase, res.PricePence, "", s.baseURL)}
	}
	// The price update and its alert are committed together; the sender loop delivers it.
	if err := s.store.RecordObservation(w.ID, obs, next, &res.PricePence, qa); err != nil {
		slog.Error("record success", "watch", w.ID, "host", hostOf(w.URL), "err", err)
	}
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return strings.ToLower(u.Hostname())
	}
	return ""
}

func (s *Scheduler) onFailure(ctx context.Context, w *Watch, fail *CheckFailure) {
	failures := w.Failures + 1
	iv := s.interval(w)
	// exponential backoff: interval * 2^failures, capped at 24h (or the interval if longer)
	delay := iv
	limit := max(24*time.Hour, iv)
	for i := 0; i < failures && delay < limit; i++ {
		delay *= 2
	}
	delay = min(delay, limit)
	next := s.clock.Now().Add(delay + s.jitter(delay)).Unix()
	var notice *QueuedAlert
	if failures >= MaxFailures && !w.FailureNotif && w.AlertsOn {
		notice = &QueuedAlert{Kind: AlertFailed, Msg: BuildAlertMessage(AlertFailed, w, nil, 0, fail.Reason, s.baseURL)}
	}
	if err := s.store.RecordFailure(w.ID, fail.Status, fail.Reason, failures, next, notice); err != nil {
		slog.Error("record failure", "watch", w.ID, "host", hostOf(w.URL), "err", err)
	}
}
