# Cost controls

> Companion to [blueprint.md](blueprint.md) §1, §3.4, §3.9–3.10, and [ADR-0013 — Cost minimisation](../adr/0013-cost-minimization.md). Where the blueprint states the *principle* ("low idle cost", "deterministic by default, AI only by escalation"), this document states the *mechanism* — the specific engineering rule, the code or configuration that enforces it, and the metric that proves it is working.
>
> Audience: anyone implementing the ingestion pipeline, the AI adapters, or the on-call rotation. This document assumes familiarity with the pipeline described in blueprint.md §16 (`CheckSource → ExtractCandidateRelease → ValidateCandidateRelease → PublishRelease`).

---

## 1. Cost principles, turned into engineering rules

The blueprint states seven operating principles (§1). The two cost-relevant ones — "deterministic by default, AI only by escalation" and "low idle cost" — decompose into the following concrete rules. Each rule names the mechanism that enforces it, because a principle without an enforcement point degrades into a comment nobody re-reads under deadline pressure.

| # | Principle | Engineering rule | Enforcement mechanism |
| --- | --- | --- | --- |
| 1 | Avoid always-on compute | The API, worker, and web front end run on Lambda behind CloudFront/API Gateway, not on a permanently running server. The only always-on cost is the database. | [ADR-0010](../adr/0010-aws-runtime.md); no ECS service, no EC2 instance, no load balancer in the MVP deployment |
| 2 | Prefer conditional requests over full downloads | `Fetcher.Fetch` always sends `If-None-Match`/`If-Modified-Since` from the prior `FetchState` when the source returned one. A `304 Not Modified` costs a request and near-zero bytes; it never re-runs extraction. | SDK contract test fails a collector that bypasses conditional fetch (blueprint §13) |
| 3 | Prefer targeted section hashes over full-page processing | `Normalizer` hashes only the `section_selector` (or a source-specific extraction target) declared in the collector config, not the full response body. A source whose layout wraps the changelog in navigation chrome does not trigger `changed` every time an unrelated banner updates. | `collector-config-spec.md`'s `normalize.section_selector`; collector fixture tests assert the hash ignores stripped elements |
| 4 | Prefer deterministic parsing over AI | Collectors (`html_selectors`, `text_regex`, and the roadmap engines) are the default and only path for extraction. AI agents are invoked from `ExtractCandidateRelease`/`RepairAgent` triggers only, never from the default per-check path. | Collector adapters have no dependency on `agents/`; `AgentRun.trigger_reason` is a required, enumerated field so every AI invocation is traceable to a specific deterministic failure |
| 5 | Batch when safe | The worker's scheduler enqueues source checks in windows rather than firing one job per source per tick; `registry sync` and `product_summaries` refresh operate on changed rows in a single transaction rather than row-by-row. | Job queue `Enqueue` accepts a batch of idempotency keys in one call; `PublishRelease`'s summary refresh is scoped to the affected product, not a full table scan |
| 6 | Cache aggressively | Every cacheable API response carries `ETag` and `Cache-Control` (blueprint §12); CloudFront serves them at the edge without invoking Lambda or touching PostgreSQL for a cache hit. | CloudFront cache policies keyed on normalised query strings; cache invalidation is targeted (specific product path) on `release.published`, never a full-distribution flush |
| 7 | Precompute product summaries | The public site and search never join `releases` + `products` + `evidence` per page view. `product_summaries` is refreshed by `PublishRelease` at write time. | blueprint §11 rule 9; a materialised view is the documented escalation if refresh cost grows |
| 8 | Avoid duplicate source checks | `jobs.idempotency_key` (e.g. `source-check:{source_id}:{scheduled_window}`) makes a second enqueue of the same check within the same window a no-op, not a second row. | Unique constraint on `idempotency_key`, per blueprint §11 rule 8 |
| 9 | Share one source result across many products | A `SourceCheck` and its `Artifact` are checked and normalised once; `ExtractCandidateRelease` can emit candidates for every product the source's `product_match` (or a multi-product extractor) covers from that single fetch. A vendor catalogue feed covering 40 products is one fetch, one hash, one extraction pass — not 40. | `release_product_mappings` is a many-to-many table specifically so one `Release`/one extraction run can cover a family (blueprint §11 rule 6) |
| 10 | Compress and deduplicate artifacts | Stored artifacts are compressed at rest, and a second fetch that produces byte-identical content after normalisation is not stored a second time — see §5, content-addressed storage. | `ArtifactStore` keys by content hash, not by `(source_id, fetched_at)` |
| 11 | Storage lifecycle policies | Every data class in §5 has a retention period and a storage tier; nothing is kept "forever by default." S3 lifecycle rules (or the Compose equivalent, a periodic cleanup job) move and expire objects without a human running a script. | §5 below; S3 `LifecycleConfiguration` per bucket prefix in the Terraform module |
| 12 | Avoid cross-region traffic | All AWS resources for a given deployment sit in one region. The database, Lambda functions, and S3 buckets share a region so inter-service traffic is intra-region (materially cheaper — retrieved 2026-09-03: intra-region cross-AZ transfer is $0.01/GB each direction versus $0.02/GB for inter-region peering, [AWS Price List API](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AWSDataTransfer/current/us-east-1/index.json), verify before relying on it). CloudFront is the only globally-distributed component by design (it is the edge, not a second region). | Terraform module takes one `aws_region` variable; no resource declares a different one |

