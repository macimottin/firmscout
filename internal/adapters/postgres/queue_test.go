package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

func checkJob(id, sourceID string) application.Job {
	return application.Job{
		ID:             id,
		Kind:           application.JobCheckSource,
		IdempotencyKey: "check:" + sourceID,
		Payload:        map[string]string{"source_id": sourceID},
		TraceContext: map[string]string{
			"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			"tracestate":  "firmscout=watcher",
		},
		MaxAttempts: 3,
	}
}

func TestQueueEnqueueIsIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	q := NewQueue(db)

	if err := q.Enqueue(ctx, checkJob("job_1", "src_1")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// A different job id, the same idempotency key: the second enqueue is a no-op.
	if err := q.Enqueue(ctx, checkJob("job_2", "src_1")); err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}

	var n int
	if err := pool(db).QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if n != 1 {
		t.Errorf("job rows = %d, want 1", n)
	}
}

func TestQueueDequeueCompleteAndTraceContext(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	q := NewQueue(db)

	job := checkJob("job_1", "src_1")
	if err := q.Enqueue(ctx, job); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	leased, err := q.Dequeue(ctx, []string{application.JobCheckSource}, 10, time.Minute, "worker-a")
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if len(leased) != 1 {
		t.Fatalf("leased %d jobs, want 1", len(leased))
	}
	got := leased[0]
	if got.ID != "job_1" || got.Kind != application.JobCheckSource {
		t.Errorf("leased job = %+v", got)
	}
	if got.Payload["source_id"] != "src_1" {
		t.Errorf("payload = %v, want source_id=src_1", got.Payload)
	}
	// The trace context is the point: a distributed trace has to survive the queue
	// hop, which is where it is normally lost.
	if got.TraceContext["traceparent"] != job.TraceContext["traceparent"] ||
		got.TraceContext["tracestate"] != job.TraceContext["tracestate"] {
		t.Errorf("trace context = %v, want %v", got.TraceContext, job.TraceContext)
	}
	if got.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1: the lease consumes an attempt", got.Attempts)
	}
	if got.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want 3", got.MaxAttempts)
	}

	// A leased job is not handed out again while its lease holds.
	again, err := q.Dequeue(ctx, []string{application.JobCheckSource}, 10, time.Minute, "worker-b")
	if err != nil {
		t.Fatalf("second Dequeue: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("a leased job was handed out again: %+v", again)
	}

	// A different kind is not leased.
	if other, err := q.Dequeue(ctx, []string{application.JobPublishRelease}, 10, time.Minute, "worker-c"); err != nil {
		t.Fatalf("Dequeue other kind: %v", err)
	} else if len(other) != 0 {
		t.Errorf("Dequeue returned a job of the wrong kind: %+v", other)
	}

	if err := q.Complete(ctx, got.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var status string
	var lockedBy *string
	if err := pool(db).QueryRow(ctx,
		`SELECT status, locked_by FROM jobs WHERE id = $1`, got.ID).Scan(&status, &lockedBy); err != nil {
		t.Fatalf("read job: %v", err)
	}
	if status != "succeeded" {
		t.Errorf("status = %q, want succeeded", status)
	}
	if lockedBy != nil {
		t.Errorf("locked_by = %q after Complete, want NULL", *lockedBy)
	}

	if err := q.Complete(ctx, got.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("completing twice = %v, want domain.ErrNotFound", err)
	}
}

func TestQueueDequeueRespectsRunAfterAndPriority(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	q := NewQueue(db)

	future := checkJob("job_future", "src_future")
	future.RunAfter = time.Now().Add(time.Hour)
	if err := q.Enqueue(ctx, future); err != nil {
		t.Fatalf("Enqueue future: %v", err)
	}
	ready := checkJob("job_ready", "src_ready")
	if err := q.Enqueue(ctx, ready); err != nil {
		t.Fatalf("Enqueue ready: %v", err)
	}

	leased, err := q.Dequeue(ctx, nil, 10, time.Minute, "worker-a")
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if len(leased) != 1 || leased[0].ID != "job_ready" {
		t.Errorf("leased %+v, want only job_ready: a scheduled job is not due yet", leased)
	}
}

func TestQueueReclaimsExpiredLeases(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	q := NewQueue(db)

	if err := q.Enqueue(ctx, checkJob("job_1", "src_1")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// A worker leases the job and then dies. The lease is written in the past to
	// simulate the time having elapsed, because a test that actually waited for a
	// lease to expire would either be slow or be flaky.
	leased, err := q.Dequeue(ctx, nil, 1, time.Hour, "worker-that-dies")
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if len(leased) != 1 {
		t.Fatalf("leased %d jobs, want 1", len(leased))
	}
	if _, err := pool(db).Exec(ctx,
		`UPDATE jobs SET locked_until = now() - interval '1 second' WHERE id = $1`,
		leased[0].ID); err != nil {
		t.Fatalf("expire the lease: %v", err)
	}

	reclaimed, err := q.Dequeue(ctx, nil, 1, time.Minute, "worker-that-lives")
	if err != nil {
		t.Fatalf("reclaiming Dequeue: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0].ID != leased[0].ID {
		t.Fatalf("expired lease was not reclaimed: %+v", reclaimed)
	}
	if reclaimed[0].Attempts != 2 {
		t.Errorf("Attempts = %d, want 2: a crashed worker still consumes an attempt", reclaimed[0].Attempts)
	}
}

func TestQueueFailBacksOffThenDeadLetters(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	q := NewQueue(db)
	q.BackoffBase = 10 * time.Second
	q.BackoffCap = time.Hour

	job := checkJob("job_1", "src_1")
	job.MaxAttempts = 3
	if err := q.Enqueue(ctx, job); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	cause := errors.New("vendor returned 503")

	for attempt := 1; attempt <= 2; attempt++ {
		leased, err := q.Dequeue(ctx, nil, 1, time.Minute, "worker-a")
		if err != nil {
			t.Fatalf("Dequeue attempt %d: %v", attempt, err)
		}
		if len(leased) != 1 {
			t.Fatalf("attempt %d leased %d jobs, want 1", attempt, len(leased))
		}
		if err := q.Fail(ctx, leased[0].ID, cause, 0); err != nil {
			t.Fatalf("Fail attempt %d: %v", attempt, err)
		}

		var (
			status   string
			runAfter time.Time
			lastErr  string
			dead     *time.Time
		)
		if err := pool(db).QueryRow(ctx,
			`SELECT status, run_after, last_error, dead_lettered_at FROM jobs WHERE id = $1`,
			job.ID).Scan(&status, &runAfter, &lastErr, &dead); err != nil {
			t.Fatalf("read job: %v", err)
		}
		if status != "pending" {
			t.Fatalf("attempt %d status = %q, want pending", attempt, status)
		}
		if dead != nil {
			t.Fatalf("attempt %d dead-lettered too early", attempt)
		}
		if lastErr != cause.Error() {
			t.Errorf("last_error = %q, want %q", lastErr, cause.Error())
		}
		if !runAfter.After(time.Now()) {
			t.Errorf("attempt %d run_after = %v, want a future time", attempt, runAfter)
		}

		// Make the job due again so the next attempt can lease it.
		if _, err := pool(db).Exec(ctx,
			`UPDATE jobs SET run_after = now() - interval '1 second' WHERE id = $1`, job.ID); err != nil {
			t.Fatalf("make job due: %v", err)
		}
	}

	// Third lease exhausts max_attempts, so the next failure dead-letters.
	leased, err := q.Dequeue(ctx, nil, 1, time.Minute, "worker-a")
	if err != nil {
		t.Fatalf("third Dequeue: %v", err)
	}
	if len(leased) != 1 || leased[0].Attempts != 3 {
		t.Fatalf("third lease = %+v, want attempts 3", leased)
	}
	if err := q.Fail(ctx, leased[0].ID, cause, 0); err != nil {
		t.Fatalf("third Fail: %v", err)
	}

	var (
		status string
		dead   *time.Time
	)
	if err := pool(db).QueryRow(ctx,
		`SELECT status, dead_lettered_at FROM jobs WHERE id = $1`, job.ID).Scan(&status, &dead); err != nil {
		t.Fatalf("read job: %v", err)
	}
	if status != "dead" {
		t.Errorf("status = %q, want dead once attempts reach max_attempts", status)
	}
	if dead == nil {
		t.Error("dead_lettered_at was not set")
	}

	// A dead job is never leased again.
	if again, err := q.Dequeue(ctx, nil, 10, time.Minute, "worker-b"); err != nil {
		t.Fatalf("Dequeue after dead-letter: %v", err)
	} else if len(again) != 0 {
		t.Errorf("a dead job was leased again: %+v", again)
	}
	if err := q.Fail(ctx, job.ID, cause, 0); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("failing a dead job = %v, want domain.ErrNotFound", err)
	}
}

func TestQueueFailHonoursRetryAfter(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	q := NewQueue(db)
	q.BackoffBase = time.Second

	if err := q.Enqueue(ctx, checkJob("job_1", "src_1")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	leased, err := q.Dequeue(ctx, nil, 1, time.Minute, "worker-a")
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}

	// The vendor asked for an hour. That must beat a one-second backoff base:
	// FirmScout does not retry sooner than a site asked it to.
	if err := q.Fail(ctx, leased[0].ID, errors.New("429"), time.Hour); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	var runAfter time.Time
	if err := pool(db).QueryRow(ctx,
		`SELECT run_after FROM jobs WHERE id = $1`, "job_1").Scan(&runAfter); err != nil {
		t.Fatalf("read job: %v", err)
	}
	if d := time.Until(runAfter); d < 55*time.Minute {
		t.Errorf("run_after is %v away, want roughly an hour", d)
	}
}

// TestConcurrentDequeuesDoNotOverlap is the property SKIP LOCKED exists for: two
// workers pulling at the same instant must partition the queue between them, never
// duplicate it.
func TestConcurrentDequeuesDoNotOverlap(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	q := NewQueue(db)

	const jobCount = 20
	for i := range jobCount {
		j := checkJob("", "src_"+string(rune('a'+i)))
		if err := q.Enqueue(ctx, j); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	const workers = 4
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = make([][]application.Job, workers)
		errs    = make([]error, workers)
		start   = make(chan struct{})
	)
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start // release all workers at once, so the leases genuinely race
			leased, err := q.Dequeue(ctx, nil, jobCount, time.Minute, "worker")
			mu.Lock()
			results[w] = leased
			errs[w] = err
			mu.Unlock()
		}(w)
	}
	close(start)
	wg.Wait()

	seen := map[string]int{}
	total := 0
	for w := range workers {
		if errs[w] != nil {
			t.Fatalf("worker %d Dequeue: %v", w, errs[w])
		}
		for _, j := range results[w] {
			seen[j.ID]++
			total++
		}
	}
	if total != jobCount {
		t.Errorf("workers leased %d jobs in total, want %d", total, jobCount)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("job %s was leased by %d workers; SKIP LOCKED did not partition the queue", id, n)
		}
	}

	// Every job is now running with a live lease, so nothing is left to lease.
	remaining, err := q.Dequeue(ctx, nil, jobCount, time.Minute, "worker-late")
	if err != nil {
		t.Fatalf("late Dequeue: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("late worker leased %d jobs, want 0", len(remaining))
	}
}

func TestQueueEnqueueJoinsAmbientTransaction(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	q := NewQueue(db)

	// The reason the queue lives in PostgreSQL: a job enqueued alongside the row it
	// refers to is rolled back with it, so a job can never point at a row that does
	// not exist.
	sentinel := errors.New("boom")
	err := db.Within(ctx, func(ctx context.Context) error {
		if err := q.Enqueue(ctx, checkJob("job_1", "src_1")); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Within = %v, want the sentinel", err)
	}

	var n int
	if err := pool(db).QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if n != 0 {
		t.Errorf("job rows = %d after a rolled-back transaction, want 0", n)
	}
}
