# Update decision tree

This diagram answers: for one source, on one scheduled check, what is the full decision path from "is it due?" to "published, rejected, or routed to a human"? It is the operational heart of the platform — the path every one of the tens of thousands of sources the blueprint's cost model depends on (§1) walks through on every check, almost always without touching an AI model.

```mermaid
flowchart TD
  A{"Source due for check?"} -->|no| Z["Skip -- wait for next scheduled window"]
  A -->|yes| B["Select the cheapest available change signal"]
  B --> B1{"Signal type available for this source"}
  B1 -->|webhook| C1["Webhook payload received"]
  B1 -->|feed| C2["RSS/Atom feed poll"]
  B1 -->|"API cursor"| C3["API cursor / watermark check"]
  B1 -->|ETag| C4["Conditional GET (If-None-Match)"]
  B1 -->|"Last-Modified"| C5["Conditional GET (If-Modified-Since)"]
  B1 -->|sitemap| C6["Sitemap lastmod check"]
  B1 -->|"targeted section hash"| C7["Fetch and hash only the configured section"]
  B1 -->|"normalised content hash"| C8["Fetch, normalise, hash the full body"]
  B1 -->|"no cheap signal available"| C9["Full page compare"]

  C1 --> D{"Outcome"}
  C2 --> D
  C3 --> D
  C4 --> D
  C5 --> D
  C6 --> D
  C7 --> D
  C8 --> D
  C9 --> D

  D -->|unchanged| E1["Record source_check(unchanged), increment consecutive_unchanged"]
  D -->|unavailable| E2["Record failure, apply retry backoff"]
  D -->|unauthorized| E3["Mark source authentication_required"]
  D -->|rate_limited| E4["Honour Retry-After, back off"]
  D -->|redirected| E5["Record redirect target for review"]
  D -->|parser_failed| E6["Record failure -- selector or parse mismatch"]
  D -->|suspicious_content| E7["Quarantine artifact, route to review"]
  D -->|manual_review_required| E8["Route directly to human review"]
  D -->|changed| F["Extraction (deterministic collector)"]

  F --> G{"Duplicate detection:<br/>same product, normalised version, and channel already published?"}
  G -->|duplicate| G1["Update last_verified_at on the existing release, stop"]
  G -->|new| H{"Product matching (slug or alias resolution)"}
  H -->|"no match"| H1["Reject -- no product match"]
  H -->|ambiguous| H2["Route to human review -- ambiguous match"]
  H -->|resolved| I["Compute confidence (source trust, gate results)"]

  I --> J{"Confidence at or above the auto-publish threshold?"}
  J -->|"yes, official source"| J10{"Gate 10: does an eligible source at or above<br/>this source's authority tier report a different version?"}
  J -->|"no, or community source"| L["Validation Agent escalation (advisory only) -- NOT BUILT;<br/>the candidate routes straight to human review"]
  J10 -->|"no -- or the disagreement is outranked,<br/>and is named in the gate detail"| K["Auto-publish candidate"]
  J10 -->|"yes -- never auto-resolved (ADR-0020)"| CFL["Open or refresh a source_conflicts row,<br/>set product_summaries.has_source_conflict,<br/>attach at most ONE open review item"]
  CFL --> M
  L --> M{"Human review"}
  M -->|"accept (DecideReviewItem)"| K
  M -->|"reject (DecideReviewItem)"| Nrej["Reject candidate -- evidence retained"]
  K --> O["PublishRelease: insert Release and Evidence,<br/>refresh product_summaries.<br/>An advisory is stored but never marked latest."]
  M --> AUD["audit_events row: asserted actor,<br/>actor_authenticated = false, reason (ADR-0021)"]

  E2 --> P["Feed outcome into the source health state machine"]
  E3 --> P
  E4 --> P
  E5 --> P
  E6 --> P
  E7 --> M
  E8 --> M
  H2 --> M
```

## What this shows

The full per-check path in the order the objective specifies: due-check gate, cheapest-signal selection among ten concrete signal types (matching `T1` in §2 — most vendor sources expose at least one cheap signal), the nine possible check outcomes from `CheckSource`, and — only on `changed` — extraction, duplicate detection, product matching, confidence computation, the multi-source agreement gate, human review, and finally publish or reject.

