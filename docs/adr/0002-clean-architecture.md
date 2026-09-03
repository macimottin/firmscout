# ADR-0002: Clean Architecture with mechanically enforced inward dependencies

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** founding team
- **Requires qualified legal review:** no
- **Related:** ADR-0001, ADR-0004, ADR-0015

## Context

FirmScout's domain concepts (Vendor, Product, Source, CandidateRelease, Release, Evidence, VersionString, PartialDate, ReleaseType, scheduling policy) are expected to outlive several implementation choices that will change faster than the business rules: the database (PostgreSQL today, but the port exists for testability, not portability), the AI provider (none in the MVP, one or several later), the queue backend (PostgreSQL `SKIP LOCKED` first, SQS later), and the cloud runtime (Lambda first, possibly Fargate later). A design that lets any of these leak into domain types would make every one of those later changes a domain-touching change, and would make domain logic untestable without spinning up the infrastructure of the day.

At the same time, Clean Architecture applied without judgment is a well-known way to turn a small project into a maze of one-implementation interfaces and four-layer struct mapping. The blueprint is explicit that this risk is real (§7.9) and this ADR is written to be honest about that trade-off rather than to sell the pattern uncritically.

## Decision

Dependencies point inward only: `domain ← application ← adapters ← platform`. `internal/domain` imports the standard library only — no HTTP status codes, no SQL, no AWS ARNs, no selectors, no token counts, nothing whose definition is owned by an external system (§7.1). `internal/application` imports `domain` and the standard library, and it **owns** every port it needs, defined as interfaces in the application package, not in the adapter that implements them. `internal/adapters/*` import `application` and `domain` plus whichever third-party library that specific adapter wraps (pgx, goquery, the AWS SDK). `internal/platform` is the only package allowed to import all of the above, because composition — building the concrete graph of adapters behind the ports a given binary needs — is its entire job.

The application layer owns transaction boundaries through a `UnitOfWork` port (§7.5): a use case declares what is atomic, and only the adapter implementing that port knows what a transaction actually is. Repositories never open transactions themselves. Domain invariants live in constructors and state-transition methods so that invalid objects cannot be constructed at all — a `PartialDate` with `month_only` precision has no accessor for a day component; a `CandidateRelease` cannot express `published → discovered`.

The rule is enforced **mechanically**, not by review convention: `internal/archtest` shells out to `go list -deps` for each package and fails the build if a forbidden import edge exists — for example, if `internal/domain` ever imports `internal/adapters/postgres`, or if a bounded context imports an adapter package directly instead of a port. A rule that only lives in a document is a comment, not a control.

## Consequences

### Positive

- Domain and application logic is tested in milliseconds with in-memory fakes and zero network, zero database, and a fake clock — the release-publication rules, version plausibility checks, and scheduling policy are exercised without Docker running.
- The AI provider, the queue backend, and the cloud runtime can each change without a domain-level pull request, because none of them is visible from `domain` or `application` in the first place. This matters concretely for FirmScout: the AI provider landscape and the cost model are both expected to change within a year.
- Collector adapters are structurally incapable of publishing a release — they return `[]CandidateRelease` and are never handed a repository. The architectural boundary implements the brief's "collectors cannot publish" requirement as a type constraint, not a code-review reminder that can be missed.
- A violation of the dependency rule fails CI immediately and points at the offending import, rather than surfacing as a subtle coupling discovered months later during a database migration.

### Negative

- There is real ceremony: mapping happens at the two genuine boundaries (sqlc rows → domain entities, domain → API DTOs), and every port needs at least a fake implementation for tests even when, at MVP scale, it will only ever have one real implementation. The blueprint's own estimate is roughly 15% more code than a straightforward layered design (§7.9).
- The pattern invites over-abstraction if applied without discipline: an interface per struct, most of which will have exactly one implementation for the project's entire lifetime, is a failure mode this ADR explicitly rejects (see below) but that a future contributor unfamiliar with the reasoning could reintroduce.
- `internal/archtest`'s `go list -deps` check is a coarse instrument: it catches import-graph violations but cannot catch a business rule that has quietly migrated into an adapter without an obviously wrong import (for example, a repository implementation deciding *whether* a release is publishable instead of just persisting the decision made by a use case). That still requires code review.
- New contributors unfamiliar with Clean Architecture face a learning curve before they can place code correctly; the layer names and port/adapter vocabulary are not self-explanatory without onboarding material.

### Neutral

- This ADR is deliberately paired with an explicit "risks of overengineering" review (§7.9) rather than presented as costless. The trade-off is judged worthwhile specifically because of expected AI-provider and cloud-runtime churn, not as a general endorsement of the pattern for any project.

## Alternatives considered

### Straightforward layered architecture (handler → service → repository, no explicit ports)

Rejected as the primary structure, though it is close to what FirmScout's simple read paths already do informally (CQRS-lite: `GetProduct` calls a query port directly rather than going through a full use-case object). A pure layered design without owned ports would let a repository's error types or SQL-specific concepts leak upward into services more easily, and would not give collectors a structural inability to publish — that guarantee specifically requires the port to be owned by the application layer and typed to exclude a repository handle.

### Hexagonal/ports-and-adapters without the "Clean" layering vocabulary

This is, in substance, very close to what was chosen — Clean Architecture as implemented here is hexagonal architecture with an explicit inward-dependency rule and named layers. The distinction is largely terminology; the decision is really about depth of layering (domain/application/adapters/platform) and where mapping boundaries sit, both of which are fixed in this ADR regardless of which name is used.

### No enforced boundary; convention and code review only

Rejected. The dependency rule is the entire value of this pattern; without mechanical enforcement it degrades under time pressure exactly when it matters most — a deadline-driven shortcut that imports `pgx` into a use case is easy to miss in review and easy to justify in the moment. `internal/archtest` costs a small, one-time implementation effort and removes this failure mode permanently.

## Revisit when

- `internal/archtest` needs to allow an exception more than a handful of times in a quarter, suggesting the layer boundaries are drawn in the wrong place rather than that the rule is wrong.
- A genuinely single-implementation-forever port accumulates enough interface/implementation pairs with no test-double benefit that the ceremony cost measurably slows contributors — tracked qualitatively via contributor feedback and PR review time, not a hard number.
- The team concludes, with evidence from actual churn, that the AI provider or cloud runtime is *not* changing as expected and the isolation this ADR bought has gone unused for over a year.
