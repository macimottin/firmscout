package postgres

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/macimottin/firmscout/internal/application"
)

// Queue is the PostgreSQL implementation of application.JobQueue.
//
// It is a database-backed queue rather than a broker because the MVP already needs a
// transactional database, and a job enqueued in the same transaction as the row it
// refers to cannot be lost or leaked -- something no separate broker can offer without
// an outbox. The leasing mechanism is SELECT ... FOR UPDATE SKIP LOCKED, which is what
// lets N workers pull from one table without any of them blocking on the others. See
// ADR-0015; an SQS adapter satisfying the same port is the intended AWS replacement.
//
// The delivery guarantee is at-least-once. A worker that dies after doing its work but
// before calling Complete will have its job re-leased when the lease expires, so job
// handlers must be idempotent -- which is why every write path this queue drives has an
// idempotency or dedupe key of its own.
type Queue struct {
	db *DB

	// BackoffBase is the first retry delay; each subsequent attempt doubles it.
	BackoffBase time.Duration
	// BackoffCap bounds the doubling, so a job with many attempts does not schedule
	// itself past the end of the retention window.
	BackoffCap time.Duration
	// JitterFraction spreads retries either side of the computed delay. Without it, a
	// vendor outage that fails two hundred jobs at once retries all two hundred at
	// the same instant, forever.
	JitterFraction float64
	// Priority is the priority assigned to enqueued jobs. application.Job carries no
	// priority field, so it is a queue-wide setting; a caller that needs a different
	// band constructs a second Queue.
	Priority int
	// MaxAttempts applies to jobs enqueued with none of their own.
	MaxAttempts int
}

var _ application.JobQueue = (*Queue)(nil)

// Queue defaults. They are deliberate starting points for tuning, not measurements.
const (
	defaultBackoffBase    = 30 * time.Second
	defaultBackoffCap     = time.Hour
	defaultJitterFraction = 0.2
	defaultJobPriority    = 100
	defaultMaxAttempts    = 5
)

// NewQueue returns a job queue bound to db, with the default backoff and priority
// settings. The exported fields may be adjusted before use.
func NewQueue(db *DB) *Queue {
	return &Queue{
		db:             db,
		BackoffBase:    defaultBackoffBase,
		BackoffCap:     defaultBackoffCap,
		JitterFraction: defaultJitterFraction,
		Priority:       defaultJobPriority,
		MaxAttempts:    defaultMaxAttempts,
	}
}

const jobReturning = `
    id, kind, idempotency_key, payload, trace_context, attempts, max_attempts, run_after`

// Enqueue adds a job, doing nothing if its idempotency key is already present.
//
// The no-op is the point. Scheduling is allowed to be enthusiastic -- a scheduler that
// runs twice, a retried HTTP request, a replayed event -- and the idempotency key is
// what turns "enqueue this check" into a statement about the desired state rather than
// an instruction that accumulates.
//
// It runs on the ambient transaction when there is one, so a job can be enqueued
// atomically with the row it is about.
func (q *Queue) Enqueue(ctx context.Context, j application.Job) error {
	id := j.ID
	if id == "" {
		id = q.db.newID("job")
	}
	key := j.IdempotencyKey
	if key == "" {
		key = id
	}
	payload := j.Payload
	if payload == nil {
		payload = map[string]string{}
	}
	traceContext := j.TraceContext
	if traceContext == nil {
		traceContext = map[string]string{}
	}
	maxAttempts := j.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = q.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = defaultMaxAttempts
		}
	}
	priority := q.Priority
	if priority == 0 {
		priority = defaultJobPriority
	}

	_, err := q.db.q(ctx).Exec(ctx,
		`INSERT INTO jobs (
            id, kind, idempotency_key, payload, trace_context, status, priority,
            attempts, max_attempts, run_after
         ) VALUES (
            $1, $2, $3, $4, $5, 'pending', $6, $7, $8, COALESCE($9::timestamptz, now())
         )
         ON CONFLICT (idempotency_key) DO NOTHING`,
		id, j.Kind, key, payload, traceContext, priority,
		j.Attempts, maxAttempts, nullTime(j.RunAfter))
	return wrap("queue.Enqueue", err)
}