Two branches deserve reading twice. **Gate 10 sits after the confidence gate, not before it**, so a candidate that would have auto-published is the only kind that can be stopped by a disagreement — a low-confidence candidate is already going to a human, and asking about conflicts first would only change the label on the item. **The human decision is now a real edge, not a placeholder:** `DecideReviewItem` publishes or rejects inside one unit of work, resolves any conflict the item covers, and writes the audit row, or none of it happens. Every terminal box on the left (`E2`–`E8`) feeds the source health state machine rather than the release pipeline, keeping the two state machines (`release-state-machine.md`, `source-health-state-machine.md`) cleanly separated: a source can be unhealthy without any candidate ever being created, and a candidate can be rejected without the source being unhealthy.

## Assumptions

- The "cheapest available signal" selection (`B1`) is a per-source, not per-check, configuration outcome in the MVP — a source is configured with the best signal it supports, and this branch represents that configuration choice rather than a live decision on every check; §16 and the collector SDK (§13) describe the fetch contract this implements.
- "Duplicate" resolves by updating `last_verified_at` on the existing release rather than creating a new row, consistent with releases being append-only (§11, rule 2) — a re-observation is not a new fact.
- The 0.85 default confidence threshold for auto-publication from official sources, and the rule that community sources always route to review, come directly from §16 gate 8. A collector config can hold a whole source below it on purpose: `fortinet.psirt-advisories` scores 0.55 with a floor of 0.55, because the feed states that an advisory was published and nothing about which products it affects (ADR-0018, and §7.4 of the collector-config spec).
- Gate 10 asks about *eligible* sources only — enabled, healthy, robots-allowed, terms-reviewed. An observation from a source FirmScout may not collect from is not evidence, so it neither raises a conflict nor loses one.
- The AI escalation box is drawn because the design calls for it. It is not implemented: a candidate that would escalate goes to human review directly.

## Failure modes

- `suspicious_content` and `manual_review_required` outcomes bypass extraction entirely and go straight to human review (`M`) — this is deliberate: some signals (e.g. a WAF challenge page swapped in for real content) should never reach a collector.
- A collector that returns candidates for a source whose compliance status forbids collection is rejected at gate 3 in §16, not shown as a separate branch here — it is folded into the deterministic gates inside `I`/`J`.
- Repeated `parser_failed` outcomes accumulate toward the repair threshold described in `source-repair-flow.md`; this diagram shows one check in isolation, not the accumulation logic.
- A conflict is a fact about a product and channel, not about one candidate, so it outlives the candidate that revealed it. Re-checking the same unchanged disagreement therefore reuses the open review item rather than creating another — without that rule the queue would grow by one item per check forever.
- The `ConflictRepository` is optional in `IngestDeps`, which means a deployment that forgets to wire it turns gate 10 into a gate that can only pass. It does not error and nothing in the pipeline notices; `internal/platform/wire_test.go` is the only thing that catches it.

## Related ADRs

- [ADR-0005 — Deterministic collectors](../adr/0005-deterministic-collectors.md)
- [ADR-0006 — AI as escalation](../adr/0006-ai-as-escalation.md)
- [ADR-0017 — Version strings and date precision](../adr/0017-version-strings-and-date-precision.md)
- [ADR-0018 — Source compliance policy](../adr/0018-source-compliance-policy.md)
- [ADR-0020 — Multi-source conflict is a recorded finding](../adr/0020-multi-source-conflict-detection.md)
- [ADR-0021 — Review decisions record an asserted, unauthenticated actor](../adr/0021-asserted-reviewer-identity.md)

## Implementing code

**Implemented, except the AI escalation branch.**

- `internal/application/checksource.go` — the whole deterministic path from the due-check gate to the nine outcomes
- `internal/adapters/fetch/fetcher.go` — signal selection and outcome classification
- `internal/domain/source.go` — the nine outcomes and the health mapping
- `internal/domain/validation.go` — the ten gates in their fixed order, gate 10 included
- `internal/application/ingest.go` — `ExtractCandidates`, `ValidateCandidate` (including `reconcileConflict`) and `PublishRelease`
- `internal/application/review.go` — the human accept/reject edge
- `internal/adapters/postgres/conflict_repo.go`, `audit_repo.go` — the rows the two new branches write

Tests: `internal/application/pipeline_test.go`, `internal/domain/validation_test.go`, and
`internal/integration/slice_test.go`, which runs the `changed` branch end to end against a
real database — once to publication, once to a conflict and a human decision, and once
through the `rss_atom` engine to an advisory that is deliberately refused publication.

AI escalation is not implemented; a candidate that would escalate goes to human review instead.

See [the architecture consistency report](../architecture/consistency-report.md) for what is verified by execution and what is not.
