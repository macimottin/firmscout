# Configuration-driven collector specification

> Expands [blueprint §14](blueprint.md#14-configuration-driven-collector-specification). This is the complete specification for the YAML documents under `collectors/config/<vendor>/*.yaml`, interpreted by a generic engine (`html_selectors`, `text_regex` in the MVP) into the `Collector` contract defined in [collector-sdk.md](collector-sdk.md). Read that document first if you have not — this one assumes its types (`CandidateRelease`, `PartialDate`, `Evidence`, `Applicability`).

## 1. Config file shape

Every collector config is one YAML document with four top-level keys:

```yaml
apiVersion: firmscout.dev/v1alpha1
kind: CollectorConfig
metadata:
  id: mikrotik.changelogs
  vendor: mikrotik
  version: 1
spec:
  engine: html_selectors
  # ... engine-specific fields, see §3
```

- **`apiVersion`** (`firmscout.dev/v1alpha1` in the MVP) namespaces the schema so a future breaking change to field semantics can be introduced as `v1beta1` or `v1` without silently reinterpreting existing files. The engine that loads a config refuses to interpret an `apiVersion` it does not recognise — it does not attempt a best-effort parse of an unknown version.
- **`kind`** is always `CollectorConfig` in the MVP. It exists so that, if `packages/schemas/` ever hosts more than one kind of registry document validated by the same tooling, a loader can dispatch on it rather than inferring shape from content.
- **`metadata`** identifies the config; see §2.
- **`spec`** is everything engine-specific; see §3 onward.

**Versioning policy for the spec itself:** a change to `apiVersion` is warranted when field *semantics* change in a way that would silently misinterpret an existing config under the old meaning — for example, if `extract.fields.*.regex` changed from "first capture group" to "whole match" semantics. Adding a new optional field, a new engine, or a new transform is **not** a breaking change and ships under the same `apiVersion`. `packages/schemas/collector-config.schema.json` (§8) is versioned alongside `apiVersion`, and CI validates every checked-in config against the schema version its `apiVersion` declares.

## 2. `metadata` field reference

| Key | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `metadata.id` | string | yes | — | Stable identifier for this config, dotted `vendor.source` convention (e.g. `mikrotik.changelogs`). Becomes the running collector's `ID()` (collector-sdk.md §2). Must never change once evidence has referenced it — see collector-sdk.md §7 on why. |
| `metadata.vendor` | string | yes | — | Vendor slug; must match an existing `dataset/vendors/<slug>.yaml` entry. Validated by `firmscout registry sync`, not just the JSON Schema, because schema validation cannot check cross-file references. |
| `metadata.version` | integer | yes | — | This config's own revision number, starting at `1`, incremented on every change to `spec` that could alter extraction output (same bump discipline as `Collector.Version()` in collector-sdk.md §7 — a config-driven collector's running `Version()` is derived from `metadata.version`). Not incremented for a comment or reordering with no output change. |

## 3. `spec` field reference

### 3.1 `spec.engine`

| Key | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `spec.engine` | string enum | yes | — | Which extraction engine interprets the rest of `spec`. MVP values: `html_selectors`, `text_regex`. Roadmap values (rejected by the MVP loader, reserved in the schema): `json_path`, `rss_atom`, `xml_xpath`, `pdf_text`, `github_releases`. See §4 for what each engine does and its limits. |

### 3.2 `spec.product_match`

| Key | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `spec.product_match.product` | string | one of `product`/`family` required | — | Product slug this source's candidates are matched against by default. Feeds `Applicability.ProductHint` (collector-sdk.md §3.8) — a hint the validation layer resolves, not a guaranteed final identity. |
| `spec.product_match.family` | string | one of `product`/`family` required | — | Product-family slug, for a source covering more than one product under one family. |

### 3.3 `spec.fetch`

