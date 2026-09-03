# Overview

> Read this first. It orients a new contributor in one page, then tells you where the deeper documents live. It expands [blueprint.md](blueprint.md) §1, §8.3, and §10; it does not re-decide anything the blueprint has already settled, and if anything here appears to contradict it, the blueprint wins.

## What FirmScout does

FirmScout is an open catalogue and continuous monitor for the version metadata of firmware, BIOS, BMC images, device drivers, embedded operating systems, appliance software, and management-platform releases. It answers one question, reliably and with evidence: **what is the current version of this product, and when did it ship?**

It is not a firmware mirror, not a patch manager, not a vulnerability scanner, and not a network-scanning asset-discovery tool — see blueprint §4.2 for the full boundary list. It is a catalogue with provenance: every fact it publishes carries a source URL, a retrieval timestamp, a content hash, and (where licensing allows) an excerpt.

Per blueprint §1, the product is three things at once, and it helps to keep all three in view because they pull in different directions on any given design decision: a **public website** that is genuinely useful for free and indexable by search engines; an **open-source platform** anyone can self-host, contribute collectors to, and audit end to end; and a **hosted service** that sells automation, freshness, history, and integration to organisations that want the data in their own pipelines rather than in a browser tab. The technical differentiator across all three is not the database schema — it is a discovery and maintenance engine whose cost per monitored source is low enough that monitoring tens of thousands of sources is economically boring, which is the thread every document in this set pulls on from a different angle.

## One fact, end to end: MikroTik RouterOS

The clearest way to understand the system is to follow a single new firmware version from the vendor's server to a user's screen. FirmScout's first vertical slice uses MikroTik RouterOS, so the example below uses real, measured endpoints rather than a hypothetical.

MikroTik exposes two sources for RouterOS, registered in `dataset/sources/mikrotik/`:

1. **A plain-text version endpoint**: `https://upgrade.mikrotik.com/routeros/NEWESTa7.stable`. A `GET` returns a body like `7.24.2 1788429434` — a version string and a Unix epoch, nothing else — and the response carries both an `ETag` and a `Last-Modified` header. This is the cheapest possible watcher: a conditional `GET` that costs a few hundred bytes when nothing has changed.
2. **A changelog page**: `https://mikrotik.com/download/changelogs`, a server-rendered HTML page (409 KB) listing entries under `div.changelog-header`, each carrying a version (`span.font-bold`), a channel badge (Long-term / Stable / Testing / Development), and an ISO release date (`YYYY-MM-DD`). This page has no `ETag` and is served `Cache-Control: private`, so it cannot be watched by conditional `GET` — see [update-pipeline.md](update-pipeline.md) for how a *targeted section hash* solves this.

The walk from vendor publication to a user seeing the fact:

1. **Schedule.** The worker's scheduler wakes a `source.check.requested` job for each of the two sources, on an interval derived from each source's own change history (`scheduling.NextInterval` in `internal/domain`).
2. **Watch and change-detect.** For the version endpoint, `CheckSource` issues a conditional `GET` with the prior `ETag`. MikroTik publishes RouterOS 7.24.3: the server returns `200` with a new `ETag` and body `7.24.3 <epoch>` — outcome `changed`. For the changelog page, the same use case fetches the full HTML, strips volatile content, hashes only the `div.changelog-header` section, and compares it to the last stored hash — also `changed`, because a new entry appeared.
3. **Store artifact.** The raw response bytes are written to the `ArtifactStore`, content-addressed by hash, so extraction is reproducible and auditable independent of whether MikroTik's server is reachable later.
4. **Extract.** The `mikrotik.changelogs` collector (a config-driven `html_selectors` extractor, see `collectors/config/mikrotik/changelogs.yaml`) parses the stored artifact and emits a `CandidateRelease{RawVersion: "7.24.3", Channel: "stable", ReleaseDate: 2026-09-10, ReleaseDatePrecision: exact_day, ...}`. The collector has no database handle — it is structurally incapable of writing a release.
5. **Normalise and validate.** `ValidateCandidateRelease` runs the ten deterministic gates from blueprint §16: product resolves to `mikrotik-routeros`, the version is non-empty, the source is active and compliant, this is not a duplicate of an already-published release, the date precision is valid, the version transition from `7.24.2` is plausible, evidence is present, confidence clears the auto-publish threshold for an official source, applicability is consistent, and (if a second active source existed) sources agree.
6. **Publish.** `PublishRelease` opens one `UnitOfWork` transaction: insert the `releases` row, insert the `release_product_mappings` row, insert `evidence`, transition the candidate to `published`, refresh `product_summaries`, write an `audit_events` row. The event `release.published` is dispatched.
7. **Serve.** The public API's `GET /api/v1/products/mikrotik-routeros/latest` and the equivalent Next.js product page now read the refreshed `product_summaries` row and return `7.24.3`, dated `2026-09-10` with `exact_day` precision, linked to `mikrotik.com/download/changelogs`, with a `lastVerifiedAt` timestamp — the fact a user sees, traceable in one hop back to the artifact that produced it.

