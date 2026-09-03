# API design

> Expands [blueprint §12](blueprint.md#12-api-design). The machine-readable contract is [`docs/api/openapi.yaml`](../api/openapi.yaml); this document is the human-readable design behind it and the source of truth when the two could be read as disagreeing (§"OpenAPI as the machine-readable contract" resolves that case). Commercial mechanics — plans, key lifecycle, billing events — are out of scope here; see `api-commercial.md`.

## 1. Design principles

- **Versioned path prefix.** Every endpoint lives under `/api/v1`. A breaking change ships as `/api/v2` alongside the still-running `v1`, never as a silent behaviour change under the same prefix (§9).
- **Stable slugs as identifiers.** Vendors and products are addressed by slug (`mikrotik`, `mikrotik-routeros`), not database surrogate keys — slugs are immutable once created (blueprint §7.6) precisely so they are safe to use as public URLs and as API identifiers that a consumer can hardcode. Releases and advisories, which have no natural human-readable name, are addressed by opaque ULIDs (`rel_01J...`).
- **Cursor pagination**, never offset pagination, on every list endpoint. Offset pagination breaks silently under concurrent inserts into an append-only table (blueprint §11 rule 2) — a page requested with `offset=100` after ten new releases have landed does not mean what the consumer thinks it means. See §5.
- **RFC 9457 `application/problem+json`** for every error response, without exception, including validation errors and rate limiting. See §7 for the catalogue.
- **`ETag` and `Cache-Control`** on every cacheable response. A `Release` is immutable once published, so `GET /releases/{id}` is cacheable indefinitely (`Cache-Control: public, max-age=31536000, immutable`); a `latest` or list endpoint is cacheable but invalidated on publish (§"cache policy" per endpoint, §3).
- **`X-Request-Id`** is generated if absent and echoed on every response, success or error, so a support conversation or a log correlation can reference one identifier end to end. It is also the trace-correlatable identifier carried into OpenTelemetry spans (see `observability.md`).
- **Rate-limit headers on every response**, not only on 429s — a consumer should be able to see how close to a limit they are before hitting it. See §8.
- **No field hidden from the free tier that a paid tier's response also contains for a given endpoint.** The paywall is on which *endpoints* are reachable and on *volume* (quota) and *history depth*, never on stripping fields out of a shared response shape (blueprint §5). Where a field's value genuinely differs by tier (e.g. `releases` history window), that is documented per endpoint, not hidden as an undocumented field omission.

## 2. Complete endpoint reference

| Method & path | Purpose | Auth | Parameters | Response shape | Status codes | Cache policy | Quota weight | Tiers |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `GET /api/v1/search` | Full-text and alias search over products and vendors | optional key | `q` (required, 1–200 chars), `limit` (default 20, max 50), `cursor` | `SearchResult` list | 200, 400, 429 | `Cache-Control: public, max-age=60`, keyed by normalised `q` | 1 | all |
| `GET /api/v1/vendors` | List all vendors | optional key | `limit` (default 50, max 200), `cursor` | `Vendor` list | 200, 429 | `Cache-Control: public, max-age=3600` | 1 | all |
| `GET /api/v1/vendors/{slug}` | One vendor's detail | optional key | path `slug` | `Vendor` | 200, 404, 429 | `Cache-Control: public, max-age=3600`, `ETag`; invalidated when any of the vendor's products publish | 1 | all |
| `GET /api/v1/products/{slug}` | One product's detail, including `latestRelease` summary | optional key | path `slug` | `Product` | 200, 404, 429 | `Cache-Control: public, max-age=3600`, `ETag`; invalidated on publish for this product | 1 | all |
| `GET /api/v1/products/{slug}/releases` | Release history for a product | optional key | path `slug`; `channel`, `releaseType` (filters); `sort` (`releaseDate` \| `firstObservedAt`, default `releaseDate`); `order` (`asc` \| `desc`, default `desc`); `limit` (default 20, max 100), `cursor` | `Release` list, cursor-paginated | 200, 404, 429 | `Cache-Control: public, max-age=300`; anonymous/free responses windowed to the most recent 12 months regardless of `limit`/`cursor` reach, Professional+ unwindowed | 1 (list) | history window: all; full history: Professional+ |
| `GET /api/v1/products/{slug}/latest` | The single latest-observed release for a product | optional key | path `slug`; `channel` (optional filter, e.g. request the latest `long_term` specifically) | `LatestReleaseResponse` | 200, 404, 429 | `Cache-Control: public, max-age=300`, `ETag`; invalidated on publish for this product | 1 | all |
| `GET /api/v1/products/{slug}/advisories` | Security advisories affecting a product | optional key | path `slug`; `limit`, `cursor` | `Advisory` list | 200, 404, 429 | `Cache-Control: public, max-age=600` | 1 (list) | list: all; structured `affectedRanges`/`evidence` detail: Professional+ (free/anonymous responses include title, link, and severity only) |
| `GET /api/v1/releases/{id}` | One immutable release by id | optional key | path `id` | `Release` | 200, 404, 429 | `Cache-Control: public, max-age=31536000, immutable`, `ETag` | 1 | all |
| `POST /api/v1/lookup` | Bulk lookup, many product slugs per call | key required | JSON body `{ "slugs": [...] }`, max 100 slugs per call | map of slug → `LatestReleaseResponse` or `null` | 200, 400, 401, 403, 429 | `Cache-Control: no-store` (per-consumer quota-relevant call, not cached) | N (one per slug resolved, minimum 1) | Professional+ |
| `GET /api/v1/usage` | The calling key's own current usage and quota | key required | none | `UsageSummary` | 200, 401, 429 | `Cache-Control: no-store` | 0 (usage queries never consume the quota they report on) | keyed (any tier with a key) |
| `GET /healthz` | Liveness probe | none | none | `{"status":"ok"}` | 200, 503 | `Cache-Control: no-store` | 0 | n/a (infrastructure endpoint, not `/api/v1`) |

Every endpoint above except `POST /api/v1/lookup` and `GET /api/v1/usage` accepts anonymous requests, per the design principle that the public website is a client of this same API (blueprint §12) — anonymous requests simply carry the lowest, most tightly rate-limited quota tier (§8) and cannot reach `/lookup`.

## 3. JSON response schemas

All timestamps are RFC 3339 UTC (`2026-09-03T18:30:00Z`). All identifiers are strings; release/advisory ids use a `rel_`/`adv_` prefix convention to make an id's type visually unambiguous in logs and support conversations.

### 3.1 `Vendor`

```json
{
  "slug": "mikrotik",
  "name": "MikroTik",
  "website": "https://mikrotik.com",
  "productCount": 1,
  "lastVerifiedAt": "2026-09-03T18:30:00Z"
}
```

### 3.2 `Product`

```json
{
  "slug": "mikrotik-routeros",
  "name": "RouterOS",
  "vendor": { "slug": "mikrotik", "name": "MikroTik" },
  "family": null,
  "category": "network_device_os",
  "releaseType": "embedded_os",
  "latestRelease": {
    "id": "rel_01J8Z3K9QWERTYUIOPASDFGH",
    "rawVersion": "7.24.2",
    "channel": "stable",
    "releaseDate": "2026-09-02",
    "releaseDatePrecision": "exact_day"
  },
  "officialSources": [
    { "url": "https://mikrotik.com/download/changelogs", "kind": "html", "official": true }
  ],
  "lastVerifiedAt": "2026-09-03T18:30:00Z"
}
```

`family` is `null` for a product not part of a multi-product family (e.g. RouterOS today is registered standalone); populated as `{ "slug": ..., "name": ... }` when applicable. `latestRelease` here is a summary, not the full `Release` object — the full object is fetched via `/products/{slug}/latest` or `/releases/{id}`.

### 3.3 `Release`

```json
{
  "id": "rel_01J8Z3K9QWERTYUIOPASDFGH",
  "product": { "slug": "mikrotik-routeros", "name": "RouterOS" },
  "rawVersion": "7.24.2",
  "normalizedVersion": "7.24.2",
  "releaseType": "embedded_os",
  "channel": "stable",
  "releaseDate": "2026-09-02",
  "releaseDatePrecision": "exact_day",
  "recommended": null,
  "withdrawn": false,
  "correctsReleaseId": null,
  "source": {
    "official": true,
    "url": "https://mikrotik.com/download/changelogs",
    "kind": "html"
  },
  "evidence": {
    "retrievedAt": "2026-09-03T12:00:00Z",
    "excerpt": "7.24.2 (2026-09-02) — stable"
  },
  "firstObservedAt": "2026-09-03T12:00:00Z",
  "lastVerifiedAt": "2026-09-03T18:30:00Z"
}
```

`recommended` is `null` unless the vendor explicitly designated this version as recommended (blueprint §4.3) — it is never inferred by FirmScout picking the newest. `withdrawn` and `correctsReleaseId` express the immutable-history model (blueprint §16): a withdrawal or correction is a fact about this row, never a mutation of a prior one.

### 3.4 `LatestReleaseResponse`

```json
{
  "vendor": { "slug": "mikrotik", "name": "MikroTik" },
  "product": { "slug": "mikrotik-routeros", "name": "RouterOS" },
  "latestRelease": { "...": "a full Release object, §3.3" }
}
```

Matches the example already given in blueprint §12 exactly; reproduced here as part of the formal schema rather than restated informally.

### 3.5 `Advisory`

```json
{
  "id": "adv_01J8Z4M2NBVCXZASDFGHJKLQ",
  "title": "RouterOS DNS resolver denial-of-service",
  "severity": "medium",
  "link": "https://mikrotik.com/download/changelogs",
  "publishedDate": "2026-08-15",
  "publishedDatePrecision": "exact_day",
  "affectedRanges": [
    { "product": "mikrotik-routeros", "introducedIn": "7.20.0", "fixedIn": "7.24.0" }
  ],
  "cves": ["CVE-2026-XXXXX"],
  "evidence": {
    "sourceUrl": "https://mikrotik.com/download/changelogs",
    "retrievedAt": "2026-08-16T09:00:00Z"
  }
}
```

`affectedRanges`, `cves`, and `evidence` are present only for Professional+ responses; free/anonymous responses include `id`, `title`, `severity`, `link`, `publishedDate`, and `publishedDatePrecision` only, per §2's per-endpoint tier note. `affectedRanges` uses raw version strings, never a numeric comparison — "introduced in / fixed in" is exactly what the vendor stated, not a computed range (blueprint §3.5). Advisory data is designed, not built, in the MVP (blueprint §1) — this schema documents the intended shape for when CVE correlation ships (mvp-roadmap.md Phase 4), not a currently populated endpoint.

### 3.6 `SearchResult`

```json
{
  "results": [
    {
      "type": "product",
      "slug": "mikrotik-routeros",
      "name": "RouterOS",
      "vendor": { "slug": "mikrotik", "name": "MikroTik" },
      "matchedOn": "name"
    },
    {
      "type": "vendor",
      "slug": "mikrotik",
      "name": "MikroTik",
      "matchedOn": "name"
    }
  ],
  "pagination": { "nextCursor": null }
}
```

`matchedOn` names which field (`name`, `alias`, `vendor`) the trigram/`tsvector` match hit, so a UI can show "matched alias RB4011" style context.

### 3.7 Error (`application/problem+json`)

```json
{
  "type": "https://firmscout.dev/problems/not-found",
  "title": "Resource not found",
  "status": 404,
  "detail": "No product with slug 'mikrotik-routrs' was found.",
  "instance": "/api/v1/products/mikrotik-routrs",
  "requestId": "req_01J8Z5N3PQRSTUVWXYZABCDE"
}
```

Every field of RFC 9457 is populated; `requestId` is a FirmScout extension member equal to the `X-Request-Id` header value, so a consumer reading only the body (not headers) still has the correlation id. See §7 for the full catalogue of `type` values.

## 4. Date rendering discipline

`releaseDate` (and `publishedDate` on advisories) is rendered according to `releaseDatePrecision`, which is **always present** regardless of whether `releaseDate` itself is:

| Precision | `releaseDate` rendering | Example |
| --- | --- | --- |
| `exact_day` | `YYYY-MM-DD` | `"2026-09-02"` |
| `month_only` | `YYYY-MM` | `"2026-09"` |
| `year_only` | `YYYY` | `"2026"` |
| `unknown` | field omitted entirely from the JSON object | (no `releaseDate` key at all) |

**Why this prevents a consumer from parsing a fake day:** a naive API design would always emit a full `YYYY-MM-DD` string, defaulting an unknown day to `01`, because that is convenient for consumers who want to `Date.parse()` the value without branching. That convenience is exactly the harm the domain model (`PartialDate`, collector-sdk.md §3.7) exists to prevent: a consumer's compliance report or patch-scheduling tool would silently treat "sometime in September 2026" as "September 1st, 2026", a fabricated fact with no basis in what the vendor actually published. By omitting the day (and, at coarser precision, the month) from the rendered string entirely rather than zero-filling it, and by making `releaseDatePrecision` a field a consumer cannot ignore without their date-parsing code visibly breaking on `"2026-09"` or a missing key, the API forces the same precision discipline onto every consumer that the domain model forces onto FirmScout's own storage. A consumer that wants a single sortable value despite this is expected to construct one deliberately from `releaseDate` + `releaseDatePrecision` themselves — an API that did that silently would be re-inventing the exact bug this design prevents.

This same rule is what `collector-sdk.md §8`'s fixture `expected.json` format mirrors, so the discipline is internalised once, consistently, from extraction through to the wire format.

## 5. Pagination, filtering, and sorting semantics

- **Cursor pagination.** Every list response includes `"pagination": { "nextCursor": "<opaque token>" | null }`. `nextCursor` is passed back as the `cursor` query parameter to fetch the next page; a `null` cursor means the list is exhausted. Cursors are opaque, implementation-defined tokens (in practice, an encoded `(sort_key, id)` tuple) — a consumer must not attempt to construct or decode one, and a cursor is not guaranteed stable across an API version boundary.
- **Filtering.** Filter parameters (`channel`, `releaseType` on `/releases`) are exact-match against the constrained vocabularies (blueprint §4.4) — there is no free-text filter expression language, and an unrecognised filter value is a `400` with the `invalid-parameter` problem type (§7), not a silently-empty result set.
- **Sorting — the explicit rule that releases are never sorted by version string.** `/products/{slug}/releases` accepts `sort=releaseDate` (default) or `sort=firstObservedAt`; there is **no `sort=version`** option, and this is intentional, not an oversight to be added later. Blueprint §3.5 and ADR-0017 establish that version strings are opaque, non-comparable tokens — `3.003.0015.001` and `7.24.2` have no defined ordering relative to each other outside a plausibility check, and sorting by version string would silently produce a wrong, misleading order for any vendor whose scheme is not a simple dotted-integer sequence (which is most of the brief's own worked examples). "Latest" is a derived fact from release date, first-observed timestamp, and channel (blueprint §3.5), never from string comparison, and the API's sort options reflect exactly that and nothing else.

## 6. Error catalogue

Every `type` is a stable URI under `https://firmscout.dev/problems/` (resolving, eventually, to a human-readable explanation page — not required to be a live URL for the error to be valid per RFC 9457, but intended to be one).

| `type` | `title` | `status` | When it occurs |
| --- | --- | --- | --- |
| `.../problems/not-found` | Resource not found | 404 | A vendor/product/release/advisory slug or id does not resolve to an existing record. |
| `.../problems/invalid-parameter` | Invalid parameter | 400 | A query or path parameter fails validation (out-of-range `limit`, unrecognised `channel`/`releaseType` filter value, malformed `cursor`, empty `q`). |
| `.../problems/validation-failed` | Request body validation failed | 400 | `POST /lookup`'s body is malformed, missing `slugs`, or exceeds the 100-slug limit. |
| `.../problems/unauthorized` | Authentication required | 401 | An endpoint requiring a key (`/lookup`, `/usage`) received no key or a malformed `Authorization` header. |
| `.../problems/invalid-api-key` | API key invalid or revoked | 401 | A syntactically well-formed key does not match a live, non-revoked `api_keys` row. |
| `.../problems/forbidden` | Endpoint not available on this plan | 403 | An authenticated but insufficiently-tiered key calls an endpoint (or requests a response feature, e.g. unwindowed `/releases` history) gated above its tier. |
| `.../problems/rate-limited` | Too many requests | 429 | The in-process token bucket for this consumer/IP is exhausted; see §8. |
| `.../problems/quota-exceeded` | Monthly quota exceeded | 429 | The durable PostgreSQL quota counter for this API key's billing period is exhausted; distinct from `rate-limited` (short-window) even though both return 429 — the `type` tells a consumer which one to look at. |
| `.../problems/internal-error` | Internal server error | 500 | An unhandled server-side failure; `detail` is deliberately generic (never leaks internals), `requestId` is the correlation handle for support. |
| `.../problems/service-unavailable` | Service temporarily unavailable | 503 | Health/readiness gate failing (e.g. database unreachable) — returned by `/healthz`/`/readyz` and, transiently, by any endpoint if a downstream dependency is down. |

## 7. Rate limiting, quota, and the 429 response

Every response, success or error, carries:

```
X-RateLimit-Limit: 60
X-RateLimit-Remaining: 47
X-RateLimit-Reset: 1789564800
X-Quota-Limit: 5000
X-Quota-Remaining: 4213
X-Quota-Reset: 1791244800
```

`X-RateLimit-*` reflects the short-window, per-instance token bucket (blueprint §3.9) — approximate under multiple API instances, bounded in the aggregate by the WAF layer. `X-Quota-*` reflects the durable, billing-relevant monthly counter in PostgreSQL, exact by design because it has billing consequences. Anonymous requests carry `X-RateLimit-*` only (no quota concept applies without a key).

A 429, whichever gate produced it, additionally carries:

```
Retry-After: 30
```

seconds until the *rate-limit* bucket (not the monthly quota) is expected to admit a request again; a `quota-exceeded` 429 sets `Retry-After` to the number of seconds until `X-Quota-Reset`, which for a monthly quota is typically a large number — the response body's `detail` field says so explicitly rather than leaving a consumer to infer a month-long retry from a raw header value.

## 8. Versioning and deprecation policy

- **A breaking change** — a removed field, a changed field type or meaning, a changed status code for an existing situation, a changed pagination or auth mechanism — ships as a new path prefix (`/api/v2`), never as an in-place change to `/api/v1`. "Breaking" is judged from the consumer's perspective: adding a new optional field or a new endpoint is not breaking and ships under the existing version.
- **Deprecation, not removal, is the first step** even within a version boundary for something narrower — for example, retiring one filter value while keeping the endpoint. A deprecated field or parameter is announced via the `Deprecation` header (an HTTP date marking when deprecation began) and, where applicable, a `Sunset` header (the date the deprecated behaviour stops being served), both per the relevant IETF drafts this project follows in spirit.
- **Notice period.** A minimum of 90 days between a `Deprecation` header appearing and the corresponding `Sunset` date, published additionally as a dated entry in the API changelog (not specified further here — the changelog's location and format is a documentation-tooling decision, not an architectural one). `/api/v1` as a whole, once `/api/v2` exists, follows the same minimum before v1 is fully retired, communicated well in advance to registered API key holders via the contact address on file.
- **No silent behaviour changes.** A bug fix that changes response content in a way a consumer could reasonably have depended on (not just an internal implementation detail) is treated as a breaking change for versioning purposes, not shipped quietly as a "fix."

## 9. Idempotency for write endpoints

The MVP's endpoint surface (§2) is entirely read-only from the public API's perspective — `POST /api/v1/lookup` is a bulk *read*, not a mutation, so it needs no idempotency key. **Idempotency becomes relevant once write endpoints exist** (a later phase, per mvp-roadmap.md — community correction submissions, webhook registration, inventory upload): those will require an `Idempotency-Key` request header on every mutating request, honoured for a bounded retention window (proposed: 24 hours), such that a retried request with the same key against the same endpoint and the same authenticated consumer returns the original response rather than performing the operation twice. This mirrors the same idempotency-key discipline the `JobQueue` port already applies internally (blueprint §11 rule 8, ADR-0015) — the API-level mechanism when it ships should reuse that pattern's semantics rather than inventing a second one, but it is not implemented in the MVP because there is nothing yet for it to protect.

## 10. OpenAPI as the machine-readable contract

`docs/api/openapi.yaml` is the authoritative, machine-readable contract for the endpoints in §2. It is maintained by hand alongside this document for the MVP — the two are written together, in the same pull request, whenever the API surface changes, rather than one being generated from the other, because the MVP's endpoint count is small enough that generation tooling would add process overhead without reducing real drift risk. As the surface grows past what hand-maintenance can keep consistent, generating the OpenAPI document from Go handler annotations (or generating Go request/response types from the OpenAPI document, the more common direction for contract-first APIs) is the natural next step, and `packages/api-client/`'s planned TypeScript client generation (blueprint §10 repository structure) already assumes `openapi.yaml` as its input regardless of which direction generation eventually runs.

**Validated in CI** (once the `schemas.yml`/API workflow referenced in the repository skeleton is populated) by: parsing the document as YAML, validating it against the OpenAPI 3.1 meta-schema, and running contract tests that assert every handler in `internal/adapters/httpapi` has a corresponding path+method entry in the document with matching request/response shapes for at least the success case — a handler with no OpenAPI entry, or an OpenAPI entry with no handler, fails the build. This is the same "a rule not enforced is a comment" discipline the architecture test (`internal/archtest`) applies to package dependencies (blueprint §7.2), applied to the API contract instead.