| Key | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `spec.fetch.expected_content_type` | string | no | none (any accepted) | Content-Type the response must match (prefix match, e.g. `text/html` matches `text/html; charset=utf-8`). A mismatch produces `FetchOutcome = failed` before any parsing is attempted — a cheap defence against a source silently changing shape. |
| `spec.fetch.max_bytes` | integer | no | `2097152` (2 MiB) | Hard cap on response body size. The SDK wrapper enforces this while streaming the response (collector-sdk.md §4); exceeding it produces `FetchOutcome = too_large`, not a truncated artifact. |
| `spec.fetch.timeout_seconds` | integer | no | `30` | Per-request timeout for this source's fetch. |
| `spec.fetch.headers` | map[string]string | no | `{}` | Additional static request headers (e.g. a required `Accept` value). Must not include `Authorization` or any credential — config-driven collectors are not permitted to carry secrets (§9). |
| `spec.fetch.conditional` | string enum | no | `auto` | `auto` uses ETag when present, falling back to Last-Modified, falling back to none; `etag_only` or `last_modified_only` force one mechanism (useful when a source sends both but only one is reliable); `none` disables conditional requests for a source known not to support them, falling back immediately to normalised-section hashing (§3.4) as the change signal. |

### 3.4 `spec.normalize`

| Key | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `spec.normalize.section_selector` | CSS selector string | `html_selectors` only, optional | whole document | Restricts what is hashed for change detection to this section, so volatile content elsewhere on the page (ads, per-visit dynamic attributes, unrelated sections) does not produce false `changed` outcomes. Strongly recommended whenever a source lacks a reliable conditional-request signal (see MikroTik changelogs, §9.1). |
| `spec.normalize.strip` | list of CSS selectors | no | `[]` | Elements removed before hashing and before extraction — `script`, `style`, timestamps unrelated to release data, framework scaffolding attributes. Applied inside `section_selector`'s scope when both are set. |
| `spec.normalize.replace` | list of `{pattern, with}` | no | `[]` | Regex substitutions applied to the normalized text before hashing (e.g. collapsing whitespace, stripping a per-request nonce embedded in markup). `pattern` is a bounded regex (§9); `with` is a literal replacement string, no back-reference expansion beyond `$1`-style numbered groups. |

### 3.5 `spec.extract`

| Key | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `spec.extract.release_container` | selector/pattern string | yes | — | Identifies each repeating unit that becomes one `CandidateRelease`. For `html_selectors`, a CSS selector where each match is one container; for `text_regex`, a named-group-bearing regex where each match is one container. |
| `spec.extract.fields` | map[string]FieldSpec | yes | — | One entry per `CandidateRelease` field to populate. See §3.6 for `FieldSpec`. Unlisted fields take their type's zero value (`""`, `PrecisionUnknown`, etc.) — a config is never required to populate a field it has no data for, but `version` (mapping to `RawVersion`) is required to be present, per §5 validation. |
| `spec.extract.release_type` | string | yes | — | Fixed `ReleaseType` value for every candidate this config produces (e.g. `embedded_os`). Config-driven collectors do not infer `release_type` per-candidate in the MVP — a source that genuinely mixes release types needs either separate configs (one per type) or a code collector. |
| `spec.extract.channel` | string | no | `""` | Fixed `Channel` value, used when a source has no per-candidate channel field to extract (contrast with a `fields.channel` entry, which extracts a *varying* channel per candidate — set at most one of the two). |
| `spec.extract.applicability` | ApplicabilityOverride | no | derived from `product_match` | Static overrides merged into each candidate's `Applicability` (e.g. a fixed `region` or `deployment_mode` for a source that only ever covers one variant). |
| `spec.extract.evidence_excerpt` | selector/pattern string | no | same as `release_container` | What text becomes `Evidence.Excerpt` for each candidate. Truncated to a bounded length by the engine (see licensing.md §"third-party content") regardless of what this selector matches — a config cannot opt out of the excerpt length bound. |

### 3.6 `FieldSpec` (entries under `spec.extract.fields`)

