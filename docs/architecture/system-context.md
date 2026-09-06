# System context

> The deep dive behind [blueprint.md](blueprint.md) §4.3 and the system-context diagram [`docs/diagrams/system-context.md`](../diagrams/system-context.md). That diagram shows *what crosses each boundary*; this document adds the two things a diagram can't: the **trust level** of each actor, and — the actual point of this document — **what FirmScout does when each external dependency is unavailable.**

## Every external actor and system

Quick reference first, detail below:

| Actor / system | Trust level |
| --- | --- |
| Anonymous web visitor | Untrusted |
| API consumer (free key) | Authenticated, low trust |
| Enterprise customer | Authenticated, contractual trust |
| Open-source contributor | Trusted collaborator, unprivileged by default |
| FirmScout maintainer | Fully trusted, bound by the same use cases as automation |
| Manufacturer sources | Untrusted content, trusted-by-provenance identity |
| CVE / advisory sources | Trusted-by-provenance for existence, not for applicability |
| GitHub | Trusted infrastructure, dual role |
| AWS | Trusted infrastructure provider |
| AI provider | Untrusted output, budgeted access |
| Search engine crawlers | Untrusted but wanted traffic |

### Anonymous web visitor

- **Flows in:** search queries, page navigation.
- **Flows out:** catalogue pages, release history, evidence links.
- **Trust level:** untrusted. No authentication; rate-limited by IP/session heuristics and the WAF. Every input — a search term, a path parameter — is treated as adversarial, never as a hint about what to trust more.

### API consumer (free key)

- **Flows in:** an API key on every request, lookup requests.
- **Flows out:** JSON responses — latest version, a recent history window, evidence.
- **Trust level:** authenticated, low trust. The key identifies the caller for quota-enforcement purposes only; it grants no elevated data access beyond what the capability matrix in blueprint §5 already lists for that tier.

### Enterprise customer

- **Flows in:** bulk lookups, webhook registration, inventory upload.
- **Flows out:** structured releases, advisories, SLA freshness commitments.
- **Trust level:** authenticated, contractual trust. The higher tier buys quota, additional endpoints, and a stronger freshness commitment — never additional *correctness*. Blueprint §5 is explicit that correctness is never a paywall lever, and that rule is what keeps "enterprise" from meaning "the truthful version."

### Open-source contributor

- **Flows in:** collector configs, dataset pull requests (vendors, products, sources), corrections.
- **Flows out:** CI results, review feedback, merged registry changes.
- **Trust level:** trusted collaborator, but **unprivileged by default**. A contributor's pull request is reviewed exactly like any other external contribution, and a merged dataset change only takes effect through `registry sync` — there is no path from "PR merged" to "live in production" that skips a maintainer's review and the sync step.

### FirmScout maintainer

- **Flows in:** review-queue items (`review_items`) awaiting a decision, pull requests awaiting approval, AI-proposed changes awaiting publication approval.
- **Flows out:** review queue notifications, alerts, escalation notifications.
- **Trust level:** fully trusted, but still bound by the same use cases as automation. When a maintainer approves a candidate for publication, they call the same `PublishRelease` use case a deterministic validator would call — there is exactly one code path into `releases`, human-triggered or not, which is what makes the publication history auditable regardless of who approved which row.

### Manufacturer sources

- **Flows in:** HTML, JSON, PDF, RSS/Atom, and XML artifacts.
- **Flows out:** conditional `GET` and polling requests, always identifying FirmScout by an honest user agent.
- **Trust level:** **untrusted content, trusted-by-provenance identity.** The domain is authoritative for the underlying fact (`source.official = true` on an official domain means something), but every byte actually received over the wire is untrusted input to a parser: guarded against pathological HTML and oversized payloads by the fetcher, and — per [ai-agents.md](ai-agents.md) — explicitly untrusted as *instructions* to any AI agent that later reads the same content. Provenance of the *source* and trustworthiness of the *bytes* are two different questions, and this row answers both separately on purpose.

### CVE and advisory sources (NVD, vendor PSIRTs)

- **Flows in:** advisory metadata, affected version ranges.
- **Flows out:** queries keyed by product identity or version range.
- **Trust level:** trusted-by-provenance for the advisory's own existence and content. The *applicability* of a given advisory to a specific FirmScout-tracked product, however, is FirmScout's own mapping, carries its own confidence score, and is never presented as if the vendor itself asserted the match.

### GitHub

- **Flows in:** code and dataset changes arriving via CI, issue and pull-request activity.
- **Flows out:** dataset pull requests opened by automation (e.g. Repair-agent proposals once that ships), released build artifacts.
- **Trust level:** trusted infrastructure, in two distinct roles at once — code host and, per blueprint §3.3 / ADR-0016, the transport for the hybrid dataset's registry pull requests. Both roles go through the same review discipline, so the dual role does not create a second, less-scrutinised path into the system.

### AWS

