# ADR-0013: Cost minimisation as a functional requirement

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** founding team
- **Requires qualified legal review:** no
- **Related:** ADR-0010, ADR-0011, ADR-0015

## Context

FirmScout's stated technical differentiator is not the database schema — anyone can build one — it is a discovery and maintenance engine whose cost per monitored source is low enough that monitoring tens of thousands of sources is economically boring (§1). That is a cost claim, not a feature claim, and it only means anything if cost is treated as something the architecture is responsible for, not an afterthought measured after the fact. Assumption O4 (medium confidence) is that costs at low scale are dominated, in order, by egress, always-on compute, and AI calls. Multiple other ADRs already make cost-driven choices — Lambda over Fargate (ADR-0010), a PostgreSQL-backed queue over SQS (ADR-0015), self-hostable observability over SaaS (ADR-0011), no Redis, no OpenSearch — but those are individually justified elsewhere. This ADR states the principle that ties them together and makes it a standing requirement future decisions are checked against, not just a retrospective pattern noticed after the fact.

## Decision

**Cost minimisation is a functional requirement**, evaluated with the same seriousness as correctness or security when a design choice is made. Concretely, this means:

- **Every AWS service added to the architecture needs an explicit justification**, stated in terms of what it costs at zero traffic and what it costs at expected MVP traffic — not just what capability it adds. A service that is technically superior but meaningfully increases idle cost needs a stronger justification than one that is merely convenient.
- **Low idle cost is a design target, not an incidental property.** Nothing in the MVP requires always-on compute beyond a small database (§1) — this is why Lambda is chosen over Fargate (ADR-0010), why the job queue is PostgreSQL-backed rather than a separate broker (ADR-0015), and why Redis and OpenSearch are deferred (§3.9, §3.10).
- **Low cost per unit of work is tracked per meaningful unit**: cost per monitored source check, cost per search query, cost per API request, and cost per published release are each treated as metrics the system should be able to report, not just aggregate monthly spend. This is what makes the "boring at scale" claim falsifiable rather than aspirational.
- **Adaptive scheduling and storage lifecycle policies are cost controls, not just operational hygiene.** Sources are checked at a frequency derived from their own change history (a pure function from source history to next check interval, §8.1's scheduling policy), so a source that rarely changes is not polled as often as one that changes weekly, directly reducing egress and compute cost per source. Storage lifecycle policies (documented in `docs/diagrams/storage-lifecycle.md`) govern how long raw artifacts, source-check history, and observation-table rows are retained before archival or deletion, controlling storage cost growth as the system's history accumulates.

## Consequences

### Positive

- Cost minimisation as an explicit, standing requirement gives every future architectural proposal a concrete question to answer ("what does this cost at zero and at expected traffic") rather than relying on someone remembering to ask informally.
- Adaptive scheduling means the system's cost scales with actual source volatility, not with a flat polling interval applied uniformly — a source that changes rarely costs almost nothing to monitor over time, which is directly what makes "tens of thousands of sources" plausible as a cost claim.
- Tracking cost per check/search/request/release, rather than only aggregate spend, means a regression in unit economics (e.g., a change that doubles cost per check) is visible and attributable, not hidden inside a monthly total that could also be moving because of traffic growth.
- The discipline this ADR states is already reflected consistently across the architecture — Lambda, PostgreSQL-backed queue, no Redis, no OpenSearch, self-hostable observability — which means the codebase and the cost principle are not in tension; adopting this ADR formalises a pattern already present rather than imposing a new one.

### Negative

- Cost minimisation can pull against other legitimate priorities — reliability margin, operational simplicity, developer velocity — and a team under this constraint has to make real trade-offs rather than defaulting to "add the managed service that makes this easier." Several of those trade-offs are documented honestly elsewhere (e.g., ADR-0010's cold-start and cost-predictability trade-offs, ADR-0003's single point of failure).
- Requiring justification for every new AWS service adds friction to adopting genuinely useful managed services even when the cost is modest, which risks the team occasionally reinventing something a managed service would have solved better, out of cost-discipline habit rather than a real cost concern.
- Per-unit cost tracking (per check, per search, per request, per release) requires instrumentation and cost-allocation work that is not free to build — attributing AWS billing line items to specific units of work at a useful granularity is itself an engineering investment, not something that falls out of default billing dashboards.
- No AWS prices are asserted anywhere in this ADR or its companions — the actual dollar thresholds for decisions like the Lambda/Fargate crossover (ADR-0010) or the SQS migration (ADR-0015) require real, current pricing lookups at decision time, which this ADR cannot substitute for.

### Neutral

- This ADR does not itself introduce new mechanisms; it names the principle that other ADRs (0003, 0010, 0011, 0015, and the Redis/OpenSearch deferrals in §3.9–3.10) already individually apply, and gives future proposals a named requirement to be checked against.
- Cost dashboards, alerting on cost anomalies, and a formal cost-containment incident process are referenced in the blueprint's diagram set (`docs/diagrams/cost-containment-incident.md`) but their operational detail is out of scope for this ADR.

## Alternatives considered

### Treat cost as an operational concern reviewed periodically, not an architectural requirement

Rejected. This is the default posture for most projects, and it is exactly what produces the common failure mode of a system that works but has quietly become expensive to run at scale, discovered only when a bill spikes. Given that FirmScout's competitive claim is specifically about cost at scale, treating it as a periodic afterthought would be inconsistent with the product's own value proposition.

### Optimise for lowest possible cost unconditionally, even at the expense of reliability or developer experience

Rejected as too extreme. The blueprint's own architecture accepts real trade-offs in the other direction where warranted — for example, PostgreSQL as a single point of failure (ADR-0003) is accepted despite the reliability cost, because the *alternative* (a redundant, multi-region setup) would itself violate cost minimisation at MVP scale disproportionately to the actual risk. The requirement is to justify cost explicitly, not to minimise it at all costs.

### A hard monthly budget cap enforced automatically (e.g., automatic service shutdown if a spend threshold is crossed)

Considered for AI cost specifically (and adopted there, as circuit breakers per ADR-0006) but rejected as a blanket mechanism for the whole system, since automatically shutting down core infrastructure (the API, the database) on a budget breach would trade a cost problem for an availability incident, which is a worse outcome for a production catalogue people rely on.

## Revisit when

- Aggregate AWS spend or any single per-unit cost metric (per check, per search, per request, per release) grows faster than the corresponding traffic or catalogue growth, indicating a unit-economics regression that needs investigation before scale amplifies it.
- A specific managed service is repeatedly reinvented in-house due to this ADR's justification bar, and the cumulative engineering cost of maintaining the in-house version exceeds what the managed service would have cost.
- The Lambda/Fargate (ADR-0010) or PostgreSQL/SQS (ADR-0015) thresholds are approached, at which point real current AWS pricing should be pulled and the crossover recalculated rather than assumed from this ADR's qualitative framing.
