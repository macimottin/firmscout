# ADR-0017: Opaque version strings, derived "latest," and explicit date precision

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** founding team
- **Requires qualified legal review:** no
- **Related:** ADR-0003, ADR-0005

## Context

Real vendor version strings do not follow semantic versioning. The brief itself gives examples like `3.003.0015.001` and `CollabOS 2.1.B (2.1.121)`, and FirmScout's own measured pilot data reinforces this — MikroTik's RouterOS versions (`7.24.2`) look semver-like but are not guaranteed to stay that way, and other vendors in scope use entirely different schemes. Any system design that assumes version strings can be parsed into comparable major/minor/patch integers will eventually encounter a string it cannot parse, or worse, will parse it "successfully" into a wrong comparison — for example, treating `CollabOS 2.1.B` and `2.1.121` as comparable when they may not even represent the same numbering scheme. A wrong "this is newer" determination is not a cosmetic bug for this product: FirmScout's entire value is being a trustworthy source for "what is the current version," so an incorrect ordering directly damages the product's core claim.

A closely related problem exists for dates. Vendors frequently publish a release with only a month ("February 2026") or only a year, not always a specific day. Storing an invented day (defaulting to the 1st, for instance) to fit a `DATE` column would silently manufacture a precision the source never provided, which is a fabrication the evidence-first principle (§1) explicitly forbids.

## Decision

**Version strings are opaque text, never an ordered type.** `raw_version` and `normalized_version` are stored as `TEXT`, with no numeric parsing at the database level and no `ORDER BY version` anywhere in the query set (§11 item 5). "Latest observed" is **never** computed by sorting version strings. It is derived instead from **release date, first-observed timestamp, and channel** (§3.5) — the newest release row, by these observable, unambiguous signals, is the latest observed release, regardless of what its version string looks like relative to others.

Version **tokenisation exists only to answer a narrower, different question: "is this transition plausible?"** A `VersionString` value object performs plausibility analysis — for example, flagging a jump from `7.24.2` to `1.0` as suspicious — and that analysis routes a candidate to human review (validation gate 6, §16) when a transition looks implausible. It is explicitly never used to decide which of two releases is newer; that determination always comes from date and observation metadata, never from comparing the tokenised structure of two version strings against each other.

**Dates carry explicit precision.** `release_date DATE` is paired with a required `release_date_precision` enum: `exact_day`, `month_only`, `year_only`, or `unknown`. A `CHECK` constraint enforces that `month_only` precision stores day = 1 and `year_only` precision stores month = day = 1, so the stored date value is a canonical anchor for sorting and filtering, never a claim that a specific day is known. `unknown` precision requires `release_date IS NULL` — there is no anchor date at all when none is known (§11 item 4). At the API layer, `releaseDate` is rendered as `YYYY-MM` for month precision and `YYYY` for year precision, and omitted entirely for `unknown`, specifically so that **an API consumer cannot parse a fake day out of the response** (§12) — the precision discipline is enforced not just in storage but all the way through to what a client can observe.

## Consequences

### Positive

