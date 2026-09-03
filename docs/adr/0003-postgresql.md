# ADR-0003: PostgreSQL 17 as the only stateful dependency

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** founding team
- **Requires qualified legal review:** no
- **Related:** ADR-0004, ADR-0015, ADR-0016

## Context

FirmScout needs relational integrity for its catalogue (vendors, products, aliases with foreign keys and uniqueness constraints), full-text and fuzzy search over product names and aliases, semi-structured storage for collector configuration and AI agent payloads, and a durable background job queue with at-least-once, exactly-processed-once-per-attempt semantics. The brief's default assumption of a specialised store per concern — a search engine, a message broker, possibly a document store — would satisfy each requirement individually but would mean every self-hosted deployment, and every contributor's laptop, needs to run four services instead of one before the vertical slice does anything.

Cost minimisation is a functional requirement (ADR-0013), and idle cost is dominated by always-on services. A search cluster or a message broker sitting mostly idle at the MVP's traffic level is exactly the always-on cost this project is trying to avoid. The relevant question is not "which store is best in isolation for each concern" but "which combination keeps the MVP self-hostable with one service and correct."

## Decision

PostgreSQL 17 is the **only stateful dependency** in the MVP. It serves four roles in one engine:

1. **Relational integrity** for the catalogue and fact tables — vendors, products, aliases, sources, releases, evidence, release-product mappings — with foreign keys, `CHECK` constraints (notably the date-precision constraints in ADR-0017), and uniqueness where the domain requires it.
2. **Full-text and fuzzy search** via `tsvector` for ranked text search and the `pg_trgm` extension for typo-tolerant and alias matching, over a precomputed `product_summaries` table refreshed on publication rather than joined at query time (§11 item 9, §3.10).
3. **Semi-structured storage** via `JSONB` for collector configuration snapshots, AI agent run payloads, and other data whose shape evolves faster than a migration cadence would comfortably support.
4. **A durable job queue**, implemented with `SELECT ... FOR UPDATE SKIP LOCKED`, idempotency keys, attempt counters, and a logical dead-letter state (ADR-0015) — no separate broker.

This is validated as sufficient rather than assumed: assumption T4 (full-text search latency at ~500k products) and T5 (queue throughput below ~10 jobs/s) are both explicitly named in the blueprint as things to measure, not permanent guarantees.

## Consequences

### Positive

- One service in `docker compose up` for the entire stateful layer; a contributor needs no search cluster, no broker, and no document store to run the full vertical slice locally.
- Transactional consistency across catalogue writes, fact writes, and queue writes in a single engine — `PublishRelease` can insert a release, its evidence, its product mappings, and refresh a summary in one `UnitOfWork` transaction, with no distributed-transaction or eventual-consistency reasoning required (§7.5).
- `pg_trgm` and `tsvector` are mature, well understood, and require no new operational skill beyond what running PostgreSQL already demands — no separate cluster to size, shard, or upgrade.
- `SKIP LOCKED` job queue semantics are simple enough to reason about and test without a broker's delivery-guarantee documentation; the same rows are visible to `SELECT` for debugging, unlike an opaque broker's internal state.

### Negative

- PostgreSQL full-text search is not a dedicated search engine: it has no built-in relevance-tuning ecosystem comparable to a purpose-built engine, no distributed sharding story, and `ts_rank`'s ranking is coarser than what a dedicated engine offers out of the box. This is an accepted limitation until the threshold below is crossed.
- A `SKIP LOCKED` queue on the primary transactional database means job queue load and catalogue/API read load compete for the same IOPS and connection pool. A poison message causing a retry storm is a database-level operational risk, not isolated to a separate broker (§8.3).
- Putting search, queue, and OLTP workloads on one engine removes the option to scale or tune any of them independently — vertical scaling of the single PostgreSQL instance is the only lever until a component is split out.
- This is a single point of failure for the entire system at MVP scale; there is no independent stateful component that could keep any subsystem running if PostgreSQL itself is down. This is accepted as reasonable for the MVP's expected uptime requirements and reconsidered if that changes.

### Neutral

- The PostgreSQL adapter is written to use PostgreSQL-specific features freely (`SKIP LOCKED`, `tsvector`, `JSONB`) rather than defensively targeting SQL-portable syntax. The port that wraps it exists for testability (in-memory fakes for use-case tests), not for a hypothetical future database swap — FirmScout will not switch database engines (§7.9).

## Alternatives considered

### OpenSearch (or a comparable dedicated search engine) for product search

Rejected for the MVP. It solves search well but adds an always-on cluster, a second data store to keep in sync with PostgreSQL (dual-write or CDC complexity), and an operational skill the small team does not need yet for a catalogue in the low hundreds of thousands of products. `tsvector` plus `pg_trgm` over a precomputed summary table is the accepted alternative (§3.10).

### A message broker (SQS, RabbitMQ, Kafka, or Redis-based queue) for background jobs

Rejected as a hard dependency for the MVP for the same reason discussed at length in ADR-0015: it is the right tool at scale but not before the measured need exists, and it complicates self-hosting for no benefit at MVP job volume.

### Redis as a cache or for rate limiting

Rejected for the MVP (§3.9). Redis solves distributed rate limiting cleanly but adds a service and a new failure mode to solve a problem the MVP does not yet have at its expected instance count; a WAF layer plus in-process token buckets plus durable PostgreSQL quota counters covers the requirement adequately until instance count or contractual SLAs demand more.

### A document store (e.g., MongoDB) for the semi-structured payloads (collector configs, AI run outputs)

Rejected. `JSONB` in PostgreSQL provides indexable semi-structured storage without adding a service, and the volume and query pattern of this data (looked up by a small number of keys, not queried with complex document filters) does not justify a dedicated document store.

## Revisit when

- p95 search latency exceeds roughly 200 ms at realistic catalogue size, or a genuine relevance-tuning need arises that `ts_rank` cannot express (§3.10).
- Sustained job enqueue rate exceeds roughly 10/s, or the `jobs` table exceeds roughly 5M rows, or a workload needs fan-out to multiple independent consumers (ADR-0015).
- Query latency, lock contention, or connection-pool exhaustion attributable to the combined OLTP + search + queue workload is measured in production and cannot be resolved by indexing, read replicas, or connection pooling alone.
- More than roughly 4 concurrent API instances are needed and exact per-second rate-limit enforcement becomes contractually required (§3.9), which is the threshold named for revisiting Redis specifically.
