# FirmScout — Technical and Product Blueprint

> Status: **Draft v0.1** — written before implementation, revised after the first vertical slice.
> Audience: founding engineers, open-source contributors, and anyone evaluating FirmScout for self-hosting.
> Companion documents are linked inline. Diagrams live in [`docs/diagrams/`](../diagrams/). Decisions live in [`docs/adr/`](../adr/).

This document is the master blueprint. It answers the 48 deliverables in order. Where a topic needs more depth than a blueprint should carry, the section summarises the decision and links to the dedicated document.

---

## 1. Executive summary

FirmScout is an open catalogue and continuous monitor for the version metadata of physical and virtual infrastructure: firmware, BIOS, BMC images, device drivers, embedded operating systems, appliance software, and management-platform releases. The long-term ambition is "Dependabot for hardware", but the near-term product is narrower and more defensible: **a trustworthy, evidence-backed answer to "what is the current version of this device, and when did it ship?"**

The market gap is not that this information is secret. It is that the information is scattered across several thousand vendor portals, published in incompatible formats, frequently relocated, and almost never machine-readable in a uniform way. Every infrastructure team rebuilds a private, decaying spreadsheet of the same facts.

**The product is three things:**

1. A **public website** that is genuinely useful for free, indexable by search engines, and answers a human's question in one page.
2. An **open-source platform** that anybody can self-host, contribute collectors to, and audit end to end.
3. A **hosted service** that sells automation, freshness, history, and integration to organisations that want the data in their pipelines rather than in their browser.

**The technical differentiator is not the database.** Anyone can build a schema. The differentiator is a discovery and maintenance engine with a **cost per monitored source low enough that monitoring tens of thousands of sources is economically boring**. That engine is deterministic by default: conditional HTTP requests, normalised content hashing, and configuration-driven extractors. Large language models are an escalation path for discovery and repair, invoked when deterministic methods fail, under explicit financial budgets, and never on the default execution path.

**The core operating principles**, which every later section defends:

| Principle | Consequence |
| --- | --- |
| Deterministic by default, AI only by escalation | Cost per check stays in fractions of a cent; behaviour is reproducible and testable |
| Evidence first | Every published fact carries a source URL, retrieval timestamp, content hash, and extracted excerpt |
| Historical data is immutable | Releases are inserted, never overwritten; corrections are new rows with audit trails |
| No invented precision | A release dated "February 2026" is stored as month precision, never as the 1st of February |
| No assumed semantic versioning | Version strings are opaque tokens with a plausibility check, not comparable integers |
| Collectors cannot publish | Collectors emit candidates; a separate use case validates and publishes |
| Low idle cost | Nothing in the MVP requires always-on compute beyond a small database |

**Recommended shape for the first six months:** a Go modular monolith with three binaries (API, worker, CLI), PostgreSQL as the only stateful dependency, a Next.js public site, and an OpenTelemetry-based observability stack that runs identically on a laptop and in AWS. No Kubernetes, no Kafka, no Redis, no OpenSearch, and no message broker beyond a PostgreSQL-backed queue until measurements justify one.

**What is deliberately *not* in the MVP:** CVE correlation (designed, not built), AI agents (ports and schemas only), billing integration, SSO, and support for more than four pilot vendors.

---

## 2. Assumptions

These are stated so they can be challenged later. Assumptions marked **(unvalidated)** are guesses that should be tested with real users or real measurements before large investments follow from them.

### Product assumptions

| # | Assumption | Confidence | How to validate |
| --- | --- | --- | --- |
| P1 | Infrastructure teams currently track device firmware versions manually and consider it a chore | High | Interviews; the prevalence of internal spreadsheets |
| P2 | The most valuable single fact is "latest version + release date + official link", not full release notes | Medium **(unvalidated)** | Search analytics on the public site; which fields API consumers actually read |
| P3 | Buyers will pay for automation and history rather than for the raw current-version fact | Medium **(unvalidated)** | Conversion from free API key to paid tier |
| P4 | A free, SEO-indexed public site is the primary acquisition channel | Medium | Organic traffic share after six months |
| P5 | Community contributors will maintain collectors for vendors they personally operate | Low **(unvalidated)** | Number of externally authored collectors merged in the first year |
| P6 | Compliance and audit use cases (proving a fleet was patched) are where enterprise money is | Medium **(unvalidated)** | Enterprise discovery conversations |

### Technical assumptions

| # | Assumption | Confidence | How to validate |
| --- | --- | --- | --- |
| T1 | The majority of vendor sources expose a cheap change signal (ETag, Last-Modified, RSS, or a small stable endpoint) | Medium | Measured across the first 200 registered sources |
| T2 | Normalised section hashing keeps false-change rates low enough that extraction runs rarely | Medium | Ratio of `changed` outcomes to actual new releases |
| T3 | Configuration-driven extractors cover most HTML and JSON sources; code collectors are the minority | Medium | Ratio of config to code collectors after 50 vendors |
| T4 | PostgreSQL full-text search is sufficient for product search up to ~500k products | High | Query latency at p95 as the catalogue grows |
| T5 | A PostgreSQL-backed job queue is sufficient below roughly 10 jobs per second sustained | High | Queue depth and dequeue latency under load |
| T6 | Source layouts break often enough to need a repair path, but rarely enough that AI repair stays affordable | Low **(unvalidated)** | Breakage rate per source per quarter |

### Operating assumptions

| # | Assumption | Confidence |
| --- | --- | --- |
| O1 | Bulk scraping of the public site will be attempted and cannot be prevented, only made unattractive | High |
| O2 | Vendor terms and robots policies will block some otherwise attractive sources | High — already observed, see §34 |
| O3 | The founder-generated initial dataset must carry the product; community contributions are additive | High |
| O4 | Costs are dominated by egress, always-on compute, and AI calls, in that order, at low scale | Medium |

---

## 3. Challenged assumptions

The brief proposed a specific architecture. Most of it is sound. The following proposals are challenged, with the alternative and the reasoning. Each accepted challenge has a corresponding ADR.

### 3.1 "Use SQS" — challenged, deferred

**Proposal:** SQS with dead-letter queues for all background work.
**Challenge:** SQS is the right answer *in AWS at scale*, but making it a hard dependency of the MVP means the self-hosted edition either ships a second queue implementation anyway or requires an AWS account to run. It also adds an operational surface for zero benefit at the MVP's job volume.
**Alternative:** Define a `JobQueue` port in the application layer. Ship a PostgreSQL-backed adapter using `SELECT ... FOR UPDATE SKIP LOCKED` with idempotency keys, attempt counters, exponential backoff, and a logical dead-letter state. Ship an SQS adapter when the AWS deployment lands. Both satisfy the same contract and the same tests.
**Why this is better:** one fewer service in Docker Compose, identical semantics for contributors and for production, no migration cost later because the domain never sees a queue type. PostgreSQL `SKIP LOCKED` comfortably handles the MVP's expected load (T5).
**Threshold to revisit:** sustained enqueue rate above ~10/s, or queue tables exceeding ~5M rows, or a need for fan-out to multiple independent consumers.
→ [ADR-0015](../adr/0015-job-queue-port.md)

### 3.2 "Evaluate Chi, Gin, Fiber" — challenged, none of them

**Proposal:** pick an HTTP framework.
**Challenge:** since Go 1.22, `net/http.ServeMux` routes by method and path pattern with wildcards. The remaining value of Chi is middleware ergonomics, which is ~40 lines of code. Gin and Fiber additionally impose non-standard context and handler types that leak into adapters and complicate `otelhttp` instrumentation. Fiber does not use `net/http` at all, which forfeits the standard middleware ecosystem.
**Alternative:** standard library `net/http` with a small explicit middleware chain.
**Why this is better:** zero framework dependency in a project whose security posture depends on a small, auditable dependency tree; `otelhttp` works out of the box; handlers are testable with `httptest` without framework harnesses; contributors need no framework knowledge.
**Cost of being wrong:** low. Chi is signature-compatible with `net/http`; adopting it later is a mechanical change.
→ [ADR-0014](../adr/0014-stdlib-http-router.md)

### 3.3 "Dataset in Git or in PostgreSQL" — challenged, hybrid

**Proposal:** the brief asks which one.
**Challenge:** the question conflates two different kinds of data. **Curated intent** (which vendors exist, which products we track, which sources are official, how to parse them) benefits from code review, pull requests, blame history, and offline diffing. **Observed facts** (releases, checks, artifacts, candidates, usage) are high-volume, machine-generated, append-only, and query-heavy. Forcing both into one store is wrong in both directions: a Git-only dataset cannot answer "latest version" efficiently; a PostgreSQL-only dataset makes community contribution require database access.
**Alternative:** hybrid.
- **Git** (`dataset/`, `collectors/config/`): vendors, product families, products — software products and hardware models alike, a model being a product carrying the vendor's published product code — aliases, categories, the product-to-product `runs_os` relationships that say which operating system a device runs, source definitions, collector configurations. YAML, validated against JSON Schema in CI, synchronised into PostgreSQL by `firmscout registry sync`.
- **PostgreSQL**: everything observed — releases, evidence, source checks, artifacts, candidates, validations, review items, API usage, audit.
- **Exported snapshots**: periodic public data dumps under the dataset licence, generated from PostgreSQL.

**Why this is better:** a contributor adds a vendor with a pull request and no infrastructure; the platform answers queries in milliseconds; the registry is diffable and revertible; the observed data is never in a merge conflict.
**Failure mode to watch:** registry drift if someone edits the database directly. Mitigated by making `registry sync` the only writer of registry tables and marking those rows `managed_by = 'registry'`.
→ [ADR-0016](../adr/0016-hybrid-dataset.md)

### 3.4 "ECS Fargate or Lambda" — challenged, Lambda first

