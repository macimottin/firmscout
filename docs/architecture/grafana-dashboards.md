# Grafana dashboards

> Companion to [`observability.md`](observability.md) (metrics, logs, and traces catalogue) and [`analytics.md`](analytics.md) (event and rollup catalogue). Every metric name below is defined once, in [`observability.md`](observability.md) §5, and reused here verbatim.

Every dashboard below states the decision it supports. A dashboard that supports no decision does not appear here — this list is intentionally the seven the blueprint asks for, not a larger "show everything Prometheus knows" set.

**Datasources referenced across these dashboards:** `Prometheus` (metrics, PromQL), `Loki` (logs, LogQL), `Tempo` (traces, linked from panels but not queried by PromQL/LogQL), `PostgreSQL` (registry counts and analytics rollups, plain SQL — used specifically where the data genuinely lives in Postgres per [`analytics.md`](analytics.md) §6, and is called out per panel), and, on AWS only, `CloudWatch` (infrastructure-level signals the OTel Collector cannot see, per [`observability.md`](observability.md) §2). A panel's query language is stated explicitly wherever it is not PromQL, since not every product question in this brief is shaped like a Prometheus time series.

---

## 1. Platform overview

**Purpose:** a single page answering "is this FirmScout instance alive, and is its catalogue growing" — the first thing a maintainer or a self-hoster checks.
**Audience:** maintainers, self-hosters, anyone doing a daily health check.
**Refresh interval:** 5m (catalogue size and daily activity, not an incident-response surface).
**Decision supported:** whether coverage is expanding at the rate the roadmap assumes, and whether the pipeline is actively producing — informs the weekly/monthly questions in [`analytics.md`](analytics.md) §7 about catalogue growth.

| # | Panel | Visualisation | Datasource | Query | Unit | Thresholds |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | Vendors | Stat | PostgreSQL | `SELECT count(*) FROM vendors WHERE managed_by = 'registry'` | short | — |
| 2 | Products | Stat | PostgreSQL | `SELECT count(*) FROM products` | short | — |
| 3 | Active sources | Stat | PostgreSQL | `SELECT count(*) FROM sources WHERE status = 'active'` | short | red if `< 1` |
| 4 | Published releases (all time) | Stat | PostgreSQL | `SELECT count(*) FROM releases WHERE withdrawn = false` | short | — |
| 5 | Source checks (24h) | Stat | Prometheus | `sum(increase(firmscout_collector_checks_total[24h]))` | short | — |
| 6 | Checks by outcome (24h) | Bar gauge | Prometheus | `sum by (outcome) (increase(firmscout_collector_checks_total[24h]))` | short | — |
| 7 | Releases published (7d) by vendor | Bar chart | Prometheus | `sum by (vendor) (increase(firmscout_collector_publications_total[7d]))` | short | — |
| 8 | Active API consumers (24h) | Stat | PostgreSQL | `SELECT count(DISTINCT api_key_id) FROM usage_records WHERE occurred_at > now() - interval '24 hours'` | short | — |
| 9 | Source health distribution | Pie chart | PostgreSQL | `SELECT status, count(*) FROM sources GROUP BY status` | short | red slice if `broken` share `> 20%` |

---

## 2. API operations

**Purpose:** the on-call triage surface for the public API and the site's own traffic.
**Audience:** platform engineers, on-call.
**Refresh interval:** 30s.
**Decision supported:** is the API healthy and fast enough right now, and are rate limiting/quotas doing their job — the dashboard an alert (see [`observability.md`](observability.md) §8) sends someone to first.

