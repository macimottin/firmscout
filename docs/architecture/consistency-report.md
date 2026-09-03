# Architecture consistency report

> Generated 2026-09-03, after the first vertical slice.
> Its purpose is to make the gap between the blueprint and the code **visible and
> specific**, rather than leaving it to be discovered later by someone who trusted a
> diagram. Where this document and [`blueprint.md`](blueprint.md) disagree, this one is
> describing what exists and the blueprint is describing what is intended.

## 1. What was verified by execution

Every claim below was produced by running the command, not by reading the code. Nothing
in this section is inferred.

| Gate | Command | Result |
| --- | --- | --- |
| Formatting | `gofmt -l .` | clean |
| Vet | `go vet ./...` | clean |
| Lint | `golangci-lint run ./...` | **0 issues** |
| Tests | `go test ./...` | 14 packages, all passing, **296 test functions** |
| Dependency rule | `go test ./internal/archtest/...` and `scripts/archcheck.sh` | PASSED, and confirmed with a deliberate violation that it fails |
| Migrations | `firmscout migrate up` against PostgreSQL 17.4 | 1 migration applied, idempotent on re-run |
| PostgreSQL adapter | `go test ./internal/adapters/postgres/...` with a real database | 32 integration tests passing |
| End-to-end slice | `go test ./internal/integration/...` | registry sync → check → extract → validate → publish → API response |
| CLI | `firmscout migrate|registry sync|sources list` | ran against a fresh database |
| API | `curl http://127.0.0.1:18080/api/v1/products/mikrotik-routeros` | 200 with correct date precision |
| Diagrams | `scripts/check-mermaid.sh` | 47 blocks parsed by Mermaid 11's own grammar |
| Registry schemas | `scripts/check-schemas.py` | 8 documents valid |
| Web typecheck | `npm run typecheck` | clean |
| Web lint | `npm run lint` | clean |
| Web build | `npm run build` | succeeds with no API running |
| Web tests | `npm test` | 9 tests passing |

**Not verified, and stated as such:** the Docker Compose stack has never been started.
Docker Desktop's WSL integration was unavailable throughout. The Compose file, the two
Dockerfiles and the observability configuration were validated statically only — YAML
and JSON parse, service references resolve, dashboard metric names cross-check against
the observability catalogue. No image has been built and no container has run.

**Also not verified:** the race detector. `go test -race` requires cgo, and this machine
has no C toolchain. Concurrency-sensitive code is guarded by mutexes and exercised by
concurrent tests, but the detector has not confirmed it.

## 2. Diagrams and the code that implements them

29 diagrams. The middle column names the packages a reader should open to see the
diagram in code; "conceptual" means the diagram documents an intended design with no
implementation behind it yet.

