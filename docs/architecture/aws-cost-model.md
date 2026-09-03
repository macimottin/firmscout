# AWS cost model

> Companion to [blueprint.md](blueprint.md) §1 and §3.4, [cost-controls.md](cost-controls.md), and [ADR-0010 — AWS runtime](../adr/0010-aws-runtime.md) / [ADR-0013 — Cost minimisation](../adr/0013-cost-minimization.md).
>
> **This document requires validation against current AWS pricing before it informs any commercial commitment.** Every price below is either (a) retrieved from the AWS Price List API on **2026-09-03** for `us-east-1`, with the source URL cited, and marked "retrieved 2026-09-03, verify before relying on it"; or (b) a named rate variable (`$RATE_*`) left unresolved because it could not be retrieved in this session. **No price in this document is invented.** AWS list prices change without notice, do not reflect negotiated discounts, Savings Plans, Free Tier eligibility windows, or the AWS Free Tier's evolving terms — re-run the retrieval before using any number here in a customer-facing or budget-approval context.

---

## 1. Methodology

The model is built in three layers, in this order, deliberately:

1. **Workload drivers** (§2) — the *things FirmScout does*, expressed as counts and ratios that are independent of AWS: sources monitored, checks per day, releases published, page views, API requests. These are product and operational assumptions, not AWS facts.
2. **Consumed units per AWS service** (§4) — each workload driver translated into the specific billable unit each AWS service meters: Lambda invocations and GB-seconds, S3 GB-months and requests, RDS instance-hours, and so on. This translation is architecture-dependent (it assumes the Lambda-first runtime from [ADR-0010](../adr/0010-aws-runtime.md)) but still price-independent.
3. **Rates** (§4, cost column) — the retrieved-or-parameterised $/unit price applied to the consumed units from step 2.

This ordering matters: a workload assumption can be wrong and only affects step 1's inputs; a rate can change (AWS re-prices Lambda) and only affects step 3's multiplication. Nothing here hardcodes a "monthly AWS bill" figure — every total in this document is a formula evaluated against the stated assumptions, reproducible by anyone who re-runs it with their own numbers or with updated rates.

**Every workload number in §3 is a labelled planning assumption**, not a measurement — FirmScout has not launched. They are chosen to be defensible order-of-magnitude figures for a catalogue-plus-monitor product at three stages of maturity, and should be replaced with real numbers once `source_checks`, `analytics_events`, and `usage_records` exist (cross-reference: blueprint §2, assumptions T1, T2, T5).

---

## 2. Workload drivers

These are the independent variables every scenario in §3 sets a value for.

| Driver | What it measures | Feeds |
| --- | --- | --- |
| Monitored sources | Count of active `sources` rows (one source can cover many products, per cost-controls.md §1 rule 9) | Check volume, worker Lambda invocations |
| Check frequency distribution | How monitored sources are spread across the interval bands in cost-controls.md §3.2 | Checks per day |
| Fraction of checks returning 304 (or hash-unchanged) | Conditional-request effectiveness (cost-controls.md §1 rule 2) | Bytes fetched, extraction runs avoided |
| Fraction of checks that change | Real change rate | Extraction runs, candidate volume |
| Extraction runs per month | Sources where content changed and extraction ran | Lambda GB-seconds (worker) |
| Published releases per month | Candidates that passed validation (blueprint §16) | Database writes, cache invalidation, `product_summaries` refresh |
| Public page views per month | Anonymous web traffic (blueprint §5) | CloudFront requests/egress, cache-miss Lambda invocations |
| API requests per month | Authenticated + anonymous API traffic (blueprint §12) | API Gateway requests, Lambda GB-seconds (API), usage-metering writes |
| AI escalations per month | Checks/extractions that triggered an AI agent (cost-controls.md §4) | Token cost, a small amount of Lambda GB-seconds for orchestration |

---

## 3. Three scenarios

**All numeric values below are planning assumptions, explicitly not measurements.** They exist to give §4's formulas concrete inputs so the model produces a number instead of staying abstract; they are order-of-magnitude judgement calls, not forecasts anyone should budget against without revisiting them post-launch.

| Driver | Low (early launch) | Expected (established) | High (successful) |
| --- | --- | --- | --- |
| Monitored sources | 50 | 2,000 | 20,000 |
| Check frequency distribution (cost-controls.md §3.2 bands) | Mostly Standard (24h); no Critical | Mixed: 5% High, 70% Standard, 20% Low, 5% Dormant | Mixed: 10% High, 60% Standard, 25% Low, 5% Dormant |
| Avg. checks per source per day (blended from the distribution above) | ~1.0 | ~1.3 | ~1.6 |
| Fraction of checks returning unchanged (304 or hash match) | 90% | 95% | 96% |
| Fraction of checks that change | 10% | 5% | 4% |
| Extraction runs per month | 1,500 | 3,900 | 38,400 |
| Published releases per month | 300 | 1,200 | 9,000 |
| Public page views per month | 5,000 | 250,000 | 5,000,000 |
| API requests per month | 2,000 | 500,000 | 15,000,000 |
| AI escalations per month | 50 (bootstrap-heavy: mostly Discovery) | 150 (mostly Repair) | 600 (Repair + Classification at scale) |