| # | Panel | Visualisation | Datasource | Query | Unit | Thresholds |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | Request rate by route | Time series | Prometheus | `sum by (route) (rate(firmscout_api_requests_total[5m]))` | reqps | — |
| 2 | Error rate | Stat + time series | Prometheus | `sum(rate(firmscout_api_requests_total{status_code=~"5.."}[5m])) / sum(rate(firmscout_api_requests_total[5m]))` | percentunit | green `< 0.01`, yellow `0.01–0.05`, red `> 0.05` |
| 3 | Latency p50 / p95 / p99 | Time series | Prometheus | `histogram_quantile(0.50, sum by (le) (rate(firmscout_api_request_duration_seconds_bucket[5m])))`, `histogram_quantile(0.95, ...)`, `histogram_quantile(0.99, ...)` | s | p95: green `< 0.5`, yellow `0.5–1`, red `> 1` |
| 4 | Requests by status code | Stacked time series | Prometheus | `sum by (status_code) (rate(firmscout_api_requests_total[5m]))` | reqps | — |
| 5 | Payload size distribution | Heatmap | Prometheus | `sum(increase(firmscout_api_payload_size_bytes_bucket[5m])) by (le)` | bytes | — |
| 6 | Rate-limit events | Time series | Prometheus | `sum by (scope) (rate(firmscout_api_rate_limit_events_total[5m]))` | ops | — |
| 7 | Quota violations by tier | Bar chart | Prometheus | `sum by (api_tier) (increase(firmscout_api_quota_violations_total[1h]))` | short | — |
| 8 | Cache hit ratio | Gauge | Prometheus | `sum(rate(firmscout_api_cache_results_total{result="hit"}[5m])) / sum(rate(firmscout_api_cache_results_total[5m]))` | percentunit | red `< 0.5`, yellow `0.5–0.8`, green `≥ 0.8` |
| 9 | Recent server errors | Logs | Loki | `{service_name="firmscout-api", level="error"} \| json` | — | — |

Panel 3's `route` label is always a **route pattern** (`/api/v1/products/{slug}`), never a resolved path — see the cardinality warning in [`observability.md`](observability.md) §5.

---

## 3. Collector health

**Purpose:** which vendor's collector needs attention right now, and how much of the pipeline is succeeding versus stalling before publication.
**Audience:** collector maintainers, contributors picking up maintenance work.
**Refresh interval:** 1m.
**Decision supported:** prioritisation of collector engineering effort — the dashboard that turns "sources newly broken" and "collector failure rate per vendor" alerts ([`observability.md`](observability.md) §8) into an actionable backlog.

| # | Panel | Visualisation | Datasource | Query | Unit | Thresholds |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | Sources by health status | Pie chart | PostgreSQL | `SELECT status, count(*) FROM sources GROUP BY status` | short | — |
| 2 | Success rate (24h) by vendor | Bar gauge | Prometheus | `sum by (vendor) (increase(firmscout_collector_checks_total{outcome!="failed"}[24h])) / sum by (vendor) (increase(firmscout_collector_checks_total[24h]))` | percentunit | red `< 0.5`, yellow `0.5–0.9`, green `≥ 0.9` |
| 3 | Checks over time by outcome | Stacked time series | Prometheus | `sum by (outcome) (rate(firmscout_collector_checks_total[15m]))` | ops | — |
| 4 | Execution duration p95 by vendor/stage | Time series | Prometheus | `histogram_quantile(0.95, sum by (le, vendor, stage) (rate(firmscout_collector_execution_duration_seconds_bucket[15m])))` | s | — |
| 5 | Extraction failures by vendor and reason | Bar chart | Prometheus | `sum by (vendor, reason) (increase(firmscout_collector_extraction_failures_total[24h]))` | short | — |
| 6 | Validation failures by gate | Bar chart | Prometheus | `sum by (gate) (increase(firmscout_collector_validation_failures_total[24h]))` | short | — |
| 7 | Duplicate candidates by vendor | Time series | Prometheus | `sum by (vendor) (rate(firmscout_collector_duplicate_candidates_total[1h]))` | ops | — |
| 8 | Stalest sources (least recently successfully checked) | Table | PostgreSQL | `SELECT vendor_slug, source_id, last_success_at FROM sources ORDER BY last_success_at ASC NULLS FIRST LIMIT 20` | — | row highlight if `last_success_at < now() - interval '48 hours'` |
| 9 | Recently broken sources | Table | PostgreSQL | `SELECT vendor_slug, source_id, broken_at, failure_class FROM sources WHERE status = 'broken' ORDER BY broken_at DESC LIMIT 20` | — | — |

---

## 4. AI operations

**Purpose:** is AI escalation earning its cost, and is spend under control.
**Audience:** engineering lead, whoever owns the AI budget.
**Refresh interval:** 5m.
**Decision supported:** whether confidence thresholds and per-job budget caps (blueprint §15) need tightening, and whether a specific vendor or trigger pattern is disproportionately driving AI spend — the dashboard behind the AI budget alert in [`observability.md`](observability.md) §8.