- **Flows in:** nothing FirmScout treats as data — AWS is infrastructure, not a data source.
- **Flows out:** it deploys the binary, invokes Lambda functions, and provides storage, compute, and queueing.
- **Trust level:** trusted infrastructure provider. Crucially, this trust relationship never modifies the application: the same Go binary runs identically outside AWS (Compose, bare metal, another cloud), per blueprint §3.4 — AWS earns its trust level by being a deployment target, not by being baked into the application's assumptions.

### AI provider

- **Flows in:** escalation requests (Discovery, Repair, Validation, Classification, Source Quality), each explicitly budgeted per run.
- **Flows out:** proposals with evidence, schema-validated before anything downstream is allowed to look at them.
- **Trust level:** **untrusted output, budgeted access.** Every response, regardless of how confident it reads, is treated as a proposal requiring deterministic validation or human review — never as a fact. This is the same discipline applied to manufacturer sources, extended one step further: even a *correctly delimited, non-adversarial* model response is still just a proposal. See [ai-agents.md](ai-agents.md) for the full contract.

### Search engine crawlers

- **Flows in:** indexing requests against public pages.
- **Flows out:** rendered HTML, structured data (JSON-LD), a sitemap.
- **Trust level:** untrusted but *wanted* traffic. Assumption P4 in blueprint §2 treats organic search as the primary acquisition channel, so crawler access is deliberately generous within the same abuse-resistance layers any other anonymous visitor gets — `robots.txt` explicitly allows indexing of public pages, and nothing about crawler traffic is treated as more suspicious than an anonymous visitor's by default.

## What happens when each dependency is unavailable

This is the section that carries the document's value: for every external dependency, what FirmScout does instead of failing outright.

### A single manufacturer source is unreachable

The source times out, returns a 5xx, is rate-limited, or fails DNS/TLS resolution. `CheckSource` records the outcome (`unavailable`) on the *source*, not on the platform. The source's own health state machine (`docs/diagrams/source-health-state-machine.md`) moves toward `degraded` then `broken` after repeated failures, backing off the check interval exponentially as it goes. **Every other source, and every already-published fact, is unaffected** — there is no shared circuit that trips platform-wide because one vendor's server is down. Already-published releases for that product remain visible, correctly labelled with their last `lastVerifiedAt`, which grows stale but is never silently refreshed with a guess.

### All manufacturer sources are unreachable at once

Unlikely, but architecturally relevant to reason through: a network partition, or a shared upstream failure affecting many vendors simultaneously. The worker's scheduler keeps running and simply produces `unavailable` outcomes across the board; the job queue absorbs the backlog, since jobs are retried per the backoff policy in [update-pipeline.md](update-pipeline.md) rather than dropped. The public site and API remain fully functional against already-published data throughout, because reads never depend on sources being reachable — they read `product_summaries`, a precomputed projection, never a live fetch.

### PostgreSQL is down

This is the one dependency FirmScout cannot degrade gracefully around, by design — blueprint §3 states "PostgreSQL as the only stateful dependency" precisely so that there is exactly one dependency this severe to reason about, rather than several partial ones. `apps/api` returns `503` with a `problem+json` body rather than a partial or fabricated response; `apps/worker` stops dequeuing and retries its own connection with backoff; nothing crashes hard, and nothing silently serves cached-forever data past its declared `Cache-Control` window. CloudFront's edge cache continues to serve already-cached pages during a brief outage, which buys time without lying about freshness, since cached responses still carry their real age.

### The artifact store is unavailable

A write or read failure against S3 (or the filesystem/large-object adapter locally). `CheckSource` treats a failed artifact write as a failed check — the `FetchOutcome` degrades to a retryable failure — rather than as a success without evidence. A candidate is never extracted from an artifact that was not durably stored, because evidence is validation gate 7 in [update-pipeline.md](update-pipeline.md), not a courtesy that can be skipped under pressure.

### The job queue is unavailable

In the MVP, the queue *is* PostgreSQL, so this shares PostgreSQL's failure domain above with no separate availability story. On AWS, the SQS adapter's availability is decoupled from RDS's, so a partial-outage scenario — SQS reachable, RDS not — still surfaces the same way: writes fail, retried with backoff, nothing lost.

### GitHub is unavailable

No effect on the running platform. GitHub is a build-time and dataset-curation-time dependency, never a runtime one. `registry sync` simply cannot pull new registry changes until GitHub recovers; already-synced registry data continues to serve exactly as before.

### AWS itself has an outage (in the AWS deployment)

Out of scope for application-level mitigation beyond what AWS's own service-level agreements provide; see [aws-deployment.md](aws-deployment.md)'s disaster recovery section for backup and restore posture. The self-hosted Compose deployment is entirely unaffected by an AWS outage by construction, since it runs the identical binary without any AWS dependency at all.

### The AI provider is unreachable or the budget is exhausted