---

## 2. Unit economics

FirmScout manages five unit costs. Each is expressed as a formula over *consumed units* — never a flat guess — so it can be recomputed as rates change or as the architecture changes. Rates ($RATE_*) are parameters; see [aws-cost-model.md](aws-cost-model.md) for which of them have been retrieved from AWS pricing and which remain unresolved variables.

### 2.1 Cost per source check

```
cost_per_source_check =
    RATE_LAMBDA_GB_SECOND * (worker_memory_gb * check_duration_seconds)
  + RATE_LAMBDA_REQUEST
  + RATE_DATA_TRANSFER_OUT_GB * bytes_fetched_gb        # usually ~0 on a 304
  + RATE_QUEUE_REQUEST * queue_operations_per_check      # enqueue + dequeue + delete
  + amortised_db_write_cost                              # one source_checks row
```

**What dominates:** for the overwhelming majority of checks (a conditional `GET` returning `304 Not Modified`), `bytes_fetched_gb` is near zero and `check_duration_seconds` is small (network round-trip, not extraction), so this cost is dominated by the **Lambda invocation floor plus the queue operation**, not by data volume. The architectural choice that dominates this unit cost is rule 2 above (conditional requests) — without it, every check would fetch and hash the full page, multiplying both the Lambda duration and the transfer cost by the fraction of checks that would otherwise be full downloads.

### 2.2 Cost per public search

```
cost_per_public_search =
    RATE_CLOUDFRONT_REQUEST
  + (1 - cache_hit_ratio) * (
        RATE_APIGW_REQUEST
      + RATE_LAMBDA_GB_SECOND * (api_memory_gb * query_duration_seconds)
    )
```

**What dominates:** the cache hit ratio. A search endpoint with a 60-second normalised-query cache (blueprint §12) turns most repeated queries — which dominate real search traffic, because a small number of product names account for a large share of lookups — into a CloudFront-only cost with no Lambda or database involvement. The architectural choice that dominates this unit cost is rule 6 (aggressive caching) combined with `product_summaries` (rule 7), which keeps the uncached-path query cheap when it does execute.

### 2.3 Cost per API request

```
cost_per_api_request =
    RATE_APIGW_REQUEST
  + endpoint_weight * RATE_LAMBDA_GB_SECOND * (api_memory_gb * request_duration_seconds)
  + RATE_CLOUDFRONT_REQUEST                              # if CloudFront fronts the API too
  + usage_metering_write_cost                             # see api-commercial.md §5
```

**What dominates:** `endpoint_weight`. A single product fetch and a bulk lookup of 500 products cost the API Gateway layer the same $RATE_APIGW_REQUEST, but wildly different Lambda duration and downstream database load — which is exactly why [api-commercial.md](api-commercial.md) §3 prices endpoints by weight rather than by request count alone. The architectural choice that dominates this unit cost is endpoint-weighted quota enforcement; without it, a customer's bulk endpoint usage is invisible to cost accounting until the AWS bill arrives.

