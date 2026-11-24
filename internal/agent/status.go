package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// StatusInput is everything `status` needs. Token is used to call the server and is never printed.
type StatusInput struct {
	Server, Token, Browser string
	Dirs                   Dirs
	Run                    Runner
	Goos, Version          string
	Connect                func(server, token string) (Server, error) // test seam
	DetectBrowser          func() (string, bool)                      // test seam
	Now                    func() time.Time
}

// StatusReport builds the text of `status`. It sets *ok to false if something needs fixing.
func StatusReport(in StatusInput, ok *bool) string {
	var b strings.Builder
	line := func(label, v string) { fmt.Fprintf(&b, "%-14s %s\n", label+":", v) }
	fail := func() { *ok = false }
	if in.Connect == nil {
		in.Connect = func(s, t string) (Server, error) { return NewClient(s, t) }
	}
	if in.DetectBrowser == nil {
		in.DetectBrowser = DetectBrowserHere
	}
	if in.Now == nil {
		in.Now = time.Now
	}
	line("version", in.Version)
	switch {
	case in.Server == "":
		line("server", "not set (run once with -server)")
		fail()
	case in.Token == "":
		line("server", in.Server)
		line("token", "none saved (run once with -token)")
		fail()
	default:
		line("server", in.Server)
		srv, err := in.Connect(in.Server, in.Token)
		if err != nil {
			line("server check", err.Error())
			fail()
			break
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_, err = srv.Jobs(ctx)
		cancel()
		switch {
		case err == nil:
			line("reachable", "yes")
			line("token", "valid")
		case errors.Is(err, ErrRejected):
			line("reachable", "yes")
			line("token", "REJECTED: revoked, replaced, or this is not the right server address")
			fail()
		default:
			var rl *RateLimitedError
			if errors.As(err, &rl) {
				line("reachable", "yes (rate limited just now; try again in a minute)")
			} else {
				line("reachable", "NO: "+err.Error())
				fail()
			}
		}
	}
	path, found := in.Browser, in.Browser != ""
	if !found {
		path, found = in.DetectBrowser()
	}
	if found {
		line("browser", path)
	} else {
		line("browser", "NOT FOUND: install Chrome or Edge, or use -browser")
		fail()
	}
	if in.Goos == "windows" {
		if TaskInstalled(in.Run) {
			line("task", "installed (starts at logon)")
		} else {
			line("task", "not installed (run: webwatcher-agent install)")
		}
	}
	if r, have := LoadLastResult(in.Dirs); have {
		ago := in.Now().Sub(r.At).Round(time.Second)
		s := fmt.Sprintf("%s for %s, %s ago", r.Status, r.Host, ago)
		if r.Class != "" {
			s += " (" + r.Class + ")"
		}
		line("last result", s)
	} else {
		line("last result", "none yet")
	}
	line("log", in.Dirs.Log)
	return b.String()
}
