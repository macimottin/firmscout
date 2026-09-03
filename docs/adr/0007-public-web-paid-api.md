# ADR-0007: Genuinely free public site, paid API for automation

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** founding team
- **Requires qualified legal review:** no
- **Related:** ADR-0008, ADR-0012, ADR-0013

## Context

FirmScout's product is three things at once (§1): a free public website, an open-source self-hostable platform, and a hosted service sold to organisations. These pull in different directions unless the boundary between free and paid is drawn deliberately. The most common failure mode for a product like this is drawing that boundary along *correctness* or *freshness of the headline fact* — showing a slightly stale "latest version" to free users to create upgrade pressure. That failure mode is corrosive here specifically: FirmScout's entire value proposition is being the trustworthy, evidence-backed answer, and assumption P4 already bets that a free, SEO-indexed site is the primary acquisition channel. A site that is not fully trustworthy for free undermines both the product's integrity and its own growth channel simultaneously.

Assumption P2 (unvalidated) is that the single most valuable fact is "latest version + release date + official link," not full release history or advisory detail. Assumption P3 (unvalidated) is that buyers pay for automation and history, not for the raw current-version fact. Both assumptions point toward the same design: the free tier answers a human's question completely; the paid tier answers a *system's* question at scale.

## Decision

**The free tier is limited by convenience, not by truth.** Nothing on the public site is deliberately wrong, stale, or crippled. Anonymous web access, manual search, latest observed version, release date with precision, release type, official source link, last-verified timestamp, and a recent-history window are all free, unlimited within abuse-prevention limits (§5). Anonymous requests hit the same API the website itself calls — there is no separate, deliberately worse code path for unauthenticated traffic, because that would just relocate the site's own requests into a different failure mode with a different cache (§12).

What the paid tiers sell is automation, scale, history depth, and guarantees: programmatic access with meaningful quota, bulk lookup across many products in one call, webhooks and alerts, inventory upload and comparison, compliance reporting and exports, source health metadata, and freshness commitments that graduate from best-effort to a contractual SLA at Enterprise. The dividing line for release history specifically is deliberate: a human checking "what firmware should my switch be on" needs the current version and a handful of recent releases; a compliance system proving "this fleet was compliant on 30 June" needs complete history and point-in-time queries — the second is unambiguously automation, and automation is what is sold (§5).

**Explicitly not used as a paywall lever:** correctness, freshness of the *displayed current version*, official source links, or accessibility. Degrading any of these to drive conversion would destroy the trust the product depends on, and this list is intentionally stated as a negative constraint so that a future pricing change can be checked against it rather than trusted to institutional memory.

## Consequences

### Positive

- The free site can be the honest acquisition channel assumption P4 requires: nothing about using it for free carries a hidden asterisk, which supports both SEO trust signals (no cloaking, no degraded content for crawlers vs. paying users) and word-of-mouth credibility.
- The commercial pitch is simple to state and defend: "the facts are free, the automation is not." This is easy for a prospective enterprise buyer to evaluate quickly and easy for the team to hold itself accountable to.
- Because anonymous API traffic and the website's own traffic share one code path, there is only one cache strategy and one failure mode to reason about and monitor, rather than two systems that can drift apart.
- The explicit "not a paywall lever" list gives future pricing or product decisions a clear check: if a proposed change would make the displayed current version staler for free users to encourage upgrades, this ADR is the document that says no.

### Negative

- Revenue depends entirely on the automation/history/integration value proposition being real to buyers — assumption P3 is explicitly unvalidated, and if buyers turn out to want the current-version fact enough to pay for it regardless of automation, this model leaves money on the table by giving that fact away free.
- A genuinely useful free tier is also genuinely useful to a competitor or to a scraper building a derivative product on FirmScout's data without contributing back — this is a direct trade-off against the licensing and scraping-resilience decisions (ADR-0008, ADR-0009), which have to carry the weight of protecting the business instead of the paywall doing it.
- The free/paid split does not, by itself, prevent a well-resourced actor from replicating history depth by making many small free-tier or web requests over time; the practical mitigation is abuse controls, not the pricing boundary (ADR-0008).
- "Recent history window" (e.g., 12 months) as the free/paid boundary for release history is stated with an illustrative number in the blueprint (§5), not a finalised one — the actual window is a product decision this ADR does not settle and should not be read as fixing.

### Neutral

- Lifecycle status (EOL/EOS) and security advisories both have a "basic vs. full/structured" split rather than a binary free/paid split (§5) — free users see the list and links, paid tiers see structured data with affected ranges and evidence. This is consistent with the "convenience, not truth" principle: the underlying fact is not hidden, only its structured, machine-consumable form is paid.

## Alternatives considered

### Freemium with degraded free-tier freshness (e.g., 24–48 hour delay on free "latest version")

Rejected. This is the classic pattern for data products and was considered, but it directly contradicts the principle that the displayed current version is never a paywall lever. It would also actively undermine the product's core trust claim — a delayed "latest version" is a wrong answer to the question the entire product exists to answer correctly.

### Fully open data, no paid tier at all (rely on hosted-service support/SLA revenue only)

Rejected as insufficient. Given assumption O3 (the founder-generated dataset must carry the product, community contribution is additive) and O4 (costs are dominated by egress, compute, and AI), a revenue model based solely on support contracts is unlikely to fund the ongoing collector maintenance, source monitoring, and AI escalation budget the platform needs to keep the free site trustworthy over time.

### Gating fields within the same response (hide certain fields from anonymous/free requests)

Rejected as the primary mechanism, per the explicit API design principle (§12): "no field in the response that the free tier could not also see in the browser." Field-level hiding was considered and rejected in favor of gating on *volume and endpoint access* (bulk lookup, webhooks, high quota) rather than on which fields a given response contains, because field-level gating is exactly the kind of subtle degradation that erodes trust and is easy to get wrong in either direction (accidentally leaking a paid field, or accidentally hiding a fact that should be free).

## Revisit when

- Six months of production data on P4 (organic traffic share) and P3 (free-to-paid conversion rate) are available — both are named in the blueprint as needing exactly this kind of validation.
- API consumer field-usage analytics (P2) show that consumers want data beyond "latest version + date + link," which would inform whether the free/paid line is drawn in the right place.
- Enterprise discovery conversations (P6) surface a willingness to pay for compliance/audit use cases specifically, which would validate prioritising compliance reporting and exports as a differentiated Enterprise feature rather than a general paid-tier one.
- Abuse or replication of free-tier data at a scale that measurably substitutes for a paid subscription is observed, which should first change scraping-resilience controls (ADR-0008), not the free/paid line itself.