Derivations shown for transparency: `checks per month = monitored_sources * avg_checks_per_day * 30`. Low: 50 × 1.0 × 30 = 1,500 checks/month. Expected: 2,000 × 1.3 × 30 = 78,000 checks/month. High: 20,000 × 1.6 × 30 = 960,000 checks/month. These check-volume figures feed §4's Lambda and queue rows directly.

---

## 4. Consumed units and cost, per scenario

Rates are cited once in §4.0 and referenced by symbol in the tables that follow, to avoid repeating ten citations per scenario. Where a rate could not be retrieved, the formula is given with the named variable left unresolved.

### 4.0 Rates used below (retrieved 2026-09-03, `us-east-1`, verify before relying on them)

| Symbol | Value | Source |
| --- | --- | --- |
| `RATE_LAMBDA_GB_SECOND_X86` | $0.0000166667 / GB-second | [aws.amazon.com/lambda/pricing](https://aws.amazon.com/lambda/pricing/) |
| `RATE_LAMBDA_GB_SECOND_ARM` | $0.0000133334 / GB-second | Same, ARM/Graviton2 rate |
| `RATE_LAMBDA_REQUEST` | $0.0000002 / request | Same |
| `RATE_APIGW_HTTP_REQUEST` | $0.000001 / request (first 300M/month) | [aws.amazon.com/api-gateway/pricing](https://aws.amazon.com/api-gateway/pricing/) |
| `RATE_CLOUDFRONT_REQUEST` | $0.000001 / HTTPS request (US) | [aws.amazon.com/cloudfront/pricing](https://aws.amazon.com/cloudfront/pricing/) |
| `RATE_CLOUDFRONT_EGRESS_GB` | $0.085 / GB, first 10 TB/month (US); tiered lower above that | Same |
| `RATE_RDS_T4G_MICRO_HOUR` | $0.016 / hour (Single-AZ, PostgreSQL) | [aws.amazon.com/rds/postgresql/pricing](https://aws.amazon.com/rds/postgresql/pricing/) |
| `RATE_RDS_T4G_SMALL_HOUR` | $0.032 / hour (Single-AZ, PostgreSQL) | Same |
| `RATE_RDS_GP3_GB_MONTH` | $0.115 / GB-month | Same |
| `RATE_AURORA_ACU_HOUR` | $0.12 / ACU-hour (Standard) | [aws.amazon.com/rds/aurora/pricing](https://aws.amazon.com/rds/aurora/pricing/) |
| `RATE_AURORA_STORAGE_GB_MONTH` | $0.10 / GB-month | Same |
| `RATE_AURORA_IO_MILLION` | $0.20 / million I/Os | Same |
| `RATE_S3_STANDARD_GB_MONTH` | $0.023 / GB-month, first 50 TB | [aws.amazon.com/s3/pricing](https://aws.amazon.com/s3/pricing/) |
| `RATE_S3_STANDARD_IA_GB_MONTH` | $0.0125 / GB-month | Same |
| `RATE_S3_PUT_1000` | $0.005 / 1,000 PUT/COPY/POST/LIST | Same |
| `RATE_S3_GET_1000` | $0.0004 / 1,000 GET and other | Same |
| `RATE_SQS_REQUEST` | $0.0000004 / request (Standard queue, first 100B/month) | [aws.amazon.com/sqs/pricing](https://aws.amazon.com/sqs/pricing/) |
| `RATE_SECRETSMANAGER_SECRET_MONTH` | $0.40 / secret / month | [aws.amazon.com/secrets-manager/pricing](https://aws.amazon.com/secrets-manager/pricing/) |
| `RATE_SECRETSMANAGER_API_10000` | $0.05 / 10,000 API calls | Same |
| `RATE_CLOUDWATCH_INGEST_GB` | $0.50 / GB ingested (Standard log class) | [aws.amazon.com/cloudwatch/pricing](https://aws.amazon.com/cloudwatch/pricing/) |
| `RATE_CLOUDWATCH_STORAGE_GB_MONTH` | $0.03 / GB-month | Same |
| `RATE_WAF_WEBACL_MONTH` | $5.00 / Web ACL / month | [aws.amazon.com/waf/pricing](https://aws.amazon.com/waf/pricing/) |
| `RATE_WAF_RULE_MONTH` | $1.00 / rule / month | Same |
| `RATE_WAF_REQUEST_MILLION` | $0.60 / million requests (base tier) | Same |
| `RATE_ROUTE53_HOSTED_ZONE_MONTH` | $0.50 / hosted zone / month (first 25) | [aws.amazon.com/route53/pricing](https://aws.amazon.com/route53/pricing/) |
| `RATE_ROUTE53_QUERY_MILLION` | $0.40 / million queries (first 1B/month) | Same |
| `RATE_DATA_TRANSFER_OUT_GB` | $0.09 / GB, first 10 TB/month (non-CloudFront "AWS Outbound") | [aws.amazon.com/ec2/pricing/on-demand](https://aws.amazon.com/ec2/pricing/on-demand/) |
| `RATE_XAZ_TRANSFER_GB` | $0.01 / GB, each direction (intra-region cross-AZ) | Same |
| `RATE_NATGW_HOUR` | $0.045 / hour (figure shown for US East (Ohio); not independently confirmed for N. Virginia specifically) | [aws.amazon.com/vpc/pricing](https://aws.amazon.com/vpc/pricing/) |
| `RATE_NATGW_GB` | $0.045 / GB processed (same caveat) | Same |
| `RATE_TOKEN_INPUT`, `RATE_TOKEN_OUTPUT` | **Not retrieved** — provider/model not yet chosen for FirmScout's AI adapters | Left as named variables; see [claude-api skill reference] for one current provider's published rates when a provider is selected |
| `RATE_S3_GLACIER_DEEP_ARCHIVE_GB_MONTH` | **Not retrieved** in this session | Left as a named variable |

### 4.1 Low scenario (early launch)

| Service | Consumed units | Formula | Cost (USD/month) |
| --- | --- | --- | --- |
| CloudFront requests | 5,000 page views × ~5 requests/view (HTML + API calls the page makes) ≈ 25,000 | `25,000 * RATE_CLOUDFRONT_REQUEST` | $0.03 |
| CloudFront egress | 5,000 views × ~0.3 MB avg response ≈ 1.5 GB | `1.5 * RATE_CLOUDFRONT_EGRESS_GB` | $0.13 |
| API Gateway requests | 2,000 API requests | `2,000 * RATE_APIGW_HTTP_REQUEST` | $0.002 |
| Lambda — worker (checks) | 1,500 checks × 2s avg × 0.5 GB memory = 1,500 GB-s; +1,500 invocations | `1,500 * RATE_LAMBDA_GB_SECOND_ARM + 1,500 * RATE_LAMBDA_REQUEST` | $0.02 + $0.0003 ≈ $0.02 |
| Lambda — extraction | 1,500 extraction runs × 3s × 0.5 GB = 2,250 GB-s | `2,250 * RATE_LAMBDA_GB_SECOND_ARM` | $0.03 |
| Lambda — API | 2,000 requests × 0.3s × 0.5 GB = 300 GB-s | `300 * RATE_LAMBDA_GB_SECOND_ARM` | $0.004 |
| Lambda — web (OpenNext SSR) | ~5,000 renders × 0.4s × 0.5 GB = 1,000 GB-s | `1,000 * RATE_LAMBDA_GB_SECOND_ARM` | $0.01 |
| RDS PostgreSQL | 1 × db.t4g.micro, 730 hours; 20 GB gp3 | `730 * RATE_RDS_T4G_MICRO_HOUR + 20 * RATE_RDS_GP3_GB_MONTH` | $11.68 + $2.30 = **$13.98** |
| S3 storage (artifacts, dedup'd per cost-controls.md §5) | ~2 GB | `2 * RATE_S3_STANDARD_GB_MONTH` | $0.05 |
| S3 requests | ~3,000 PUT + 3,000 GET | `3 * RATE_S3_PUT_1000 + 3 * (RATE_S3_GET_1000*10)`* | ~$0.03 |
| SQS requests | ~4,500 (enqueue+dequeue+delete per check) | `4,500 * RATE_SQS_REQUEST` | $0.002 |
| Secrets Manager | 3 secrets (DB credentials, AI provider key, signing key) | `3 * RATE_SECRETSMANAGER_SECRET_MONTH` | $1.20 |
| CloudWatch Logs | ~0.5 GB ingested, 0.5 GB stored | `0.5*RATE_CLOUDWATCH_INGEST_GB + 0.5*RATE_CLOUDWATCH_STORAGE_GB_MONTH` | $0.25 + $0.015 ≈ $0.27 |
| WAF | 1 Web ACL, 5 rules, 25,000 requests | `RATE_WAF_WEBACL_MONTH + 5*RATE_WAF_RULE_MONTH + 0.025*RATE_WAF_REQUEST_MILLION` | $5 + $5 + $0.015 = $10.02 |
| Route 53 | 1 hosted zone, ~25,000 queries | `RATE_ROUTE53_HOSTED_ZONE_MONTH + 0.025*RATE_ROUTE53_QUERY_MILLION` | $0.50 + $0.01 = $0.51 |
| AI (50 escalations/month) | `50 * cost_per_ai_agent_run` | `50 * (RATE_TOKEN_INPUT*input_tokens + RATE_TOKEN_OUTPUT*output_tokens)` | **unresolved** — provider not chosen |
| **Subtotal (excluding AI)** | | | **≈ $27** |

\* S3 GET rate is per 1,000 at `RATE_S3_GET_1000`; shown scaled for readability.

At this scale the bill is **dominated by the fixed monthly floors**: RDS ($13.98), WAF ($10.02 — almost entirely the flat Web ACL + rule fees, not the request volume), and Secrets Manager ($1.20). Usage-based line items (Lambda, CloudFront, S3, SQS) round to fractions of a dollar because the workload is genuinely tiny. **The highest-leverage optimisation at this scale is not usage efficiency — it's removing or deferring flat-fee services** (see §7, the lowest-cost MVP).

### 4.2 Expected scenario (established)

| Service | Consumed units | Formula | Cost (USD/month) |
| --- | --- | --- | --- |
| CloudFront requests | 250,000 views × 5 ≈ 1,250,000 | `1,250,000 * RATE_CLOUDFRONT_REQUEST` | $1.25 |
| CloudFront egress | 250,000 × 0.3 MB ≈ 75 GB | `75 * RATE_CLOUDFRONT_EGRESS_GB` | $6.38 |
| API Gateway requests | 500,000 | `500,000 * RATE_APIGW_HTTP_REQUEST` | $0.50 |
| Lambda — worker (checks) | 78,000 checks × 2s × 0.5 GB = 78,000 GB-s | `78,000 * RATE_LAMBDA_GB_SECOND_ARM + 78,000*RATE_LAMBDA_REQUEST` | $1.04 + $0.016 ≈ $1.06 |
| Lambda — extraction | 3,900 runs × 3s × 0.5 GB = 5,850 GB-s | `5,850 * RATE_LAMBDA_GB_SECOND_ARM` | $0.08 |
| Lambda — API | 500,000 × 0.3s × 0.5 GB = 75,000 GB-s | `75,000 * RATE_LAMBDA_GB_SECOND_ARM` | $1.00 |
| Lambda — web (SSR) | 250,000 × 0.4s × 0.5 GB = 50,000 GB-s | `50,000 * RATE_LAMBDA_GB_SECOND_ARM` | $0.67 |
| RDS PostgreSQL | 1 × db.t4g.small, 730h; 100 GB gp3 (or Aurora — see §8) | `730*RATE_RDS_T4G_SMALL_HOUR + 100*RATE_RDS_GP3_GB_MONTH` | $23.36 + $11.50 = **$34.86** |
| S3 storage | ~40 GB (dedup'd) | `40 * RATE_S3_STANDARD_GB_MONTH` | $0.92 |
| S3 requests | ~120,000 PUT + 120,000 GET | | ~$0.65 |
| SQS requests | 78,000 × 3 ≈ 234,000 | `234,000 * RATE_SQS_REQUEST` | $0.09 |
| Secrets Manager | 3 secrets | | $1.20 |
| CloudWatch Logs | ~15 GB ingested, 10 GB stored | `15*0.50 + 10*0.03` | $7.50 + $0.30 = $7.80 |
| WAF | 1 Web ACL, 5 rules, 1.25M requests | `5 + 5 + 1.25*0.60` | $10.75 |
| Route 53 | 1 zone, 1.25M queries | `0.50 + 1.25*0.40` | $1.00 |
| AI (150 escalations) | `150 * cost_per_ai_agent_run` | | **unresolved** |
| **Subtotal (excluding AI)** | | | **≈ $67** |

At this scale, **RDS is now the single largest line item**, with CloudWatch log ingestion a close second and rising with traffic (a common surprise — see §10). CloudFront and Lambda remain small because most public traffic is cache hits and most API traffic is short Lambda invocations behind API Gateway's cheap per-request pricing. **The highest-leverage optimisation here is CloudWatch log sampling/filtering** (structured logs at appropriate levels, not verbose per-request debug logging in production) and confirming the cache-hit ratio assumption behind the CloudFront figures is actually holding.

### 4.3 High scenario (successful)

| Service | Consumed units | Formula | Cost (USD/month) |
| --- | --- | --- | --- |
| CloudFront requests | 5,000,000 views × 5 = 25,000,000 | `25,000,000 * RATE_CLOUDFRONT_REQUEST` | $25.00 |
| CloudFront egress | 5,000,000 × 0.3 MB ≈ 1,500 GB | tiered: first 10 TB at $0.085 | $127.50 |
| API Gateway requests | 15,000,000 | `15,000,000 * RATE_APIGW_HTTP_REQUEST` | $15.00 |
| Lambda — worker (checks) | 960,000 checks × 2s × 0.5 GB = 960,000 GB-s | | $12.80 + $0.19 ≈ $13.00 |
| Lambda — extraction | 38,400 runs × 3s × 0.5 GB = 57,600 GB-s | | $0.77 |
| Lambda — API | 15,000,000 × 0.3s × 0.5 GB = 2,250,000 GB-s | | $30.00 |
| Lambda — web (SSR) | 5,000,000 × 0.4s × 0.5 GB = 1,000,000 GB-s | | $13.33 |
| Database (Aurora Serverless v2 recommended at this scale — see §8) | Variable ACU, modelled at avg 4 ACU sustained + bursts to 16 ACU | `avg_ACU * 730 * RATE_AURORA_ACU_HOUR + storage*RATE_AURORA_STORAGE_GB_MONTH + IO_millions*RATE_AURORA_IO_MILLION` | 4×730×0.12 = $350.40 (compute) + ~500GB×0.10=$50 (storage) + I/O (workload-dependent, formula only) |
| S3 storage | ~400 GB (dedup'd) | `400 * RATE_S3_STANDARD_GB_MONTH` | $9.20 |
| S3 requests | ~1.2M PUT + 1.2M GET | | ~$6.5 |
| SQS requests | 960,000 × 3 ≈ 2,880,000 | `2,880,000 * RATE_SQS_REQUEST` | $1.15 |
| Secrets Manager | 3–5 secrets | | ~$2.00 |
| CloudWatch Logs | ~150 GB ingested, 80 GB stored (**needs active filtering at this volume**) | `150*0.50 + 80*0.03` | $75.00 + $2.40 = $77.40 |
| WAF | 1 Web ACL, 8 rules (more abuse rules at scale), 25M requests | `5 + 8 + 25*0.60` | $28.00 |
| Route 53 | 1 zone, 25M queries | `0.50 + 25*0.40` | $10.50 |
| AI (600 escalations) | `600 * cost_per_ai_agent_run` | | **unresolved** |
| **Subtotal (excluding AI)** | | | **≈ $704** |

At this scale, **the database and CloudWatch logging dominate**, followed by CloudFront egress and API Gateway/Lambda together (which scale roughly linearly with traffic and are the "normal" cost of doing that much business). CloudWatch log ingestion at $75/month for 150 GB is exactly the kind of line item that looks trivial at low scale and becomes a top-three cost once request volume multiplies logging volume with it — see §10. **The highest-leverage optimisation at this scale is (a) aggressive log sampling/level tuning and (b) revisiting the Lambda-vs-Fargate threshold from cost-controls.md §6**, since sustained API Lambda cost ($30/month for API alone, growing with traffic) is exactly the measured signal that threshold asks for.

---

## 5. What dominates, and the single highest-leverage optimisation, by scale

| Scale | What dominates | Highest-leverage optimisation |
| --- | --- | --- |
| Low | Fixed monthly floors: RDS instance-hours, WAF's flat Web ACL/rule fees, Secrets Manager's per-secret fee — not usage | Defer or consolidate flat-fee services (§7); the database floor is close to irreducible, but WAF's flat fee is a real candidate for "add it when there's traffic worth protecting," not day one |
| Expected | RDS instance cost, rising CloudWatch log ingestion | Log level/sampling discipline before it becomes a top-line cost; validate the CloudFront cache-hit assumption, since a lower-than-modelled hit ratio directly multiplies the API Lambda line |
| High | Database (compute or ACU-hours), CloudWatch ingestion, then CloudFront egress and API Lambda together | Log sampling is now a top-three cost lever; re-evaluate Lambda vs. Fargate for the API using cost-controls.md §6's measured threshold, not a guess; confirm artifact dedup (cost-controls.md §5) is holding as source count grows, since undetected dedup failure silently multiplies S3 growth |

---

## 6. What scales to zero, and what is always on

**Scales to zero (or arbitrarily close):**

- Lambda (API, worker, web via OpenNext) — no invocations, no cost, by design of the runtime choice ([ADR-0010](../adr/0010-aws-runtime.md)).
- API Gateway, SQS, CloudFront requests/egress — all metered per unit consumed.
- S3 requests — metered per operation (storage itself does not scale to zero once artifacts exist, but growth is bounded by the retention and dedup policy in cost-controls.md §5).
- AI spend — genuinely zero when the circuit breaker (cost-controls.md §4.6) has not tripped and no escalation has fired; this is a deliberate design property, not an accident of low traffic.

**Always on (the irreducible floor):**

- **PostgreSQL** (RDS or Aurora) — almost certainly the floor. Even the smallest instance (`db.t4g.micro`) accrues cost every hour it exists, whether or not a single query runs against it, because the database holds state that must survive between invocations of an otherwise-stateless compute layer. This is the direct, unavoidable cost of the architecture's one stateful dependency (blueprint §1, §3).
- **Secrets Manager** — a flat per-secret monthly fee regardless of read volume; small in absolute terms ($0.40 × N secrets) but non-zero and non-negotiable while the secrets exist.
- **Route 53 hosted zone** — $0.50/month flat, trivial but real, and only avoidable by not using Route 53 for DNS at all.
- **WAF**, if deployed — the $5.00 Web ACL fee plus $1.00 per rule is flat regardless of request volume, which is why it is called out in §7 as a candidate to defer for a genuinely zero-traffic MVP.

**The floor, concretely, for the Low scenario in §4.1**: RDS ($13.98) + Secrets Manager ($1.20) + Route 53 ($0.50) ≈ **$15.68/month before a single visitor arrives**, plus whatever of WAF's flat fees are enabled. Everything else in that scenario's $27 subtotal is usage-driven and would shrink further with zero traffic.

---

## 7. The lowest-cost viable MVP

The absolute minimum AWS footprint that can serve the product described in blueprint §1–§5:

- **Compute**: Lambda for API, worker, and web (OpenNext) — no change from the documented architecture; this is already the low-idle-cost choice.
- **Database**: a single `db.t4g.micro` RDS PostgreSQL instance, Single-AZ, no read replica, gp3 storage sized to the actual dataset (tens of GB at launch). No Multi-AZ failover — accept a manual-recovery RTO in exchange for not paying for a standby that mirrors the primary's cost.
- **Storage**: S3 Standard only; skip Standard-IA/Glacier lifecycle transitions until artifact volume (§6 of cost-controls.md) justifies the operational complexity of tiering — at MVP volume the savings are cents.
- **DNS/CDN**: Route 53 for the hosted zone (unavoidable, and cheap); CloudFront in front of the web app and API, since it is the same or cheaper per-GB than direct Lambda/API Gateway egress (cost-controls.md §1, rule 12's citation) and provides the caching that makes the public site cheap.
- **What it sacrifices**:
  - **WAF**: deferred. Public-launch abuse resistance relies on CloudFront's built-in DDoS protection (AWS Shield Standard, included at no extra cost) and the application-layer rate limiting in blueprint §3.9, accepting a materially weaker edge defence than a configured WAF would provide. This is an explicit, revisitable trade — not a permanent decision — and should be revisited before any material public-launch marketing push, not after an incident.
  - **High availability on the database**: a single-AZ instance is a single point of failure. Acceptable for a pre-revenue MVP with a documented backup/restore procedure; not acceptable once paying customers have freshness SLAs (blueprint §5) that a database outage would breach.
  - **Read replica / dedicated search**: none. Full-text search runs directly against the primary (blueprint §3.10); acceptable until the search-latency threshold in cost-controls.md §6 is measured, not assumed, to be a problem.
  - **Observability retention depth**: minimal CloudWatch retention, or self-hosted Prometheus/Loki/Tempo/Grafana on a small footprint instead of managed alternatives, per blueprint §1's "runs identically on a laptop and in AWS" — the self-hosted stack itself needs *somewhere* to run in AWS if not colocated with the app, which is its own small floor if not folded into the existing compute.

This MVP footprint's monthly floor is essentially the Low scenario's fixed costs in §4.1 minus WAF's flat fee: **RDS ($13.98) + Secrets Manager (~$1.20) + Route 53 ($0.50) ≈ $15.68/month**, plus genuinely usage-proportional CloudFront/Lambda/S3/SQS costs that stay in the low single dollars at MVP traffic.

---

## 8. Aurora Serverless v2 versus a fixed RDS instance

| Dimension | RDS fixed instance (`db.t4g.micro`/`small`) | Aurora Serverless v2 |
| --- | --- | --- |
| Billing model | Per instance-hour, flat regardless of load ($0.016 or $0.032/hour retrieved 2026-09-03) | Per ACU-hour, scales with actual load ($0.12/ACU-hour Standard, retrieved 2026-09-03) |
| Minimum practical footprint | The instance class itself is the floor — `db.t4g.micro` at $0.016/hour ≈ $11.68/month is genuinely the smallest unit | **The minimum-ACU floor problem**: Aurora Serverless v2's minimum capacity can be configured as low as 0 ACU (auto-pause) in principle, but a continuously available database in practice needs a non-zero floor — even at the smallest realistic always-warm setting of 0.5 ACU, that is 0.5 × $0.12 × 730 ≈ **$43.80/month**, nearly *four times* `db.t4g.micro`'s cost, before a single query runs |
| Storage | gp3, $0.115/GB-month | Aurora-native, $0.10/GB-month (slightly cheaper per GB) plus per-million-I/O charges ($0.20/million) that a fixed-instance gp3 volume does not separately meter |
| Scale-up behaviour | Manual: resize the instance class, which involves a brief failover-like interruption | Automatic, in fine increments (as small as 0.5 ACU), no manual intervention, genuinely useful for bursty or unpredictable load |
| Where it wins | **Predictable, small, always-on workloads** — exactly FirmScout's MVP and Low-scenario profile, where the workload is small enough that Aurora's finer-grained billing does not overcome its higher floor | **Workloads with real burstiness** — a sudden traffic spike (a product going viral, a large customer's bulk-lookup batch job) that would otherwise require either over-provisioning a fixed instance or accepting degraded performance during the spike |
| Honest verdict for FirmScout | **RDS `db.t4g.micro`/`small` is the better choice through the Low and most of the Expected scenario**, precisely because of the minimum-ACU floor problem: Aurora Serverless v2's floor costs roughly 4× a `db.t4g.micro` instance for a workload that, at MVP scale, does not have the burstiness that would make Serverless v2's elasticity pay for itself. | **Aurora Serverless v2 becomes attractive once the workload is genuinely variable** — e.g. the High scenario's mix of steady catalogue traffic plus occasional heavy bulk-API usage from Professional/Enterprise customers, where paying for peak capacity around the clock (a large fixed instance) would cost more than paying ACU-hours that flex with the actual load. The crossover point should be measured (compare modelled Aurora ACU-hour cost at observed load against the equivalent fixed-instance cost), not assumed. |

**Recommendation, stated plainly**: start on a fixed RDS instance. Move to Aurora Serverless v2 only when either (a) measured load is bursty enough that a fixed instance sized for the peak is wasted most of the time, or (b) the operational cost of manual instance resizing during growth outweighs Aurora's higher floor — both are §6-style measured thresholds in cost-controls.md's framework, not calendar-driven decisions.

---

## 9. The self-hosted alternative

FirmScout's own architecture already makes this concrete, not hypothetical: Docker Compose with a PostgreSQL-backed job queue (blueprint §1, [ADR-0015](../adr/0015-job-queue-port.md)) is not a development convenience layered on top of an AWS-only design — it is a first-class deployment target using the *same binaries*.

**What it takes**: a single modest VM (2–4 vCPU, 4–8 GB RAM is comfortably enough for the MVP's workload — the API and worker binaries are small Go processes, and PostgreSQL is the only component with real resource needs at this scale) running `docker compose up` with the services from `infrastructure/docker/`: `postgres`, `api`, `worker`, `web`, and optionally the observability profile (`otel-collector`, `prometheus`, `loki`, `tempo`, `grafana`). A reverse proxy (Caddy or nginx) in front for TLS termination replaces CloudFront/API Gateway; a cron-equivalent or the worker's own scheduler loop replaces EventBridge Scheduler; the PostgreSQL-backed `JobQueue` adapter replaces SQS with zero code change (this substitution is the entire reason that port exists).

**Cost**: a single mid-tier VM from any mainstream provider is commonly in the range of $20–$80/month depending on specs and provider, plus bandwidth — this is a rough order-of-magnitude planning note, not a retrieved AWS price (it is explicitly *not* an AWS cost, since this is the non-AWS alternative), and should be checked against whichever provider is actually being considered.

**When self-hosting is the right answer**:

- **A self-hoster running their own instance of the open-source project** (blueprint §1's second pillar — "an open-source platform that anybody can self-host") — this is the primary intended audience for this deployment mode, not a fallback.
- **Bootstrap/pre-revenue operation of the hosted service itself**, if the founder wants to avoid any AWS spend before validating demand — a single VM can serve the Low scenario's entire workload (§3, §4.1) without strain, and the PostgreSQL-backed queue comfortably handles that job volume (cost-controls.md §6's 10 jobs/second threshold is far above what the Low scenario needs).
- **When the operational simplicity of "one VM, one `docker compose up`" outweighs AWS's elasticity** — no IAM, no VPC design, no multi-service Terraform module to maintain, at the cost of manual scaling, manual backup management, and no CloudFront-grade edge caching or DDoS absorption.

**When it is the wrong answer**: once traffic or reliability requirements exceed what a single VM and a human on-call can absorb — no automatic scaling, no multi-AZ resilience, no edge caching for a geographically distributed audience, and every AWS-native cost control in cost-controls.md (budgets, circuit breakers, WAF) has to be reimplemented or done without. The crossover is the same kind of measured threshold as cost-controls.md §6: sustained load, uptime requirements, or team capacity to operate infrastructure manually.

---

## 10. Financial risks

| Risk | Mechanism | Mitigation |
| --- | --- | --- |
| **Denial-of-wallet through the public site** | An attacker (or a scraper indifferent to cost) drives enough anonymous traffic that CloudFront/Lambda/API Gateway usage-based costs spike, without ever crossing a request-rate abuse *pattern* threshold that a naive detector would catch | Layered rate limiting (blueprint §3.9): WAF rate rules at the edge absorb volumetric abuse before it reaches compute; CloudFront caching means a flood of *identical* requests is nearly free regardless of volume; AWS Budgets alarms (§11) catch the spend even if the traffic pattern itself looks superficially legitimate |
| **Runaway AI spend** | A hot loop (a flapping source repeatedly triggering repair, a malformed prompt causing retries) or a genuine surge in escalations exhausts the AI budget quickly, given token cost's high per-unit variance (cost-controls.md §2.5) | The full budget-cap ladder and circuit breaker in cost-controls.md §4; the daily/monthly global caps exist specifically because per-job and per-vendor caps alone do not bound aggregate exposure across many vendors failing independently |
| **Egress from a scraper** | The Fetcher itself, not just inbound traffic, generates egress-adjacent cost: fetching large artifacts (a multi-MB vendor catalogue) from many sources on a tight schedule adds up, and a misconfigured source (`max_bytes` set too high, or absent) could fetch pathologically large responses repeatedly | `Fetcher`'s size limits and MIME validation (blueprint §7.3, adapters/fetch) cap per-fetch cost; conditional requests (cost-controls.md §1 rule 2) mean the *steady-state* cost of a stable source is near-zero regardless of its full size, since only changed sources pay the full-fetch cost |
| **CloudWatch log ingestion surprises** | Verbose per-request debug logging left on in production scales log volume with traffic, and log ingestion ($0.50/GB retrieved 2026-09-03) is one of the more expensive per-GB AWS services in this model — visible directly in §4.2/§4.3 where it becomes a top-three line item at the Expected and High scenarios | Structured logging at appropriate levels by default (info/warn/error in production, debug only via explicit runtime toggle); sampling for high-volume, low-value log lines; CloudWatch retention tuned per cost-controls.md §5's metrics/traces retention table, not left at "never expire" |
| **NAT Gateway costs, and how the architecture avoids needing one** | A NAT Gateway costs $0.045/hour flat (≈$32.85/month) plus $0.045/GB processed (retrieved 2026-09-03, figure shown for US East (Ohio) — not independently confirmed for N. Virginia) — a real, easy-to-overlook always-on cost that a VPC-with-private-subnets design would otherwise require for any Lambda function needing outbound internet access (e.g. to fetch vendor sources) | **The Lambda-first architecture in [ADR-0010](../adr/0010-aws-runtime.md) does not need a NAT Gateway**: functions that only need outbound internet access (the `Fetcher`, calling vendor sources) and AWS API access can run without being attached to a VPC at all, avoiding the NAT requirement entirely; only functions needing to reach the RDS instance directly (rather than through a VPC-less path) need VPC attachment, and RDS access from Lambda can be arranged without forcing every function through a NAT-gated private subnet. This is a design constraint worth actively protecting: **adding a VPC-attached Lambda that also needs general internet access is exactly the change that would silently reintroduce this cost**, and should be flagged in review. |
| **Cross-AZ traffic** | Any architecture with resources split across Availability Zones for resilience (a Multi-AZ RDS standby, or workers and a database in different AZs) pays $0.01/GB each direction for that traffic (retrieved 2026-09-03) — usually small in absolute terms at FirmScout's data volumes, but non-zero and easy to lose track of in a Multi-AZ design | Keep the MVP Single-AZ (§7) deliberately, accepting the availability trade-off documented there; if/when Multi-AZ is adopted for reliability, model the cross-AZ replication traffic explicitly rather than assuming it is negligible, using cost-controls.md §1 rule 12's same-region discipline as the baseline |

---

## 11. Budgets and alarms to configure on day one

Cross-reference: cost-controls.md §7.1 for the on-call detection/triage procedure these alarms feed into.

| Control | Configuration | Purpose |
| --- | --- | --- |
| AWS Budgets — overall monthly spend | Threshold set to roughly 2× the modelled Low-scenario floor (§4.1) initially, e.g. ~$50/month at launch, revised upward as the scenario in §3 that best matches reality changes; alerts at 50%, 80%, 100%, and forecast-to-exceed | Catches any spend trending above what the current scenario should cost, before the invoice arrives |
| AWS Budgets — service-scoped | A separate budget scoped to RDS alone (the largest fixed-cost service) and one scoped to CloudWatch (the surprise-prone service per §10) | Isolates which service is driving an overall-budget alert without waiting for cost-explorer analysis mid-incident |
| CloudWatch alarm — AI daily spend | On the daily sum of `ai_runs.estimated_cost_usd`, at 80% of the daily cap from cost-controls.md §4.1 | Early warning before the circuit breaker trips at 100%, per cost-controls.md §7.1 |
| CloudWatch alarm — Lambda error rate / duration | Per-function alarms on elevated error rate or p95 duration, tuned per function (worker vs. API vs. web) | Surfaces retry storms or pathological-input processing (§10's egress-adjacent risk) before they compound into a spend spike |
| CloudWatch alarm — anomalous request volume | Anomaly detection (or a static threshold well above the current scenario's modelled baseline from §3) on CloudFront and API Gateway request counts | The primary signal for denial-of-wallet (§10) |
| Cost Anomaly Detection (AWS Cost Explorer) | Enabled account-wide with alerting to the same channel as the budget alarms | Catches cost anomalies that do not map to a single obvious service-level alarm, including ones this document's model did not anticipate |

---

## Related documents

- [blueprint.md](blueprint.md) — the master document this one details
- [cost-controls.md](cost-controls.md) — the engineering practices that keep the consumed-units side of this model small
- [api-commercial.md](api-commercial.md) — the commercial model that this cost model's API-request driver feeds
- [ADR-0010 — AWS runtime](../adr/0010-aws-runtime.md), [ADR-0013 — Cost minimisation](../adr/0013-cost-minimization.md)
