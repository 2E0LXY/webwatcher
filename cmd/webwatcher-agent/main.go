// Command webwatcher-agent is the Webwatcher Home Agent. It runs on your own
// always-on PC, opens the pages you have chosen in your installed Chrome or
// Edge, reads the rendered page and reports it to your Webwatcher server.
//
//	webwatcher-agent [run] -server URL [-token TOKEN]   run in the foreground (saves the token)
//	webwatcher-agent install                            Windows: start at logon via a Scheduled Task
//	webwatcher-agent uninstall                          Windows: remove the task
//	webwatcher-agent status                             server, token, browser and last result
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
	"strings"
	"syscall"
	"time"

	"github.com/2E0LXY/webwatcher/internal/agent"
)

var version = "dev"

func main() {
	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var code int
	switch cmd {
	case "run":
		code = cmdRun(args)
	case "install":
		code = cmdInstall(args)
	case "uninstall":
		code = cmdUninstall()
	case "status":
		code = cmdStatus(args)
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintln(os.Stderr, "usage: webwatcher-agent [run|install|uninstall|status|version] [flags]")
		code = 2
	}
	os.Exit(code)
}

type common struct {
	server, token, browser, profile string
	headless, headed                bool
}

func addFlags(fs *flag.FlagSet, c *common) {
	fs.StringVar(&c.server, "server", "", "Webwatcher server address, e.g. https://watch.example.com (also WW_AGENT_SERVER; remembered once saved)")
	fs.StringVar(&c.token, "token", "", "agent token from the dashboard (also WW_AGENT_TOKEN; saved to a protected file on first use)")
	fs.StringVar(&c.browser, "browser", "", "path to chrome.exe or msedge.exe (default: auto-detect)")
	fs.StringVar(&c.profile, "profile", "", "dedicated browser profile folder (default: under %LOCALAPPDATA%\\webwatcher-agent\\profile)")
	fs.BoolVar(&c.headed, "headed", true, "show the browser window, minimised (the default)")
	fs.BoolVar(&c.headless, "headless", false, "run the browser headless (headless=new) instead of a minimised window")
}

func getenv(k string) string { return strings.TrimSpace(os.Getenv(k)) }

func pick(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func cmdRun(args []string) int {
	var c common
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	addFlags(fs, &c)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	d := agent.HereDirs()
	set, _ := agent.LoadSettings(d)
	saved, err := agent.LoadToken(d)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: cannot read the saved token:", err)
		return 1
	}
	server := pick(c.server, getenv("WW_AGENT_SERVER"), set.Server)
	token := pick(c.token, getenv("WW_AGENT_TOKEN"), saved)
	if server == "" || token == "" {
		fmt.Fprintln(os.Stderr, "error: first run needs -server and -token, for example:\n  webwatcher-agent -server https://watch.example.com -token wwa_...\nCreate the token under Home agent on your dashboard.")
		return 2
	}
	client, err := agent.NewClient(server, token)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	// Check the token before saving it, so a typo is never remembered.
	if c.token != "" || getenv("WW_AGENT_TOKEN") != "" {
		_, err := client.Jobs(context.Background())
		if errors.Is(err, agent.ErrRejected) {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not check the token with the server just now:", err)
		}
	}
	if token != saved {
		if err := agent.SaveToken(d, token); err != nil {
			fmt.Fprintln(os.Stderr, "error: could not save the token:", err)
			return 1
		}
		fmt.Fprintln(os.Stderr, "Token saved to", d.Config, "(readable by your account only). You do not need -token again.")
	}
	if server != set.Server {
		agent.SaveSettings(d, agent.Settings{Server: server})
	}

	browserPath := pick(c.browser, getenv("WW_AGENT_BROWSER"))
	if browserPath == "" {
		p, ok := agent.DetectBrowserHere()
		if !ok {
			fmt.Fprintln(os.Stderr, "error: no Chrome or Edge found. Install one, or give its path with -browser.")
			return 1
		}
		browserPath = p
	}
	profile := pick(c.profile, d.Profile)
	if err := os.MkdirAll(profile, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "error: cannot create the browser profile folder:", err)
		return 1
	}

	lf, err := agent.NewRotatingFile(d.Log, agent.DefaultLogCap)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: cannot open the log file:", err)
		return 1
	}
	defer lf.Close()
	logger := slog.New(slog.NewTextHandler(io.MultiWriter(lf, os.Stderr), &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("webwatcher-agent starting", "version", version, "server", client.Base.Host, "browser", baseName(browserPath))

	release, err := agent.AcquireSingleInstance(agent.InstancePort)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer release()

	br := agent.NewChrome(agent.ChromeOptions{Path: browserPath, Profile: profile, Headless: c.headless || !c.headed})
	defer br.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := agent.New(client, br, agent.Options{Logger: logger, OnResult: func(host, status, class string, at time.Time) {
		agent.SaveLastResult(d, agent.LastResult{At: at, Host: host, Status: status, Class: class})
	}})
	a.Run(ctx)
	return 0
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		return p[i+1:]
	}
	return p
}

func cmdInstall(args []string) int {
	d := agent.HereDirs()
	if tok, _ := agent.LoadToken(d); tok == "" {
		fmt.Fprintln(os.Stderr, "error: no saved token yet. Run once first:\n  webwatcher-agent -server https://watch.example.com -token wwa_...\nthen press Ctrl+C and run install.")
		return 1
	}
	if set, _ := agent.LoadSettings(d); set.Server == "" {
		fmt.Fprintln(os.Stderr, "error: no saved server address. Run once with -server first.")
		return 1
	}
	exe, err := agent.ExePath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	if err := agent.Install(agent.Here, agent.ExecRunner, exe, name); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println("Installed. The agent starts at every logon and has been started now. Check it with: webwatcher-agent status")
	return 0
}

func cmdUninstall() int {
	if err := agent.Uninstall(agent.Here, agent.ExecRunner); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println("The scheduled task is removed. Stop any running agent from Task Manager. Your saved token and log files are left in place; delete them if you no longer want them.")
	return 0
}

func cmdStatus(args []string) int {
	var c common
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	addFlags(fs, &c)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	d := agent.HereDirs()
	set, _ := agent.LoadSettings(d)
	saved, _ := agent.LoadToken(d)
	server := pick(c.server, getenv("WW_AGENT_SERVER"), set.Server)
	token := pick(c.token, getenv("WW_AGENT_TOKEN"), saved)
	ok := true
	fmt.Print(agent.StatusReport(agent.StatusInput{
		Server: server, Token: token, Browser: pick(c.browser, getenv("WW_AGENT_BROWSER")), Dirs: d,
		Run: agent.ExecRunner, Goos: agent.Here, Version: version,
	}, &ok))
	if !ok {
		return 1
	}
	return 0
}
