package platform

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/macimottin/firmscout/internal/adapters/artifact"
	"github.com/macimottin/firmscout/internal/adapters/collectors"
	"github.com/macimottin/firmscout/internal/adapters/fetch"
	"github.com/macimottin/firmscout/internal/adapters/normalize"
	"github.com/macimottin/firmscout/internal/adapters/postgres"
	"github.com/macimottin/firmscout/internal/adapters/registry"
	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// Container holds the assembled object graph.
//
// This is the only place in FirmScout where a concrete adapter meets a use case. Every
// other package speaks to ports, which is what allows the domain to be tested in
// microseconds and the queue, artifact store and AI provider to change without
// touching a business rule.
type Container struct {
	Config Config
	Logger *slog.Logger

	DB       *postgres.DB
	Migrator *postgres.Migrator

	Vendors    application.VendorRepository
	Categories application.CategoryRepository
	Products   application.ProductRepository
	Sources    application.SourceRepository
	Candidates application.CandidateRepository
	Releases   application.ReleaseRepository
	Evidence   application.EvidenceRepository
	Reviews    application.ReviewRepository
	Summaries  application.SummaryRepository
	Queue      application.JobQueue

	Artifacts  application.ArtifactStore
	Fetcher    application.Fetcher
	Normalize  application.Normalizer
	Collectors application.CollectorRegistry

	Clock  application.Clock
	IDs    application.IDGenerator
	Events application.EventPublisher

	closers []func(context.Context) error
}

// Build assembles the container from configuration.
//
// It deliberately fails fast and loudly: a container that starts with a broken
// collector registry or an unreachable database would only surface the problem later,
// inside a job, where it looks like a source failure rather than a deployment failure.
func Build(ctx context.Context, cfg Config) (*Container, error) {
	logger := NewLogger(cfg)

	ids := NewIDGenerator()
	clock := SystemClock{}

	db, err := postgres.Open(ctx, postgres.Config{
		URL:             cfg.DatabaseURL,
		MaxConns:        cfg.DatabaseMaxConn,
		ApplicationName: cfg.Service,
		IDs:             ids,
	})
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}

	blobs, err := artifact.NewStore(cfg.ArtifactDir)
	if err != nil {
		return nil, fmt.Errorf("open artifact blob store at %s: %w", cfg.ArtifactDir, err)
	}

	guard := fetch.NewGuard(fetch.WithGuardLogger(logger))
	fetcher := fetch.New(guard)

	collectorRegistry, err := collectors.NewRegistryFromDir(
		os.DirFS("."), cfg.CollectorDir, collectors.WithLogger(logger))
	if err != nil {
		return nil, fmt.Errorf("load collector configurations from %s: %w", cfg.CollectorDir, err)
	}

	c := &Container{
		Config:     cfg,
		Logger:     logger,
		DB:         db,
		Migrator:   postgres.NewMigrator(db),
		Vendors:    postgres.NewVendorRepo(db),
		Categories: postgres.NewCategoryRepo(db),
		Products:   postgres.NewProductRepo(db),
		Sources:    postgres.NewSourceRepo(db),
		Candidates: postgres.NewCandidateRepo(db),
		Releases:   postgres.NewReleaseRepo(db),
		Evidence:   postgres.NewEvidenceRepo(db),
		Reviews:    postgres.NewReviewRepo(db),
		Summaries:  postgres.NewSummaryRepo(db),
		Queue:      postgres.NewQueue(db),
		Artifacts:  postgres.NewArtifactStore(db, blobs),
		Fetcher:    fetcher,
		Normalize:  normalize.New(),
		Collectors: collectorRegistry,
		Clock:      clock,
		IDs:        ids,
		Events:     NewEventPublisher(logger),
	}
	c.closers = append(c.closers, func(context.Context) error { db.Close(); return nil })
	return c, nil
}

