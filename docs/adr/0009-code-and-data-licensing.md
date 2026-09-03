# ADR-0009: Apache-2.0 code, CC BY 4.0 delayed snapshots, contractual data terms for the live feed

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** founding team
- **Requires qualified legal review:** yes
- **Related:** ADR-0016, ADR-0018

## Context

The brief's starting proposal was AGPL for the code, with an implicit assumption that a strong copyleft license protects the business from a competitor running FirmScout as a service. Separately, the dataset needs a license, and the brief's framing leaned toward a share-alike database license (ODbL) for consistency with the code's copyleft posture. Both proposals deserve scrutiny because FirmScout's actual moat is not the source code: a competitor with the full repository still lacks the curated source registry, the operational history that tunes adaptive scheduling, the accumulated collector maintenance, and the reliability record — none of which a license on the code protects (§3.7).

AGPL's network-copyleft threat model targets a competitor running the code as a service without contributing improvements back. That threat is weak here because the code alone does not reproduce the business. Meanwhile AGPL's costs are concrete and well documented in industry practice: corporate legal departments commonly block AGPL as a dependency outright, which would suppress exactly the contributor population FirmScout needs — infrastructure engineers at enterprises who would maintain collectors for vendors they operate (assumption P5) — and self-hosting adoption is itself a distribution and trust-building channel the project wants to maximize, not restrict.

For the dataset, ODbL's share-alike obligation is viral into "derived databases" — precisely what a customer's internal asset-management database becomes the moment they import FirmScout data into it. For an enterprise buyer, an obligation to share-alike their internal asset database is a deal-breaker, and in practice such obligations on internal, non-redistributed derivative databases are close to unenforceable, making the restriction costly to the business relationship without a corresponding practical benefit.

## Decision

**Code:** Apache-2.0, for the entire open-source codebase, chosen specifically for its explicit patent grant and its track record of being acceptable to corporate legal review — removing the single most common blocker to enterprise and corporate-employed individual contribution. A **trademark policy** is maintained separately, protecting the "FirmScout" name and any associated marks independent of the code license, so that a fork is free to reuse the code under Apache-2.0 but is not free to call itself FirmScout or imply endorsement.

**Dataset — two tiers, deliberately different licenses for deliberately different data:**

- **Delayed public snapshots** (periodic exports of the catalogue, generated from PostgreSQL per the hybrid dataset design, ADR-0016) are licensed **CC BY 4.0** — attribution only, no share-alike. CC 4.0 was drafted to explicitly cover the EU *sui generis* database right, which the Open Data Commons licenses (including ODbL) predate and were designed around before CC 4.0 existed to do so directly; using CC BY 4.0 is intended to give cleaner coverage of that specific right for a factual database than an ODC license would.
- **The live API feed and enriched fields** (real-time data, freshness guarantees, structured advisory data, and other paid-tier-exclusive content) are governed by separate, contractual **FirmScout Data Terms** — not a public copyright license at all, but a contract entered into by API consumers, which is where usage restrictions, redistribution limits, and commercial terms for the live, high-value data actually live.

**Where the protection actually resides:** not in the code license, but in the *combination* of the dataset license (which permits broad reuse of static snapshots but under attribution) and the contractual terms governing the live feed (which can impose commercial restrictions a copyright license cannot cleanly express for a database of facts). This mirrors §3.7's core finding: the moat is the data and the operation, not source secrecy.

## Consequences

### Positive

- Apache-2.0 removes the most common corporate-legal blocker to contribution, directly supporting the collector-contribution assumption (P5) the project depends on for coverage beyond the founder-generated pilot dataset.
- The explicit patent grant in Apache-2.0 protects contributors and users from patent claims arising from contributed code, which is a meaningful protection AGPL and permissive-without-patent-grant licenses (like plain MIT) do not offer as clearly.
- Splitting delayed snapshots (CC BY 4.0) from the live feed (contractual terms) lets the project be maximally open about historical, non-time-sensitive data — supporting the open-source and research communities — while still having a commercial lever (freshness, structure, guarantees) that a copyright license on a factual database could not enforce cleanly anyway.
- A trademark policy separate from the code license means forks are legally straightforward (Apache-2.0 is unambiguous) while brand confusion is handled by a different, more suitable legal instrument.

### Negative