| Diagram | Implementing code | State |
| --- | --- | --- |
| [system-context](../diagrams/system-context.md) | whole repository | partial: manufacturer sources and GitHub are real; CVE sources, AWS and the AI provider are not connected |
| [high-level-architecture](../diagrams/high-level-architecture.md) | `apps/*`, `internal/*` | implemented except the AI agent box |
| [clean-architecture](../diagrams/clean-architecture.md) | `internal/{domain,application,adapters,platform}`, enforced by `internal/archtest` | **implemented and enforced** |
| [module-dependencies](../diagrams/module-dependencies.md) | `internal/archtest/arch_test.go` | **implemented and enforced** |
| [bootstrap-flow](../diagrams/bootstrap-flow.md) | — | conceptual |
| [update-decision-tree](../diagrams/update-decision-tree.md) | `internal/application/checksource.go`, `internal/adapters/fetch` | implemented for the deterministic path; AI escalation conceptual |
| [adaptive-scheduling](../diagrams/adaptive-scheduling.md) | `internal/domain/scheduling.go` | implemented; the defaults are untuned |
| [source-repair-flow](../diagrams/source-repair-flow.md) | — | conceptual |
| [ai-cost-escalation](../diagrams/ai-cost-escalation.md) | — | conceptual |
| [release-state-machine](../diagrams/release-state-machine.md) | `internal/domain/candidate.go` | **implemented**, illegal transitions rejected |
| [source-health-state-machine](../diagrams/source-health-state-machine.md) | `internal/domain/source.go` | **implemented**, `retired` is terminal |
| [request-protection-flow](../diagrams/request-protection-flow.md) | `internal/adapters/httpapi/{middleware,ratelimit}.go` | partial: in-process rate limiting only, no edge, no abuse scoring |
| [api-sequence](../diagrams/api-sequence.md) | `internal/adapters/httpapi` | partial: quota and metering paths exist, billing events do not |
| [collector-sequence](../diagrams/collector-sequence.md) | `internal/application/{checksource,ingest}.go`, `internal/adapters/collectors` | **implemented** and covered end to end |
| [data-model](../diagrams/data-model.md) | `database/migrations/00001_initial.sql`, `internal/adapters/postgres` | implemented for 29 tables; advisories, CVEs, lifecycle and corrections deferred |
| [deployment](../diagrams/deployment.md) | `infrastructure/terraform` (skeleton) | conceptual, nothing deployed |
| [contribution-flow](../diagrams/contribution-flow.md) | `.github/workflows`, `CONTRIBUTING.md` | implemented except staging validation |
| [observability](../diagrams/observability.md) | `internal/adapters/telemetry`, `infrastructure/observability` | code implemented; the stack has never been started |
| [product-analytics](../diagrams/product-analytics.md) | `internal/domain/events.go`, `internal/platform/wire.go` | partial: events are emitted to logs, the privacy filter and analytics tables are not written |
| [cost-aware-request](../diagrams/cost-aware-request.md) | `internal/adapters/httpapi/middleware.go`, `product_summaries` | partial: the precomputed-summary path is real, the CDN is not |
| [storage-lifecycle](../diagrams/storage-lifecycle.md) | `internal/adapters/postgres/artifact_repo.go`, `internal/adapters/artifact` | partial: content addressing and deduplication implemented, the retention sweep is not |
| [cost-containment-incident](../diagrams/cost-containment-incident.md) | — | conceptual runbook |
| [cve-correlation](../diagrams/cve-correlation.md) | — | conceptual |
| [version-normalization](../diagrams/version-normalization.md) | `internal/domain/version.go` | **implemented**, including the plausibility check |
| [alias-resolution](../diagrams/alias-resolution.md) | `internal/adapters/postgres/product_repo.go`, `internal/application/ingest.go` | implemented for exact and alias matching; trigram fuzzy matching is in search only |
| [multi-source-conflict](../diagrams/multi-source-conflict.md) | `internal/domain/validation.go` gate 10 | partial: the gate exists, but nothing yet populates the conflicting-version list |
| [entitlement-evaluation](../diagrams/entitlement-evaluation.md) | `internal/application/queries.go`, `internal/adapters/httpapi/middleware.go` | partial: quota evaluation implemented, plans and billing are not |
| [dataset-correction](../diagrams/dataset-correction.md) | — | conceptual |
| [review-prioritization](../diagrams/review-prioritization.md) | `internal/application/ingest.go` (`reviewPriority`) | partial: items are created and scored crudely, there is no review interface |

## 3. Components implemented versus conceptual

**Implemented and exercised by tests:** the domain (entities, value objects, both state
machines, the ten validation gates, adaptive scheduling); the application layer (ports,
the check/extract/validate/publish pipeline, registry sync, the read-side queries); the
PostgreSQL adapter (twelve ports, the migration runner, the `SKIP LOCKED` queue); the
guarded fetcher; content normalisation and section hashing; the two configuration-driven
collector engines and the fixture harness; the content-addressed artifact store; the HTTP
API with its presenters and middleware; OpenTelemetry setup; the three binaries; the
Next.js site.

**Conceptual — designed, documented, not built:** all five AI agents (ports and schemas
only, no runtime); CVE correlation and security advisories; lifecycle and EOL tracking;
the community correction workflow; billing events and plan management; the review
interface; webhooks and alerts; inventory upload; SSO and RBAC; the entire AWS
deployment.

## 4. Security controls: implemented versus planned

