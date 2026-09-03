# Clean Architecture

> The deep dive behind [blueprint.md](blueprint.md) §7 and §8. Diagram: [`docs/diagrams/clean-architecture.md`](../diagrams/clean-architecture.md) (package dependency graph) and [`docs/diagrams/module-dependencies.md`](../diagrams/module-dependencies.md) (bounded-context view). This document explains the layering mechanically enough that a contributor can predict, before running the linter, whether an import they're about to add will fail CI.

## The four layers and which packages belong to each

```
domain  ←  application  ←  adapters  ←  platform (composition root)
```

| Layer | Packages | May import |
| --- | --- | --- |
| **Domain** | `internal/domain`, and its bounded-context sub-packages (`internal/domain/catalog`, `internal/domain/sourcing`, `internal/domain/ingestion`, `internal/domain/distribution`, `internal/domain/intelligence`) | Go standard library only |
| **Application** | `internal/application` and its sub-packages (`internal/application/catalog`, `.../sourcing`, `.../ingestion`, `.../distribution`, `.../intelligence`) — ports and use cases | `internal/domain` (any sub-package) + standard library |
| **Adapters** | `internal/adapters/postgres`, `.../httpapi`, `.../fetch`, `.../normalize`, `.../collectors`, `.../telemetry`, `.../ai` | `internal/application`, `internal/domain`, plus the adapter's specific third-party library (`pgx`, `goquery`, `otelhttp`, …) |
| **Platform** | `internal/platform` | Everything: domain, application, every adapter package |

`agents/` (JSON schemas, prompt registry) and `collectors/sdk`, `collectors/config`, `collectors/vendors` are not Go packages participating in this dependency graph in the same way — `collectors/sdk` is a public contract consumed by `internal/adapters/collectors`, and `agents/` is data (schemas), not code, in the MVP. See blueprint §10 for the deviation note on why bounded-context sub-packages live *inside* `domain/` and `application/` rather than as siblings.

## The dependency rule and how it is enforced

The rule is stated once, precisely, and checked mechanically rather than by convention:

- `internal/domain` (and sub-packages) may import **only the Go standard library**.
- `internal/application` (and sub-packages) may import **only `internal/domain` and the standard library**.
- `internal/adapters/*` may import `internal/application`, `internal/domain`, and their own third-party dependencies, but **may not import `internal/platform`**.
- **Only `internal/platform` may import everything** — it is the composition root: it wires concrete adapters into use cases and starts the three binaries' entry points (`apps/api/main.go`, `apps/worker/main.go`, `apps/cli/main.go`).

Two mechanisms enforce this, independently, so that a broken build and a broken test both catch the same violation:

1. **`internal/archtest`** — a Go test package that shells out to `go list -deps <package>` for each layer, computes the set of imported packages, and asserts the forbidden edges above do not appear. Because it is a `go test` target, it runs in the same `go test ./...` invocation as every other test and fails the suite the same way a broken assertion would — a contributor cannot merge a violation without either fixing it or deliberately deleting a test, which is a visible, reviewable act.
2. **`scripts/archcheck.sh`** — a standalone script performing the same `go list -deps` check outside the Go test binary, so the rule can be verified in a pre-commit hook, in CI as a distinct step (with its own clear failure message, not buried in test output), or by a contributor with no Go toolchain context loaded, by running one script.

```go
// internal/archtest/deps_test.go (sketch)
func TestDomainImportsOnlyStdlib(t *testing.T) {
    forbidden := importsOutsideStdlib(t, "github.com/macimottin/firmscout/internal/domain/...")
    if len(forbidden) > 0 {
        t.Fatalf("internal/domain imports outside the standard library: %v", forbidden)
    }
}

func TestApplicationImportsOnlyDomain(t *testing.T) {
    allowed := stdlibSet().Union(prefixSet("github.com/macimottin/firmscout/internal/domain"))
    forbidden := importsOutside(t, "github.com/macimottin/firmscout/internal/application/...", allowed)
    if len(forbidden) > 0 {
        t.Fatalf("internal/application imports outside domain+stdlib: %v", forbidden)
    }
}

func TestAdaptersDoNotImportPlatform(t *testing.T) {
    forbidden := importsMatching(t, "github.com/macimottin/firmscout/internal/adapters/...",
        "github.com/macimottin/firmscout/internal/platform")
    if len(forbidden) > 0 {
        t.Fatalf("adapter package imports internal/platform, which inverts the composition root: %v", forbidden)
    }
}
```

