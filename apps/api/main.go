// Command api serves FirmScout's public HTTP API.
//
// The same binary runs under Docker Compose, on ECS Fargate, and on AWS Lambda behind
// the Lambda Web Adapter. Nothing here knows which, which is the property that makes
// the runtime decision in ADR-0010 reversible.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/macimottin/firmscout/internal/adapters/httpapi"
	"github.com/macimottin/firmscout/internal/adapters/telemetry"
	"github.com/macimottin/firmscout/internal/platform"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "firmscout-api: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := platform.Load("firmscout-api")
	if err != nil {
		return err
	}
	cfg.Version = version

	// Telemetry is set up before anything else so that startup failures are traced
	// too. An unset or unreachable OTLP endpoint degrades to local metrics with no
	// tracing rather than preventing the service from starting: a missing collector
	// must never take the API down.
	tp, err := telemetry.New(ctx, telemetry.Config{
		ServiceName:    cfg.Service,
		ServiceVersion: cfg.Version,
		Environment:    cfg.Environment,
		OTLPEndpoint:   cfg.OTLPEndpoint,
		OTLPInsecure:   cfg.OTLPInsecure,
	})
	if err != nil {
		return fmt.Errorf("set up telemetry: %w", err)
	}
	tp.InstallGlobals()

	logger := tp.Logger(os.Stderr, logLevel(cfg.LogLevel)).With(
		slog.String("service", cfg.Service),
		slog.String("version", cfg.Version),
		slog.String("environment", cfg.Environment),
	)

	c, err := platform.Build(ctx, cfg)
	if err != nil {
		return err
	}
	c.Logger = logger
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), platform.ShutdownTimeout)
		defer cancel()
		_ = c.Close(shutdownCtx)
		_ = tp.Shutdown(shutdownCtx)
	}()

	// Refuse to serve against a schema the code does not match. Serving stale or
	// partially-migrated data would be a correctness failure in a product whose only
	// asset is being right.
	status, err := c.Migrator.Status(ctx)
	if err != nil {
		return fmt.Errorf("read migration status: %w", err)
	}
	if len(status.Pending) > 0 {
		return fmt.Errorf("%d migration(s) pending; run 'firmscout migrate up' before starting the API", len(status.Pending))
	}

	server, err := httpapi.NewServer(httpapi.Deps{
		Summaries:      c.Summaries,
		Vendors:        c.Vendors,
		Releases:       c.Releases,
		Events:         c.Events,
		Clock:          c.Clock,
		IDs:            c.IDs,
		Metrics:        tp.Metrics(),
		Gatherer:       tp.Gatherer(),
		TracerProvider: nil,
		Propagator:     tp.Propagator(),
		Logger:         logger,
	})
	if err != nil {
		return fmt.Errorf("build HTTP server: %w", err)
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           server,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		logger.Info("listening", slog.String("addr", cfg.HTTPAddr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		logger.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTPShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("stopped")
	return nil
}

func logLevel(s string) slog.Leveler {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
