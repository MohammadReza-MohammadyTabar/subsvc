// Command subsvc is a lightweight subscription collector and tester.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"subsvc/internal/api"
	"subsvc/internal/config"
	"subsvc/internal/service"
	"subsvc/internal/settings"
	"subsvc/internal/store"
	"subsvc/internal/subscription"
	"subsvc/internal/tester"
)

var version = "dev"

func main() {
	cfgPath := flag.String("config", "", "optional legacy config file (deployment keys only; SUBSVC_* environment variables take precedence)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("subsvc", version)
		return
	}
	if err := run(*cfgPath); err != nil {
		fmt.Fprintln(os.Stderr, "subsvc:", err)
		os.Exit(1)
	}
}

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}

func run(cfgPath string) error {
	cfg, err := config.Load(cfgPath, os.Getenv)
	if err != nil {
		return err
	}
	// Logs go to stderr (journal / docker logs). Secrets are never logged: only
	// the SOURCE of the auth token is.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(cfg.LogLevel)}))
	for _, w := range cfg.Warnings {
		log.Warn(w)
	}

	// A network-reachable API is never left open: without a token a random one
	// is generated once and kept in <data_dir>/auth_token.
	if err := cfg.EnsureToken(); err != nil {
		return err
	}
	if cfg.TokenSource == "generated" {
		log.Warn("no SUBSVC_AUTH_TOKEN set: generated an access token", "file", filepath.Join(cfg.DataDir, "auth_token"))
	}
	if cfg.AuthToken == "" {
		log.Warn("API authentication is disabled (loopback only); set SUBSVC_AUTH_TOKEN to enable it")
	}

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	// Operational settings: stored in <data_dir>/settings.json, editable via the API/UI.
	mgr, err := settings.Open(cfg.DataDir, cfg.Limits)
	if err != nil {
		return err
	}
	// Temporary per-test files (credentials, sockets) live here; clear anything
	// a crash left behind.
	workDir := filepath.Join(cfg.DataDir, "run")
	_ = os.RemoveAll(workDir)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", workDir, err)
	}

	newFetcher := func(s settings.Settings) service.Fetcher {
		return subscription.NewFetcher(time.Duration(s.FetchTimeoutSec)*time.Second, s.FetchMaxBodyBytes, s.UserAgent)
	}
	probe := tester.New(tester.Options{CorePath: cfg.CorePath, WorkDir: workDir})
	svc := service.New(log, st, newFetcher, probe, mgr)

	if _, err := os.Stat(cfg.CorePath); err != nil {
		log.Warn("proxy core not found: tests will fail until it is installed", "path", cfg.CorePath)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.New(svc, log, cfg.AuthToken, mgr, cfg.Summary()),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	cur := mgr.Get()
	log.Info("started", "version", version, "listen", ln.Addr().String(), "data_dir", cfg.DataDir,
		"test_concurrency", cur.TestConcurrency, "youtube", cur.YouTubeEnabled, "speed", cur.SpeedEnabled)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case s := <-sig:
		log.Info("shutting down", "signal", s.String())
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	svc.Shutdown(ctx) // stops a running job and its proxy-core processes
	if err := st.Flush(); err != nil {
		log.Error("final save", "error", err)
	}
	return nil
}
