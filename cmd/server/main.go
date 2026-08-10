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
	"github.com/ziad/liveness-verifier/internal/storage/postgres"
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
	migrate := flag.Bool("migrate", false,
		"apply pending database migrations and exit; run once per deployment, before the new version starts")
	flag.Parse()

	if *healthcheck {
		return probeHealth()
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Migrations are a separate invocation, never something the server does on
	// its way up.
	//
	// A server that migrates at boot is a server that races with itself the
	// moment there is more than one replica, and one that half-migrates when it
	// crashes during startup. Making it a command means a deployment decides
	// when the schema moves, and can stop if it does not.
	if *migrate {
		return runMigrations(cfg)
	}

	log := newLogger(cfg.Log)
	slog.SetDefault(log)

	// A setting that widens what anyone on the network can reach should be
	// visible in the log of every boot, not only in the file somebody edited
	// once.
	if cfg.Server.AllowAnonymousSessions {
		log.Warn("anonymous session creation is enabled: anyone who can reach this port can open a verification session",
			slog.Int("rate_limit_per_min", cfg.Server.RateLimitPerMin),
			slog.String("hint", "set LV_ALLOW_ANONYMOUS_SESSIONS=false and create sessions from your own backend"),
		)
	}

	// A defence that has been switched off has to announce itself on every
	// boot. The alternative is that it stays off because nobody remembers, and
	// the only record is a line in a file nobody opens.
	if !cfg.Liveness.EnforceAntiSpoof {
		log.Warn("passive anti-spoof is NOT enforced: printed photographs and screen replays are not blocked by it",
			slog.Float64("threshold_still_measured", cfg.Liveness.MinScore),
			slog.String("reason", "the bundled MiniFASNetV2 conversion scores real faces near 0.006 and rejects every genuine subject"),
			slog.String("hint", "set LV_LIVENESS_ANTISPOOF_ENFORCE=true once the model is calibrated; this system is not PAD-compliant while it is off"),
		)
	}

	// Everything that can fail about the wiring fails here, before the listener
	// opens. A service that accepts connections it cannot serve is worse than
	// one that refuses to start.
	application, err := build(context.Background(), cfg, log)
	if err != nil {
		return fmt.Errorf("build application: %w", err)
	}
	defer func() {
		if err := application.Close(); err != nil {
			log.Error("shutdown incomplete", slog.String("error", err.Error()))
		}
	}()

	handler, err := httpapi.NewRouter(httpapi.Deps{
		Config:     cfg,
		Logger:     log,
		Liveness:   application.Liveness,
		Enrollment: application.Enrollment,
		Tokens:     application.Tokens,
		Ready:      readinessChecks(application, cfg),
	})
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

// runMigrations applies pending migrations and reports where the schema landed.
//
// Forward only. Rolling a migration back is not something a deployment should
// do on its own: `retries` holds real values, and dropping the column to undo a
// release would take them with it. Reverting the code is safe because every
// migration here is additive — see deploy/deploy.sh for what that buys.
func runMigrations(cfg *config.Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dsn := cfg.Database.URL.Reveal()

	before, err := postgres.MigrationVersion(ctx, dsn)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	if err := postgres.Migrate(ctx, dsn); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	after, err := postgres.MigrationVersion(ctx, dsn)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	// Printed rather than logged: this runs as a one-shot command whose output
	// somebody is reading, and a deployment log that says which versions moved
	// is what makes a failed release diagnosable afterwards.
	if before == after {
		fmt.Printf("schema already at version %d; nothing to apply\n", after)
	} else {
		fmt.Printf("schema moved from version %d to %d\n", before, after)
	}

	if after < postgres.ExpectedSchemaVersion {
		return fmt.Errorf("schema is at %d but this binary needs %d; migrations did not fully apply",
			after, postgres.ExpectedSchemaVersion)
	}
	return nil
}
