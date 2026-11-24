package agent

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"
)

// Defaults of the loop. They are deliberately slow: this is one person's price
// check, not a crawler.
const (
	DefaultPollInterval = 60 * time.Second
	DefaultHostGap      = 5 * time.Minute
	DefaultBlockBackoff = 6 * time.Hour
	DefaultJitterMin    = 5 * time.Second
	DefaultJitterMax    = 60 * time.Second
	DefaultAuthBackoff  = 5 * time.Minute
	MaxHTMLBytes        = 2 << 20
)

// Options configure an Agent. Zero fields take the defaults above.
type Options struct {
	PollInterval time.Duration
	HostGap      time.Duration // minimum time between two loads of the same host
	BlockBackoff time.Duration // how long a host is left alone after a challenge
	JitterMin    time.Duration // random pause before each load
	JitterMax    time.Duration
	AuthBackoff  time.Duration // wait after the server rejects the token
	Logger       *slog.Logger
	// OnResult is called after each result is reported (the status file hook).
	OnResult func(host, status, class string, at time.Time)

	// Test seams.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	Rand  func(n int64) int64
}

// Agent runs the poll, load, report loop.
type Agent struct {
	o        Options
	srv      Server
	br       Browser
	hostNext map[string]time.Time // earliest next load per host
	lastLoad map[int64]time.Time  // per watch: when it was last loaded
}

func New(srv Server, br Browser, o Options) *Agent {
	def := func(p *time.Duration, v time.Duration) {
		if *p == 0 {
			*p = v
		}
	}
	def(&o.PollInterval, DefaultPollInterval)
	def(&o.HostGap, DefaultHostGap)
	def(&o.BlockBackoff, DefaultBlockBackoff)
	def(&o.JitterMin, DefaultJitterMin)
	def(&o.JitterMax, DefaultJitterMax)
	def(&o.AuthBackoff, DefaultAuthBackoff)
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = sleepCtx
	}
	if o.Rand == nil {
		o.Rand = rand.Int64N
	}
	return &Agent{o: o, srv: srv, br: br, hostNext: map[string]time.Time{}, lastLoad: map[int64]time.Time{}}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run loops until ctx is cancelled. It returns ctx.Err().
func (a *Agent) Run(ctx context.Context) error {
	a.o.Logger.Info("agent started")
	defer a.o.Logger.Info("agent stopped")
	for ctx.Err() == nil {
		wait := a.Cycle(ctx)
		if err := a.o.Sleep(ctx, wait); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// Cycle polls once, processes the eligible jobs one at a time, and returns how
// long to wait before the next poll.
func (a *Agent) Cycle(ctx context.Context) time.Duration {
	jl, err := a.srv.Jobs(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return 0
		}
		switch e := err.(type) {
		case *RateLimitedError:
			a.o.Logger.Warn("server rate limit; slowing down", "wait", e.After.String())
			return max(e.After, a.o.PollInterval)
		}
		if err == ErrRejected {
			a.o.Logger.Error("the server rejected the agent token: create a new token on the dashboard and save it again", "retry_in", a.o.AuthBackoff.String())
			return a.o.AuthBackoff
		}
		a.o.Logger.Warn("could not reach the server", "err", err.Error())
		return a.o.PollInterval
	}
	for _, j := range jl.Jobs {
		if ctx.Err() != nil {
			return 0
		}
		a.process(ctx, j)
	}
	wait := a.o.PollInterval
	if s := time.Duration(jl.PollAfterSec) * time.Second; s > wait {
		wait = s
	}
	return wait
}

// eligible applies the agent's own pacing on top of the server's schedule.
func (a *Agent) eligible(j Job, host string) bool {
	now := a.o.Now()
	if until, ok := a.hostNext[host]; ok && now.Before(until) {
		return false
	}
	if last, ok := a.lastLoad[j.WatchID]; ok {
		min := time.Duration(j.MinDelaySec) * time.Second
		if now.Before(last.Add(min)) {
			return false
		}
	}
	return true
}

func (a *Agent) process(ctx context.Context, j Job) {
	log := a.o.Logger
	if !AllowedURL(j.URL) {
		log.Warn("refusing a URL that is not plain http or https", "watch", j.WatchID)
		a.submit(ctx, j, &Result{WatchID: j.WatchID, Version: j.Version, Status: "error", FailureClass: "nav_error"}, "")
		return
	}
	host := HostOf(j.URL)
	if !a.eligible(j, host) {
		return
	}
	// A random pause before every load, so the loads of one host never fall on a rhythm.
	span := int64(a.o.JitterMax - a.o.JitterMin)
	d := a.o.JitterMin
	if span > 0 {
		d += time.Duration(a.o.Rand(span + 1))
	}
	if err := a.o.Sleep(ctx, d); err != nil {
		return
	}
	// Re-check after the pause: the clock has moved.
	if !a.eligible(j, host) {
		return
	}
	start := a.o.Now()
	a.lastLoad[j.WatchID] = start
	a.hostNext[host] = start.Add(a.o.HostGap)
	log.Info("loading", "watch", j.WatchID, "page", SafeURL(j.URL))
	pg, err := a.br.Load(ctx, j.URL)
	if ctx.Err() != nil {
		delete(a.lastLoad, j.WatchID)
		return
	}
	res := &Result{WatchID: j.WatchID, Version: j.Version}
	switch {
	case err != nil:
		res.Status, res.FailureClass = "error", "other"
		if le, ok := err.(*LoadError); ok {
			res.FailureClass = le.Class
		}
		log.Warn("load failed", "watch", j.WatchID, "host", host, "class", res.FailureClass)
	case DetectChallenge(pg.Status, pg.HTML):
		res.Status, res.FailureClass, res.FinalURL, res.HTTPStatus = "blocked", "challenge", SafeFinal(pg.FinalURL), pg.Status
		a.hostNext[host] = a.o.Now().Add(a.o.BlockBackoff)
		log.Warn("bot challenge or access denied; leaving this host alone", "host", host, "backoff", a.o.BlockBackoff.String())
	default:
		html := pg.HTML
		if len(html) > MaxHTMLBytes {
			html = SlimHTML(html)
		}
		if len(html) > MaxHTMLBytes {
			res.Status, res.FailureClass = "error", "too_large"
			log.Warn("rendered page too large to report", "watch", j.WatchID, "host", host)
		} else {
			res.Status, res.RenderedHTML, res.FinalURL, res.HTTPStatus = "ok", html, pg.FinalURL, pg.Status
		}
	}
	if pg != nil {
		res.Timings = Timings{LoadMS: pg.LoadMS, IdleMS: pg.IdleMS}
	}
	a.submit(ctx, j, res, host)
}

// SafeFinal drops the query and fragment of a URL before it is sent for a failure.
func SafeFinal(raw string) string {
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		return raw[:i]
	}
	return raw
}

func (a *Agent) submit(ctx context.Context, j Job, res *Result, host string) {
	if err := a.srv.Submit(ctx, res); err != nil {
		if ctx.Err() != nil {
			return
		}
		a.o.Logger.Warn("could not report the result; it will be retried", "watch", j.WatchID, "err", err.Error())
		delete(a.lastLoad, j.WatchID) // let the next poll offer it again
		return
	}
	a.o.Logger.Info("reported", "watch", j.WatchID, "status", res.Status, "class", res.FailureClass)
	if a.o.OnResult != nil {
		a.o.OnResult(host, res.Status, res.FailureClass, a.o.Now())
	}
}