- Apache-2.0 provides no network-copyleft protection at all: a well-resourced competitor genuinely can take the entire codebase, run it as a directly competing hosted service, and owe the project nothing beyond license attribution. This ADR accepts that risk explicitly, betting that the data and operational moat matters more than the code moat — a bet that could be wrong, particularly if a competitor found a way to independently replicate the source registry and operational tuning faster than expected.
- CC BY 4.0's lack of share-alike means a delayed public snapshot can be taken, enriched, and resold by a third party without any obligation to contribute improvements back or even to keep the derivative open — the project has no license-based claim on downstream derivatives of the snapshots it publishes.
- Relying on contractual terms (rather than a copyright license) for the live feed means the enforcement mechanism is contract law against a named counterparty (an API consumer who agreed to the terms), not a copyright claim against an arbitrary infringer — this is a materially different and generally weaker enforcement posture against, for example, an anonymous scraper who never agreed to any terms (see ADR-0008, which acknowledges scraping cannot be prevented, only discouraged).
- The extent to which a purely factual catalogue (firmware version, release date, source URL) is protectable by copyright or database right *at all* varies significantly by jurisdiction — the EU's sui generis database right, US case law on factual compilations (post-*Feist*), and UK post-Brexit divergence from EU database right are all materially different, and this ADR's licensing choices assume rights exist to license that may not exist, or may not exist in the same form, in every jurisdiction FirmScout operates in or is accessed from.

### Neutral

- Contributor licensing mechanics (CLA vs. DCO) are not settled by this ADR and are listed explicitly as an open legal/product question below — the blueprint names this as one of the decisions requiring input beyond the founding team's own judgment.
- The delay period for public snapshots (how long after publication a fact appears in the free CC BY 4.0 export) is a product parameter, not fixed by this ADR.

## Alternatives considered

### AGPL-3.0 for the code

Rejected, per §3.7. The network-copyleft protection it offers is weak against FirmScout's actual competitive threat model (the data and operational history are the moat, not the code), while its cost — suppressing corporate and enterprise contribution — is real and directly opposed to assumption P5, which the project needs to hold for collector coverage to scale beyond the founding team.

### ODbL (Open Database License) for the dataset

Rejected, per §3.8. Its share-alike obligation extending into "derived databases" is precisely what makes an enterprise customer's internal use of FirmScout data legally fraught the moment they store it alongside their own asset data, which is the primary paid use case (ADR-0007). It is also considered close to unenforceable against typical internal, non-redistributed use, making the restriction more costly to the customer relationship than protective in practice.

### CC0 / public domain dedication for the entire dataset, including the live feed

Rejected. This would remove any licensing basis for the paid tier's commercial terms entirely, undermining the revenue model in ADR-0007 that funds the platform's ongoing collector maintenance and AI escalation budget.

### A single license covering both code and data (e.g., everything under one copyleft or one permissive license)

Rejected implicitly by the two-tier structure itself: code and a factual database are different kinds of work, subject to different areas of law (copyright licensing for code; database right, contract, and in some jurisdictions unfair-competition or trade-secret-adjacent doctrines for the data), and conflating them under one license either over-restricts the code or under-protects the commercially relevant data.

## Legal review — specific questions to resolve

This ADR is explicitly marked as requiring qualified legal review because several of its premises are legal judgments the founding team is not qualified to make final. A lawyer needs to confirm or correct:

1. Whether the *sui generis* database right (EU) attaches to FirmScout's catalogue as compiled, and whether CC BY 4.0 is the correct instrument to license it, versus needing a supplementary or different license for that specific right.
2. Whether and how a factual compilation of firmware/version metadata is protectable under US law post-*Feist* (the "sweat of the brow" doctrine is not good law in the US), and what if anything a US-only contract can restrict absent an underlying IP right.
3. UK-specific database right status post-Brexit, given divergence from the EU regime.
4. Whether the FirmScout Data Terms (contractual, live-feed) are enforceable against a party who accesses the API without a key or without affirmatively agreeing to terms (relevant given ADR-0008's acknowledgment that scraping cannot be prevented).
5. Whether Apache-2.0's patent grant creates any unintended exposure given the AI agent components (ADR-0006) that may, in the future, involve third-party model providers with their own IP terms.
6. Trademark policy specifics: registrability of "FirmScout" in relevant jurisdictions, and the appropriate scope of a community trademark guideline (naming forks, using the name in promotional contexts) without chilling legitimate self-hosted use.
7. Contributor licensing mechanics: CLA versus DCO, and which better serves both contributor friction (P5) and the project's ability to relicense or dual-license in the future if needed.

## Revisit when

- A competitor is observed running FirmScout's code as a directly competing hosted service, which would test in practice whether the "data and operations are the moat, not the code" bet was correct.
- Legal review (above) returns findings that require a different license structure for any jurisdiction FirmScout operates in or targets commercially.
- The delayed-snapshot export pipeline is built and a concrete delay period and dataset scope need to be fixed, at which point the CC BY 4.0 scope should be reconfirmed against what is actually being exported.