### 2.4 Cost per published release

```
cost_per_published_release =
    (checks_per_release * cost_per_source_check)
  + extraction_runs_per_release * (RATE_LAMBDA_GB_SECOND * extract_duration_seconds)
  + validation_runs_per_release * (RATE_LAMBDA_GB_SECOND * validate_duration_seconds)
  + ai_escalations_per_release * cost_per_ai_agent_run    # usually 0
  + publish_transaction_db_cost
```

**What dominates:** `checks_per_release`, i.e. how many checks a source needs before it detects a real change. This is the number the adaptive scheduling strategy in §3 exists to minimise: a source checked too frequently wastes checks on `unchanged` outcomes; a source checked too rarely delays detection, which is a product-quality cost, not a dollar cost, but pushes work into an anxious catch-up check cadence that *is* a dollar cost. The architectural choice that dominates this unit cost is adaptive interval selection (§3), because deterministic extraction and validation are cheap in absolute terms — the check volume that precedes a real change is where the budget goes.

### 2.5 Cost per AI agent run

```
cost_per_ai_agent_run =
    RATE_TOKEN_INPUT * input_tokens
  + RATE_TOKEN_OUTPUT * output_tokens
  + RATE_LAMBDA_GB_SECOND * (agent_memory_gb * orchestration_duration_seconds)
```

**What dominates:** token count, and specifically *how much fetched content is included in the prompt*. This is why §4 imposes content chunk limits and prompt size limits before it imposes anything else — token cost has the highest per-unit variance of any cost in the system (blueprint §8.3), and the input side (fetched HTML, prior artifacts) is attacker- and vendor-influenced content that the system does not control the size of unless the adapter truncates and summarises it first. The architectural choice that dominates this unit cost is escalation discipline itself: this cost is zero for the overwhelming majority of checks by design (rule 4), so the aggregate AI bill is a function of the *escalation rate*, not the *check rate*.

---

## 3. Adaptive source scheduling

Adaptive scheduling is the single highest-leverage cost control in the system, because it acts on §2.1 and §2.4 simultaneously: fewer wasted checks *and* fewer checks-per-release. Full flow: [`docs/diagrams/adaptive-scheduling.md`](../diagrams/adaptive-scheduling.md) (forthcoming). This section is the policy that diagram will render.

### 3.1 Inputs to the next-interval decision

`scheduling.NextInterval` (blueprint §7.1, a pure domain function) takes:

| Input | Effect |
| --- | --- |
| Historical publication cadence | A source that has published every ~90 days for two years does not need daily checks between releases |
| Product lifecycle stage | Actively developed products check more often than end-of-life products; a product past its stated EOL date without a source override moves to the longest band |
| Source health | `degraded`/`recovering` sources check more often, briefly, to confirm recovery, then fall back to the normal band |
| Recent changes | A source that just changed is likely to be mid-release-cycle (patch follow-ups); shorten the interval briefly after a `changed` outcome |
| Consecutive unchanged checks | Each additional `unchanged` result lengthens the interval, within the band ceiling |
| Consecutive failures | Each additional `failed` result lengthens the interval too (see backoff, §3.2) — a broken source is not checked at the same rate as a healthy one |
| Rate-limit responses (`429`, `Retry-After`) | Directly sets the next check time from the header when present; otherwise triggers backoff |
| Product popularity | Higher observed API/search demand for a product justifies a shorter interval for its source, within the band |
| Security criticality | Sources for products with a history of security advisories default to a shorter band regardless of popularity |
| Customer subscriptions | A source backing a product with active webhook subscribers (blueprint §5, Professional+) is checked at least as often as the fastest subscribed freshness expectation |
| Paid freshness commitments | A source backing a product under a contractual freshness SLA (blueprint §5, Enterprise) has a floor interval derived from the SLA, overriding the popularity-derived interval if shorter |

### 3.2 Interval bands (starting defaults — tune from measured data, per assumption T1/T2 in blueprint §2)

