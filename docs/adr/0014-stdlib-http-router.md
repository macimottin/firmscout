# ADR-0014: Standard library `net/http` router, no framework

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** founding team
- **Requires qualified legal review:** no
- **Related:** ADR-0001, ADR-0002

## Context

The brief asked for an evaluation of Chi, Gin, and Fiber as HTTP framework candidates for the public API (§3.2). Historically, choosing a framework over the standard library `net/http` package was mostly about routing ergonomics — matching a path pattern with method and wildcard support required either hand-rolled logic or a third-party router. That calculus changed with **Go 1.22**, which added method- and pattern-based routing directly to `http.ServeMux` (`mux.HandleFunc("GET /products/{slug}", handler)`), removing the single biggest reason to reach for a router library. What remains as a framework's potential value is middleware ergonomics — a small amount of composition logic — which is not a large amount of code to write directly.

FirmScout's security posture depends on a small, auditable dependency tree (§3.2) — every third-party dependency is attack surface, supply-chain risk, and something a self-hoster or security-conscious contributor has to trust or audit. A framework dependency for routing convenience alone is a cost that needs to earn its place against that backdrop, and ADR-0011 already commits to `otelhttp` for observability instrumentation, which needs to compose cleanly with whatever routing choice is made.

## Decision

FirmScout's HTTP API uses the standard library's `net/http.ServeMux` with Go 1.22+ method-and-pattern routing, and a small, explicit middleware chain (request ID assignment, OpenTelemetry instrumentation via `otelhttp`, optional API key authentication, rate limiting, quota enforcement, cache headers, and RFC 9457 `application/problem+json` error formatting) written directly in the project, estimated in the blueprint at roughly 40 lines of composition logic — the actual value Chi would have added, without the dependency.

## Consequences

### Positive

- Zero framework dependency in the HTTP layer: nothing beyond `net/http` and, for instrumentation, `otelhttp`, keeps the dependency tree small and auditable, directly supporting the security posture the project has committed to.
- `otelhttp` instruments `net/http` handlers directly with no adapter shim — Gin and Fiber's non-standard context and handler types would each need a translation layer to integrate cleanly with OpenTelemetry, adding indirection ADR-0011's observability stack does not need with the stdlib router.
- Handlers are testable with `httptest` with no framework test harness required — a contributor writing a new endpoint test needs no framework-specific knowledge, just `net/http/httptest`, which is stdlib and already familiar to any Go developer.
- Contributors need no framework-specific knowledge to add or review an HTTP handler, lowering the bar for the collector- and API-contributor population the project is trying to grow (assumption P5).
- Fiber specifically does not use `net/http` at all (it is built on `fasthttp`), which would forfeit the entire standard middleware ecosystem and every stdlib-compatible tool (including `otelhttp`) — this was a deciding factor in ruling it out even before considering Gin.

### Negative

- The ~40 lines of hand-rolled middleware composition, while small, is code the project owns, tests, and maintains itself rather than code maintained by a framework community — a subtle bug in the custom middleware chain (e.g., an ordering issue between rate limiting and authentication) is the project's own responsibility to find, not a framework's already-battle-tested default behaviour.
- `http.ServeMux`'s pattern routing, while sufficient for FirmScout's relatively small and flat API surface (§12's endpoint table), has a simpler pattern-matching feature set than a mature third-party router accumulated over years of edge-case handling (e.g., some advanced wildcard or route-precedence semantics some frameworks offer) — this is judged acceptable given the API's actual shape, but is a real capability ceiling if the API surface grows into something with more complex routing needs.
- New contributors coming from other Go projects that conventionally use Chi or Gin may expect a framework and need a short orientation to the project's stdlib-only approach, though this is a minor friction compared to the framework-specific knowledge those same contributors would otherwise need to unlearn.

### Neutral

- This decision is scoped specifically to routing and middleware composition for the HTTP API; it says nothing about the Next.js web application's own internal architecture, which is a separate, unrelated technology stack (ADR scope: Go API only).

## Alternatives considered

### Chi

The closest alternative, and the one most seriously considered. Chi is signature-compatible with `net/http` (its handlers are ordinary `http.Handler`s), so it composes with `otelhttp` without a shim and would not have introduced the non-standard-types problem Gin and Fiber do. It was still not chosen because, since Go 1.22, its main remaining value — path-pattern and method routing with wildcards — is now in the standard library, and Chi's middleware-composition convenience is a genuinely small amount of code (§3.2 estimates roughly 40 lines) to write directly rather than take on as a dependency. The **cost of having chosen wrong is explicitly judged low**: because Chi is `net/http`-signature-compatible, adopting it later, if the hand-rolled middleware chain becomes unwieldy, is a mechanical, low-risk change — this is one of the few ADRs in this set where the "alternatives considered" section doubles as an explicit low-regret escape hatch.

### Gin

Rejected. Gin's `gin.Context` is a custom type that handlers are written against instead of `http.Handler`/`http.ResponseWriter`, which means every handler, every middleware, and every test in the codebase is written against Gin's API rather than the standard library's — a much larger commitment than Chi's compatible approach, for routing convenience the standard library now provides natively. It also complicates `otelhttp` integration, which expects standard `http.Handler`s.

### Fiber

Rejected, and rejected more decisively than Gin. Fiber is built on `fasthttp` rather than `net/http`, which means it does not merely add a custom context type on top of the standard library — it replaces the standard library's HTTP implementation entirely. This forfeits `net/http`-compatible middleware (including `otelhttp`) altogether and would require Fiber-specific alternatives for every piece of standard tooling the project would otherwise get for free, for no capability FirmScout's API actually needs.

## Revisit when

- The hand-rolled middleware chain grows complex enough (measured by contributor friction, bug reports specific to middleware ordering or composition, or a genuine need for a routing feature `http.ServeMux` cannot express) that adopting Chi's signature-compatible convenience becomes clearly worth the dependency — this is the low-regret path this ADR explicitly leaves open.
- The API surface grows in a way that needs routing capabilities beyond method-and-pattern matching (e.g., complex route groups, versioned sub-routers with independent middleware stacks at a scale the current approach does not express cleanly).