| Key | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `selector` | selector/pattern string | yes | — | Where to find this field's raw value, scoped inside the matched `release_container`. `:scope` refers to the container element itself (its own text/attributes), for engines that support it. |
| `attribute` | string | no | element text content | For `html_selectors`: read an HTML attribute (e.g. `href`, `data-version`) instead of the element's text. |
| `regex` | string | no | none (use full matched text) | A bounded regex (§9) applied to the text `selector` produced; if it has a capture group, group 1 is used, otherwise the whole match. Used both to extract from free text and, for `text_regex`, in combination with `release_container`'s own capture groups. |
| `transform` | string or list of strings | no | `[]` (no transform) | One or an ordered pipeline of transforms from the vocabulary in §5. Applied in list order. |
| `map` | map[string]string | no | none | Exact-match lookup translating a raw value (after `transform`) to FirmScout's constrained vocabulary (e.g. channel labels). A value with no entry in `map` is a **hard extraction error** for that candidate, not a silent pass-through — an unmapped label must fail loudly (developing-collectors.md), routing that candidate to a failed-extraction state rather than inventing a channel. |
| `date_format` | Go time-layout string | `release_date` field only | none | The reference-time layout (`"2006-01-02"`, `"January 2006"`, …) used to parse the field's text into a date, after `regex`/`transform`. Required whenever `precision` is `exact_day` or `month_only`; irrelevant for `year_only` (a bare year is parsed directly) and `unknown`. |
| `precision` | `exact_day` \| `month_only` \| `year_only` \| `unknown` | `release_date` field only, yes | — | The `PartialDate` precision this field is declared to produce. See §6 — this is the field the whole date-precision discipline hinges on. |

## 4. Engines

### 4.1 `html_selectors` (MVP)

**Input:** the fetched artifact's body, parsed as HTML (via `goquery`/`golang.org/x/net/html`).
**Selector language:** CSS selectors (the same subset `goquery` supports — no XPath, no jQuery-specific pseudo-selectors beyond what `goquery` implements). `:scope` is supported for container-relative field selectors.
**Limitations:**
- No JavaScript execution. A source that only renders its release data client-side (React/Vue hydration with an empty server-rendered shell) cannot be handled by this engine at all — it needs either a vendor-provided API/feed instead, or is out of scope until a headless-rendering capability is deliberately added and reviewed for its cost and security implications (not currently planned).
- Selector matching against malformed HTML follows the parser's error-recovery behaviour, not a guarantee of matching author intent — a source with severely broken markup may need `normalize.strip`/`replace` preprocessing to get reliable matches.
- No support for selecting across iframe boundaries or shadow DOM (both require a real browser engine).

### 4.2 `text_regex` (MVP)

**Input:** the fetched artifact's body, treated as plain text (works equally on genuinely plain-text sources and as a fallback against HTML the config chooses to treat as text — e.g. an inline `<pre>` block already extracted by `normalize.section_selector`).
**Selector language:** Go's `regexp` (RE2 syntax) with named capture groups for `release_container`, and additional regexes for individual fields.
**Limitations:**
- RE2 has no backtracking, which is a safety property (bounded worst-case matching time, §9) but also means some patterns expressible in PCRE (backreferences, lookahead/lookbehind) are not available — this is a deliberate trade against the correctness a more expressive but unbounded engine could offer.
- No structural awareness: a `text_regex` config over an HTML source sees markup as literal text unless `normalize` strips it first, so it is a poor fit for anything with meaningful nesting — that is what `html_selectors` is for.

### 4.3 Roadmap engines (not implemented in the MVP)

| Engine | Input | Selector language | Known limitations to design around |
| --- | --- | --- | --- |
| `json_path` | Parsed JSON body | JSONPath (or a bounded subset) | JSONPath implementations vary in expressiveness/safety; the eventual engine picks one bounded subset and documents it rather than exposing a full scripting-capable variant. |
| `rss_atom` | Parsed RSS 2.0 / Atom feed | Fixed field mapping (title, link, pubDate, description) plus optional `regex` post-processing on those fields | Feed metadata is usually coarser than a dedicated changelog page (often no channel, sometimes no structured version field) — expect lower-confidence candidates and more validation-gate 8 review routing than `html_selectors`. |
| `xml_xpath` | Parsed XML body | XPath 1.0 subset | XPath's full expressiveness (functions, unbounded axis traversal) is a similar risk surface to unbounded regex; the engine bounds evaluation cost the same way `text_regex` bounds regex cost. |
| `pdf_text` | Extracted text layer of a PDF | Same as `text_regex`, applied to extracted text | PDF text extraction quality varies wildly (scanned images with no text layer, multi-column layouts producing scrambled reading order); this engine is the intended path for Poly/HP release notes but needs its own accuracy review before being trusted for auto-publication-eligible sources. |
| `github_releases` | GitHub Releases API response | Fixed field mapping (tag_name, published_at, body, prerelease) plus optional `regex`/`transform` on those fields | Tied to GitHub API rate limits and to whether a vendor's releases are actually tagged consistently; a reasonable fit for vendor-maintained-repository-class sources (DATA_SOURCES.md quality classes). |

