# Request and scraping protection flow

This diagram answers: when a request arrives at FirmScout's edge, what sequence of checks decides whether it is served, throttled, challenged, or blocked, and why does the answer depend on which endpoint class it targets rather than on a single global limit?

```mermaid
flowchart TD
    Request["Incoming request"] --> CF["CloudFront"]
    CF --> WAF["WAF: managed rule groups, IP reputation, rate rules"]
    WAF -->|"blocked at edge"| EdgeBlock["Blocked at edge (logged)"]
    WAF -->|"passed"| Classify{"Request classification"}

    Classify -->|"verified crawler"| ConsumerCrawler["Verified crawler"]
    Classify -->|"anonymous browser"| ConsumerAnon["Anonymous browser"]
    Classify -->|"free API key"| ConsumerFree["Free API key"]
    Classify -->|"paid API key"| ConsumerPaid["Paid API key"]
    Classify -->|"internal"| ConsumerInternal["Internal service"]

    ConsumerCrawler --> EndpointClass{"Endpoint class"}
    ConsumerAnon --> EndpointClass
    ConsumerFree --> EndpointClass
    ConsumerPaid --> EndpointClass
    ConsumerInternal --> EndpointClass

    EndpointClass -->|"public HTML"| PolHTML["Policy: generous, cache-first"]
    EndpointClass -->|"static assets"| PolAssets["Policy: very generous, long cache"]
    EndpointClass -->|"internal frontend API"| PolInternalAPI["Policy: scoped to web origin"]
    EndpointClass -->|"free API"| PolFreeAPI["Policy: small persistent quota"]
    EndpointClass -->|"paid API"| PolPaidAPI["Policy: plan-based quota"]
    EndpointClass -->|"auth endpoints"| PolAuth["Policy: tight, never cached"]
    EndpointClass -->|"search endpoints"| PolSearch["Policy: moderate, circuit breaker on cost"]
    EndpointClass -->|"bulk endpoints"| PolBulk["Policy: strict, paid tiers only"]

    PolHTML --> CacheLookup["Cache lookup"]
    PolAssets --> CacheLookup
    PolInternalAPI --> CacheLookup
    PolFreeAPI --> CacheLookup
    PolPaidAPI --> CacheLookup
    PolAuth --> CacheLookup
    PolSearch --> CacheLookup
    PolBulk --> CacheLookup

    CacheLookup -->|"hit"| ServeCacheDirect["Serve cached response"]
    CacheLookup -->|"miss"| RiskScore["Abuse risk score"]

    RiskScore --> RateLimit{"Layered rate limit: WAF rules, in-process token bucket, PostgreSQL quota"}

    RateLimit -->|"within limit, low risk"| Allow["Allow"]
    RateLimit -->|"within limit, elevated risk"| AllowMonitor["Allow and monitor"]
    RateLimit -->|"cache-eligible, near limit"| ServeCachedFallback["Serve cached"]
    RateLimit -->|"over soft limit"| ReduceRate["Reduce rate"]
    RateLimit -->|"missing credentials on gated endpoint"| RequireAuth["Require auth"]
    RateLimit -->|"suspicious pattern"| Challenge["Accessible challenge"]
    RateLimit -->|"over hard limit"| TempBlock["Temporary block"]
    RateLimit -->|"repeated abuse, reviewed"| PermBlock["Permanent block after review"]

    Allow --> App["Application"]
    AllowMonitor --> App
    ServeCachedFallback --> App
    ReduceRate --> App
    RequireAuth --> App
    Challenge --> App
    ServeCacheDirect --> App

    App --> Log["Usage and security logging"]
    TempBlock --> Log
    PermBlock --> Log
    EdgeBlock --> Log
```

## What this shows

Two independent axes determine treatment: **who is asking** (verified crawler, anonymous browser, free key, paid key, internal service) and **what they are asking for** (public HTML, static assets, internal frontend APIs, free API, paid API, auth endpoints, search endpoints, bulk endpoints). There is no single global rate limit — each endpoint class has its own policy, evaluated after classification and before the shared cache and risk-scoring stages. The response ladder is graduated: allow, allow and monitor, serve cached, reduce rate, require auth, an accessible challenge, a temporary block, and — only after human review of the evidence — a permanent block. Every terminal path, including edge blocks, reaches usage and security logging.

## Assumptions

- WAF managed rule groups and IP reputation lists catch high-confidence automated abuse before it reaches the application, at negligible marginal cost.
- Request classification (crawler / anonymous / free key / paid key / internal) is resolved from verifiable signals (API key, verified bot user-agent plus reverse DNS, mTLS or VPC source for internal) rather than trusted self-declaration.
- The abuse risk score is a lightweight, explainable heuristic (request pattern, header consistency, historical behaviour), not an opaque ML classifier in the MVP.
- "Accessible challenge" means a challenge that does not depend on vision or fine motor control (per O1/O2 in the blueprint) — never a CAPTCHA that blocks legitimate accessibility tools or well-behaved crawlers.
- Permanent blocks always require a human review step; nothing is permanently blocked by an automated decision alone.

## Failure modes

- Over-aggressive classification misfiles a legitimate paid API consumer as anonymous, applying the wrong (stricter) policy — mitigated by always trusting a valid API key signature over any other heuristic.
- WAF rate rules tuned for one endpoint class bleed into another if rules are not scoped per path — this diagram exists specifically to keep that scoping visible and auditable.
- A challenge step that is not accessible becomes a de facto block for legitimate users, which is the failure this design explicitly avoids (O1).
- Risk scoring based on stale reputation data allows an abusive IP through after it changes behaviour, or blocks a shared corporate NAT egress unfairly — both require the "allow and monitor" and "reduce rate" middle tiers rather than jumping straight to a block.

## Related ADRs

- [ADR-0008: scraping resilience](../adr/0008-scraping-resilience.md)
- [ADR-0007: public web, paid API](../adr/0007-public-web-paid-api.md)
- [ADR-0013: cost minimisation](../adr/0013-cost-minimization.md)

## Implementing code

**Partially implemented.**

- `internal/adapters/httpapi/middleware.go` — request classification and the response path
- `internal/adapters/httpapi/ratelimit.go` — per-endpoint-class token buckets

There is no edge: no CloudFront, no WAF, and no abuse scoring. Rate limiting is per instance.

See [the architecture consistency report](../architecture/consistency-report.md) for what is verified by execution and what is not.
