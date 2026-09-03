# Dataset Correction Policy

Published facts will sometimes be wrong — a collector misreads a page, a vendor publishes an error and quietly fixes it, a date's precision was overstated, or a genuine release turns out to be a duplicate under a different alias. This document describes how a wrong published fact gets corrected, honestly and traceably, without violating FirmScout's immutable-history principle.

See also: [GOVERNANCE.md §6](../../GOVERNANCE.md#6-dataset-stewardship) for who has approval authority, and [`docs/diagrams/dataset-correction.md`](../diagrams/dataset-correction.md) for the visual flow.

## History is immutable. Corrections are new rows.

This is the load-bearing rule and it does not bend for corrections: **`releases` rows are never updated or deleted.** A correction is a **new row** that references the one it corrects (`corrects_release_id`), carries its own evidence, and is tagged with who approved it and when. The original row stays exactly as it was, still queryable, so that "what did FirmScout say on 30 June" remains a truthful, reconstructible answer even after the fact underneath it turned out to be wrong. Silently editing history would make every past API response a potential lie.

The same applies to withdrawal: a release that turns out not to exist, or that a vendor pulled, is marked `withdrawn = true` with a reason and evidence — never deleted.

## Submitting a correction

Anyone — not just contributors — can submit a correction, either through the [`data_correction` issue form](../../.github/ISSUE_TEMPLATE/data_correction.yml) or, for anyone comfortable with the dataset format, a pull request. A submission should include:

- **What's wrong**, specifically: the release (by ID, or by product + version) and the field that's incorrect.
- **What it should be instead.**
- **Evidence** — see below. This is not optional.
- **How you found it** — a link to the vendor's page, a screenshot with a visible timestamp, a changelog entry, direct experience deploying the release, etc.

## Evidence requirements

A correction is only as credible as its evidence, and the bar scales with how consequential the change is:

- **A field-level correction with a clear, citable source** (e.g. "the vendor's changelog actually says 2026-08-15, not 2026-08-05 — here's the URL and a screenshot") is straightforward: it needs a source URL or equivalent citable evidence, and ideally a retrieval timestamp.
- **A withdrawal claim** ("this release doesn't actually exist" / "the vendor pulled it") needs evidence that it's actually gone or wrong, not just that you can't currently find it — vendors reorganise sites often, and "I can't find it anymore" is weaker evidence than "the vendor's page for it now returns 404 as of [date], see [archived screenshot / URL]."
- **A correction that reverses a source-quality judgment** (e.g. "this source isn't actually official, it's an unaffiliated mirror") needs the same domain-control evidence described in [source-quality-policy.md](source-quality-policy.md#how-a-class-is-assigned).
- **An assertion with no evidence beyond "I know this is wrong"** is treated as a lead, not a correction — it may prompt someone to go look, but it does not get applied on its own.

## Triage

1. **Intake.** A maintainer (or the relevant collector maintainer, if one exists for the affected vendor) reads the submission and checks whether the evidence, on its face, supports the claim.
2. **Verification.** Wherever possible, the evidence is checked independently — re-fetching the cited source, comparing against other active sources for the same product, or checking the artifact history already stored for the affected release's evidence chain.
3. **Classification.** The correction is one of:
   - **A new corrected release row**, linked via `corrects_release_id`, for a fact that was genuinely wrong (wrong date, wrong version string, wrong release type).
   - **A withdrawal**, for a release that shouldn't have been published at all.
   - **A duplicate merge**, when the "correction" turns out to be an alias-resolution problem (the same release under two product identities) rather than a factual error — this is a `release_product_mappings` fix, not a new release row.
   - **Rejected**, when the evidence doesn't hold up, with a reason recorded so the submitter (and future reviewers) understand why.
4. **A rejected correction is not deleted from the record either** — the review item stays, with its outcome, for the same reason releases aren't deleted: so the history of "we looked into this and here's why we didn't change it" survives.

## Approval authority

Per [GOVERNANCE.md §6](../../GOVERNANCE.md#6-dataset-stewardship):

- The **collector maintainer** for the affected vendor, where one exists, has first authority to approve a correction.
- **Any maintainer** may approve one where no dedicated collector maintainer exists, or where the collector maintainer is unavailable.
- A correction that **reverses a previous maintainer's own published judgment**, or that is otherwise contested, escalates to a maintainer vote rather than being approved unilaterally by whoever happens to see it first.
- A contributor with a [disclosed conflict of interest](../../GOVERNANCE.md#7-conflicts-of-interest) regarding the affected vendor may submit or discuss a correction, but per that policy, a contested correction concerning their own employer's products should get a reviewer without that conflict before it's approved.

## Attribution

The submitter of a correction is recorded, the same way collector and contribution authorship is recorded elsewhere in the project — both because it's the right thing to do for someone who did the work of finding and reporting an error, and because a track record of accurate correction submissions is exactly the kind of history that makes someone a strong candidate for collector-maintainer status later.

## Appeal path

If a submitter disagrees with a rejection or with how a correction was resolved:

1. **Ask, in the same issue or PR thread, for the reasoning to be reconsidered**, ideally with new or clarified evidence — most disagreements are resolved this way, because the original triage was working from incomplete information.
2. **If that doesn't resolve it, request escalation to a maintainer vote.** Any contributor can ask for this; it isn't limited to maintainers.
3. **Steering has final say** on an appeal that a maintainer vote doesn't resolve, per [GOVERNANCE.md §2](../../GOVERNANCE.md#2-how-decisions-are-made), particularly where the disagreement touches something with legal exposure (e.g. a vendor formally disputing a published fact — see the removal-request process in [DATA_SOURCES.md](../../DATA_SOURCES.md#removal-request-process)).

Throughout, the goal of an appeal is never to "win" — it's to get the historical record right, and because history is immutable, getting it right later is always still possible: a successful appeal produces a new corrected row, exactly like any other correction, with the appeal's evidence attached.