Each roadmap engine, when built, gets its own `spec.engine` value, its own section in this document, and its own fixture corpus in the engine's own test suite (distinct from any individual vendor config's fixtures) before any vendor config is allowed to declare it.

## 5. Transform vocabulary

Transforms are applied in the order listed under `FieldSpec.transform`. Each is a pure string-to-string (or string-to-list, for `map`) function with no side effects and no unbounded cost:

| Transform | Behaviour |
| --- | --- |
| `trim` | Removes leading/trailing Unicode whitespace. |
| `lowercase` | Unicode-aware lowercasing. |
| `uppercase` | Unicode-aware uppercasing. |
| `regex_extract` | Applies a bounded regex (from the transform's own `pattern` argument) and keeps capture group 1, or the whole match if there is no group. Distinct from `FieldSpec.regex`: this form lives inside a transform pipeline, so it can run after `trim`/`lowercase` rather than only before. |
| `regex_replace` | Applies a bounded regex substitution (`pattern`, `with` arguments), same substitution rules as `spec.normalize.replace` (§3.4). |
| `strip_prefix` | Removes a literal prefix string (argument), no-op if the value does not start with it. |
| `strip_suffix` | Removes a literal suffix string (argument), no-op if the value does not end with it. |
| `map` | Exact-match dictionary lookup (see `FieldSpec.map`, §3.6); an unmapped input is a hard error, not a pass-through. |
| `parse_date` | Parses the value according to the field's `date_format` and `precision` (§3.6, §6); only valid on `release_date`. |

No transform executes arbitrary code, reads a file, or makes a network call — the vocabulary is closed, and adding to it means adding a new named transform to the engine (reviewed, tested, documented here), never an escape hatch that accepts arbitrary expressions.

## 6. Date precision handling

This is the field-level rule that makes blueprint's "no invented precision" principle enforceable at the config level, not just at the domain-type level (collector-sdk.md §3.7):

**The rule:** `spec.extract.fields.release_date.precision` must match exactly what the paired `regex`/`date_format` can actually determine from the source text — never more. A config whose regex only ever captures a month and a year (e.g. `"(January|February|...|December) (\d{4})"`) **must** declare `precision: month_only`. It is a configuration error — caught by schema validation plus the engine's own runtime check — to pair a month-only capture with `precision: exact_day` and let the parser default the day to `1`, because that produces a `PartialDate` claiming a precision the source never supported, which is exactly the failure mode the domain type (`PartialDate`, collector-sdk.md §3.7) is designed to make structurally impossible upstream of the config layer.

**Practical guidance:**

- If a source sometimes gives a full date and sometimes only a month (inconsistent formatting across entries), do not write one config that assumes the best case. Either write a `regex` that only matches the well-formed, day-precise case and let entries that do not match fall through to a lower-confidence handling path (a second field attempt with `month_only`, chained via multiple field definitions is **not** supported in the MVP schema — this is a signal that inconsistent date formatting across entries may need a code collector, §"choosing between a configuration-driven collector and a code collector" in collector-sdk.md), or conservatively declare `month_only` for the whole config if that is the *worst case* the regex can guarantee.
- `year_only` is for sources that only ever state a year (rare, but seen in some legacy vendor changelogs) — `date_format` is not required in this case; a bare four-digit year is parsed directly.
- `unknown` is for a field that is present in `spec.extract.fields` for documentation purposes but whose source text is not date-like enough to parse reliably; a candidate with `precision: unknown` still publishes (if it clears the other validation gates) but its `releaseDate` is omitted entirely in every rendering (api.md §"date rendering discipline"), never approximated.
- The engine's runtime check (not just schema validation) verifies, at config load time, that the `date_format` layout string is internally consistent with the declared `precision` — a `date_format` containing a day-of-month directive (`02`) paired with `precision: month_only` is rejected at load time, before any source is ever fetched with this config.

## 7. Validation

**JSON Schema location:** `packages/schemas/collector-config.schema.json`. It is the machine-checkable expression of every table in §2–§3.6, versioned to track `apiVersion` (§1). It is not yet checked into the repository as of this document — the schema itself is scaffolded and populated during Phase 1 (mvp-roadmap.md) alongside the first real config, so the schema's shape is validated against a real worked example rather than designed speculatively ahead of any config it has to accept.

**What CI checks**, once the schema and the `schemas.yml` workflow referenced in the repository skeleton exist:

1. Every file under `collectors/config/**/*.yaml` validates against the schema version matching its declared `apiVersion`.
2. `metadata.vendor` and `spec.product_match.product`/`family` reference an entry that actually exists under `dataset/vendors/` and `dataset/products/` respectively (a cross-file check the JSON Schema alone cannot express, run as a separate registry-consistency check rather than folded into schema validation).
3. Every `regex` and `pattern` field is checked against the bounded-regex rule (§9) — rejecting constructs known to enable catastrophic backtracking under RE2-incompatible engines, and rejecting excessive quantifier nesting generally, as a defence-in-depth measure even where RE2 itself already bounds worst-case cost.
4. Every config with at least one fixture pair (collector-sdk.md §8) has its fixtures re-run and diffed against `expected.json` — a config change that is not accompanied by a fixture update, or that breaks an existing fixture, fails CI.
5. The date-precision consistency check from §6 runs as part of config load, exercised in CI the same way it runs in production.

**The `firmscout collector test` workflow** (CLI subcommand, `apps/cli`) is the local-development equivalent of checks 1, 3, and 4, runnable against one config file without waiting for CI: `firmscout collector test collectors/config/mikrotik/changelogs.yaml` loads the config, validates it against the schema, and runs every fixture found in the corresponding `testdata/fixtures/<vendor>/<source>/` directory, printing a diff for any mismatch. It is the command a contributor runs before opening a pull request, and the one CI runs (across every config) to produce check 4 above.

## 8. Two complete worked examples

### 8.1 MikroTik changelog — `html_selectors` (exactly as given in blueprint §14)

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

Why each choice is made is walked through in [`docs/collectors/developing-collectors.md`](../collectors/developing-collectors.md#worked-example-the-mikrotik-changelog-collector") — the short version: `normalize.section_selector` avoids hashing the ~400 KB page's volatile Livewire/Alpine attributes so unrelated page churn does not trigger needless extraction; `channel.map` fails loudly on an unrecognised badge label instead of guessing; `release_date.precision: exact_day` is set because the page genuinely publishes full `YYYY-MM-DD` dates, matched exactly by the regex, per §6's rule.

### 8.2 MikroTik plain-text version endpoint — `text_regex`

Measured 2026-09-03: `GET https://upgrade.mikrotik.com/routeros/NEWESTa7.stable` returns `Content-Type: text/plain`, a body of exactly `7.24.2 1788429434` (version, a single space, a Unix epoch timestamp — the file's *last-modified-content* timestamp, not necessarily the release date itself, hence the conservative precision choice below), and both an `ETag` and a `Last-Modified` response header, making it the cheapest possible watcher: a conditional GET against a few bytes.

```yaml
apiVersion: firmscout.dev/v1alpha1
kind: CollectorConfig
metadata:
  id: mikrotik.newest-stable
  vendor: mikrotik
  version: 1
spec:
  engine: text_regex
  product_match:
    product: mikrotik-routeros
  fetch:
    expected_content_type: text/plain
    max_bytes: 4096                # this endpoint's body is a handful of bytes; 4 KiB is generous headroom, not an expectation
    conditional: auto              # both ETag and Last-Modified are sent; auto prefers ETag
  extract:
    # The whole body is one "container": a version token, one space, an
    # epoch integer. Named groups map directly onto fields below.
    release_container: "(?P<version>\\S+)\\s+(?P<epoch>\\d+)"
    fields:
      version:
        selector: ":scope"
        regex: "(?P<version>\\S+)\\s+\\d+"
        transform: trim
      release_date:
        selector: ":scope"
        regex: "\\S+\\s+(?P<epoch>\\d+)"
        transform: [regex_extract]
        # The epoch marks when this pointer file last changed, which is a
        # reliable upper bound on when the release became current but is
        # NOT documented by MikroTik as the release's publication date --
        # the changelog page (§8.1) is the source of truth for release_date
        # at exact_day precision. This endpoint intentionally declares a
        # coarser precision rather than presenting a technically-derived
        # timestamp as if it were the vendor's stated release date.
        date_format: "epoch_seconds"
        # No precision, because there is no release_date field. See below.
    release_type: embedded_os
    channel: stable                # this endpoint is documented by MikroTik as the "stable" channel pointer specifically
    evidence_excerpt: ":scope"
```

This example earns its place for a reason beyond MikroTik specifically: **the cheapest, most reliable change signal is often not evidence for anything else the response happens to contain.**

The endpoint's body is `7.24.2 1788429434`. The second token is an epoch, precise to the second, and it is tempting to publish it as an exact-day release date. MikroTik does not document what it means. Comparing it against the same response's `Last-Modified` header (measured 2026-09-03) puts the two within hours of each other, which is consistent with the epoch being the moment the pointer file last changed — close to, but not the same thing as, the date the release was published.

An earlier draft of this document argued for `year_only` here, on the grounds that day precision overstated what the source asserted. That reasoning was half right and reached the wrong conclusion: `year_only` is the *same inference*, merely coarser. It still claims the source told us a year, and it did not. Degrading precision does not launder an unsupported inference into a supported one.

**So this config extracts no release date at all.** The candidate carries `unknown` precision, and the raw epoch survives verbatim in the evidence excerpt, where a reviewer can see it and a later, deliberate decision could promote it. Nothing is lost, because the changelog collector (§8.1) reads MikroTik's own published `YYYY-MM-DD` dates, so the product is dated by the source that actually states a date. Where both collectors produce a candidate for the same version, gate 10 reconciles them (blueprint §16).

The rule this generalises to: **a field is extractable only when the source says what it means.** Numeric precision is not semantic precision, and a value's being present in a response is not the same as the vendor asserting it.

## 9. Security rules a config must obey

A collector config is data submitted by a contributor and, in the community-contribution model, potentially reviewed by a maintainer who trusts the schema and the engine more than they hand-audit every regex. The rules that keep that trust model sound:

- **No arbitrary code.** A config is a declarative document interpreted by a fixed engine (§4). There is no `eval`, no scripting field, no way to reference an external script or binary. The transform vocabulary (§5) is closed and additions go through engine review, never through a config-level escape hatch.
- **No network access beyond the declared source URL.** The engine fetches exactly `spec.fetch` (implicitly, the source's registered URL) and nothing else — no field in `spec.extract` can cause a follow-up request. A source needing more than one request (pagination, a join across pages) is by definition outside what a config-driven collector can express (collector-sdk.md §10) and needs a code collector instead, where such requests are visible in reviewed Go code rather than hidden in YAML.
- **Bounded regex.** Every `regex` and `pattern` field is RE2-syntax (Go's `regexp`), which has no backtracking and therefore no catastrophic-backtracking denial-of-service class at all — this is why `text_regex` and every `regex`/`transform` argument across engines standardise on RE2 rather than a PCRE-compatible engine, even though RE2 is less expressive. CI additionally rejects excessively nested quantifiers as defence in depth (§7, check 3), and the SDK wrapper's extraction wall-clock limit (collector-sdk.md §4) is the last line of defence regardless.
- **Bounded selectors.** CSS selectors and (future) XPath expressions are evaluated by libraries with bounded per-document cost proportional to document size, not to selector complexity in a way that could be adversarially amplified; `max_bytes` (§3.3) bounds document size itself, which bounds worst-case selector evaluation cost as a direct consequence.
- **No secrets.** `spec.fetch.headers` may not contain an `Authorization` header or any credential-shaped value — a source needing authentication is, per DATA_SOURCES.md, either out of scope for automated collection entirely or (rarely, and only after explicit compliance review) handled by a code collector where credential handling is visible, reviewed Go code using the platform's secret-management path, never a YAML file that could be merged with a bearer token pasted into it.
