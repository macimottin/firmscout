# API design

> Expands [blueprint §12](blueprint.md#12-api-design). The machine-readable contract is [`docs/api/openapi.yaml`](../api/openapi.yaml); this document is the human-readable design behind it and the source of truth when the two could be read as disagreeing (§"OpenAPI as the machine-readable contract" resolves that case). Commercial mechanics — plans, key lifecycle, billing events — are out of scope here; see `api-commercial.md`.

## Base URL

| Environment | Base URL |
| --- | --- |
| Production | `https://api.firmscout.dev/api/v1` |
| Local development | `http://localhost:8080/api/v1` |

The `/api/v1` prefix is retained on the production host even though `api.` already says
"API". That redundancy is deliberate: the route is then byte-for-byte identical in every
environment, and no layer strips or adds a prefix depending on where it is deployed.
Environment-dependent path rewriting is a reliable source of bugs that appear only in
production.

The reasoning behind the hostname — including why a path on the apex domain was
rejected, and the cookie-isolation argument that decided it — is in
[ADR-0019](../adr/0019-public-domain-shape.md).

## 1. Design principles

- **Versioned path prefix.** Every endpoint lives under `/api/v1`. A breaking change ships as `/api/v2` alongside the still-running `v1`, never as a silent behaviour change under the same prefix (§9).
- **Stable slugs as identifiers.** Vendors and products are addressed by slug (`mikrotik`, `mikrotik-routeros`), not database surrogate keys — slugs are immutable once created (blueprint §7.6) precisely so they are safe to use as public URLs and as API identifiers that a consumer can hardcode. Releases and advisories, which have no natural human-readable name, are addressed by opaque ULIDs (`rel_01J...`).
- **Cursor pagination**, never offset pagination, on every list endpoint. Offset pagination breaks silently under concurrent inserts into an append-only table (blueprint §11 rule 2) — a page requested with `offset=100` after ten new releases have landed does not mean what the consumer thinks it means. See §5.
- **RFC 9457 `application/problem+json`** for every error response, without exception, including validation errors and rate limiting. See §6 for the catalogue.
- **`ETag` and `Cache-Control`** on every cacheable response, with the policy chosen by the *caller* and not only by the route. An anonymous response carries the route's public policy — a `Release` is immutable once published, so `GET /releases/{id}` is cacheable indefinitely (`Cache-Control: public, max-age=31536000, immutable`), while a `latest` or list endpoint is cacheable but invalidated on publish (§"cache policy" per endpoint, §3). **Any response to a request that presented a credential carries `Cache-Control: private, no-store` instead, whatever the route's policy says.** See the `Vary` bullet below for why that is not left to `Vary`, and §2's cache column for the per-endpoint policies as they apply to an anonymous caller.
- **`X-Request-Id`** is generated if absent and echoed on every response, success or error, so a support conversation or a log correlation can reference one identifier end to end. It is also the trace-correlatable identifier carried into OpenTelemetry spans (see `observability.md`).
- **Rate-limit headers on every response**, not only on 429s — a consumer should be able to see how close to a limit they are before hitting it. See §7.
- **No authenticated response is storable, and `Vary` is the second line rather than the first.** The release history endpoint's body depends on the caller's plan (§2): the same URL is twelve months of history for an anonymous caller and the complete archive for a Professional one. The obvious mitigation is `Vary: Authorization, X-API-Key`, and `Vary` alone is not trusted, because its correctness depends on every layer agreeing and they do not always — CloudFront, which this architecture uses, honours `Vary` only for `Accept-Encoding` and ignores every other value. With a URL-only cache key it would hand the paid archive to an anonymous caller, or the empty anonymous page to a paying customer, and the second failure is the worse one because nobody reports it. The response also carries that caller's own `X-RateLimit-Remaining` and `X-Quota-Remaining`, which describe one consumer's allowance and nobody else's. So a keyed request gets `private, no-store` (`security.md` §4.10, T-13) and `Vary` goes on every cacheable response *as well*, for a layer that ignores `no-store` and because an anonymous response genuinely is shared and must not be reused for a caller who sent a credential. Both carriers are named, because a `Vary` listing only `Authorization` would leave every `X-API-Key` response interchangeable. The rule is uniform across endpoints, the immutable one included: a per-endpoint exception has to be got right again on every endpoint added later, and it fails silently when it is wrong. The cost is that paid traffic no longer hits the CDN — and the traffic that decides hit rate, the public website, sends no credential at all.
- **No field hidden from the free tier that a paid tier's response also contains for a given endpoint.** The paywall is on which *endpoints* are reachable and on *volume* (quota) and *history depth*, never on stripping fields out of a shared response shape (blueprint §5). Where a field's value genuinely differs by tier (e.g. `releases` history window), that is documented per endpoint, not hidden as an undocumented field omission.

## 2. Complete endpoint reference

| Method & path | Purpose | Auth | Parameters | Response shape | Status codes | Cache policy | Quota weight | Tiers |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `GET /api/v1/search` | Full-text and alias search over products and vendors, including a hardware model looked up by the product code stamped on its chassis | optional key | `q` (required, 2–200 characters; a longer query is truncated to 200 bytes of UTF-8 on a character boundary, never rejected), `limit` (default 20, max 50), `cursor` | `SearchResult` list | 200, 400, 401, 403, 429 | `Cache-Control: public, max-age=60`, `Vary`; keyed by normalised `q` | 1 | all |
| `GET /api/v1/vendors` | List all vendors | optional key | `limit` (default 50, max 200), `cursor` | `Vendor` list | 200, 401, 403, 429 | `Cache-Control: public, max-age=3600`, `Vary` | 1 | all |
| `GET /api/v1/vendors/{slug}` | One vendor's detail | optional key | path `slug` | `Vendor` | 200, 401, 403, 404, 429 | `Cache-Control: public, max-age=3600`, `ETag`, `Vary`; invalidated when any of the vendor's products publish | 1 | all |
| `GET /api/v1/products/{slug}` | One product's detail, including `latestRelease` summary, `hasSourceConflict`, and — for a hardware model — `modelIdentifier`, the `runs` array naming the operating system it runs, and `firmwareApplicability` | optional key | path `slug` | `Product` | 200, 401, 403, 404, 429 | `Cache-Control: public, max-age=3600`, `ETag`, `Vary`; invalidated on publish for this product | 1 | all |
| `GET /api/v1/products/{slug}/releases` | Release history for a product | optional key | path `slug`; `channel`, `releaseType` (filters); `sort` (`releaseDate` \| `firstObservedAt`, default `releaseDate`); `order` (`asc` \| `desc`, default `desc`); `limit` (default 20, max 100), `cursor` | `Release` list, cursor-paginated, plus a `window` member | 200, 400, 401, 403, 404, 429 | `Cache-Control: public, max-age=300`, `Vary`; anonymous/free responses windowed to the most recent 12 months regardless of `limit`/`cursor` reach, Professional+ unwindowed | 1 (list) | history window: all; full history: Professional+ |
| `GET /api/v1/products/{slug}/latest` | The single latest-observed release for a product, with the vendor's own `recommendedRelease` and `hasSourceConflict`; **404 for a hardware model with no release stream of its own**, see §3.2 | optional key | path `slug`; `channel` (optional filter, e.g. request the latest `long_term` specifically) | `LatestReleaseResponse` | 200, 400, 401, 403, 404, 429 | `Cache-Control: public, max-age=300`, `ETag`, `Vary`; invalidated on publish for this product | 1 | all — **never windowed**, see below |
| `GET /api/v1/products/{slug}/advisories` | Security advisories affecting a product | optional key | path `slug`; `limit`, `cursor` | `Advisory` list | 200, 404, 429 | `Cache-Control: public, max-age=600` | 1 (list) | list: all; structured `affectedRanges`/`evidence` detail: Professional+ (free/anonymous responses include title, link, and severity only) |
| `GET /api/v1/releases/{id}` | One immutable release by id | optional key | path `id` | `Release` | 200, 400, 401, 403, 404, 429 | `Cache-Control: public, max-age=31536000, immutable`, `ETag`, `Vary` | 1 | all |
| `POST /api/v1/lookup` | Bulk lookup, many product slugs per call | key required | JSON body `{ "slugs": [...] }`, max 100 slugs per call | map of slug → `LatestReleaseResponse` or `null` | 200, 400, 401, 403, 429 | `Cache-Control: no-store` (per-consumer quota-relevant call, not cached) | N (one per slug resolved, minimum 1) | Professional+ |
| `GET /api/v1/usage` | The calling key's own current usage and quota | key required | none | `UsageSummary` | 200, 401, 429 | `Cache-Control: no-store` | 0 (usage queries never consume the quota they report on) | keyed (any tier with a key) |
| `GET /healthz` | Liveness probe; answers 200 unconditionally and touches no dependency | none | none | `{"status":"ok"}` | 200 | `Cache-Control: no-store` | 0 | n/a (infrastructure endpoint, not `/api/v1`) |
| `GET /readyz` | Readiness probe; runs the deployment's gate, in production a database ping | none | none | `{"status":"ok"}` or a problem document | 200, 503 | `Cache-Control: no-store` | 0 | n/a (infrastructure endpoint, not `/api/v1`) |
| `GET /metrics` | Prometheus exposition | none | none | text exposition | 200 | `Cache-Control: no-store` | 0 | n/a (infrastructure endpoint, not `/api/v1`) |

**Three of those rows are design, not deployed surface.** The table above is the intended `/api/v1` contract; `internal/adapters/httpapi/router.go` currently registers seven of its ten API routes plus the three infrastructure ones. `GET /api/v1/products/{slug}/advisories`, `POST /api/v1/lookup` and `GET /api/v1/usage` **are not routed today** — a request for any of them gets the catch-all's `not-found` problem document, the same as any unrouted path. They are equally absent from `docs/api/openapi.yaml`, and `TestEveryRouteIsDocumentedAndEveryDocumentedRouteExists` and `TestInternalRoutesAreNotInThePublicContract` in `internal/adapters/httpapi/router_test.go` assert that parity in both directions, so the three cannot be added to the machine-readable contract without a handler appearing alongside them. Marking them here rather than deleting them is deliberate: advisories are a Phase 4 feature (§3.5, mvp-roadmap.md), and `/lookup` and `/usage` land with API keys and metering in Phase 3 — but a consumer reading a "complete endpoint reference" is entitled to know which rows they can call this afternoon. The routed set is exactly:

```
GET  /api/v1/search
GET  /api/v1/vendors
GET  /api/v1/vendors/{slug}
GET  /api/v1/products/{slug}
GET  /api/v1/products/{slug}/releases
GET  /api/v1/products/{slug}/latest
GET  /api/v1/releases/{id}
GET  /healthz
GET  /readyz
GET  /metrics
```

plus the four `/internal/review` routes of §11 when, and only when, that surface's two API-server switches are both on.

**The cache column above describes an anonymous caller.** Every one of those policies applies to a request that presented no credential. A request carrying an `Authorization` bearer token or an `X-API-Key` receives `Cache-Control: private, no-store` on the same endpoint, whatever the row says, and still receives its `ETag` so it can revalidate a copy it holds itself. The reasoning is in §1 and in `security.md` §4.10 (T-13).

**What the 12-month window measures.** A release is inside the window when the *vendor's own release date* falls on or after the boundary, and — only when the vendor published no date at all — when FirmScout first observed it on or after the boundary. Windowing purely on observation time would make the window a no-op we nonetheless advertised, because in a young catalogue everything was observed last month; windowing purely on release date would silently drop every undated release, which is a different and worse lie. The window is applied by the query rather than by filtering a page the query already returned, so a windowed consumer's cursors still mean what they say. The response's `window` member (§3.3a) states which window was applied.

**Reduced-precision dates at the boundary.** A month- or year-precision date is compared at the *end of the period its published precision denotes* — a release the vendor dated only `2025` is inside a window opening on 5 September 2025, because the vendor may have shipped it in December and nothing FirmScout holds says otherwise. Comparing at the canonical anchor instead (§4) would treat `2025` as 1 January 2025 and hide the release, which asserts a day the vendor never published — precisely what ADR-0017 exists to prevent. The rule is therefore over-inclusive at the boundary by design: a caller may see a release a few weeks older than their window, which is visible and harmless, rather than lose one they are entitled to, which is neither.

Both sides of the comparison are reduced to a UTC calendar day, so `2025-09-05` is inside a window opening on 5 September and `2025-09-04` is not, whatever hour of the day the boundary itself carries.

**Honest status, corrected.** Until the closing pass of Phase 2 this paragraph carried a disclosure that the rule lived in one place only: the API layer applied it where it computed `window.latestOutsideWindow`, while the query that applies the window compared the stored anchor and therefore dropped reduced-precision releases at the boundary. That gap is closed. `internal/adapters/postgres`'s `ListForProduct` now compares the period end in SQL (`CASE … month_only → release_date + 1 month − 1 day …`) against the boundary reduced with `AT TIME ZONE 'UTC'`, `internal/application/apptest`'s in-memory double compares `domain.PartialDate.PeriodEnd()`, and `latestOutsideWindow` in `internal/adapters/httpapi` compares that same `PeriodEnd` against the boundary's UTC day. The rule is expressed in three languages because the period end varies per row and cannot be folded into the single `since` parameter; `TestListForProductWindowKeepsReducedPrecisionDates` checks the SQL's verdict for every seeded row against `domain.PeriodEnd` rather than a hand-written expectation, so a change to one language that the others do not follow fails rather than drifts.

**Filters are applied to the page, not by the query.** `channel` and `releaseType` narrow the page after the query has run and after its next cursor has been minted, so **a filtered page can be empty while `nextCursor` is non-null** — there were rows, and none of them matched. A client must page until `nextCursor` is `null` rather than stopping at the first empty page. Only the window is pushed into the query; pushing the two filters down as well is the intended end state and would make an empty first page mean what a consumer expects it to mean.

**Sorting is page-local, for the same reason.** Pagination is always by first-observed time descending with the release id breaking ties, because that pair is the keyset the cursor encodes and a page order that differs from the cursor order cannot paginate. `sort` and `order` reorder the rows *within* each returned page; they do not change which rows fall on which page. `order=asc` therefore returns the newest page first with its rows ascending inside it. A consumer who needs the whole history in a particular order must page to the end and sort the result. Making `sort` real means a query that orders by the requested key with a cursor built from that key — a repository change, not a handler one, and it is not made. This is documented rather than implied because the alternative is a contract that promises an ordering the endpoint does not deliver.

Every endpoint above except `POST /api/v1/lookup` and `GET /api/v1/usage` accepts anonymous requests, per the design principle that the public website is a client of this same API (blueprint §12) — anonymous requests simply carry the lowest, most tightly rate-limited quota tier (§7) and cannot reach `/lookup`. Sending *no* credential is therefore never an error on a read endpoint; sending one that cannot be used is, which is why every read endpoint lists 401 and 403 among its status codes.

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

Only `slug` and `name` are guaranteed. `website` is omitted entirely for a vendor whose registry document records no homepage — never sent as an empty string — and `productCount` and `lastVerifiedAt` are aggregate facts `domain.Vendor` does not carry, so they are absent rather than zero. See §10 on why that is stated here and enforced by a test.

### 3.2 `Product`

```json
{
  "slug": "mikrotik-routeros",
  "name": "RouterOS",
  "vendor": { "slug": "mikrotik", "name": "MikroTik" },
  "family": null,
  "modelIdentifier": null,
  "runs": [],
  "aliases": ["RouterOS v7"],
  "firmwareApplicability": {
    "verified": true,
    "basis": "own_releases",
    "ownReleases": { "mapped": true, "releaseCount": 12 }
  },
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
  "hasSourceConflict": false,
  "conflict": null,
  "lastVerifiedAt": "2026-09-03T18:30:00Z"
}
```

`officialSources` lists the sources that have actually contributed a currently-mapped, non-withdrawn release to *this* product — not every source registered for the vendor. A source that has never successfully produced data for this product does not appear here even if it exists in the registry; the example above is RouterOS's real changelog page, the one source that has actually contributed its published releases.

`hasSourceConflict` is `true` when eligible sources disagree about this product's newest version and no human has resolved that yet (ADR-0020). It is always present, never omitted when false: an absent key reads as "no conflict", and "we checked and they agree" is a different claim from "we did not check" — which is the entire reason a disagreement is recorded rather than quietly arbitrated.

`conflict` is additive alongside `hasSourceConflict`, never a replacement for it. Where the boolean only answers "is something disputed", `conflict` answers "what, exactly" — which channel, which versions, how many sources, and when the disagreement was first detected:

```json
"conflict": {
  "channel": "stable",
  "versions": ["7.24.1", "7.24.2"],
  "sourceCount": 2,
  "detectedAt": "2026-09-03T18:30:00Z"
}
```

It is `null` whenever `hasSourceConflict` is `false`, and it is also `null` — honestly, not defensively — when `hasSourceConflict` is `true` but nothing has recorded a channel and versions for it yet: a product summary computed before this field existed, or a conflict the detector opened without finishing that detail. FirmScout never fabricates a placeholder object just to make the two fields agree; a consumer that wants the boolean's certainty without the object's detail can keep reading only `hasSourceConflict`, exactly as before this field existed.

`family` is `null` for a product not part of a multi-product family; populated as `{ "name": ... }` when applicable. It carries **no `slug`**, deliberately: a family slug is unique per vendor rather than globally, `/products/{slug}` resolves product slugs, and there is no family route for a slug to point at. Emitting one would hand a consumer an identifier that resolves to nothing. See ADR-0024.

`latestRelease` here is a summary, not the full `Release` object — the full object is fetched via `/products/{slug}/latest` or `/releases/{id}`.

#### A hardware model is the same schema

A fleet's inventory is a list of model numbers, so the model number has to be a thing this API can return. It is: a hardware model is a `Product` whose `modelIdentifier` is the vendor's own published product code, reached on this same route by the same slug lookup. There is no `/devices` route and no `kind` discriminator, because a caller holding a model number does not know in advance which kind of thing the slug they followed names — and because the two are not exclusive: a rack server is a device *and* publishes its own BIOS versions.

```json
{
  "slug": "mikrotik-crs328-24p-4s-rm",
  "name": "CRS328-24P-4S+RM",
  "vendor": { "slug": "mikrotik", "name": "MikroTik" },
  "family": { "name": "ARM 32bit" },
  "modelIdentifier": "CRS328-24P-4S+RM",
  "runs": [{ "slug": "mikrotik-routeros", "name": "RouterOS" }],
  "aliases": ["CRS328-24P-4S+RM"],
  "firmwareApplicability": {
    "verified": false,
    "basis": "runs_os_unverified",
    "ownReleases": { "mapped": false, "releaseCount": 0 }
  },
  "category": "network-devices",
  "latestRelease": null,
  "officialSources": [],
  "hasSourceConflict": false,
  "conflict": null
}
```

**`modelIdentifier`, `runs`, `aliases` and `firmwareApplicability` are always present**, on every product, under the same rule §3.2 states for `hasSourceConflict`: an absent key is read as a claim, and here the claim it would be read as is false. `modelIdentifier` is an explicit `null` rather than an omitted key so a consumer can tell "this is not a hardware model" from "the API did not send it"; `runs` and `aliases` are empty arrays rather than `null` so they can be ranged over without a check.

`aliases` is the other strings the product is known by — marketing names, typeable spellings, and above all the vendor's model number. It is on the wire because the alias is frequently the reason the caller is on the page at all: per ADR-0024, model-number search runs through a `model_number` alias rather than through a column, so the hit a fleet manager clicked was produced by a string the response was not sending back, and the page could truthfully render "no aliases recorded" about the very code they had pasted. Raw alias text ships, never the normalised form — the normalised form exists so a lookup can match loosely, and showing it would hand the reader a mangled version of what they typed. The alias *kind* is deliberately not on the wire: `modelIdentifier` is already the authoritative answer to "which of these is the model number", and an alias kind would be a second, weaker answer to the same question.

The three are kept apart because they carry different evidence and different confidence, and collapsing them would let the weakest borrow the strongest's authority:

1. **`modelIdentifier` — "this is that model."** Read verbatim from the vendor's own product page.
2. **`runs` — "this model runs that operating system."** An existence claim, and a navigation one: it says where the releases are published. Each entry's `slug` resolves on this same route.
3. **"The right firmware release for this exact model is X" — not claimed.** `firmwareApplicability` is where that absence is stated out loud rather than left to be inferred from a missing field.

#### `firmwareApplicability` answers two questions, and keeps them apart

`firmwareApplicability` carries two members because there are two independent claims, and a product can need both answered:

- **`ownReleases`** — *"are the releases this response carries this product's own?"* A **mapping** fact: `{ "mapped": true, "releaseCount": 12 }` says a `release_product_mappings` row exists, which is what makes `latestRelease` non-null and `/products/{slug}/releases` non-empty.
- **`verified` + `basis`** — *"which of another product's releases apply to this exact model?"* A **verification** status, and today the honest answer for every device is that nobody has performed one.

They were a single `{verified, basis}` scalar until the product shape ADR-0024 itself names — *"a rack server is a device AND publishes its own BIOS versions, so a product can be both"* — showed what that cost. Such a product reported `{ "verified": true, "basis": "own_releases" }` and dropped the `runs_os` caveat entirely: its own BIOS-shaped release vouched for an operating-system applicability nobody had checked. One scalar has room for only the winner of a test, and there is no winner to pick — the two claims are simply both true.

`basis` is a closed vocabulary, not a sentence: the API ships a machine-readable state and a client renders the prose, the same split `hasSourceConflict` and its banner already use.

| `basis` | `verified` | Means |
| --- | --- | --- |
| `own_releases` | `true` | No other product's releases are in play: releases are mapped to this product and it runs nothing FirmScout catalogues, so no per-model question is open. For an operating system that is the whole question. |
| `runs_os_unverified` | `false` | This product runs an operating system that publishes releases, and FirmScout has **not** established which of them are the right image for this exact model. It says nothing about whether this product *also* publishes releases of its own — `ownReleases` answers that separately. |
| `none_recorded` | `false` | Neither is true yet: no releases mapped, no `runs` edge recorded. |

**The `runs` edge is tested before the release count, and that ordering is the contract.** A caveat may be retracted only by a verification; a release-mapping row is not one. Under the old ordering any mapped release outranked the `runs` edge and silently withdrew the caveat, so a device with one BIOS build of its own claimed verified firmware applicability for an operating system it had never been checked against.

#### A product that is both: the worked example

`hAP be lite` as a device that has also published one firmware image of its own. Both claims are on the response, and neither is allowed to speak for the other:

```json
{
  "slug": "mikrotik-hap-be-lite",
  "name": "hAP be lite",
  "vendor": { "slug": "mikrotik", "name": "MikroTik" },
  "modelIdentifier": "C53UiG+5HaxD2HaxD",
  "runs": [{ "slug": "mikrotik-routeros", "name": "RouterOS" }],
  "aliases": ["C53UiG+5HaxD2HaxD", "hAP be lite"],
  "firmwareApplicability": {
    "verified": false,
    "basis": "runs_os_unverified",
    "ownReleases": { "mapped": true, "releaseCount": 1 }
  },
  "latestRelease": {
    "id": "rel_01J8Z3K9QWERTYUIOPASDFGH",
    "rawVersion": "1.4.0",
    "channel": "stable",
    "releaseDatePrecision": "unknown"
  },
  "hasSourceConflict": false,
  "conflict": null
}
```

Read as a pair: **`latestRelease` is this product's own** — `ownReleases.mapped` is `true`, so the version shown is a release genuinely mapped to this model — **and it is not an answer to "what RouterOS build does this box take"**, which `verified: false` with `basis: "runs_os_unverified"` says is still unestablished. A client rendering only `verified` shows the caveat, which is the safe half; a client that wants to say "this device publishes its own firmware *and* runs RouterOS, whose per-model fit is unverified" has both facts to say it with. A `basis` member meaning "own releases *and* an unverified runs edge" was rejected as the alternative: it encodes a pair of claims as a product of them, so each new claim doubles the vocabulary, and every consumer switching on `basis` mishandles the new member on the day it ships.

**What `runs_os_unverified` costs a consumer, stated plainly:** the device page names the operating system and does not name a version. `GET /products/{slug}/latest` answers **404** for such a product and `GET /products/{slug}/releases` answers an empty page — both correct, because no release is mapped to it. Substituting the operating system's newest release would be worse than the gap: it would tell an operator to flash an image FirmScout never established their hardware takes, which is the fabricated fact this whole contract is built to refuse. See ADR-0024.

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

### 3.3a `ReleaseListResponse` and the history window

```json
{
  "releases": [ { "...": "Release objects, §3.3" } ],
  "pagination": { "nextCursor": null },
  "window": {
    "windowed": true,
    "since": "2025-09-05T00:00:00Z",
    "latestOutsideWindow": false,
    "detail": "This history is windowed to the most recent 12 months. A Professional plan or above returns the complete archive."
  }
}
```

`window` is always present. For a Professional, Enterprise, or internal plan it is `{"windowed": false}` and nothing else — there is no boundary to state, because there is no boundary.

`latestOutsideWindow` is present only on a windowed response, and it is `true` when this product's newest observed release is older than `since`. It exists because `GET /products/{slug}/latest` is never windowed (§2): for a discontinued product — the normal state of most hardware in this catalogue — an anonymous caller otherwise receives a latest release from one endpoint and an empty history from the other, with nothing to reconcile them. When the flag is `true`, `detail` says so in words and names the endpoint that still answers. The flag claims "outside" only when the vendor's own date puts the release outside at *every* precision that date could denote (see "Reduced-precision dates at the boundary", §2), and it applies exactly the comparison the window query applies — period end against the boundary's UTC calendar day — so the flag and the page it describes cannot contradict each other. An undated release is never claimed at all: those are windowed by first-observed time, which this response does not carry, and an absent hint is worth more than a guessed one.

§1 forbids hiding a *field* from a tier, and windowing is not that: the shape is identical for every caller and every field is present. But a consumer who cannot tell a windowed history from a complete one has been misled by omission — they would read a catalogue holding two years of releases as a product that shipped twice. One small, always-present object closes that, and `detail` names the plan that lifts the window so the flag is not a dead end.

### 3.4 `LatestReleaseResponse`

```json
{
  "vendor": { "slug": "mikrotik", "name": "MikroTik" },
  "product": { "slug": "mikrotik-routeros", "name": "RouterOS" },
  "latestRelease": { "...": "a full Release object, §3.3" },
  "recommendedRelease": null,
  "hasSourceConflict": false,
  "conflict": null
}
```

Extends the example given in blueprint §12 with the members that make the answer honest.

`hasSourceConflict` is always present, in both states, and carries the same meaning as on `Product` (§3.2). This is the endpoint whose entire purpose is answering "what version should I be on?", and answering it with a version that eligible sources disagree about, with no flag a consumer can check, is the outcome ADR-0020 exists to prevent: the disagreement is recorded rather than arbitrated precisely so it can be surfaced here.

`conflict` is additive alongside `hasSourceConflict`, with the same shape and the same nil-when-absent rule as on `Product` (§3.2) — see that section for the full description. It exists on this endpoint for the same reason the boolean does: a caller asking "what version should I be on?" and receiving a contested answer deserves to know which channel and versions are contested, not only that a dispute exists.

`recommendedRelease` is the vendor's own designated release as a full `Release` object, or `null` when the vendor never designated one. It is a separate member rather than a flag on `latestRelease` because the two are often different releases — a vendor that recommends an older, longer-supported build is making a statement FirmScout passes on rather than overrides with a sort order. FirmScout never substitutes "the newest release" for a missing designation (§3.3, `recommended`).

**This endpoint is never windowed.** A plan bounds how much *history* a caller receives, and the newest observed release is not history: it is the current answer, and it is the answer the public website — an anonymous caller of this same API — exists to show. Windowing it would make FirmScout report "this product has no published release" for a product that plainly has one whenever the vendor last shipped over twelve months ago, which is a false statement in a product whose only asset is being right; §1 already puts the paywall on endpoints, volume and history depth, never on the current fact. The resulting difference between this endpoint and an empty windowed history page is stated by that page rather than left to be discovered — see `window.latestOutsideWindow` in §3.3a.

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
      "type": "product",
      "slug": "mikrotik-hap-ax3",
      "name": "hAP ax³",
      "modelIdentifier": "C53UiG+5HPaxD2HPaxD",
      "vendor": { "slug": "mikrotik", "name": "MikroTik" },
      "matchedOn": "alias"
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

`modelIdentifier` echoes the vendor's product code back on a hit, so somebody who pasted the string stamped on a chassis can see that this row is the thing they are holding. Unlike the field on `Product`, it carries `omitempty` and is simply absent for a product that is not a hardware model: a search result is a compact hit descriptor, and an explicit `null` on every software product in every result set is noise rather than honesty.

**A model number is a first-class query.** Searching a device's published product code returns that device, because the code is registered as a `model_number` alias and alias text is part of the product's search vector — the same path that already matches a marketing name or a misspelling, with no separate index and no separate route. This is the query a fleet manager actually types, and it is the one the catalogue exists to answer (ADR-0024).

`matchedOn` reports which field the query plausibly hit, not which registry row made the row findable, and for some devices those differ. `CRS328-24P-4S+RM` is both the model's name and its product code, so a search for it reports `"name"`; `C53UiG+5HPaxD2HPaxD` looks nothing like `hAP ax³`, so a search for that reports `"alias"`. Both found the device, which is the part a caller acts on.

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

Every field of RFC 9457 is populated; `requestId` is a FirmScout extension member equal to the `X-Request-Id` header value, so a consumer reading only the body (not headers) still has the correlation id. See §6 for the full catalogue of `type` values.

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
- **Filtering.** Filter parameters (`channel`, `releaseType` on `/releases`) are exact-match against the constrained vocabularies (blueprint §4.4) — there is no free-text filter expression language, and an unrecognised filter value is a `400` with the `invalid-parameter` problem type (§6), not a silently-empty result set.
- **Sorting — the explicit rule that releases are never sorted by version string.** `/products/{slug}/releases` accepts `sort=releaseDate` (default) or `sort=firstObservedAt`; there is **no `sort=version`** option, and this is intentional, not an oversight to be added later. Blueprint §3.5 and ADR-0017 establish that version strings are opaque, non-comparable tokens — `3.003.0015.001` and `7.24.2` have no defined ordering relative to each other outside a plausibility check, and sorting by version string would silently produce a wrong, misleading order for any vendor whose scheme is not a simple dotted-integer sequence (which is most of the brief's own worked examples). "Latest" is a derived fact from release date, first-observed timestamp, and channel (blueprint §3.5), never from string comparison, and the API's sort options reflect exactly that and nothing else.

## 6. Error catalogue

Every `type` is a stable URI under `https://firmscout.dev/problems/` (resolving, eventually, to a human-readable explanation page — not required to be a live URL for the error to be valid per RFC 9457, but intended to be one).

This table is the canonical set. The Go constants in `internal/adapters/httpapi/problem.go` are named after it, and `TestProblemTypesAreCanonical` asserts every one of them against this list character for character, so the two cannot drift without a test failing.

| `type` | `title` | `status` | Go constant | When it occurs |
| --- | --- | --- | --- | --- |
| `.../problems/not-found` | Resource not found | 404 | `TypeNotFound` | A vendor/product/release/advisory/review-item slug or id does not resolve to an existing record. |
| `.../problems/invalid-parameter` | Invalid parameter | 400 | `TypeInvalidParameter` | A query or path parameter fails validation (out-of-range `limit`, unrecognised `channel`/`releaseType`/`state`/`kind`/`sla` value, malformed `cursor`, empty `q`), or a required non-credential header is absent (`X-FirmScout-Actor`, §11). |
| `.../problems/validation-failed` | Request body validation failed | 400 | `TypeValidationFailed` | A request body is malformed, carries an unknown field, exceeds its size limit, or fails its own rules — a review decision with no `reason`, and `POST /lookup`'s body when that endpoint ships. |
| `.../problems/unauthorized` | Authentication required | 401 | `TypeUnauthorized` | An endpoint requiring a key received none, or an `Authorization` header that could not be read as a bearer token, or the deployment has no key store configured and therefore cannot check a credential at all. |
| `.../problems/invalid-api-key` | API key invalid or revoked | 401 | `TypeInvalidAPIKey` | A syntactically well-formed key does not match a live, non-revoked `api_keys` row. Split out of `unauthorized` because "send a credential" and "replace the one you sent" are different instructions, and a consumer that cannot tell them apart retries the dead key forever. |
| `.../problems/forbidden` | Not available on this plan | 403 | `TypeForbidden` | An authenticated caller's plan does not reach this endpoint or feature, or the key's account is not active. |
| `.../problems/conflict` | Conflicting state | 409 | `TypeConflict` | The resource's current state forbids the operation — a review item somebody has already decided (§11). Nothing the caller can fix by retrying or by editing their payload, which is exactly what a 400 would have implied. |
| `.../problems/rate-limited` | Too many requests | 429 | `TypeRateLimited` | The in-process token bucket for this consumer/IP is exhausted; see §7. |
| `.../problems/quota-exceeded` | Monthly quota exceeded | 429 | `TypeQuotaExceeded` | The durable PostgreSQL quota counter for this API key's billing period is exhausted; distinct from `rate-limited` (short-window) even though both return 429 — the `type` tells a consumer which one to look at. |
| `.../problems/internal-error` | Internal server error | 500 | `TypeInternal` | An unhandled server-side failure; `detail` is deliberately generic (never leaks internals), `requestId` is the correlation handle for support. |
| `.../problems/service-unavailable` | Service temporarily unavailable | 503 | `TypeServiceUnavailable` | A readiness gate or dependency is failing (e.g. the database or the key store is unreachable) — returned by `/readyz` and, transiently, by any endpoint. |

### Reconciled in Phase 2 — three URIs changed

The Go implementation and this table used to spell five of these differently. This document won, because its own preamble already declared itself the tie-break when the two could be read as disagreeing, and because its split is the more informative one. A problem type is an identifier consumers hardcode into their error handling, so the cost of a rename only ever goes up; the API is pre-alpha, unpublished, and has no keyed consumers, which made this the last cheap moment. See [ADR-0022](../adr/0022-canonical-problem-types.md).

| Old URI (code before Phase 2) | New URI (canonical) | Go identifier |
| --- | --- | --- |
| `.../problems/invalid-request` | `.../problems/invalid-parameter` | `TypeInvalidRequest` → `TypeInvalidParameter` |
| `.../problems/unauthenticated` | `.../problems/unauthorized` | `TypeUnauthenticated` → `TypeUnauthorized` |
| `.../problems/internal` | `.../problems/internal-error` | `TypeInternal` (value changed, name unchanged) |

`invalid-api-key` and `conflict` are additions, not renames: nothing emitted either before Phase 2. The constructor functions renamed with their types (`InvalidRequest` → `InvalidParameter`, `Unauthenticated` → `Unauthorized`, plus new `ValidationFailed`, `InvalidAPIKey` and `Conflict`), so a stale reference in Go is a compile error rather than a runtime surprise. A reader of a pre-Phase-2 draft can map the old spellings with the table above; no live consumer was broken, because there were none.

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

### 10.1 `required` means "always sent", and a test now says so

A key in a schema's `required` list is not a documentation nicety. A generated client types it non-optional and crashes on the first response that omits it, which is precisely what happened to the web app's product and search pages on the first hardware model ever catalogued: `releaseType` and `lastVerifiedAt` were listed as required, a device has neither, and the pages 500ed.

That correction has now been made three times by hand — `releaseType`, `lastVerifiedAt` and `productCount`; then `Product.category` and `Vendor.website`; then `channel` on both release shapes, `Release.product`, `Evidence.retrievedAt` and the two release observation timestamps — and every round was found by a human reading YAML against Go struct tags. `TestEveryRequiredKeyIsActuallyAlwaysSent` in `internal/adapters/httpapi` now does it instead: it parses `components/schemas`, renders the **emptiest** value the presenter can produce for each schema, and fails if any required key is missing from the JSON. A schema with no sample fails too, so a new one cannot arrive unchecked.

The rule the test enforces, stated once so both directions are deliberate:

- A key is `required` **only if** the presenter sends it for every reachable input. Over-declaring breaks a client and is the defect above.
- A key that *is* always sent **should** be `required`, even when it is an explicit `null` or an empty array — `modelIdentifier`, `runs`, `aliases`, `latestRelease`, `conflict`, `hasSourceConflict`, `firmwareApplicability`. Under-declaring only understates a guarantee, so the test does not enforce it, but a consumer reading the document deserves the real contract.
- `Product.family` uses `ProductFamilySummary`, which requires `name` and not `slug`, rather than the `ProductSummary` used by `runs` and `Release.product`. Family slugs are unique per vendor and no route resolves one, so a family reference deliberately carries no slug (ADR-0024) — and weakening `ProductSummary` to accommodate it would have dropped the slug guarantee from `runs`, the one array whose entire purpose is being followable.

## 11. The internal review surface (`/internal/review`)

> **Nothing that switches this surface on may be reachable from the public internet — and that is three switches on two different hosts, not two switches on one.** Two of them sit on the API server and decide whether these endpoints exist at all; the third sits on the public Next.js site and decides whether a read-only viewer of the same data exists there. The endpoints are unauthenticated, and two of the four write to the catalogue: an accept publishes a release that a validation gate refused. The API server belongs behind an authenticating proxy or on a private network, and the web app must not have its viewer switched on in a deployment the public can reach. Nothing in the code can detect its own network exposure — placement is the operator's control and the only one there is. See [ADR-0021](../adr/0021-asserted-reviewer-identity.md).

**Why this section now names three switches.** It named two, both on the API server, and that omission was not hypothetical: the code that exploited it was written, reviewed and integrated into Phase 2's working tree. It never reached a deployment, because FirmScout has never had one — no image has ever been built and no container has ever run — and the write path was removed inside the same phase, before any of this work was committed. The precise claim is therefore not "this shipped to users" but "this was the failure mode the first real deployment would have had", which is the one worth writing down. Read this section, place the API on a private subnet exactly as prescribed, then set `FIRMSCOUT_REVIEW_UI_ENABLED=true` on the *public* site believing it gates nothing dangerous, and the result is an internet-reachable unauthenticated publish with both documented switches set correctly and the API never reachable from outside its subnet. What that third switch gated was a Next.js Server Action that issued the accept and reject requests from `apps/web`'s own server — which necessarily sits somewhere that can reach the API, because that is what makes it a client at all. The write path has since been removed from `apps/web` (ADR-0021's amendment); this section names all three switches so the next operator is not asked to infer the third from a codebase.

**The three switches, and the host each is set on.**

| Switch | Set on | What it gates | Default | Verified in |
| --- | --- | --- | --- | --- |
| `FIRMSCOUT_REVIEW_API_ENABLED` → `Deps.ReviewAPIEnabled` | the **API server** (`apps/api`) | Whether the four routes in §11.1 are registered on the mux at all | `false` | `internal/platform/config.go` (`envBool("FIRMSCOUT_REVIEW_API_ENABLED", false)`), `internal/adapters/httpapi/router.go` |
| The three review use cases being non-nil in `Deps` (`ReviewQueue`, `ReviewItems`, `Review`) | the **API server**'s composition root (`apps/api/main.go`) | The same four routes: with any of them nil the routes are not registered even when the flag is on. `apps/api` leaves all three nil unless the flag is set, so the two switches are wired to move together and neither alone is sufficient | nil | `apps/api/main.go`, `internal/adapters/httpapi/router.go` |
| `FIRMSCOUT_REVIEW_UI_ENABLED` | the **public web app** (`apps/web`) — a different process from the API, and in any real deployment a different host | Whether `/review` and `/review/{id}` exist on the public site. These are **read-only** pages that call `GET /internal/review/items` and `GET /internal/review/items/{id}` server-side, against `FIRMSCOUT_API_URL`. They cannot accept or reject: `apps/web` has no write path to this surface, by construction | unset (falsy) | `apps/web/lib/review.ts` (`reviewUiEnabled()`), `apps/web/app/review/page.tsx`, `apps/web/app/review/[id]/page.tsx` |

The first two are ANDed in `NewServer`: with either off the paths do not exist, and a request for one gets the same `not-found` problem document any other unrouted path gets — there is no 403 that would confirm the surface is there. The third is independent of both and lives in another process: `apps/web` cannot see the API server's flag, so a deployment with the UI switch on and the API switch off renders the same honest "not enabled on this deployment" state any unreachable endpoint produces.

**The web switch is not a lesser one.** `FIRMSCOUT_REVIEW_UI_ENABLED=true` on an internet-facing site is an information disclosure, not a defacement risk — anyone who can reach `/review` sees candidate provenance, evidence excerpts, priority scoring, gate verdicts and the audit trail's asserted (unverified) reviewer names, with no login anywhere in the path. Smaller than a forged publish; not zero. Two details are worth an operator's attention: the pages set `robots: { index: false, follow: false }` in their metadata, but `apps/web/app/robots.ts` serves `Allow: /` for every user agent, so `noindex` is the only thing keeping the queue out of a search index — a crawler is not told to stay away, only not to publish what it finds; and the page renders a `<UnauthenticatedBanner />` saying all of this to whoever is looking, which is honesty toward a reader and not a control on an operator, who never sees it while setting environment variables.

**Accepting and rejecting is the CLI's job, and only the CLI's.** `firmscout review accept --id <id> --actor <name> --reason <text>` and `review reject` (`apps/cli/main.go`) call `application.DecideReviewItem` directly against `FIRMSCOUT_DATABASE_URL` — the same use case the HTTP handler calls, reached without the HTTP surface running or being reachable. The `POST` endpoints in §11.1 still exist and are still unauthenticated; what changed is that no FirmScout-authored client calls them from a public deployment. The CLI's control is that running it takes a database connection string and a shell on a host that has one, which is the boundary operators already protect for `migrate up` and `registry sync`.

**Residual risk, stated plainly.** The actor on every decision is asserted and unverified. The platform does not check it, cannot check it, and says so in a column: every `audit_events` row this phase writes sets `actor_authenticated = false`, and the decision response echoes `"actorAuthenticated": false` (§11.2). Anyone who can reach the endpoint can write any name into the audit trail, including someone else's. There is no authentication, no session, no roles, and no permission distinction between reading the queue and publishing from it. **Network placement is the only real control on this surface, and it is a control the code cannot verify, cannot report on, and will not warn you about** — the API server logs one warning line at startup when the flag is on, and that is the whole of the machine's contribution.

They are deliberately **absent from `docs/api/openapi.yaml`**. That document is the machine-readable *public* contract, and a client generator pointed at it must not produce bindings for an off-by-default unauthenticated surface. It is the one place where "the OpenAPI file describes the endpoints" is true only of the public API, and this section is where a reader is told so.

### 11.1 Endpoints

| Method & path | Auth | Actor header | Response | Cache | Quota | Status codes |
| --- | --- | --- | --- | --- | --- | --- |
| `GET /internal/review/items` | none | not required | `ReviewQueueResponse` | `no-store`, no `ETag` | 0 (unmetered) | 200, 400, 429, 500, 503 |
| `GET /internal/review/items/{id}` | none | not required | `ReviewItemDetailResponse` | `no-store`, no `ETag` | 0 | 200, 400, 404, 429, 500, 503 |
| `POST /internal/review/items/{id}/accept` | none | **required** | `ReviewDecisionResponse` | `no-store`, no `ETag` | 0 | 200, 400, 404, 409, 429, 500, 503 |
| `POST /internal/review/items/{id}/reject` | none | **required** | `ReviewDecisionResponse` | `no-store`, no `ETag` | 0 | 200, 400, 404, 409, 429, 500, 503 |

`GET /internal/review/items` accepts `state`, `kind` and `sla` (each repeatable, each matched against a closed vocabulary), `vendor` and `product` (slugs), `limit` (default 50, max 200) and `cursor`. An unrecognised value in any enumerated parameter is `invalid-parameter`, never a silently empty page — a reviewer who mistypes a filter must be told, not shown a queue that looks caught up. A `vendor` or `product` slug that resolves to nothing is different: narrowing to a scope that does not exist is a legitimate way to get zero results, so it returns an empty page. A `cursor` this API did not issue is also `invalid-parameter`, and its problem `detail` names the `cursor` rather than the filters: telling a reviewer whose cursor was truncated to check `state`, `kind` and `sla` sends them to fix three parameters that are correct, and leaves them unable to page past it. When a request carries both a cursor and an enumerated filter, the `detail` names both candidates rather than guessing.

`{id}` must match `^rev_[A-Za-z0-9]{20,32}$`, checked before any repository is touched.

### 11.2 The actor header

Both decision endpoints require `X-FirmScout-Actor`. It is **not a credential**: the platform does not verify it, cannot verify it, and records it as unverified — every `audit_events` row this phase writes sets `actor_authenticated = false`, and the decision response echoes `"actorAuthenticated": false` so a client rendering an audit trail cannot present an asserted name as a verified one.

A missing or blank header is `invalid-parameter` (400), not `unauthorized` (401). A 401 obliges the server to offer a `WWW-Authenticate` challenge, and there is no scheme to name — answering 401 would send a caller hunting for a key that does not exist.

The body is `{"reason": "..."}`, at most 8 KiB, decoded with unknown fields refused. A malformed body, an unknown field, or an empty reason is `validation-failed`. A reason is required on both accept and reject: a decision with no stated reason is not an audit trail, it is a timestamp.

### 11.3 What an accept does

Accepting an item publishes its candidate through the same `PublishRelease` use case the worker runs, inside one transaction with the review resolution, the resolution of any conflict the item covers, and the audit row. Either all of it happens or none of it does — a published release whose review item is still open, and a closed item pointing at a release that was rolled back, are both worse than a failed decision a reviewer can retry.

The override is recorded in three places that must agree: the gate verdicts stay in `validation_results`, the review item records who resolved it and why, and `audit_events` records the same decision with the reason and the request id. An item somebody has already decided answers `conflict` (409).
