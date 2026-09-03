# FirmScout Grafana dashboards

Provisioned by `../provisioning/dashboards/dashboards.yml` (a file provider
watching this directory) into the "FirmScout" folder. Every metric name used
below is defined once, in
[`docs/architecture/observability.md`](../../../../docs/architecture/observability.md)
§5, and reused verbatim — nothing here invents its own metric name.

The full specification for all seven dashboards — audience, refresh
interval, the decision each one supports, and every panel's exact query —
is [`docs/architecture/grafana-dashboards.md`](../../../../docs/architecture/grafana-dashboards.md).
This directory does not repeat that document; it implements it, partially.

## What is real and what is a stub

**Real, working dashboards** (genuine panels, genuine PromQL/SQL/LogQL
queries, matching the spec panel-for-panel):

| File | Dashboard | Spec section |
| --- | --- | --- |
| `platform-overview.json` | Platform overview | [§1](../../../../docs/architecture/grafana-dashboards.md#1-platform-overview) |
| `api-operations.json` | API operations | [§2](../../../../docs/architecture/grafana-dashboards.md#2-api-operations) |
| `collector-health.json` | Collector health | [§3](../../../../docs/architecture/grafana-dashboards.md#3-collector-health) |

**Stubs — explicitly not implemented.** Each file below loads in Grafana
(it is valid dashboard JSON, so provisioning does not error), but its only
content is a text panel saying so and pointing at the real specification. Do
not mistake a stub loading successfully in the Grafana UI for the dashboard
being built:

| File | Dashboard | Spec section |
| --- | --- | --- |
| `ai-operations.json` | AI operations | [§4](../../../../docs/architecture/grafana-dashboards.md#4-ai-operations) |
| `search-product-analytics.json` | Search and product analytics | [§5](../../../../docs/architecture/grafana-dashboards.md#5-search-and-product-analytics) |
| `cost-finops.json` | Cost and FinOps | [§6](../../../../docs/architecture/grafana-dashboards.md#6-cost-and-finops) |
| `abuse-scraping.json` | Abuse and scraping | [§7](../../../../docs/architecture/grafana-dashboards.md#7-abuse-and-scraping) |

Why these four and not the first three: AI operations and Cost/FinOps
depend on the `firmscout_ai_*` metrics group and the `firmscout_cost_rate_usd`
gauge (read from `infrastructure/observability/cost-rates.yaml`), neither of
which has a live data source yet — the AI agent adapters are ports and fakes
only in the MVP (observability.md §10), and `cost-rates.yaml` itself has not
been authored as part of this change (only the dashboards and alerting
infrastructure were in scope here). Search and product analytics and Abuse
and scraping both read almost entirely from PostgreSQL analytics-rollup
tables (`search_query_rollup_daily`, `product_view_rollup_daily`,
`vendor_view_rollup_daily`, `abuse_signals`) that exist only in
[`analytics.md`](../../../../docs/architecture/analytics.md)'s design, not
yet in `database/migrations/`. Building real panels against tables that do
not exist would produce a dashboard that fails at query time, which is worse
than an honest stub.

## Building out a stub

When the underlying data source lands (the `firmscout_ai_*` metrics start
being emitted, `cost-rates.yaml` is authored, or the analytics rollup tables
are migrated), replace the stub file's content with real panels following
the same structure as `platform-overview.json` / `api-operations.json` /
`collector-health.json` — panel-per-row from the spec table, exact query
text from the spec, correct `unit` and `thresholds` from the spec's Unit and
Thresholds columns. Do not edit the dashboard through the Grafana UI and
export it back (that workflow is legitimate for an exploratory incident
edit per grafana-dashboards.md's "Dashboard provisioning" section, but the
canonical path for planned work is editing the JSON directly).
