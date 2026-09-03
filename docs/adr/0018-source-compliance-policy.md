# ADR-0018: Source compliance is a first-class field, evaluated before collection

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** founding team
- **Requires qualified legal review:** yes
- **Related:** ADR-0005, ADR-0009, ADR-0016

## Context

FirmScout's function is to retrieve version metadata from several thousand manufacturer websites. That activity sits inside a web of constraints the project does not control: each site's terms of use, its `robots.txt`, the copyright in its release documents, database rights in its catalogues, licences on its public APIs, authentication requirements, rate limits, attribution demands, regional legal differences, and the possibility of a data-removal request. The brief is unambiguous that FirmScout must not bypass authentication, circumvent CAPTCHAs, evade rate limits, ignore `robots.txt`, or reproduce copyrighted release documents in full.

The naive way to honour that is to treat compliance as a review step: a maintainer reads the terms before merging a source, and the result lives in someone's memory or a pull-request comment. This fails in three specific ways. First, it does not survive time — a site's terms and `robots.txt` change, and nothing re-checks them. Second, it does not survive scale — a Discovery Agent can propose fifty sources in a run, and human memory is not a control that scales. Third, and most damaging for an open-source project, it is invisible: a self-hoster running FirmScout's code has no way to know that a particular source was collected under a permission that does not extend to them.

This stopped being hypothetical during the blueprint work. Dell's `downloads.dell.com/catalog/Catalog.xml.gz` is, on measurement (2026-09-03), close to an ideal source: a 1.4 MB well-formed catalogue serving both `ETag` and `Last-Modified`, covering BIOS, drivers and firmware across a large product range. It was the intended pilot for the "clean API" archetype. But `https://downloads.dell.com/robots.txt` returns, for all user agents:

```
User-agent: *
Allow: /manuals
Allow: /topicspdf
Disallow: /
```

Automating collection of that catalogue would have violated FirmScout's own stated policy in its first week, in public, in a repository whose entire premise is trustworthiness. The technical attractiveness of a source and the permissibility of collecting it are independent variables, and the architecture has to model both.

## Decision

**Compliance status is a first-class, persisted attribute of every source, evaluated before any collection occurs, and re-evaluated on a schedule.**

The `sources` table carries, alongside its technical fields:

| Field | Meaning |
| --- | --- |
| `official` | Whether the source is on a manufacturer-controlled domain |
| `quality_class` | Official manufacturer source, authorised support portal, vendor-maintained repository, trusted community source, or unknown third party |
| `robots_policy_status` | `allowed`, `disallowed`, `unknown`, or `not_applicable`, derived from the host's `robots.txt` for the specific path and the project's declared user agent |
| `terms_review_status` | `pending`, `approved`, `restricted`, or `prohibited`, set by a human after reading the site's terms |
| `authentication_type` | `none`, `api_key`, `account_required`, or `entitlement_required` |
| `enabled` | Whether the scheduler may dispatch checks at all |

A source is dispatchable **only** when `enabled = true`, `robots_policy_status` is `allowed` or `not_applicable`, and `terms_review_status` is `approved` or `restricted` (with `restricted` carrying documented conditions such as reduced frequency or attribution requirements). This is enforced in the `CheckSource` use case and in the scheduler's query, not left to the collector — a collector that is never dispatched cannot make a mistake.

Four consequences follow directly:

**Dell stays registered and disabled.** The vendor, its products, and the catalogue source are all recorded in the registry with `robots_policy_status = 'disallowed'`, `terms_review_status = 'pending'`, and `enabled = false`. This is deliberately more useful than deleting it: the record documents that the source exists, that it was evaluated, and why it is not collected, so the question is not silently re-opened every few months by someone who rediscovers the catalogue. The path forward is Dell's official APIs or written permission, not a quiet policy exception.

**Ubiquiti becomes the recommended substitute for the clean-API pilot archetype.** Its firmware endpoint at `fw-update.ubnt.com` returns structured JSON with channel, platform, product, timestamps and checksums, and the host serves no `robots.txt` (404), so no robots restriction applies. Its terms still require review before launch, and it is registered `terms_review_status = 'pending'` until that happens.

**`robots.txt` is fetched, parsed and cached by the platform**, not consulted manually. The fetcher honours it for the declared user agent, honours `Retry-After`, and applies per-domain concurrency limits. A source whose robots policy changes to disallow collection transitions out of the dispatchable set automatically at the next re-evaluation.

