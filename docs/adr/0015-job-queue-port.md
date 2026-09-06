# ADR-0015: `JobQueue` port, PostgreSQL `SKIP LOCKED` adapter first, SQS later

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** founding team
- **Requires qualified legal review:** no
- **Related:** ADR-0002, ADR-0003, ADR-0010, ADR-0013, ADR-0023 (what the `SKIP LOCKED` dequeue chosen here does and does not guarantee)

## Context

The brief proposed SQS with dead-letter queues for all background work — scheduling source checks, running extraction, dispatching AI escalation. SQS is a sound choice in AWS at scale, but making it a hard dependency of the MVP has a real cost: the self-hosted edition either has to ship a second, different queue implementation anyway (since self-hosters do not have an AWS account by definition) or the entire project requires AWS to run at all, which contradicts the self-hosting commitment (§1). It also adds an operational surface — a new service to configure, monitor, and reason about failure modes for — at a job volume the MVP does not come close to justifying (assumption T5: a PostgreSQL-backed queue is expected sufficient below roughly 10 jobs/second sustained).

## Decision

A `JobQueue` **port** is defined in the application layer, per ADR-0002's rule that ports are owned by the layer that needs them. The **MVP and Compose adapter** is PostgreSQL-backed, implemented with `SELECT ... FOR UPDATE SKIP LOCKED` for concurrent-safe dequeue, an `idempotency_key UNIQUE` constraint to make enqueue safely retryable, an `attempts` counter with exponential backoff via `run_after`, a `locked_until` lease for in-flight jobs, and a logical dead-letter state (`dead_lettered_at`) rather than a separate DLQ construct — all backed by one `jobs` table (§11 item 8). An **SQS adapter** is planned for the AWS deployment (ADR-0010), implementing the exact same `JobQueue` port contract, so the domain and application layers never see a queue-technology-specific type regardless of which adapter is wired in for a given deployment.

Both adapters are held to the same contract and the same test suite — a use case that enqueues a `source.check.requested` job behaves identically whether it is running against the Compose PostgreSQL adapter or, later, the AWS SQS adapter, because nothing above the adapter boundary can tell the difference.

## Consequences

### Positive

- One fewer service in Docker Compose: self-hosters and contributors get a fully functional background job system with the same single PostgreSQL dependency the rest of the system already requires (ADR-0003) — no broker to install, configure, or monitor separately.
- Identical semantics for contributors and for AWS production: the `JobQueue` port's contract (idempotency, retry, backoff, dead-lettering) is the same regardless of adapter, so behaviour learned or debugged locally transfers directly to the production deployment's queue semantics.
- No migration cost is paid later for having chosen "wrong," because the domain never sees a queue type at all — swapping the PostgreSQL adapter for the SQS adapter when the AWS deployment lands is purely an `internal/platform` wiring change, invisible to `internal/domain` and `internal/application`.
- `SKIP LOCKED` job rows are ordinary, queryable database rows — an engineer debugging a stuck job can `SELECT * FROM jobs WHERE status = 'dead'` directly, which is a simpler debugging story than inspecting an opaque broker's internal delivery state.
- PostgreSQL `SKIP LOCKED` comfortably handles the MVP's expected load per assumption T5, so this is not a compromise made against evidence — it is a choice made because the evidence available at this scale supports it.

### Negative

- Job queue load and the rest of the application's OLTP read/write load share the same database connections and IOPS (this is the same trade-off named in ADR-0003) — a burst of enqueued jobs, or a poison message causing a retry storm, is a load event on the primary transactional database, not isolated to a dedicated broker's own infrastructure (§8.3 names retry storms as this component's main security/reliability risk).
- `SKIP LOCKED` polling-based dequeue (workers periodically querying for available jobs) has inherently higher latency between "job enqueued" and "job picked up" than a broker with push-based delivery or long-polling — acceptable for FirmScout's per-source background check cadence, but a real difference from SQS's delivery model.
- The PostgreSQL adapter does not natively provide SQS's fan-out to multiple independent consumer groups (multiple services each getting their own copy of every message) — if a future workload needs genuine multi-consumer fan-out rather than competing consumers, the PostgreSQL adapter's model does not express that without additional schema and logic.
- Maintaining two adapters (PostgreSQL now, SQS later) against one contract is more code than committing to a single queue technology, and the contract itself needs to be general enough to fit both without leaking either one's specific capabilities upward — a design constraint that has to be actively maintained as both adapters evolve.

### Neutral

- The `jobs` table is described in the blueprint as partition-ready by month but not partitioned in the MVP (§11 item 3) — this is a forward-looking schema note, not a current requirement, and partitioning would only be adopted once table growth justifies it.
- This ADR does not commit to exactly when the SQS adapter is built — only that it is the planned second adapter for the AWS deployment, consistent with ADR-0010's Lambda-first runtime using SQS to trigger worker functions.

## Alternatives considered

### SQS with dead-letter queues as the only queue implementation (the brief's original proposal)

Rejected as a hard MVP dependency, per §3.1. It is the right technology in AWS at scale, but requiring it from day one forces every self-hosted deployment to either depend on AWS or maintain a second, functionally equivalent queue implementation anyway — solving the self-hosting requirement twice for no benefit at the MVP's job volume. It remains the planned adapter for the AWS deployment specifically, just not as the sole implementation.

### RabbitMQ, Kafka, or a Redis-based queue as a self-hostable alternative to SQS

Rejected. Each of these is a new always-on service with its own operational learning curve, contradicting both the single-stateful-dependency principle (ADR-0003) and the low-idle-cost principle (ADR-0013) for a job volume PostgreSQL's `SKIP LOCKED` already handles comfortably per T5. None of them offers a clear enough advantage over PostgreSQL at this scale to justify the added operational surface.

### No formal job queue; cron-style polling loops calling use cases directly with no persisted job state

Rejected. This would lose idempotency guarantees, retry/backoff semantics, and dead-letter visibility — all of which the blueprint requires for source-check reliability (a failed or partially completed check needs to be retried safely, not silently dropped or duplicated) — and would make debugging stuck or failing background work much harder without a queryable `jobs` table as the source of truth.

## Revisit when

- Sustained enqueue rate rises above roughly 10 jobs per second, the threshold named in assumption T5 and §3.1 as where PostgreSQL `SKIP LOCKED` is expected to start showing strain.
- The `jobs` table exceeds roughly 5M rows, at which point partitioning (already anticipated in the schema design) or migration to the SQS adapter should be evaluated based on actual query performance.
- A workload needs genuine fan-out to multiple independent consumers of the same job, which the PostgreSQL competing-consumers model does not express without significant additional complexity.
- The AWS deployment (ADR-0010) is built out and the SQS adapter needs to move from planned to implemented as part of that work.
