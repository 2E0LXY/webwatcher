package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const tok = "wwa_SUPERSECRETTOKENVALUE0123456789abcdefghij"

func TestClientJobsAndSubmit(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/api/agent/jobs":
			io.WriteString(w, `{"server_time":1,"poll_after_sec":60,"jobs":[{"watch_id":7,"url":"https://www.jet2.com/p","selector":".x","version":2,"interval_min":60,"min_delay_sec":3600,"next_check_at":5}]}`)
		case "/api/agent/result":
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			io.WriteString(w, `{"recorded":true}`)
		}
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, tok)
	if err != nil {
		t.Fatal(err)
	}
	jl, err := c.Jobs(context.Background())
	if err != nil || len(jl.Jobs) != 1 || jl.Jobs[0].WatchID != 7 || jl.Jobs[0].Selector != ".x" || jl.Jobs[0].MinDelaySec != 3600 {
		t.Fatalf("%+v %v", jl, err)
	}
	if gotAuth != "Bearer "+tok {
		t.Fatal("bearer header")
	}
	if err := c.Submit(context.Background(), &Result{WatchID: 7, Version: 2, Status: "ok", RenderedHTML: "<p>x</p>", FinalURL: "https://www.jet2.com/p", HTTPStatus: 200}); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal([]byte(gotBody), &m)
	if m["watch_id"].(float64) != 7 || m["status"] != "ok" || m["rendered_html"] != "<p>x</p>" || m["final_url"] == nil || m["timings"] == nil {
		t.Fatalf("%s", gotBody)
	}
}

func TestClientErrors(t *testing.T) {
	code, retry := 404, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if retry != "" {
			w.Header().Set("Retry-After", retry)
		}
		w.WriteHeader(code)
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, tok)
	if _, err := c.Jobs(context.Background()); err != ErrRejected {
		t.Fatalf("404: %v", err)
	}
	code, retry = 429, "120"
	_, err := c.Jobs(context.Background())
	if rl, ok := err.(*RateLimitedError); !ok || rl.After != 120*time.Second {
		t.Fatalf("429: %v", err)
	}
	code = 500
	if _, err := c.Jobs(context.Background()); err == nil || strings.Contains(err.Error(), tok) {
		t.Fatalf("500: %v", err)
	}
	// redirects are not followed (a token must never be replayed elsewhere)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("followed redirect") }))
	defer other.Close()
	red := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 302) }))
	defer red.Close()
	c2, _ := NewClient(red.URL, tok)
	if _, err := c2.Jobs(context.Background()); err == nil {
		t.Fatal("redirect treated as success")
	}
}

func TestClientNeverLeaksTokenInErrors(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // connection refused
	c, _ := NewClient(url, tok)
	_, err := c.Jobs(context.Background())
	if err == nil || strings.Contains(err.Error(), tok) {
		t.Fatalf("%v", err)
	}
	err = c.Submit(context.Background(), &Result{Status: "error"})
	if err == nil || strings.Contains(err.Error(), tok) {
		t.Fatalf("%v", err)
	}
}

