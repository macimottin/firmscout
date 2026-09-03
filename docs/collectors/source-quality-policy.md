# Source Quality Policy

This document defines how a source's quality and authority are classified, how that classification feeds into confidence scoring, and how quality interacts with the automatic-publication thresholds described in [blueprint §16](../architecture/blueprint.md#16-update-and-publication-rules). It's the detailed companion to the summary table in [DATA_SOURCES.md](../../DATA_SOURCES.md).

## Why source quality is a first-class concept

FirmScout's core promise is an evidence-backed answer, not merely *an* answer. Two sources can both claim "RouterOS 7.24.2 was released on 2026-09-02," and they are not equally trustworthy just because they agree. Source quality is what lets the system say, correctly, "the vendor says this" versus "a community source, which has been reliable before, says this" — and it's what determines how much scrutiny a candidate from that source needs before it's allowed to become a published fact.

## Quality classes

| Class | Criteria | Default trust posture |
| --- | --- | --- |
| **Official manufacturer source** | The domain (or a domain the vendor has publicly and verifiably claimed, such as via a link from their primary domain) is controlled by the vendor. Includes official downloads pages, changelogs, PSIRT/security feeds, and vendor-run APIs. | Highest. Eligible for automatic publication above the confidence threshold. |
| **Authorised support portal** | A third-party-operated portal that the vendor has designated as their support channel, typically evidenced by the vendor's own site linking to it, or a documented acquisition/relocation (e.g. Poly documentation moving to `support.hp.com` after acquisition). | High, but slightly more conservative than a first-party domain — relocations and portal reorganisations happen without the underlying vendor relationship changing, so extra care is warranted on structural change. |
| **Vendor-maintained repository** | A code or artifact repository directly controlled by the vendor (typically verifiable via a GitHub organisation with vendor-controlled membership or an organisation verified badge) that isn't their primary marketing/support domain. | High for the specific artifacts it publishes; not assumed to cover the vendor's full catalogue. |
| **Trusted community source** | Not vendor-controlled, but the source and its maintainer(s) are known, identifiable, and have a track record — an established hardware/firmware community project, a widely used distribution's package index, etc. Trust here is earned incrementally, not assumed at registration. | Medium. Never eligible for automatic publication on its own; always routes to human review, and never overrides an official source's data when the two disagree. |
| **Unknown third party** | Anything that doesn't fit the above — an aggregator, a mirror with no established relationship to the vendor, or a source whose provenance hasn't been investigated. | Lowest. Registered (if at all) for exploratory or cross-referencing purposes; never a basis for a published fact on its own. |

**Labelling is mandatory, and it is never hidden.** A candidate's or release's evidence always shows which class its source belongs to. A third-party source is never allowed to silently look identical to an official one in the data model or in the API response — see the `source.official` boolean and the `quality_class` field described in [blueprint §12](../architecture/blueprint.md#12-api-design) and §4.3.

## How a class is assigned

Class assignment happens at source registration, per [DATA_SOURCES.md](../../DATA_SOURCES.md), and is one of the compliance checks a maintainer performs before a source is enabled:

1. **Verify domain control**, for a claimed official source — a link from the vendor's known primary domain, DNS/WHOIS evidence, or (best) the vendor's own published list of official properties.
2. **Verify the relationship**, for a claimed authorised portal — evidence that the vendor itself directs users there (a redirect from the vendor's old domain, an explicit statement on the vendor's site).
3. **Default to the most conservative class** when evidence is ambiguous or incomplete. A source that *might* be official but isn't verifiably so is registered as trusted community or unknown, not official, until the evidence catches up. Overclaiming trust is the failure mode this policy exists to prevent.
4. **Reclassification** happens if evidence changes — a source that turns out to be vendor-controlled after all can be promoted; a source that turns out to be an unaffiliated mirror is demoted, and any releases published on the strength of the old classification are reviewed (see [dataset-correction-policy.md](dataset-correction-policy.md)).

The Source Quality agent (blueprint §15) may *propose* a `quality_class_proposed` value for a newly registered source, with supporting evidence — but per the architecture's forbidden-write rules, it never writes the authoritative `quality_class` directly. A human decision (a maintainer, typically the relevant collector maintainer) confirms it.

## Confidence scores

Every candidate release carries a `confidence` value in `[0, 1]`, produced by the collector (or, for AI-escalated repair, by the agent that proposed the extraction) reflecting how certain the extraction itself is — not how trustworthy the source is. These are deliberately kept as two separate signals:

- **Confidence** answers "how sure is the extractor that it read this artifact correctly?" — e.g. a selector match with a clean, unambiguous version string scores higher than a regex extraction from unstructured text with a fallback pattern.
- **Source quality class** answers "how much do we trust this source to be telling the truth in the first place?"

A high-confidence extraction from an unknown third party is not the same thing as a lower-confidence extraction from an official source, and the publication gates treat them differently, not interchangeably.

## How quality interacts with automatic publication

From [blueprint §16, gate 8](../architecture/blueprint.md#16-update-and-publication-rules):

> Confidence ≥ threshold for auto-publication (default 0.85 for official sources; **community sources always review**).

Concretely:

| Source class | Automatic publication possible? | Threshold |
| --- | --- | --- |
| Official manufacturer source | Yes | `confidence ≥ 0.85` (default; may be tuned per-source based on observed reliability) |
| Authorised support portal | Yes | `confidence ≥ 0.85`, same as official — the trust is in the vendor relationship, not the specific domain |
| Vendor-maintained repository | Yes | `confidence ≥ 0.85` |
| Trusted community source | **No** | Always routes to human review (`review_items`), regardless of confidence |
| Unknown third party | **No** | Always routes to human review; typically used only as a cross-reference or a lead for discovery, not as a publication basis |

This is a deliberately blunt rule rather than a continuously tuned scoring formula: source class draws a hard line at official-and-above, and confidence only matters as a threshold *within* that line. The rationale mirrors [blueprint §3.11](../architecture/blueprint.md#311-ai-assisted-bootstrap-produces-the-dataset--challenged-in-framing) — a plausible-looking wrong fact is worse than a missing one, and community sources, however well-intentioned, don't carry the accountability an official vendor publication does.

## When sources disagree

When more than one active source covers the same product and they disagree on version or date, [blueprint §16, gate 10](../architecture/blueprint.md#16-update-and-publication-rules) routes this to human review rather than resolving it by source-class priority alone — even an official-vs-official disagreement (e.g. two regional vendor domains publishing slightly different dates) needs a human to look, because the disagreement itself is often a sign one of the sources is stale or one collector has a bug. Source quality determines *whose data is eligible to publish automatically when uncontested*, not *who wins a dispute*.
