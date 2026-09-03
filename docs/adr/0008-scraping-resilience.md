# ADR-0008: Layered scraping resilience without dataset poisoning

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** founding team
- **Requires qualified legal review:** no
- **Related:** ADR-0007, ADR-0012, ADR-0013

## Context

Assumption O1 states, with high confidence, that bulk scraping of the public site will be attempted and cannot be prevented — only made unattractive. This is stated as a fact to be designed around, not a problem to be solved. A genuinely useful, unlimited-within-reason free tier (ADR-0007) is, by construction, also useful to a scraper. The tempting responses — serving subtly wrong data to suspected bots, hiding accessibility affordances behind interactive challenges, or fingerprinting browsers aggressively to distinguish humans from automation — each solve the scraping problem by breaking something the product cannot afford to break: correctness for real users, accessibility, or privacy-conscious analytics (ADR-0012).

## Decision

FirmScout raises the cost of bulk scraping through **layered controls that never touch data correctness**, and treats the underlying premise (scraping cannot be prevented) as settled rather than as a target to disprove:

- **CloudFront** as the edge layer, absorbing volumetric traffic and providing the first opportunity to apply caching and coarse geographic/rate controls before requests reach compute.
- **WAF rate rules** at the edge — coarse-grained, cheap, and effective against the bulk of unsophisticated volumetric abuse before it costs any application compute (§3.9). This is also the primary lever named in the rate-limiting decision, ahead of per-instance token buckets.
- **Per-endpoint policies**, because a search endpoint, a bulk-lookup endpoint, and a single-product page have different legitimate usage shapes and therefore different reasonable limits; a single global rate limit would either be too loose for the endpoints that matter or too tight for normal browsing.
- **An explainable abuse score**, combining request patterns (rate, endpoint sequence, header consistency) into a score that drives a response — additional friction or throttling — that can be described and justified after the fact, not an opaque black-box classifier whose false positives cannot be explained to a legitimate user or defended in a support ticket.

**Explicitly forbidden, regardless of how effective it might be against scraping:**

- **Dataset poisoning** — serving deliberately incorrect firmware or version data to suspected bots. This is rejected outright: the moment FirmScout's dataset contains any intentionally false fact, distinguishing "poisoned for a bot" from "wrong" becomes impossible to guarantee, and the entire evidence-first trust model (§1) is compromised for everyone, not just the targeted traffic.
- **False firmware data** of any kind, for any anti-abuse purpose.
- **Hidden accessibility traps** (e.g., invisible honeypot links or fields designed to catch automated traffic) that could also catch or degrade the experience for assistive technology users. Accessibility is explicitly named as never a paywall or anti-abuse lever (ADR-0007).
- **Dependence on browser fingerprinting** as a load-bearing control. Fingerprinting techniques are also the toolkit of cross-site tracking, and this project has separately committed to first-party, privacy-conscious analytics with no third-party trackers (ADR-0012); building anti-abuse dependent on fingerprinting would work against that commitment and would degrade for privacy-conscious real users (who use fingerprint-resistant browsers) in the same stroke it targets scrapers.

The system **acknowledges explicitly, including in its own documentation, that scraping cannot be prevented** — only made slower, more expensive, and less attractive relative to using the API as intended (which is, not coincidentally, also the product's revenue path per ADR-0007).

## Consequences

### Positive

- No anti-abuse control can ever compromise data correctness, because dataset poisoning is categorically forbidden rather than merely discouraged — this closes off the single most damaging failure mode a scraping-defense system could introduce.
- Layered, explainable controls degrade gracefully: a legitimate power user hitting a rate limit gets a clear, documented reason (RFC 9457 problem+json, per §12) rather than an opaque block, which is good for support burden and good for API trust.
- CloudFront and WAF absorb the cheapest, highest-volume attacks before they cost application compute or database load, which is directly aligned with the cost-minimisation requirement (ADR-0013) — abuse mitigation is also a cost control.
- Avoiding fingerprinting keeps the product's privacy posture consistent across both its analytics (ADR-0012) and its anti-abuse layer, rather than having one part of the product avoid tracking while another part depends on it.

### Negative

- Because dataset poisoning, fingerprinting, and accessibility traps are all off the table, the remaining controls (rate limits, abuse scoring, edge caching) are fundamentally rate- and pattern-based, and a patient, well-resourced scraper that respects rate limits and mimics normal usage patterns is, by design, very hard to distinguish from a legitimate heavy user — and will not be stopped.
- An explainable abuse score is inherently less sophisticated at catching adversarial traffic than an opaque ML-based classifier could be; this is an accepted trade-off for explainability, not a claim that it is equally effective.
- Per-instance token buckets (named in §3.9 as part of the layered approach for rate limiting specifically) mean the effective per-second limit scales with instance count — at N instances, the true ceiling is N times the configured per-instance limit — which is an approximation the team is aware of and accepts at MVP instance counts, relying on the WAF layer to bound the aggregate regardless.
- Building and tuning an "explainable abuse score" that is genuinely explainable (not just a linear combination that is technically inspectable but practically opaque) is real, ongoing product and engineering work, not a one-time control to configure and forget.

### Neutral

- This ADR does not specify exact rate-limit numbers, WAF rule thresholds, or the abuse score's precise formula — those are operational tuning parameters expected to change based on observed traffic, and are not architectural commitments this ADR fixes.

## Alternatives considered

### Aggressive bot-detection challenges (CAPTCHA, proof-of-work, or similar friction) on all API and web traffic

Rejected as a default-on control. Universal friction would degrade the genuinely free, genuinely useful public site (ADR-0007) for every legitimate user, including the search engines whose indexing is the primary acquisition channel assumption (P4). Targeted friction, driven by the abuse score, is preferred over blanket friction.

### Fingerprinting-based bot detection (canvas/WebGL fingerprinting, browser attestation)

Rejected, as stated in the decision. Beyond the privacy-consistency argument, fingerprinting techniques are also actively defeated by the privacy-conscious browsers and extensions that a meaningful share of FirmScout's target audience (infrastructure engineers) already uses, making it both ethically inconsistent with the product's stated values and practically less effective against the specific user population.

### Dataset poisoning or honeypot data for suspected scrapers

Rejected outright, discussed above. Considered explicitly and rejected because it is irreconcilable with the evidence-first trust model that is the entire basis of the product's credibility (§1).

### Doing nothing beyond basic infrastructure limits, accepting scraping as a cost of being open

Considered, but rejected as too passive given that unmitigated bulk scraping directly undermines the automation/history paid tier (ADR-0007) that funds the platform — some friction on non-standard, high-volume access patterns is necessary to make the paid API meaningfully more attractive than replicating its function via the free site.

## Revisit when

- More than roughly 4 concurrent API instances are running and per-instance token buckets' aggregate imprecision becomes operationally significant, or a customer contract requires exact per-second enforcement (§3.9) — at that point Redis-backed distributed rate limiting is the documented next step.
- Measured scraping volume or its cost impact (egress, compute) crosses a threshold that materially affects unit economics (tracked under ADR-0013's cost dashboards), warranting either more aggressive edge controls or a reconsideration of what the free tier serves unauthenticated.
- The abuse score's false-positive rate (legitimate users incorrectly throttled, measured via support tickets or churn) exceeds an acceptable level, requiring the scoring model to be revisited for explainability or accuracy.