| Control | State |
| --- | --- |
| SSRF guard: scheme restriction, blocked CIDRs, address validation at `net.Dialer.Control`, redirect revalidation | **implemented**, tested against loopback, link-local, RFC1918, CGNAT, IPv6 unique-local and IPv4-mapped IPv6 |
| Destination port allow-list | **implemented** (relaxing it requires constructing the guard differently, not configuration) |
| robots.txt fetched, parsed, cached, honoured | **implemented** |
| Content size limit, MIME validation, timeouts | **implemented** |
| Per-host concurrency limiting and `Retry-After` | **implemented** |
| Compliance gating before dispatch | **implemented** in the domain, the dispatch query and a partial index; a test asserts no committed source is collectable |
| RE2-only regex in collector configs | **implemented** (Go's `regexp` has no backtracking) |
| Collectors structurally unable to publish | **implemented**, enforced by `archtest` |
| API keys hashed at rest with a display prefix | **implemented** in the schema and repository; no key-issuing CLI command yet |
| GitHub Actions pinned to commit SHAs | **implemented** (was a live gap; every `uses:` now names a 40-character SHA) |
| `govulncheck`, gitleaks, CodeQL, dependency review | **implemented** and no longer able to pass by doing nothing |
| Decompression ratio limits | partial: byte limits enforced, ratio limits not |
| Prompt-injection defence | conceptual (no agent runtime exists) |
| Audit event write paths | planned; the table exists, nothing writes it |
| Row-level security for tenant isolation | planned |
| Release signing and build provenance | planned |
| CloudFront, WAF, Shield | planned; no AWS account exists |

## 5. Observability: implemented versus planned

**Implemented:** OpenTelemetry traces, metrics and logs with OTLP export; a Prometheus
exporter so `/metrics` works without a collector; W3C trace context propagation
including through the job queue; `slog` JSON with trace and span correlation; the metric
name catalogue as Go constants shared with the alert rules and dashboards; graceful
degradation when no collector is reachable.

**Written but never run:** the OpenTelemetry Collector configuration with its secret
redaction and tail sampling, Prometheus with 13 alert rules, Loki, Tempo, and Grafana
with three fully specified dashboards and four stubs.

**Planned:** the analytics privacy filter and its tables (events currently go to logs
only), the remaining four dashboards, and continuous profiling.

## 6. Cost controls: implemented versus planned

**Implemented:** conditional requests with ETag and Last-Modified, so an unchanged
source transfers no body and runs no parser; targeted section hashing, so a page whose
furniture rotates does not trigger extraction; content-addressed artifact deduplication,
so unchanged content is stored once; precomputed product summaries, so a product page is
one indexed read; adaptive scheduling; bounded search input; per-host concurrency limits.

**Planned:** the retention sweep, S3 lifecycle policies, AI budgets and circuit breakers,
CDN caching, and every AWS-specific control.

## 7. Decisions requiring legal review

Unchanged from [ADR-0009](../adr/0009-code-and-data-licensing.md) and
[ADR-0018](../adr/0018-source-compliance-policy.md); itemised in
[licensing.md](licensing.md). The ten questions are all open. Two are now more concrete
because the code exists:

- **Collecting from a host whose robots.txt disallows it.** Dell is registered and
  disabled. The platform cannot currently be made to fetch it, by configuration or by
  flag, which is deliberate.
- **Using an undocumented vendor update API.** Ubiquiti is the recommended substitute
  pilot and has not been registered, because its terms have not been reviewed.

## 8. Decisions requiring product input

1. **Enabling the two MikroTik sources.** Both ship `enabled: false` with
   `terms_review_status: pending`. Someone must read mikrotik.com's terms and decide.
   Until then FirmScout collects nothing at all.
2. **The GitHub organisation.** The module path assumes `github.com/macimottin/firmscout`.
3. **Conduct and security contact addresses**, left as `TODO:` in `CODE_OF_CONDUCT.md`
   and `SECURITY.md`.
4. **Plan pricing and the value metric.** The FinOps document recommends charging by
   products monitored rather than request volume; no prices are set.
5. **How much release history the free tier sees.** The blueprint proposes a twelve-month
   window; the API does not yet implement tier-based windowing.
6. **Whether the epoch in MikroTik's pointer file may ever be published as a date.** The
   collector currently publishes no date from it, on the grounds that the vendor does
   not document what it means.

## 9. Known limitations

1. **One vendor, one product, two sources, neither enabled.** The catalogue is empty by
   design. Everything that has run end to end has run against a recorded fixture.
2. **The multi-source conflict gate has nothing to compare.** Gate 10 exists and is
   tested, but no code yet gathers versions reported by other sources, so it always
   passes. A product with a single source therefore has no integrity cross-check — the
   most significant unmitigated data-integrity weakness in the system.
3. **`ProductSummary.HasSourceConflict` and `AdvisoryCount` are never set**, because
   neither a conflict record nor an advisory table exists. They are preserved rather
   than reset on refresh, so they will not silently report zero once populated.
4. **Rate limiting is per instance.** With N API instances the effective limit is N
   times the configured one. Acceptable at MVP instance counts, and documented in the
   middleware.
5. **Quota enforcement fails open** when the usage store errors, on the reasoning that a
   metering outage should not become a customer outage.
6. **The review queue has no interface.** Items are created and scored; a human would
   have to read them out of the database.
7. **Several OpenAPI fields are absent rather than fabricated** where the read model
   cannot supply them: vendor product counts, official source lists, family slugs, and
   the product and source objects on a release. The API omits the key rather than
   inventing a value.
8. **Problem-type URIs diverge** between `api.md` and the implementation. A problem type
   is an identifier consumers hardcode, so reconciling it is a contract decision, taken
   before the API is published.
9. **`go list ./...` picks up a vendored Go file inside `apps/web/node_modules`.** It is
   harmless and absent from a fresh checkout, but it appears in local package listings.
10. **The adaptive scheduling defaults are guesses.** They are labelled as such
    throughout and should be tuned against measured publication cadence.

## 10. Defects found by executing rather than reading

Recorded because they are the argument for the verification discipline, not for the
architecture. None of these would have been caught by review alone.

| Defect | How it surfaced |
| --- | --- |
| `array_to_string` is only `STABLE`, so the generated `search_vector` column made the entire migration fail | first real `migrate up` |
| `TruncateExcerpt` returned up to 4002 bytes, because the ellipsis is three bytes in UTF-8, and `Evidence.Validate` rejects over 4000 | flagged by an implementer, then pinned with a test |
| `publication_date` had no precision column while `release_date` did, so a month-precision publication date would read back as a real day | schema review against ADR-0017 |
| `CheckSource` stored the *normalised* text as the artifact, so extraction found nothing | end-to-end run |
| Nothing wrote the `source_artifacts` metadata row, so the first check violated a foreign key | end-to-end run |
| `ResolveByAlias` compared a normalised hint against a raw slug, silently matching nothing | end-to-end run |
| GitHub Actions pinned to mutable tags, one to `@main`, and a `govulncheck` job that could pass without running | threat-model review |
| A domain test compared an expression with itself and therefore asserted nothing | `staticcheck` |
| A Mermaid diagram used backslash-escaped quotes, which GitHub renders as an error box | real Mermaid parser, after a structural lint had passed it |
| `Dockerfile.go` was compiled by the Go toolchain and broke `go build ./...` | full-module build |

## 11. Recommended next implementation step

**Enable one source and let the pipeline run against the real internet, once.**

Everything else is second. The system has never fetched from a manufacturer, and the
gap between a recorded fixture and a live page is exactly where this class of project
fails. The sequence:

1. Review mikrotik.com's terms of use and record the decision in
   `dataset/sources/mikrotik/changelogs.yaml`.
2. If approved, set `enabled: true`, run `firmscout registry sync`, and run
   `firmscout check-source --vendor mikrotik --slug changelogs` by hand.
3. Compare what it publishes against the page in a browser, and fix what differs.
4. Only then start the worker on a schedule.

After that, in order: **the second collector archetype** (Poly's PDF release notes,
which also exercises the source-relocation path since `docs.poly.com` now redirects to
`support.hp.com`); **the review queue interface**, because the pipeline already produces
items nobody can see; and **multi-source conflict population**, which closes the biggest
integrity gap in section 9.

Do not start the AWS deployment or the AI agents. Neither is on the critical path to
knowing whether the catalogue can be maintained, which is the only question the MVP
exists to answer.
