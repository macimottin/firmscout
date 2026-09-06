# ADR-0020: Multi-source conflict is a recorded finding, never an auto-resolved guess

- **Status:** Accepted
- **Date:** 2026-09-05
- **Deciders:** founding team
- **Requires qualified legal review:** no
- **Related:** ADR-0005, ADR-0013, ADR-0017, ADR-0018

## Context

Validation gate 10 — "do the active sources agree?" — has existed since the first vertical slice and has always passed, because nothing populated `ValidationContext.ConflictingSourceVersions`. The gate was real; the input was not. A gate that structurally cannot fail is worse than an absent one: it appears in `validation_results` as a verdict a reviewer can rely on, and it says "no disagreement among active sources" whether or not anybody looked.

Populating it turns out to require three decisions that the gate itself does not contain.

The first is **where "what does each source currently say" comes from**. The obvious answer is `candidate_releases`, which already holds every observation any source ever made. It is the wrong answer, because that table is an append-log deduplicated per `(source_id, dedupe_key)`, so "the current claim of source X" is not a row it holds — it is the result of an ordering over rows. The only orderings available are the version string, which ADR-0017 forbids because vendor version strings have no defined order, and `discovered_at`, which records when FirmScout looked rather than which release is newer: a 46-entry changelog page produces 46 candidates discovered in the same second, and the newest release is not the last one parsed.

The second is **what a disagreement means**. `docs/diagrams/multi-source-conflict.md` already answers most of it with an authority ladder — official manufacturer, authorised portal, vendor repository, trusted community, unknown third party — that only ever lets a higher tier stand in for a lower one. But the diagram also describes two same-tier tie-breakers: prefer the more recent evidence, then the decisively higher confidence "by a wide, configured margin". Those tie-breakers publish a value only one source supports.

The third is **whether a conflict is a thing or a computation**. `product_summaries.has_source_conflict` is a boolean the public product page reads. It could be derived in SQL from the observations with a `GROUP BY ... HAVING count(DISTINCT normalized_version) > 1`.

## Decision

**A conflict is computed from a per-source observation projection, assessed in the domain, and recorded as a row with a lifecycle.**

`source_observations` holds exactly one row per `(source_id, product_id, channel)`: what that source currently claims, with the date it published and the evidence behind it. A row advances only when the new observation is *later* than the stored one, and "later" is `domain.LaterObservation` — a known release date beats an unknown one, then the later date wins, then the later observation instant breaks the tie. It never compares version strings. The ordering rule is stated once, in the domain, where a test can execute it.

`domain.AssessSourceConflict` compares one source's claim with every other eligible source's by string equality on the normalised version, and returns one of three statuses. `outranked` means every disagreeing source sits *strictly below* the subject on the authority ladder: the higher-authority value stands, the losing observations are retained and named in gate 10's detail, and publication proceeds. `unresolved` means at least one disagreeing source sits *at or above* the subject's authority. Ineligible sources are ignored entirely, and a source's authority and eligibility are read from the registry at load time rather than denormalised onto the observation, so a source that is reclassified or disabled stops counting on the next read rather than when something rewrites its rows.

**Same-tier disagreement is never auto-resolved, and the diagram's recency and confidence tie-breakers are deliberately not built.** Two official manufacturer sources disagreeing is the finding, not a problem to be arbitrated. The same rule applies at every other tier: two community sources disagreeing is a conflict a human settles, not a race the more recent one wins.

`domain.SourceConflict` is a row in `source_conflicts` with `open` and `resolved` states, the participating versions and sources, the authority rank the disagreement sits at, and the review item a human will act on. `product_summaries.has_source_conflict` is `EXISTS (SELECT 1 FROM source_conflicts WHERE product_id = p.id AND state = 'open')` — a question with no policy in it.

**One open conflict per product and channel carries at most one open review item.** `UpsertOpenConflict` reports whether it opened the conflict or refreshed one already open; a review item is created only on the former, or when an open conflict has no item attached. Otherwise the validation result reuses the existing item's id.

## Consequences

### Positive

- Gate 10 can now fail. The rule it encodes — a version only one source supports does not publish itself — is enforced rather than declared, and `TestGateTenRoutesUnresolvedConflictToReview` fails if it is ever reverted to always-pass.
- The authority ladder exists in exactly one place, in Go, where `TestAssessSourceConflictNeverResolvesTwoOfficialSources` executes it. There is no second copy in a SQL `CASE` expression that could drift and produce a product page saying "sources disagree" while the pipeline says they do not.
- A conflict outlives the candidate that triggered it. A reviewer opening the queue a week later still has something to resolve, and the product page has something to link to.
- The queue cannot be flooded by one disagreement. Two sources checked every six hours produce one review item, not eight a day.
- The projection is small and bounded — one row per source, product and channel — so the conflict query is an index scan over a handful of rows rather than an aggregate over the whole candidate log.
- A losing observation is never deleted. `ConflictVerdict.OutrankedVersions` is retained and named in the gate's recorded detail, so a maintainer reading `validation_results` sees that a disagreement existed and why it did not stop the release.