// Close releases everything the container opened, in reverse order.
func (c *Container) Close(ctx context.Context) error {
	var firstErr error
	for i := len(c.closers) - 1; i >= 0; i-- {
		if err := c.closers[i](ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// AddCloser registers a shutdown hook, used by binaries that add their own resources
// such as a telemetry pipeline or an HTTP server.
func (c *Container) AddCloser(f func(context.Context) error) {
	c.closers = append(c.closers, f)
}

// IngestDeps assembles the dependency struct the ingestion use cases take.
func (c *Container) IngestDeps() application.IngestDeps {
	return application.IngestDeps{
		Sources:             c.Sources,
		Products:            c.Products,
		Candidates:          c.Candidates,
		Releases:            c.Releases,
		Evidence:            c.Evidence,
		Reviews:             c.Reviews,
		Artifacts:           c.Artifacts,
		Registry:            c.Collectors,
		Queue:               c.Queue,
		Events:              c.Events,
		UoW:                 c.DB,
		Clock:               c.Clock,
		IDs:                 c.IDs,
		ConfidenceThreshold: c.Config.ConfidenceThreshold,
		FutureDateTolerance: c.Config.FutureDateTolerance,
	}
}

// CheckSourceDeps assembles the dependency struct the watcher takes.
func (c *Container) CheckSourceDeps() application.CheckSourceDeps {
	return application.CheckSourceDeps{
		Sources:   c.Sources,
		Fetcher:   c.Fetcher,
		Normalize: c.Normalize,
		Artifacts: c.Artifacts,
		Queue:     c.Queue,
		Events:    c.Events,
		Clock:     c.Clock,
		IDs:       c.IDs,
		Policy:    domain.DefaultSchedulingPolicy(),
		UserAgent: c.Config.UserAgent,
		MaxBytes:  c.Config.FetchMaxBytes,
		Timeout:   c.Config.FetchTimeout,
	}
}

// QueryDeps assembles the dependency struct the read-side use cases take.
func (c *Container) QueryDeps() application.QueryDeps {
	return application.QueryDeps{
		Summaries: c.Summaries,
		Vendors:   c.Vendors,
		Products:  c.Products,
		Releases:  c.Releases,
		Events:    c.Events,
		Clock:     c.Clock,
	}
}

// RegistryLoader returns a loader for the Git-managed registry directory.
func (c *Container) RegistryLoader() application.RegistryLoader {
	return registry.New(os.DirFS("."), c.Config.RegistryDir)
}

// NewLogger builds the structured logger.
//
// Logs are JSON by default because they are read by Loki far more often than by a
// human tailing a terminal. The redaction of secrets happens in the OpenTelemetry
// Collector rather than here, so that a log line escaping through another path is
// still cleaned, but call sites are expected not to put a credential in a log in the
// first place.
func NewLogger(cfg Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if cfg.LogFormat == "text" {
		h = slog.NewTextHandler(os.Stderr, opts)
	} else {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(h).With(
		slog.String("service", cfg.Service),
		slog.String("version", cfg.Version),
		slog.String("environment", cfg.Environment),
	)
}

// EventPublisher writes domain and analytics events to the log and, in a later phase,
// to the analytics tables and EventBridge.
//
// The MVP implementation is deliberately minimal but not a no-op: the events are the
// product's own instrumentation, and losing them silently would hide exactly the
// signals (a publication rate falling to zero, a search returning nothing) that no
// error surfaces.
type EventPublisher struct {
	logger *slog.Logger
}

// NewEventPublisher builds the publisher.
func NewEventPublisher(l *slog.Logger) *EventPublisher {
	return &EventPublisher{logger: l.With(slog.String("stream", "events"))}
}

// Publish records events.
func (p *EventPublisher) Publish(ctx context.Context, events ...domain.Event) error {
	for _, e := range events {
		attrs := []any{
			slog.String("event", string(e.Name)),
			slog.String("subject_type", e.SubjectType),
			slog.String("subject_id", e.SubjectID),
			slog.Time("occurred_at", e.OccurredAt),
		}
		if e.VendorSlug != "" {
			attrs = append(attrs, slog.String("vendor", e.VendorSlug))
		}
		if e.ProductSlug != "" {
			attrs = append(attrs, slog.String("product", e.ProductSlug))
		}
		for k, v := range e.Attributes {
			attrs = append(attrs, slog.String("attr."+k, v))
		}
		p.logger.LogAttrs(ctx, slog.LevelInfo, "business event", toAttrs(attrs)...)
	}
	return nil
}

func toAttrs(vals []any) []slog.Attr {
	out := make([]slog.Attr, 0, len(vals))
	for _, v := range vals {
		if a, ok := v.(slog.Attr); ok {
			out = append(out, a)
		}
	}
	return out
}

// ShutdownTimeout bounds how long a graceful shutdown may take before the process
// stops waiting for in-flight work.
const ShutdownTimeout = 25 * time.Second

// RegistryLoaderFor returns a registry loader for a directory without building the
// whole container. It exists so `firmscout registry validate` can check a contributor's
// file without a database.
func RegistryLoaderFor(dir string) application.RegistryLoader {
	if dir == "" {
		dir = "dataset"
	}
	return registry.New(os.DirFS("."), dir)
}
