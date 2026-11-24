package agent

import (
	"context"
	"testing"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// openTabs counts the page targets of the running browser.
func openTabs(t *testing.T, c *Chrome) int {
	t.Helper()
	c.mu.Lock()
	b := c.browser
	c.mu.Unlock()
	var infos []*target.Info
	if err := chromedp.Run(b, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		infos, err = target.GetTargets().Do(ctx)
		return err
	})); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, i := range infos {
		if i.Type == "page" {
			n++
		}
	}
	return n
}
