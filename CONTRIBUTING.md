# Contributing to FirmScout

Thank you for considering a contribution. FirmScout's whole value proposition depends on a wide base of contributors who know their corner of the hardware and firmware world better than any single maintainer can — this document exists to make that easy and safe to do.

Read [`docs/architecture/blueprint.md`](docs/architecture/blueprint.md) first if you're planning anything beyond a small fix; it is the authoritative source for how the project is meant to work, and it will save you from proposing something already decided against (and explained why).

## What we accept

Roughly in order of how often we expect to see them:

- **New vendors, products, and aliases** — additions to `dataset/vendors/`, `dataset/products/`. A pull request with well-formed YAML is enough; no code required.
- **New sources** — a monitored vendor URL, feed, or API added to `dataset/sources/`, with its compliance status filled in honestly (see [DATA_SOURCES.md](DATA_SOURCES.md)).
- **Collector configurations** — YAML collector configs under `collectors/config/`, for sources a configuration-driven engine can already handle.
- **Code-based collectors** — Go packages under `collectors/vendors/`, for sources that genuinely need custom logic. See [`docs/collectors/developing-collectors.md`](docs/collectors/developing-collectors.md).
- **Release corrections** — a wrong published fact, submitted per [`docs/collectors/dataset-correction-policy.md`](docs/collectors/dataset-correction-policy.md).
- **Parser and extraction fixes** — a collector that broke because a vendor changed their page, or was wrong to begin with.
- **Test fixtures** — recorded or synthetic fixtures that improve coverage of edge cases (layout changes, missing fields, month-only dates).
- **Security advisory mappings** — links between a vendor advisory or CVE and an affected product/release range.
- **Documentation** — anything from a typo to a new guide.

If what you have in mind doesn't fit any of these, open an issue first and describe it. We'd rather talk before you write code than ask you to rewrite it after.

## The contribution workflow

Every contribution — dataset YAML, a collector config, or Go code — follows the same shape:

1. **Open an issue or a pull request.** Small, obviously correct changes (a typo, a single-field correction with evidence) can go straight to a PR. Anything that adds a new vendor, source, or collector should usually start as an issue using the relevant [issue template](.github/ISSUE_TEMPLATE), so the compliance and scope questions get answered before code is written.
2. **Schema validation.** Dataset YAML and collector configs are validated against JSON Schema (`packages/schemas/`) as part of CI. A pull request that doesn't validate doesn't merge, regardless of how correct the underlying fact is.
3. **Fixture extraction tests.** For anything touching a collector, the `Extract` function is run against recorded fixtures and its output compared to an expected result. See "Fixtures" below — this is not optional and there is no live-site fallback.
4. **Security checks.** Automated checks for the classes of risk that matter most here: SSRF-prone URLs, secrets accidentally committed, and collector configs that would bypass authentication or rate limits. See [SECURITY.md](SECURITY.md) for scope.
5. **Maintainer review.** A maintainer (see [GOVERNANCE.md](GOVERNANCE.md)) reviews for correctness, compliance, and fit with the architecture. For a new source, this includes verifying the compliance evidence you provided actually supports the claimed status.
6. **Merge.**
7. **Staging validation.** Before a registry change reaches production, it is synced into a staging environment and, for new sources or collectors, exercised against the real source at least once to confirm the extraction actually works — not just that the fixture-based test passes.
8. **Registry sync.** `firmscout registry sync` loads the merged YAML into PostgreSQL. Registry tables are only ever written by sync; a direct database edit to a registry row is a bug, not a shortcut.
9. **Attribution is retained.** Your authorship is preserved in Git history and, for dataset and collector contributions, in the commit trail that the registry sync process can trace back to. We don't rewrite that away.

## Development setup

You'll need:

- **Go 1.27+**
- **Node.js 22+**
- **Docker and Docker Compose**

```bash
git clone https://github.com/macimottin/firmscout.git
cd firmscout
docker compose -f infrastructure/docker/docker-compose.yml up -d postgres
go run ./apps/cli migrate up
```