| # | Panel | Visualisation | Datasource | Query | Unit | Thresholds |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | Agent executions by agent/outcome (24h) | Bar chart | Prometheus | `sum by (agent, outcome) (increase(firmscout_ai_agent_executions_total[24h]))` | short | — |
| 2 | Success rate by agent | Gauge | Prometheus | `sum by (agent) (increase(firmscout_ai_agent_executions_total{outcome="success"}[24h])) / sum by (agent) (increase(firmscout_ai_agent_executions_total[24h]))` | percentunit | red `< 0.5`, yellow `0.5–0.8`, green `≥ 0.8` |
| 3 | Total cost (24h) | Stat | Prometheus | `sum(increase(firmscout_ai_cost_usd_total[24h]))` | currencyUSD | — |
| 4 | Cost by vendor (7d) | Bar chart | Prometheus | `sum by (vendor) (increase(firmscout_ai_cost_usd_total[7d]))` | currencyUSD | — |
| 5 | Cost by agent/model (7d) | Table | Prometheus | `sum by (agent, model_id) (increase(firmscout_ai_cost_usd_total[7d]))` | currencyUSD | — |
| 6 | Token consumption rate | Time series | Prometheus | `sum by (agent, token_type) (rate(firmscout_ai_tokens_total[1h]))` | ops | — |
| 7 | Rejected proposals by reason | Bar chart | Prometheus | `sum by (agent, reason) (increase(firmscout_ai_proposals_rejected_total[7d]))` | short | — |
| 8 | Cost per execution | Stat | Prometheus | `sum(increase(firmscout_ai_cost_usd_total[24h])) / sum(increase(firmscout_ai_agent_executions_total[24h]))` | currencyUSD | — |
| 9 | Daily budget burn | Gauge | Prometheus | `sum(increase(firmscout_ai_cost_usd_total[24h])) / firmscout_ai_budget_cap_usd{period="daily"}` | percentunit | green `< 0.8`, yellow `0.8–1.0`, red `≥ 1.0` |

---

## 5. Search and product analytics

**Purpose:** which aliases to add, which vendors/collectors to prioritise, which products are missing.
**Audience:** product, growth, the founding team's roadmap review.
**Refresh interval:** 1h (data is nightly-aggregated per [`analytics.md`](analytics.md) §3 — a faster refresh would show nothing new).
**Decision supported:** directly the weekly/monthly product questions in [`analytics.md`](analytics.md) §7 — this is the dashboard version of that section.

