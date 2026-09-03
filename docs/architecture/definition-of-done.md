# Definition of done

> Cross-references [blueprint §7.8](blueprint.md#78-testing-boundaries) (testing boundaries this document turns into literal commands) and [§9](blueprint.md#9-complete-mermaid-diagram-set) (the diagram set this document's documentation-obligation rule protects). Applies to every level of change in this repository; a change that does not meet its level's bar is not done, regardless of how much of it is finished.

## 1. What "done" means, per level

### A code change

A code change (a bug fix, a small feature, a refactor confined to one or a few packages) is done when:

- It compiles and passes `go vet` with no new warnings.
- It has tests appropriate to the layer it touches, per the testing-boundary table in blueprint §7.8 (domain: pure unit tests, no DB/network/clock; application: fakes only; adapters: real dependency, skipped explicitly if unavailable; collectors: fixtures only, never a live site).
- `internal/archtest` still passes — a change that makes an inward-dependency violation necessary is not a code change, it is an architectural decision (see "an architectural decision," below) and needs the corresponding review.
- Existing tests it did not intend to affect still pass; a change that requires updating an unrelated test's expected output is either explained in the pull request or is not actually as scoped as it claims to be.
- It does not silently change API response shape, database schema, or collector config semantics without the corresponding documentation update (§3).

### A collector

A collector (config-driven or code-based) is done when, in addition to the code-change bar above:

- It satisfies the full checklist in [`docs/collectors/developing-collectors.md`](../collectors/developing-collectors.md#checklist-for-submitting-a-collector): configuration-driven unless there is a stated reason for code; `Extract` (or the config's declarative extraction) is pure per collector-sdk.md §5; the cheapest available change signal is used and recorded in evidence; volatile content is excluded from hashing; date precision matches what the source actually supports; `channel`/`release_type` use the constrained vocabularies; at least one fixture with full provenance plus at least one edge-case fixture; the source's compliance status is filled in honestly.
- `firmscout collector test` (collector-config-spec.md §7) passes for a config-driven collector; `collectortest.RunFixtures` passes for a code collector.
- JSON Schema validation passes (`packages/schemas/collector-config.schema.json`) for a config-driven collector.
- It has never, at any point in its development, been tested against the live vendor site as a substitute for a fixture — this is checked in review, not just by CI, because CI cannot detect that a contributor manually verified against a live site and then only committed the fixture (blueprint §7.8's absolute rule).

### A new vendor

Registering a new vendor is done when:

- `dataset/vendors/<slug>.yaml` exists and validates against its JSON Schema.
- Every source proposed for that vendor has a filled-in `robots_policy_status` (mechanically verified, not assumed — DATA_SOURCES.md) and `terms_review_status` (a stated, honest value, even if `pending`) **before** any collector is written for it, not after — the ordering matters, per risks.md R10's narrative on why Dell is the standing counterexample to writing the collector first.
- A source's `enabled` flag is `true` only if `robots_policy_status = allowed` **and** `terms_review_status = reviewed_ok`; anything short of that is registered, documented, and left `enabled = false`.
- At least one source has a quality class assigned per DATA_SOURCES.md's table, with the reasoning for that class (not just the label) visible somewhere reviewable — the pull request description, a comment in the YAML, or a linked issue.
- `firmscout registry sync` runs cleanly against the new vendor's files with no schema or cross-reference errors.

### A data correction

A correction to previously published data (a wrong version, a wrong date, a mis-resolved product) is done when:

- It is implemented as a new row referencing the corrected one (`corrects_release_id`) or a withdrawal with reason, per blueprint §16 — **never** as an `UPDATE` against an existing `releases` row, which the schema's append-only discipline (blueprint §11 rule 2) does not permit in the first place, so this bar is largely self-enforcing at the database level, but a correction path that tries to route around it (e.g. deleting and re-inserting to fake an update) fails this bar even if the schema constraint itself is not literally violated.
- The correction carries evidence — what showed the original fact was wrong, not just an assertion that it was.
- The audit trail (who/what approved the correction) is populated, per blueprint §16's "correction inserts a new row with the audit trail of who approved it."
- `product_summaries` is refreshed to reflect the correction, exactly as it would be for an original publication.

### A documentation change

A documentation change is done when:

- It does not contradict `docs/architecture/blueprint.md`, which is authoritative — a document that expands the blueprint may add detail the blueprint does not carry, but never reverses a decision the blueprint states without the blueprint itself being updated first, in a separate, deliberate change.
- Every Mermaid diagram it adds or touches passes `bash scripts/check-mermaid.sh` with `RESULT: PASSED`, using only `flowchart`, `sequenceDiagram`, `stateDiagram-v2`, or `erDiagram`, with every space/punctuation/colon/parenthesis-bearing label double-quoted and no node id literally named `end`.
- Any cross-reference it adds (a link to another doc, an ADR, a diagram) actually resolves to a real file and section — a broken internal link is a defect, not a nitpick, in a document set this heavily cross-referenced.
- It states plainly, and never implies otherwise, what is actually built versus what is designed-but-not-built versus what is planned — this repository is, as of this writing, a documented skeleton with nothing implemented beyond it, and every document in this set is written to be honest about that distinction rather than aspirational.

### An architectural decision

A change that alters something blueprint §6's decision table, an existing ADR, or the module-dependency rules in blueprint §8.2 established is done when:

- It is recorded as a new ADR (or an explicit amendment/supersession of an existing one) in `docs/adr/`, following the existing format: Status, Context, Decision, Consequences, Alternatives, Requires legal review (yes/no).
- Its "Requires legal review" field is answered honestly — a decision touching licensing, data rights, or compliance policy is marked `yes` even if legal review has not happened yet; marking it `no` to avoid the follow-up obligation defeats the field's purpose.
- Diagrams affected by the decision are updated in the same change (§3) — not scheduled for later, because a diagram that is already known to be wrong at the moment a decision lands is a defect from the moment it is committed, not from the moment someone notices.
- The blueprint itself is updated if the decision changes something the blueprint states directly (its decision-summary table, an architecture-decision-summary row) — an ADR that contradicts an un-updated blueprint leaves the repository in a state where its own authoritative document is wrong, which is the one state this document set is built specifically to avoid.

### A release

A release (a tagged version of the codebase, once there is a codebase to tag — not applicable to the current documentation-only state of the repository) is done when:

- Every quality gate in §2 passes on the exact commit being tagged, not on a commit from earlier in the day.
- `docker compose up --build` succeeds from a clean checkout of that commit, matching mvp-roadmap.md's Phase 1 exit criteria and every later phase's equivalent.
- The consistency between diagrams and code (§3) has been checked for anything touched since the prior release, not assumed still valid.
- Anything shipped that touches the dataset licence, hosted terms, or API terms (licensing.md) has had its "requires legal review" items either resolved or explicitly, visibly still open with a named owner — a release does not silently ship a change to a licensing-adjacent area without that status being current.

## 2. Quality gates, with literal commands

These are the commands a change must pass, once the corresponding tooling exists (this repository is, as of this document, a skeleton — commands below are the gates the first vertical slice and every change after it must satisfy, matching first-sprint-backlog.md's SLICE-27 exit criteria):

```bash
# Formatting
gofmt -l .            # must print nothing
goimports -l .         # must print nothing

# Static analysis
go vet ./...

# Linting
golangci-lint run

# Architecture rule enforcement
go test ./internal/archtest/...

# SQL/generated-code drift
sqlc diff

# Full test suite: domain, application, adapters, archtest, collector fixtures
go test ./...
# Adapter tests against PostgreSQL are skipped with an explicit message if
# FIRMSCOUT_TEST_DATABASE_URL is unset -- a skip must be visible in the
# output, never silently treated the same as a pass.

# Race detector on anything touching the job queue or concurrent state
go test -race ./internal/adapters/postgres/... ./internal/adapters/queue/...

# Diagram validation
bash scripts/check-mermaid.sh   # must print RESULT: PASSED

# OpenAPI contract
python3 -c "import yaml; d=yaml.safe_load(open('docs/api/openapi.yaml')); assert d['openapi'].startswith('3.1')"

# Collector config validation (per changed config)
firmscout collector test collectors/config/<vendor>/<source>.yaml

# Registry consistency (dataset cross-references)
firmscout registry sync --dry-run

# Web app
cd apps/web && npm run lint && npm run typecheck && npm run build

# Full-stack smoke test
docker compose -f infrastructure/docker/docker-compose.yml up --build -d
curl -f http://localhost:8080/healthz
docker compose -f infrastructure/docker/docker-compose.yml down
```

A change need not run every command above — a documentation-only change does not need `go test ./...` to run meaningfully, and a Go-only change under `internal/domain` does not need the web app's build. The rule is: run every gate that the change could plausibly have broken, and state in the pull request which gates were run and which were judged not applicable and why — "I didn't run it" and "I ran it and it wasn't relevant" are different claims and should never be presented identically.

## 3. Documentation obligations

**Which diagrams must be updated when architecture changes:** every diagram listed in blueprint §9 whose subject matter the change touches. Concretely: a change to the check/extract/validate/publish pipeline touches `collector-sequence.md` and `update-decision-tree.md` at minimum; a change to module boundaries touches `module-dependencies.md` and `clean-architecture.md`; a change to the API surface touches `api-sequence.md`; a schema change touches `data-model.md`; a deployment-topology change touches `deployment.md`. A change that plausibly touches a diagram and the author is unsure whether it does should default to checking that diagram, not skipping it — the cost of checking a diagram that turns out fine is small; the cost of a diagram silently going stale compounds every time someone trusts it afterward.

**The rule that a diagram which no longer matches the code is a defect:** stated in blueprint §9's own framing of why `check-mermaid.sh` exists ("a syntax error fails the build rather than silently producing a broken image"), extended here to semantic accuracy, not just syntax: a diagram that renders correctly but describes a pipeline, a schema, or a dependency graph that the code no longer implements is not a passing diagram, it is a **defect that happens to render**. This is why every diagram file (per blueprint §9's own per-file convention) carries an "Implementing code" section — filled in, per the diagram files' current placeholder text, "after the first vertical slice" — specifically so that once code exists, a diagram's claim about what implements it is a checkable, falsifiable statement, not a permanent placeholder. Filling that section in, and correcting the diagram itself when the placeholder era ends and the two no longer match, is part of what "done" means for the change that first makes a diagram's subject matter real.

## 4. Evidence obligations for any published fact

This restates, in the specific vocabulary of "done," what blueprint's "evidence first" principle already requires structurally: no fact reaches the public catalogue without evidence, and "the code compiled and the test passed" is not, by itself, sufficient for a change that publishes or would publish data. Specifically:

- Every `Release` a change causes to be published carries a non-empty `Evidence` — `SourceURL`, `RetrievedAt`, `ContentHash`, `Excerpt`, `CollectorID`, `CollectorVersion`, `DiscoveryMethod` (collector-sdk.md §3.6) — enforced by validation gate 7 in blueprint §16, which a change is not done if it bypasses, weakens, or special-cases.
- A candidate that fails to clear this bar routes to review or is rejected; it is never published anyway "because the data is probably right." Confidence is an input to the gates, not a justification for skipping one.
- A seed script, fixture, or test helper that constructs a `Release`-shaped value for testing purposes must be unambiguously distinguishable, in the code and in any documentation describing it, from a real published fact — `database/seeds/` is explicitly documented in blueprint §10 as "development-only seed scripts (never production facts)," and a change that blurs that line (a seed that could be mistaken for or accidentally promoted to production data) is not done regardless of what else it accomplishes.
- Any change to the excerpt-length bound, the evidence schema, or the validation gates themselves is treated as an architectural decision (§1, "an architectural decision" above), not an ordinary code change, because it changes what "evidence" means platform-wide.
