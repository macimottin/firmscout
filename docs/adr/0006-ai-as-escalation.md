# ADR-0006: AI as an escalation mechanism, never the default path

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** founding team
- **Requires qualified legal review:** no
- **Related:** ADR-0005, ADR-0013

## Context

The brief invites AI-assisted bootstrapping and repair, and that invitation is sound — but the framing risks being read as "point a language model at the internet and it produces a catalogue," which it cannot do, and a catalogue of plausible-looking wrong firmware versions is worse than no catalogue, because it is actively harmful to someone patching a device based on it (§3.11). AI is also, by a wide margin, the highest-variance cost driver in the system (§8.3): token cost scales with model choice, input size, and retry behaviour in ways that per-request compute and egress do not. A system whose default execution path invokes an LLM has both an unpredictable bill and unpredictable, non-reproducible behaviour — two properties directly opposed to FirmScout's stated operating principles (§1).

At the same time, deterministic collectors (ADR-0005) genuinely cannot handle everything: sources relocate, layouts change, new vendors need initial discovery, and some candidates carry genuine ambiguity a selector cannot resolve. The system needs *some* mechanism for these cases, and demanding purely deterministic bootstrapping for four pilot vendors is unrealistic. The resolution is to name AI as what it structurally is here — an escalation path, invoked when deterministic methods fail, under an explicit budget, and never in a position to write a fact directly.

## Decision

Five AI agent **ports** are defined in the application layer — Discovery, Repair, Validation, Classification, and Source Quality (§15) — with **no runtime implementation in the MVP**: only interfaces, JSON schemas (in `agents/`), and fakes for testing. Each agent shares one envelope: a `run` block naming the agent, trigger reason, prompt version, model ID, and a `budget_cap_usd`; an `input` block; an `output` block; and a `usage` block recording tokens and estimated cost. **Output is validated against a JSON Schema before it is allowed to influence anything.** An output that fails schema validation is treated as a failed run — it is recorded and billed, and the system falls back to human review, never to a retry loop that might eventually produce a passing-but-wrong response.

| Agent | Trigger | Produces | May write to |
| --- | --- | --- | --- |
| Discovery | Bootstrap; new vendor | Source, collector config, and product/alias proposals with evidence | `review_items` only |
| Repair | Source `broken`/`relocated`; selector miss; content-type change | Replacement URL, updated selectors, generated fixture, PR body | `review_items`; a Git branch via CI |
| Validation | Low-confidence candidate; implausible transition; multi-source conflict | Structured verdict with per-check reasoning | `validation_results` (advisory) |
| Classification | Candidate with `release_type = unknown` | `release_type` with confidence | `candidate_releases.proposed_release_type` |
| Source quality | New source registration | Quality class with evidence | `sources.quality_class_proposed` |

**None of these agents writes to `releases`, `sources`, or `products`.** This is enforced the same way collectors' inability to publish is enforced (ADR-0005, §8.2): agent ports return proposal types, and no publishing use case in Ingestion or Sourcing accepts a proposal as input without either a deterministic validation pass or an explicit human decision. AI-generated collector code (from Repair) is a pull request, reviewed and merged by a human, never a directly deployed artifact. Model selection is by task cost-sensitivity: Classification and Source Quality use the least expensive capable model; Discovery and Repair may use a more capable model, always within a per-job cap. Budgets and circuit breakers operate at multiple levels (per-run cap, and an aggregate budget with a breaker that halts further AI calls once exhausted), and the system has an explicit **degraded mode**: when AI budgets are exhausted, escalation-worthy cases queue for human review instead of failing silently or falling back to an uncontrolled retry.

## Consequences

### Positive

- Cost is bounded and predictable: the default path (deterministic collectors) is free of AI cost entirely, and escalation cost is capped per run and in aggregate, so a pathological source (one that breaks repeatedly) cannot produce an open-ended bill.
- Behaviour stays reproducible on the default path — the vast majority of checks — while still having a defined, auditable mechanism for the genuinely ambiguous cases that deterministic rules cannot resolve.
- Schema validation before any output can influence state means a malformed or hallucinated AI response is caught mechanically, not by a human happening to notice something looks wrong.
- The "AI never writes directly" rule, combined with collectors' equivalent restriction, means the two riskiest sources of bad data in the system — non-deterministic extraction and community/config error — both funnel through the same deterministic validation gates (§16) before publication.
- Degraded mode (queue for human review when budget is exhausted) means a cost spike anywhere in the system degrades gracefully to slower discovery/repair, not to silent failure or unbounded spend.

### Negative

- No AI agent ships in the MVP — ports, schemas, and fakes only — which means the system's answer to "a source moved" or "a new vendor needs bootstrapping" is, for now, entirely manual. This is a known and accepted MVP gap, not a hidden one.
- Schema-validated JSON output constrains what an agent can usefully express; genuinely novel failure modes an agent encounters but cannot fit into the schema are simply failed runs, which may under-utilise a capable model's actual judgment in edge cases.
- Per-run and aggregate budget caps mean that under sustained source breakage (assumption T6, currently unvalidated: how often sources break vs. how affordably AI can repair them), the system could plausibly hit degraded mode regularly, pushing more work onto human reviewers than initially planned. The breakage-rate assumption is explicitly unvalidated and needs real measurement before AI agent implementation is prioritised.
- Five distinct agent contracts (schemas, prompt version registries, budget logic) is nontrivial surface to design, test, and keep in sync with the evolving domain model, even before any of them has a runtime implementation.
- Prompt injection from fetched vendor content is a named risk (§8.3) for any agent that processes fetched artifacts (Repair, Discovery) — content from a compromised or adversarial source could attempt to manipulate agent output. Schema validation constrains the blast radius but does not eliminate the risk, and this needs dedicated adversarial testing before any agent goes live.

## Alternatives considered

### AI as the default extraction and discovery mechanism

Rejected, and this is the specific proposal challenged in §3.11. It would make cost proportional to check volume instead of change frequency, make output non-reproducible, and risk exactly the "plausible-looking wrong firmware version" failure mode the blueprint calls out as actively harmful. Deterministic collectors remain the default; AI is escalation only.

### No AI at all, even as an escalation path

Rejected as too conservative given the realities of source maintenance at scale: sources do relocate, and new vendor bootstrapping genuinely benefits from a discovery assistant that can propose candidates with evidence for a human to review. Removing AI entirely would mean every source break requires a fully manual investigation, which does not scale to "tens of thousands of sources" even with fixture-based collector maintenance covering routine cases well.

### Free-text or unstructured AI output, parsed heuristically downstream

Rejected. Unstructured output would require a parsing layer that is itself a new source of bugs and would make "did the agent's output actually mean what we think it meant" unauditable. JSON Schema validation up front is the cheaper and more reliable control.

## Revisit when

- Breakage rate per source per quarter (T6) is measured across the first cohort of registered sources, giving a real number to size AI repair budgets against instead of the current unvalidated assumption.
- The first agent (most likely Repair or Discovery, given pilot vendor needs) is implemented; this ADR's "ports only" status should be updated to reflect the first real adapter and its measured cost per run.
- Aggregate AI spend approaches a meaningful fraction of total infrastructure cost (tracked per ADR-0013's cost dashboards), at which point per-agent budget caps should be re-tuned against actual value delivered (successful repairs / discoveries per dollar).
- Degraded mode is triggered in production with meaningful frequency, indicating budgets are set too low relative to actual source-maintenance demand.
