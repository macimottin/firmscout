// Command worker runs FirmScout's background pipeline: the scheduler that decides
// which sources are due, and the job runner that checks, extracts, validates and
// publishes.
//
// Under Docker Compose this is a long-lived process with an internal scheduler loop.
// On AWS the same binary runs as a Lambda function triggered by SQS and EventBridge
// Scheduler; the scheduler loop is simply not used there. Neither arrangement changes
// a use case.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/macimottin/firmscout/internal/adapters/telemetry"
	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/platform"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "firmscout-worker: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := platform.Load("firmscout-worker")
	if err != nil {
		return err
	}
	cfg.Version = version

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
		slog.String("worker_id", cfg.WorkerID),
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

	status, err := c.Migrator.Status(ctx)
	if err != nil {
		return fmt.Errorf("read migration status: %w", err)
	}
	if len(status.Pending) > 0 {
		return fmt.Errorf("%d migration(s) pending; run 'firmscout migrate up' before starting the worker", len(status.Pending))
	}

	w := &worker{
		container: c,
		logger:    logger,
		metrics:   tp.Metrics(),
		cfg:       cfg,
		check:     application.NewCheckSource(c.CheckSourceDeps()),
		extract:   application.NewExtractCandidates(c.IngestDeps()),
		validate:  application.NewValidateCandidate(c.IngestDeps()),
		publish:   application.NewPublishRelease(c.IngestDeps()),
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); w.runScheduler(ctx) }()
	go func() { defer wg.Done(); w.runJobs(ctx) }()

	logger.Info("worker started",
		slog.Duration("scheduler_interval", cfg.SchedulerInterval),
		slog.Int("concurrency", cfg.WorkerConcurrency))
	<-ctx.Done()
	logger.Info("shutting down; waiting for in-flight jobs")
	wg.Wait()
	logger.Info("stopped")
	return nil
}

type worker struct {
	container *platform.Container
	logger    *slog.Logger
	metrics   application.Metrics
	cfg       platform.Config

	check    *application.CheckSource
	extract  *application.ExtractCandidates
	validate *application.ValidateCandidate
	publish  *application.PublishRelease
}

// runScheduler enqueues checks for sources that have become due.
//
// It enqueues rather than checking inline so that a slow vendor cannot delay the
// scheduling of every other source, and so that the same work is dispatchable by
// EventBridge on AWS without a second code path.
func (w *worker) runScheduler(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.SchedulerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		now := w.container.Clock.Now()
		due, err := w.container.Sources.ListDispatchable(ctx, now, 100)
		if err != nil {
			w.logger.Error("scheduler could not list due sources", slog.Any("error", err))
			continue
		}
		if len(due) == 0 {
			continue
		}

		enqueued := 0
		for _, s := range due {
			job := application.Job{
				ID:   w.container.IDs.NewID("job"),
				Kind: application.JobCheckSource,
				// The key includes the scheduled instant, so re-running the
				// scheduler within one interval does not enqueue a second check
				// for the same source.
				IdempotencyKey: fmt.Sprintf("check:%s:%d", s.ID, s.NextCheckAt.Unix()),
				Payload:        map[string]string{"source_id": s.ID},
				RunAfter:       now,
			}
			if err := w.container.Queue.Enqueue(ctx, job); err != nil {
				w.logger.Error("enqueue check failed",
					slog.String("source_id", s.ID), slog.Any("error", err))
				continue
			}
			enqueued++
		}
		w.logger.Info("scheduled source checks",
			slog.Int("due", len(due)), slog.Int("enqueued", enqueued))
	}
}

