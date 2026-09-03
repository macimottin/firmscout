# ADR-0012: First-party, privacy-conscious product analytics

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** founding team
- **Requires qualified legal review:** no
- **Related:** ADR-0007, ADR-0008

## Context

Several of the blueprint's own assumptions can only be validated with real usage data: which fields API consumers actually read (P2), what drives free-to-paid conversion (P3), how much organic traffic the free site attracts (P4). Product decisions cannot be made responsibly on vibes alone. At the same time, FirmScout's target users — infrastructure and security engineers — are a population disproportionately likely to run tracker blockers, to distrust third-party analytics scripts, and to judge a project's values partly by whether its own site tracks them the way it explicitly refuses to let others track visitors of vendor sites it monitors. A product that needs data to make good decisions and a user base that distrusts conventional tracking are both real constraints this decision has to satisfy simultaneously.

## Decision

FirmScout uses **first-party, privacy-conscious analytics** — events collected and stored by FirmScout's own infrastructure, not sent to a third-party analytics vendor, and **no third-party trackers** (advertising pixels, cross-site tracking scripts, third-party cookies) are embedded anywhere in the product. A defined list of **business events** is what gets recorded — searches performed, product pages viewed, API endpoints hit, conversion funnel steps (free key created, upgraded to paid tier) — captured as structured events through the `EventPublisher` port already used for domain events (§7.7), persisted to `analytics_events` in PostgreSQL, and correlated with the product-analytics diagram's flow (`docs/diagrams/product-analytics.md`).

A hard rule governs what analytics and logging may ever capture: **API keys, tokens, credentials, and the contents of a customer's uploaded inventory are never logged, in analytics events or anywhere else.** This is treated as an absolute constraint on the same level as "collectors cannot publish" — a logging or analytics code path that captures a raw API key or the contents of an uploaded fleet inventory is a defect regardless of how useful the resulting data might be, because a customer's inventory (what devices they run, at what versions) is itself sensitive operational and security information about that customer.

## Consequences

### Positive

- The team gets real usage data to validate or falsify P2, P3, and P4 without asking users to accept third-party tracking, which is directly aligned with the target audience's stated preferences and with the product's own no-fingerprinting stance in scraping resilience (ADR-0008).
- No third-party analytics vendor means no data-sharing agreement, no third-party subprocessor to vet for the privacy policy, and no external dependency whose downtime or policy change affects the product.
- Being first-party and self-hosted-consistent (the analytics pipeline runs on the same PostgreSQL and event infrastructure as everything else) means self-hosters get the same analytics capability the hosted service uses, with no proprietary tool gating it.
- A hard, explicit rule against logging credentials and inventory contents gives the team and any future security reviewer a clear, checkable invariant rather than a vague "be careful with logging" guideline.

### Negative

- Building and maintaining a first-party analytics pipeline (event schema, storage, any aggregation/dashboard layer for internal use) is real engineering work that a third-party analytics SaaS would have provided out of the box — this is accepted as worthwhile but is not free.
- First-party analytics collected via the same domain infrastructure as everything else carries some risk of scope creep: without discipline, "just add this field to the event" is an easy way to accumulate more collected data over time than the original privacy-conscious framing intended, and this needs ongoing review, not a one-time design decision.
- No third-party tooling means no inherited best-practice tooling for common analytics needs (funnel visualization, cohort analysis, session replay) — the team builds only what it specifically needs, which may mean slower iteration on some product questions than a mature third-party analytics product would allow.
- The "never log credentials or inventory contents" rule is only as good as its enforcement; nothing described here is a technical guarantee (unlike, say, `internal/archtest`'s mechanical check) — it depends on code review and, ideally, a lint rule or test specifically targeting log/event call sites that could leak sensitive fields, which is not yet specified.

### Neutral

- This ADR does not enumerate the complete business-events list or the exact retention period for `analytics_events` — those are product and operational parameters expected to evolve, documented in `docs/diagrams/product-analytics.md` rather than fixed here.
- Aggregate, anonymized usage statistics may eventually be published (e.g., "N sources monitored", "N releases published this month") as a transparency and marketing signal; this ADR does not decide that question, only establishes that any such publication draws from the same privacy-conscious first-party pipeline, never from third-party tracking data.

## Alternatives considered

### A mainstream third-party analytics tool (Google Analytics or similar)

Rejected. Third-party analytics of this kind typically involves cross-site tracking infrastructure and data sharing with the vendor that directly contradicts both the audience's likely preferences and the project's own anti-fingerprinting stance in scraping resilience (ADR-0008) — it would be inconsistent to refuse to fingerprint suspected scrapers while fingerprinting every ordinary visitor for marketing analytics.

### A privacy-focused third-party analytics SaaS (e.g., a cookieless, first-party-proxied analytics vendor)

Considered as a middle ground — genuinely more privacy-respecting than mainstream tools — but still ultimately sends event data to a third party's infrastructure, which conflicts with the self-hosting parity goal (a self-hoster would either need their own account with that vendor or get a degraded analytics experience) and with keeping customer-adjacent usage data entirely within infrastructure FirmScout itself controls and can audit.

### No analytics at all

Rejected as too conservative given how many of the blueprint's own product assumptions (P2, P3, P4) are explicitly marked unvalidated and dependent on exactly this kind of data. Operating without any usage visibility would mean pricing, feature, and acquisition-channel decisions are made on guesswork indefinitely.

## Revisit when

- The business-events list needs to grow meaningfully beyond its initial scope to answer a specific, named product question — each addition should be evaluated against the same "does this need to exist" discipline as the initial list, not added by default.
- A lint rule, test, or automated check for credential/inventory-content leakage in logs and events is designed and should be added to CI, closing the gap between "this is a rule" and "this is mechanically enforced" that ADR-0002 and ADR-0005 achieve for their respective invariants.
- Aggregate usage metrics are considered for public transparency reporting, at which point what is safe to publish (vs. what remains internal-only) needs explicit review.
