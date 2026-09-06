# Architecture Decision Records

Every architecturally significant decision in FirmScout is recorded here, with the reasoning
that produced it and the concrete condition under which it should be revisited. An ADR is not
a description of what the code does. It is the argument for why the code does it that way,
written at the moment the alternatives were still live.

**When a new ADR is required:** a change to a layer boundary, a new stateful dependency, a
change to the licensing or data model, a new external service, or any decision a future
contributor would otherwise have to reverse-engineer from the code. Routine implementation
choices do not need one. See [GOVERNANCE.md](../../GOVERNANCE.md) for the process.

**Format:** every ADR carries Status, Date, Deciders, whether it requires qualified legal
review, and Related ADRs; then Context, Decision, Consequences (positive, negative, neutral),
Alternatives considered, and Revisit when. The Negative section is mandatory and non-empty —
a decision with no downside has not been examined.

## Index

| # | Title | Status | Summary | Legal review |
| --- | --- | --- | --- | --- |
| [0001](0001-modular-monolith.md) | Modular monolith, one Go module, three binaries | Accepted | One module builds the API, worker and CLI; bounded contexts are packages, not services | no |
| [0002](0002-clean-architecture.md) | Clean Architecture with mechanically enforced inward dependencies | Accepted | The domain outlives AWS, the database driver and the AI provider; the rule is enforced by a test, not a convention | no |
| [0003](0003-postgresql.md) | PostgreSQL 17 as the only stateful dependency | Accepted | Relational integrity, full-text search, JSONB and a job queue in one engine | no |
| [0004](0004-sqlc.md) | sqlc + pgx/v5 for data access, no ORM | Accepted | Compile-time-checked SQL; no ORM annotations reach domain entities | no |
| [0005](0005-deterministic-collectors.md) | Deterministic, config-driven collectors that cannot publish | Accepted | Collectors emit candidates and are structurally incapable of writing releases | no |
| [0006](0006-ai-as-escalation.md) | AI as an escalation mechanism, never the default path | Accepted | Five agents, schema-validated output, hard budgets, and a degraded mode that works without AI | no |
| [0007](0007-public-web-paid-api.md) | Genuinely free public site, paid API for automation | Accepted | Correctness, freshness and official links are never paywall levers | no |
| [0008](0008-scraping-resilience.md) | Layered scraping resilience without dataset poisoning | Accepted | Raise the cost of abuse; never serve false firmware or security data | no |
| [0009](0009-code-and-data-licensing.md) | Apache-2.0 code, CC BY 4.0 delayed snapshots, contractual data terms for the live feed | Accepted | The moat is the dataset and the operation, not source secrecy | **yes** |
| [0010](0010-aws-runtime.md) | Lambda-first AWS runtime, same binary everywhere | Accepted | Idle cost approaches the database bill; Fargate stays available without a rewrite | no |
| [0011](0011-open-source-observability.md) | OpenTelemetry with a self-hostable Prometheus/Loki/Tempo/Grafana stack | Accepted | Vendor-neutral telemetry, identical locally and in AWS | no |
| [0012](0012-product-analytics.md) | First-party, privacy-conscious product analytics | Accepted | Enough data to make product decisions, no trackers and no fingerprinting | no |
| [0013](0013-cost-minimization.md) | Cost minimisation as a functional requirement | Accepted | Unit economics decide whether the catalogue can grow at all | no |
| [0014](0014-stdlib-http-router.md) | Standard library `net/http` router, no framework | Accepted | Since Go 1.22 the framework's remaining value is smaller than its dependency cost | no |
| [0015](0015-job-queue-port.md) | `JobQueue` port, PostgreSQL `SKIP LOCKED` adapter first, SQS later | Accepted | Self-hostable without an AWS account; identical contract on both sides | no |
| [0016](0016-hybrid-dataset.md) | Hybrid dataset — registry in Git, observed facts in PostgreSQL | Accepted | Curation gets code review; observation gets a query planner | no |
| [0017](0017-version-strings-and-date-precision.md) | Opaque version strings, derived "latest," and explicit date precision | Accepted | Real vendor versions are not semver, and a day is never invented | no |
| [0018](0018-source-compliance-policy.md) | Source compliance is a first-class field, evaluated before collection | Accepted | Robots and terms status gate dispatch; Dell is registered but disabled | **yes** |
| [0019](0019-public-domain-shape.md) | The API is served from `api.firmscout.dev`, keeping the `/api/v1` path prefix | Accepted | A base URL consumers hardcode has to be able to move; apex cookies must not ride on API requests | no |
| [0020](0020-multi-source-conflict-detection.md) | Multi-source conflict is a recorded finding, never an auto-resolved guess | Accepted | Observations per source, an authority ladder that only ever demotes, and no same-tier tie-break | no |
| [0021](0021-asserted-reviewer-identity.md) | Review decisions record an asserted, unauthenticated actor | Accepted | There is no login; the audit trail says so in a column rather than pretending otherwise | no |
| [0022](0022-canonical-problem-types.md) | One canonical problem-type catalogue, taken from api.md | Accepted | Three URIs renamed and two added while the API is still unpublished | no |
| [0023](0023-queue-defers-rather-than-drains.md) | A concurrent dequeue defers work; it does not drain the queue | Accepted | `SKIP LOCKED` promises no duplication and no loss, never drainage; the test asserted drainage and was wrong | no |
| [0024](0024-device-first-catalogue.md) | A hardware model is a Product, and per-model firmware applicability is an explicit unknown | Accepted | A fleet searches by model number; a family claims a shared image file, never a shared version, and the gap is stated rather than guessed | no |

## Decisions requiring qualified legal review

Two ADRs record decisions that a lawyer must confirm before FirmScout launches a hosted
service or publishes a dataset snapshot. Neither is settled by this repository.

- **[ADR-0009](0009-code-and-data-licensing.md)** — the dataset licence, database rights by
  jurisdiction, attribution requirements, DCO versus CLA, and trademark registration.
- **[ADR-0018](0018-source-compliance-policy.md)** — collecting from hosts whose `robots.txt`
  disallows it, the use of undocumented vendor update APIs, acceptable evidence-excerpt
  length, and handling of vendor delisting requests.

The itemised questions are collected in [`docs/architecture/licensing.md`](../architecture/licensing.md).

## Superseded decisions

None yet. When an ADR is superseded, its Status becomes `Superseded by ADR-NNNN` and the
original text is left unchanged — an ADR is a record of what was decided and why, so editing
it after the fact destroys the only thing it is for.
