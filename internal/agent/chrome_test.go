package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestChromeIntegration drives a real installed browser against local httptest
// servers only. It is skipped when no Chrome or Edge is installed.
func TestChromeIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	path, ok := DetectBrowserHere()
	if !ok {
		t.Skip("no browser installed")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/js", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><head><title>t</title></head><body><div id="p">loading</div>
<script>setTimeout(function(){document.getElementById('p').textContent='From £499 pp';},200)</script></body></html>`)
	})
	mux.HandleFunc("/denied", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprint(w, `<html><title>Access Denied</title></html>`)
	})
	mux.HandleFunc("/redir", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/js?q=1", 302) })
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewChrome(ChromeOptions{Path: path, Profile: t.TempDir(), Headless: true, ExtraFlag: map[string]any{"no-sandbox": true, "disable-gpu": true}})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	r, err := c.Load(ctx, srv.URL+"/js")
	if err != nil {
		t.Skipf("browser would not start here: %v", err)
	}
	if r.Status != 200 || !strings.Contains(r.HTML, "From £499 pp") || strings.Contains(r.HTML, ">loading<") {
		t.Fatalf("rendered html: status %d\n%s", r.Status, r.HTML)
	}
	if r.LoadMS <= 0 {
		t.Fatal("timings")
	}
	r, err = c.Load(ctx, srv.URL+"/denied")
	if err != nil || r.Status != 403 || !DetectChallenge(r.Status, r.HTML) {
		t.Fatalf("denied: %+v %v", r, err)
	}
	r, err = c.Load(ctx, srv.URL+"/redir")
	if err != nil || !strings.HasSuffix(r.FinalURL, "/js?q=1") || r.Status != 200 {
		t.Fatalf("redirect: %+v %v", r, err)
	}
	// connection refused
	dead := httptest.NewServer(http.NotFoundHandler())
	u := dead.URL
	dead.Close()
	_, err = c.Load(ctx, u+"/x")
	if le, ok := err.(*LoadError); !ok || le.Class != "connect" {
		t.Fatalf("refused: %v", err)
	}
	// unresolvable name
	_, err = c.Load(ctx, "http://no-such-host.invalid/x")
	if le, ok := err.(*LoadError); !ok || le.Class != "dns" {
		t.Fatalf("dns: %v", err)
	}
	// the tab count does not grow: every tab is closed after its load
	n := openTabs(t, c)
	for i := 0; i < 20 && n > 1; i++ {
		time.Sleep(250 * time.Millisecond)
		n = openTabs(t, c)
	}
	if n > 1 {
		t.Fatalf("%d tabs left open (only the browser's own start tab may remain)", n)
	}
	// shutdown
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