**Proposal:** either.
**Challenge:** the brief also demands low idle cost and scale-to-zero. Fargate contradicts both: the smallest useful always-on task costs real money every month whether or not anyone visits, and two tasks for availability doubles it. FirmScout's traffic profile at launch is bursty and low-volume — exactly Lambda's shape.
**Alternative:** run the *same* Go binary in Lambda behind the AWS Lambda Web Adapter, fronted by CloudFront and an API Gateway HTTP API. Workers run as Lambda functions triggered by SQS and EventBridge Scheduler. The web front end runs via OpenNext on Lambda + CloudFront.
**Why this is better:** idle cost approaches the database bill alone; no container orchestration to learn; the binary is unmodified, so `docker compose up` and Fargate remain available without a rewrite.
**Honest downsides:** cold starts (a Go binary's are small, tens of milliseconds), a 15-minute execution ceiling (irrelevant — jobs are per-source and short), and less predictable cost under sustained high traffic.
**Threshold to switch to Fargate:** sustained request rate where Fargate's monthly cost undercuts Lambda's, measured rather than guessed; or a workload needing long-lived connections.
→ [ADR-0010](../adr/0010-aws-runtime.md)

### 3.5 "Semantic versioning" — challenged, never assumed

**Proposal:** the brief already says not to assume semver, and gives examples like `3.003.0015.001` and `CollabOS 2.1.B (2.1.121)`. This is correct and worth reinforcing because it has deep design consequences.
**Consequence:** "latest" cannot be computed by sorting version strings. FirmScout derives *latest observed* from release date, first-observed timestamp, and channel — never from version ordering. Version tokenisation exists **only** to answer "is this transition plausible?" (a jump from `7.24.2` to `1.0` is suspicious and routes to review). It is never used to decide which release is newer.
→ [ADR-0017](../adr/0017-version-strings-and-date-precision.md)

### 3.6 "Dell as the clean-API pilot" — challenged on compliance grounds

**Measured on 2026-09-03:** `https://downloads.dell.com/catalog/Catalog.xml.gz` is a well-formed, ETag-bearing, 1.4 MB catalogue — technically ideal. But `https://downloads.dell.com/robots.txt` returns `Disallow: /` for all user agents, allowing only `/manuals` and `/topicspdf`.
**Challenge:** the brief requires respecting robots.txt. Automated collection of that catalogue would violate the project's own stated policy on day one, in public, in an open-source repository. That is a reputational and legal risk out of proportion to the value of one pilot.
**Alternative:** keep Dell in the registry with `robots_policy_status = 'disallowed'`, `terms_review_status = 'pending'`, and `enabled = false` — visible, documented, uncollected. Pursue Dell's official APIs or a written permission instead. For the "clean API" pilot profile, substitute **Ubiquiti** (`https://fw-update.ubnt.com/api/firmware-latest`), a JSON endpoint with channel, platform, product, timestamps, and checksums, and no robots restriction. Ubiquiti's terms still require review before launch.
**Why this matters beyond one vendor:** it establishes that compliance status is a first-class field in the source registry, evaluated before collection, not an afterthought.
→ [ADR-0018](../adr/0018-source-compliance-policy.md), [DATA_SOURCES.md](../../DATA_SOURCES.md)

### 3.7 "AGPL protects the business" — challenged, Apache-2.0

**Challenge:** AGPL's network copyleft is aimed at a specific threat: a competitor running your code as a service without contributing back. For FirmScout that threat is weak, because **the code is not the moat**. A competitor with the full source still lacks the dataset, the curated source registry, the operational history that tunes scheduling, and the reliability record. Meanwhile AGPL's costs are real: corporate legal departments block AGPL dependencies, which suppresses exactly the contributor population (infrastructure engineers at enterprises) FirmScout needs for collectors, and self-hosting adoption is a distribution channel worth more than the copyleft protection.
**Alternative:** Apache-2.0 for the source, with an explicit patent grant; a trademark policy protecting the "FirmScout" name; and separately licensed dataset, hosted service terms, and API terms.
**Where the protection actually lives:** the dataset licence and the hosted terms, not the code licence.
→ [ADR-0009](../adr/0009-code-and-data-licensing.md), [licensing.md](licensing.md)

### 3.8 "Share-alike for the dataset (ODbL)" — challenged, CC BY 4.0 + contractual terms

**Challenge:** ODbL's share-alike is viral into "derived databases", which is precisely what a customer's internal asset-management database becomes when they import FirmScout data. That is a deal-breaker for the enterprise buyer, and it is unenforceable in practice.
**Alternative:** a two-tier data strategy — **CC BY 4.0** on delayed public snapshots (attribution only; CC 4.0 explicitly covers the EU *sui generis* database right, which ODC licences were written for before CC 4.0 existed), and separate contractual **FirmScout Data Terms** for the live API feed and enriched fields.
**This requires qualified legal review.** Database rights differ materially between the EU, UK, and US, and the extent to which a factual catalogue is protectable at all is jurisdiction-dependent.
→ [ADR-0009](../adr/0009-code-and-data-licensing.md)

### 3.9 "Redis for rate limiting" — challenged, not in the MVP

**Challenge:** Redis is the reflex answer for distributed rate limiting, but it adds an always-on service, a second data store, and a new failure mode, to solve a problem the MVP does not yet have.
**Alternative:** three layers that need no new service — WAF rate rules at the edge (coarse, cheap, absorbs volumetric abuse before it costs compute), in-process token buckets per instance (approximate but adequate when instance count is small), and durable quota counters in PostgreSQL (exact, for billing-relevant monthly quotas).
**Honest limitation:** with N instances, the effective per-second limit is N times the configured one. At MVP instance counts this is acceptable and documented; the WAF layer bounds the total regardless.
**Threshold:** more than ~4 concurrent API instances, or a customer contract requiring exact per-second enforcement.

### 3.10 "OpenSearch for search" — challenged, PostgreSQL full text

**Alternative:** `tsvector` with `pg_trgm` trigram indexes for fuzzy matching, over a precomputed `product_summaries` table. Handles typo tolerance and alias matching for a catalogue in the hundreds of thousands.
**Threshold:** p95 search latency above ~200 ms at realistic catalogue size, or a genuine need for relevance tuning beyond `ts_rank`.

### 3.11 "AI-assisted bootstrap produces the dataset" — challenged in framing

**Challenge:** the brief is already careful here, but the framing invites a dangerous reading: that an LLM can be pointed at the internet and produce a catalogue. It cannot, and a catalogue of plausible-looking wrong firmware versions is worse than no catalogue — it is actively harmful to someone patching a device.
**Reinforcement:** the Discovery Agent produces **source candidates with evidence**, not facts. Every fact reaching the public site has passed deterministic extraction from a retrievable artifact, or explicit human review. The agent's output is a proposal for a pull request, never a database write.
→ [ADR-0006](../adr/0006-ai-as-escalation.md)

### 3.12 Accepted without change

The following proposals from the brief are accepted as specified: Clean Architecture with inward dependencies; modular monolith first; PostgreSQL; sqlc; deterministic collectors separated into fetch/extract/validate/publish stages; immutable history; evidence and provenance on every fact; explicit date precision; OpenTelemetry with a self-hostable Prometheus/Loki/Tempo/Grafana stack; Next.js with TypeScript, Tailwind, and shadcn/ui; a free and genuinely useful public site; layered rate limiting without dataset poisoning or fingerprinting.

---

## 4. Product boundaries

### 4.1 What FirmScout is

A **version metadata catalogue with provenance**. For a given device or software product, it answers: what versions exist, when were they released, what kind of release is each one, which official source says so, and when did we last verify it.

### 4.2 What FirmScout is not

| Not | Why it matters |
| --- | --- |
| A firmware download mirror | Redistribution rights, storage cost, and vendor relations all say no. FirmScout links to official downloads; it does not host binaries. |
| A patch-management or deployment tool | Applying firmware is device-specific, risky, and a different product. FirmScout tells you what exists; your tooling decides what to do. |
| A vulnerability scanner | It does not probe devices. CVE correlation is metadata-to-metadata, based on declared affected ranges. |
| An asset-discovery agent | It does not run on customer networks or inspect customer devices. Inventory arrives by upload or API, never by scanning. |
| A source of "you should upgrade" advice | It reports what vendors published. It never asserts that the newest version is the safest or the recommended one unless the vendor says so explicitly. |
| A full-text mirror of release notes | It stores concise evidence excerpts and links to the original. Reproducing copyrighted documents in full is both a legal risk and a maintenance burden. |

### 4.3 Trust boundaries

The most important boundary is between **what a vendor published** and **what FirmScout inferred**. Every field in the API is one or the other, and the distinction is visible:

- `source.official = true` means the fact came from a manufacturer-controlled domain.
- `recommended` is `null` unless the vendor explicitly designates a recommended version. FirmScout never fills it in by picking the newest.
- `confidence` and `validation_status` travel with candidate data; published data carries the evidence that justified publication.
- Conflicting sources are surfaced as conflicts, not silently resolved by picking one.

### 4.4 Scope of categories

The brief's category list is accepted in full, and the data model treats categories as registry data rather than code, so adding "power and UPS equipment" or "industrial equipment" is a YAML change. The MVP populates four vendors; the schema imposes no ceiling.

Crucially, **release type is orthogonal to category**. A server has BIOS, BMC firmware, drivers, and an appliance OS — four different `release_type` values on products under one vendor. The brief's instruction not to label everything "firmware" is enforced by making `release_type` a required, non-defaulted field with a constrained vocabulary.

---

## 5. Free website versus paid API capability matrix

The design rule: **the free tier is limited by convenience, not by truth.** Nothing on the public site is deliberately wrong, stale, or crippled. What the paid tiers buy is automation, scale, history depth, and guarantees.

| Capability | Anonymous web | Free API key | Professional API | Enterprise |
| --- | --- | --- | --- | --- |
| Product and vendor pages | ✅ full | — | — | — |
| Manual search | ✅ unlimited within abuse limits | ✅ | ✅ | ✅ |
| Latest observed version | ✅ | ✅ | ✅ | ✅ |
| Release date with precision | ✅ | ✅ | ✅ | ✅ |
| Release type classification | ✅ | ✅ | ✅ | ✅ |
| Official source link | ✅ | ✅ | ✅ | ✅ |
| Last verified timestamp | ✅ | ✅ | ✅ | ✅ |
| Lifecycle status (EOL/EOS) | ✅ basic | ✅ basic | ✅ full with dates and evidence | ✅ full |
| Release history | ✅ recent window (e.g. 12 months) | ✅ recent window | ✅ complete | ✅ complete |
| Security advisories | ✅ list and links | ✅ list | ✅ structured with affected ranges | ✅ structured |
| Community corrections | ✅ submit | ✅ submit | ✅ | ✅ |
| Programmatic access | ❌ | ✅ small quota | ✅ high quota | ✅ dedicated quota |
| Bulk lookup (many products per call) | ❌ | ❌ | ✅ | ✅ |
| Webhooks and alerts | ❌ | ❌ | ✅ | ✅ |
| Inventory upload and comparison | ❌ | ❌ | ✅ limited | ✅ |
| Compliance reporting and exports | ❌ | ❌ | ❌ | ✅ |
| Source health metadata | ❌ | ❌ | ✅ | ✅ |
| Freshness commitment | best effort | best effort | ✅ target SLO | ✅ contractual SLA |
| SSO, RBAC, audit log export | ❌ | ❌ | ❌ | ✅ (later phase) |
| Support | community | community | business hours | contractual |

**Why history depth is the right free/paid line:** a human checking "what firmware should my switch be on?" needs the current version and the last few releases. A compliance system proving "this fleet was compliant on 30 June" needs the complete history and point-in-time queries. The first is a genuine free use case; the second is unambiguously automation, and automation is what the paid tier sells.

**What is explicitly *not* used as a paywall lever:** correctness, freshness of the *displayed* current version, official source links, or accessibility. Degrading any of those to drive conversions would destroy the trust the product depends on.

→ [api-commercial.md](api-commercial.md) for plan mechanics, quotas, key lifecycle, and billing events.

---

## 6. Architecture decision summary

| # | Decision | Rationale in one line | ADR |
| --- | --- | --- | --- |
| 1 | Modular monolith, three binaries from one module | Deployment simplicity with enforced internal boundaries | [0001](../adr/0001-modular-monolith.md) |
| 2 | Clean Architecture, dependencies inward | The domain outlives AWS, the ORM, and the AI provider | [0002](../adr/0002-clean-architecture.md) |
| 3 | PostgreSQL as the only stateful dependency | Relational integrity, full-text search, JSONB, and a queue in one engine | [0003](../adr/0003-postgresql.md) |
| 4 | sqlc for data access | Compile-time-checked SQL, no ORM annotations in the domain | [0004](../adr/0004-sqlc.md) |
| 5 | Deterministic collectors, config-driven where possible | Cheap, testable, reviewable by non-programmers | [0005](../adr/0005-deterministic-collectors.md) |
| 6 | AI as escalation only, with budgets | Predictable cost; reproducible behaviour; no hallucinated firmware versions | [0006](../adr/0006-ai-as-escalation.md) |
| 7 | Free useful web, paid automation API | Trust and SEO are the acquisition channel; automation is the product | [0007](../adr/0007-public-web-paid-api.md) |
| 8 | Layered scraping resilience, no poisoning | Raise the cost of abuse without ever serving false data | [0008](../adr/0008-scraping-resilience.md) |
| 9 | Apache-2.0 code; CC BY 4.0 delayed snapshots; contractual live data | The moat is data and operations, not source secrecy | [0009](../adr/0009-code-and-data-licensing.md) |
| 10 | Lambda-first AWS runtime, Fargate documented as the alternative | Idle cost approaches zero; same binary everywhere | [0010](../adr/0010-aws-runtime.md) |
| 11 | OpenTelemetry + Prometheus/Loki/Tempo/Grafana | Vendor-neutral, self-hostable, identical locally and in AWS | [0011](../adr/0011-open-source-observability.md) |
| 12 | Privacy-conscious first-party analytics | Product decisions need data; users do not need trackers | [0012](../adr/0012-product-analytics.md) |
| 13 | Cost minimisation as a functional requirement | Unit economics decide whether the catalogue can grow | [0013](../adr/0013-cost-minimization.md) |
| 14 | `net/http` standard library router | Framework value is now marginal; dependency cost is not | [0014](../adr/0014-stdlib-http-router.md) |
| 15 | `JobQueue` port; PostgreSQL adapter first, SQS later | Self-hostable without AWS; identical contract | [0015](../adr/0015-job-queue-port.md) |
| 16 | Hybrid dataset: registry in Git, facts in PostgreSQL | Reviewable curation, queryable observation | [0016](../adr/0016-hybrid-dataset.md) |
| 17 | Opaque version strings, explicit date precision | Real vendor data is not semver and not always day-precise | [0017](../adr/0017-version-strings-and-date-precision.md) |
| 18 | Compliance status is a first-class source field | Robots and terms are evaluated before collection, not after | [0018](../adr/0018-source-compliance-policy.md) |
| 19 | API on `api.firmscout.dev`, `/api/v1` prefix retained | A base URL consumers hardcode must be able to move; apex cookies must not ride on API requests | [0019](../adr/0019-public-domain-shape.md) |
| 20 | Multi-source conflict is a recorded finding, never an auto-resolved guess | The authority ladder only ever demotes; a same-tier disagreement is the finding, not a tie to break | [0020](../adr/0020-multi-source-conflict-detection.md) |
| 21 | Review decisions record an asserted, unauthenticated actor | There is no login; the audit trail says so in a column rather than pretending otherwise | [0021](../adr/0021-asserted-reviewer-identity.md) |
| 22 | One canonical problem-type catalogue, taken from api.md | Three URIs renamed and two added while the API is still unpublished and nobody depends on them | [0022](../adr/0022-canonical-problem-types.md) |
| 23 | A concurrent dequeue defers work; it does not drain the queue | `SKIP LOCKED` promises no duplication and no loss, never drainage — the test that asserted drainage was wrong, not the queue | [0023](../adr/0023-queue-defers-rather-than-drains.md) |

---

## 7. Clean Architecture review

Full detail in [clean-architecture.md](clean-architecture.md); diagram in [`docs/diagrams/clean-architecture.md`](../diagrams/clean-architecture.md). This section is the review the brief asked for, including where the approach risks overengineering.

### 7.1 Domain boundaries

The domain contains the concepts that would still exist if FirmScout were run on paper by a diligent librarian:

- **Vendor**, **ProductFamily**, **Product**, **ProductAlias** — identity and naming.
- **Source** — a monitored location, its capabilities, its health state machine, and its compliance status.
- **CandidateRelease** — an extracted, not-yet-trusted observation with its state machine.
- **Release** — a published, immutable fact with evidence.
- **Evidence** — source URL, retrieval time, content hash, excerpt, collector version, discovery method.
- **VersionString** — an opaque version with plausibility analysis, explicitly not an ordered type.
- **PartialDate** — a date with declared precision (`exact_day`, `month_only`, `year_only`, `unknown`).
- **ReleaseType** — the constrained vocabulary that prevents "everything is firmware".
- **Scheduling policy** — the pure function from source history to next check interval.

What is deliberately **not** in the domain: HTTP status codes, SQL, selectors, S3 keys, token counts, AWS ARNs, and anything whose definition is owned by an external system.

### 7.2 Dependency direction

```
domain  ←  application  ←  adapters  ←  platform (wiring)
```

`internal/domain` imports only the standard library. `internal/application` imports `domain` and the standard library, and defines every port it needs as an interface it owns. `internal/adapters/*` import `application` and `domain` plus their specific third-party library. `internal/platform` is the only package permitted to import all of them, because its job is composition.

This is enforced mechanically, not by convention: `internal/archtest` shells out to `go list -deps` and fails the test suite if a forbidden edge exists. A rule that is not enforced is a comment.

### 7.3 Application ports

Ports are defined by the application because the application decides what it needs. The full list:

| Port | Purpose | MVP adapter | Later adapters |
| --- | --- | --- | --- |
| `VendorRepository`, `ProductRepository`, `SourceRepository`, `ReleaseRepository`, `CandidateRepository`, `ReviewRepository` | Persistence | PostgreSQL (sqlc) | — |
| `ArtifactStore` | Raw fetched content, content-addressed | Filesystem / PostgreSQL large object | S3 |
| `Fetcher` | Guarded, conditional HTTP retrieval | `net/http` with SSRF guard | — |
| `Normalizer` | Content normalisation and hashing | HTML/text normaliser | PDF, XML |
| `CollectorRegistry` | Resolve a source to a collector | Config-driven + code collectors | — |
| `JobQueue` | Enqueue/dequeue background work | PostgreSQL `SKIP LOCKED` | SQS |
| `Clock`, `IDGenerator` | Determinism in tests | Real / fake | — |
| `UnitOfWork` | Transaction boundary | pgx transaction | — |
| `EventPublisher` | Domain and analytics events | In-process + PostgreSQL | EventBridge |
| `UsageRecorder`, `APIKeyRepository`, `QuotaStore` | Entitlement and metering | PostgreSQL | — |
| `DiscoveryAgent`, `RepairAgent`, `ValidationAgent`, `ClassificationAgent`, `SourceQualityAgent` | AI escalation | **fakes only in the MVP** | Anthropic / other |

### 7.4 Infrastructure adapters

Each adapter is a translation layer and nothing more. The rules that keep them honest:

- An adapter may not contain a business rule. If `postgres` decides *whether* a release is publishable, the rule has escaped the domain.
- An adapter converts its library's errors into application-level error types at its boundary. `pgx.ErrNoRows` never reaches a use case.
- Collector adapters return `[]CandidateRelease` and have no repository handle at all — they are structurally incapable of publishing. This is the brief's requirement, implemented as a type constraint rather than a code-review guideline.

### 7.5 Transaction ownership

The **application layer owns transactions**, through the `UnitOfWork` port. A use case declares "these repository calls are one atomic unit"; the PostgreSQL adapter implements that with a pgx transaction. Repositories never open transactions themselves, because a repository cannot know whether it is the whole operation or part of one.

Concretely: `PublishRelease` opens one unit of work that inserts the release, inserts release-product mappings, inserts evidence, transitions the candidate to `published`, refreshes the product summary, and writes an audit event. Either all of it happens or none of it does. `CheckSource` uses a *separate*, earlier transaction, because a fetch is not something that can be rolled back.

### 7.6 Domain invariants

Invariants live in constructors and state-transition methods, so an invalid object cannot be constructed:

- A `Release` cannot exist without a non-empty version, a source reference, and evidence.
- A `PartialDate` with `month_only` precision cannot expose a day component — the type has no accessor for it.
- A `CandidateRelease` can only move along declared transitions; `published → discovered` is not expressible.
- A `Source` in `retired` accepts no further checks.
- A `Product` must reference an existing vendor, and its slug is immutable after creation (it is a public URL and an API identifier).

### 7.7 Domain events

Events are values produced by the domain and dispatched by the application: `SourceChanged`, `SourceUnchanged`, `SourceFailed`, `CandidateCreated`, `CandidateValidated`, `CandidateRejected`, `ReleasePublished`, `ReleaseWithdrawn`, `HumanReviewRequested`, `SourceRelocated`.

In the MVP these are dispatched in-process and persisted to `analytics_events` and the job queue. On AWS the same events can be forwarded to EventBridge without the domain changing, which is the point of defining them as values rather than as SQS message sends.

### 7.8 Testing boundaries

| Layer | Test style | Dependencies allowed |
| --- | --- | --- |
| Domain | Table-driven unit tests | None — no DB, no network, no clock |
| Application | Use-case tests against in-memory fakes | Fakes only |
| Adapters (PostgreSQL) | Integration tests against a real database | PostgreSQL from Compose; skipped with an explicit message if absent |
| Adapters (HTTP fetch) | `httptest` servers | Local only |
| Collectors | Fixture-based extraction tests | Recorded fixtures on disk — **never a live vendor site** |
| AI agents | Schema-validated fixed responses | Recorded JSON |
| API handlers | `httptest` with fake use cases | None |

The rule that live manufacturer websites are never a test dependency is absolute. A test suite that fails because Poly reorganised its documentation site is a test suite that teaches contributors to ignore failures.

### 7.9 Risks of overengineering — an honest accounting

Clean Architecture applied dogmatically to a small project produces ceremony without benefit. The specific risks here, and the mitigations:

| Risk | Mitigation |
| --- | --- |
| An interface per struct, most with one implementation forever | Ports are defined only where substitution is genuinely needed: persistence, network, queue, clock, AI. `VersionString` has no interface. |
| Mapping structs between four layers | Domain entities are used directly by the application. Mapping happens only at the two real boundaries: sqlc rows → domain, and domain → API DTOs. |
| Use cases that are one-line pass-throughs to a repository | Read paths for simple queries call a query port directly (CQRS-lite). `GetProduct` is not wrapped in ceremony. |
| Premature module splitting | One Go module, one binary set. Package boundaries are enforced; deployment boundaries are not created until measured need. |
| Abstracting the database "in case we switch" | We will not switch. The PostgreSQL adapter is free to use PostgreSQL-specific features (`SKIP LOCKED`, `tsvector`, JSONB). The port exists for *testing*, not for portability. |

**The trade-off accepted:** roughly 15% more code than a straightforward layered design, in exchange for a domain that can be tested in milliseconds and an AI/cloud/queue strategy that can change without touching business rules. Given that the AI provider landscape and FirmScout's own cost model will both change within a year, that is a good trade. It would not be a good trade for a CRUD application.

---

## 8. Domain and module boundaries

Diagram: [`docs/diagrams/module-dependencies.md`](../diagrams/module-dependencies.md).

### 8.1 Bounded contexts

FirmScout has five cohesive areas. They are packages today, and are the natural seams if services are ever needed.

| Context | Owns | Key entities | Talks to |
| --- | --- | --- | --- |
| **Catalog** | Identity and naming of things | Vendor, ProductFamily, Product, Alias, Category | Everything reads it |
| **Sourcing** | Where facts come from and whether they changed | Source, SourceCheck, Artifact, CollectorDefinition | Catalog (read), Ingestion (emits) |
| **Ingestion** | Turning observations into published facts | CandidateRelease, ValidationResult, Release, Evidence, ReviewItem | Catalog, Sourcing |
| **Distribution** | Serving the public | ProductSummary, search index, API keys, quotas, usage | Catalog, Ingestion (read) |
| **Intelligence** | Escalation and enrichment | AIRun, SecurityAdvisory, CVE mapping, LifecycleEvent | All (read), proposes to Sourcing and Ingestion |

### 8.2 Allowed and forbidden dependencies

**Allowed:**
- Any context may *read* Catalog.
- Ingestion may read Sourcing.
- Distribution may read Catalog and Ingestion.
- Intelligence may read everything and *propose* to Sourcing and Ingestion.

**Forbidden, and enforced:**
- Catalog must not depend on any other context. It is the vocabulary.
- Sourcing must not write releases. It produces candidates only.
- Distribution must not write anything except usage, analytics, and corrections-as-proposals.
- Intelligence must not write to `releases`, `sources`, or `products` directly. Every AI output is a proposal reviewed by an Ingestion or Sourcing use case.
- No context imports an adapter package. Contexts speak to ports.

The last two are the ones that matter most. They are the structural expression of "AI-generated code must never deploy directly to production" and "collectors cannot write to official release tables".

### 8.3 Per-component summary

The brief asks that every major component state its responsibility, layer, necessity, cost impact, MVP status, observation, security risk, and replacement condition. That table is long; it lives in [overview.md](overview.md#component-catalogue) and is summarised here for the components that carry the most risk.

| Component | Layer | MVP | Main cost driver | Main security risk | Replace when |
| --- | --- | --- | --- | --- | --- |
| Fetcher | Adapter | ✅ | Egress, per-request compute | **SSRF** — attacker-supplied source URLs reaching internal metadata endpoints | Never; harden in place |
| Config collectors | Adapter | ✅ | CPU on changed content only | Malicious HTML causing pathological parsing | Code collector when config cannot express the source |
| Job queue (PostgreSQL) | Adapter | ✅ | Database IOPS | Poison messages causing retry storms | >10 jobs/s sustained → SQS |
| Public API | Adapter | ✅ | Lambda invocations, egress | Enumeration, quota evasion | Never |
| Product summaries | Application + DB | ✅ | Storage, refresh CPU | Stale data after publication | Materialised view if refresh cost grows |
| AI agents | Adapter | ❌ (ports only) | **Tokens — the highest-variance cost** | Prompt injection from fetched content | N/A |
| Observability stack | Infrastructure | ✅ locally | Storage, cardinality | Secrets in logs | Managed service only if operating cost exceeds hosting cost |

---

## 9. Complete Mermaid diagram set

All diagrams live in [`docs/diagrams/`](../diagrams/), one file each, every file containing the diagram, an explanation, its assumptions, its failure modes, related ADRs, and the code modules that implement it. They are validated in CI by [`scripts/check-mermaid.sh`](../../scripts/check-mermaid.sh), which extracts every fenced `mermaid` block in the repository and renders it, so a syntax error fails the build rather than silently producing a broken image on GitHub.

Only diagram types GitHub renders natively are used: `flowchart`, `sequenceDiagram`, `stateDiagram-v2`, and `erDiagram`.

| # | Diagram | File |
| --- | --- | --- |
| 1 | System context | [system-context.md](../diagrams/system-context.md) |
| 2 | High-level architecture | [high-level-architecture.md](../diagrams/high-level-architecture.md) |
| 3 | Clean Architecture dependencies | [clean-architecture.md](../diagrams/clean-architecture.md) |
| 4 | Module dependencies, allowed and forbidden | [module-dependencies.md](../diagrams/module-dependencies.md) |
| 5 | Initial bootstrap flow | [bootstrap-flow.md](../diagrams/bootstrap-flow.md) |
| 6 | Update decision tree | [update-decision-tree.md](../diagrams/update-decision-tree.md) |
| 7 | Adaptive watcher scheduling | [adaptive-scheduling.md](../diagrams/adaptive-scheduling.md) |
| 8 | Source failure and AI repair | [source-repair-flow.md](../diagrams/source-repair-flow.md) |
| 9 | AI cost escalation | [ai-cost-escalation.md](../diagrams/ai-cost-escalation.md) |
| 10 | Release state machine | [release-state-machine.md](../diagrams/release-state-machine.md) |
| 11 | Source health state machine | [source-health-state-machine.md](../diagrams/source-health-state-machine.md) |
| 12 | Request and scraping protection | [request-protection-flow.md](../diagrams/request-protection-flow.md) |
| 13 | API request sequence | [api-sequence.md](../diagrams/api-sequence.md) |
| 14 | Collector sequence | [collector-sequence.md](../diagrams/collector-sequence.md) |
| 15 | Data model ER | [data-model.md](../diagrams/data-model.md) |
| 16 | AWS deployment | [deployment.md](../diagrams/deployment.md) |
| 17 | Open-source contribution flow | [contribution-flow.md](../diagrams/contribution-flow.md) |
| 18 | Observability architecture | [observability.md](../diagrams/observability.md) |
| 19 | Product analytics flow | [product-analytics.md](../diagrams/product-analytics.md) |
| 20 | Cost-aware request flow | [cost-aware-request.md](../diagrams/cost-aware-request.md) |
| 21 | Storage lifecycle | [storage-lifecycle.md](../diagrams/storage-lifecycle.md) |
| 22 | Cost-containment incident | [cost-containment-incident.md](../diagrams/cost-containment-incident.md) |
| 23 | CVE correlation | [cve-correlation.md](../diagrams/cve-correlation.md) |
| 24 | Version normalisation | [version-normalization.md](../diagrams/version-normalization.md) |
| 25 | Alias resolution | [alias-resolution.md](../diagrams/alias-resolution.md) |
| 26 | Multi-source conflict resolution | [multi-source-conflict.md](../diagrams/multi-source-conflict.md) |
| 27 | Entitlement evaluation | [entitlement-evaluation.md](../diagrams/entitlement-evaluation.md) |
| 28 | Dataset correction | [dataset-correction.md](../diagrams/dataset-correction.md) |
| 29 | Human review prioritisation | [review-prioritization.md](../diagrams/review-prioritization.md) |

---

## 10. Repository structure

A single monorepo. One Go module at the root; the Next.js app has its own `package.json`. Rationale: the API contract, the OpenAPI document, the collector configs, and the dataset schemas change together and should be reviewed together.

```
/
  apps/
    api/            Go: public HTTP API binary (main.go only — wiring lives in internal/platform)
    worker/         Go: scheduler loop + job runner binary
    cli/            Go: firmscout CLI (migrate, registry sync, check-source, apikey)
    web/            Next.js public site
  internal/
    domain/         Entities, value objects, state machines, domain services. Stdlib only.
    application/    Ports (interfaces) and use cases. Imports domain + stdlib only.
    adapters/
      postgres/     sqlc-generated code + repository implementations + queue adapter
      httpapi/      HTTP handlers, middlewares, presenters, problem+json errors
      fetch/        Guarded HTTP fetcher (SSRF guard, conditional GET, robots, limits)
      normalize/    Content normalisation and section hashing
      collectors/   Config-driven collector engines (html_selectors, text_regex, …)
      telemetry/    OpenTelemetry setup, slog bridge, metric instruments
      ai/           AI provider adapters (post-MVP) and fakes
    platform/       Configuration, dependency wiring, lifecycle. The only package that imports everything.
    archtest/       Dependency-rule enforcement test
  collectors/
    sdk/            Public collector contract + fixture test helpers for code-based collectors
    config/         YAML collector configurations, one directory per vendor
    vendors/        Code-based collectors, one package per vendor (empty in MVP)
  agents/           AI agent contracts: JSON schemas for inputs/outputs, prompt version registry (no runtime code in MVP)
  database/
    migrations/     Numbered SQL migrations (goose format), embedded into the binaries
    queries/        sqlc query files
    seeds/          Development-only seed scripts (never production facts)
  dataset/          Registry YAML: vendors/, products/, sources/ — synchronised into PostgreSQL
  packages/
    schemas/        JSON Schemas for dataset YAML, collector configs, and AI agent I/O
    api-client/     Generated TypeScript client from OpenAPI (post-MVP)
  infrastructure/
    docker/         docker-compose.yml, Dockerfiles, local config
    observability/  OTel Collector, Prometheus, Loki, Tempo, Grafana provisioning
    terraform/      AWS modules (post-MVP; skeleton only)
  docs/             architecture/, adr/, diagrams/, api/, collectors/, data-model/, security/, finops/
  testdata/fixtures/  Recorded and synthetic fixtures with provenance READMEs
  scripts/          check-mermaid.sh, archcheck, dev helpers
  .github/          CI workflows, issue and PR templates, CODEOWNERS
```

**Deviation from the brief's proposal:** the brief listed `internal/ingestion`, `internal/validation`, `internal/sourcehealth`, `internal/entitlements` as siblings of `domain` and `application`. Those are bounded contexts, and they exist — but *inside* `domain/` and `application/` as sub-packages, so that the layer rule stays one-dimensional and machine-checkable. `internal/domain/ingestion` and `internal/application/ingestion` are clearer than a top-level `ingestion` that would have to contain both layers.

**Dataset location:** hybrid, per §3.3 and [ADR-0016](../adr/0016-hybrid-dataset.md).

---

## 11. PostgreSQL schema

The complete DDL with commentary is in [data-model.md](data-model.md); the ER diagram is [`docs/diagrams/data-model.md`](../diagrams/data-model.md). The design rules that shape every table:

1. **Registry tables** (`vendors`, `product_families`, `products`, `product_aliases`, `categories`, `sources`, `collector_definitions`) carry `managed_by` and `registry_path` so the Git-synchronised rows are distinguishable from rows created through the admin path.
2. **Fact tables** (`releases`, `release_product_mappings`, `evidence`) are **append-only**. There is no `UPDATE` statement against `releases` in the query set; corrections and withdrawals are new rows referencing the original.
3. **Observation tables** (`source_checks`, `source_artifacts`, `candidate_releases`, `validation_results`) are high-volume and subject to retention policies; they are partition-ready by month but not partitioned in the MVP.
4. **Dates carry precision.** `release_date DATE NOT NULL` is paired with `release_date_precision` (`exact_day | month_only | year_only | unknown`), and a `CHECK` constraint forces day = 1 for month precision and month = day = 1 for year precision, so the stored date is a canonical anchor and can never be mistaken for a real day. `unknown` precision requires `release_date IS NULL`.
5. **Versions are text.** `raw_version TEXT` and `normalized_version TEXT` with no numeric parsing at the database level. There is no `ORDER BY version`.
6. **Applicability is a mapping table.** A release applies to products through `release_product_mappings`, which carries `hardware_revision`, `region`, `channel`, and `deployment_mode`, so one release can legitimately map to a whole family, one model, or one model's EU variant.
7. **Every candidate and release points at evidence.** `evidence` rows hold `source_url`, `retrieved_at`, `content_hash`, `excerpt`, `collector_id`, `collector_version`, `discovery_method`, and, when AI was involved, `ai_model_id` and `prompt_version`.
8. **Jobs are a table.** `jobs` has `idempotency_key UNIQUE`, `status`, `attempts`, `run_after`, `locked_until`, and `dead_lettered_at`, implementing the queue port.
9. **Summaries are precomputed.** `product_summaries` is refreshed by the `PublishRelease` use case and is what the public site and search read. PostgreSQL is not queried per page view for the join.
10. **API keys are hashed.** `api_keys` stores a SHA-256 of the secret and an 8-character display prefix; the plaintext is shown once at creation and never stored.

The MVP migration creates the tables needed for the vertical slice plus the fact and entitlement tables. `security_advisories`, `cves`, `cve_product_mappings`, `lifecycle_events`, `user_corrections`, and the full `ai_runs` shape are specified in `data-model.md` and created by later migrations, so the schema evolves by addition.

---

## 12. API design

Full design in [api.md](api.md); the machine-readable contract is [`docs/api/openapi.yaml`](../api/openapi.yaml).

**Principles:** versioned path prefix (`/api/v1`), stable slugs as identifiers, cursor pagination, RFC 9457 `application/problem+json` for every error, `ETag` and `Cache-Control` on every cacheable response, `X-Request-Id` echoed on every response, rate-limit headers on every response, and no field in the response that the free tier could not also see in the browser (the paywall is on volume and endpoints, not on hiding fields in shared responses).

**The cache policy is chosen by the caller, not only by the route.** The table below describes an anonymous request. Any request that presented a credential receives `Cache-Control: private, no-store` on the same endpoint, whatever its row says — the immutable `/releases/{id}` included — because the release-history body depends on the caller's plan and `Vary` alone is not trusted at the edge. The `ETag` is still issued, so a keyed client can revalidate its own copy. Enforced in `CacheHeaders` (`internal/adapters/httpapi/middleware.go`); reasoning in [api.md](api.md) §1 and [security.md](security.md) §4.10 (T-13).

| Endpoint | Auth | Cache | Tier |
| --- | --- | --- | --- |
| `GET /api/v1/search?q=` | optional key | short (60 s), normalised query key | all |
| `GET /api/v1/vendors` | optional | long | all |
| `GET /api/v1/vendors/{slug}` | optional | long, invalidated on publish | all |
| `GET /api/v1/products/{slug}` | optional | long, invalidated on publish | all |
| `GET /api/v1/products/{slug}/releases` | optional | medium (300 s) | window for free; full history for Professional+ |
| `GET /api/v1/products/{slug}/latest` | optional | medium (300 s), invalidated on publish | all |
| `GET /api/v1/products/{slug}/advisories` | optional | medium | all (structured detail Professional+) — **not routed yet** (Phase 4) |
| `GET /api/v1/releases/{id}` | optional | immutable | all |
| `POST /api/v1/lookup` (bulk) | key required | none | Professional+ — **not routed yet** (Phase 3) |
| `GET /api/v1/usage` | key required | none | keyed — **not routed yet** (Phase 3) |

The three rows marked *not routed yet* are the intended contract and have no handler today; `internal/adapters/httpapi/router.go` registers the other seven plus `/healthz`, `/readyz` and `/metrics`, and `docs/api/openapi.yaml` matches the router exactly, in both directions, under test. A fourth group exists and is deliberately outside this table: the four unauthenticated `/internal/review` routes, off by default behind three switches on two hosts — see [api.md](api.md) §11 and [ADR-0021](../adr/0021-asserted-reviewer-identity.md).

**Anonymous access to the read endpoints is deliberate.** The public website is a client of this same API. Blocking anonymous API access would only push the site's own traffic into a different code path with a different cache and a different failure mode. Anonymous requests are simply rate-limited more tightly and cannot reach bulk endpoints.

Example `latest` response, showing the precision discipline:

```json
{
  "vendor": { "name": "MikroTik", "slug": "mikrotik" },
  "product": { "name": "RouterOS", "slug": "mikrotik-routeros" },
  "latestRelease": {
    "id": "rel_01J...",
    "rawVersion": "7.24.2",
    "normalizedVersion": "7.24.2",
    "releaseType": "embedded_os",
    "channel": "stable",
    "releaseDate": "2026-09-02",
    "releaseDatePrecision": "exact_day",
    "recommended": null,
    "withdrawn": false,
    "source": { "official": true, "url": "https://mikrotik.com/download/changelogs", "kind": "html" },
    "firstObservedAt": "2026-09-03T12:00:00Z",
    "lastVerifiedAt": "2026-09-03T18:30:00Z"
  }
}
```

`releaseDate` is rendered as `YYYY-MM` for month precision and `YYYY` for year precision, and omitted for unknown, so a consumer cannot parse a fake day out of it.

---

## 13. Collector SDK

Full document: [collector-sdk.md](collector-sdk.md). The contract, in Go:

```go
type Collector interface {
    ID() string                 // stable, e.g. "html_selectors" or "vendor.fortinet.docs"
    Version() string            // bumped on any behavioural change; recorded in evidence
    Vendor() string             // "" for generic engines
    Supports(src Source) bool
    Fetch(ctx context.Context, src Source, prior *FetchState) (Artifact, FetchOutcome, error)
    Extract(ctx context.Context, src Source, art Artifact) ([]CandidateRelease, error)
}
```

Key properties, each enforced by the SDK rather than by convention:

- `Fetch` receives the prior ETag/Last-Modified and **must** issue a conditional request when the source supports it. The SDK's `Fetcher` does this; a code collector that bypasses it fails the contract test.
- `Extract` is a pure function of `(Source, Artifact)`. It is given no clock, no repository, and no network. The same artifact produces the same candidates forever, which is what makes fixture tests meaningful.
- Candidates carry `RawVersion`, `NormalizedVersion` (may equal raw), `ReleaseDate PartialDate`, `ReleaseType`, `Channel`, `Applicability` (product match hints), `Evidence` excerpt, and a `Confidence` in `[0,1]` — but **no** `ReleaseID`, because the collector does not decide whether this is a release.
- `collectors/sdk/collectortest` provides `RunFixtures(t, collector, dir)`: every `*.fixture.html` (or `.json`, `.txt`) next to an `expected.json` is extracted and compared. Contributors add a collector by adding a fixture pair.
- Resource limits (max artifact bytes, extraction wall-clock, max candidates per artifact) are applied by the SDK wrapper, not trusted to the collector.

---

## 14. Configuration-driven collector specification

Full specification with JSON Schema: [collector-config-spec.md](collector-config-spec.md). The MikroTik changelog config used by the vertical slice, as measured on 2026-09-03:

```yaml
apiVersion: firmscout.dev/v1alpha1
kind: CollectorConfig
metadata:
  id: mikrotik.changelogs
  vendor: mikrotik
  version: 1
spec:
  engine: html_selectors
  product_match:
    product: mikrotik-routeros
  fetch:
    expected_content_type: text/html
    max_bytes: 2097152
  normalize:
    section_selector: "div.changelog-header"         # hash only this, ignore the rest of the page
    strip: [script, style, noscript, "[wire\\:id]", "[x-data]"]
  extract:
    release_container: "div.changelog-header"
    fields:
      version:
        selector: "span.font-bold"
        transform: trim
      channel:
        selector: "div.rounded-\\[4px\\]"
        transform: [trim, lowercase]
        map: { "long-term": long_term, "stable": stable, "testing": testing, "development": development }
      release_date:
        selector: ":scope"
        regex: "(\\d{4}-\\d{2}-\\d{2})"
        date_format: "2006-01-02"
        precision: exact_day
    release_type: embedded_os
    evidence_excerpt: ":scope"
```

Engines in scope: `html_selectors`, `text_regex` (MVP); `json_path`, `rss_atom`, `xml_xpath`, `pdf_text`, `github_releases` (roadmap). The spec is versioned by `apiVersion`; a breaking change to field semantics bumps it. A config is a *proposal* until `firmscout collector test` passes its fixtures and a maintainer merges it.

---

## 15. AI agent contracts

Full document: [ai-agents.md](ai-agents.md); schemas in [`agents/`](../../agents/); escalation diagram in [`docs/diagrams/ai-cost-escalation.md`](../diagrams/ai-cost-escalation.md).

Every agent has the same envelope. Input and output are JSON validated against a schema *before* the output is allowed to influence anything. An output that fails schema validation is a failed run, recorded and billed, and the fallback is human review — never a retry loop.

```json
{
  "run": {
    "agent": "repair",
    "trigger_reason": "selector_miss",
    "prompt_version": "repair/2026-09-01",
    "model_id": "<provider model id>",
    "budget_cap_usd": 0.50
  },
  "input": { "source_id": "...", "previous_artifact_ref": "...", "current_artifact_ref": "..." },
  "output": { "proposals": [ ... ], "confidence": 0.0, "ambiguities": [ ... ], "evidence": [ ... ] },
  "usage": { "input_tokens": 0, "output_tokens": 0, "estimated_cost_usd": 0.0 }
}
```

| Agent | Trigger | Produces | May write to |
| --- | --- | --- | --- |
| Discovery | Bootstrap; new vendor | Source registry proposals, collector config proposals, product/alias proposals, each with evidence URLs | `review_items` only |
| Repair | Source `broken`/`relocated`; selector miss; content-type change | Replacement URL, updated selectors, generated fixture and expected output, PR body | `review_items`; a branch in Git via CI |
| Validation | Candidate with low confidence, implausible transition, or multi-source conflict | Structured verdict with per-check reasoning | `validation_results` (advisory) |
| Classification | Candidate with `release_type = unknown` | `release_type` with confidence | `candidate_releases.proposed_release_type` |
| Source quality | New source registration | Quality class with evidence | `sources.quality_class_proposed` |

**None of them writes to `releases`, `sources`, or `products`.** The Go type system reflects this: agent ports return proposal types that no publishing use case accepts as input without a human decision or a deterministic validation pass.

Model selection is by task: classification and source quality use the least expensive capable model; discovery and repair may use a more capable one, within the per-job cap. Provider choice is an adapter decision; the domain sees `AgentRun` and `Proposal`.

---

## 16. Update and publication rules

Full pipeline: [update-pipeline.md](update-pipeline.md); decision tree: [`docs/diagrams/update-decision-tree.md`](../diagrams/update-decision-tree.md); state machines: [release](../diagrams/release-state-machine.md), [source](../diagrams/source-health-state-machine.md).

**A changed hash is not a release.** It is a reason to run extraction. Extraction produces candidates. Candidates pass through deterministic validation before anything is published. The validation gates, in order, each of which can reject or route to review:

| # | Gate | Reject | Route to human review |
| --- | --- | --- | --- |
| 1 | Product identity resolved (exact product or family, via slug or alias) | No match | Ambiguous match (multiple products) |
| 2 | Non-empty version after normalisation | Empty | — |
| 3 | Source is `active` and compliance status permits collection | Otherwise | — |
| 4 | Exact duplicate: same product + same normalised version + same channel already published | Duplicate (update `last_verified_at` on the existing release instead) | — |
| 5 | Date precision valid; date not in the future beyond a tolerance; not before vendor founding | Invalid | Future-dated |
| 6 | Version transition plausible relative to latest observed | — | Implausible (large regression, token-shape change) |
| 7 | Evidence retained (artifact stored, excerpt non-empty, hash present) | Missing | — |
| 8 | Confidence ≥ threshold for auto-publication (default 0.85 for official sources; community sources always review) | — | Below threshold |
| 9 | Applicability constraints (hardware revision, region, channel) consistent with product definition | Inconsistent | Unknown revision/region |
| 10 | Multi-source agreement, when more than one active source covers the product | — | Disagreement on version or date |

**Publication** inserts a new `releases` row, maps it to products, marks the previous latest-observed row as no longer latest (a flag update on a derived column, not a change to the fact), refreshes `product_summaries`, invalidates the CDN path, and emits `release.published`.

**Withdrawal** never deletes. It inserts a `withdrawn` marker with reason and evidence; the release remains queryable with `withdrawn = true`. **Correction** inserts a new row with `corrects_release_id` and the audit trail of who approved it. **Rollback of a bad import** is a bulk withdrawal keyed on `collector_run_id`, which is why every release records the run that produced it.

The system therefore answers the brief's questions by query, not by reconstruction: *latest observed* is the row flagged latest; *latest as of date D* is the newest row with `first_observed_at ≤ D` not withdrawn as of D; *first observed* is a column; *which source* is the evidence join; *which products share this release* is the mapping table.

---

## 17. AWS architecture

Full document: [aws-deployment.md](aws-deployment.md); diagram: [`docs/diagrams/deployment.md`](../diagrams/deployment.md); decision: [ADR-0010](../adr/0010-aws-runtime.md).

**Shape:** Route 53 and ACM in front of CloudFront; AWS WAF and Shield Standard at the edge; S3 for static assets and for content-addressed artifacts; API Gateway HTTP API routing to the Go API running on Lambda through the Lambda Web Adapter; the Next.js site on Lambda through OpenNext; workers on Lambda triggered by SQS and by EventBridge Scheduler; SQS queues each paired with a dead-letter queue; PostgreSQL on RDS or Aurora Serverless v2 in private subnets; Secrets Manager for credentials; CloudWatch for infrastructure-level signals that the OpenTelemetry Collector cannot see.

**The properties that matter:**

- **The same binary runs everywhere.** The Lambda Web Adapter puts a normal HTTP server behind Lambda without the code knowing. `docker compose up`, ECS Fargate, and Lambda all run the identical artifact. That is what makes the Lambda choice reversible.
- **Nothing but the database is always on.** Idle cost converges on the database bill.
- **Outbound fetching is deliberately placed** so that the architecture does not require a NAT Gateway, whose fixed hourly cost plus per-GB processing charge would dominate a small deployment's bill. The trade-off is documented in the deployment doc rather than discovered later.
- **Migrations are expand-and-contract.** A destructive migration never ships in the same deployment as the code that requires it, because Lambda versions overlap during a rollout.

**What is *not* built:** the Terraform under `infrastructure/terraform/` is a skeleton. Nothing is deployed. No AWS account is configured. This is stated plainly rather than implied.

---

## 18. Docker Compose architecture

The local environment is the reference environment. A contributor who runs `docker compose up` gets the same telemetry, the same database, and the same job semantics as production, with the AWS-specific adapters swapped at the port boundary.

| Service | Image | Purpose | Profile |
| --- | --- | --- | --- |
| `postgres` | `postgres:17` | The only stateful dependency; also the job queue and the artifact metadata store | default |
| `api` | built from repo | Go HTTP API | default |
| `worker` | built from repo | Scheduler loop and job runner | default |
| `web` | built from repo | Next.js public site | default |
| `otel-collector` | `otel/opentelemetry-collector-contrib` | Receives OTLP, batches, filters, redacts, fans out | `observability` |
| `prometheus` | `prom/prometheus` | Metrics | `observability` |
| `loki` | `grafana/loki` | Logs | `observability` |
| `tempo` | `grafana/tempo` | Traces | `observability` |
| `grafana` | `grafana/grafana` | Dashboards, provisioned as code | `observability` |

The observability services sit behind a Compose profile so that a contributor working on a collector can run three containers instead of nine. Grafana datasources and dashboards are provisioned from `infrastructure/observability/`, never configured by hand, so a dashboard is a reviewable file.

**Local substitutions at the port boundary:** `JobQueue` → PostgreSQL instead of SQS. `ArtifactStore` → local filesystem instead of S3. `EventPublisher` → in-process instead of EventBridge. Agent ports → recorded fixtures instead of a live model. No business code changes between local and AWS.

---

## 19. Open-source observability architecture

Full document: [observability.md](observability.md); diagram: [`docs/diagrams/observability.md`](../diagrams/observability.md); decision: [ADR-0011](../adr/0011-open-source-observability.md).

OpenTelemetry is the instrumentation foundation for traces, metrics, and logs. Applications speak OTLP to an OpenTelemetry Collector, which batches, samples, filters attributes, and redacts secrets before fanning out to Prometheus, Loki, and Tempo, all read by Grafana. Alerting runs through Grafana Alerting and Alertmanager. Continuous profiling with Pyroscope is deferred until a measured need exists.

Two constraints shape the implementation:

**The domain cannot import an OTel SDK.** The dependency rule forbids it. Instrumentation of business logic therefore happens through a port, or at the adapter boundary where the operation is already crossing into infrastructure. In practice almost all useful spans are at adapter boundaries anyway, so this costs little.

**Trace context must survive the queue hop.** This is where correlation is normally lost. The `JobQueue` port carries a metadata map, the queue adapter writes W3C `traceparent` into it on enqueue, and the worker restores it on dequeue, so a single trace runs from the HTTP request or scheduler tick through the fetch, the extraction, the validation, and the publication.

**Cardinality is a budget, not an afterthought.** Product slugs, source ids, URLs, and API keys are never Prometheus labels. Safe dimensions are vendor, source type, outcome, and endpoint pattern. Per-entity detail belongs in logs and traces.

---

## 20. Product analytics design

Full document: [analytics.md](analytics.md); diagram: [`docs/diagrams/product-analytics.md`](../diagrams/product-analytics.md); decision: [ADR-0012](../adr/0012-product-analytics.md).

First-party, privacy-conscious, no third-party trackers, no fingerprinting. Events are validated against a schema, passed through a privacy filter, aggregated, and stored in PostgreSQL, then read by Grafana.

The privacy filter is the load-bearing part: no raw IP storage (a salted rotating hash exists only for abuse detection, with short retention), coarse geography at most, no persistent visitor identifier for anonymous users, truncated search queries, and an absolute prohibition on recording API keys, tokens, credentials, customer inventory contents, or authenticated portal content.

The single most valuable signal is **`SearchReturnedNoResults`**. Every zero-result search is either a product FirmScout should catalogue, an alias it should learn, or a spelling it should tolerate. It converts directly into a prioritised backlog, which is why it is instrumented from day one rather than added later.

---

## 21. Grafana dashboard specification

Full specification: [grafana-dashboards.md](grafana-dashboards.md).

Seven dashboards: Platform overview, API operations, Collector health, AI operations, Search and product analytics, Cost and FinOps, and Abuse and scraping. Each is specified with its audience, its panels in layout order, the exact PromQL or LogQL for every panel, units, and thresholds — and, critically, **the decision each dashboard exists to support**. A dashboard that supports no decision is deleted.

Dashboards and datasources are provisioned from version-controlled JSON under `infrastructure/observability/grafana/`. Editing a dashboard in the UI without committing it is treated as a defect, because it makes the self-hosted and hosted deployments diverge invisibly.

The FinOps dashboard carries a standing caveat: its figures are **estimates** derived from consumed units multiplied by configured rates. They are not billing data and must never be quoted as such.

---

## 22. Security threat model

Full document: [security.md](security.md); decision context: [ADR-0008](../adr/0008-scraping-resilience.md).

The threat model is organised by trust boundary, with STRIDE applied per boundary. Two threats dominate.

**Server-side request forgery is the highest-severity technical threat.** FirmScout's core function is fetching URLs that contributors propose and that AI agents discover. An attacker who gets `http://169.254.169.254/latest/meta-data/` into the source registry reaches cloud credentials. The mitigation is layered: scheme restriction to HTTP and HTTPS; DNS resolution followed by IP validation against blocked ranges (loopback, link-local, RFC 1918, CGNAT, IPv6 unique-local, IPv4-mapped IPv6, and the metadata endpoints); validating the resolved literal address from inside `net.Dialer.Control`, immediately before `connect(2)`, so there is no re-resolution between the check and the use and DNS rebinding has no window; **re-validating after every redirect**, because a redirect chain is the usual bypass; rejecting non-standard IP encodings; and network-level egress restriction as defence in depth. This is implemented (`internal/adapters/fetch/ssrf.go`), not deferred — and it has never met an adversary, which is why an outside review of the implementation is still an open question in [security.md](security.md) §8.

**Data integrity is the highest-*impact* threat to the product's purpose.** If an attacker induces FirmScout to publish a wrong firmware version, a real organisation may run vulnerable firmware believing it is current. That is worse than an outage. The mitigations are the same mechanisms that make the product trustworthy in the first place: evidence retention, source authority ranking, multi-source conflict detection that never auto-resolves a disagreement between two official sources, plausibility checks, and human review for anything below threshold.

Also modelled in full: collector sandbox escape and resource exhaustion (decompression bombs, XML entity expansion, catastrophic regex backtracking), prompt injection from fetched vendor content, supply-chain compromise through a malicious collector configuration in a pull request, API key handling, tenant inventory data exposure, denial of wallet, audit and repudiation, and — added after it actually happened — the internal review surface being reached from the internet through a public first-party client that called it on a visitor's behalf ([security.md](security.md) §4.11, T-16).

**Implemented versus planned is stated honestly**, in [security.md](security.md) §5's control table. Implemented today: the SSRF guard, content-size, MIME and timeout limits, decompression-ratio limits, the RE2-only regex policy, caller-chosen cache policy so a keyed response is never publicly cacheable, an `audit_events` row on every human review decision, and the removal of the public web app's write path to the review surface. Still design: extraction deadlines, API key generation, tenant isolation, redaction, and everything that needs an AWS account. "Implemented" means the code exists and its tests pass locally — there is no deployment, so nothing here has been exercised against an adversary.

---

## 23. Bulk-scraping and API-avoidance threat model

Full document: [scraping-resilience.md](scraping-resilience.md).

**The honest starting position:** scraping cannot be prevented. Anything sent to a browser can be extracted. Any design premised on hiding data client-side is rejected outright, and JavaScript obfuscation is not a security control.

**So the goal is not prevention.** It is to protect availability, control AWS spend, preserve legitimate manual access, preserve SEO, preserve accessibility, and make the official API the path of least resistance. The last one is a product lever, not a security lever: if the API is cheap, well documented, and pleasant, building a scraper is simply worse engineering than getting a key.

Controls are layered at the edge (CloudFront, WAF managed rule groups, IP reputation, request-rate rules, Shield Standard), in the application (layered rate limits, abuse scoring, quotas, aggressive caching), and in the product. **Distinct policies apply to public HTML, public assets, internal frontend APIs, the free API, the paid API, authentication endpoints, search endpoints, and bulk endpoints.** A single global rate limit is wrong because these endpoints have entirely different cost profiles and legitimate-use patterns.

**Forbidden countermeasures, and why:**

| Forbidden | Reason |
| --- | --- |
| Serving false firmware or security data to suspected scrapers | The product's only asset is that its data is true. A user acting on poisoned data could leave a device unpatched. Non-negotiable. |
| Poisoning scraped datasets | Same reason. There is no way to guarantee the poison only reaches the scraper. |
| Hidden traps that harm accessibility | Honeypot links invisible to sighted users are followed by screen readers. |
| Depending on browser fingerprinting | Privacy-hostile, and trivially defeated by the competent scrapers it is aimed at. |
| Collecting unnecessary personal information | Contradicts the analytics stance and creates liability. |

---

## 24. Abuse risk-scoring strategy

The score is **additive and explainable**. Every enforcement action records the signals and weights that produced it, so a false positive can be explained to the affected user and appealed. A black-box score is not acceptable for a service whose users include corporate networks behind a single NAT address.

Signals include sequential product enumeration, excessive pagination depth, catalogue-wide traversal patterns, requests without the asset fetches a real browser makes, suspiciously regular intervals, distributed identical behaviour across addresses, cache-busting query parameters, high-volume expensive searches, API keys used from many networks simultaneously, and repeated authentication failures.

**No single signal is decisive.** Each one alone has a benign explanation: a documentation crawler enumerates, a slow connection produces regular intervals, a corporate proxy strips referrers, a monitoring tool polls on a schedule. The score requires corroboration across independent signal families.

Response ladder, in order of escalation: allow; allow and monitor; serve cached data; reduce rate; require authentication; apply an accessible challenge; temporarily block; permanently block only after human review. Weights are starting points to be tuned against real traffic, and are labelled as such.

---

## 25. SEO versus scraping analysis

There is a genuine, unresolvable tension: the server-rendered, crawlable pages that make FirmScout discoverable are exactly what makes it scrapable. FirmScout accepts that trade, because organic search is the primary acquisition channel (assumption P4) and a site that cannot be indexed cannot acquire users.

**Crawler identity is never trusted from the User-Agent alone**, which any scraper can forge. Verification uses published crawler IP ranges where the operator publishes them, plus reverse DNS on the client address followed by a forward DNS confirmation of the resulting hostname. An unverified claim to be a search crawler is treated as anonymous traffic, not as a crawler.

Verified crawlers get a distinct, generous policy. Unverified traffic claiming to be a crawler gets the anonymous policy, which is the correct outcome in both the honest and dishonest cases.

---

## 26. Accessibility analysis

Accessibility is treated as a constraint on the abuse controls, not as a separate feature.

- **CAPTCHAs are not the default response.** They are one rung near the top of the ladder, and only when the score is corroborated.
- **Any challenge must have an accessible alternative.** A visual-only challenge excludes users who need the data most.
- **Privacy-preserving browsers are not blocked by default.** Tor and similar are anonymous traffic, not presumed abuse.
- **Assistive technology and slow connections must not be misread as abuse.** A screen reader's traversal pattern and a slow connection's retry behaviour can superficially resemble crawling; the signal set is chosen to avoid these.
- The public pages target WCAG 2.2 AA: semantic HTML, correct heading structure, keyboard navigability, sufficient contrast, and text alternatives. Server-rendered content means the data is available without JavaScript.

---

## 27. False-positive analysis

This deserves its own section because the asymmetry is counter-intuitive.

**A false negative costs a fraction of a cent.** Some data gets scraped. FirmScout's costs rise slightly. The scraper still has a stale copy that decays daily, which is precisely why the *monitoring* is the product rather than the snapshot.

**A false positive costs a customer, silently.** A blocked engineer at a large company behind one NAT address does not file a support ticket. They conclude the site is broken and never return, and their organisation never becomes a customer. The loss is invisible in the metrics, which is what makes it dangerous.

Therefore thresholds are deliberately biased toward permissiveness, and:

- Corporate NAT, shared proxies, VPNs, university networks, and CGNAT are explicitly modelled. Per-IP limits are set assuming an address may legitimately represent hundreds of people.
- Every enforcement action is logged with its reasons and is reversible.
- There is a documented appeal path, staffed.
- The monitored metric is the **rate of enforcement actions against traffic that later authenticates successfully** — a direct proxy for false positives.

---

## 28. API-key lifecycle

Full design: [api-commercial.md](api-commercial.md), including a state diagram.

Keys are generated from a cryptographically secure source, displayed exactly once at creation, and stored only as a SHA-256 hash alongside an 8-character non-secret prefix used for identification in dashboards and logs. Rotation issues a new key with an overlap window so a deployment can migrate without downtime. Revocation is immediate and audited. Leaked-key handling covers detection through public-repository scanning, notification, and forced rotation.

**Logs and traces never contain a key**, only its prefix. This is enforced in the telemetry redaction layer, not left to individual call sites.

---

## 29. Usage metering design

Usage is recorded per request with an idempotency key, so a retried request cannot be double-counted. Recording is asynchronous relative to the response path — the customer's latency does not pay for the meter — but durable, so a crash does not silently lose billable usage.

Quota enforcement reads aggregated counters rather than scanning raw usage rows. The accuracy-versus-latency trade-off is explicit: enforcement may lag by a small window, which can allow slight overage at a quota boundary. That is accepted and documented, because the alternative (synchronous exact counting on every request) costs latency on every request to prevent a rounding error on a few.

Endpoints are **weighted**. A bulk lookup or a full-text search consumes more quota than a single product fetch, because they cost more to serve. A flat request count would let the cheapest and most expensive operations trade at the same price, which pushes consumers toward exactly the expensive patterns.

---

## 30. Billing-event design

Billing events are emitted from usage aggregation, carry idempotency keys, and are provider-agnostic by design — no billing vendor is chosen, and the events are shaped so that choosing one later is an adapter, not a redesign.

The design covers plan upgrades and downgrades mid-period, proration, and the reconciliation path between billing events and raw usage records, so that a disputed invoice can be investigated down to the individual request. The invariant is that usage records are the source of truth and billing events are a derived, replayable projection.

---

## 31. AWS cost model

Full model: [aws-cost-model.md](aws-cost-model.md).

**Methodology, stated because it is the honest part:** the model computes **consumed units** (invocations, GB-seconds, GB egress, request counts, GB-months, ACU-hours, tokens) and applies rates as **parameters**. Where a current published rate could be retrieved, it is cited with the source URL and the retrieval date. Where it could not, the rate remains a named variable rather than a guess.

**No AWS price in this repository should be treated as authoritative.** Rates change, differ by region, and are subject to commitments and free tiers. Every figure requires validation before any commercial commitment. A cost model built on invented numbers is more dangerous than one with explicit unknowns, because it invites decisions.

**What the structure of the model already tells us, independent of the rates:**

- The **irreducible monthly floor is the database.** Everything else scales to zero. Whether that floor is small or moderate depends on the RDS-versus-Aurora-Serverless-v2 choice, and Aurora's minimum-ACU floor is the specific number to check.
- **Egress and CloudWatch log ingestion are the classic surprises.** Both are metered on volume that is easy to generate accidentally.
- **NAT Gateway is avoided by architecture**, because its fixed hourly charge would otherwise dominate a small deployment.
- **AI tokens are the highest-variance line item**, which is why §33 gives them hard caps rather than monitoring.

---

## 32. Low, expected, and high-usage scenarios

Three scenarios, each fixing a value for every workload driver (monitored sources, check frequency distribution, fraction of checks returning 304, fraction that change, extraction runs, published releases per month, page views, API requests, AI escalations), with consumed units computed per service.

These are **planning assumptions, not measurements**, and are labelled as such throughout. Their value is not the totals; it is identifying which line item dominates at each scale and therefore which optimisation is worth engineering effort at that scale. Optimising the wrong line item is the most common FinOps mistake.

The structural conclusions:

| Scale | Dominant cost | Highest-leverage optimisation |
| --- | --- | --- |
| Low | Database floor | Choose the right database tier; everything else rounds to nothing |
| Expected | CDN egress and Lambda invocations | Cache hit ratio on product pages |
| High | Egress, database IOPS, AI escalations | Precomputed summaries, adaptive scheduling, AI caps |

---

## 33. AI budget policy

Full policy: [cost-controls.md](cost-controls.md); diagram: [`docs/diagrams/ai-cost-escalation.md`](../diagrams/ai-cost-escalation.md).

Hard caps at four levels: per job, per vendor per day, per tenant per month, and global daily and monthly. Model selection is by task complexity — the least expensive capable model for classification and source quality, a more capable one only for discovery and repair. Prompt size, response size, and content chunk counts are bounded. Results are cached and duplicate jobs are suppressed. Retries are capped, and the fallback after the cap is **human review, never another attempt**, because a loop of retries against a genuinely ambiguous page is how AI budgets evaporate.

**Degraded mode is a first-class operating state.** When budgets are exhausted, deterministic collection, validation, publication, and serving all continue unaffected. Only discovery of new sources and automated repair pause, and broken sources queue for human attention. The platform's core function does not depend on AI being available or affordable.

Tracked: tokens per run, cost per agent type, cost per source repair, cost per discovered product, cost per published release, AI escalation rate, AI rejection rate, and the savings attributable to deterministic processing — the last one being the metric that justifies the whole architecture.

---

## 34. Storage retention policy

Full policy: [cost-controls.md](cost-controls.md); diagram: [`docs/diagrams/storage-lifecycle.md`](../diagrams/storage-lifecycle.md).

Every data class is assigned to one of five categories, which determines its retention, tier, and deletion trigger:

| Class | Meaning | Examples |
| --- | --- | --- |
| Permanent | Never deleted | Published releases, evidence references, audit events |
| Audit-required | Retained for a defined compliance window | Publication decisions, corrections, human review outcomes, AI run records |
| Temporarily required | Needed for a bounded operational window | Recent source artifacts, collector logs, dead-letter messages |
| Reconstructable | Can be regenerated from other data | Product summaries, search indexes, aggregates |
| Disposable | Deleted aggressively | Unchanged-artifact bodies, verbose debug logs, sampled-out traces |

**Artifacts are content-addressed and deduplicated.** An unchanged source produces the same hash and stores nothing new — only a new `source_checks` row pointing at the existing artifact. Given that the overwhelming majority of checks return unchanged content, this is the single largest storage saving in the system, and it falls out of the design rather than needing a cleanup job.

---

## 35. Adaptive scheduling strategy

Full strategy: [cost-controls.md](cost-controls.md); diagram: [`docs/diagrams/adaptive-scheduling.md`](../diagrams/adaptive-scheduling.md).

Checking every source at the same frequency is the most expensive possible policy and also the least useful one. The next interval is a pure function of observed history:

**Inputs:** historical publication cadence for the product, product lifecycle state, previous check outcome, consecutive unchanged count, consecutive failures, source health state, rate-limit responses, product popularity from analytics, security criticality, and any paid freshness commitment covering the product.

**Behaviours:** a source that just changed is checked more often for a while, because vendors frequently publish in clusters. A source unchanged for many consecutive checks backs off geometrically toward a ceiling. An EOL product is checked rarely. A failing source backs off with jitter and a retry ceiling. A rate-limited source honours `Retry-After` and lowers its base frequency permanently, not just for the current cycle. A paid freshness commitment sets a floor that the backoff cannot cross.

**One source result is shared across every product it covers.** A vendor catalogue page serving forty products is fetched once, and that is a forty-fold saving that no amount of per-product tuning could achieve.

All intervals are configuration, not constants, and the defaults are labelled as starting points for tuning against measured cadence.

---

## 36. Cost-containment runbook

Full runbook: [cost-controls.md](cost-controls.md); diagram: [`docs/diagrams/cost-containment-incident.md`](../diagrams/cost-containment-incident.md).

Written to be followed by a tired person at an inconvenient hour. Detection signals (budget alarms, anomalous request volume, AI spend rate, egress rate), triage to distinguish abuse from a bug from legitimate growth — **which matters, because the correct response to each is different, and throttling real growth is the worst outcome** — then a graduated response ladder: tighten WAF rate rules, protect expensive endpoints, raise cache TTLs, reduce collector frequency, suspend AI spend, enter degraded mode. Then recovery, and a post-incident review that feeds the thresholds back into configuration.

---

## 37. Scaling thresholds

Every "we will do X later" in this blueprint has a measurable trigger, so the decision is made by data rather than by anxiety.

| Measurement | Threshold | Change it triggers |
| --- | --- | --- |
| Sustained job enqueue rate | > ~10/s | PostgreSQL queue → SQS ([ADR-0015](../adr/0015-job-queue-port.md)) |
| Concurrent API instances | > ~4 | In-process rate limiting → shared store |
| p95 product search latency | > ~200 ms | PostgreSQL FTS → dedicated search index |
| Sustained API request rate | Where Fargate undercuts Lambda | Lambda → ECS Fargate ([ADR-0010](../adr/0010-aws-runtime.md)) |
| `jobs` table row count | > ~5M | Partitioning or archival |
| `source_checks` / `source_artifacts` volume | Monthly growth exceeding retention budget | Monthly partitioning |
| Database size or read load | Read saturation | Read replica for the public API |
| Observability storage | Exceeds hosting-cost benefit | Reassess self-hosted versus managed |

---

## 38. Open-source governance

Full document: [GOVERNANCE.md](../../GOVERNANCE.md).

Roles from user to contributor to collector maintainer to maintainer. Lazy consensus for routine changes, an ADR for architectural ones, a maintainer vote for contested ones. A documented path to becoming a maintainer and to stepping down gracefully.

**The separation between the project and the hosted service is stated explicitly**, including which decisions the hosted service does not get to impose on the project. A single-vendor open-source project with a commercial arm has to be honest about this boundary in writing, before it is tested by a conflict rather than during one.

Also covered: dataset stewardship and who may approve corrections, and a conflict-of-interest note for contributors employed by catalogued vendors — a realistic situation for this project, since the people who know a vendor's firmware best often work there.

---

## 39. Code licensing recommendation

**Recommendation: Apache License 2.0.** Reasoning in §3.7 and [ADR-0009](../adr/0009-code-and-data-licensing.md); details in [licensing.md](licensing.md).

The permissive licence maximises the two things FirmScout actually needs — collector contributions from engineers at enterprises whose legal departments block AGPL, and self-hosted adoption as a distribution channel — while the explicit patent grant gives more protection than MIT at no practical cost. Contribution is under DCO sign-off rather than a CLA, accepting that this forecloses a future relicensing in exchange for a lower contribution barrier.

The name **FirmScout** is protected by a trademark policy rather than by the code licence, which is the appropriate instrument.

---

## 40. Dataset licensing recommendation

**Recommendation: CC BY 4.0 for delayed public snapshots; separate contractual FirmScout Data Terms for the live feed and enriched fields.** Reasoning in §3.8.

CC BY 4.0 is chosen over the Open Data Commons licences because CC 4.0 explicitly addresses the EU *sui generis* database right, and over ODbL because ODbL's share-alike propagates into a customer's internal asset database, which is commercially unacceptable and practically unenforceable.

**This entire section requires qualified legal review before launch.** The specific questions are itemised in [licensing.md](licensing.md), and include: whether the catalogue attracts database rights in each target jurisdiction; whether the selection and arrangement is protectable where the individual facts are not; the permissibility of collecting from sources whose robots.txt disallows it; the use of undocumented vendor update APIs; acceptable evidence-excerpt length; handling of vendor delisting requests; and the hosted service and API terms.

---

## 41. MVP roadmap

Full roadmap: [mvp-roadmap.md](mvp-roadmap.md).

**Pilot vendors, one per source archetype**, chosen so that each teaches the platform something structurally different:

| Vendor | Archetype | What it teaches | Status |
| --- | --- | --- | --- |
| **MikroTik** | Structured HTML plus a cheap plain-text version endpoint | The whole happy path, and targeted-section hashing on a page with no ETag | ✅ first vertical slice |
| **Poly (HP)** | PDF release notes, and a real relocation problem — `docs.poly.com` now redirects to `support.hp.com` after the acquisition | PDF extraction and the source-relocation and repair path | Phase 2 |
| **Fortinet** | Difficult portal: authenticated downloads, but a public PSIRT RSS feed | Legitimate `authentication_required` handling, and advisory ingestion separate from release ingestion | Phase 2 |
| **Dell** | Technically ideal XML catalogue, **blocked by `Disallow: /`** | That compliance is evaluated before collection, and that the answer is sometimes no | Registered, **disabled** |

Substitute for the clean-API archetype: **Ubiquiti**'s JSON firmware endpoint, pending terms review.

Phases: 0 blueprint and skeleton; 1 vertical slice; 2 the remaining collector archetypes and a review queue; 3 API keys, quotas, metering, usage dashboard; 4 advisories and lifecycle; 5 AWS deployment; 6 AI agents. Each with exit criteria.

---

## 42. First sprint backlog

Full backlog: [first-sprint-backlog.md](first-sprint-backlog.md). It delivers the twenty steps of the vertical slice as ordered, individually acceptance-tested items with explicit dependencies, so the sprint can be executed without re-deriving the order.

---

## 43. Major risks and mitigations

Full register: [risks.md](risks.md). The five that most deserve attention:

| Risk | Why it is the one that matters | Early-warning signal |
| --- | --- | --- |
| **Wrong data causes real harm** | A user leaves a device on vulnerable firmware because FirmScout said it was current. This is the risk that would end the project's credibility permanently. | Any correction where the published value was wrong rather than merely stale |
| **Sources break faster than they can be repaired** | The maintenance burden is the whole business. If breakage outpaces repair, the catalogue decays into a liability. | Ratio of sources entering `broken` to sources returning to `active`, trending above 1 |
| **A vendor sends a legal demand** | Already partially materialised: Dell's robots policy blocks a technically ideal source. | Any vendor contact that is not a routine question |
| **Nobody buys because the free tier suffices** | The free tier is deliberately generous. If history depth and automation are not the right paywall line, the model fails. | Free API keys growing while paid conversion stays flat |
| **Founder is a single point of failure** | One person holds the domain knowledge, the dataset judgement, and the maintainer role. | Bus factor of 1 persisting past the first six months |

---

## 44. Definition of done

Full definition: [definition-of-done.md](definition-of-done.md). Per level — code change, collector, new vendor, data correction, documentation change, architectural decision, release — with the literal commands that must pass and the documentation obligations, including the rule that **a diagram which no longer matches the code is a defect**, not a stale document.

---

## 45. ADR list

Index: [`docs/adr/README.md`](../adr/README.md). Twenty-three ADRs, 0001 through 0023, listed with their decisions in §6. Two require qualified legal review: [0009](../adr/0009-code-and-data-licensing.md) and [0018](../adr/0018-source-compliance-policy.md).

---

## 46. Implementation sequence

Full sequence with reasoning: [implementation-sequence.md](implementation-sequence.md).

Build from the inside out, because each step's tests must not depend on the step after it:

1. **Domain value objects and entities** — `PartialDate`, `VersionString`, `ReleaseType`, the state machines. Verified by pure unit tests in milliseconds. Building anything else first means writing tests that need a database.
2. **Application ports and use cases against fakes** — the pipeline is provably correct before any infrastructure exists.
3. **PostgreSQL schema and repositories** — the first real adapter, with the contract tests the fakes already defined.
4. **Guarded fetcher** — the SSRF guard belongs here, before any URL is ever fetched in anger.
5. **Normalisation and section hashing** — verified against recorded fixtures.
6. **Configuration-driven collector engine** — fixture tests only.
7. **Job queue adapter** — the pipeline becomes asynchronous.
8. **CLI** — the first way to run the pipeline end to end without an HTTP server.
9. **HTTP API** — with the presenters that enforce date-precision rendering.
10. **Telemetry** — instrument what exists rather than instrumenting speculatively.
11. **Web app** — a client of a working API.
12. **Docker Compose and the observability stack** — the reference environment.

**The instruction that governs the whole sequence: do not implement the entire roadmap in one pass.** The vertical slice proves the architecture. Everything after it is informed by what the slice teaches.

---

## 47. First vertical-slice scaffold

The slice is MikroTik RouterOS, chosen because it exercises the complete pipeline against a real, permissive, well-structured source without needing PDF parsing or authentication.

**Measured source facts (2026-09-03), which the slice is built against:**

| Source | Observation |
| --- | --- |
| `upgrade.mikrotik.com/routeros/NEWESTa7.stable` | `text/plain`, body `7.24.2 1788429434` (version and epoch), serves ETag and Last-Modified. The cheapest possible watcher. |
| `mikrotik.com/download/changelogs` | Server-rendered HTML, ~409 KB, 46 entries under `div.changelog-header`, each carrying version, a channel badge, and an ISO date. **No ETag, `cache-control: private`** — so whole-page hashing would report a change on every check, and targeted section hashing is required. |
| `mikrotik.com/robots.txt` | `Disallow:` empty — all paths permitted. |

The twenty steps of the slice run from the vendor record through to a distributed trace, verified by fixture-based tests that never touch a live site. What the slice implements versus what remains conceptual is recorded in the architecture consistency report.

**No production data is fabricated.** The registry contains real vendor, product, and source records; releases appear only if a real check is executed, and the fixtures are trimmed captures with recorded provenance.

---

## 48. Architecture consistency report

Written after the slice was built: [consistency-report.md](consistency-report.md).

It records which gates were verified by execution and which were not, which code module implements each of the 29 diagrams, which components are implemented versus conceptual, the state of every security, observability and cost control, the decisions still requiring legal or product input, the known limitations, and the recommended next step.

It also lists the ten defects that executing the system found and reading it would not have — a failed migration, an artifact stored in the wrong form, a foreign key with nothing behind it, a product hint compared against the wrong shape of string. That section is the argument for the verification discipline rather than for the architecture.

Its purpose is to make the gap between this blueprint and the code **visible and specific** rather than discovered later by someone who trusted a diagram.

---

## Cost review

The brief asks for this review explicitly, alongside the Clean Architecture review in §7.

**Main AWS cost drivers**, in the order they will actually appear: the database floor (the only always-on component); CloudFront egress; Lambda invocations and GB-seconds; CloudWatch log ingestion (routinely underestimated); S3 storage growth for artifacts; and WAF request charges. NAT Gateway is absent by design and must stay absent.

**Main AI cost drivers:** the Repair Agent (triggered by breakage, which is bursty and correlated — a vendor redesigning a site can break dozens of sources at once); the Discovery Agent during bootstrap (a one-off but potentially large spend); and the Validation Agent if its escalation threshold is set too low. The mitigation for all three is the same: hard caps, caching, duplicate suppression, and human review as the fallback rather than retry.

**Lowest-cost viable MVP:** a single small PostgreSQL instance, the API and worker on Lambda, static assets on S3 behind CloudFront, and no observability stack in AWS at all — telemetry to CloudWatch only, with the full Grafana stack reserved for local development. This sacrifices trace-level debugging in production, which is an acceptable trade until there is production traffic worth debugging.

**Scales to zero:** API, worker, web, all queues, all AI. **Always on:** the database, and Route 53 hosted zones. That is the whole floor.

**Financial risk specific to this product:** denial of wallet through the public site. FirmScout deliberately serves rich, cacheable, crawlable pages to anonymous users, which is exactly the surface an attacker uses to generate cost. The mitigations are the cache-hit ratio (a CDN hit costs a fraction of an origin request), the WAF rate rules, and AWS Budgets with alarms configured on day one rather than after the first surprising invoice.

---

## Observability review

**Telemetry sources:** the API, the worker, the CLI, the web app, PostgreSQL, and the AWS infrastructure layer. All application signals flow through OpenTelemetry; infrastructure signals that OTel cannot see come from CloudWatch.

**The signal that matters most and is easiest to miss:** *publication rate falling to zero*. Every other failure announces itself with an error. A pipeline that checks sources successfully, finds no changes, and publishes nothing looks perfectly healthy on every conventional dashboard — and is indistinguishable from a silently broken extractor. This gets an alert of its own.

**Privacy:** the redaction rules in the collector are not optional configuration; they are the mechanism preventing API keys, credentials, and customer inventory contents from reaching Loki. Redaction happens at the collector rather than at each call site, because a call site can be forgotten.

**Retention:** metrics longest (they are small and their value is trend), logs medium, traces shortest with tail-based sampling that always keeps errors and slow requests.

**Alerting strategy:** a small number of alerts that page, tied to user-visible harm; a larger number that create tickets. An alert that fires routinely and is routinely ignored is worse than no alert, because it trains the team to ignore the channel.

**Expected operational cost:** locally, free. In AWS for the MVP, the recommendation is to defer the self-hosted stack entirely and use CloudWatch, revisiting once there is traffic whose debugging justifies the hosting cost. The instrumentation is vendor-neutral precisely so that this choice can be changed without touching application code.