| # | Panel | Visualisation | Datasource | Query | Unit | Thresholds |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | Top searched queries (30d) | Table | PostgreSQL | `SELECT query_normalized, sum(search_count) AS searches FROM search_query_rollup_daily WHERE date >= now() - interval '30 days' GROUP BY query_normalized ORDER BY searches DESC LIMIT 25` | short | — |
| 2 | Top viewed products (30d) | Table | PostgreSQL | `SELECT product_slug, vendor_slug, sum(view_count) AS views FROM product_view_rollup_daily WHERE date >= now() - interval '30 days' GROUP BY product_slug, vendor_slug ORDER BY views DESC LIMIT 25` | short | — |
| 3 | Top viewed vendors (30d) | Bar chart | PostgreSQL | `SELECT vendor_slug, sum(view_count) AS views FROM vendor_view_rollup_daily WHERE date >= now() - interval '30 days' GROUP BY vendor_slug ORDER BY views DESC LIMIT 15` | short | — |
| 4 | Zero-result search rate over time | Time series | PostgreSQL | `SELECT date, sum(zero_result_count)::float / NULLIF(sum(search_count), 0) AS zero_result_rate FROM search_query_rollup_daily GROUP BY date ORDER BY date` | percentunit | yellow `> 0.1`, red `> 0.25` |
| 5 | Zero-result queries needing attention (30d) | Table | PostgreSQL | `SELECT query_normalized, sum(zero_result_count) AS misses FROM search_query_rollup_daily WHERE date >= now() - interval '30 days' GROUP BY query_normalized ORDER BY misses DESC LIMIT 25` | short | — |
| 6 | Search latency p95 | Time series | Prometheus | `histogram_quantile(0.95, sum(rate(firmscout_api_request_duration_seconds_bucket{route="/api/v1/search"}[1h])) by (le))` | s | green `< 0.2`, red `≥ 0.2` (per blueprint §3.10's stated revisit threshold) |
| 7 | Search-to-view conversion (7d) | Stat | PostgreSQL | `SELECT (SELECT sum(view_count) FROM product_view_rollup_daily WHERE date >= now() - interval '7 days')::float / NULLIF((SELECT sum(search_count) FROM search_query_rollup_daily WHERE date >= now() - interval '7 days'), 0)` | percentunit | — |

---

## 6. Cost and FinOps

**Purpose:** unit economics per activity — is the catalogue's cost-per-monitored-source still "boring" at current scale (blueprint §1), and where is the pressure building.
**Audience:** the founder / whoever owns the AWS bill.
**Refresh interval:** 1d.
**Decision supported:** whether to invest in caching, batching, or a source-scheduling change before a specific cost line grows unsustainably — ties directly to [ADR-0013](../adr/0013-cost-minimization.md).

> **These are estimates, not billing data.** Every panel on this dashboard multiplies a consumed-unit counter (requests, checks, publications, agent runs, storage bytes — all defined in [`observability.md`](observability.md) §5) by a rate configured in `infrastructure/observability/cost-rates.yaml` and exposed as the `firmscout_cost_rate_usd{unit_type}` gauge. No number here comes from an AWS or vendor invoice. Reconcile monthly against actual billing; a growing gap between this dashboard and the real bill is a signal the configured rates are stale, not that spend is out of control.

| # | Panel | Visualisation | Datasource | Query | Unit | Thresholds |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | Estimated search-serving cost (24h) | Stat | Prometheus | `sum(increase(firmscout_api_requests_total{route="/api/v1/search"}[24h])) * on() firmscout_cost_rate_usd{unit_type="search"}` | currencyUSD | — |
| 2 | Estimated API-serving cost (24h) | Stat | Prometheus | `sum(increase(firmscout_api_requests_total[24h])) * on() firmscout_cost_rate_usd{unit_type="api_request"}` | currencyUSD | — |
| 3 | Estimated source-check cost (24h) | Stat | Prometheus | `sum(increase(firmscout_collector_checks_total[24h])) * on() firmscout_cost_rate_usd{unit_type="source_check"}` | currencyUSD | — |
| 4 | Estimated cost per published release (24h) | Stat | Prometheus | `sum(increase(firmscout_collector_publications_total[24h])) * on() firmscout_cost_rate_usd{unit_type="published_release"}` | currencyUSD | — |
| 5 | Actual AI cost per agent run (24h) | Stat | Prometheus | `sum(increase(firmscout_ai_cost_usd_total[24h])) / sum(increase(firmscout_ai_agent_executions_total[24h]))` | currencyUSD | *(the most precise figure on this dashboard — derived from real token usage, not a flat rate estimate)* |
| 6 | Estimated monthly storage cost at current size | Stat | Prometheus | `sum(firmscout_db_storage_bytes) / (1024*1024*1024) * on() firmscout_cost_rate_usd{unit_type="storage_gb_month"}` | currencyUSD | — |
| 7 | Total estimated daily platform cost | Stat | Prometheus | `sum(increase(firmscout_api_requests_total[24h])) * on() firmscout_cost_rate_usd{unit_type="api_request"} + sum(increase(firmscout_collector_checks_total[24h])) * on() firmscout_cost_rate_usd{unit_type="source_check"} + sum(increase(firmscout_ai_cost_usd_total[24h]))` | currencyUSD | red if `> 2×` trailing-7-day average |
| 8 | 7-day cost trend | Time series | Prometheus | `sum(increase(firmscout_ai_cost_usd_total[1d])) + sum(increase(firmscout_collector_checks_total[1d])) * on() firmscout_cost_rate_usd{unit_type="source_check"}` (repeated per day over 7d) | currencyUSD | — |

---

## 7. Abuse and scraping

**Purpose:** is scraping or abusive traffic degrading service for real users, and is the layered defence ([ADR-0008](../adr/0008-scraping-resilience.md)) actually working.
**Audience:** platform/security engineer.
**Refresh interval:** 1m.
**Decision supported:** whether to tighten the WAF rules, the in-process token bucket, or the persisted quota layer (blueprint §3.9) — and, on AWS, whether CloudWatch shows the WAF layer catching what the app-level metrics show getting through.

| # | Panel | Visualisation | Datasource | Query | Unit | Thresholds |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | Anonymous vs. keyed request volume | Stacked time series | Prometheus | `sum by (api_tier) (rate(firmscout_api_requests_total[5m]))` | reqps | — |
| 2 | Throttled requests | Time series | Prometheus | `sum by (scope) (rate(firmscout_api_rate_limit_events_total[5m]))` | reqps | — |
| 3 | Quota violations (sustained heavy use) | Time series | Prometheus | `sum by (api_tier) (rate(firmscout_api_quota_violations_total[5m]))` | reqps | — |
| 4 | Expensive endpoint usage | Time series | Prometheus | `sum by (route) (rate(firmscout_api_requests_total{route=~"/api/v1/lookup\|/api/v1/search"}[5m]))` | reqps | — |
| 5 | WAF blocked/challenged requests | Time series | CloudWatch (AWS only) | `AWS/WAFV2` namespace, `BlockedRequests` and `CountedRequests` metrics, dimensioned by the FirmScout Web ACL | short | — *(no data locally — there is no WAF in Docker Compose; this is exactly the CloudWatch-only signal described in [`observability.md`](observability.md) §2)* |
| 6 | Top request volume by API key (1h) | Table | PostgreSQL | `SELECT api_key_prefix, sum(request_count) AS requests FROM usage_records WHERE occurred_at > now() - interval '1 hour' GROUP BY api_key_prefix ORDER BY requests DESC LIMIT 20` | short | row highlight if `requests` exceeds the consumer's tier quota |
| 7 | Suspected scraping (flagged IP-hash buckets) | Table | PostgreSQL | `SELECT ip_hash_bucket, request_count, first_seen, last_seen FROM abuse_signals WHERE flagged = true ORDER BY request_count DESC LIMIT 20` | short | — |

Panel 6 queries `api_key_prefix` — the 8-character display prefix from blueprint §11.10 — never the full key. Panel 7 reads from the rotating-salt-hashed, short-retention abuse-detection table described in [`analytics.md`](analytics.md) §4, which is deliberately kept out of the product-analytics tables in [`analytics.md`](analytics.md) §6.

---

## Dashboard provisioning

**Where the JSON lives:** `infrastructure/observability/grafana/dashboards/`, one file per dashboard (`platform-overview.json`, `api-operations.json`, `collector-health.json`, `ai-operations.json`, `search-product-analytics.json`, `cost-finops.json`, `abuse-scraping.json`). This repository owns the JSON; another process (`infrastructure/`) is responsible for actually building and maintaining those files — this document specifies their required content, it does not ship them.

**How datasources are provisioned as code:** Grafana's provisioning files in `infrastructure/observability/grafana/provisioning/datasources/*.yaml` declare all four (five, on AWS) datasources — Prometheus, Loki, Tempo, PostgreSQL (read-only role, scoped to the analytics rollup tables and the specific registry tables read above), and CloudWatch where applicable — with their connection details injected from environment configuration, identically to how the OTel Collector's exporters are configured per [`observability.md`](observability.md) §2. Grafana reads these files on container start; no datasource is ever added by hand through the UI.

**Dashboards are version-controlled, not edited in the UI.** Grafana's dashboard-provisioning mechanism (`infrastructure/observability/grafana/provisioning/dashboards/*.yaml`, pointing at the `dashboards/` directory above) loads the JSON files at startup. Any change to a dashboard — a new panel, an adjusted threshold, a query fix — is made by editing the JSON (or authoring it through the Grafana UI and using "Export for sharing externally," then committing the result) and opened as a pull request like any other change to this repository, reviewed the same way a change to `internal/application` would be. An exploratory edit made directly in the Grafana UI during an incident is legitimate in the moment, but is either re-exported back into the repository afterward or discarded before the next deploy — a dashboard that only exists in a running Grafana instance's database is one container restart away from not existing, and it is invisible to code review, which defeats the purpose of treating observability configuration as auditable per §1 of [`observability.md`](observability.md).
