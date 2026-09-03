# Implementation sequence

> Cross-references [blueprint §7.2](blueprint.md#72-dependency-direction) (the inward-dependency rule this document turns into a build order) and [first-sprint-backlog.md](first-sprint-backlog.md) (the concrete backlog this document explains the ordering logic for, at a coarser grain). **This document does not authorise implementing the whole roadmap in one pass** — see §2. It states, and justifies, the recommended order to build the vertical slice's components, domain outward, so that each step rests on something already proven rather than on something assumed.

## 1. Recommended build order

### Step 1 — Domain value objects and entities

**What:** `internal/domain` — `VersionString`, `PartialDate`, `Vendor`, `Product`, `Source`, `CandidateRelease`, `Release`, `Evidence`, `ReleaseType`, the state machines, and the scheduling policy function. Zero external imports (blueprint §7.2).

**What it unblocks:** everything else. Every application-layer port and use case, every adapter's mapping code, and every API DTO ultimately reference these types — building anything else first means guessing at a shape the domain has not yet committed to.

**How it is verified:** table-driven unit tests with no dependencies at all — no database, no network, no clock (blueprint §7.8). A state machine's illegal transitions (`published → discovered`) must be unrepresentable in the type system, not merely rejected at runtime, and the tests should assert that where possible (e.g. by confirming no method exists to make the illegal transition, rather than only testing that an attempt errors).

**What would be a mistake to build before this:** anything. Building a database schema, an API handler, or a collector engine before the domain types they will eventually reference exist means those things get built against a guessed shape that the domain, once actually designed, will not match — producing rework instead of the intended "each step rests on something already proven."

### Step 2 — Application ports and use cases, with fakes

**What:** `internal/application` — the port interfaces (`VendorRepository`, `Fetcher`, `JobQueue`, `Clock`, `UnitOfWork`, etc., per blueprint §7.3) and the use cases (`CheckSource`, `ExtractCandidateRelease`, `ValidateCandidateRelease`, `PublishRelease`, `GetProduct`, and the rest), tested exclusively against in-memory fakes.

**What it unblocks:** every adapter (Step 3 onward) now has a concrete interface to implement rather than an interface designed at the same time as its first implementation — a sequencing choice that keeps the port shaped by what the *application* needs, not by what happens to be convenient for PostgreSQL or `net/http` to provide (blueprint §7.3's stated rule: "ports are defined by the application because the application decides what it needs"). It also unblocks a full, fast, network-free proof that the check → extract → validate → publish pipeline is logically correct (blueprint §7.8's fakes-based use-case test), independent of any real infrastructure existing yet.

**How it is verified:** use-case tests against fakes, including one end-to-end fakes-only test exercising the whole pipeline (first-sprint-backlog.md SLICE-06). `internal/application` importing only `internal/domain` and the standard library is itself a fact worth asserting, not just aspiring to — this is exactly what `internal/archtest` (Step 3's sibling concern) will later enforce mechanically once it exists, but the discipline should already be true by construction at this step, before the enforcement mechanism is even built.

**What would be a mistake to build before this:** a real PostgreSQL repository implementation before the port it implements is settled — this produces an adapter shaped by SQL convenience (e.g. returning raw rows instead of domain entities) that then has to be reworked once the actual port signature is finalized against what use cases need, not what the schema happens to expose.

### Step 3 — PostgreSQL schema and repositories

**What:** `database/migrations/0001_initial.sql`, `database/queries/*.sql`, `sqlc generate`, and the `internal/adapters/postgres` repository implementations of Step 2's persistence ports. This is also the natural point to stand up `internal/archtest` itself, since it is the first moment there are enough real packages across enough layers for the dependency-direction test to be meaningful.

**What it unblocks:** durable state. Every later step that needs to remember something across a process restart (the job queue, Step 4; the pipeline's actual data, Step 4 onward) depends on this existing first.

**How it is verified:** integration tests against a real PostgreSQL instance from Compose, explicitly skipped with a visible message (never silently) when `FIRMSCOUT_TEST_DATABASE_URL` is absent (blueprint §7.8); `sqlc diff` clean; the date-precision `CHECK` constraint (blueprint §11 rule 4) verified by a test that deliberately attempts to violate it and confirms the database itself rejects the insert, not just application-level validation.

**What would be a mistake to build before this:** the fetcher (Step 4) or the collector engines (Step 5) do not strictly need persistence to exist first in terms of pure code dependencies, but building them before the schema is settled risks discovering, once persistence lands, that an `Artifact` or `CandidateRelease` field the fetcher/engine produces has no home in the schema as designed — better to have the schema (and therefore the full shape of what gets stored) settled before investing in the components that produce that data.

### Step 4 — The fetcher, with its SSRF guard

**What:** `internal/adapters/fetch` — conditional HTTP, DNS resolution and validation before connecting, redirect re-validation, `max_bytes` enforcement, timeout, and `robots.txt` awareness (blueprint §8.3's Fetcher row, first-sprint-backlog.md SLICE-09).

**What it unblocks:** every collector's `Fetch` method (whether config-driven or code) is built on top of this shared component, per collector-sdk.md §2's requirement that a collector not open its own uninstrumented HTTP client. Building it now, isolated and fully tested with `httptest`, means every later collector inherits a security-reviewed fetch path rather than each collector reimplementing (and potentially getting wrong) SSRF protection independently.

**How it is verified:** `httptest`-based tests specifically targeting the guard's failure modes: a private/loopback/link-local target refused before any connection; a redirect chain that resolves to a private IP refused at the redirect hop, not only at the initial URL; a response exceeding `max_bytes` rejected, not silently truncated and returned as if complete; conditional-request headers sent correctly given a prior `FetchState`.

**What would be a mistake to build before this:** any collector engine (Step 5) before the fetcher exists — a collector engine built first would need its own throwaway HTTP handling to be testable at all, which either gets thrown away (wasted work) or, worse, survives into the real engine and becomes an unreviewed, ungueded network path that the whole point of a shared, hardened `Fetcher` was meant to prevent.

### Step 5 — Normalisation and hashing

**What:** `internal/adapters/normalize` — HTML normalisation (stripping volatile content per `normalize.strip`) and section-scoped content hashing (`normalize.section_selector`), plus the plain-text pass-through path (first-sprint-backlog.md SLICE-11).

**What it unblocks:** the `CheckSource` use case's change-detection logic (Step 6, and pipeline wiring generally) for sources that lack a reliable conditional-request signal — MikroTik's changelog page specifically, which sends `Cache-Control: private` with no `ETag` (DATA_SOURCES.md), depends on this existing.

**How it is verified:** unit tests confirming that content differing only outside the configured section produces an identical hash, and content differing inside it produces a different one — no network, no database, pure function tests matching the same purity discipline as `Extract` itself (collector-sdk.md §5), since this component sits directly upstream of it in the pipeline.

**What would be a mistake to build before this:** wiring the full `CheckSource` use case (Step 6/pipeline wiring) before this exists — it would either have to fake the hashing behaviour or skip the MikroTik changelog source's actual change-detection path, silently leaving the "no ETag" case unexercised until much later, which is exactly the kind of gap that turns into a surprise once real traffic hits it.

### Step 6 — The config collector engine

**What:** `internal/adapters/collectors` — `html_selectors` and `text_regex`, interpreting collector-config-spec.md §3's field reference, including the `map` hard-fail rule and the date-precision consistency check at config-load time (collector-config-spec.md §6).

**What it unblocks:** the first real, concrete proof that collector-sdk.md's `Extract`-is-pure contract (§5) and the "collectors cannot publish" type constraint (§6) hold for something other than a hypothetical — this is the step where those two design commitments either demonstrably work or reveal a gap in the interface as designed. It also unblocks the two MikroTik configs (first-sprint-backlog.md SLICE-15) and their fixtures (SLICE-19), which are the actual content of the vertical slice.

**How it is verified:** each engine's own fixture corpus, independent of any specific vendor's fixtures, exercising every transform in the vocabulary (collector-config-spec.md §5) and both success and hard-failure paths; then the MikroTik-specific fixtures against the engine, run through `collectortest.RunFixtures`.

**What would be a mistake to build before this:** wiring the scheduler and job queue (Step 7) fully before at least one real collector engine exists to extract with — the pipeline wiring step is meant to connect already-proven components; connecting the scheduler to an engine that does not exist yet just means building the wiring twice, once speculatively and once for real.

### Step 7 — The job queue

**What:** `internal/adapters/postgres`'s queue adapter (or its own `internal/adapters/queue/postgres` package, per the repository structure) — `SKIP LOCKED` dequeue, idempotency keys, attempt counting, backoff, dead-lettering (blueprint §3.1, ADR-0015, first-sprint-backlog.md SLICE-13).

**What it unblocks:** the scheduler loop and the actual asynchronous handoff between `CheckSource` and `ExtractCandidateRelease` (blueprint's collector-sequence.md diagram) — before this exists, the pipeline can only be exercised synchronously, in-process, inside a test; the queue is what makes it a real, restart-safe, worker-driven pipeline.

**How it is verified:** a race-detector test (`go test -race`) proving concurrent dequeue calls never return the same row twice; an idempotency-key test proving double-enqueue is a no-op; a dead-letter test proving a job that fails past its retry budget reaches `dead_lettered_at` rather than retrying forever.

**What would be a mistake to build before this:** deferring it past this point — some teams are tempted to skip a "proper" queue for a script that just calls use cases in sequence, planning to "add the queue later." Doing so here specifically would mean the pipeline-wiring step (next) gets built against a shape (synchronous, in-process) that the real, worker-driven shape does not match, again risking the exact kind of rework this whole ordering is designed to avoid.

### Step 8 — The CLI

**What:** `apps/cli` — `migrate up`, `registry sync`, `check-source --id`, `collector test` (first-sprint-backlog.md SLICE-16).

**What it unblocks:** manual, local verification of every component built so far, without needing the HTTP API or the scheduler loop running — `firmscout check-source --id mikrotik.changelogs` becomes the fastest way to prove the pipeline works end to end against a real (or fixture-served) MikroTik source, and it is the tool the CLI-driven parts of `definition-of-done.md`'s quality gates rely on directly.

**How it is verified:** running the four subcommands in sequence against the Compose Postgres — `migrate up` → `registry sync` → `collector test` on both MikroTik configs → `check-source --id` — produces a real `releases` row from a real (fixture-backed, per blueprint §7.8's rule) run.

**What would be a mistake to build before this:** the HTTP API (Step 9) before the CLI — building the API first means the only way to verify the pipeline actually works is by standing up a whole HTTP server and issuing requests against it, which is strictly more setup than a CLI invocation for the same verification purpose, and delays having *any* convenient way to exercise the pipeline until the API layer, one of the later steps, is also done.

### Step 9 — The HTTP API

**What:** `internal/adapters/httpapi` — the eight MVP-scope handlers, RFC 9457 errors, cursor pagination, cache headers, rate-limit headers, matching `docs/api/openapi.yaml` (api.md §2, first-sprint-backlog.md SLICE-21/22).

**What it unblocks:** the web app (Step 11) and every external consumer — this is the first point at which FirmScout's data is reachable by anything outside the repository's own tooling.

**How it is verified:** `httptest`-based handler tests with fake use cases (blueprint §7.8's API-handler row) covering every documented status code; a contract test asserting handler-to-OpenAPI-entry parity (api.md §10).

**What would be a mistake to build before this:** telemetry (Step 10) fully wired before real handlers exist to instrument — some of telemetry's value (span naming conventions, what a "request" even means as a unit of observability) is easier to get right once there is a real handler shape to attach it to, rather than designing the instrumentation in the abstract.

### Step 10 — Telemetry

**What:** `internal/adapters/telemetry` — OpenTelemetry traces and metrics wired through the pipeline and the API, the `slog` OTel bridge for structured logs (blueprint's telemetry adapter, first-sprint-backlog.md SLICE-24–26).

**What it unblocks:** actual visibility into whether Steps 1–9 work correctly under real (even if locally simulated) load, and it is the prerequisite for the Compose stack's Grafana/Prometheus/Tempo/Loki services (Step 13) to have anything meaningful to display.

**How it is verified:** a single `check-source` run produces one trace spanning fetch → normalize → extract → validate → publish with no orphaned spans; Prometheus counters increment on real pipeline activity; a log line is correlatable to its trace by `request_id`/`trace_id`.

**What would be a mistake to build before this:** instrumenting a pipeline that is not yet functionally correct — per `definition-of-done.md` §3's framing, telemetry is meant to *observe* correctness that Steps 1–9 already established through their own dedicated tests, not to be the mechanism that discovers whether the pipeline works at all. Building telemetry first (or interleaved too early) risks producing traces and dashboards that describe bugs rather than confirming their absence, and wastes effort instrumenting code paths that are still actively changing shape.

### Step 11 — The web app

**What:** `apps/web` — the MikroTik RouterOS product page, reading from the Step 9 API (first-sprint-backlog.md SLICE-23).

**What it unblocks:** the first genuinely public-facing artifact of the vertical slice — everything before this step is internal-facing (a database, a CLI, an API a browser could technically hit but nothing built for a human to look at).

**How it is verified:** `npm run lint`, `npm run typecheck`, `npm run build`; the page renders real data from a running Compose stack, with `releaseDatePrecision` visibly affecting date rendering rather than a page that always assumes a full date is available (api.md §4).

**What would be a mistake to build before this:** building the web app against a mocked or hand-typed API response before the real API (Step 9) exists and is stable — a common temptation for parallelising frontend and backend work, but one that, given this is a single small team building one vertical slice rather than large parallel teams, offers little parallelisation benefit here while risking exactly the kind of shape-mismatch rework this whole document is organized to avoid.

### Step 12 — Docker Compose

**What:** `infrastructure/docker/docker-compose.yml` bringing up postgres, api, worker, web, and the observability stack together (first-sprint-backlog.md SLICE-28).

**What it unblocks:** the actual Phase 1 exit criterion (mvp-roadmap.md) — a laptop-runnable, no-AWS-account-needed proof that the whole system works together, and the environment every quality gate in `definition-of-done.md` §2's smoke test runs against.

**How it is verified:** `docker compose up --build` bringing up every service healthy; `curl localhost:8080/healthz` returning `200`; `curl localhost:8080/api/v1/products/mikrotik-routeros` returning real data once `registry sync` and at least one `check-source` run have populated it; Grafana's provisioned dashboard loading without manual configuration.

**What would be a mistake to build before this:** essentially nothing — Compose is deliberately last because it is an integration artifact, not a component with its own logic; building it earlier would mean repeatedly updating a Compose file to track components that do not exist yet, with no way to actually verify it works until those components are real. It is worth noting explicitly, though, that Compose being *last in the build order* does not mean it should be neglected or rushed — it is the concrete deliverable that proves every step before it actually composes into a working system, and its own exit criteria (above) are not optional just because it comes at the end of a list.

### Step 13 — The observability stack (Compose services)

**What:** the `otel-collector`, `prometheus`, `loki`, `tempo`, `grafana` services and their provisioning (datasources, the "Platform overview" dashboard) inside `infrastructure/observability/`, brought up as part of Step 12's Compose file.

**What it unblocks:** the human-facing half of Step 10's telemetry work — traces and metrics that Step 10 emits are only useful once there is somewhere to look at them, and Grafana's provisioned dashboard is what turns "the system emits OTLP data" into "a person can open a browser and see whether the system is healthy."

**How it is verified:** Grafana's dashboard loads with no manual datasource configuration required after `docker compose up`; a trace produced by a real `check-source` run (Step 8's CLI, or the scheduler) is visible in Tempo by its trace id; a Prometheus query against `source_checks_total` or `releases_published_total` returns a nonzero value after that same run.

**What would be a mistake to build before this:** provisioning dashboards before Step 10's instrumentation exists to feed them — an empty, uninstrumented dashboard provisioned early provides false confidence that observability is "done" when in fact nothing is emitting the data it expects to display.

## 2. Do not implement the whole roadmap in one pass

This document describes the build order for **Phase 1 of mvp-roadmap.md — the MikroTik vertical slice — and nothing beyond it.** It is not, and must not be read as, a green light to proceed straight through Phases 2 through 6 using this same step list extended indefinitely.

The explicit reasons this matters, restated from mvp-roadmap.md and definition-of-done.md rather than invented fresh here:

- **The MVP's own scope boundary is deliberate**, not a placeholder waiting to be filled in immediately — blueprint §1 names AI agents, CVE correlation, billing integration, SSO, and additional pilot vendors as things the MVP explicitly excludes, each for a stated reason (mvp-roadmap.md §1), not because they were forgotten.
- **Each phase in mvp-roadmap.md has its own exit criteria** that the *next* phase's usefulness depends on being genuinely met, not merely attempted — Phase 2's review-queue UI is only worth building once Phase 1 has proven the pipeline produces real candidates worth reviewing; Phase 6's AI agents are only worth activating once there is a deterministic baseline (Phases 1–2) whose failure modes the agents are actually escalating from, per blueprint §3.11's warning that AI pointed at nothing but good intentions produces "a catalogue of plausible-looking wrong firmware versions," which is "actively harmful," not merely unfinished.
- **A step in this document that looks reusable for a later phase (the fetcher, the config engine, telemetry) is being built once, correctly, specifically so later phases do not rebuild it** — but "later phases reuse Step 4's fetcher" is different from "build Step 4 and then keep going through every later phase's version of Steps 5 onward without stopping to verify Phase 1 actually works first." The verification steps attached to each numbered step above (tests, fixture runs, the Compose smoke test) are what make each step trustworthy enough to build on; skipping ahead skips exactly the checkpoints that justify calling the next step "built on something already proven" rather than "built on something assumed."
- **Concretely:** finish Step 13, run the full `definition-of-done.md` §2 gate list against the result, and treat mvp-roadmap.md's Phase 1 exit criteria as a real stopping point — a place to report status, get review, and deliberately decide to proceed — rather than a waypoint passed through without pausing on the way to Phase 2.