| Band | Default interval | Applies to |
| --- | --- | --- |
| Critical | 15 minutes | Source under an active incident-response window, or a security-criticality override during an active CVE investigation |
| High | 4 hours | Recently changed source; source with subscribed customers or a tight freshness SLA; high-popularity active product |
| Standard | 24 hours | Default band for an actively maintained product with no recent signal either way |
| Low | 7 days | Product past its typical release cadence with several consecutive unchanged checks; low popularity |
| Dormant | 30 days | Product past EOL, or a source with a long unchanged streak and no subscriptions or SLA riding on it |

A source moves between bands by policy, not by an operator editing a cron expression: each check outcome feeds `NextInterval`, which recomputes the band. **These numbers are starting points**, not measurements — they should be revisited once `source_checks` history exists (assumption T1).

### 3.3 Backoff, jitter, and concurrency

- **Backoff on failure**: exponential, base 2, from the standard-band interval, capped at the Dormant band ceiling (30 days) so a permanently broken source does not stop being checked forever — it becomes eligible for `RepairAgent` escalation instead, per blueprint §15.
- **Jitter**: every computed interval gets ±10% random jitter before being written to `run_after`, so that sources on the same band do not all wake up in the same second and produce a thundering herd against the queue or against a shared upstream host.
- **Per-domain concurrency limits**: the worker enforces a configurable ceiling (default: 2 concurrent in-flight checks per registered domain) so that one vendor with many monitored sources under the same hostname cannot be hammered by FirmScout's own scheduler, independent of any global worker concurrency.
- **`Retry-After` handling**: when a `429` or `503` response carries `Retry-After`, that value is used verbatim as the next check time (bounded by the Dormant ceiling), overriding whatever the interval band would otherwise compute. Ignoring `Retry-After` is treated as a compliance and reliability bug, not a minor detail — it is the source telling FirmScout its own rate limit.

### 3.4 Job deduplication and shared results

- **Deduplication**: `jobs.idempotency_key` prevents a second `source.check.requested` job for the same source within the same scheduling window from being enqueued twice (rule 8, §1).
- **One check, many products**: when a source's fetch and extraction cover a product family (a vendor catalogue, an RSS feed listing several models), the check happens once and `ExtractCandidateRelease` fans out candidates per matched product from the single `Artifact` — never a second fetch per product (rule 9, §1). This is the mechanism, not just a principle: `CollectorConfig.spec.product_match` can express a family match, and the `Collector.Extract` contract returns `[]CandidateRelease`, plural, from one `Artifact`.

---

## 4. AI budget policy

AI is an escalation path (blueprint §1, [ADR-0006](../adr/0006-ai-as-escalation.md)), and escalation paths need hard ceilings because their per-unit cost variance is the highest in the system (§2.5). The policy has five layers, from narrowest to broadest, so a single bad job cannot exhaust a monthly budget before anyone notices.

### 4.1 Budget caps, narrowest to broadest

| Cap | Default (starting point) | Enforcement point |
| --- | --- | --- |
| Per-job cap | `$0.50` (matches the `budget_cap_usd` field in the agent envelope, blueprint §15) | The AI adapter aborts the run and returns a failed-run result the instant projected cost would exceed the cap, before the final model call if streaming, or by rejecting a job whose estimated prompt size implies a cost over the cap before dispatch |
| Per-vendor-per-day cap | `$5.00` | A vendor whose sources are all broken at once (a site redesign) cannot alone trigger unbounded repair attempts in one day |
| Per-tenant-per-month cap | Not applicable in the MVP (AI runs are platform-internal, not tenant-triggered); reserved for a future capability where a customer's inventory-comparison feature might trigger AI-assisted matching | — |
| Global daily budget | `$25.00` | A circuit breaker (§4.6) trips when the day's aggregate `ai_runs.estimated_cost_usd` crosses this; no queued AI job dispatches until the next UTC day |
| Global monthly budget | `$300.00` | Same circuit breaker at monthly granularity; crossing it is an incident (§7), not a silent throttle |

All four numeric defaults are **starting points for a pre-revenue project**, sized to bound the founder's own AWS bill during bootstrap, not derived from a traffic model. Revisit them once §4.7's metrics exist.