No step in this path required a large language model. That is deliberate: see [ai-agents.md](ai-agents.md) for when and how AI is allowed to participate at all.

MikroTik is one of four pilot vendors, each chosen to exercise a different profile of the problem the catalogue exists to solve — the full, current compliance status of each lives in `DATA_SOURCES.md`, but the shape is worth knowing here:

| Vendor | Profile | Format |
| --- | --- | --- |
| MikroTik | Structured HTML changelog, the vertical slice above | Plain-text endpoint + server-rendered HTML |
| Dell | "Clean API" pilot, currently blocked by `robots.txt: Disallow: /` | Gzipped XML catalogue |
| Poly / HP | Source relocation after acquisition | PDF release notes behind a JS shell |
| Fortinet | Difficult portal; public advisory feed, authenticated downloads | RSS 2.0 PSIRT feed |

## Design principles a new contributor should internalize

These are the operating principles from blueprint §1, restated with why they matter day to day rather than just what they are:

- **Deterministic by default, AI only by escalation.** If you are adding a new vendor and reaching for a prompt before you have tried a conditional `GET`, an `ETag`, or a config-driven selector, you are working against the grain of this codebase. AI is the last resort, not the first idea.
- **Evidence first.** If you write code that publishes a fact without a source URL, a retrieval timestamp, a content hash, and an excerpt attached, that code will not pass review, regardless of how correct the fact turns out to be. Correctness without provenance is not trusted here.
- **Historical data is immutable.** There is no `UPDATE releases SET version = ...` anywhere in this codebase, and there should never be one. A correction is a new row that references the old one.
- **No invented precision.** A release date you don't know to the day is stored as month- or year-precision, never padded out to a plausible-looking day. `PartialDate`'s type system makes this a compile error, not a style guideline.
- **No assumed semantic versioning.** Do not sort version strings to find "the latest." "Latest" is derived from release date, first-observed timestamp, and channel — see [update-pipeline.md](update-pipeline.md) for exactly how.
- **Collectors cannot publish.** A `Collector` implementation has no repository handle. If you find yourself wiring one up with database access to "save time," stop — that access is structurally supposed to be impossible, not merely discouraged.
- **Low idle cost.** A design that requires a new always-on service to solve a problem the MVP doesn't have yet (Redis for rate limiting, OpenSearch for search, SQS for the MVP's job volume) should be treated with suspicion until a measured threshold — stated in the relevant ADR — is actually crossed.
- **Anonymous and paid access share one API.** There is no separate "free tier code path" that quietly gets less engineering attention. `apps/web` is a client of the same `apps/api` a Professional-tier consumer calls; the free/paid line is drawn on volume, endpoints, and history depth — never on whether the underlying fact is true (blueprint §5).
- **Compliance is evaluated before collection, never after.** A source with a disqualifying `robots_policy_status` or `terms_review_status` stays registered and visibly disabled — it is never quietly skipped, and it is never collected from "just this once" while the review is pending. See [system-context.md](system-context.md)'s compliance boundary section.

## Where things stand today

