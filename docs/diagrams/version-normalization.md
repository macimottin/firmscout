# Version normalisation

This diagram answers: when a collector extracts a raw version string, what happens between "text on a page" and "a value FirmScout is willing to store and reason about" — and why that process never sorts, compares, or trusts version strings as numbers.

```mermaid
flowchart TD
    raw["Raw version string,<br/>as extracted<br/>e.g. #quot;3.003.0015.001#quot;,<br/>#quot;CollabOS 2.1.B (2.1.121)#quot;,<br/>#quot;2026.24.5#quot;"]

    raw --> vendor_rules["Vendor-specific normalisation rules<br/>(collector config: trim, case,<br/>separator unification, prefix strip)"]

    vendor_rules --> ex1["Dell/BIOS style:<br/>#quot;3.003.0015.001#quot; kept intact,<br/>leading zeros preserved"]
    vendor_rules --> ex2["Suite-with-build style:<br/>#quot;CollabOS 2.1.B (2.1.121)#quot;<br/>split into display + build token"]
    vendor_rules --> ex3["Calendar-look style:<br/>#quot;2026.24.5#quot;<br/>NOT parsed as a date"]
    vendor_rules --> ex4["Date-based version:<br/>e.g. #quot;20260315#quot;<br/>kept as opaque token"]
    vendor_rules --> ex5["BIOS revision string:<br/>e.g. #quot;F.64 Rev A#quot;"]
    vendor_rules --> ex6["Proprietary build number:<br/>e.g. #quot;build_48213-x64#quot;"]

    ex1 --> tokenize
    ex2 --> tokenize
    ex3 --> tokenize
    ex4 --> tokenize
    ex5 --> tokenize
    ex6 --> tokenize

    tokenize["Tokenisation<br/>(split into typed segments:<br/>numeric runs, letter runs,<br/>separators, parenthetical groups)"]

    tokenize --> plausibility["Plausibility analysis ONLY<br/>(compare token shape and magnitude<br/>to the latest observed version<br/>for this product + channel)"]

    plausibility --> note_no_order["NOTE: this step never orders versions.<br/>It answers one question:<br/>#quot;is this transition plausible?#quot;<br/>It never decides which version is newer."]

    note_no_order --> plausible{"Transition plausible?<br/>(token count/shape similar,<br/>no drastic unexplained regression,<br/>no channel-incompatible jump)"}

    plausible -->|"yes — e.g. 7.24.1 to 7.24.2,<br/>or F.63 to F.64"| store_ok["Store raw AND normalised form<br/>on the candidate release"]

    plausible -->|"no — e.g. 7.24.2 to 1.0,<br/>or stable channel token shape<br/>suddenly resembling a beta build"| human_review["Route to human review<br/>(review_items)<br/>NOT auto-rejected,<br/>NOT auto-accepted"]

    plausible -->|"cannot evaluate — no prior<br/>observed version for this<br/>product + channel yet"| store_ok

    human_review -->|"maintainer confirms it is a<br/>legitimate transition<br/>(e.g. vendor renumbering)"| store_ok
    human_review -->|"maintainer confirms it is<br/>an extraction error"| reject["Candidate rejected<br/>(collector or config bug filed)"]

    store_ok --> both["releases.raw_version = as extracted<br/>releases.normalized_version = normalised form<br/>Both persisted, always.<br/>No numeric parsing at the database level.<br/>No ORDER BY version, ever."]
```

## What this shows

Normalisation exists to make display and matching consistent (trimming whitespace, unifying separators, stripping vendor boilerplate), not to impose an ordering. Tokenisation feeds exactly one downstream decision — plausibility of the observed transition relative to the latest observed version for that product and channel — and that decision has exactly three outcomes: store as plausible, route an implausible transition to a human for a judgment call, or reject an extraction that a human confirms is simply wrong. Both the raw and the normalised string are always persisted side by side; the normalised form is never treated as the source of truth for "newest".

## Assumptions

- "Latest observed" for the plausibility check is derived from release date, first-observed timestamp, and channel (per ADR-0017), and is independent of this pipeline — plausibility analysis *consumes* that value, it does not produce it.
- Vendor-specific normalisation rules are registry data (collector config), reviewable and versioned like any other collector configuration, not hardcoded per-vendor logic buried in the domain.
- An implausible transition is a *signal*, not a verdict. The default behaviour is neither silent acceptance nor silent rejection — both destroy trust in different ways, so the default is a queued human decision.
- When there is no prior observed version for the product/channel pair (first-ever release recorded), plausibility cannot be evaluated and the candidate proceeds — there is nothing to compare against.

## Failure modes

- A vendor changes its numbering scheme deliberately (e.g. moving from build numbers to calendar versioning, matching the `2026.24.5` example). The plausibility check will flag this as implausible on the first occurrence; that is the intended behaviour — it becomes a one-time human decision, not a recurring one, because after the maintainer confirms it, the new shape becomes the new baseline for future comparisons.
- Over-aggressive vendor normalisation rules could accidentally collapse two genuinely different versions into the same normalised string (e.g. stripping a suffix that actually distinguishes a hotfix). This is why `raw_version` is retained unconditionally — a normalisation bug is recoverable by re-deriving from raw, never by trusting normalised-only storage.
- A collector regression (broken selector) could feed garbage into tokenisation. The plausibility gate catches shape anomalies, but a garbage string that happens to look shape-plausible (e.g. truncated to a lone digit) still needs the gate-4/gate-5 checks from the update pipeline (non-empty, duplicate, date sanity) to be fully caught.

## Related ADRs

- [ADR-0017 — Version strings and date precision](../adr/0017-version-strings-and-date-precision.md)
- [ADR-0005 — Deterministic collectors](../adr/0005-deterministic-collectors.md)

## Implementing code

**Implemented.** This diagram describes code that exists and is covered by tests.

- `internal/domain/version.go` — tokenisation and `AssessTransition`
- `internal/domain/version_test.go` — the vendor shapes that break semver

See [the architecture consistency report](../architecture/consistency-report.md) for what is verified by execution and what is not.