**Evidence is stored as concise excerpts and links, never as full documents.** Release notes are linked at their canonical URL. What FirmScout retains is the factual metadata and a short excerpt sufficient to verify the extraction, which is what the evidence-first principle actually requires.

The absolute prohibitions are restated here because they constrain code, not just conduct: never bypass authentication, never circumvent CAPTCHAs or access controls, never evade rate limits, never ignore `robots.txt`, never reproduce copyrighted release documents in full, and never present unofficial information as official.

## Consequences

### Positive

- The most attractive-looking source cannot be collected by accident. The gate is in the dispatch query.
- A self-hoster inherits the same constraints, because they are data in the registry rather than knowledge in a maintainer's head.
- A vendor asking why FirmScout collects their site gets a precise answer, including when the terms were reviewed and by whom.
- A vendor asking FirmScout to stop is handled by flipping one field, with the record retained for audit.
- The Discovery Agent's proposals are harmless by construction: a proposed source arrives with `terms_review_status = 'pending'` and `enabled = false`, so an agent cannot cause collection.
- Contributors get an unambiguous rule to follow, which is kinder than a vague instruction to "be respectful."

### Negative

- **FirmScout will have gaps that a less scrupulous competitor does not.** Dell is the first example and will not be the last. Some of those gaps will be visible to users as missing products, and the honest explanation ("we are not permitted to collect this") is less satisfying than having the data.
- Terms review is human work that does not scale linearly with the number of vendors, and it is the likeliest bottleneck when onboarding a large vendor batch.
- `robots.txt` interpretation is genuinely ambiguous in places — path matching, wildcard handling, and the question of whether a directive aimed at search crawlers binds a metadata monitor are all matters on which reasonable people differ. The project resolves ambiguity conservatively, which sometimes means declining a source that might have been permissible.
- A cached robots policy can be stale between re-evaluations, leaving a window where FirmScout collects from a source that has just disallowed it.

### Neutral

- Compliance status is orthogonal to source quality. An official manufacturer source can be prohibited; a community source can be permitted. The two fields are recorded separately and neither implies the other.
- The policy makes no claim about what the law requires. It describes what the project chooses to do, which is deliberately more conservative than the legal minimum in several respects.

## Alternatives considered

### Treat `robots.txt` as advisory for a non-search-engine client

Some argue that `robots.txt` addresses search crawlers and does not bind a targeted metadata monitor fetching a handful of pages per day, and that the relevant question is the site's terms of use rather than its robots file.

Rejected, for a reason that is more about the project than about the argument's merits. FirmScout is an open-source project asking manufacturers to view it as a good-faith participant in their ecosystem, and it publishes its collection behaviour in a public repository. A stated policy of respecting `robots.txt` that is then qualified with exceptions is worth less than no policy, and the first time a vendor noticed the exception it would cost far more in trust than the source is worth. The conservative reading is also the one that stays defensible if the legal position changes.

### Compliance as a pull-request checklist only

Rejected. It does not re-evaluate over time, does not scale to agent-proposed sources, and leaves no machine-readable record for self-hosters. A checkbox in a merged pull request is not a control.

### Collect first, remove on complaint

Rejected outright. It inverts the burden onto the vendor, is incompatible with the project's public positioning, and would poison the vendor relationships that are the only realistic path to better sources — an official API from a manufacturer is worth more than any amount of careful scraping, and it is not offered to projects that behaved badly first.

### Drop Dell from the catalogue entirely

Rejected as less useful than registering it disabled. Deleting the record loses the evaluation, guarantees the question gets re-opened, and makes the catalogue look merely incomplete rather than deliberately constrained.

## Revisit when

- A vendor grants written permission, an API licence, or a partnership that changes `terms_review_status` for their sources. Dell is the first candidate.
- The proportion of registered sources blocked by compliance exceeds roughly 15%, which would suggest the conservative reading is costing more coverage than it buys in trust and warrants a legal opinion on specific categories.
- Qualified legal review returns answers on the itemised questions in [`licensing.md`](../architecture/licensing.md), particularly on collecting from hosts whose `robots.txt` disallows it and on the use of undocumented vendor update APIs.
- A jurisdiction FirmScout operates in changes the legal status of automated retrieval of publicly available factual metadata.
- Terms review becomes the measured bottleneck in vendor onboarding, at which point the response is more reviewer capacity or a tiered review depth, not a relaxed policy.
