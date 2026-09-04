# ADR-0019: The API is served from `api.firmscout.dev`, keeping the `/api/v1` path prefix

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** founding team
- **Requires qualified legal review:** no
- **Related:** ADR-0007, ADR-0008, ADR-0010

## Context

FirmScout serves two audiences from one system: a human reading a product page in a
browser, and a program calling a JSON API. Both sit behind the same CloudFront
distribution ([aws-deployment.md](../architecture/aws-deployment.md)), and the question
this ADR settles is what URL each audience uses.

The two candidate shapes are a path on one domain (`firmscout.dev/api/v1/...`) and a
dedicated subdomain (`api.firmscout.dev/...`).

This needed deciding now, not later, for a reason that has nothing to do with elegance:
**an API base URL is an identifier consumers hardcode.** Changing it after publication
breaks every integration built against it, and the cost of the change grows with
adoption. It is one of the few decisions in this project that is genuinely expensive to
reverse.

It also needed deciding because the repository had already made the choice by accident
and in only one place. `docs/api/openapi.yaml` declared
`https://api.firmscout.dev/api/v1` as its production server, while
`docs/architecture/aws-deployment.md` described a single distribution routing by path
and `docs/architecture/api.md` never named a host at all. A machine-readable contract
that asserts something the architecture documents do not is worse than an open question,
because tooling generated from it inherits a decision nobody made.

## Decision

**The API is published at `https://api.firmscout.dev`, and the `/api/v1` path prefix is
retained**, so the production base URL is `https://api.firmscout.dev/api/v1`.

**One CloudFront distribution serves both names.** The ACM certificate covers
`firmscout.dev`, `www.firmscout.dev` and `api.firmscout.dev`; all three are aliases on
the same distribution, so there is one WAF web ACL, one certificate, and one edge
configuration to keep correct. CloudFront behaviours match on path, so a request to
either host for `/api/v1/*` reaches the API origin.

**The path prefix stays even though it is redundant in the production URL.** This is
deliberate. `http://localhost:8080/api/v1/products/{slug}` and
`https://api.firmscout.dev/api/v1/products/{slug}` name the same route, so the Go router
is byte-for-byte identical in every environment and no layer strips or adds a prefix
depending on where it is deployed. Environment-dependent path rewriting is a reliable
source of the kind of bug that only appears in production, and avoiding it is worth more
than the cosmetic tidiness of `api.firmscout.dev/v1`.

**The apex serves the website.** `firmscout.dev/api/v1/*` also resolves, because the
behaviour matches on both aliases, but it is not the documented base URL and is not what
OpenAPI advertises. A later CloudFront Function may redirect it to the subdomain to make
the canonical form unambiguous; that is a refinement, not a prerequisite.

## Consequences

### Positive

- **The API can move without breaking consumers.** ADR-0010 documents a measurable
  condition under which the API tier moves from Lambda to ECS Fargate. With a dedicated
  hostname, that is a DNS and origin change. With a path on the apex, it would mean
  either splitting one distribution's behaviours across two very different origins or
  changing the URLs consumers depend on. This is the single strongest argument, and it
  is not hypothetical: the threshold is already written down.
- **Cookie isolation.** A cookie scoped to `firmscout.dev` is sent on every request to
  every path under it, including `/api/v1`. The roadmap includes a customer usage
  dashboard, which implies an authenticated browser session. Putting the API on a
  sibling host keeps session cookies off API requests entirely, which removes a
  browser-driven CSRF surface and keeps a cookie out of the CDN cache key. Getting this
  wrong interacts badly with the cache-key confusion threat (T-13 in
  [security.md](../architecture/security.md)).
- **Per-audience edge policy is expressible at the hostname**, in addition to the
  per-path-class policies in [scraping-resilience.md](../architecture/scraping-resilience.md),
  which gives one more coarse lever without inventing a new mechanism.
- The shape matches what API consumers expect, which is worth something for a product
  whose paid tier is an API.

### Negative

- **A second name to keep correct** in DNS, in the certificate, and in whatever
  eventually manages them. Small, but non-zero, and it is a name that will be wrong at
  some point in some environment.
- **Same-origin is lost for browser-based calls.** This costs nothing today, because the
  website is server-rendered and its data fetching happens server-to-server where CORS
  does not apply. It would cost a CORS configuration the day any browser-side JavaScript
  calls the API directly. That configuration is a known quantity, but it is a thing that
  would not otherwise exist.
- **The production base URL reads redundantly** (`api.` followed by `/api/`). Accepted
  in exchange for identical routing in every environment.

### Neutral

- Nothing in `internal/domain`, `internal/application`, or the HTTP handlers changes.
  The router already mounts `/api/v1`, and the host it is reached by is not the
  application's concern.
- The local development URL is unaffected: `http://localhost:8080/api/v1`.

## Alternatives considered

### A path on the apex domain: `firmscout.dev/api/v1`

The simpler-looking option, and the one initially favoured on the grounds that the
website is a client of its own API and same-origin avoids CORS.

Rejected once that reasoning was examined. **The CORS argument does not apply here**:
the site's data fetching happens in Server Components, server-to-server, where the
browser's same-origin policy is not involved. The argument survived only as long as
nobody checked which side of the wire the fetch happens on.

What remained were the two arguments against: a base URL that cannot move without
breaking consumers, and apex cookies riding along on every API request. Neither is
urgent today, and both are expensive to fix after the first integration exists.

### A separate CloudFront distribution for the API

Rejected. Two distributions means two WAF web ACLs, two sets of cache behaviours and two
certificates to keep in agreement, and the one that falls out of sync is the security
hole. The stated benefit — independent edge configuration — is already available through
behaviours and hostname matching on a single distribution.

### Versioning in the hostname: `v1.api.firmscout.dev`

Rejected. It moves a versioning concern into DNS, where it is slower to change and
harder to run two versions side by side, and it buys nothing over a path segment.
[api.md](../architecture/api.md) already specifies the versioning and deprecation policy
in terms of the path.

### Deferring the decision

Rejected, and this is the reason the ADR exists at all. The decision had already been
made implicitly, in a machine-readable file, without being written down or reconciled
with the architecture documents. Deferring a decision that a generated client will
silently inherit is not deferring it.

## Revisit when

- **Any browser-side JavaScript needs to call the API directly**, at which point a CORS
  policy has to be designed rather than assumed, and the trade re-examined with that
  cost included.
- **The API tier actually moves off Lambda**, which is when the flexibility this ADR
  buys is either used or shown to have been unnecessary.
- **A second public API audience appears** with genuinely different edge requirements —
  a bulk export or a webhook receiver, say — where a third hostname might be clearer
  than a fourth path class.
- **Anyone proposes dropping the `/api/v1` prefix from the production URL.** That is
  defensible on aesthetics and should be weighed against reintroducing
  environment-dependent path handling, which is what the prefix currently prevents.
