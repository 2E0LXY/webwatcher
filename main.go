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
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
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
	msgr := app.NewMessenger(cfg)
	if cfg.Messenger == "log" && !cfg.Dev {
		slog.Warn("WW_MESSENGER=log writes login links to the log; use it for development only")
	}
	auth := app.NewAuth(store, msgr, clock, cfg.BaseURL)
	srv, err := app.NewServer(cfg, store, auth, msgr, clock)
	if err != nil {
		return err
	}
	sched := app.NewScheduler(store, app.NewHTTPFetcher(), msgr, clock, cfg.BaseURL)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); sched.Run(ctx) }()
	go func() { defer wg.Done(); app.RunMaintenance(ctx, db, store, cfg.Dir, clock) }()

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