### Negative

- **The system is deliberately less decisive than the design diagram promised.** Two same-tier sources with a stale one and a fresh one produce a review item that a recency tie-break would have cleared automatically. Every such item is human time this ADR is choosing to spend, and if the pilot vendors turn out to disagree often the queue will feel noisy before it feels careful.
- A second projection table has to be kept correct. `source_observations` is written on every validation and read on every validation; a bug that fails to advance a row makes a source appear to still claim an old version, which manufactures a conflict that does not exist. The unique index makes duplicate current rows impossible but nothing makes a *stale* row impossible.
- The summary refresh on conflict open and close is a synchronous write inside the validation path, which makes validation slightly slower and couples it to the summary table. The alternative — enqueueing `JobRefreshSummary` — was rejected because `apps/worker` leases four job kinds and that is not one of them, so the job would never run and `has_source_conflict` would never become true. That is a real coupling accepted for a real reason, and it will need revisiting when the worker's job kinds are widened.
- `AssessSourceConflict` compares normalised version strings for equality, so two sources that describe the same release differently — `7.24.3` and `RouterOS 7.24.3` — are recorded as disagreeing until a collector's normalisation is fixed. The conflict is honest about what the strings say; it is not evidence that the vendor shipped two releases.
- Conflicts are detected only for candidates that reach validation with a resolved product. A source reporting a version for a product FirmScout cannot match contributes nothing, so a disagreement can hide behind a product-matching failure.

### Neutral

- The recency and confidence branches of `docs/diagrams/multi-source-conflict.md` remain in the diagram, marked designed-but-not-built. They are a description of a possible future, not a description of the code, and the diagram says so.
- Nothing here decides *how* a human resolves a conflict beyond accepting or rejecting the candidate that triggered it. Choosing a value neither source reported, or splitting a conflict per channel, is not modelled.

## Alternatives considered

### Derive the conflict flag in SQL from the observations

Rejected. The query would have to distinguish a disagreement the ladder settles from one it does not, which means re-implementing `domain.QualityClass.Authority()` as a `CASE` expression. Two copies of an authority ladder — one in Go covered by tests, one in SQL covered by nothing — is precisely the drift that produces a product page and a pipeline that disagree about whether the sources disagree. It would also leave nothing for a reviewer to resolve and nothing for the web page to link to, because a derived boolean has no identity, no history and no owner.

### Query `candidate_releases` at validation time instead of maintaining a projection

Rejected. It requires an ordering over candidates, and the two orderings available are forbidden (version string, ADR-0017) or wrong (`discovered_at`, which is when FirmScout looked). Writing that ordering into a query would put the "which observation is current" rule in SQL, unexecuted by any test, which is the same failure as the previous alternative in a different table.

### Implement the diagram's recency and confidence tie-breakers as described

Rejected for this phase, on three grounds. A recency tie-break publishes a value only one source supports, which is exactly the guess gate 10 exists to refuse — a staged regional rollout and a genuine error look identical to it. "A wide, configured margin" is a number nobody has evidence for, and this repository's rule is not to invent thresholds. And with two pilot vendors and at most two sources per product, the branch would be dead code with no fixture that exercises it honestly; building it now means testing it against invented data and shipping the result as if it were validated.

### Keep gate 10 always-passing until more vendors are onboarded

Rejected. A gate that cannot fail is a false assurance recorded in `validation_results` on every candidate. Either the check is real or the row should not say the check ran.

## Revisit when

- The review queue's conflict items are consistently resolved the same way — a reviewer picking the more recent evidence every time, over a measurable number of items — which is the evidence a recency tie-break needs and does not have today.
- A product accumulates more than two eligible sources, at which point "at or above this source's authority" starts admitting three-way disagreements whose participant list is worth modelling more richly than two arrays.
- `apps/worker` leases a wider set of job kinds, making an asynchronous summary refresh viable and removing the synchronous write from the validation path.
- A vendor is onboarded whose sources disagree by design — a regional mirror that lags the primary by a known interval — which is a case the ladder cannot express and would need a per-source "expected lag" rather than a tie-break.