// runJobs leases and executes jobs until the context is cancelled.
func (w *worker) runJobs(ctx context.Context) {
	sem := make(chan struct{}, w.cfg.WorkerConcurrency)
	var wg sync.WaitGroup
	defer wg.Wait()

	kinds := []string{
		application.JobCheckSource,
		application.JobExtractCandidates,
		application.JobValidateCandidate,
		application.JobPublishRelease,
	}

	// backoff grows when the queue is empty so an idle worker does not poll a
	// database every hundred milliseconds forever.
	idle := 0
	for {
		if err := ctx.Err(); err != nil {
			return
		}

		jobs, err := w.container.Queue.Dequeue(ctx, kinds, w.cfg.WorkerConcurrency,
			w.cfg.JobLeaseDuration, w.cfg.WorkerID)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			w.logger.Error("dequeue failed", slog.Any("error", err))
			sleep(ctx, 5*time.Second)
			continue
		}

		if len(jobs) == 0 {
			idle++
			d := time.Duration(min(idle, 20)) * 500 * time.Millisecond
			sleep(ctx, d)
			continue
		}
		idle = 0

		for _, job := range jobs {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			wg.Add(1)
			go func(j application.Job) {
				defer wg.Done()
				defer func() { <-sem }()
				w.handle(ctx, j)
			}(job)
		}
	}
}

// handle executes one job and reports the outcome to the queue.
func (w *worker) handle(ctx context.Context, j application.Job) {
	started := time.Now()
	log := w.logger.With(slog.String("job_id", j.ID), slog.String("kind", j.Kind))

	err := w.dispatch(ctx, j)
	elapsed := time.Since(started)

	if err != nil {
		log.Error("job failed", slog.Any("error", err), slog.Duration("elapsed", elapsed))
		// Backoff is the queue adapter's decision; the worker only reports that
		// the attempt failed.
		if ferr := w.container.Queue.Fail(ctx, j.ID, err, 0); ferr != nil {
			log.Error("could not record job failure", slog.Any("error", ferr))
		}
		return
	}

	if err := w.container.Queue.Complete(ctx, j.ID); err != nil {
		log.Error("could not mark job complete", slog.Any("error", err))
		return
	}
	log.Info("job completed", slog.Duration("elapsed", elapsed))
}

func (w *worker) dispatch(ctx context.Context, j application.Job) error {
	switch j.Kind {
	case application.JobCheckSource:
		id := j.Payload["source_id"]
		if id == "" {
			return errors.New("check job carries no source_id")
		}
		res, err := w.check.Execute(ctx, id)
		if err != nil {
			return err
		}
		w.metrics.Counter(ctx, application.MetricCollectorChecksTotal, 1,
			application.A("outcome", string(res.Outcome)))
		return nil

	case application.JobExtractCandidates:
		sourceID, artifactID := j.Payload["source_id"], j.Payload["artifact_id"]
		if sourceID == "" || artifactID == "" {
			return errors.New("extraction job needs source_id and artifact_id")
		}
		res, err := w.extract.Execute(ctx, sourceID, artifactID)
		if err != nil {
			w.metrics.Counter(ctx, application.MetricCollectorExtractionFail, 1)
			return err
		}
		w.metrics.Counter(ctx, application.MetricCollectorDuplicates, int64(res.Duplicates))
		return nil

	case application.JobValidateCandidate:
		id := j.Payload["candidate_id"]
		if id == "" {
			return errors.New("validation job carries no candidate_id")
		}
		res, err := w.validate.Execute(ctx, id)
		if err != nil {
			w.metrics.Counter(ctx, application.MetricCollectorValidationFail, 1)
			return err
		}
		w.logger.Info("candidate validated",
			slog.String("candidate_id", id),
			slog.String("decision", string(res.Decision)),
			slog.String("reason", res.Reason))
		return nil

	case application.JobPublishRelease:
		id := j.Payload["candidate_id"]
		if id == "" {
			return errors.New("publication job carries no candidate_id")
		}
		res, err := w.publish.Execute(ctx, id)
		if err != nil {
			return err
		}
		if res.Published {
			w.metrics.Counter(ctx, application.MetricCollectorPublications, 1)
			w.logger.Info("release published",
				slog.String("release_id", res.ReleaseID),
				slog.String("version", res.Version))
		}
		return nil

	default:
		// An unknown job kind is a deployment mismatch, not a transient failure.
		// Retrying it would burn the attempt budget to no purpose.
		return fmt.Errorf("unknown job kind %q", j.Kind)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
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