**FirmScout operates fully without it.** This is not a degraded mode retrofitted onto a design that assumes AI is present — it is the actual default mode, per the blueprint's core operating principle "deterministic by default, AI only by escalation." Sources that would have escalated to Repair simply accumulate in `review_items` for a human instead; Discovery-driven vendor onboarding pauses; already-published data, and any source whose deterministic watcher still works, are entirely unaffected. See [ai-agents.md](ai-agents.md)'s degraded-mode section for the same point from the AI document's own angle.

### CVE and advisory sources are unavailable or malformed

Advisory display degrades to "last known" data, carrying its own `lastVerifiedAt` so staleness is visible rather than hidden. Release publication is entirely independent of advisory correlation — advisories are enrichment, not one of the ten validation gates — so a stalled advisory feed never blocks a version from being published.

### Search engine crawlers are accidentally blocked

Not an external outage but an internal failure mode worth naming here, because it defeats assumption P4 (organic search as the primary acquisition channel): a misconfigured WAF rule or a `robots.txt` regression. Treated as a release-blocking regression once detected, monitored via crawl-rate and search-impression metrics rather than discovered by chance months later.

### The pattern across every case above

The consistent shape: **the blast radius of an external failure is contained to the specific source, product, or subsystem it touches.** Nothing external to FirmScout is a single point of failure for the whole platform except PostgreSQL, which is the one dependency the architecture deliberately concentrates risk into — blueprint §3's "low idle cost... nothing in the MVP requires always-on compute beyond a small database" — in exchange for operational simplicity. That concentration is exactly why [aws-deployment.md](aws-deployment.md)'s disaster recovery section treats PostgreSQL's backup and restore posture as the single most important operational procedure in the system.

## The compliance boundary

Two independent, mechanically distinct checks govern every interaction with a manufacturer source, both evaluated **before** collection, never after. The practical, contributor-facing version of this policy lives in `DATA_SOURCES.md`; its authority is [ADR-0018](../adr/0018-source-compliance-policy.md).

- **`robots_policy_status`** — a mechanical fact obtained by fetching and parsing the host's `robots.txt`: `allowed`, `disallowed`, `unknown`, or `not_applicable`. This is not a judgment call; it is the literal, verifiable result of the check, and it cannot be argued with the way a terms-of-service reading can. `not_applicable` is for a source `robots.txt` does not govern at all; a host that merely fails to serve the file is still governed by it and is recorded as `allowed`, per RFC 9309 §2.3.1.3.
- **`terms_review_status`** — a human judgment, often needing legal input: `pending`, `approved`, `restricted`, or `prohibited`. `restricted` permits collection under recorded conditions; `pending` and `prohibited` do not permit it at all.

**A source is dispatched for automated collection only when `enabled` is true AND `robots_policy_status` is `allowed` or `not_applicable` AND `terms_review_status` is `approved` or `restricted`** (and its health is `active` or `degraded`). Either field short of that, and the source stays registered — visible, documented, in the dataset — but `enabled = false`. This is why Dell's `downloads.dell.com/catalog/Catalog.xml.gz`, a technically ideal, ETag-bearing, well-formed catalogue, sits in the registry uncollected: its `robots.txt` returns `Disallow: /` for all user agents. FirmScout's own stated policy is not suspended for its most convenient source (blueprint §3.6) — if it were, the policy would mean nothing for the next thousand sources that are less convenient and less tempting to bend the rule for.

The same boundary explains why manufacturer sources are marked "untrusted content, trusted-by-provenance identity" in the actor list above rather than simply "trusted": compliance status governs *whether FirmScout is allowed to fetch at all*, and is entirely separate from *whether the fetched content can be trusted as input to a parser or an AI agent once fetched*. The latter question is defended by SSRF guards, size and MIME limits, and — for AI specifically — explicit content delimiting, all covered in [update-pipeline.md](update-pipeline.md) and [ai-agents.md](ai-agents.md) respectively. A source can clear the compliance boundary entirely and its content can still be hostile input to a parser; the two defences are independent and both are required.

The absolute prohibitions that follow from this boundary, stated without qualification: FirmScout never evades authentication, CAPTCHAs, access controls, rate limits, `robots.txt`, or a website's terms of service — not with a different user agent, not with a residential proxy, not by finding an undocumented endpoint that happens to route around a block — regardless of how retrievable the data would technically be. Fortinet's firmware downloads requiring authentication is treated as a legitimate `authentication_required` state on that source, permanently if that's what it takes, never as an obstacle to route around.

## Related documents

- [overview.md](overview.md) for the end-to-end walkthrough this document's actors and dependencies plug into.
- [update-pipeline.md](update-pipeline.md) for the mechanics of what happens on the `platform → manuf` edge specifically: the watcher ladder, the validation gates, and publication.
- [ai-agents.md](ai-agents.md) for the full contract governing the `platform ↔ aiprovider` edge, including why its output is always a proposal.
- [aws-deployment.md](aws-deployment.md) for how the `platform ↔ aws` edge is realised in infrastructure, and for the disaster-recovery procedure referenced above.
- `DATA_SOURCES.md` at the repository root for the contributor-facing version of the compliance boundary, including the current status of each pilot vendor's sources.