The full local workflow, once it exists end to end, is in the [README Quick start](README.md#quick-start-target-local-development-workflow) — note the honesty caveat there: some of this is describing the target shape of the repository, not something you can run today.

### Running tests

```bash
go test ./...
```

Domain and application tests run with no external dependencies. Adapter tests against PostgreSQL are skipped automatically, with an explicit message, if `FIRMSCOUT_TEST_DATABASE_URL` is not set — they are not silently green.

### Lint

```bash
gofmt -l .
go vet ./...
golangci-lint run
```

For the web app:

```bash
cd apps/web && npm run lint && npm run typecheck
```

### The architecture-boundary check

`internal/archtest` enforces the dependency rules from the blueprint (domain → application → adapters → platform, inward only) mechanically, by inspecting `go list -deps` output, not by convention or code-review vigilance. Run it as part of `go test ./...`; if it fails, the fix is almost always to move a type, not to weaken the rule. If you genuinely believe a rule needs to change, that's an architectural decision — open an ADR discussion, don't route around the check.

### Mermaid diagrams

If your change touches a `docs/diagrams/*.md` file or adds a Mermaid block anywhere, run:

```bash
bash scripts/check-mermaid.sh
```

Only `flowchart`, `sequenceDiagram`, `stateDiagram-v2`, and `erDiagram` are used, because those are the types GitHub renders natively without a plugin.

## Fixtures: live vendor sites are never a test dependency

This is an absolute rule, not a preference. A test suite that fails because a vendor reorganised their documentation site teaches contributors to ignore failures, and it makes CI depend on infrastructure nobody on the project controls. Every collector test runs against a **recorded fixture on disk**.

### Recording a fixture

1. Fetch the content once, by hand or with a small script, from the real source.
2. Save it under `testdata/fixtures/<vendor>/<source>/`.
3. **Trim it to the minimum needed to exercise the case you care about.** A 400 KB changelog page becomes a fixture with the handful of entries that matter — do not commit an entire scraped page when three records prove the point. Keep a synthetic fixture separate from a recorded one if you need to construct an edge case (a missing version, a month-only date, a layout regression) that doesn't occur naturally in the real page yet.
4. Write a `README` alongside the fixture recording its **provenance**: the exact source URL, the retrieval timestamp (UTC), and a content hash (`sha256sum` is fine) of the original, untrimmed response. This is what lets a reviewer — or you, a year later — tell whether a fixture is stale or was ever real to begin with.
5. Write the corresponding `expected.json` describing what a correct `Extract` call should produce from that fixture.

A fixture with no provenance README is treated as unreviewable and will not be merged.

## Commit and PR conventions

- Keep commits focused; a dataset addition, a collector fix, and a doc typo are three PRs, not one.
- Write commit subject lines in the imperative mood ("Add MikroTik changelog collector", not "Added" or "Adding").
- Reference the issue a PR resolves, where one exists.
- Fill out the [pull request template](.github/PULL_REQUEST_TEMPLATE.md) — it exists so reviewers don't have to ask the same five questions on every PR.
- Every commit must be signed off (see DCO, below). Squash-merges preserve the sign-off of the final commit; if your PR has multiple commits, each one still needs `-s`.

## Developer Certificate of Origin, not a CLA

Contributions require a **sign-off** under the [Developer Certificate of Origin (DCO)](https://developercertificate.org/), not a Contributor License Agreement:

```bash
git commit -s -m "Add MikroTik changelog collector"
```

This adds a `Signed-off-by: Your Name <your@email>` trailer certifying that you wrote the contribution, or otherwise have the right to submit it under the project's licence.

**Why DCO instead of a CLA:** a CLA typically assigns or broadly licenses your copyright to a legal entity behind the project, which is a real barrier for individual contributors and for engineers who need employer sign-off just to sign a CLA. FirmScout's licensing strategy (Apache-2.0 code; the moat is the dataset and the operational history, not exclusive control of the source — see [blueprint §3.7](docs/architecture/blueprint.md#37-agpl-protects-the-business--challenged-apache-20)) doesn't need that leverage. DCO gets the same practical outcome — a clear, attributable record of who has the right to contribute what — with much less friction, and it's the model used by the Linux kernel, Git itself, and most large infrastructure projects with a similar contributor base.

## Source compliance rules — non-negotiable

These apply to every source you register, every collector you write, and every fixture you record:

- **Never bypass authentication, CAPTCHAs, rate limits, or `robots.txt`.** If a source requires working around any of these to collect from, it does not get collected from — it gets registered with an honest `authentication_required` or `robots_policy_status = disallowed` status and left disabled, the way Dell's catalogue currently is (see [DATA_SOURCES.md](DATA_SOURCES.md)).
- **Prefer official APIs and feeds over scraping**, whenever a vendor offers one. A stable JSON or RSS endpoint is cheaper, more reliable, and less legally ambiguous than parsing HTML.
- **Never reproduce copyrighted release notes in full.** Store a short evidence excerpt sufficient to justify what was extracted, and link to the vendor's page for the complete text. This is both a legal boundary and a maintenance one — FirmScout is not trying to become a mirror of vendor documentation.

Compliance status (`robots_policy_status`, `terms_review_status`) is evaluated **before** a source is enabled for collection, not discovered after the fact. If you're unsure whether a source you want to add is compliant, say so explicitly in the issue — "unknown, needs review" is a completely acceptable answer and the correct default.

## Questions

Open a [discussion or issue](.github/ISSUE_TEMPLATE) if anything here is unclear, or if you're not sure which kind of contribution you're making. We'd rather answer a question than have you guess wrong and redo work.