A rule that only lives in a code review checklist erodes the first time someone is in a hurry. `go list -deps` doesn't get tired.

## Ports and adapters

Ports are defined by the application layer, as interfaces it owns, because the application decides what it needs — not because a database happens to exist. The full port table from blueprint §7.3, expanded:

| Port | Method set (sketch) | Why it exists |
| --- | --- | --- |
| `VendorRepository` | `Get(ctx, id) (Vendor, error)`; `GetBySlug(ctx, slug) (Vendor, error)`; `List(ctx, ListOpts) ([]Vendor, error)`; `Save(ctx, Vendor) error` | Substitution for tests — an in-memory fake backs every application-layer test |
| `ProductRepository` | `Get`, `GetBySlug`, `ListByVendor`, `Save`, `ResolveAlias(ctx, string) (Product, bool, error)` | Substitution for tests |
| `SourceRepository` | `Get`, `ListDue(ctx, before time.Time) ([]Source, error)`, `Save`, `UpdateHealthState` | Substitution for tests; `ListDue` is the scheduler's read path |
| `ReleaseRepository` | `Get(ctx, id)`, `ListByProduct(ctx, productID, ListOpts)`, `Insert(ctx, Release) error`, `MarkNoLongerLatest(ctx, id) error` | Substitution for tests; note there is no `Update` — releases are append-only |
| `CandidateRepository` | `Insert(ctx, CandidateRelease) error`, `Get`, `Transition(ctx, id, to CandidateState) error` | Substitution for tests |
| `ReviewRepository` | `Insert(ctx, ReviewItem) error`, `ListPending(ctx, ListOpts) ([]ReviewItem, error)`, `Resolve(ctx, id, Decision) error` | Substitution for tests |
| `ArtifactStore` | `Put(ctx, content []byte) (ArtifactRef, error)`, `Get(ctx, ArtifactRef) ([]byte, error)` | Genuine multiple implementations: filesystem/PG large object locally, S3 on AWS |
| `Fetcher` | `Fetch(ctx, FetchRequest) (FetchResult, error)` where `FetchRequest` carries the prior `ETag`/`Last-Modified` | Substitution for tests (`httptest`-backed fake vs. real guarded client) |
| `Normalizer` | `Normalize(ctx, raw []byte, cfg NormalizeConfig) (normalized []byte, err error)`; `Hash(normalized []byte) string` | Substitution for tests; genuine multiple implementations per content type (HTML today, PDF/XML later) |
| `CollectorRegistry` | `Resolve(ctx, Source) (Collector, error)` | Substitution for tests; decouples the application from how collectors are loaded (YAML vs. compiled) |
| `JobQueue` | `Enqueue(ctx, Job) error`, `Dequeue(ctx, queue string) (Job, func() error /*ack*/, error)`, `MarkDead(ctx, id) error` | Genuine multiple implementations: PostgreSQL `SKIP LOCKED` today, SQS on AWS — same contract, different scaling envelope |
| `Clock` | `Now() time.Time` | Substitution for tests — deterministic time in every test that touches scheduling or evidence timestamps |
| `IDGenerator` | `New() string` (ULID) | Substitution for tests — deterministic, sortable IDs in fixtures |
| `UnitOfWork` | `Do(ctx, func(ctx context.Context) error) error` | Substitution for tests (a no-op fake); see below for why it's also a real architectural boundary, not only a test seam |
| `EventPublisher` | `Publish(ctx, ...Event) error` | Genuine multiple implementations: in-process + `analytics_events` locally, EventBridge on AWS |
| `UsageRecorder` | `Record(ctx, UsageEvent) error` | Substitution for tests |
| `APIKeyRepository` | `GetByHash(ctx, hash string) (APIKey, error)`, `Save`, `Revoke` | Substitution for tests |
| `QuotaStore` | `Consume(ctx, keyID string, n int) (allowed bool, remaining int, err error)` | Substitution for tests |
| `DiscoveryAgent`, `RepairAgent`, `ValidationAgent`, `ClassificationAgent`, `SourceQualityAgent` | Each: `Run(ctx, Input) (Output, AgentRun, error)` — see [ai-agents.md](ai-agents.md) for full schemas | Substitution for tests; **in the MVP, fakes are the only implementation that exists** |