Not every component described in this documentation set exists yet. The [component catalogue](#component-catalogue) below marks each row's MVP status explicitly, and it is worth reading that column literally: a "✅" row is built and exercised by the first vertical slice (MikroTik RouterOS end to end, as walked through above); a "❌" row is specified — ports, schemas, and interfaces exist — but has no running implementation. The clearest example is the AI agent adapters: [ai-agents.md](ai-agents.md) describes five agents in detail, and none of them make a real model call in the MVP. The clearest infrastructure example is [aws-deployment.md](aws-deployment.md)'s topology: nothing described there is deployed, and that document says so without hedging. Treat any claim in this documentation set that isn't marked "MVP" or "built" as a design target, not a status report.

## Runtime components

FirmScout ships as one Go module with three binaries, a Next.js frontend, and one stateful dependency:

| Component | What it is |
| --- | --- |
| `apps/api` | The public HTTP API server. Stateless; horizontally replaceable. |
| `apps/worker` | The scheduler loop and job runner: watches sources, runs collectors, runs validation, publishes releases. |
| `apps/cli` | `firmscout migrate`, `registry sync`, `check-source`, `apikey create` — operational and bootstrap commands. |
| `apps/web` | The public Next.js site: search, vendor pages, product pages. A client of the same API consumers use. |
| PostgreSQL | The only stateful dependency: registry sync target, fact store, job queue (`SKIP LOCKED`), search (`tsvector`/`pg_trgm`), quotas. |
| Artifact store | Content-addressed storage for raw fetched artifacts — filesystem/PostgreSQL large object locally, S3 on AWS. |
| Observability stack | OpenTelemetry SDK → OTLP → OTel Collector → Prometheus, Loki, Tempo → Grafana. Identical locally and in AWS. |

All four binaries share `internal/domain` and `internal/application`; only `internal/platform` wires concrete adapters into each binary's entry point. See [clean-architecture.md](clean-architecture.md) for the full layering rule.

Two things are worth internalising about this list before going further. First, three of the four binaries — `apps/api`, `apps/worker`, `apps/cli` — are built from the *same* Go module and share every layer below `internal/platform`; they differ only in which use cases their `main.go` wires up and which trigger drives them (an HTTP request, a scheduler tick, a CLI invocation). Second, PostgreSQL is deliberately the only thing on this list a contributor must actually run to get a working system locally — no Redis, no OpenSearch, no message broker — which is why `docker compose up` with just `postgres`, `api`, `worker`, and `web` is a complete, working FirmScout, and the observability profile is opt-in on top of that.

## Cost philosophy in one paragraph

FirmScout's cost model is a functional requirement, not an afterthought (blueprint ADR-0013): every component in the catalogue below states its own cost driver so that "is this expensive" is answerable per-component rather than as a vague worry about the whole system. The recurring pattern across the catalogue is that the *expensive* components — egress-heavy fetching, AI tokens, always-on compute — are also the ones gated behind the most deliberate escalation logic (the watcher ladder in [update-pipeline.md](update-pipeline.md), the AI escalation rules in [ai-agents.md](ai-agents.md)), while the *cheap* components (conditional `GET`s, config-driven extraction, a Lambda that scales to zero) are what run by default, thousands of times a day, without anyone having to think about it.

## Component catalogue

<a id="component-catalogue"></a>

Blueprint §8.3 defers its full per-component table to this section. Each row states the component's responsibility, its Clean Architecture layer, why it exists, its cost impact, whether it ships in the MVP, how it is observed, its main security risk, and the condition under which it would be scaled or replaced. "MVP" means: built and exercised by the first vertical slice (MikroTik RouterOS) or immediately required to support it; "Planned" means specified but not yet built.

| Component | Responsibility | Layer | Why needed | Cost impact | MVP? | Observed via | Main security risk | Scale/replace when |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| HTTP API (`apps/api`, `internal/adapters/httpapi`) | Serve public and keyed reads: search, vendors, products, releases, usage | Adapter | The one interface both anonymous visitors and paying consumers share (§12) | Lambda invocations + egress | ✅ | OTel traces/metrics per route, request-id logs | Enumeration, quota evasion, injection via query params | Sustained load pushes p95 latency past target → provisioned concurrency, then Fargate (§3.4) |
| Worker / scheduler (`apps/worker`) | Drive the scheduling loop that turns source history into the next check time | Adapter + Application | Deterministic cadence independent of manual triggering | Lambda/EventBridge invocations, mostly idle between checks | ✅ | Scheduler tick metrics, per-source next-check gauge | A scheduling bug causing a check storm against one vendor | Scheduling logic proven correct at scale → no planned replacement, only tuning |
| Job queue (`internal/adapters/postgres` queue, `JobQueue` port) | Durable, deduplicated background work: checks, extraction, validation | Adapter | Decouples scheduling from execution; survives worker restarts | Database IOPS | ✅ | Queue depth, dequeue latency, dead-letter count | Poison messages causing retry storms | Sustained >10 jobs/s or fan-out to independent consumers → SQS adapter (ADR-0015) |
| Fetcher (`internal/adapters/fetch`) | Guarded, conditional HTTP retrieval: ETag/Last-Modified, size/MIME limits, SSRF guard, robots.txt, `Retry-After` | Adapter | The only component allowed to reach manufacturer networks | Egress, per-request compute | ✅ | Fetch outcome counters, latency histograms, SSRF-block counter | **SSRF** — an attacker-influenced source URL reaching internal/metadata endpoints | Never replaced; hardened in place |
| Normaliser (`internal/adapters/normalize`) | Strip volatile content, hash a targeted section or the full body | Adapter | Makes change detection stable against non-content noise (ads, tokens, nav) | CPU on changed content only | ✅ | Hash-mismatch counters, normalisation duration | A normaliser that strips too little produces false `changed` outcomes, wasting extraction budget | Add PDF/XML normalisers when new source types are onboarded |
| Config-driven collector engine (`internal/adapters/collectors`, `html_selectors`, `text_regex`) | Turn a stored artifact into `[]CandidateRelease` per a YAML config | Adapter | Reviewable by non-programmers; no code deploy to add a vendor | CPU on changed content only | ✅ | Extraction success/failure counters, candidates-per-run | Malicious HTML causing pathological parsing (ReDoS, huge DOM) | Config cannot express the source's structure → code collector |
| Code collector runtime (`collectors/vendors`, `collectors/sdk`) | Custom `Collector` implementations for sources config engines cannot express | Adapter | Escape hatch for irregular sources (PDFs, JS shells, exotic APIs) | Development and review time, not runtime cost | ❌ (planned; empty in MVP) | Same collector metrics as config engines | A code collector bypassing the SDK's mandatory conditional-GET wrapper | Grows as vendor coverage grows past what config engines cover |
| Candidate validator (`ValidateCandidateRelease` use case) | Run the ten deterministic gates (blueprint §16) before anything can publish | Application | The chokepoint that prevents plausible-looking wrong data from reaching users | Negligible CPU | ✅ | Gate pass/fail counters by gate number | A gate with a logic bug that lets bad data through silently | Never replaced; gates are added, not removed |
| Publisher (`PublishRelease` use case) | Atomically insert a release, mappings, evidence, transition the candidate, refresh the summary, emit an audit event | Application | The only path by which a fact becomes public (§7.5) | Database write + summary refresh | ✅ | Publish latency, publish count, `release.published` event volume | A transaction left partially applied on crash (mitigated by `UnitOfWork` atomicity) | Never replaced |
| Product summary projection (`product_summaries`) | Precomputed, denormalised row the API and site actually read | Application + DB | Avoids a multi-table join on every page view | Storage + refresh CPU on publish | ✅ | Refresh duration, staleness gauge (time since last refresh vs. last publish) | Stale data served after a publish if refresh silently fails | Refresh cost grows unacceptably → materialised view with scheduled refresh |
| Search (`tsvector` + `pg_trgm` over `product_summaries`) | Typo-tolerant product/vendor search | Adapter | Free-tier and paid search both need fuzzy matching, not exact match only | Index storage, query CPU | ✅ | p95 query latency, zero-result-rate | Query injection if input isn't parameterised (mitigated by sqlc) | p95 > ~200ms at realistic catalogue size → OpenSearch (§3.10) |
| API key and quota enforcement (`APIKeyRepository`, in-process token bucket) | Authenticate keyed requests, enforce per-tier rate and volume limits | Adapter + Application | Distinguishes anonymous, free-key, Professional, Enterprise access (§5) | Negligible CPU, DB reads for quota state | ✅ | 429 rate, quota-exhaustion events | Key leakage granting a higher tier than purchased | >~4 concurrent API instances or a contract needing exact per-second limits → Redis (§3.9) |
| Usage metering (`UsageRecorder`, `usage_records`) | Record billable API calls per key | Adapter | Billing and plan-conversion analytics depend on it (P3) | DB write per keyed request | ✅ | Usage-record write failures (must never silently drop) | Under-recording usage undercharges; over-recording overcharges — both are integrity risks | Volume requiring batched/async writes → buffered metering pipeline |
| Artifact store (`ArtifactStore` port, filesystem/PG large object → S3) | Durable, content-addressed storage of raw fetched bytes | Adapter | Extraction must be reproducible from a stored artifact, not a live re-fetch | Storage (grows with monitored sources) | ✅ | Store/read latency, storage volume gauge | Artifacts containing vendor-supplied HTML/scripts stored but never executed | Storage volume or egress cost crosses a threshold → lifecycle policy (see `storage-lifecycle` diagram) |
| Registry sync (`firmscout registry sync`, `dataset/`) | Load Git-versioned vendor/product/source/collector-config YAML into PostgreSQL | Adapter (CLI) | Curated intent gets code review; observed facts get a query engine (§3.3) | Runs on deploy/CI, negligible | ✅ | Sync diff counts, schema-validation failures | A hand-edited registry row bypassing Git review (mitigated by `managed_by = 'registry'`) | Never replaced; the hybrid model is the design |
| AI agent adapters (`internal/adapters/ai`, `agents/*`) | Discovery, Repair, Validation, Classification, Source Quality escalation | Adapter | Handles the minority of sources/cases deterministic methods can't (§ai-agents.md) | **Tokens — highest-variance cost in the system** | ❌ (ports, schemas, fakes only) | Per-run cost/token counters, budget-exhaustion events | Prompt injection from fetched vendor content | Escalation rate and False-positive rate measured and acceptable → real provider adapter behind the existing port |
| OTel pipeline (`internal/adapters/telemetry`, OTel Collector, Prometheus, Loki, Tempo, Grafana) | Traces, metrics, structured logs, uniformly, locally and in AWS | Adapter + Infra | Debuggability without vendor lock-in (§ADR-0011) | Storage, cardinality-driven | ✅ locally | Self-monitoring: Grafana dashboards, collector health | Secrets or artifact excerpts leaking into logs | Operating cost of self-hosting exceeds a managed service → managed OTel backend |
| Next.js web app (`apps/web`) | Public site: search, vendor pages, product pages | Adapter (consumer of the API) | Free, SEO-indexed, genuinely useful without a key (§5) | Lambda (OpenNext) invocations, CDN egress | ✅ | Web vitals, build-time OTel where applicable | XSS via unsanitised vendor-sourced excerpt rendering | Traffic pattern justifies a dedicated CDN-only static path | 
| CDN and WAF (CloudFront, AWS WAF, Shield Standard) | Edge caching, TLS termination, volumetric abuse absorption | Infra (AWS-only) | Absorbs bulk scraping and DDoS before it costs compute (O1) | CDN egress, WAF rule evaluation | ✅ in AWS deployment, N/A locally | CloudFront access logs, WAF blocked-request metrics | Misconfigured cache serving stale or private data | Never replaced; rules tuned as abuse patterns are observed |

Row count: **19**.

## Glossary of core concepts

A handful of terms carry precise, load-bearing meaning throughout this documentation set. Getting them right early saves a lot of confusion later:

- **Vendor** — the manufacturer (MikroTik, Dell, Fortinet, Poly/HP, Ubiquiti). Owns product families and, indirectly, the compliance status of its sources.
- **Product** — a specific thing users track the version of (RouterOS, a specific server model's BIOS). Has an immutable slug used as its public URL and API identifier.
- **Source** — a monitored location: a URL plus a watcher mechanism plus a compliance status. A product can have more than one source; a source belongs to exactly one vendor.
- **Candidate (`CandidateRelease`)** — an extracted, not-yet-trusted observation, moving through its own state machine. Nothing a candidate carries is public until it clears validation.
- **Release** — a published, immutable fact with evidence. Once published, a release row is never mutated — only superseded, withdrawn, or corrected by new rows.
- **Evidence** — the source URL, retrieval timestamp, content hash, excerpt, collector version, and discovery method backing a specific candidate or release. The thing that makes "why do you say this?" answerable.
- **Artifact** — the raw bytes fetched from a source, stored content-addressed, independent of whether extraction from it succeeded.
- **Collector** — a component (config-driven or code-based) that turns a `(Source, Artifact)` pair into `[]CandidateRelease`, with no ability to write to the database.
- **Review item (`review_items`)** — a candidate, correction, or agent proposal a deterministic gate could not resolve on its own, waiting for a maintainer's decision. Everything that reaches `review_items` eventually flows through the same publish/reject use case a fully automated pass would use.
- **Registry** — the Git-versioned, YAML-authored curated intent (`dataset/`, `collectors/config/`) that `firmscout registry sync` loads into PostgreSQL. Distinct from the observed facts (releases, evidence, checks) that only ever live in PostgreSQL — see blueprint §3.3 for why the split exists.
- **Product summary (`product_summaries`)** — the precomputed, denormalised row the public site and API actually read on every request, refreshed by `PublishRelease` so no page view pays the cost of a multi-table join.

## Getting a local environment running

This document does not replace `CONTRIBUTING.md`, but the short version, so the rest of this documentation set makes sense against something real: `docker compose up` in `infrastructure/docker/` brings up `postgres`, `apps/api`, `apps/worker`, and `apps/web` as containers running the same binaries described above; an `observability` Compose profile adds the OTel Collector, Prometheus, Loki, Tempo, and Grafana on top, provisioned with a starter "Platform overview" dashboard. `firmscout migrate up` and `firmscout registry sync` (both `apps/cli` subcommands) bring the database schema and the MikroTik pilot registry data up to date before the API has anything to serve. From there, `firmscout check-source --id <mikrotik-source-id>` runs the walkthrough at the top of this document against the real MikroTik endpoints, end to end, on your own machine.

## How to read the rest of the documentation

Start here, then go deeper along whichever axis matches your question:

- **"How is the code organised, and what stops it from becoming a mess?"** → [clean-architecture.md](clean-architecture.md) — layers, ports, transaction ownership, invariants, testing.
- **"Who talks to FirmScout, and what happens when they're unreachable?"** → [system-context.md](system-context.md) — external actors, trust levels, degradation behaviour, the compliance boundary.
- **"How does a new firmware version actually get from a vendor to the database?"** → [update-pipeline.md](update-pipeline.md) — the full pipeline, watcher mechanisms, validation gates, publication mechanics, idempotency.
- **"Where does AI fit, and how is it kept from making things up?"** → [ai-agents.md](ai-agents.md) — the five agents, their contracts, cost controls, injection defence.
- **"How does this run in AWS, and what does it cost to be idle?"** → [aws-deployment.md](aws-deployment.md) — the Lambda-first topology, network boundary, deployment and rollback, disaster recovery.

For decisions and their rationale rather than mechanism, the [blueprint](blueprint.md) is authoritative and the [ADRs](../adr/) record individual decisions with alternatives considered. For visual references, [`docs/diagrams/`](../diagrams/) holds one Mermaid diagram per concern, each with its own assumptions and failure modes. `DATA_SOURCES.md` at the repository root is the practical, contributor-facing compliance policy referenced throughout this document.

A useful habit when reading any of the five deep-dive documents: each one opens with a blockquote naming exactly which blueprint section it expands and which diagram accompanies it. If something in a deep-dive document ever seems to say something different from the blueprint it names, that is a documentation bug worth filing — the blueprint is the single source of truth these six documents exist to expand, never to override.

If you read only one more document after this one, make it whichever deep dive answers the question you actually have — there is no prescribed reading order beyond this page.

One last orientation note: this document, and the five it links to, describe a system that is partly built. The [component catalogue](#component-catalogue) above and the "what is NOT built yet" sections in [ai-agents.md](ai-agents.md) and [aws-deployment.md](aws-deployment.md) are not disclaimers tacked on for legal comfort — they are load-bearing information a contributor needs before assuming a described capability is something they can rely on today.

Welcome to FirmScout.
