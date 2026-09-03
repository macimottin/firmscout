# High-level architecture

This diagram answers: what are the deployable and logical building blocks inside FirmScout, and how does a request or a background check move through them? It groups the modular monolith's runtime pieces (§10) into the layers the blueprint uses to reason about cost, security, and replacement (§8.3), from the AWS edge down to the observability stack that watches all of it.

```mermaid
flowchart TD
  subgraph edge["Edge (AWS)"]
    cf["CloudFront"]
    waf["WAF"]
  end

  subgraph frontend["Frontend"]
    web["Next.js public site<br/>(apps/web)"]
  end

  subgraph apilayer["API (Go)"]
    httpapi["HTTP handlers, middleware, problem+json<br/>(apps/api)"]
  end

  subgraph background["Background (Go worker)"]
    scheduler["Scheduler loop"]
    watchers["Watchers<br/>(due-check selection)"]
    fetchers["Fetchers<br/>(conditional GET, SSRF guard)"]
    extractors["Extractors<br/>(config and code collectors)"]
    validators["Validators<br/>(deterministic gates)"]
    publishers["Publishers<br/>(PublishRelease)"]
  end

  subgraph aiagents["AI agents (escalation only)"]
    discovery["Discovery Agent"]
    repair["Repair Agent"]
    validation["Validation Agent"]
    classification["Classification Agent"]
    sourcequality["Source Quality Agent"]
  end

  subgraph data["Data"]
    postgres[("PostgreSQL")]
    artifacts[("Artifact store<br/>(filesystem/PG large object, S3 later)")]
  end

  subgraph observability["Observability"]
    otelcol["OTel Collector"]
    prometheus["Prometheus"]
    loki["Loki"]
    tempo["Tempo"]
    grafana["Grafana"]
  end

  cf --> waf
  waf --> web
  waf --> httpapi
  web -->|"fetch, revalidate on publish"| httpapi
  httpapi --> postgres
  scheduler --> watchers
  watchers --> fetchers
  fetchers --> extractors
  extractors --> validators
  validators --> publishers
  fetchers --> artifacts
  publishers --> postgres

  validators -->|"low confidence, ambiguous match, or multi-source conflict"| validation
  fetchers -->|"source broken or relocated"| repair
  repair -.->|"proposal only, human-reviewed PR before deploy"| fetchers
  extractors -->|"release_type unresolved"| classification
  watchers -->|"new source registration"| sourcequality
  discovery -->|"writes review_items only, never publishes"| postgres

  httpapi --> otelcol
  scheduler --> otelcol
  otelcol --> prometheus
  otelcol --> loki
  otelcol --> tempo
  prometheus --> grafana
  loki --> grafana
  tempo --> grafana
```

## What this shows

Seven groupings, matching the blueprint's own component catalogue (§8.3): the AWS edge (CloudFront + WAF, ADR-0010), the Next.js frontend, the Go API, the worker's five-stage pipeline (scheduler → watchers → fetchers → extractors → validators → publishers, §16), the five AI agent ports that are escalation-only in the MVP (§7.3, §15), the single stateful store plus the artifact store, and the OTel-based observability stack (ADR-0011). The dashed edge from Repair back to Fetchers is deliberately weak — it represents a proposal that only reaches production through the PR and CI path in `source-repair-flow.md`, never a direct write. AI agents feed advisory or proposal signals into the pipeline (validation verdicts, classification labels, quality scores) but only Discovery is drawn writing to PostgreSQL, and even then only to `review_items`.

## Assumptions

- All AI agent boxes are drawn even though only fakes exist in the MVP (§7.3), because the diagram documents the target architecture, not only what is implemented today.
- The worker's five named stages are logical, not necessarily five separate goroutine pools; §10 and §16 describe them as pipeline stages within one binary.
- CloudFront/WAF terminate both the web and API paths, consistent with ADR-0010's "same binary behind CloudFront" approach.

## Failure modes

- A worker stage backs up (e.g. `extractors` behind slow parsing of large artifacts) — visible as job queue depth in Prometheus; see `adaptive-scheduling.md` for how backpressure is expressed as interval changes rather than a separate control plane.
- AI agent budget exhaustion degrades `validators` and `fetchers` escalation paths without stopping the deterministic pipeline; see `ai-cost-escalation.md`.
- Loss of the artifact store breaks re-extraction and repair (no `previous_artifact_ref`), but does not affect already-published releases, which are immutable rows in PostgreSQL.

## Related ADRs

- [ADR-0001 — Modular monolith](../adr/0001-modular-monolith.md)
- [ADR-0003 — PostgreSQL](../adr/0003-postgresql.md)
- [ADR-0006 — AI as escalation](../adr/0006-ai-as-escalation.md)
- [ADR-0010 — AWS runtime](../adr/0010-aws-runtime.md)
- [ADR-0011 — Open-source observability](../adr/0011-open-source-observability.md)
- [ADR-0015 — Job queue port](../adr/0015-job-queue-port.md)

## Implementing code

**Partially implemented.**

- `apps/*` — the four runtime components
- `internal/adapters/postgres` — data, queue and artifact metadata
- `internal/adapters/telemetry` — the observability edge

The AI agent box has ports and schemas but no runtime.

See [the architecture consistency report](../architecture/consistency-report.md) for what is verified by execution and what is not.