**The persistence ports exist for testing, not for database portability.** FirmScout will not switch off PostgreSQL — see blueprint §7.9. The PostgreSQL adapter is explicitly free to use PostgreSQL-specific features: `SELECT ... FOR UPDATE SKIP LOCKED` for the queue, `tsvector`/`pg_trgm` for search, `JSONB` for flexible evidence fields, partial and expression indexes, and `LISTEN`/`NOTIFY` if a future feature needs it. The port's job is to let `internal/application` be tested in milliseconds with an in-memory fake, not to keep the door open for MySQL. Confusing the two purposes leads to writing repository interfaces in the lowest-common-denominator SQL dialect for a switch that will never happen — a cost paid daily for a benefit collected never.

Ports with a genuinely interchangeable second implementation (`ArtifactStore`, `JobQueue`, `EventPublisher`, `Fetcher`'s underlying transport) earn their abstraction twice over: they're a test seam *and* they're the reason the AWS deployment doesn't require rewriting `internal/application`.

## Transaction ownership through `UnitOfWork`

**The application layer owns transactions.** A repository never opens one, because a repository cannot know whether it is the entire operation or one step inside a larger one. A use case declares the atomic unit by wrapping its repository calls in `UnitOfWork.Do`.

### Worked example: `PublishRelease`

```go
func (uc *PublishRelease) Execute(ctx context.Context, cmd PublishReleaseCommand) (Release, error) {
    var published Release
    err := uc.uow.Do(ctx, func(ctx context.Context) error {
        release, err := uc.releases.Insert(ctx, cmd.toRelease())
        if err != nil {
            return err
        }
        if err := uc.releases.InsertMappings(ctx, release.ID, cmd.Mappings); err != nil {
            return err
        }
        if err := uc.evidence.Insert(ctx, cmd.Evidence); err != nil {
            return err
        }
        if err := uc.candidates.Transition(ctx, cmd.CandidateID, domain.CandidateStatePublished); err != nil {
            return err
        }
        if err := uc.summaries.Refresh(ctx, release.ProductID); err != nil {
            return err
        }
        if err := uc.audit.Insert(ctx, cmd.toAuditEvent(release.ID)); err != nil {
            return err
        }
        published = release
        return nil
    })
    if err != nil {
        return Release{}, err
    }
    uc.events.Publish(ctx, domain.ReleasePublished{ReleaseID: published.ID})
    return published, nil
}
```

Insert the release, insert the product mappings, insert evidence, transition the candidate, refresh the product summary, write the audit event: either all six happen or none do. The `pgx` adapter implements `UnitOfWork.Do` with `BEGIN` / `COMMIT` / `ROLLBACK` around a single connection, and injects that connection into the repositories it calls via `context.Context` so they participate in the same transaction transparently.

### Contrast: `CheckSource`

`CheckSource` is deliberately **not** wrapped in the same unit of work, because the middle of the operation is an HTTP request to a manufacturer's server — an action with no rollback semantics. If the write of the `source_checks` row failed after a successful fetch, re-running the transaction would mean fetching again, which is wasted egress and, worse, could trip a vendor's rate limit for no benefit. So `CheckSource` runs as two steps in sequence, not one transaction: (1) the fetch, entirely outside any database transaction, producing a `FetchResult` plus stored artifact; (2) a *separate*, short transaction that records the `source_checks` row, updates the source's health state, and enqueues the extraction job if the outcome was `changed`. If step 2 fails, the fetch is not repeated — the job is retried from "record the outcome" onward, using the artifact already stored in step 1.

## Domain invariants

Invariants live in constructors and state-transition methods, so an invalid object cannot exist in memory, not just cannot be persisted:

```go
package release

func NewRelease(product ProductRef, version version.VersionString, date partialdate.PartialDate,
    releaseType ReleaseType, ev evidence.Evidence) (Release, error) {
    if version.IsEmpty() {
        return Release{}, ErrEmptyVersion
    }
    if ev.IsZero() {
        return Release{}, ErrMissingEvidence
    }
    if releaseType == ReleaseTypeUnspecified {
        return Release{}, ErrReleaseTypeRequired
    }
    return Release{product: product, version: version, date: date, releaseType: releaseType, evidence: ev}, nil
}
```

```go
package partialdate

// Precision-specific accessors: Day() only compiles against ExactDay-precision values.
type PartialDate struct {
    precision Precision // ExactDay | MonthOnly | YearOnly | Unknown
    year, month, day int
}

func (d PartialDate) Day() (int, error) {
    if d.precision != ExactDay {
        return 0, fmt.Errorf("Day() called on a %s-precision date", d.precision)
    }
    return d.day, nil
}
```

```go
package candidate

func (c *Candidate) TransitionTo(to State) error {
    if !c.state.CanTransitionTo(to) {
        return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, c.state, to)
    }
    c.state = to
    return nil
}
// State machine forbids e.g. published -> discovered; only forward or explicit rejection edges exist.
```

```go
package source

func (s *Source) RecordCheck(outcome CheckOutcome) error {
    if s.state == StateRetired {
        return ErrSourceRetired // a retired source accepts no further checks
    }
    ...
}
```

```go
package product

func (p Product) Slug() string { return p.slug } // no setter — slug is immutable after construction,
                                                    // because it is a public URL and API identifier
```

## Domain events

Events are values produced by the domain and dispatched by the application through `EventPublisher` — never sent directly to a message broker from inside domain logic:

```go
package events

type ReleasePublished struct {
    ReleaseID  string
    ProductID  string
    OccurredAt time.Time
}

type SourceChanged struct{ SourceID string; PreviousHash, NewHash string }
type SourceFailed struct{ SourceID string; Reason string }
type CandidateCreated struct{ CandidateID string; SourceID string }
type CandidateValidated struct{ CandidateID string; Verdict string }
type CandidateRejected struct{ CandidateID string; Reason string }
type ReleaseWithdrawn struct{ ReleaseID string; Reason string }
type HumanReviewRequested struct{ ReviewItemID string; Priority string }
type SourceRelocated struct{ SourceID string; NewURL string }
```

In the MVP, `EventPublisher`'s adapter dispatches these in-process (synchronous handlers) and persists them to `analytics_events`, and — where an event implies follow-on work — enqueues a job via `JobQueue`. On AWS, the same `Event` values are marshalled and put on EventBridge by a different `EventPublisher` adapter. **The domain does not change.** This is the entire point of defining events as plain values the application dispatches, rather than as, say, an SQS `SendMessage` call made directly from a use case: the transport is an adapter concern, and swapping it is a platform-layer wiring change, not a rewrite.

## Error handling across layers

Three distinct error vocabularies, each owned by its layer, translated at the boundary between them:

- **Domain errors** are sentinel values or small typed errors describing a violated invariant: `ErrEmptyVersion`, `ErrInvalidTransition`, `ErrSourceRetired`. They know nothing about HTTP, SQL, or JSON.
- **Application errors** wrap domain errors with use-case context (`fmt.Errorf("publish release: %w", err)`) and introduce a small set of categories a presenter can map to a response: `NotFound`, `Conflict`, `Invalid`, `Unauthorized`, `RateLimited`. These categories exist so `internal/adapters/httpapi` can render RFC 9457 `problem+json` without knowing what a `Candidate` is.
- **Adapter errors** are library-specific and **must not leak past the adapter boundary**. The canonical example: `pgx.ErrNoRows` is a `pgx` concept. The PostgreSQL `ReleaseRepository` adapter converts it at the point of return:

```go
func (r *pgReleaseRepository) Get(ctx context.Context, id string) (release.Release, error) {
    row, err := r.q.GetRelease(ctx, id)
    if errors.Is(err, pgx.ErrNoRows) {
        return release.Release{}, apperr.NotFound("release", id) // application-layer error; pgx is gone
    }
    if err != nil {
        return release.Release{}, fmt.Errorf("get release %s: %w", id, err)
    }
    return mapRowToRelease(row), nil
}
```

`pgx.ErrNoRows` never reaches a use case. Neither does an HTTP status code from `Fetcher`'s underlying client, nor a `goquery` parse error from a collector — each adapter is responsible for translating its own library's failure vocabulary into the application's, at the point the adapter returns.

## Testing strategy per layer

| Layer | Test style | Dependencies allowed |
| --- | --- | --- |
| Domain | Table-driven unit tests | None — no DB, no network, no clock (inject a fixed time where needed) |
| Application | Use-case tests against in-memory fakes for every port | Fakes only |
| Adapters (PostgreSQL) | Integration tests against a real database | PostgreSQL from Compose; skipped with an explicit message if `FIRMSCOUT_TEST_DATABASE_URL` is unset |
| Adapters (HTTP fetch) | `httptest` servers | Local only |
| Collectors | Fixture-based extraction tests (`collectors/sdk/collectortest.RunFixtures`) | Recorded fixtures on disk |
| AI agents | Schema-validated fixed responses | Recorded JSON fixtures |
| API handlers | `httptest` with fake use cases | None |

**The rule that live manufacturer websites are never a test dependency is absolute, with no exceptions for "just this once" or "just to double-check the fixture is still accurate."** A collector's fixture is a frozen, dated, provenance-documented snapshot (`testdata/fixtures/mikrotik/README` records the URL, timestamp, and hash it was captured from); the fixture, not the live site, is what the test suite asserts against. A test suite that fails because Poly reorganised its documentation site is a test suite that teaches contributors to ignore CI failures — the fastest way to destroy the value of having tests at all. Refreshing a fixture is a deliberate, reviewed act (a new capture, a new README entry, a PR), never an implicit side effect of running `go test`.

## When this is overkill

Clean Architecture applied dogmatically to a small project produces ceremony without benefit. Expanding blueprint §7.9, candidly:

| Risk | Why it would happen here | Mitigation actually applied |
| --- | --- | --- |
| An interface per struct, most with exactly one implementation forever | The temptation is strongest around simple value objects | Ports are defined only where substitution is genuinely needed: persistence, network, queue, clock, AI. `VersionString`, `PartialDate`, `Evidence` have no interface — they're just types. |
| Mapping structs between four layers | A naive Clean Architecture tutorial has a DTO per layer | Domain entities are used directly by the application layer. Mapping happens only at the two real boundaries: sqlc rows → domain (inside the postgres adapter) and domain → API DTOs (inside `httpapi` presenters). |
| Use cases that are one-line pass-throughs to a repository | Wrapping every read in ceremony "for consistency" | **CQRS-lite exception**: simple read paths call a query port directly instead of being wrapped in a use case. `GetProduct` is a thin handler that calls `ProductRepository.GetBySlug` and maps the result to a DTO — no `GetProductUseCase` struct exists for it. The line: a read gets a use case only when it composes multiple sources, applies entitlement logic, or has a name a domain expert would recognise (`SearchProducts` — which blends full-text ranking, alias resolution, and tier-based history windowing — earns one; `GetProduct` does not). |
| Premature module or service splitting | The bounded-context table (blueprint §8.1) looks like a microservices roadmap | One Go module, one binary set. Package boundaries are enforced (`archtest`); deployment boundaries are not created until a measured need — see blueprint §3.4 for the Fargate threshold as the same discipline applied to infrastructure. |
| Abstracting the database "in case we switch" | The persistence-port table above looks, at a glance, like DB-portability plumbing | It isn't — see "Ports and adapters" above. The PostgreSQL adapter uses `SKIP LOCKED`, `tsvector`, `pg_trgm`, and `JSONB` freely. |

**The trade-off accepted, stated plainly:** roughly 15% more code than a straightforward layered design (blueprint §7.9), bought in exchange for a domain testable in milliseconds and an AI/cloud/queue strategy that can change without touching a business rule. That is a good trade for a project whose AI provider landscape and cost model will both change within a year. It would be a bad trade for a CRUD app that will never see a second adapter for anything.