// Dequeue leases up to limit jobs of the given kinds for the given lease duration and
// returns them.
//
// Three properties matter here:
//
//   - FOR UPDATE SKIP LOCKED means two workers dequeuing at the same instant never
//     receive the same job. The second worker skips the rows the first has locked
//     rather than waiting behind them, so throughput scales with worker count.
//   - Expired leases are reclaimed in the same statement: a job left 'running' by a
//     worker that crashed becomes eligible again once locked_until has passed. This
//     is what stops a machine failure from silently stranding work, and it is why the
//     selection predicate is not simply status = 'pending'.
//   - attempts is incremented at lease time, not at failure time. A worker that dies
//     without reporting anything still consumes an attempt, so a job that reliably
//     kills its worker eventually dead-letters instead of looping forever.
//
// An empty kinds slice leases jobs of any kind. The ordering is (priority, run_after),
// matching jobs_dequeue_idx.
func (q *Queue) Dequeue(ctx context.Context, kinds []string, limit int, lease time.Duration, workerID string) ([]application.Job, error) {
	limit = clampLimit(limit, 10, 1000)
	if lease <= 0 {
		lease = time.Minute
	}
	if kinds == nil {
		kinds = []string{}
	}

	rows, err := q.db.q(ctx).Query(ctx,
		`UPDATE jobs SET
            status       = 'running',
            locked_until = now() + make_interval(secs => $3::float8),
            locked_by    = $4,
            attempts     = attempts + 1,
            updated_at   = now()
          WHERE id IN (
              SELECT id FROM jobs
               WHERE (status = 'pending'
                      OR (status = 'running' AND locked_until IS NOT NULL AND locked_until <= now()))
                 AND run_after <= now()
                 AND (cardinality($1::text[]) = 0 OR kind = ANY($1::text[]))
               ORDER BY priority, run_after
               LIMIT $2
               FOR UPDATE SKIP LOCKED
          )
          RETURNING`+jobReturning,
		kinds, limit, lease.Seconds(), workerID)
	if err != nil {
		return nil, wrap("queue.Dequeue", err)
	}
	defer rows.Close()

	out := make([]application.Job, 0, limit)
	for rows.Next() {
		var j application.Job
		if err := rows.Scan(&j.ID, &j.Kind, &j.IdempotencyKey, &j.Payload,
			&j.TraceContext, &j.Attempts, &j.MaxAttempts, &j.RunAfter); err != nil {
			return nil, wrap("queue.Dequeue.scan", err)
		}
		out = append(out, j)
	}
	return out, wrap("queue.Dequeue.rows", rows.Err())
}

// Complete marks a leased job finished and releases its lease.
//
// It only affects a job that is currently running, so completing a job twice, or
// completing one that has already been dead-lettered, is reported as
// domain.ErrNotFound rather than silently resurrecting it.
//
// The port passes no worker id, so this cannot verify that the caller still holds the
// lease. A worker whose lease expired while it was working can therefore complete a
// job another worker has already re-leased. That is the at-least-once contract showing
// through; handlers are idempotent for exactly this reason.
func (q *Queue) Complete(ctx context.Context, jobID string) error {
	tag, err := q.db.q(ctx).Exec(ctx,
		`UPDATE jobs SET
            status       = 'succeeded',
            locked_until = NULL,
            locked_by    = NULL,
            last_error   = NULL,
            updated_at   = now()
          WHERE id = $1 AND status = 'running'`, jobID)
	if err != nil {
		return wrap("queue.Complete", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("queue.Complete")
	}
	return nil
}

// Fail records a failure and either reschedules the job with backoff or dead-letters
// it once its attempts are exhausted.
//
// The delay is chosen in this order:
//
//  1. retryAfter, when the caller supplies one. This is how a vendor's Retry-After
//     header reaches the queue, and it always wins: FirmScout does not retry sooner
//     than a site asked it to.
//  2. Otherwise exponential backoff from BackoffBase, doubling per attempt, bounded by
//     BackoffCap, multiplied by a jitter factor drawn per call. The jitter is what
//     stops a fleet of jobs that failed together from retrying together forever.
//
// Once attempts have reached max_attempts the job becomes 'dead' with a
// dead_lettered_at timestamp instead of being rescheduled. A dead job is retained
// rather than deleted: the failure text is the evidence for why a source stopped
// producing releases, and deleting it is deleting the diagnosis.
func (q *Queue) Fail(ctx context.Context, jobID string, cause error, retryAfter time.Duration) error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}

	base := q.BackoffBase
	if base <= 0 {
		base = defaultBackoffBase
	}
	ceiling := q.BackoffCap
	if ceiling <= 0 {
		ceiling = defaultBackoffCap
	}
	jitter := q.JitterFraction
	if jitter < 0 {
		jitter = 0
	}
	// One factor per call, so two jobs failing in the same instant get different
	// delays. Drawing it in Go rather than in SQL keeps the statement deterministic
	// and therefore explainable from the log line that records the factor.
	factor := 1 + jitter*(2*rand.Float64()-1)

	tag, err := q.db.q(ctx).Exec(ctx,
		`UPDATE jobs SET
            status = CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'pending' END,
            dead_lettered_at = CASE WHEN attempts >= max_attempts THEN now() ELSE NULL END,
            run_after = CASE
                WHEN attempts >= max_attempts THEN run_after
                WHEN $3::float8 > 0 THEN now() + make_interval(secs => $3::float8)
                ELSE now() + make_interval(secs =>
                    LEAST($4::float8 * power(2, GREATEST(attempts - 1, 0)), $5::float8) * $6::float8)
            END,
            last_error   = $2,
            locked_until = NULL,
            locked_by    = NULL,
            updated_at   = now()
          WHERE id = $1 AND status = 'running'`,
		jobID, nullString(message), retryAfter.Seconds(),
		base.Seconds(), ceiling.Seconds(), factor)
	if err != nil {
		return wrap("queue.Fail", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("queue.Fail")
	}
	return nil
}
