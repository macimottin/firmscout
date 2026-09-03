# Developing Collectors

A collector is the piece of FirmScout that turns a fetched artifact from a vendor source into candidate release facts. This guide covers how to build one, whichever kind you need. Read [`docs/architecture/blueprint.md` §13–14](../architecture/blueprint.md#13-collector-sdk) for the authoritative contract this guide walks through — this document is the practical companion, not a replacement.

## Configuration-driven vs. code-based collectors

FirmScout has two ways to extract candidates from a source, and choosing correctly matters for maintainability and for who can review your contribution.

**Configuration-driven collectors** (a YAML file under `collectors/config/<vendor>/`) are the default and should be your first attempt for any new source. They're interpreted by a generic engine (`html_selectors`, `text_regex`, and later `json_path`, `rss_atom`, `xml_xpath`, `pdf_text`, `github_releases`) and require no Go code at all. Their biggest advantage isn't developer convenience — it's that **a non-programmer can review and correct one**. A collector maintainer who knows a vendor's site intimately but doesn't write Go can propose a selector fix directly.

**Code-based collectors** (a Go package under `collectors/vendors/<vendor>/`) are for sources a configuration engine genuinely cannot express: multi-step authentication flows (within the bounds of what's compliant to automate at all — see [DATA_SOURCES.md](../../DATA_SOURCES.md)), pagination logic that depends on response content, formats with no generic engine yet, or extraction logic too conditional to express declaratively without the config becoming its own undocumented programming language.

**Rule of thumb:** if you find yourself wanting `if`/`else` branches, loops, or string manipulation beyond a `transform` pipeline inside a YAML config, that's a sign the source needs a code collector, not a more elaborate config. Don't grow the config engine's expressiveness ad hoc to avoid writing Go — that path leads to an unreviewable YAML DSL. Propose a new engine capability (with its own JSON Schema and tests) instead, if the need is general enough to justify one.

## The `Collector` interface

Every collector, config-driven or code-based, ultimately implements this contract (from blueprint §13):

```go
type Collector interface {
    ID() string                 // stable, e.g. "html_selectors" or "vendor.fortinet.docs"
    Version() string            // bumped on any behavioural change; recorded in evidence
    Vendor() string              // "" for generic engines
    Supports(src Source) bool
    Fetch(ctx context.Context, src Source, prior *FetchState) (Artifact, FetchOutcome, error)
    Extract(ctx context.Context, src Source, art Artifact) ([]CandidateRelease, error)
}
```

- **`ID()`** must be stable across versions — it's how evidence rows and collector configs reference a specific collector.
- **`Version()`** must be bumped on any change to extraction *behavior* (a new field, a changed selector, a different date-parsing rule), even if the ID stays the same. This is what lets a reviewer look at an old candidate's evidence and know exactly what code produced it.
- **`Supports(src Source)`** is a cheap, side-effect-free check of whether this collector can handle a given source — used by the `CollectorRegistry` to route.
- **`Fetch`** performs the actual network request. It receives the prior `FetchState` (ETag, Last-Modified) and **must** issue a conditional request when the source supports it — the SDK's shared `Fetcher` handles this for you; a code collector that opens its own HTTP client and skips conditional requests fails the collector contract test.
- **`Extract`** turns a fetched `Artifact` into candidate releases.

## `Extract` is a pure function — no exceptions

This is the rule that makes fixture-based testing meaningful, and it is enforced, not suggested: **`Extract` is a pure function of `(Source, Artifact)`.** It receives:

- **No clock.** It cannot call `time.Now()`. Anything that needs "now" (like a `retrieved_at` timestamp) is provided by the caller as part of the artifact or added later by the use case that turns a candidate into evidence — not computed inside `Extract`.
- **No repository.** It cannot query the database to check "does this product already exist" or "is this a duplicate." Identity resolution and duplicate detection are validation-gate concerns (blueprint §16), not extraction concerns.
- **No network.** All the network access already happened in `Fetch`; `Extract` only ever looks at the `Artifact` it was given.

The payoff: the same artifact produces the same candidates forever. A fixture recorded today and an `expected.json` written against it will still pass in five years, regardless of what changed elsewhere in the system. If your `Extract` implementation needs something that isn't in `(Source, Artifact)`, that's a sign the thing you're computing doesn't belong in extraction.

## Collectors return candidates. They cannot publish.

This is enforced by the type system, not by convention. `Extract` returns `[]CandidateRelease` — a type with no `ReleaseID`, because a collector never decides whether a candidate is real. Collector adapters have **no repository handle at all**: there is no code path by which a collector, however buggy or however malicious a submitted config might be, can reach the `releases` table. Publication happens later, through the `ValidateCandidateRelease` and `PublishRelease` use cases, which apply the deterministic gates in [blueprint §16](../architecture/blueprint.md#16-update-and-publication-rules) — product identity, duplicate detection, date plausibility, version-transition plausibility, evidence completeness, and confidence thresholds — before anything reaches the public catalogue.

## Resource limits

Collectors run against attacker-influenceable input (a compromised or hostile vendor site, or a malicious registered source). The SDK, not the collector, enforces:

- **Maximum artifact size** (`fetch.max_bytes` in config) — `Fetch` refuses to buffer a response larger than this.
- **Extraction wall-clock limit** — `Extract` is run under a context deadline; a collector that hangs (e.g. on pathological regex backtracking against attacker-controlled HTML) is killed and the run recorded as a failure, not left to consume the worker indefinitely.
- **Maximum candidates per artifact** — a collector that produces an implausible number of candidates from one artifact is capped and flagged, rather than allowed to flood the validation pipeline.

Do not implement your own equivalent of these inside a collector; rely on the SDK wrapper, and if you find you need a limit the SDK doesn't provide, propose adding it there so every collector benefits.

## Choosing the cheapest change signal

Not every check needs to fetch and hash a whole page. In order of preference, cheapest first:

1. **A dedicated, tiny "version" endpoint**, if the vendor has one (MikroTik's `NEWESTa7.stable` text file is the model case: a few bytes, ETag-bearing, purpose-built).
2. **Conditional HTTP** (`ETag` / `If-None-Match`, or `Last-Modified` / `If-Modified-Since`) against the real content page, when the server supports it — a `304 Not Changed` costs almost nothing.
3. **An official feed** (RSS/Atom, a JSON API) that a vendor already publishes for change notification, even if it's not the primary download page.
4. **Normalised section hashing**, when none of the above is available: fetch the full page, strip volatile content (scripts, timestamps unrelated to the release, ads), hash only the section that actually contains release data, and compare against the prior hash. This is the fallback, not the default — it costs a full fetch every check.

A source's `sources.yaml` entry and its collector config should reflect the cheapest signal actually available, and the fetch method used should be visible in evidence so a reviewer can tell how much a given check "cost."

## Worked example: the MikroTik changelog collector

This is the config-driven collector for MikroTik's structured HTML changelog page, as used in the first vertical slice, taken directly from [blueprint §14](../architecture/blueprint.md#14-configuration-driven-collector-specification) and measured against the real page on 2026-09-03:

```yaml
apiVersion: firmscout.dev/v1alpha1
kind: CollectorConfig
metadata:
  id: mikrotik.changelogs
  vendor: mikrotik
  version: 1
spec:
  engine: html_selectors
  product_match:
    product: mikrotik-routeros
  fetch:
    expected_content_type: text/html
    max_bytes: 2097152
  normalize:
    section_selector: "div.changelog-header"         # hash only this, ignore the rest of the page
    strip: [script, style, noscript, "[wire\\:id]", "[x-data]"]
  extract:
    release_container: "div.changelog-header"
    fields:
      version:
        selector: "span.font-bold"
        transform: trim
      channel:
        selector: "div.rounded-\\[4px\\]"
        transform: [trim, lowercase]
        map: { "long-term": long_term, "stable": stable, "testing": testing, "development": development }
      release_date:
        selector: ":scope"
        regex: "(\\d{4}-\\d{2}-\\d{2})"
        date_format: "2006-01-02"
        precision: exact_day
    release_type: embedded_os
    evidence_excerpt: ":scope"
```

Walking through why it's shaped this way:

- **`fetch.expected_content_type` and `max_bytes`** reject a response that isn't what this collector expects, before any parsing happens — a defensive check against the source silently changing shape.
- **`normalize.section_selector`** is the key cost-control decision: the page is 400+ KB and server-rendered with Livewire/Alpine, but the release data lives entirely inside `div.changelog-header` elements, so only that section is hashed for change detection. Hashing the whole page (including per-visit dynamic attributes `wire:id` and `x-data`) would produce false "changed" signals on every check.
- **`extract.release_container`** tells the engine each match of this selector is one candidate release.
- **`fields.channel.map`** normalises the vendor's display labels ("Long-term", "Stable", ...) into FirmScout's constrained `channel` vocabulary — note this is a lookup table, not an inference; an unmapped label should fail loudly rather than silently guess.
- **`release_date.precision: exact_day`** is set explicitly, matching what's actually observed on the page (a full `YYYY-MM-DD`) rather than left to be inferred — per the "no invented precision" principle, a config should never claim a precision the regex it pairs with can't actually guarantee.
- **`release_type: embedded_os`** is a fixed value for this source, not inferred, because everything this collector extracts is RouterOS.

## Fixture-based testing

Every collector, config-driven or code-based, is tested exclusively against recorded fixtures — **never against the live vendor site.** See [CONTRIBUTING.md](../../CONTRIBUTING.md#fixtures-live-vendor-sites-are-never-a-test-dependency) for how to record one, including the provenance requirements. The SDK provides `collectors/sdk/collectortest.RunFixtures(t, collector, dir)`: drop a `*.fixture.html` (or `.json`/`.txt`) file next to an `expected.json` describing the candidates it should produce, and the harness does the rest. Add coverage for the failure modes that matter most in practice: a layout change that makes a selector miss entirely, a release with a missing or empty version, and a date with reduced precision (month-only, year-only).

## Checklist for submitting a collector

- [ ] Chose configuration-driven unless there's a specific, stated reason a code collector is needed.
- [ ] `Extract` (or the config's declarative extraction) is a pure function of `(Source, Artifact)` — no clock, no repository, no network.
- [ ] The cheapest available change signal is used (see "Choosing the cheapest change signal" above), and evidence records how the check was performed.
- [ ] `normalize.section_selector` (or equivalent) excludes volatile, release-irrelevant content from hashing.
- [ ] `release_date_precision` matches what the source actually provides — never inferred as more precise than the evidence supports.
- [ ] `channel` and `release_type` use the project's constrained vocabularies, not free text.
- [ ] At least one fixture with full provenance (source URL, retrieval timestamp, content hash), trimmed to the minimum needed.
- [ ] At least one edge-case fixture (missing field, reduced date precision, or a deliberately broken layout) with a documented `expected.json`.
- [ ] The source's compliance status (`robots_policy_status`, `terms_review_status`) is filled in honestly in its `dataset/sources/` entry — see [DATA_SOURCES.md](../../DATA_SOURCES.md).
- [ ] JSON Schema validation passes for the config (`packages/schemas/`).
- [ ] `go test ./...` (or the equivalent fixture command) passes locally.
