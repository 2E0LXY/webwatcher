package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// Page timing. The hard limit covers navigation up to the load event; the idle
// wait afterwards is best effort and never fails a load.
const (
	PageTimeout = 45 * time.Second
	IdleWait    = 10 * time.Second
	evalTimeout = 15 * time.Second
)

// ChromeOptions configure the browser the agent drives.
type ChromeOptions struct {
	Path      string // chrome.exe, msedge.exe or another Chromium
	Profile   string // dedicated user-data-dir, never the owner's main profile
	Headless  bool   // default is a headed, minimised window
	ExtraFlag map[string]any
}

// Chrome drives the owner's installed Chrome or Edge through the DevTools
// protocol. It keeps one browser instance and one tab per load. It uses the
// browser's own user agent and sets no flag that hides automation.
type Chrome struct {
	opt ChromeOptions

	mu      sync.Mutex
	allocC  context.CancelFunc
	browser context.Context
	brC     context.CancelFunc
}

func NewChrome(o ChromeOptions) *Chrome { return &Chrome{opt: o} }

func (c *Chrome) allocFlags() []chromedp.ExecAllocatorOption {
	opts := []chromedp.ExecAllocatorOption{
		chromedp.ExecPath(c.opt.Path),
		chromedp.UserDataDir(c.opt.Profile),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
	}
	if c.opt.Headless {
		opts = append(opts, chromedp.Flag("headless", "new"))
	} else {
		opts = append(opts, chromedp.Flag("headless", false), chromedp.Flag("start-minimized", true))
	}
	for k, v := range c.opt.ExtraFlag {
		opts = append(opts, chromedp.Flag(k, v))
	}
	return opts
}

// start launches the single browser instance if it is not running.
func (c *Chrome) start(ctx context.Context) (context.Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.browser != nil && c.browser.Err() == nil {
		return c.browser, nil
	}
	c.stopLocked()
	actx, acancel := chromedp.NewExecAllocator(context.Background(), c.allocFlags()...)
	bctx, bcancel := chromedp.NewContext(actx)
	// The first Run must use bctx itself: the browser lives and dies with that
	// context, so a derived timeout context would kill it on cancel. A watchdog
	// enforces the start-up limit instead.
	watchdog := time.AfterFunc(30*time.Second, bcancel)
	err := chromedp.Run(bctx)
	watchdog.Stop()
	if err != nil {
		bcancel()
		acancel()
		return nil, &LoadError{Class: "other", Err: fmt.Errorf("could not start the browser: %w", err)}
	}
	c.allocC, c.browser, c.brC = acancel, bctx, bcancel
	return bctx, nil
}

func (c *Chrome) stopLocked() {
	if c.brC != nil {
		c.brC()
	}
	if c.allocC != nil {
		c.allocC()
	}
	c.allocC, c.browser, c.brC = nil, nil, nil
}

// Close shuts the browser down.
func (c *Chrome) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
	return nil
}

// Load opens rawURL in a new tab, waits for the load event (at most PageTimeout)
// and then for the network to go idle (at most IdleWait), reads the rendered
// document and closes the tab. It takes no screenshot and solves nothing.
func (c *Chrome) Load(ctx context.Context, rawURL string) (*Rendered, error) {
	if !AllowedURL(rawURL) {
		return nil, &LoadError{Class: "nav_error", Err: errRefused}
	}
	bctx, err := c.start(ctx)
	if err != nil {
		return nil, err
	}
	tabCtx, closeTab := chromedp.NewContext(bctx) // a new tab in the same browser
	defer closeTab()                              // closes the tab
	stop := context.AfterFunc(ctx, closeTab)      // shutdown closes it promptly
	defer stop()

	var mu sync.Mutex
	var docStatus int
	var docURL string
	var docLoader cdp.LoaderID
	idle := make(chan struct{})
	var idleOnce sync.Once
	chromedp.ListenTarget(tabCtx, func(ev any) {
		switch e := ev.(type) {
		case *network.EventResponseReceived:
			if e.Type == network.ResourceTypeDocument && e.Response != nil {
				mu.Lock()
				// the last main-frame document response wins (redirects end on the final one)
				docStatus, docURL, docLoader = int(e.Response.Status), e.Response.URL, e.LoaderID
				mu.Unlock()
			}
		case *page.EventLifecycleEvent:
			// Enabling lifecycle events replays those of the blank start page, so
			// only an idle event of the page's own document counts.
			mu.Lock()
			mine := docLoader != "" && e.LoaderID == docLoader
			mu.Unlock()
			if e.Name == "networkIdle" && mine {
				idleOnce.Do(func() { close(idle) })
			}
		}
	})

	start := time.Now()
	navCtx, navCancel := context.WithTimeout(tabCtx, PageTimeout)
	defer navCancel()
	err = chromedp.Run(navCtx,
		network.Enable(),
		page.SetLifecycleEventsEnabled(true),
		chromedp.Navigate(rawURL),
	)
	loadMS := time.Since(start).Milliseconds()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) || navCtx.Err() != nil {
			return nil, &LoadError{Class: "timeout", Err: errors.New("page did not finish loading in time")}
		}
		return nil, &LoadError{Class: ClassifyNavError(err.Error()), Err: err}
	}

	idleStart := time.Now()
	select {
	case <-idle:
	case <-time.After(IdleWait):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	idleMS := time.Since(idleStart).Milliseconds()

	evalCtx, evalCancel := context.WithTimeout(tabCtx, evalTimeout)
	defer evalCancel()
	var html, loc string
	if err := chromedp.Run(evalCtx,
		chromedp.Evaluate(`document.documentElement.outerHTML`, &html),
		chromedp.Location(&loc),
	); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &LoadError{Class: ClassifyNavError(err.Error()), Err: err}
	}
	mu.Lock()
	defer mu.Unlock()
	if loc == "" {
		loc = docURL
	}
	return &Rendered{HTML: html, FinalURL: loc, Status: docStatus, LoadMS: loadMS, IdleMS: idleMS}, nil
}
