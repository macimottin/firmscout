# MVP roadmap

> Expands [blueprint §1](blueprint.md#1-executive-summary) (executive summary's MVP scope), [§3.6](blueprint.md#36-dell-as-the-clean-api-pilot--challenged-on-compliance-grounds) (pilot vendor selection), and the "explicitly not in the MVP" list. This document is the phased delivery plan from the current repository skeleton (Phase 0, complete) through the vertical slice and beyond. It does not implement anything by itself — [first-sprint-backlog.md](first-sprint-backlog.md) is the concrete, ordered work for Phase 1.

## 1. MVP scope, as a checklist

The MVP is the smallest system that proves the whole pipeline end to end for one real vendor, with the observability and testing discipline the rest of the platform depends on, without building anything the brief explicitly defers.

**In scope:**

- [ ] One Go module, three binaries (`api`, `worker`, `cli`) plus the Next.js `web` app, per blueprint §10.
- [ ] Clean Architecture layering (`domain` → `application` → `adapters` → `platform`), enforced by `internal/archtest`.
- [ ] PostgreSQL schema for the registry and fact tables needed by the vertical slice (blueprint §11), via sqlc.
- [ ] The full check → extract → validate → publish pipeline (blueprint §16), running for MikroTik RouterOS end to end.
- [ ] A PostgreSQL-backed `JobQueue` adapter (`SKIP LOCKED`), no external queue service.
- [ ] Two config-driven collector engines: `html_selectors`, `text_regex` (collector-config-spec.md §4).
- [ ] The MVP read API surface: `search`, `vendors`, `vendors/{slug}`, `products/{slug}`, `products/{slug}/releases`, `products/{slug}/latest`, `releases/{id}`, `healthz` (api.md §2), with RFC 9457 errors, cursor pagination, cache headers, rate-limit headers.
- [ ] A public product page for MikroTik RouterOS on the Next.js site, reading from the API.
- [ ] OpenTelemetry traces, metrics, and structured logs across the pipeline; a self-hostable Prometheus/Loki/Tempo/Grafana stack running in Docker Compose.
- [ ] API usage recording for keyed requests (the mechanism, not the commercial plan machinery — see "explicitly deferred" below).
- [ ] Four pilot vendors registered in the dataset registry (§2), with **one** (MikroTik) collected end to end as the vertical slice; the other three registered, documented, and — where compliance status permits — partially exercised (e.g. Fortinet's PSIRT feed fetched and normalised) without necessarily reaching full publication.
- [ ] `docker compose up` bringing up the full stack (Postgres, api, worker, web, observability) on a laptop with no AWS account.

**Explicitly out of scope for the MVP** (blueprint §1's list, restated with the reasoning):

- **CVE correlation** — designed (api.md §3.5's `Advisory` schema documents the intended shape), not built. Correlating a release against a CVE's affected-version ranges is a meaningful data-modelling and confidence problem in its own right (see [`docs/diagrams/cve-correlation.md`](../diagrams/cve-correlation.md)) that does not need to block proving the core catalogue pipeline works.
- **AI agents** — ports and JSON schemas only (`agents/`), fakes in tests, no runtime provider integration. Blueprint §3.11's core caution — a catalogue of plausible-looking wrong firmware versions is actively harmful — means AI escalation earns its cost budget and its trust only after the deterministic path is proven and the agents have something real to escalate *from*.
- **Billing integration** — usage is recorded (a prerequisite for billing) but no payment processor, invoicing, or plan-upgrade flow exists yet. Phase 3 below adds quotas and metering; actual billing integration is later still.
- **SSO** — not needed until there is an Enterprise-tier customer asking for it (blueprint §5's capability matrix marks SSO/RBAC/audit-log-export as "later phase" explicitly).
- **More than four pilot vendors** — collector coverage breadth is a Phase 2+ concern; the MVP's job is to prove the pipeline and the four source archetypes below, not to maximise catalogue size prematurely.

## 2. The four pilot vendors, and why each was chosen

Each pilot vendor is chosen to be the canonical example of one **source archetype** — not because the vendor itself is strategically important, but because the *shape of its source* teaches the platform something no other archetype does. Facts below are measured, per DATA_SOURCES.md, as of 2026-09-03.

### MikroTik — structured HTML plus a cheap plain-text version endpoint

**Why chosen:** MikroTik exposes both the cheapest possible change signal (`upgrade.mikrotik.com/routeros/NEWESTa7.stable`, a few bytes of `version epoch` text with ETag and Last-Modified — collector-config-spec.md §8.2) *and* a structured, server-rendered HTML changelog page with genuinely useful metadata (version, channel, exact-day date). No authentication, no PDF, no robots restriction, and both artifacts are small enough to fixture cheaply. **This is what makes it the vertical slice**: it is the one pilot with nothing blocking full pipeline exercise, so it is the one that proves check → extract → validate → publish works before any harder archetype is layered on.

**What it teaches the platform:** the "cheapest change signal" discipline (developing-collectors.md) in its purest form — a real, working example of preferring a dedicated tiny endpoint over full-page hashing; the `html_selectors` engine against a real, moderately complex (Livewire/Alpine-rendered) page, including the `normalize.section_selector` technique needed to avoid false-`changed` signals from volatile per-visit markup; and the date-precision discipline in a case where the source genuinely provides exact-day dates on one artifact but only a change-detection timestamp (not a documented release date) on the other — exercising the "don't invent precision even when a number that looks more precise is available" rule concretely (collector-config-spec.md §8.2).

### Poly / HP — PDF release notes, plus a real post-acquisition relocation problem

**Why chosen:** Poly's release notes are PDF documents — the format archetype no MVP config engine (`html_selectors`, `text_regex`) can parse at all, making Poly the natural forcing function for scoping (not necessarily building, in the MVP) the `pdf_text` roadmap engine and, in the interim, for exercising the code-collector path if any Poly extraction is attempted during the MVP window. Just as importantly, `docs.poly.com` — Poly's own historical documentation domain — now 301-redirects to `support.hp.com/bundle/...`, a direct, measured consequence of HP's acquisition of Poly. This is not a hypothetical scenario the platform should merely be designed to tolerate; it already happened.

**What it teaches the platform:** that source URLs are not permanent even for a vendor's *own* official documentation (DATA_SOURCES.md), which is why the source registry models relocation as a first-class event (`SourceRelocated`, blueprint §7.7) rather than treating a moved source as an undifferentiated fetch failure; that a redirect target changing ownership (Poly's docs now live on an HP-controlled domain) raises a genuine "is this still an official source, or now an authorised-support-portal-class source" classification question (DATA_SOURCES.md's quality classes) that the registry has to be able to represent, not just a URL to update; and that PDF, as a format, is a real, not theoretical, gap in the MVP's engine coverage that the roadmap needs to name honestly (collector-config-spec.md §4.3) rather than pretend `html_selectors` will eventually stretch to cover it.

### Fortinet — a difficult portal with authenticated downloads, but a public PSIRT RSS feed

**Why chosen:** Fortinet's firmware download portal requires authentication, which is out of scope for automated collection entirely under DATA_SOURCES.md's compliance policy — this is the pilot for "a source that is mostly, deliberately, not collected." But Fortinet also publishes a genuinely public, unauthenticated PSIRT advisory feed (`fortiguard.com/rss/ir.xml`, 302-redirecting to `filestore.fortinet.com/fortiguard/rss/ir.xml`, measured as RSS 2.0) — so the pilot is not "Fortinet is unusable," it is "part of Fortinet is legitimately collectible and part is not, and the registry has to represent that distinction per-source, not per-vendor."

**What it teaches the platform:** that `FetchAuthenticationRequired` (collector-sdk.md §3.4) is a legitimate, expected, non-alarming outcome for a specific source, not a symptom of something broken — the platform must not treat "this source needs auth we don't have" the same as "this source is failing and needs repair"; that compliance scope is evaluated **per source**, not per vendor, since one vendor can have both an in-scope and an out-of-scope source simultaneously; and, once RSS/Atom parsing lands as a config engine (`rss_atom`, collector-config-spec.md §4.3), Fortinet's feed is the natural first real config to validate that engine against, alongside exercising the `advisory` release type end to end for the first time.

### Dell — a technically ideal XML catalogue, BLOCKED by `robots.txt`, kept registered but disabled

**Why chosen:** Dell's `downloads.dell.com/catalog/Catalog.xml.gz` is, on pure technical merit, the best-shaped source among all four pilots — a single well-formed, gzipped XML catalogue, ETag- and Last-Modified-bearing, 1.4 MB, exactly the "clean structured catalogue" archetype the brief originally asked a pilot to represent. It is included specifically **because** it is blocked: `downloads.dell.com/robots.txt` returns `Disallow: /` for all user agents (permitting only `/manuals` and `/topicspdf`), and Dell is registered with `robots_policy_status = 'disallowed'`, `terms_review_status = 'pending'`, `enabled = false` — visible, documented, never collected, per blueprint §3.6 and ADR-0018.

**What it teaches the platform, and why it is kept in the registry rather than silently dropped:** that compliance status is a first-class, evaluated-before-collection field on every source, not a footnote — DATA_SOURCES.md states this directly: "if FirmScout is willing to bend this rule for its most convenient source, the rule doesn't mean anything for the next thousand sources." Keeping Dell registered-but-disabled, with its status visible in a public, open-source repository, is a deliberate demonstration that the policy holds even against its own strongest counter-example. **Ubiquiti's JSON firmware API (`fw-update.ubnt.com/api/firmware-latest`) is recommended as the substitute** for the "clean API" archetype pending Dell's terms review or an official API — Ubiquiti has no `robots.txt` restriction (a 404 on the file itself) and returns structured JSON with channel, platform, product, timestamps, and checksums, but its own terms of use still require review (`terms_review_status = pending`) before it is enabled, so it is registered and documented, not yet collected either, as of this roadmap.

## 3. Phased delivery

Each phase states its goal, its deliverables, its exit criteria, and a rough size (S = days, M = one to a few weeks, L = a month or more of focused work) — sizes are for sequencing intuition, not a committed schedule.

### Phase 0 — Blueprint, ADRs, diagrams, skeleton (done)

**Goal:** establish the architecture, the decisions behind it, and a navigable repository shape before any product code exists.
**Deliverables:** `docs/architecture/blueprint.md` and its companion documents (this one included); ADRs 0001–0012 and onward; the full Mermaid diagram set under `docs/diagrams/`; the repository skeleton (`apps/`, `internal/`, `collectors/`, `database/`, `dataset/`, `packages/`, `infrastructure/`, `docs/`, `testdata/`, `scripts/`) with community files (README, CONTRIBUTING, CODE_OF_CONDUCT, SECURITY, GOVERNANCE, DATA_SOURCES).
**Exit criteria:** `scripts/check-mermaid.sh` passes on every diagram; the architecture documents are internally consistent (this document does not contradict the blueprint it expands).
**Size:** L (already spent).

### Phase 1 — First vertical slice, MikroTik

**Goal:** prove the entire pipeline — vendor record through published release through public API and page — for one real vendor, with tests, telemetry, and Compose execution, exactly as the twenty steps in [first-sprint-backlog.md](first-sprint-backlog.md) lay out.
**Deliverables:** `internal/domain` and `internal/application` with fakes-based tests; PostgreSQL schema (initial migration) and sqlc-generated repositories; the guarded `Fetcher` adapter; `html_selectors` and `text_regex` engines plus the two MikroTik configs (collector-config-spec.md §8); the PostgreSQL job queue; the eight MVP-scope API endpoints; the MikroTik RouterOS product page; OpenTelemetry instrumentation end to end; Docker Compose bringing up the full stack.
**Exit criteria:** `go test ./...` passes (domain, application, adapters, archtest, collector fixtures); `docker compose up --build` succeeds; a real or `httptest`-simulated `check-source` run against MikroTik's changelog and version endpoint produces a published release retrievable via `GET /api/v1/products/mikrotik-routeros/latest` with a correct `releaseDatePrecision`; a trace for that request is visible in Tempo and a corresponding metric in Prometheus.
**Size:** L.

### Phase 2 — Second and third collector archetypes, review queue UI

**Goal:** prove the platform generalises beyond one vendor and one clean archetype, and give a human reviewer a real interface for the candidates that do not auto-publish (blueprint §16 gates that route to review).
**Deliverables:** Fortinet's PSIRT RSS feed collected end to end (exercising `advisory` release type and, if built in this phase, the `rss_atom` engine); further work toward the Poly/HP PDF archetype (either a `pdf_text` engine spike or an explicit code-collector attempt, whichever proves more tractable — a genuine open question, not pre-decided here); a review-queue UI in the Next.js app (or an admin surface) for `review_items`, letting a human accept/reject a candidate with the decision recorded per blueprint §16's audit trail.
**Exit criteria:** at least one second vendor's data is publicly queryable end to end; the review queue UI lets a human resolve a routed candidate without direct database access; diagrams updated to reflect what was actually built (definition-of-done.md's rule that a stale diagram is a defect).
**Size:** L.

### Phase 3 — API keys, quotas, metering, usage dashboard

**Goal:** make the commercial tier (blueprint §5's capability matrix) real: issuable API keys, enforced quotas, and a consumer-facing view of their own usage.
**Deliverables:** `apikey create`/rotate/revoke in the CLI; the durable PostgreSQL quota counters and in-process token-bucket rate limiting (blueprint §3.9); `GET /api/v1/usage`; a usage dashboard (web app, keyed) surfacing what `X-RateLimit-*`/`X-Quota-*` headers already expose programmatically (api.md §7).
**Exit criteria:** a created key is rate-limited and quota-limited correctly under load testing; usage recorded matches what the dashboard displays; `POST /api/v1/lookup` (bulk, key-required) is reachable and correctly gated by tier.
**Size:** M.

### Phase 4 — Advisories and lifecycle

**Goal:** build the CVE-correlation and lifecycle (EOL/EOS) capability that was deliberately deferred out of the MVP (§1), now that the core catalogue and its confidence machinery exist to build on.
**Deliverables:** `security_advisories`, `cves`, `cve_product_mappings`, `lifecycle_events` tables (named in blueprint §11 as created "by later migrations"); the `Advisory` API schema (api.md §3.5) populated for real; the affected-version-range correlation logic (never by numeric version comparison, per blueprint §3.5 — matched against vendor-stated ranges, same as everything else); [`docs/diagrams/cve-correlation.md`](../diagrams/cve-correlation.md) validated against what was actually built.
**Exit criteria:** at least one pilot vendor's advisories are correlated to specific product releases with vendor-stated affected ranges, visible via `/products/{slug}/advisories`.
**Size:** L.

### Phase 5 — AWS deployment

**Goal:** run the same binaries built for Compose in AWS, per the Lambda-first runtime decision (blueprint §3.4, ADR-0010), without a rewrite.
**Deliverables:** the Lambda Web Adapter wiring for `apps/api`; SQS and EventBridge Scheduler adapters for `JobQueue` and the scheduler, satisfying the same ports the PostgreSQL adapters already satisfy; OpenNext deployment for `apps/web`; Terraform modules under `infrastructure/terraform/` (currently a skeleton only); the observability stack's AWS equivalents or continued self-hosted operation, a decision this phase makes explicitly rather than by default.
**Exit criteria:** the production deployment serves the same API contract (`docs/api/openapi.yaml`) with no behavioural difference from Compose; cost is measured, not estimated, against the low/expected/high scenarios in `aws-cost-model.md`.
**Size:** L.

### Phase 6 — AI agents

**Goal:** activate the AI escalation path (blueprint §15) that has existed only as ports, JSON schemas, and fakes since Phase 0 — Discovery, Repair, Validation, Classification, and Source-quality agents, each bounded by an explicit per-run budget cap and each producing proposals a human or a deterministic gate reviews, never a direct publish.
**Deliverables:** a real provider adapter behind the `DiscoveryAgent`/`RepairAgent`/etc. ports; `ai_runs` cost and token tracking populated for real; the circuit breaker and budget-cap enforcement exercised under real usage, not just fakes; prompt version registry (`agents/`) with actual versioned prompts.
**Exit criteria:** at least one Repair-agent run against a genuinely broken source (a real layout change, not a synthetic fixture) produces a reviewable proposal — a fixture and an updated selector — that a human merges, closing the loop the roadmap opened in Phase 0 with `docs/diagrams/ai-cost-escalation.md` and `source-repair-flow.md`.
**Size:** L.

## 4. What is explicitly deferred, and why

Beyond the MVP-scope exclusions in §1, several things visible elsewhere in this document set are deliberately not committed to a phase number here:

- **Engine expansion beyond what a specific phase needs** (`json_path`, `xml_xpath`, `github_releases` from collector-config-spec.md §4.3) is deferred until a real pilot vendor's source actually needs it, rather than built speculatively — Fortinet's feed motivates `rss_atom` in Phase 2; nothing in the current four pilots motivates `json_path`, `xml_xpath`, or `github_releases` yet, so they stay undesigned beyond the roadmap table until a concrete source does.
- **Community contribution tooling beyond the basics** (a contributor-facing collector-authoring UI, automated fixture recording tooling) is deferred — assumption P5 is explicitly marked **(unvalidated)** in blueprint §2, and building tooling to lower a contribution barrier that may not be the actual barrier (compared to, say, discoverability or trust) would be optimising before the real bottleneck is known.
- **A formal SLA or freshness commitment** (blueprint §5's Professional/Enterprise row) is deferred until there is operational history to base one on — committing to a freshness target before Phase 1 has even measured real check intervals and extraction latency would be a number picked from nowhere.
- **Kubernetes, Kafka, Redis, OpenSearch, or any message broker beyond the PostgreSQL-backed queue** (blueprint §1's explicit list) are deferred indefinitely, not to a phase — each has a stated threshold to revisit (blueprint §3.1, §3.9, §3.10) tied to a measured condition, not a calendar date, and none of those thresholds are expected to be crossed within the phases above.
