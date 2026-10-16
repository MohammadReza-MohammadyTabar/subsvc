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
	"subsvc/internal/store"
	"subsvc/internal/subscription"
	"subsvc/internal/tester"
)

var version = "dev"

func main() {
	cfgPath := flag.String("config", "", "path of the JSON config file (default "+config.DefaultPath+", built-in defaults if missing)")
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
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	// Logs go to stderr; under systemd they end up in the journal. No timestamps
	// are needed there, but they are harmless when running in a terminal.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(cfg.LogLevel)}))

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	// Temporary per-test files live here; clear anything a crash left behind.
	workDir := filepath.Join(cfg.DataDir, "run")
	_ = os.RemoveAll(workDir)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", workDir, err)
	}

	fetcher := subscription.NewFetcher(cfg.FetchTimeout(), cfg.MaxBodyBytes, cfg.UserAgent)
	probe := tester.New(tester.Options{
		CorePath:       cfg.Test.CorePath,
		URL:            cfg.Test.URL,
		Timeout:        cfg.TestTimeout(),
		StartupTimeout: cfg.StartupTimeout(),
		WorkDir:        workDir,
	})
	svc := service.New(log, st, fetcher, probe, service.Options{
		FetchConcurrency: cfg.FetchConcurrency,
		TestConcurrency:  cfg.Test.Concurrency,
	})

	if _, err := os.Stat(cfg.Test.CorePath); err != nil {
		log.Warn("proxy core not found: tests will fail until it is installed", "path", cfg.Test.CorePath)
	}
	if cfg.AuthToken == "" {
		if host, _, err := net.SplitHostPort(cfg.Listen); err != nil || !isLoopback(host) {
			log.Warn("API is reachable from the network without an auth_token; set auth_token in the config")
		}
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.New(svc, log, cfg.AuthToken),
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
	log.Info("started", "version", version, "listen", ln.Addr().String(), "data_dir", cfg.DataDir,
		"test_url", cfg.Test.URL, "test_timeout_ms", cfg.Test.TimeoutMS, "test_concurrency", cfg.Test.Concurrency)

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

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
