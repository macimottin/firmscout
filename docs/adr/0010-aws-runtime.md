# ADR-0010: Lambda-first AWS runtime, same binary everywhere

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** founding team
- **Requires qualified legal review:** no
- **Related:** ADR-0001, ADR-0013, ADR-0015

## Context

The brief left the choice between ECS Fargate and AWS Lambda open. Both are legitimate ways to run a Go HTTP API and a background worker on AWS, but they have opposite cost shapes: Fargate's smallest useful always-on task costs real money every month regardless of traffic, and running two tasks for availability doubles that fixed cost; Lambda's cost is proportional to actual invocations, with idle cost approaching zero. Cost minimisation is a functional requirement here (ADR-0013), and FirmScout's expected traffic profile at launch — a small number of pilot vendors, low web and API traffic, bursty rather than sustained — is exactly the shape Lambda is built for, not the shape that justifies a standing container fleet.

## Decision

FirmScout runs **Lambda-first**. The `apps/api` Go binary runs unmodified inside AWS Lambda behind the **AWS Lambda Web Adapter**, fronted by an **API Gateway HTTP API** and **CloudFront**. Background work (`apps/worker`'s responsibilities) runs as Lambda functions triggered by **SQS** (using the SQS `JobQueue` adapter, ADR-0015) and **EventBridge Scheduler** for time-based scheduling. The public web front end runs via **OpenNext** on Lambda + CloudFront, keeping the Next.js deployment on the same serverless, scale-to-zero shape as the API.

The defining property of this decision is that **the same Go binary** runs under Docker Compose locally, under this Lambda deployment, and — if the alternative below is ever adopted — under ECS Fargate, with no code branch for "which runtime am I in" beyond how the process is invoked (the Lambda Web Adapter translates Lambda's invocation model into a standard HTTP request the unmodified binary handles). This is what makes Lambda a runtime decision rather than an architecture decision: nothing in `internal/domain`, `internal/application`, or even most of `internal/adapters` needs to know which runtime it is running under.

**Fargate is documented as the alternative, not discarded.** It is understood to be the correct choice at sustained high traffic, where Lambda's per-invocation pricing exceeds a comparably provisioned always-on container fleet's cost, or for workloads needing long-lived connections Lambda's execution model does not suit well.

## Consequences

### Positive

- Idle cost approaches the database bill alone — with no traffic, there is no compute cost beyond PostgreSQL and negligible fixed charges (API Gateway, CloudFront, EventBridge each have minimal or no fixed cost at zero usage), which is the central promise of "low idle cost" as an operating principle (§1).
- No container orchestration to learn, operate, or secure for the small founding team — no task definitions, no cluster capacity planning, no container image vulnerability surface beyond the Lambda runtime layer itself.
- The binary being unmodified means `docker compose up` for local development and a Fargate deployment both remain available without a rewrite if the Fargate threshold is ever crossed — this decision does not lock the project into Lambda permanently, it defers the Fargate investment until it is justified by measurement.
- EventBridge Scheduler and SQS-triggered Lambda workers give the background job system a fully managed, scale-to-zero equivalent of the worker binary's local scheduler loop, without a standing process to keep alive.

### Negative

- **Cold starts** are real, even if a compiled Go binary's cold starts are small (typically tens of milliseconds for the runtime itself, though the Lambda Web Adapter and any VPC networking configuration add their own overhead that needs to be measured, not assumed). Latency-sensitive or first-request-after-idle traffic will occasionally see this, and it has not yet been measured against this specific binary and adapter combination — this is a claim to verify in the AWS deployment phase, not a settled fact.
- **A 15-minute execution ceiling** applies to every Lambda invocation. This is judged irrelevant for FirmScout's workload — source checks and API requests are short — but it is a real constraint that would break if a future workload (e.g., a very large batch registry sync or export job) needed longer, and that workload would need a different execution path (e.g., Fargate or Step Functions orchestration) rather than fitting inside this model.
- **Cost predictability degrades under sustained high traffic.** Lambda's per-invocation, per-GB-second pricing is excellent at low and bursty volume and can become more expensive than an equivalently sized always-on fleet once traffic is sustained and high enough — this is the explicit threshold for switching to Fargate, and it needs real measurement against real AWS pricing at the time, not an assumption baked in today.
- Running behind API Gateway and the Lambda Web Adapter adds a translation layer between the client and the Go binary that would not exist with a container directly behind a load balancer — this is one more component in the request path to reason about during debugging, even though it requires no code change.
- VPC networking for Lambda (needed if Lambda functions must reach a private RDS/PostgreSQL instance) has historically added cold-start and connection-management complexity (e.g., connection pooling behavior across frequent cold starts hitting PostgreSQL's connection limits) that needs explicit handling — likely an RDS Proxy or equivalent — and is not free of operational nuance despite the "same binary" framing.

### Neutral

- No AWS prices are asserted in this ADR. Any cost comparison between Lambda and Fargate at a given traffic level must be computed from current AWS pricing at decision time — this ADR commits to the decision framework (measure, then switch if justified) not to specific dollar figures.
- This decision is scoped to the AWS deployment specifically; self-hosted deployments via Docker Compose are unaffected and remain the reference environment for local development and any non-AWS self-hosting, using the PostgreSQL `JobQueue` adapter instead of the SQS one.

## Alternatives considered

### ECS Fargate from the start

Rejected for the MVP, per §3.4. It directly contradicts the low-idle-cost and scale-to-zero principles: the smallest useful always-on task has a real monthly cost whether or not anyone visits, and running two tasks for availability doubles it. FirmScout's traffic profile at launch does not justify paying for standing capacity. Fargate remains the documented alternative once sustained traffic makes its cost profile favorable, per the threshold below.

### A traditional EC2-based deployment (self-managed instances, no container orchestration, no serverless)

Rejected. This would reintroduce always-on cost (worse than Fargate's, typically, once instance management overhead is included) without even the operational simplicity Fargate offers over raw EC2, and would add OS-level patching and instance lifecycle management the small team does not want to own.

### Kubernetes (EKS or self-managed)

Rejected outright, consistent with the blueprint's explicit "no Kubernetes" recommendation for the first six months (§1). Kubernetes's operational overhead is entirely disproportionate to a modular monolith with three binaries and one database at MVP scale.

## Revisit when

- Sustained request rate is high enough, measured against then-current AWS pricing, that a comparably provisioned Fargate deployment's monthly cost is lower than Lambda's — this crossover must be computed from real numbers at the time, not estimated in advance.
- A workload emerges that needs long-lived connections (e.g., WebSocket-based real-time features) that Lambda's execution model does not suit well.
- Measured cold-start latency, once instrumented in production, proves unacceptable for a latency-sensitive path even after tuning (provisioned concurrency, VPC networking adjustments), at which point Fargate or provisioned concurrency should be evaluated with real numbers.
- A job genuinely needs to run longer than Lambda's 15-minute ceiling, requiring either Fargate for that specific workload or a Step Functions-orchestrated decomposition.