### 4.2 Model selection by task complexity

Per blueprint §15: classification and source-quality checks (small, closed-vocabulary outputs) use the least expensive capable model available from the provider. Discovery and repair (open-ended, evidence-gathering tasks) may use a more capable model, but still within the per-job cap — capability does not override budget. The provider/model identifier is adapter configuration, never hardcoded into a use case, so a cheaper model becoming capable enough for a task is a config change, not a code change.

### 4.3 Prompt, response, and content limits

- **Prompt size limit**: a hard ceiling on total input tokens per run (task-dependent; classification prompts are far smaller than repair prompts). Content exceeding the limit is truncated with the truncation explicitly marked in the prompt, never silently dropped, so the model does not reason as though it saw the whole artifact.
- **Response size limit**: the schema (blueprint §15) bounds output shape; an oversized or malformed response is treated as a schema-validation failure, i.e. a failed run routed to human review, never retried against a larger budget.
- **Content chunk limits**: when an artifact (a changelog page, a PDF) exceeds the prompt budget, it is chunked and only the chunks relevant to the trigger (e.g. the section around a selector miss) are included — not the whole document. Chunk selection is deterministic (nearest-N lines/elements to the point of failure), so the same failure produces the same chunk every time, keeping the run reproducible for debugging.

### 4.4 Result caching and duplicate suppression

- **Result caching**: an AI run's output is cached against `(agent, prompt_version, input_hash)`. A second trigger with byte-identical input (e.g. the same broken selector detected twice before a fix lands) returns the cached proposal rather than re-running the model.
- **Duplicate job suppression**: the same idempotency-key mechanism used for source-check jobs (§3.4) applies to AI jobs — a source flapping between `failed` and `recovering` within one scheduling window cannot enqueue a second repair job for the same failure.

### 4.5 Retries, human review, and no repeated AI loops

- **Maximum retries**: at most one retry per AI run, and only for a transient provider error (timeout, 5xx from the provider), never for a low-confidence or schema-invalid result. A model producing a low-confidence answer is a signal to route to a human, not a signal to ask the model again — asking again spends the budget on the same evidence without new information.
- **Human review instead of repeated AI loops**: every agent's low-confidence or failed path terminates in `review_items`, never in a second AI call. This is the direct cost implication of the domain rule in blueprint §8.2 ("Intelligence must not write to `releases`, `sources`, or `products` directly") — the fallback for an uncertain AI result is a person, and a person is a fixed cost already staffed, not a marginal token cost.

### 4.6 Circuit breakers and degraded mode

A circuit breaker sits in front of every AI adapter call site and trips on any of:

- Global daily or monthly budget crossed (§4.1).
- Provider error rate above a threshold (protects against paying for a run against a degraded provider that is likely to fail schema validation anyway).
- A single vendor's per-day cap crossed (§4.1), scoped to that vendor only — other vendors' repair jobs are unaffected.

**Degraded mode**, i.e. what the system does with the breaker open: `CheckSource`, `DetectSourceChange`, and deterministic `ExtractCandidateRelease`/`ValidateCandidateRelease` continue to run exactly as before — none of them depend on AI. Sources with selector misses or `broken` status simply accumulate in `review_items` for human triage instead of being auto-repaired. The public site, the API, and the core update pipeline are **fully functional with AI unavailable**; only the auto-repair and discovery conveniences pause. This is the direct consequence of AI being an escalation path rather than a dependency (blueprint §1) — it is also why the breaker is safe to trip aggressively.

### 4.7 Metrics to track

