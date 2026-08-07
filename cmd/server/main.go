// Command server runs the Liveness Verifier HTTP service.
//
// It is the only place in the codebase where dependencies are constructed and
// wired together; every other package receives what it needs.
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
	"syscall"
	"time"

	"github.com/ziad/liveness-verifier/internal/config"
	"github.com/ziad/liveness-verifier/internal/httpapi"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	healthcheck := flag.Bool("healthcheck", false,
		"probe the local /healthz endpoint and exit; used by the container HEALTHCHECK")
	flag.Parse()

	if *healthcheck {
		return probeHealth()
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := newLogger(cfg.Log)
	slog.SetDefault(log)

	handler, err := httpapi.NewRouter(httpapi.Deps{Config: cfg, Logger: log})
	if err != nil {
		return fmt.Errorf("build router: %w", err)
	}

	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.Server.ReadTimeout,
		ReadTimeout:       cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout,
		IdleTimeout:       cfg.Server.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		log.Info("server listening",
			slog.String("addr", srv.Addr),
			slog.String("pipeline_mode", string(cfg.Models.Mode)),
			slog.String("log_level", cfg.Log.Level.String()),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		// Restore default signal handling so a second Ctrl-C kills the process
		// immediately instead of waiting out the grace period.
		stop()
		log.Info("shutdown signal received, draining",
			slog.Duration("grace_period", cfg.Server.ShutdownTimeout))
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	log.Info("shutdown complete")
	return nil
}

func newLogger(c config.Log) *slog.Logger {
	opts := &slog.HandlerOptions{Level: c.Level}

	var h slog.Handler
	if c.Format == "text" {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

// probeHealth performs the container health check from inside the container, so
// the runtime image does not need curl or wget installed.
//
// It reads the listen address straight from the environment rather than through
// config.Load: an unrelated configuration error should not change what the
// probe reports.
func probeHealth() error {
	addr := os.Getenv("LV_HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse LV_HTTP_ADDR %q: %w", addr, err)
	}
	// A wildcard bind is not a valid destination; dial the loopback instead.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	url := "http://" + net.JoinHostPort(host, port) + "/healthz"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build probe request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("probe %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probe %s: status %d", url, resp.StatusCode)
	}
	return nil
}
