// Command api serves FirmScout's public HTTP API.
//
// The same binary runs under Docker Compose, on ECS Fargate, and on AWS Lambda behind
// the Lambda Web Adapter. Nothing here knows which, which is the property that makes
// the runtime decision in ADR-0010 reversible.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/macimottin/firmscout/internal/adapters/httpapi"
	"github.com/macimottin/firmscout/internal/adapters/telemetry"
	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/platform"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// One subcommand, and it exists for the container rather than for a person.
	//
	// The final image is gcr.io/distroless/static-debian12, which has no shell, no
	// curl and no wget, so neither a Dockerfile HEALTHCHECK nor a Compose healthcheck
	// has anything to exec. The binary is the only executable in the image, so it has
	// to be able to probe itself. infrastructure/docker/docker-compose.yml has assumed
	// this subcommand existed since before this file did; it did not, so the api
	// container reported unhealthy forever while serving traffic perfectly.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := healthcheck(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "firmscout-api healthcheck: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "firmscout-api: %v\n", err)
		os.Exit(1)
	}
}

// healthcheck performs one local HTTP GET and reports the result as an exit code.
//
// It deliberately shares nothing with run(): no config load, no database pool, no
// telemetry. A probe that needed the process's own dependencies to start would report
// the health of a second copy of the service rather than of the one being probed, and
// would fail for reasons — an unreachable database, a missing environment variable —
// that say nothing about whether this process is answering requests.
func healthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	url := fs.String("url", "http://127.0.0.1:8080/healthz", "the endpoint to probe")
	timeout := fs.Duration("timeout", 3*time.Second, "how long to wait for a response")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain before closing so the connection can be reused; a probe that leaks a
	// connection every ten seconds is a slow leak in a long-lived container.
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %s", *url, resp.Status)
	}
	return nil
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

	// The reviewer surface is built only when it is switched on, and the use cases are
	// left nil when it is not. Passing them unconditionally and letting the router's
	// own flag decide would leave three live, write-capable objects reachable from a
	// process that is not meant to expose them -- a single mistaken condition away from
	// serving unauthenticated writes. Nil is the safer default because it cannot be
	// re-enabled by a typo. See ADR-0021.
	var (
		reviewQueue  *application.ListReviewQueue
		reviewItems  *application.GetReviewItem
		reviewDecide *application.DecideReviewItem
	)
	if cfg.ReviewAPIEnabled {
		reviewQueue = application.NewListReviewQueue(c.ReviewQueryDeps())
		reviewItems = application.NewGetReviewItem(c.ReviewQueryDeps())
		reviewDecide = application.NewDecideReviewItem(
			c.ReviewDeps(application.NewPublishRelease(c.IngestDeps())))
		logger.Warn("internal review API enabled; decisions are recorded against an asserted, unauthenticated actor (ADR-0021)")
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
		// Without this, /readyz answers 200 unconditionally -- including from an
		// instance whose database is unreachable, which is the one case a readiness
		// probe exists to catch. postgres.DB.Ping has carried the comment "for
		// readiness probes" since the first slice and was called by nothing.
		Ready:            c.DB.Ping,
		ReviewQueue:      reviewQueue,
		ReviewItems:      reviewItems,
		Review:           reviewDecide,
		ReviewAPIEnabled: cfg.ReviewAPIEnabled,
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
