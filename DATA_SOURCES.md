# Data Sources and Compliance Policy

This document is the practical, contributor-facing version of the compliance policy established in [blueprint §3.6](docs/architecture/blueprint.md#36-dell-as-the-clean-api-pilot--challenged-on-compliance-grounds) and [ADR-0018](docs/adr/0018-source-compliance-policy.md). If you're registering a new source, read this before opening a pull request.

## The principle

**Compliance status is a first-class field on every source, evaluated before collection, not discovered after the fact.** A source that would require bypassing authentication, a CAPTCHA, a rate limit, or `robots.txt` is not collected from — full stop — regardless of how technically convenient the data would be. This is enforced structurally: a source with a disqualifying compliance status is registered as `enabled = false`, visible in the dataset, and documented, rather than quietly omitted or quietly scraped anyway.

## Source quality classes

Every source is labelled with a quality class, and **third-party sources are labelled as such and never silently outrank an official one** — if an official source and a community source disagree, the official source wins by default, and the disagreement itself is surfaced (see [blueprint §16, gate 10](docs/architecture/blueprint.md#16-update-and-publication-rules)) rather than resolved by picking whichever was collected first.

| Class | Definition | Example |
| --- | --- | --- |
| **Official manufacturer source** | A domain controlled by the vendor itself: their downloads page, changelog, PSIRT feed, or official API. | `mikrotik.com`, `fortiguard.com` |
| **Authorised support portal** | A vendor-sanctioned third-party portal operating with the vendor's knowledge (e.g. after an acquisition moves documentation to a new domain, or a vendor uses a partner platform for support content). | `support.hp.com` hosting former Poly documentation |
| **Vendor-maintained repository** | A code or artifact repository the vendor operates directly (e.g. a GitHub org under the vendor's control) but that isn't the "primary" marketing/support domain. | A vendor's official GitHub releases page |
| **Trusted community source** | Not vendor-controlled, but maintained by a known, accountable party with a track record of accuracy (e.g. a well-established hardware enthusiast project, a distro's package repository). | — |
| **Unknown third party** | Anything else: a mirror, an aggregator, or a source whose relationship to the vendor hasn't been established. | — |

The full criteria for assigning a class, and how class interacts with confidence scoring and auto-publication thresholds, are in [`docs/collectors/source-quality-policy.md`](docs/collectors/source-quality-policy.md).

## `robots_policy_status` and `terms_review_status`

Every source carries two independent status fields, both evaluated **before** the source is enabled. The vocabularies below are the ones the system actually implements: they are declared in [`internal/domain/source.go`](internal/domain/source.go) and enforced by `CHECK` constraints in [`database/migrations/00001_initial.sql`](database/migrations/00001_initial.sql), so a value outside them cannot be stored.

- **`robots_policy_status`** — the result of actually fetching and reading the host's `robots.txt`: `allowed`, `disallowed`, `unknown`, or `not_applicable`. This is a mechanical, verifiable fact, not a judgment call — if `robots.txt` disallows the path you want to collect, the status is `disallowed`, and the source stays disabled until either the path changes or the vendor grants explicit permission. `unknown` is the column default and means nobody has checked yet, or the check could not reach a verdict; it is not dispatchable, because "we could not read the rules" is not "the rules permit this". `not_applicable` is for sources that `robots.txt` does not govern at all.
- **`terms_review_status`** — whether the source's terms of service have been reviewed for anything that would make automated, low-volume, polite collection of publicly available version metadata a problem: `pending`, `approved`, `restricted`, or `prohibited`. Terms review is a human judgment call (often needing legal input for anything ambiguous), unlike the mechanical `robots_policy_status`. `restricted` means collection is permitted under documented conditions — a reduced frequency, an attribution requirement — and is dispatchable; `prohibited` means it is not.

A source is dispatchable **only** when `enabled = true`, `health` is `active` or `degraded`, `robots_policy_status` is `allowed` or `not_applicable`, **and** `terms_review_status` is `approved` or `restricted`. Anything short of that leaves the source uncollected, regardless of how good the data would be. That predicate is deliberately written twice — once in `Source.CompliancePermitsCollection` and once in the `sources_dispatchable_idx` partial index — so the Go rule and the SQL rule cannot drift apart silently.

## The pilot vendors — measured 2026-09-03; every row below re-measured or first measured 2026-09-05

These are the sources evaluated for the MVP's first vertical slice. The facts below were measured directly, not assumed, and **every row names the registry file that holds it**, so a reader can check the claim against the record rather than against this paragraph. `go run ./apps/cli registry validate` reports 5 vendors, 2 products and 6 sources; `registry sync` reports all 6 as sources that will not be checked, each with its reason.

One row is measured differently from the rest, and the difference is the point. Dell's *payload* facts — its size, its validators, its format — carry the 2026-09-03 date and were **not** re-measured in this pass, because confirming them means requesting a path Dell's `robots.txt` disallows. Only its `robots.txt` was re-read. A compliance policy that requires a policy violation to keep its own evidence current is not a policy, so the evidence is allowed to age instead.

| Vendor | Source | Format | robots_policy_status | terms_review_status | Status | Notes |
| --- | --- | --- | --- | --- | --- | --- |
| **MikroTik** | `upgrade.mikrotik.com/routeros/NEWESTa7.stable` — [`dataset/sources/mikrotik/newest-stable.yaml`](dataset/sources/mikrotik/newest-stable.yaml) | Plain text (`version epoch`), ETag + Last-Modified | `allowed` | `pending` | Registered, cheapest change signal available | Near-zero-cost watcher: a conditional GET against a tiny endpoint. The host serves **no** `robots.txt` — it answers 403 — and publishes an `X-RateLimit-*` budget the fetcher does not read; see [the evidence below](#mikrotik-compliance-evidence-measured-2026-09-05). |
| **MikroTik** | `mikrotik.com/download/changelogs` — [`dataset/sources/mikrotik/changelogs.yaml`](dataset/sources/mikrotik/changelogs.yaml) | Server-rendered HTML (structured `.changelog-header` entries: version, channel badge, date) | `allowed` | `pending` | Registered | No ETag; `Cache-Control: private` — requires normalised section hashing rather than a conditional request to detect change. `mikrotik.com/robots.txt` permits every path; see [the evidence below](#mikrotik-compliance-evidence-measured-2026-09-05). |
| **Poly / HP** | Documentation portal root, registered at `docs.poly.com/bundle/` — [`dataset/sources/poly/documentation-portal.yaml`](dataset/sources/poly/documentation-portal.yaml) | The reachable bytes are a JavaScript app shell (`text/html`, 7 087 bytes), not a document | `allowed` | `pending` | **Registered, `enabled = false`, `health = relocated`** — the pilot for "source relocated after acquisition" | Source URLs are not permanent even for a vendor's own official documentation, and the registry tracks the relocation as a state rather than as a fetch failure. Measured 2026-09-05: `docs.poly.com/bundle/` 301s to `support.hp.com/bundle/`, path preserved. Registered at the portal root and **not** at a release-notes document, because no document URL under `/bundle/` can be verified — see [the evidence below](#poly-and-hp-compliance-evidence-measured-2026-09-05). `robots_policy_status` is `allowed`, corrected from the `unknown` this table carried before it was measured. |
| **Fortinet** | PSIRT advisories — [`dataset/sources/fortinet/psirt-advisories.yaml`](dataset/sources/fortinet/psirt-advisories.yaml) — registered at `filestore.fortinet.com/fortiguard/rss/ir.xml` (the redirect target of the published `fortiguard.com/rss/ir.xml`) | RSS 2.0 (XML), `content-type: text/xml`, both `ETag` and `Last-Modified` present | `allowed` | `pending` | Registered, advisory feed only, not collected (terms review pending) | Firmware **downloads** require authentication and are out of scope for automated collection; the public PSIRT feed is the pilot for "difficult portal, but an official public feed exists for part of the data." Measured 2026-09-05: `www.fortiguard.com/rss/ir.xml` 302s to `filestore.fortinet.com/fortiguard/rss/ir.xml`, which is what is registered — the published address is a different registrable domain, and registering it would put the source into `relocated` on every check. `www.fortiguard.com/robots.txt` disallows only `/*?*` and `/threat-research/data` (neither matches the feed path); `filestore.fortinet.com/robots.txt` returns 404, so no robots rule restricts the registered host. |
| **Dell** | `downloads.dell.com/catalog/Catalog.xml.gz` — [`dataset/sources/dell/catalog.yaml`](dataset/sources/dell/catalog.yaml) | Gzipped XML, ETag + Last-Modified, ~1.4 MB — *recorded 2026-09-03; deliberately **not** re-measured, see above* | **`disallowed`** | `pending` | **Registered, `enabled = false`, `health = disabled`** | Technically the ideal source for the "clean, structured catalogue" profile — but `downloads.dell.com/robots.txt` returns `Disallow: /` for all user agents (only `/manuals` and `/topicspdf` are permitted, and both of those now redirect off the host). Collecting it anyway would violate this project's own stated policy, in public, on day one. It stays in the registry, visibly disabled, pending either an official API or written permission from Dell. `source_type` is `download_portal`, not `xml_feed`: the URL ends `.xml.gz`, but a filename is not a content type and this pass read no bytes from it. See [the evidence below](#dell-compliance-evidence-measured-2026-09-05). |
| **Ubiquiti** | `fw-update.ubnt.com/api/firmware-latest` — [`dataset/sources/ubiquiti/firmware-latest.yaml`](dataset/sources/ubiquiti/firmware-latest.yaml) | HAL JSON, `application/json; charset=utf-8`, 744 990 bytes, **no `ETag`, no `Last-Modified`, `Range` ignored** | `allowed` | `pending` | Registered, `enabled = false`, `health = pending_review` | The recommended substitute for Dell in the "clean API" profile, and the one source here whose data shape genuinely earns the label: 785 entries, 224 product codes, 353 platforms, and RFC 3339 `created`/`updated` timestamps the vendor publishes, so an `exact_day` date needs no inference (ADR-0017). Clean in structure and **expensive in bandwidth**: with no conditional-request validator, every check transfers the full 745 KB. See [the evidence below](#ubiquiti-compliance-evidence-measured-2026-09-05). |

**What the Fortinet PSIRT feed cannot publish.** The feed states that an advisory exists, its identifier, and when it was published; it does not state which Fortinet products the advisory affects — that fact lives on the advisory's own page, not in the feed. Every candidate this source produces is therefore honestly limited to "Fortinet published this advisory on this date," never "this advisory affects FortiOS." The collector config's confidence score is set below the automatic-publication threshold specifically so that every candidate from this source is routed to human review rather than auto-published, and an `advisory`-type release can never carry the latest-observed flag. Correlating an advisory with the products it actually affects is Phase 4 (`security_advisories`) and is explicitly out of scope for this registration.

**The recommended substitute for the "clean API" pilot profile is Ubiquiti**, not Dell: `fw-update.ubnt.com/api/firmware-latest` is an official JSON (HAL) endpoint returning `channel`, `platform`, `product`, timestamps, and checksums, with no `robots.txt` restriction (the host returns 404 for `robots.txt`, and RFC 9309 §2.3.1.3 makes an unavailable `robots.txt` a full allow, which is how `RobotsCache.policyFor` treats every 4xx — `terms_review_status` still has to move from `pending` to `approved` or `restricted` before this source is enabled). Ubiquiti is registered in [`dataset/sources/ubiquiti/firmware-latest.yaml`](dataset/sources/ubiquiti/firmware-latest.yaml) with `robots_policy_status = allowed`, `terms_review_status = pending`, `enabled = false`.

**"Clean" is a claim about shape, not about cost, and this pass separated the two.** Measured 2026-09-05, the endpoint serves **no `ETag`, no `Last-Modified`, no `Cache-Control`, no `Accept-Ranges`**, and answers a `Range: bytes=0-0` request with the entire 744 990-byte body. There is no conditional-request validator to make an unchanged check cheap, so the only change signal available is a hash of the whole document and every check transfers 745 KB. Set against MikroTik's `NEWESTa7.stable`, which answers an unchanged check with a 304 over a body of a few bytes, the substitute is the better-structured source and the far more expensive one. That is a real trade the recommendation has to carry, and it is why the registered `min_frequency_seconds` is six hours rather than the ten minutes the MikroTik pointer gets.

### Which of these sources has a collector config, and why the rest do not

A collector config is an expression of intent to collect. Writing one for a source FirmScout must not or cannot read would make the repository's refusal legible in prose and illegible in its file tree, so three of the four sources registered in this pass have **no** config, for three different reasons (Fortinet's PSIRT feed, the fourth, does have one — it is readable and permitted, and it is disabled for a terms review, not for a compliance refusal):

| Source | Config? | Why |
| --- | --- | --- |
| MikroTik ×2, Fortinet ×1 | Yes — `collectors/config/{mikrotik,fortinet}/` | `robots` permits the path and the engine they need (`text_regex`, `html_selectors`, `rss_atom`) is implemented. They still ship `enabled = false`: a config describes how a source *would* be read; it does not authorise reading it. |
| **Dell** | **No, on principle** | `robots.txt` disallows the path. A config in `collectors/config/dell/` would describe how to parse a document this project has committed to not fetching. This is the one of the three where the absence is a policy statement rather than a limitation. |
| **Poly / HP** | **No, nothing to configure** | The only candidate engine for the PDFs the roadmap describes is `pdf_text`, a declared-but-unimplemented roadmap engine, and the bytes actually reachable are a JavaScript app shell that no config engine can extract from. There is also no verified document URL to point a config at. |
| **Ubiquiti** | **No, blocked on the engine** | This is the one that *should* have a config — `robots` permits it, it is official, the JSON is well-formed and it publishes real dates. The engine it needs is `json_path`, which is reserved but unimplemented (`roadmapEngines` in `internal/adapters/collectors/config.go`); `LoadDir` refuses a config naming one, so writing it now would break the build rather than wait quietly. It waits on the engine. |

### MikroTik: compliance evidence measured 2026-09-05

Re-measured directly against the live hosts on 2026-09-05. These are observations. The
legal conclusion they invite has deliberately **not** been drawn here — see the last
paragraph for what is still a person's decision.

- **`mikrotik.com/robots.txt` permits everything.** The file is exactly three lines:
  `User-agent: *`, an empty `Disallow:`, and a `Sitemap:` line naming
  `https://mikrotik.com/sitemap.xml`. An empty `Disallow` value permits every path.
- **`upgrade.mikrotik.com` serves no `robots.txt` at all.** The request returns **HTTP
  403**, not 404: the host refuses to serve the file rather than reporting it absent.
  FirmScout treats any 4xx on `robots.txt` as "the host published no restrictions" (RFC
  9309 §2.3.1.3, implemented in `RobotsCache.policyFor` in
  [`internal/adapters/fetch/robots.go`](internal/adapters/fetch/robots.go)), so at
  runtime this host is crawlable. The registered value stays `allowed` rather than
  `not_applicable`: `not_applicable` is for sources `robots.txt` does not govern, and
  this is an ordinary public HTTPS host that it does govern — the file is merely
  unavailable. Recording `not_applicable` would admit the source to the dispatchable set
  for a reason that is not the true one, which is a category error hiding a measurement.
  The 403 still deserves a human's attention: unlike a 404 it is an active refusal, and a
  conservative reading could treat it as an access policy in its own right.
- **MikroTik publishes no terms of use.** `/terms`, `/terms.html`, `/legal`,
  `/terms-of-use` and `/tos` all return 404. The sitemap's only legal document is
  `https://mikrotik.com/privacy`, whose headings are `Privacy Policy` and `Processing of
  personal data by MikroTik`, and which contains no occurrence of "crawl", "scrape",
  "automated", "robot", "reuse" or "redistribute". No document states a position on
  automated access or on reuse of published content.
- **`upgrade.mikrotik.com` publishes a rate-limit budget FirmScout ignores.** Every
  response now carries `x-ratelimit-limit` and `x-ratelimit-remaining` — measured `6066`
  and `6042` at 2026-09-05T18:19:57Z, live counters that will read differently on any
  other request. The fetcher honours `Retry-After` and 429 but reads no `X-RateLimit-*`
  header, so it can only discover the budget by exhausting it and earning the 429.
  **Known gap, recorded and not fixed here:** a source is publishing its budget and
  FirmScout is declining to read it.
- **The `NEWESTa7.stable` fixture is still faithful.** The endpoint returns the body
  `7.24.2 1788429434` with `last-modified: Thu, 03 Sep 2026 12:31:14 GMT`, unchanged from
  the 2026-09-03 measurement, so the recorded fixture still matches the live source.

**What is still a person's decision.** `terms_review_status` stays `pending` for both
MikroTik sources. The absence of a terms document is not the presence of permission, and
[ADR-0018](docs/adr/0018-source-compliance-policy.md) puts that judgement with a human
rather than with a measurement. A reviewer has to settle at least three things no
measurement can settle: whether "no published terms" means there is no contractual
restriction on automated access, or whether some other instrument reaches it; whether
MikroTik being a Latvian company puts the EU *sui generis* database right in play over
the changelog page, and if so whether copying version strings and dates is a substantial
extraction or de minimis factual data; and whether the 403 on
`upgrade.mikrotik.com/robots.txt`, taken together with the published rate-limit budget,
should be read as an access policy warranting more caution than RFC 9309 alone requires.

### Dell: compliance evidence measured 2026-09-05

One request was made to Dell in this pass, and it was the only one the policy permits.

- **`downloads.dell.com/robots.txt` disallows the catalogue.** Measured at 2026-09-05T19:45:10Z (the response's own `Date`), a plain GET with FirmScout's declared User-Agent and no credentials: HTTP 200, `content-type: text/plain`, `content-length: 64`, `last-modified: Mon, 12 Apr 2021 17:53:37 GMT`, `etag: "5a9e8fc3c42fd71:0"`. The body, in full:

  ```
  User-agent: *
  Allow: /manuals
  Allow: /topicspdf
  Disallow: /
  ```

  There is one group and it is the wildcard group, so it is the group that applies to FirmScout. Under RFC 9309 §2.2.2 the most specific match wins, and for `/catalog/Catalog.xml.gz` the only rule that matches is `Disallow: /` — neither `Allow` prefix matches the path at all. The verdict is `disallowed`, mechanically, with no judgment involved.
- **Even the permitted prefixes have left the host.** `https://downloads.dell.com/manuals/` answers HTTP 301 to `https://dl.dell.com/manuals/`. Both `Allow`-ed prefixes now redirect to a different registrable host, so `downloads.dell.com` effectively serves `robots.txt` and nothing else FirmScout may read.
- **The catalogue itself was not fetched.** Not a GET, not a HEAD, not a ranged request. "We only looked at the headers" is still a request to a path the host told us not to request. Everything that would normally be measured from the payload — its content type, its size, whether it serves an `ETag`, which products it covers — is therefore absent from this pass rather than guessed. The `~1.4 MB, ETag + Last-Modified` in the table above is the 2026-09-03 reading, kept with its original date and not refreshed.
- **No Dell product is registered, and that is deliberate.** [ADR-0018](docs/adr/0018-source-compliance-policy.md) says "the vendor, its products, and the catalogue source are all recorded in the registry". The vendor and the source are; the products are not, because establishing which products that catalogue covers means reading the catalogue. The domain already models this: `Source.ProductID` is documented as "empty when the source covers a family or a whole catalogue". A catalogue whose contents nobody is allowed to read is the strongest possible case for that field being empty.
- **The refusal is visible in the tool output, not only here.** `firmscout registry sync` prints: `dataset/sources/dell/catalog.yaml: source "catalog" will not be checked because it is not enabled, and the host's robots.txt disallows this path, and its terms of use have not been reviewed`. Three independent reasons, so no single edit turns it on by accident.

### Poly and HP: compliance evidence measured 2026-09-05

- **The relocation is real and it preserves the path.** `https://docs.poly.com/bundle/` answers HTTP 301 with `location: https://support.hp.com/bundle/`. So does every other path: `https://docs.poly.com/anything/deep/path` → `301` → `https://support.hp.com/anything/deep/path`. Only the bare root is additionally sent through HP's locale redirects (`https://docs.poly.com/` → `support.hp.com/` → `/br-pt/` → `/br-pt`, observed from a Brazilian egress address). `https://www.poly.com/` likewise ends on `https://www.hp.com/br-pt/poly.html`.
- **`robots_policy_status` is `allowed`, corrected from `unknown`.** `https://docs.poly.com/robots.txt` answers HTTP 301 to `https://support.hp.com/robots.txt`, which answers HTTP 200, `content-type: text/plain`, `content-length: 1882`, `last-modified: Thu, 14 May 2026 08:38:29 GMT`. It contains exactly one `User-agent` line — the wildcard — carrying `Crawl-delay: 1`, `Request-rate: 1/2`, sixteen `Disallow` prefixes (`/wps/portal`, `/wps/mycontenthandler/pps`, `/wps/hp-skp-portlets/render/`, `/wps/hp-skp-portlets/kaas/`, `/error/`, `/renderdocument/`, `/wcc-assets/images/`, `/wcc-assets/videos/`, `/wcc-services*`, `/*?*q=`, `/*?*search=`, `/*?*query=`, `/auth/`, `/login/`, `/logout/`, `/session/`) and two `Allow` rules for CSS and JS. None matches `/bundle/`. RFC 9309 §2.3.1.2 requires a crawler to follow a `robots.txt` redirect, and [`RobotsCache.fetch`](internal/adapters/fetch/robots.go) follows it deliberately — "a robots.txt that redirects to a CDN on another host is normal and is followed". The question was answered, so recording `unknown` would be the mirror image of the error this registry exists to prevent. The residual doubt is recorded rather than hidden: a cross-authority 301 means `docs.poly.com` has delegated its crawl policy to a host it does not obviously control, and that is a reviewer's call to revisit.
- **HTTP 200 from `support.hp.com` is not evidence a document exists.** `https://support.hp.com/bundle/g7500-release-notes/title-page` and the invented path `https://support.hp.com/anything/deep/path` both answer HTTP 200, `content-type: text/html`, `content-length: 7087`, `etag: W/"6a885636-1baf"`, `last-modified: Fri, 21 Aug 2026 13:44:22 GMT` — and the two bodies are byte-identical, `sha256 = 539f9cafcc98817e2eaadc28b9d675556410d0c890af0b1ecad08847ef21c6ae`. It is a single-page-application shell (`<base href="/">`, deferred scripts, a `<noscript>` fallback) served for every path and filled in by JavaScript afterwards. This is why the registered URL is the bundle root and not a release-notes document: registering one would assert a document this project cannot confirm.
- **The validator that is present lies.** That `ETag` and `Last-Modified` belong to the shell, not to the content beneath it. A conditional GET would answer `304` indefinitely while the release notes changed freely — a worse position than having no validator at all, because a fetcher would believe it.
- **No document URL is discoverable the other way round either.** HP's `robots.txt` declares eight sitemaps; the document index resolves to 980 legacy sitemaps and the microsite index to 43, and neither covers the `/bundle/` namespace.

### Ubiquiti: compliance evidence measured 2026-09-05

- **`fw-update.ubnt.com` serves no `robots.txt`.** Measured 2026-09-05T19:45:23Z: HTTP 404, `content-type: text/plain; charset=utf-8`, `content-length: 9`, body `Not Found`. RFC 9309 §2.3.1.3 makes an unavailable `robots.txt` a full allow and `RobotsCache.fetch` treats every 4xx that way, so `robots_policy_status` is `allowed`. It is `allowed` rather than `not_applicable` for the same reason argued for `upgrade.mikrotik.com`: `not_applicable` is for sources `robots.txt` does not govern, and this is an ordinary public HTTPS host that it does govern — the file is merely absent.
- **The endpoint has no cheap change signal at all.** `HEAD https://fw-update.ubnt.com/api/firmware-latest` (2026-09-05T19:48:17Z): HTTP 200, `content-type: application/json; charset=utf-8`, `content-length: 744990`, `vary: Accept-Encoding, Origin`, `access-control-allow-origin: *`, `access-control-expose-headers: WWW-Authenticate,Server-Authorization`, a fresh `x-request-id` per request — and no `ETag`, no `Last-Modified`, no `Cache-Control`, no `Accept-Ranges`. A GET carrying `Range: bytes=0-0` was answered HTTP 200 with the whole 744 990-byte body, so partial reads are not available either.
- **The payload is what makes it worth the bandwidth.** HAL JSON, `{"_embedded":{"firmware":[…]},"_links":{…}}`: 785 entries, 224 distinct `product` codes, 353 distinct `platform` values, channels `release` (615), `beta-public` (168), `lts` (1) and `release-cn` (1). Each entry carries `channel`, `created`, `file_size`, `id`, `md5`, `sha256_checksum`, `platform`, `product`, `probability_computed`, `tags.fullVersion`, `updated`, `version`, `version_major/minor/patch/prerelease` and `_links`; the most recent `updated` across the corpus was `2026-09-04T13:21:21Z`. The `created` and `updated` fields are RFC 3339 timestamps the *vendor* publishes, so a collector could record `exact_day` dates here without inferring anything — which is rarer than it sounds and is the real attraction. `version_major/minor/patch` are the vendor's own decomposition and are data, not a licence to order releases: version strings stay opaque, and "latest" remains release date then first observed.
- **`_links` leave the registered host.** `_links.self.href` points at `https://fw-update.ui.com/api/firmware/<id>` and `_links.data.href` at `https://fw-download.ubnt.com/data/…`. The cross-linking between `ubnt.com` and `ui.com` is the evidence for classifying `fw-update.ubnt.com` as `official_manufacturer`; it also means any follow-up fetch would be a decision about a different host, not a continuation of this one.
- **A question for the terms reviewer that MikroTik does not raise.** The response advertises `access-control-expose-headers: WWW-Authenticate`, so the API has an authenticated mode even though it served this request without credentials. Whether an endpoint that *can* authenticate but did not is "public" in the sense the terms mean is a judgement, not a measurement.

## Why Dell matters beyond one vendor

Dell's disallowed status is the clearest illustration of why compliance is a first-class field rather than an afterthought: on the 2026-09-03 reading it is the single most attractive source, technically, among the pilots — a well-formed, cacheable, ETag-bearing catalogue — and it is the one the project refuses to collect from until the policy conflict is resolved. If FirmScout is willing to bend this rule for its most convenient source, the rule doesn't mean anything for the next thousand sources.

That argument only carries weight if the refusal is a record rather than a claim, which is the whole reason Dell is *in* the registry instead of merely being written about here. The check anyone can run:

```
$ go run ./apps/cli registry validate
Registry is valid: 5 vendor(s), 7 category(ies), 0 family(ies), 2 product(s), 6 source(s).
0 of 6 source(s) are currently collectable; the rest await a compliance decision (ADR-0018).
```

A document asserting a refusal that the registry does not contain is exactly the failure this section warns about, one level up: it would be the project bending its own honesty rule about its own honesty demonstration. `dataset/sources/dell/catalog.yaml` is the artefact; this page is the commentary.

## Removal-request process

If a vendor asks that their data be removed or that FirmScout stop collecting from a specific source:

1. **The request is honoured promptly.** Open an issue (or, if the requester prefers privacy, use the security contact in [SECURITY.md](SECURITY.md) — vendor relations issues are handled with the same care as security reports) describing what was requested.
2. **The affected source(s) are set to `enabled = false` immediately**, pending any further conversation. Automated collection stops before the conversation about scope even finishes.
3. **Published facts already derived from that source are not silently deleted** — per the immutability principle, they are marked withdrawn with the removal request recorded as the reason and evidence, exactly like any other withdrawal. This preserves the audit trail (what did we publish, and why did it stop being published) without pretending the historical record never existed. If the vendor's request specifically requires deletion rather than withdrawal-with-reason, that's handled case by case and may need legal input — flag it clearly as such.
4. **The source's `robots_policy_status` or `terms_review_status` is updated** to reflect the new understanding, so the same conflict doesn't recur silently later.

## Attribution policy

Every published fact carries evidence pointing back to where it came from: the source URL, the retrieval timestamp, and (where the source class supports it) an excerpt. FirmScout does not present vendor-published information as if it originated with FirmScout — the official source link is a first-class, always-present field in the API and on the public site, not a footnote. For community-sourced or trusted-third-party data, the source and its quality class are shown alongside the fact, so a reader can tell "MikroTik says this" apart from "a community source we trust says this," and weight it accordingly.