func TestNewClientValidation(t *testing.T) {
	for _, s := range []string{"", "watch.example.com", "ftp://watch.example.com", "http://watch.example.com", "https://"} {
		if _, err := NewClient(s, tok); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
	for _, s := range []string{"https://watch.example.com", "https://watch.example.com/", "http://localhost:8080", "http://127.0.0.1:1"} {
		if _, err := NewClient(s, tok); err != nil {
			t.Errorf("refused %q: %v", s, err)
		}
	}
	if _, err := NewClient("https://watch.example.com", ""); err == nil {
		t.Error("empty token accepted")
	}
}

func TestTokenFileProtectedAndNeverInStatus(t *testing.T) {
	d := Dirs{Config: filepath.Join(t.TempDir(), "cfg"), Data: t.TempDir()}
	d.Log = filepath.Join(d.Data, "agent.log")
	if err := SaveToken(d, tok); err != nil {
		t.Fatal(err)
	}
	got, err := LoadToken(d)
	if err != nil || got != tok {
		t.Fatal(got, err)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(filepath.Join(d.Config, "token"))
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode())
		}
		ds, _ := os.Stat(d.Config)
		if ds.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode %v", ds.Mode())
		}
	}
	if tk, _ := LoadToken(Dirs{Config: t.TempDir()}); tk != "" {
		t.Fatal("token from nowhere")
	}

	SaveLastResult(d, LastResult{At: time.Now().Add(-time.Minute), Host: "www.jet2.com", Status: "blocked", Class: "challenge"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"jobs":[]}`) }))
	defer srv.Close()
	ok := true
	out := StatusReport(StatusInput{Server: srv.URL, Token: tok, Browser: "/usr/bin/chromium", Dirs: d, Goos: "windows", Version: "1.0",
		Run: func(string, ...string) (string, error) { return "", nil }}, &ok)
	if !ok || strings.Contains(out, tok) || strings.Contains(out, "SUPERSECRET") {
		t.Fatalf("ok=%v\n%s", ok, out)
	}
	for _, want := range []string{"reachable:", "yes", "valid", "/usr/bin/chromium", "installed", "blocked for www.jet2.com", "(challenge)"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
}

func TestStatusReportProblems(t *testing.T) {
	d := Dirs{Config: t.TempDir(), Log: "x.log"}
	run := func(string, ...string) (string, error) { return "", os.ErrNotExist }
	rej := httptest.NewServer(http.NotFoundHandler())
	defer rej.Close()
	cases := []struct {
		name string
		in   StatusInput
		want string
	}{
		{"no server", StatusInput{Dirs: d, Run: run, Goos: "linux", DetectBrowser: func() (string, bool) { return "", false }}, "not set"},
		{"no token", StatusInput{Server: "https://x.example", Dirs: d, Run: run, Goos: "linux", Browser: "b"}, "none saved"},
		{"rejected", StatusInput{Server: rej.URL, Token: tok, Dirs: d, Run: run, Goos: "linux", Browser: "b"}, "REJECTED"},
		{"unreachable", StatusInput{Server: "http://127.0.0.1:1", Token: tok, Dirs: d, Run: run, Goos: "linux", Browser: "b"}, "NO:"},
		{"no browser", StatusInput{Server: rej.URL, Token: tok, Dirs: d, Run: run, Goos: "linux", DetectBrowser: func() (string, bool) { return "", false }}, "NOT FOUND"},
		{"task absent", StatusInput{Server: rej.URL, Token: tok, Dirs: d, Run: run, Goos: "windows", Browser: "b"}, "not installed"},
	}
	for _, c := range cases {
		ok := true
		out := StatusReport(c.in, &ok)
		if ok || !strings.Contains(out, c.want) || strings.Contains(out, tok) {
			t.Errorf("%s: ok=%v\n%s", c.name, ok, out)
		}
	}
}

func TestRotatingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "agent.log")
	r, err := NewRotatingFile(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	line := bytes.Repeat([]byte("x"), 39)
	line = append(line, '\n')
	for i := 0; i < 10; i++ {
		if _, err := r.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	cur, _ := os.Stat(p)
	old, err := os.Stat(p + ".1")
	if err != nil || cur.Size() > 100 || old.Size() > 100 {
		t.Fatalf("rotation: %v %v %v", cur, old, err)
	}
	if _, err := os.Stat(p + ".2"); err == nil {
		t.Fatal("more than one old file kept")
	}
	if runtime.GOOS != "windows" && cur.Mode().Perm() != 0o600 {
		t.Fatalf("log mode %v", cur.Mode())
	}
}

func TestSingleInstance(t *testing.T) {
	rel, err := AcquireSingleInstance(0)
	if err != nil {
		t.Skip("cannot listen:", err)
	}
	rel()
	l, err := AcquireSingleInstance(47999)
	if err != nil {
		t.Skip("port busy")
	}
	if _, err := AcquireSingleInstance(47999); err == nil {
		t.Fatal("second instance allowed")
	}
	l()
	if l2, err := AcquireSingleInstance(47999); err != nil {
		t.Fatal("not released")
	} else {
		l2()
	}
}