| Metric | Why it matters |
| --- | --- |
| Tokens per run (input/output, by agent type) | Detects prompt bloat before it shows up as a bill |
| Cost per agent type | Answers "which agent is expensive" without waiting for the monthly invoice |
| Cost per source repair | The unit economics of keeping one source alive via AI, comparable against the cost of a human fixing the same selector |
| Cost per discovered product | The unit economics of AI-assisted bootstrap (§2.5's aggregate, divided by discovery output) |
| Cost per published release (AI-assisted share) | Isolates how much of §2.4's cost is attributable to AI escalation versus the deterministic path |
| AI escalation rate | Fraction of checks/extractions that triggered AI at all — the number that, if it trends upward, means either sources are degrading faster than expected or deterministic collectors need attention (assumption T6) |
| AI rejection rate | Fraction of AI proposals that a human reviewer rejects — a high rate means the model or the prompt needs work, not more budget |
| Savings attributable to deterministic processing | Modelled as `(checks_that_would_have_needed_ai_if_ai_were_the_default) * cost_per_ai_agent_run - actual_ai_spend` — makes the value of "AI as escalation, not default" visible as a number, not just an architectural preference |

---

## 5. Storage retention policy

Every data class the system produces has an explicit classification, retention period, storage tier, and deletion trigger. "Keep everything forever" is not a retention policy; it is the absence of one, and it is also directly a cost driver (S3/CloudWatch storage is billed per GB-month indefinitely — see [aws-cost-model.md](aws-cost-model.md)).

| Data class | Classification | Retention | Storage tier | Deletion trigger |
| --- | --- | --- | --- | --- |
| HTML artifacts (raw fetched content) | Reconstructable (re-fetchable from the source, but the *historical* copy is the evidence) | Latest N per source indefinitely (default N=5); older superseded copies 90 days | S3 Standard for the current/recent copies; S3 Standard-IA after 30 days without access | Lifecycle rule transitions by age; explicit delete when a `Release`'s evidence is superseded by a correction and the old artifact is no longer referenced by any published fact |
| PDF artifacts | Reconstructable, same as HTML | Same as HTML | Same as HTML | Same as HTML |
| JSON API responses (from `json_path`-style sources) | Reconstructable | Same as HTML | Same as HTML | Same as HTML |
| Screenshots (visual evidence, where captured) | Reconstructable | 90 days, or life of the referencing `Evidence` row if published — whichever is longer | S3 Standard-IA | Age-based lifecycle rule; retained beyond 90 days only while a live `Release` cites it |
| Collector logs | Temporarily required (debugging) | 30 days | CloudWatch Logs Standard | CloudWatch log-group retention setting; no manual deletion needed |
| AI inputs and outputs (`ai_runs` payloads) | Audit-required (cost and quality accountability) | 1 year in hot storage, then archived | S3 Standard-IA after 90 days; consider Glacier Flexible Retrieval beyond 1 year if audit needs allow slower access (rate not independently confirmed — see [aws-cost-model.md](aws-cost-model.md)) | Age-based lifecycle rule; never deleted before 1 year without an explicit legal/audit override |
| Audit events (`audit_events`: publish/withdraw/correct/API-key actions) | Permanent | Indefinite | PostgreSQL (small per-row footprint; append-only) with periodic export to S3 for cold, cheap long-term retention | Never auto-deleted; export-and-compact only moves the storage tier, not the fact |
| Source-check history (`source_checks`) | Temporarily required (feeds adaptive scheduling and source-health metrics) | 1 year in PostgreSQL (partition-ready by month per blueprint §11 rule 3), older aggregated into monthly rollups | PostgreSQL, then rollup table | Row-level rows older than 1 year deleted after their rollup is computed; the rollup itself is kept indefinitely (small) |
| Metrics (OpenTelemetry) | Disposable at fine grain, useful in aggregate | Raw: 15 days; downsampled (Prometheus long-term or equivalent): 13 months | Local Prometheus storage (self-hosted) / CloudWatch Metrics if used in AWS | Prometheus retention config; CloudWatch default retention tiers |
| Traces | Disposable | 7–14 days | Tempo (self-hosted) or equivalent | Tempo retention config |
| Dead-letter messages (jobs stuck in `dead` state) | Temporarily required (operational triage) | 30 days | PostgreSQL `jobs` table (or SQS DLQ in the AWS adapter) | Explicit cleanup job; never silently retried past the DLQ threshold, never silently deleted before the retention window so an incident review always has evidence |

### Content-addressed storage and artifact deduplication

`ArtifactStore` keys artifacts by the SHA-256 hash of their normalised content, not by `(source_id, fetched_at)`. Concretely: a fetch that returns bytes whose content hash already exists in the store does not write a second object — it writes a new `source_artifacts` row that *points at* the existing content-addressed object. This means:

- A source checked daily for a month that only actually changed twice stores two artifact bodies, not thirty, regardless of how many `source_checks` rows reference them.
- Deduplication happens naturally across sources too: two different vendor pages that happen to serve byte-identical boilerplate (a shared CDN error page, for instance) collapse to one stored object.
- **The rule, stated directly: an unchanged artifact is never stored twice.** This is enforced by the store's write path computing the hash before any write decision, not by a cleanup job reconciling duplicates after the fact — deduplication-after-the-fact would still have paid the storage cost for the window between write and cleanup.

---

## 6. Scaling thresholds

These are the measurements that should trigger an architecture change — not vibes, not calendar time. Each threshold already appears as a "revisit at" note somewhere in the blueprint; this table collects them in one place with the concrete change each one implies.

| Measurement | Threshold | Architecture change | Reference |
| --- | --- | --- | --- |
| PostgreSQL job queue throughput | Sustained enqueue rate above ~10/s, or queue tables exceeding ~5M rows, or a need for fan-out to multiple independent consumers | Swap the `JobQueue` port's adapter from PostgreSQL `SKIP LOCKED` to SQS. The domain and application layers do not change — this is the entire point of the port (blueprint §3.1) | [ADR-0015](../adr/0015-job-queue-port.md) |
| API instance count | More than ~4 concurrent API instances, or a customer contract requiring exact per-second rate enforcement | Introduce a shared rate-limit store (Redis or equivalent) so the effective limit stops scaling with instance count | blueprint §3.9 |
| Search latency | p95 search latency above ~200 ms at realistic catalogue size, or a genuine need for relevance tuning beyond `ts_rank` | Introduce a dedicated search index (OpenSearch or equivalent) fed from `product_summaries`; PostgreSQL remains the system of record | blueprint §3.10 |
| Lambda cost at sustained traffic | Measured monthly Lambda cost exceeds the equivalent Fargate cost for the same sustained request rate — computed, not guessed, from actual invocation counts and durations | Move the API (and/or worker) runtime from Lambda to Fargate behind the same load balancer pattern; the same Go binary runs unmodified in both (blueprint §3.4) | [ADR-0010](../adr/0010-aws-runtime.md) |
| Artifact volume | S3 storage growth rate outpaces the deduplication savings modelled in §5, or lifecycle transitions are not keeping the hot tier small | Tune lifecycle transition ages, tighten the "latest N per source" retention count, or reduce artifact TTL for low-value source classes | §5 above |
| Database size | Table size or query latency on `source_checks`, `candidate_releases`, or `releases` degrades past acceptable p95, or the working set no longer fits comfortably in the instance's memory | Partition the high-volume observation tables by month (already partition-ready per blueprint §11 rule 3) and/or add a read replica for read-heavy public-site traffic, keeping writes on the primary | blueprint §11 |

---

## 7. Cost-containment runbook

This is written to be followed under pressure, by whoever is on call, at 3 a.m., without needing to have memorised the rest of this document first.

### 7.1 Detection signals and alarms

Configure these on day one (cross-reference: [aws-cost-model.md](aws-cost-model.md) §11 for the AWS Budgets/CloudWatch alarm specifics):

- **AWS Budgets** alert at 50%, 80%, and 100% of the monthly budget, forecast-based where the tool supports it (catches a runaway trend before month-end, not after).
- **AI spend alarm**: CloudWatch alarm (or equivalent) on the daily `ai_runs.estimated_cost_usd` sum crossing 80% of the daily cap (§4.1), so the circuit breaker tripping at 100% is not the first anyone hears of it.
- **Anomalous request-rate alarm**: a spike in API Gateway or CloudFront request count well above the trailing baseline (candidate signal for denial-of-wallet, see aws-cost-model.md §10).
- **Lambda duration/error-rate alarm**: a spike in duration (runaway extraction against a pathological artifact) or error rate (retry storms against a poison job).
- **Queue depth alarm**: `jobs` table (or SQS) depth growing faster than it drains — an early signal of either a scheduling bug producing duplicate work or a downstream dependency (a specific vendor host) throttling checks into backoff en masse.

### 7.2 Triage

1. **Identify which budget alarm fired** (overall AWS spend, AI spend specifically, or a request-rate/anomaly alarm) — this determines which of the graduated responses below is relevant; do not apply an AI-spend response to a CloudFront egress spike.
2. **Check the metrics from §4.7 and the scaling measurements from §6** for the affected area: is this a genuine traffic/workload increase (a success problem), a bug (a scheduling loop, a retry storm), or abuse (denial-of-wallet, credential-stuffed API keys)?
3. **Check `ai_runs` and `jobs` for a hot loop**: a single source or a single job repeatedly failing and re-triggering AI or re-enqueuing is the most common self-inflicted cost incident, and is visible directly in those tables without needing AWS-side data.

### 7.3 Graduated response ladder

Apply the least disruptive step that addresses the signal; escalate only if it does not resolve within a reasonable window (minutes, not hours, for anything actively spending money).

1. **Tighten, don't stop**: reduce the affected budget cap (per-vendor, daily, or global — §4.1) to force the circuit breaker to trip sooner while investigation continues. This stops the bleeding without taking the whole AI path offline for unrelated vendors.
2. **Trip the circuit breaker manually** (§4.6) if a hot loop is confirmed and tightening the cap has not stopped it — the system falls back to degraded mode (deterministic-only), which is fully functional for the public site and API.
3. **Rate-limit or block the offending source of traffic** at the WAF layer (a specific IP range, ASN, or API key) if the signal is abuse rather than a legitimate workload increase — see §8 of [api-commercial.md](api-commercial.md) for the abuse-response path in the commercial context.
4. **Disable the specific vendor/source** in the registry (`enabled = false`) if a single source is the source of a scheduling or extraction cost anomaly (e.g. a pathological artifact that is expensive to parse repeatedly) — this is scoped and reversible, unlike stopping the worker entirely.
5. **Scale down or pause the affected component** (e.g. temporarily widen the scheduling bands globally, or pause the worker's dispatch loop) only if the above scoped responses do not contain the incident — this is the most disruptive step short of a full outage and should be treated as such.

### 7.4 Communication

- Post a short status note (internal channel, and the public status surface if customer-facing service is affected) as soon as triage identifies genuine customer impact — degraded freshness from widened scheduling bands is a real, disclosable impact, not an internal-only detail, because it affects the freshness commitments in blueprint §5.
- If the incident affects the AI-assisted repair/discovery path only, and the deterministic pipeline and API remain fully functional, say so explicitly — this distinction matters to anyone worried the whole site is affected.

### 7.5 Recovery and post-incident review

1. Confirm the triggering condition is resolved (loop stopped, abuse blocked, budget alarm cleared) before restoring any tightened cap or disabled source to its normal state — restore deliberately, one step at a time, watching the metrics from §4.7/§6 after each restoration, not all at once.
2. Write a short post-incident note covering: detection (which alarm, how long from onset to detection), root cause, the specific response steps taken from the ladder in §7.3, the dollar impact if known, and — this is the part that actually prevents a repeat — which threshold in §6, which cap in §4.1, or which retention rule in §5 should change as a result, if any.
3. If the incident revealed a genuinely new scaling threshold was crossed (§6), file the architecture change as a tracked decision, not as an ad hoc tweak — a threshold crossed once is a data point; a threshold crossed and not acted on is a recurring incident waiting to happen.

---

## Related documents

- [blueprint.md](blueprint.md) — the master document this one details
- [aws-cost-model.md](aws-cost-model.md) — the AWS service-level cost model that these controls keep small
- [api-commercial.md](api-commercial.md) — quota/entitlement mechanisms that are the commercial complement to the rate/quota controls in §3 and §7
- [ADR-0006 — AI as escalation](../adr/0006-ai-as-escalation.md), [ADR-0010 — AWS runtime](../adr/0010-aws-runtime.md), [ADR-0013 — Cost minimisation](../adr/0013-cost-minimization.md), [ADR-0015 — Job queue port](../adr/0015-job-queue-port.md)
