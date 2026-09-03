# ADR-0005: Deterministic, config-driven collectors that cannot publish

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** founding team
- **Requires qualified legal review:** no
- **Related:** ADR-0006, ADR-0016, ADR-0018

## Context

FirmScout's core economic claim is a cost per monitored source low enough that monitoring tens of thousands of sources is boring, not exceptional (§1). That claim only holds if the default path for checking a source and extracting a candidate release is cheap, fast, reproducible, and reviewable by someone who is not a Go programmer — a maintainer proposing a new vendor's collector should be able to do so with a YAML file and a fixture, not a code change and an AI call. It also has to be trustworthy: a catalogue of plausible-looking wrong firmware versions is actively harmful to someone patching a device (§3.11), so whatever produces candidate data must be testable against a fixed input and produce a fixed output, forever, with no hidden dependency on network state, wall-clock time, or a language model's non-determinism.

## Decision

Collectors implement a fixed, four-stage separation — **fetch, extract, validate, publish** — and only the first two stages are the collector's responsibility. The collector contract (§13) is:

```go
type Collector interface {
    ID() string
    Version() string
    Vendor() string
    Supports(src Source) bool
    Fetch(ctx context.Context, src Source, prior *FetchState) (Artifact, FetchOutcome, error)
    Extract(ctx context.Context, src Source, art Artifact) ([]CandidateRelease, error)
}
```

`Fetch` must issue a conditional HTTP request when the source supports it (using the prior ETag/Last-Modified), enforced by the SDK's `Fetcher` rather than trusted to each collector implementation. `Extract` is a **pure function** of `(Source, Artifact)` — it receives no clock, no repository, and no network access, so the same artifact produces the same candidates every time, which is what makes fixture-based testing meaningful at all.

Most sources are handled by **configuration-driven engines** (`html_selectors`, `text_regex` in the MVP; `json_path`, `rss_atom`, `xml_xpath`, `pdf_text`, `github_releases` on the roadmap), expressed as versioned YAML (`apiVersion: firmscout.dev/v1alpha1`) under `collectors/config/<vendor>/`. A code-based collector is the exception, used only when a source's structure cannot be expressed declaratively. Candidates carry `RawVersion`, `NormalizedVersion`, `ReleaseDate` as a `PartialDate`, `ReleaseType`, `Channel`, applicability hints, an evidence excerpt, and a `Confidence` score — but **no `ReleaseID`**, because a collector does not decide whether an extracted observation becomes a published fact.

This last point is enforced structurally, not by convention: collector adapters have no repository handle at all (§7.4, §8.2). Extraction returns `[]CandidateRelease` to the application layer; a separate `ValidateCandidateRelease` use case runs the deterministic gates (§16), and only `PublishRelease` writes a `releases` row. A collector cannot publish because its Go type signature makes publishing something it cannot express, not because a reviewer remembered to check.

Testing is fixture-based and this is treated as an absolute rule, not a preference: `collectors/sdk/collectortest.RunFixtures` runs every recorded fixture against the collector and compares output to an `expected.json`, and **a live vendor site is never a test dependency** (§7.8). A test suite that fails because a vendor reorganised their site teaches contributors to ignore CI failures, which is worse than not having the test.

## Consequences

### Positive

- A new collector is, in the common case, a YAML file plus a fixture pair — no Go knowledge required, and reviewable as data by a maintainer who understands the target vendor's site but not Clean Architecture.
- Determinism makes collector behaviour fully specified by its recorded fixtures: `Extract` given the same artifact always produces the same candidates, so a regression in extraction logic is caught by a fixture test, not discovered in production against live data.
- Cost per check stays a fraction of a cent because the default path involves no AI call — conditional HTTP requests and section hashing (§16) mean most checks are a cheap `304 Not Modified` or an unchanged-hash outcome, and extraction only runs when content actually changed (T2).
- Collectors being structurally unable to publish removes an entire class of bug: there is no code path where a collector's mistake or a compromised or malicious vendor of a config file could insert a fact directly into the public catalogue.

### Negative

- Configuration-driven engines cannot express every source structure. Sites with heavy client-side rendering, sources requiring authentication flows beyond a static header, or genuinely irregular layouts fall back to code collectors, which are more expensive to write and maintain, and the boundary between "config can express this" and "needs code" is a judgment call that will sometimes be made wrong and need revisiting.
- Fixture-based testing means the test suite is only as good as the fixtures' fidelity to reality. A vendor's HTML can drift in ways the fixture does not capture, so passing tests are evidence of correct extraction logic against a known input, not a guarantee of current correctness against the live site — that gap is exactly what source health monitoring and AI repair (ADR-0006) exist to catch.
- Pure, side-effect-free `Extract` functions occasionally push complexity into how a collector's supporting data (e.g., a large lookup table for model-name normalisation) is packaged, since the function cannot fetch anything else at extraction time.
- Maintaining fixtures with clear provenance (URL, timestamp, hash — required per §B5 of the implementation plan) is ongoing work that grows with the number of vendors; stale or synthetic-only fixtures reduce the tests' value over time if not refreshed.

### Neutral

- The YAML collector config spec is versioned by `apiVersion`, and a breaking change to field semantics bumps it — this is a deliberate acknowledgment that the config format itself will evolve as more source shapes are encountered, and is not expected to be frozen at MVP scope.

## Alternatives considered

### AI-driven extraction as the default path for every source

Rejected. This is the most consequential rejection in the collector design and is reinforced separately in ADR-0006: an LLM invoked on every check would make cost proportional to check volume rather than to change frequency, would make behaviour non-reproducible run to run, and would make "why did this candidate appear" much harder to audit than a deterministic selector or regex. AI is reserved for genuine escalation — new source discovery and repair after deterministic extraction fails — never for the default extraction path.

### Code-only collectors (no configuration-driven engine)

Rejected. Requiring every new vendor to be a Go package would gate community contribution behind Go proficiency and a full PR review cycle for what is, for the majority of sources (T3: config expected to cover most HTML and JSON sources), a mechanical selector-and-field mapping. The config engine exists specifically to lower that bar.

### Allowing collectors a repository handle, relying on code review to prevent them publishing

Rejected. This is a real category of alternative — "trust but verify via review" — and it was rejected because ADR-0002's whole premise is that structural enforcement outlasts review diligence under time pressure. Giving collectors persistence access for convenience and relying on reviewers to catch misuse would recreate the exact failure mode Clean Architecture's mechanical boundary is meant to prevent.

## Revisit when

- The ratio of code collectors to config collectors, measured after roughly 50 vendors are registered (T3), is high enough that the configuration engine's coverage assumption was wrong and the engine's field vocabulary needs expansion rather than more code collectors.
- Ratio of `changed` fetch outcomes to actual new releases (T2) is high enough that section-hash normalisation is not filtering noise effectively, driving unnecessary extraction runs and AI escalation cost.
- A source category emerges (e.g., authenticated APIs, GraphQL endpoints) common enough across vendors to justify a new first-class config engine rather than repeated one-off code collectors.
