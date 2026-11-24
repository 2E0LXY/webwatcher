// Command webwatcher watches web pages for price changes and alerts via WhatsApp.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/2E0LXY/webwatcher/internal/app"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "doctor" || os.Args[1] == "adduser" || os.Args[1] == "loginlink" || os.Args[1] == "permalink" || os.Args[1] == "setadmin" || os.Args[1] == "agenttoken") {
		os.Exit(subcommand(os.Args[1], os.Args[2:]))
	}
	showVersion := flag.Bool("version", false, "print version and exit")
	decrypt := flag.String("decrypt-backup", "", "decrypt an encrypted backup file (key from WW_BACKUP_KEY) and exit")
	out := flag.String("out", "", "output file for -decrypt-backup")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if *decrypt != "" {
		if err := decryptBackup(*decrypt, *out); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func decryptBackup(in, out string) error {
	if out == "" {
		return errors.New("-out is required")
	}
	cfg, err := app.LoadConfig(os.Getenv)
	if err != nil {
		return err
	}
	if cfg.BackupKey == nil {
		return errors.New("WW_BACKUP_KEY is not set")
	}
	data, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	plain, err := app.DecryptBackup(cfg.BackupKey, data)
	if err != nil {
		return err
	}
	return os.WriteFile(out, plain, 0o600)
}

func run() error {
	app.Version = version
	app.SetUserAgentVersion(version)
	cfg, err := app.LoadConfig(os.Getenv)
	if err != nil {
		return err
	}
	db, err := app.OpenDB(cfg.Dir)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	clock := app.SystemClock()
	store := app.NewStore(db, clock)
	store.UseDashboardAlerts(cfg.Messenger == "dashboard")
	msgr := app.NewMessenger(cfg)
	if cfg.BackupKey == nil {
		slog.Warn("WW_BACKUP_KEY is not set: backups are written unencrypted (file mode 0600, kept 7 days)")
	}
	auth := app.NewAuth(store, msgr, clock, cfg.BaseURL)
	auth.SetSendBudget(cfg.SendBudget)
	auth.SetSessionTTL(cfg.SessionLifetime())
	srv, err := app.NewServer(cfg, store, auth, msgr, clock)
	if err != nil {
		return err
	}
	fetcher := app.NewHTTPFetcher()
	fetcher.SetRespectRobots(cfg.RespectRobots)
	sched := app.NewScheduler(store, fetcher, clock, cfg.BaseURL)
	fetcher.SetHostLimiter(sched.Limiter())
	sender := app.NewSender(store, msgr, clock, cfg.SendBudget)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); sched.Run(ctx) }()
	go func() { defer wg.Done(); sender.Run(ctx) }()
	go func() {
		defer wg.Done()
		app.RunMaintenance(ctx, db, store, clock, app.MaintenanceConfig{Dir: cfg.Dir, BackupKey: cfg.BackupKey, RetainHistoryDays: cfg.RetainHistoryDays, RetainDiagDays: cfg.RetainDiagDays})
	}()

	hs := &http.Server{
		Addr: cfg.Addr, Handler: srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	slog.Info("webwatcher listening", "addr", cfg.Addr, "base_url", cfg.BaseURL, "messenger", msgr.Name(), "version", version)

	select {
	case err := <-errc:
		stop()
		wg.Wait()
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("http shutdown", "err", err)
	}
	srv.Wait()
	wg.Wait()
	return nil
}

// subcommand runs the operator commands `doctor`, `adduser`, `loginlink`, `permalink` and `setadmin` and returns the
// process exit status. Both read the same settings as the service.
func subcommand(name string, args []string) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	envFile := fs.String("env", "", "read settings from this file instead of the environment (default: the environment, else "+app.DefaultEnvFile+")")
	revoke := fs.Bool("revoke", false, "permalink/setadmin: revoke instead of create")
	status := fs.Bool("show-status", false, "permalink: print status, never the token")
	admin := fs.Bool("admin", false, "adduser/permalink: also mark the account admin")
	// Allow the phone number before or after flags.
	var rest []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return 2
		}
		args = fs.Args()
		if len(args) > 0 {
			rest = append(rest, args[0])
			args = args[1:]
		}
	}
	getenv, err := app.EnvLookup(*envFile, os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	switch name {
	case "doctor":
		if len(rest) != 0 {
			fmt.Fprintln(os.Stderr, "usage: webwatcher doctor [-env FILE]")
			return 2
		}
		if app.Doctor(getenv, os.Stdout) != 0 {
			return 1
		}
	case "loginlink":
		if len(rest) != 1 {
			fmt.Fprintln(os.Stderr, "usage: webwatcher loginlink [-env FILE] +447700900123")
			return 2
		}
		if err := app.LoginLink(getenv, rest[0], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
	case "permalink":
		if len(rest) != 1 {
			fmt.Fprintln(os.Stderr, "usage: webwatcher permalink [-env FILE] [--revoke | --show-status] [--admin] +447700900123")
			return 2
		}
		if err := app.Permalink(getenv, rest[0], app.PermalinkOpts{Revoke: *revoke, Status: *status, Admin: *admin}, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
	case "setadmin":
		if len(rest) != 1 {
			fmt.Fprintln(os.Stderr, "usage: webwatcher setadmin [-env FILE] [--revoke] +447700900123")
			return 2
		}
		if err := app.SetAdmin(getenv, rest[0], *revoke, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
	case "agenttoken":
		if len(rest) != 1 {
			fmt.Fprintln(os.Stderr, "usage: webwatcher agenttoken [-env FILE] [--revoke | --show-status] +447700900123")
			return 2
		}
		if err := app.AgentToken(getenv, rest[0], app.AgentTokenOpts{Revoke: *revoke, Status: *status}, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
	case "adduser":
		if len(rest) != 1 {
			fmt.Fprintln(os.Stderr, "usage: webwatcher adduser [-env FILE] +447700900123")
			return 2
		}
		add := app.AddUser
		if *admin {
			add = app.AddUserAdmin
		}
		if err := add(getenv, rest[0], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
	}
	return 0
}