- The system never claims false precision: a release known only to have shipped "in February 2026" is never displayed, stored, or returned via the API as having shipped on a specific day, which directly protects the evidence-first trust claim the entire product depends on.
- "Latest observed" is well-defined and computable for every product regardless of how inconsistent or vendor-specific its version numbering scheme is — MikroTik's near-semver strings, a build-number scheme, and a marketing-name-plus-build scheme are all handled identically because none of them is ever sorted against another.
- Version plausibility checking still catches genuinely useful signal (a large unexplained regression, a token-shape change suggesting a different numbering scheme entirely) and routes it to human review, without pretending that signal is strong enough to determine chronological order.
- The date-precision `CHECK` constraint makes an invalid state (e.g., `month_only` precision with a day component that isn't 1) impossible to insert, not just discouraged — the database itself enforces the invariant, consistent with ADR-0002's preference for invariants enforced in constructors and constraints rather than by convention.
- Query answers the brief's temporal questions directly rather than by reconstruction: "latest observed" is the row flagged latest, "latest as of date D" is the newest non-withdrawn row with `first_observed_at ≤ D`, "first observed" is a column (§16, closing paragraph) — none of this requires parsing or comparing version strings at query time.

### Negative

- Users and API consumers accustomed to semantic-versioning tools (sorting, range comparisons like `>=1.2.0`) do not get that capability from FirmScout by design — there is no way to ask "give me all releases newer than version X" by version comparison alone; the question has to be reframed in terms of date or observation order, which is a real capability gap relative to ecosystems that do assume semver.
- Plausibility-check false positives (a legitimate but unusual version jump flagged as implausible) route to human review rather than auto-publishing, adding review-queue load for genuinely valid releases that happen to look suspicious by token-shape heuristics — the specific plausibility thresholds need tuning against real-world version schemes as more vendors are onboarded, and are not free of tuning risk.
- Storing `month_only` and `year_only` dates as a canonical anchor (day = 1, or month = day = 1) creates an attractive nuisance: any code path that reads `release_date` without also checking `release_date_precision` will silently treat a month-only release as if it happened on the 1st. This is mitigated by the API rendering discipline described above, but any new internal code path touching `release_date` has to remember to respect precision — it is not automatically impossible to get this wrong in application code the way the database constraint prevents it at the storage layer.
- `unknown` precision requiring `release_date IS NULL` means such releases cannot be placed in simple date-range queries or date-sorted lists at all without special-casing — correct, but adds a branch every date-ordering query needs to consider (e.g., where do `unknown`-precision releases sort relative to dated ones in a UI list).

### Neutral

- This ADR does not specify the exact plausibility-check algorithm or its thresholds (what counts as a "suspicious" transition) — that logic lives in the `VersionString` value object's implementation and is expected to be tuned empirically as more vendor version schemes are observed.
- The diagram `docs/diagrams/version-normalization.md` documents the tokenisation and plausibility-check flow in more detail than this ADR restates.

## Alternatives considered

### Semantic versioning with a fallback for non-conforming strings

Rejected. A "try to parse as semver, fall back to opaque" approach would work correctly for the subset of vendors that happen to use semver-shaped strings and silently do the wrong thing for edge cases within that same subset (MikroTik's versions look semver-shaped but nothing guarantees they always will, and a false sense of "this vendor is semver" is worse than treating every vendor uniformly as opaque). It would also create two code paths with different guarantees, doubling the testing burden for a distinction with no real payoff, since date-based "latest" already answers the question semver ordering would have been used for.

### A per-vendor pluggable version-comparison scheme (each vendor registers its own ordering logic)

Rejected. This would require every new vendor's registry entry to also define comparison logic, adding real complexity and a new source of subtle bugs (a poorly written comparator silently misordering releases) for a capability — direct version-to-version ordering — that the date-and-observation-based "latest" already provides without needing per-vendor logic at all.

### Storing dates without precision, defaulting unknown-day releases to the 1st of the month

Rejected outright, consistent with the "no invented precision" operating principle stated at the top level of the blueprint (§1). This is the single most direct violation of the evidence-first commitment this ADR exists to prevent — a fabricated day looks identical to a real one to any consumer of the data, silently corrupting trust in every date the system reports.

## Revisit when

- A vendor or product category emerges where date precision below `month_only` is genuinely needed (e.g., intra-day release timing matters for a specific compliance use case) — the current enum would need an additional, more granular value, which is a schema-compatible addition, not a breaking change.
- Plausibility-check false-positive rates (tracked via human review queue outcomes: how often a flagged "implausible" transition turns out to be valid) are high enough to warrant tuning the `VersionString` heuristics, once real data from several vendors' actual version-numbering quirks is available.
- API consumers request version-range or version-comparison query capability with enough frequency (tracked via the product-analytics events, ADR-0012, and API support requests) that the lack of any version-ordering feature becomes a validated product gap rather than a design that has not yet been challenged by real usage.
