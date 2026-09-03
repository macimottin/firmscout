# API request sequence

This diagram answers: what is the exact sequence of calls a single API request makes through the platform, and what changes when a consumer has exhausted their quota?

```mermaid
sequenceDiagram
    participant Consumer as API consumer
    participant CF as CloudFront
    participant WAF as WAF
    participant GW as API Gateway
    participant Handler as API handler
    participant Auth as Auth and API key verification
    participant Quota as Quota check
    participant Cache as Cache
    participant DB as PostgreSQL
    participant Meter as Usage meter
    participant Billing as Billing event sink

    Consumer->>CF: GET /api/v1/products/{slug}
    CF->>WAF: Forward request
    WAF-->>CF: Passed edge rules
    CF->>GW: Forward request
    GW->>Handler: Invoke handler
    Handler->>Auth: Verify API key (if present)
    Auth-->>Handler: Consumer identity and plan

    Handler->>Quota: Check quota for consumer and endpoint

    alt Quota available
        Quota-->>Handler: Within quota
        Handler->>Cache: Look up cached response
        alt Cache hit
            Cache-->>Handler: Cached payload
        else Cache miss
            Handler->>DB: Query product summary
            DB-->>Handler: Row(s)
            Handler->>Cache: Store response
        end
        Handler->>Meter: Record usage event
        Meter->>DB: Persist usage record
        Meter->>Billing: Emit billable usage event
        Handler-->>GW: 200 OK with data
        GW-->>CF: 200 OK
        CF-->>Consumer: 200 OK (ETag, Cache-Control, rate-limit headers)
    else Quota exceeded
        Quota-->>Handler: Quota exceeded
        Handler->>Meter: Record rejected request
        Meter->>DB: Persist usage record (rejected)
        Handler-->>GW: 429 Too Many Requests (problem+json)
        GW-->>CF: 429 Too Many Requests
        CF-->>Consumer: 429 Too Many Requests (Retry-After)
    end
```

## What this shows

The full round trip for one read endpoint, including the two paths that matter most for the commercial model: a normal response, and a quota-exceeded rejection. Both paths still write a usage record — rejected requests are metered too, because they are the evidence a consumer needs to see their own quota state and the evidence FirmScout needs to size plans correctly. Caching sits inside the handler's control flow, after entitlement is confirmed and before the database is touched, so an over-quota consumer never reaches PostgreSQL at all.

## Assumptions

- API key verification is fast enough to run on every request without a network round trip beyond PostgreSQL (a hashed key lookup, per §11 of the blueprint).
- Quota checks read from durable PostgreSQL counters, not from an in-memory approximation, because quota correctness has billing consequences (§3.9).
- Anonymous requests (no API key) still pass through the same sequence with a default low-tier quota; there is no separate code path for anonymous traffic.
- The billing event sink in the MVP is the same `analytics_events`/usage-record mechanism used elsewhere; a dedicated billing platform integration is a later adapter, not a new sequence.
- Cache invalidation on publish is handled elsewhere (§16 of the blueprint); this sequence only shows lookup and store.

## Failure modes

- A quota check that races with a burst of concurrent requests from the same key can allow a small overshoot; the PostgreSQL counter update should be atomic (`UPDATE ... RETURNING`) to bound this to one request's worth of slack.
- If the usage meter write fails after a successful response has already been sent, usage undercounts for that request — this must be logged as a metering gap, never silently dropped, since it affects both product analytics and billing.
- A cache store that succeeds but a database read that partially failed could cache an incomplete response; the handler must only cache after a fully successful read.
- If the billing event sink is unavailable, request handling must not block on it — usage recording to PostgreSQL is the durable source of truth, and forwarding to the billing sink can retry asynchronously.

## Related ADRs

- [ADR-0007: public web, paid API](../adr/0007-public-web-paid-api.md)
- [ADR-0014: stdlib HTTP router](../adr/0014-stdlib-http-router.md)
- [ADR-0013: cost minimisation](../adr/0013-cost-minimization.md)

## Implementing code

**Partially implemented.**

- `internal/adapters/httpapi/router.go`, `middleware.go`, `handlers.go`
- `internal/application/queries.go` — entitlement and usage recording

Billing events are not emitted.

See [the architecture consistency report](../architecture/consistency-report.md) for what is verified by execution and what is not.
