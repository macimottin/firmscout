# ADR-0011: OpenTelemetry with a self-hostable Prometheus/Loki/Tempo/Grafana stack

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** founding team
- **Requires qualified legal review:** no
- **Related:** ADR-0013

## Context

FirmScout is designed to be self-hosted by anyone (§1), which means its observability story has to work identically for a contributor running `docker compose up` on a laptop and for the production AWS deployment — a proprietary SaaS observability product that only makes sense in the cloud, or that requires an account and network egress to function, would break local development parity and would add a recurring cost the self-hosted edition should not need to bear. At the same time, the system genuinely needs traces, metrics, and logs from day one: source checks, AI escalation cost, job queue depth, and API latency are all things the team needs visibility into to operate the platform responsibly, not optional instrumentation to add later.

## Decision

Application code is instrumented with the **OpenTelemetry SDK** for Go — traces, metrics, and logs via an `slog` bridge — emitting OTLP to an **OpenTelemetry Collector**. The Collector fans out to **Prometheus** (metrics), **Loki** (logs), and **Tempo** (traces), all visualized through **Grafana** with datasources and dashboards provisioned as code (not clicked together manually), so a fresh environment has working dashboards immediately. `otelhttp` instruments the standard-library HTTP router (ADR-0014) with no framework-specific adapter needed. This exact stack runs identically under Docker Compose locally and in AWS — the same OTel Collector configuration, the same Grafana provisioning, deployed as containers or managed equivalents, with no separate "local observability" and "production observability" story to maintain.

**Pyroscope** (continuous profiling) is named as available but **only added if justified** — it is not part of the MVP's default stack, added when a specific performance investigation need arises rather than instrumented preemptively everywhere.

## Consequences

### Positive

- A contributor debugging a failing source check locally sees the exact same trace shape, metric names, and log structure that production incident response would use — there is no "works differently on my machine" gap in observability itself, which is a common failure mode when local dev skips instrumentation for convenience.
- Self-hosted deployments get full observability with no external account, no per-seat SaaS pricing, and no data leaving the operator's own infrastructure — consistent with FirmScout's broader self-hosting commitment.
- OpenTelemetry's vendor-neutral instrumentation means the *application code* never needs to change if the backing systems change (Prometheus/Loki/Tempo swapped for a managed equivalent, or vice versa) — only the Collector's export configuration changes.
- Dashboards and datasources provisioned as code make the observability stack itself reviewable and versioned alongside the application, and reproducible for any contributor or self-hoster without manual Grafana configuration.
- `otelhttp`'s clean integration with the standard-library router (ADR-0014) was one of the deciding factors against Gin/Fiber, which don't compose as cleanly with it — this ADR and ADR-0014 reinforce each other.

### Negative

- Running Prometheus, Loki, Tempo, and Grafana is four additional services in the Compose stack (beyond the single PostgreSQL dependency, ADR-0003) for anyone who wants observability locally — heavier than the minimal footprint of the rest of the local stack, and a contributor who just wants to run the API and worker without observability needs an explicit lighter profile.
- Self-hosting this stack means the operator owns its storage growth and cardinality management — an unbounded label cardinality in metrics, or unbounded log volume, is now this project's operational problem rather than a managed vendor's, and needs retention and cardinality discipline built in rather than assumed.
- Four components (Collector, Prometheus, Loki, Tempo) plus Grafana is meaningfully more moving parts to keep versioned, upgraded, and configured correctly than a single SaaS agent/API key would be — the "no proprietary SaaS" decision trades an operational simplicity the team gives up deliberately for the self-hosting and cost goals it serves.
- Secrets appearing in logs is named explicitly as the main security risk of this component (§8.3) — self-hosted log aggregation makes this the operator's responsibility to guard against (structured logging discipline, redaction) rather than relying on a vendor's default redaction features, if any.

### Neutral

- This ADR does not commit to specific retention windows, storage backends for Loki/Tempo (e.g., local filesystem vs. object storage), or dashboard content beyond a "Platform overview" starting point — those are operational tuning decisions made as the platform's actual usage patterns become clear.

## Alternatives considered

### A proprietary observability SaaS (e.g., Datadog, New Relic, Honeycomb) for the MVP

Rejected for the MVP specifically because it breaks local/production parity for self-hosters — a SaaS product used in the reference production deployment but unavailable or cost-prohibitive for someone self-hosting means the project's own operational tooling is not something the community it depends on (assumption P5, O3) can fully exercise or learn from. It would also add a recurring per-seat or per-GB cost at a stage where cost minimisation is a functional requirement (ADR-0013). This is not a permanent rejection of any proprietary tool — see below.

### No structured observability stack at all in the MVP; rely on plain application logs

Rejected. The platform's core cost claim (cheap per-source monitoring at scale) is not verifiable without metrics on check outcomes, AI escalation cost, and queue depth from day one; retrofitting instrumentation after operational problems appear is more expensive than building it in from the start, and OpenTelemetry's Go SDK is mature enough that there is little "too early" argument against adopting it now.

### Building on a single all-in-one tool (e.g., Grafana Cloud's free tier, or a single-binary LGTM-stack distribution) rather than composing Prometheus/Loki/Tempo/Grafana separately

Considered as a lighter-weight local development option, but the production reference deployment needs each component sized and configured independently for its own workload (metrics cardinality, log volume, trace sampling), so composing them explicitly — even if a single-binary distribution is used for the lightest local profile — keeps the local and production configurations conceptually the same rather than diverging.

## Revisit when

- The self-hosted operating cost of running Prometheus/Loki/Tempo/Grafana (storage, compute) measurably exceeds what a managed observability SaaS would cost at the project's actual scale — the blueprint's own stated threshold for reconsidering a proprietary alternative (§8.3: "managed service only if operating cost exceeds hosting cost").
- Trace, metric, or log volume grows enough that cardinality or retention management becomes a recurring operational burden, requiring either more deliberate sampling/retention policy or a managed backend for one or more signal types.
- A specific, recurring performance investigation need justifies adding Pyroscope continuous profiling, rather than adding it preemptively.
