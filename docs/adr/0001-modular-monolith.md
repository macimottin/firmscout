# ADR-0001: Modular monolith, one Go module, three binaries

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** founding team
- **Requires qualified legal review:** no
- **Related:** ADR-0002, ADR-0010, ADR-0015

## Context

FirmScout needs to run three distinct processes: a public HTTP API, a background worker that schedules and executes source checks, and a CLI for operational tasks (migrations, registry sync, ad hoc source checks, API key management). It also needs a Next.js public web app. The brief invited a broader services conversation, but at the MVP's expected scale — four pilot vendors, a few hundred sources, low request volume — a distributed-services architecture solves problems FirmScout does not yet have, at the cost of problems it would then have on day one: service discovery, distributed tracing across process boundaries beyond what a monolith already needs, independent deployment coordination, and a larger surface for a two-person team to operate.

The product's own economics reinforce this. FirmScout's differentiator is a low cost per monitored source, and the primary cost drivers at this scale are egress, always-on compute, and AI tokens — not lack of horizontal service decomposition (§13, ADR-0013). Splitting into services before there is a measured reason to would spend engineering time on infrastructure instead of on the collector coverage and data quality that the product actually needs.

## Decision

FirmScout ships as **one Go module** containing **three binaries** — `apps/api`, `apps/worker`, `apps/cli` — plus a separate `apps/web` Next.js application with its own `package.json` in the same monorepo. All three Go binaries share `internal/domain`, `internal/application`, and `internal/adapters`; each binary's `main.go` is limited to wiring, living under `internal/platform`. The domain is organised into five bounded contexts implemented as **packages, not services**: Catalog, Sourcing, Ingestion, Distribution, and Intelligence (§8.1). Each context declares what it owns, what it may read, and — critically — what it may not write to (§8.2): Sourcing cannot write releases, Distribution cannot write catalogue or fact data, Intelligence cannot write to `releases`, `sources`, or `products` directly.

These context boundaries are the seam for future service extraction. If a context ever needs independent scaling, deployment cadence, or a different runtime, its package boundary is where a service boundary would be drawn — because its dependency edges are already constrained to what a service API would need to expose. No context is designed today as if it were already a service (no internal RPC, no serialisation between contexts), because that ceremony has no payoff at this scale.

Repository structure, binary responsibilities, and the deviation from the brief's originally proposed top-level `internal/ingestion`, `internal/validation`, `internal/sourcehealth`, `internal/entitlements` packages (folded as sub-packages of `domain` and `application` instead, per §10) are documented in the blueprint and are not restated here.

## Consequences

### Positive

- One `go build` per binary, one dependency graph, one version of every shared type — no version-skew problems between "services" that are really the same code compiled differently.
- Deployment is a single artifact per binary; the same binary runs under Docker Compose, ECS Fargate, or AWS Lambda unmodified (ADR-0010), which would not be true of a services architecture split prematurely along the wrong lines.
- Local development requires no service mesh, no inter-process contract testing, and no distributed transaction reasoning — a contributor clones the repo and runs `docker compose up`.
- Refactoring a bounded-context boundary is a compiler-checked, single-repo change. Getting the boundary wrong costs an afternoon, not a cross-team migration.

### Negative

- All three binaries share a release cadence in the sense that a bug in a shared package can affect all of them simultaneously; there is no independent blast-radius isolation between, say, the worker and the API beyond process separation.
- The monolith can absorb architectural sloppiness that a hard service boundary would have prevented by construction — the discipline has to come from `internal/archtest` (ADR-0002) and code review, not from the deployment topology.
- Scaling API and worker independently is possible (they are separate binaries and separate deployments already) but scaling *within* a context — for example running Sourcing's fetch workload on different infrastructure from Ingestion's validation workload — is not available without further decomposition.
- A team that grows significantly faster than the codebase's module boundaries may find package-level ownership harder to enforce via tooling than repository-level ownership would be.

### Neutral

- The bounded contexts exist today purely as an organisational and dependency-direction discipline; they carry no runtime cost and no operational visibility of their own (no separate health checks, no separate logs) beyond what the two binaries already expose.
- This decision is explicitly revisited by measurement, not by preference — see below.

## Alternatives considered

### Microservices from the start

Rejected. Splitting Catalog, Sourcing, Ingestion, Distribution, and Intelligence into independently deployed services would require solving service discovery, network-boundary error handling, distributed tracing across process hops, and eventual consistency for the entitlement and publication use cases, none of which the MVP's traffic or team size justifies. It would also directly contradict the low-idle-cost principle (ADR-0013): each service is a new unit of always-on compute or a new class of cold start.

### Multiple Go modules, one binary set

Rejected. Splitting `internal/domain`, `internal/application`, etc. into separate Go modules with their own `go.mod` files buys nothing at this size — there is no independent versioning consumer for any of them — and adds `go.work` complexity and slower CI without a corresponding benefit. A single module keeps `go build ./...` and `go test ./...` simple.

### One binary for everything (API, worker, and CLI as subcommands of one process)

Rejected, though it was close. Combining API and worker into one binary would save one Docker image but would force the API's request-serving lifecycle and the worker's long-running scheduler loop to share a process, complicating graceful shutdown, resource limits, and independent scaling of the two workloads, which have different traffic shapes (bursty low-latency reads vs. steady background polling). Separate binaries built from the same module keep the benefits of a monolith's shared code without conflating two different runtime lifecycles.

## Revisit when

- Any single bounded context's CPU, memory, or request volume grows large enough that it needs infrastructure the rest of the monolith does not (for example, Sourcing's fetch workload needing many more concurrent outbound connections than Distribution's read path).
- A context needs an independent deployment cadence for operational reasons (e.g., Intelligence needs to roll out AI provider changes far more often than the rest of the system, and monolith-wide redeploys become a bottleneck).
- Team size grows past roughly 8–10 engineers actively committing to the Go module, at which point package-level ownership starts to strain without repository- or service-level boundaries.
- `internal/archtest` dependency violations recur despite enforcement, indicating the package boundaries are not actually holding as architectural boundaries in practice.
