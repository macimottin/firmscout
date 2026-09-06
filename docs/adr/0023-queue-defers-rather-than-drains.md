# ADR-0023: A concurrent dequeue defers work; it does not drain the queue

- **Status:** Accepted
- **Date:** 2026-09-05
- **Deciders:** fix-pass integration
- **Requires qualified legal review:** no
- **Related:** ADR-0015, ADR-0003

## Context

ADR-0015 chose `SELECT ... FOR UPDATE SKIP LOCKED` as the dequeue mechanism for the
PostgreSQL `JobQueue` adapter. It settled the mechanism and never stated what the mechanism
promises a caller, and that gap was not harmless: the guarantee nobody wrote down was
assumed, and the assumption was wrong.

`TestConcurrentDequeuesDoNotOverlap` encoded the assumption. It released four workers at
once against twenty available jobs and required that they lease all twenty between them,
under the heading "two workers pulling at the same instant must partition the queue". It
failed roughly one run in eighty — often enough to be seen, rarely enough to be dismissed.
It had already been reported twice as interference from a concurrently running agent, and
attributing it to the environment was cheaper each time than measuring it.

Measurement inside the race settled it. A worker that leased nothing could **see eighteen
pending rows and lock none of them** (`visible=18`, `lockableRetry=0`, `allRows=20`). That
is `SKIP LOCKED` behaving exactly as documented: it passes over a row another transaction
holds and it never comes back for it. A worker that takes a large batch therefore leaves the
others scanning a queue in which everything is locked, they correctly return empty, and any
row that every worker happened to skip stays `pending` with `attempts` untouched.

So the queue was right and the test was wrong. Three separate hypotheses about the cause —
an `EvalPlanQual` recheck dropping rows, the CTE being re-evaluated, rows leased but not
returned — were each proposed and then **disproved by measurement** before the real
explanation was confirmed. That sequence is the reason this ADR exists rather than a code
comment: the failure is counter-intuitive enough that the next person to meet it will reach
for the same wrong answers.

## Decision

**The queue guarantees that no job is delivered to two workers at once and that no job is
lost. It does not guarantee that a set of concurrent dequeues drains the available work.**
A job skipped because it was momentarily locked is deferred to the next dequeue, not
dropped, and a worker returning zero jobs is not evidence that the queue is empty.

Three things follow, and all three are implemented:

1. The test asserts the real invariant: every job is either leased by exactly one worker or
   still `pending`, and `leased + pending = total`. That invariant held in **every** measured
   run, including all the ones where the workers did not drain the queue.
2. The dequeue statement is a materialised CTE joined into the `UPDATE`, not
   `UPDATE ... WHERE id IN (SELECT ... FOR UPDATE SKIP LOCKED)`. The `IN` form plans as a
   Hash Semi Join whose enclosing `UPDATE` re-locks the rows through a scan of its own that
   carries **no** `SKIP LOCKED`, so a worker can block behind another instead of stepping
   around it. Both forms were measured leasing the same rows, so this is a contention fix and
   not the cause of the flake — recorded explicitly because an early draft of the code
   comment claimed it *was* the cause, and that claim was wrong.
3. Callers may not treat an empty dequeue as "no work". The scheduler already polls, so
   nothing needed changing; this is written down so that a future caller does not invent a
   dependency on drainage.

## Consequences

### Positive

- The intermittent failure is gone because the assertion is now true, rather than because a
  retry or a sleep was added to make a wrong assertion pass.
- The property that actually matters — at-least-once delivery with no duplicates — is stated
  and tested, where before it was neither.
- Removing the outer re-lock removes a real hazard: under the `IN` form one dequeuer could
  wait on another, which is the precise behaviour `SKIP LOCKED` was chosen to avoid, and a
  plausible contributor to the lock contention seen in this package.

### Negative

- The test is weaker than it looked. It no longer proves that four workers drain twenty jobs,
  because that was never true; a genuine throughput regression that leaves work pending for
  longer would not fail it. The remaining guard is the accounting invariant, which catches
  loss and duplication but not slowness.
- Work can sit pending briefly under contention even though workers were asking for it. At
  the MVP's cadence this is invisible, but it is a real latency floor that scales with how
  hard workers collide, and nothing currently measures it.
- The CTE form is more SQL than the `IN` form and reads as gratuitous to anyone who does not
  know why. It is load-bearing only against a hazard that does not show up in a passing test,
  which is exactly the kind of thing a later simplification removes.

### Neutral

- Nothing above the adapter boundary changes. The `JobQueue` port contract in ADR-0015 is
  unchanged; this records what that contract has always actually meant.
- The same reasoning will apply to the planned SQS adapter, whose visibility-timeout model
  makes the same promise and equally does not guarantee drainage.

## Alternatives considered

### Make the queue drain in one pass: have each worker rescan until it comes up genuinely empty

Rejected. It buys a property no caller needs — the scheduler dequeues again on its next tick —
at the price of lock contention that rises with worker count, which is the cost `SKIP LOCKED`
was chosen to avoid in the first place. It would also convert a benign deferral into a real
throughput problem under exactly the load where throughput matters.

### Serialise dequeues with a plain `FOR UPDATE`, so workers queue behind each other

Rejected. This would make drainage true by making the queue single-threaded at the point it is
meant to scale, converting ADR-0015's competing-consumers design into a lock convoy.

### Leave the test as it was and retry it on failure

Rejected, and worth naming because it is the cheap answer. A retry would have hidden a real
and correct behaviour behind a flake-suppression mechanism, and left the next reader believing
a guarantee the database does not offer. The test was wrong; suppressing it would have
preserved the wrongness and thrown away the evidence.

### Delete the assertion without replacing it

Rejected. The accounting invariant is the part worth keeping, and it is stronger than what was
there before in the way that matters: it fails if a job is ever leased twice or lost, which is
the actual correctness property.

## Revisit when

- A worker is ever added that treats an empty dequeue as a signal to stop or to sleep longer,
  at which point the deferral described here becomes visible as latency.
- The lock contention between dequeuers is measured rather than reasoned about, which would
  let the CTE decision in point 2 be confirmed or reverted on evidence.
- The SQS adapter lands, and the equivalent guarantee under a visibility timeout should be
  stated in the same terms rather than rediscovered.
