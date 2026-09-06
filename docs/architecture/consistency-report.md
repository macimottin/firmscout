# Architecture consistency report

> Regenerated 2026-09-06, at the close of Phase 3's **repair pass** — thirteen defects found
> by two adversarial reviews of the device-first catalogue (hardware models as products,
> architecture families, `product_relationships`, and the model-number search path —
> ADR-0024), closed by four owners and reconciled by an integrator. Section 1 was
> re-measured in full at that close; the figures it replaces are named where they moved.
> §10's final block records what the repair pass closed and what it left open.
> Its purpose is to make the gap between the blueprint and the code **visible and
> specific**, rather than leaving it to be discovered later by someone who trusted a
> diagram. Where this document and [`blueprint.md`](blueprint.md) disagree, this one is
> describing what exists and the blueprint is describing what is intended.
>
> **Nothing in this repository has been deployed.** No image has been built, no container
> has run, no AWS account exists, and the pipeline has never fetched from a manufacturer's
> website. Every "implemented" below means "the code exists and its tests pass on a
> developer machine". Read §1's *Not verified* table before treating any of it as
> operational evidence.

## 1. What was verified by execution

Every claim below was produced by running the command and reading its output. Nothing in
this section is inferred, and nothing that could not be run is recorded as passing.

These are a **snapshot taken by the integrator of Phase 2's closing pass on 2026-09-05**,
after four parallel owners' repairs were merged, the cross-owner gaps between them were
closed, and the tree stopped moving. Every figure was re-measured here rather than carried
forward from an owner's report, which is why several differ from the previous draft of this
section: that draft was taken mid-pass against a moving tree and recorded 431 test
functions, 3 registered sources and 12 registry documents. Every figure below is the
reading at the moment named, not a claim about every moment since.

| Gate | Command | Result |
| --- | --- | --- |
| Formatting | `gofmt -l .` | no output |
| Imports | `goimports -l .` | no output |
| Vet | `go vet ./...` | no output, exit 0 |
| Lint | `golangci-lint run` (v2.13.2, built with go1.27.1) | `0 issues.`, exit 0 |
| Build | `go build ./...` | clean, exit 0 |
| Dependency rule | `go test ./internal/archtest/... -count=1` | `ok  github.com/macimottin/firmscout/internal/archtest  1.642s` |
| Dependency rule | `bash scripts/archcheck.sh` | `Dependency rule holds across every package in github.com/macimottin/firmscout.` `RESULT: PASSED` |
| Tests | `FIRMSCOUT_TEST_DATABASE_URL=… go test ./... -count=1 -p 1` | 14 packages `ok`, exit 0, **0 skipped**, **505 distinct test functions** across the module (496 before the repair pass, 445 at the close of Phase 2). `-p 1` is not cosmetic: `internal/adapters/postgres` and `internal/integration` both `TRUNCATE` the same test database in their fixtures, so running them as concurrent packages produces spurious deadlocks and foreign-key violations. Without it the suite still normally passes; with it the result is deterministic |
| Tests, no database | `env -u FIRMSCOUT_TEST_DATABASE_URL go test ./... -count=1 -p 1` | still exit 0. **75 tests skip** (71 before the repair pass, 56 at the close of Phase 2), and all 75 skip messages name `FIRMSCOUT_TEST_DATABASE_URL` — counted mechanically, `--- SKIP` occurrences and lines naming the variable are both 75, from 11 distinct files (`catalog_test.go`, `conflict_test.go`, `device_test.go`, `ingest_test.go`, `latest_test.go`, `migrate_test.go`, `queue_test.go`, `review_test.go`, `review_uniqueness_test.go`, `slice_test.go`, `source_test.go`). The new file is `latest_test.go`, the repair pass's database-backed revert detectors. No test is silently skipped |
| Tests, race detector | `go test ./... -race -count=1` with the variable set **and an out-of-tree C toolchain on `PATH`** | all 14 packages `ok`, no race reported — but see the caveat below: this cannot be reproduced from a clean checkout |
| PostgreSQL adapter | `FIRMSCOUT_TEST_DATABASE_URL=… go test ./internal/adapters/postgres/... -count=1` | `ok … 5.143s`, **82 test functions** (78 before the repair pass, 64 at the close of Phase 2) against a live **PostgreSQL 17.4** |
| Migrations | `TestMigrateUpIsIdempotent`, `TestMigration00002UpAndDown`, `TestMigration00005ShapesTheDeviceSchema`, `TestMigrationsApplyToAFreshDatabase` (adapter), `TestMigrationsAreIdempotent` (integration) | 5 migration files, **33 tables** — 32 `CREATE TABLE` statements across `00001` (29), `00002` (2) and `00005` (1), plus `schema_migrations`, which the runner in `migrate.go` creates. `00003` and `00004` add indexes and columns, no table |
| End-to-end | `FIRMSCOUT_TEST_DATABASE_URL=… go test ./internal/integration/... -count=1 -v` | **8 tests**, all `PASS`, `ok … 1.854s`: the seven Phase 2 tests plus `TestAModelNumberFindsTheFirmware`, which walks a product code to the firmware across registry, sync, PostgreSQL search and the HTTP API |
| Registry | `go run ./apps/cli registry validate` | `Registry is valid: 5 vendor(s), 11 category(ies), 5 family(ies), 8 product(s), 6 source(s).` `0 of 6 source(s) are currently collectable; the rest await a compliance decision (ADR-0018).` The first families and the first six hardware models in the project's history |
| Registry sync | `FIRMSCOUT_DATABASE_URL=… go run ./apps/cli registry sync` | against the **development** database: `Synchronised: 5 vendor(s), 11 category(ies), 5 family(ies), 8 product(s), 13 alias(es), 6 source(s).` then `6 product relationship(s) written; 8 product summary(ies) refreshed.` then `6 source(s) will NOT be checked:` and one reason per source. Dell's is the only reason that names robots.txt |
| Diagrams | `bash scripts/check-mermaid.sh` | `Mermaid: parsed 47 block(s) across 40 file(s).` `RESULT: PASSED` |
| Registry schemas | `python3 scripts/check-schemas.py` | `Checked 25 registry document(s).` `RESULT: PASSED` — 39 documents across 25 files, including the new `family.schema.json` pair |
| OpenAPI | `python3 -c "import yaml; …"` | `3.1.0`; **10 documented paths**, matching the 10 non-internal registered routes exactly (7 under `/api/v1`, plus `/healthz`, `/readyz`, `/metrics`) |
| Web lint | `npm run lint` | clean, exit 0 |
| Web typecheck | `npm run typecheck` | clean, exit 0 |
| Web tests | `npm run test` | `Test Files 13 passed (13)`, `Tests 77 passed (77)` (13/71 before the repair pass; 11/62 at the close of Phase 2) |
| Web build | `npm run build` | `✓ Compiled successfully`; 12 routes, with `/review` and `/review/[id]` dynamic |
| Public API, live | `go build -o /tmp/fs-bin/firmscout-api ./apps/api`, run against the **development** database on `127.0.0.1:8099` | `/healthz` and `/readyz` both `200`. `GET /api/v1/products/mikrotik-routeros/latest` **with no `?channel`** answers `200` with `7.23.5` — the call that answered `404` before the repair pass — and names the same release id as the product page's headline `latestRelease` (`rel_06G77600G0JMXCB539TCCD1JSW`). See §10's repair-pass block |

**`registry sync` writes into the test database.** The run recorded above pointed
`FIRMSCOUT_DATABASE_URL` at `firmscout_test`, the same database the adapter suite uses. The
suite truncates every table before each test, so it passes afterwards; that was re-checked,
not assumed. Note also that `sync` reads `FIRMSCOUT_DATABASE_URL` and *not* the `_TEST_`
variable — a `registry sync` invoked with only the test variable set exits 1 with
`firmscout: FIRMSCOUT_DATABASE_URL is required`.

**The race detector runs, and the gate is still not reproducible.** Both halves of that
sentence are load-bearing, and an earlier draft of this report asserted the first without
pinning the second down well enough for a reader to check either. Re-verified 2026-09-05:

*What was run.* A GCC 13.3.0 toolchain (`Ubuntu 13.3.0-6ubuntu2~24.04.1`) unpacked from
`.deb` packages into a session scratch directory without root, reached through a two-line
`cc`/`gcc` wrapper that supplies `--sysroot` and `-B`, placed ahead of everything on
`PATH`:

```
PATH=<scratch>/gccbin:$PATH CGO_ENABLED=1 \
  FIRMSCOUT_TEST_DATABASE_URL=… go test ./... -race -count=1
```

All 14 packages reported `ok` and the detector reported no race. The closing-pass
integrator re-ran it once more and got the same result, so this entry rests on more than
one execution.

*What happens without it.* On the machine's own `PATH`, `which gcc cc clang` finds
nothing, and the same command fails to build, verbatim:

```
# runtime/cgo
cgo: C compiler "gcc" not found: exec: "gcc": executable file not found in $PATH
FAIL	github.com/macimottin/firmscout/internal/domain [build failed]
```

So the correct reading is **passed once, here, by hand**. The toolchain is not vendored,
not installed system-wide, not referenced by any script in the repository, and lives in a
scratch directory that will not outlive the session; nobody has observed CI run `-race`
either. The result is real evidence that the code is clean under the detector. It is not
evidence that the project *has* a working race gate, and the two must not be conflated —
a reader who clones this repository cannot reproduce it.

**The PostgreSQL suite runs, and its skips are visible when it does not.** A PostgreSQL
17.4 server is running; every adapter test, the migration up/down test and all seven
end-to-end tests were run against it, with **zero skips**. Without
`FIRMSCOUT_TEST_DATABASE_URL` those 56 tests skip, `go test` still exits 0, and every one
of the 56 skip messages names the variable — checked mechanically, not by sampling:
`go test -v` output was filtered for `--- SKIP` and for `FIRMSCOUT_TEST_DATABASE_URL`
and both counts are 56. A silent skip is the failure mode this check exists to catch,
because a suite that passes by not running is indistinguishable from one that passes.

### Phase 3, the device-first catalogue: what it added, and what it proved by running

Phase 3 exists to answer one question the catalogue previously could not: a company managing
a fleet holds an inventory of **model numbers**, and reaches firmware through them or not at
all. Before this phase, pasting `CRS328-24P-4S+RM` into FirmScout returned nothing, because
the catalogue knew RouterOS and knew nothing about the boxes that run it.

**What was added.** Six hardware models as ordinary `products` rows with `model_identifier`
set, five architecture families, one new table (`product_relationships`) carrying the
`runs_os` edge, two display columns on `product_summaries`, and one partial unique index
closing a family-targeted latest-flag hole that registering the first families made
reachable. No device table, no device flag, no new API route — a device is a product, so it
inherits aliases, categories, summaries and full-text search unchanged (ADR-0024 D1).

**Verified by execution against the development database**, which holds real published
MikroTik RouterOS releases collected from a recorded fixture, plus one open source conflict.
`migrate up` applied `00005`; `registry sync` reported `6 product relationship(s) written; 8
product summary(ies) refreshed.` The API binary was then built and run against that database
on a real socket, and the web app built and served against the API:

| Step a fleet manager takes | Command | Result |
| --- | --- | --- |
| Pastes the code stamped on the chassis | `GET /api/v1/search?q=A42G-HbeP` | `200`, one hit: `mikrotik-hap-be-lite`, `"modelIdentifier":"A42G-HbeP"`, `"matchedOn":"alias"`. The code shares no lexeme with the device's name `hAP be lite`, so the hit can only have come through the `model_number` alias in `aliases_text` |
| Opens the device | `GET /api/v1/products/mikrotik-crs328-24p-4s-rm` | `200`, `"modelIdentifier":"CRS328-24P-4S+RM"`, `"runs":[{"slug":"mikrotik-routeros","name":"RouterOS"}]`, `"aliases":["CRS328-24P-4S+RM"]`, `"firmwareApplicability":{"verified":false,"basis":"runs_os_unverified","ownReleases":{"mapped":false,"releaseCount":0}}`, `"latestRelease":null`. The `aliases` array and the `ownReleases` member are both additions of the repair pass; re-measured 2026-09-06 against the running binary |
| Asks the device for its firmware version | `GET /api/v1/products/mikrotik-crs328-24p-4s-rm/latest` | `404`, by design. The device reports no version of its own |
| Follows `runs` to the firmware | `GET /api/v1/products/mikrotik-routeros/latest` | `200` — **`404` before the repair pass, which broke this step of the journey.** `7.23.5`, long-term, `2026-09-04`. `GET …/releases` also `200` |
| Loads the device page in a browser | `GET http://127.0.0.1:3000/products/mikrotik-crs328-24p-4s-rm` | `200`, rendering `Product code CRS328-24P-4S+RM`, `Runs RouterOS`, and the caveat "FirmScout has not verified which of RouterOS's releases apply to this exact model" |

All six device pages, the operating system page, the vendor page and four model-number search
pages return `200` with **zero server-side errors** in the Next.js log.

**The device facts were spot-checked against the live vendor pages by the integrator**, not
taken on the implementer's word, because house rule 5 makes fabricated device data the worst
available outcome. Three of the six pages were re-fetched independently
(`crs328_24p_4s_rm`, `RB941-2nD`, `hap_ax3`); all three returned HTTP 200 at byte sizes
matching the implementer's report exactly (368434 / 335694 / 425479), and every recorded
value — product code, architecture, operating system, breadcrumb, `<h1>` — matched. The
claim that no device page mentions a 6.x version was re-checked by byte offset: the single
`6.49.18`-shaped match on the CRS328 page sits at offset 154810 inside an SVG path
(`…q.195.165.3.36c…`), and is a coordinate, not a version.

### The honest limitation: per-model firmware applicability is DEFERRED, not evidenced

This is the load-bearing caveat of the whole phase, and it is stated here because a reader
skimming the table above could otherwise conclude more than was proved.

**What is evidenced.** That a given model exists, what its vendor-published product code is,
which architecture MikroTik publishes for it, and that it runs RouterOS. Each of those traces
to a page that was fetched, with the URL, the fetch timestamp and a verbatim excerpt recorded
in the registry document.

**What is not evidenced, and is not claimed anywhere in the data.** Which RouterOS release is
the correct image for a specific model. MikroTik publishes no per-model answer that FirmScout
has measured. The catalogue therefore records **zero** `release_product_mappings` rows for any
device and **zero** family-targeted mappings, and the API states the absence as a value
(`firmwareApplicability.basis = runs_os_unverified`) rather than by omitting a key — because
an absent key reads as "they apply".

**Why the family does not close the gap, despite looking like it should.** The original design
direction assumed architecture is "the axis that decides firmware applicability". Measurement
half-refuted that. Architecture decides which *file* — 17 of 17 fetched devices link an npk
whose architecture token matches their published `Architecture` field exactly. It does not
decide which *version*: every architecture measured, including a 32 MB SMIPS `hAP lite` and a
2007-era MIPSBE `RB433`, is offered the same current 7.24.2. And it is not even sufficient for
the package set: `hAP be lite` and `CRS328-24P-4S+RM` are both `ARM 32bit` and take the same
base image, yet only the former is offered a wireless driver package. So a family here claims
exactly one thing — same base image file — and carries no applicability weight.

A fleet manager gets: *"your `CRS328-24P-4S+RM` is a MikroTik switch that runs RouterOS, and
here is RouterOS."* They do not get: *"install 7.24.2 on it."* The second sentence is the next
stage's work, and stating the gap is strictly better than the alternative failure, which is a
device page silently inheriting the operating system's latest version and telling an operator
to flash an image nobody verified their hardware accepts.

**Also deliberately not done in this phase:** the live `6.49.18` / `7.24.2` source conflict was
left open and untouched. The hypothesis that it reflected a missing device dimension was
refuted by measurement (no fetched device page mentions any 6.x version); its real cause is a
channel misparse in `dataset/sources/mikrotik/changelogs.yaml`, which is a separate change with
its own evidence. Resolving an open conflict as a side effect of a device build is precisely
the silent arbitration ADR-0020 exists to prevent.

### NOT VERIFIED — the explicit list, with reasons

Nothing in this table is claimed to work or claimed to be broken. It is the list of things
a reader might reasonably assume were checked and which were not.

| Gate or claim | Why it is not verified |
| --- | --- |
| **Docker and `docker compose`** — `docker compose … up --build`, the `curl http://localhost:8080/healthz` smoke test, the two Dockerfiles, the Compose stack, the observability stack | **Docker is unavailable in this environment** (WSL integration is off). `docker compose` cannot run. No image has ever been built and no container has ever run, so nothing about the Compose stack — including whether it starts at all — is known. Reported as not verified rather than worked around. Note one thing that *was* checked statically: `grep -n "FIRMSCOUT_REVIEW\|REVIEW" infrastructure/docker/docker-compose.yml` returns nothing, so neither review switch is set by the local stack |
| **The race detector as a project gate** | `go test ./... -race` passes — all 14 packages `ok`, no race — but only with an out-of-tree GCC 13.3.0 toolchain unpacked into a session scratch directory. It is **not vendored, not installed system-wide, not referenced by any script in this repository, and will not outlive the session.** It is evidence that the code is clean under the detector; it is **not** evidence that the project has a working race gate, and the two must not be conflated. On the machine's own `PATH`, `which gcc cc clang` finds nothing and the build fails with `cgo: C compiler "gcc" not found`. A reader who clones this repository cannot reproduce it |
| **Anything fetched from the live internet by the pipeline** | The pipeline has never made a request to a manufacturer's website. Every collector run in every test is against a recorded fixture served by `httptest` on loopback. Compliance *evidence* in `dataset/` and `DATA_SOURCES.md` was measured by a human with `curl` — `robots.txt` files and response headers, never a disallowed path — which is a different activity from the pipeline collecting, and neither implies the other |
| **Dell's catalogue payload facts** (~1.4 MB, gzipped XML, `ETag` + `Last-Modified`, product coverage) as restated in ADR-0018 and `DATA_SOURCES.md` | Deliberately **not re-measured**. Verifying them requires a request to `/catalog/Catalog.xml.gz`, which `robots.txt` disallows — not a `GET`, not a `HEAD`, not a ranged request. They are kept with their original 2026-09-03 attribution. See ADR-0018's follow-through note: those facts can only have been obtained by fetching the path the same ADR declares off-limits, which is a tension inside the project's own flagship compliance example |
| Dell's `authentication_type: none` | A schema default, not a measurement. Establishing it means requesting the disallowed path, and the vocabulary has no member meaning "not established" |
| `sqlc diff` | There is no `sqlc.yaml` and `database/queries/` is empty. Every repository is hand-written SQL over pgx. The gate cannot be run because the tool was never adopted — see §9 |
| `firmscout collector test collectors/config/<vendor>/<source>.yaml` | The CLI has no `collector` command. Collector configs are validated by `scripts/check-schemas.py` and exercised by fixture tests in `internal/adapters/collectors`, which is coverage but not the gate the definition of done names |
| `firmscout registry sync --dry-run` | `registry sync` exists and has no `--dry-run` flag. `registry validate` is the closest equivalent and was run |
| `go test -race ./internal/adapters/postgres/... ./internal/adapters/queue/...` **as written** | `internal/adapters/queue` does not exist; the queue adapter lives in `internal/adapters/postgres` |
| The API served over a real socket | **No longer true as written, and corrected here rather than left standing.** At the close of Phase 3 the API binary was built (`go build -o /tmp/fs-bin/firmscout-api ./apps/api`) and run against the development database on `:8080`; at the close of the repair pass it was rebuilt to the same path and run on `127.0.0.1:8099`. `/healthz` and `/readyz` both answered `200` on both occasions, and every device and `/latest` assertion in the tables above went over that socket with `curl`. What remains unverified is narrower: **timeouts and graceful shutdown**, which no test and no manual step exercised |
| Anything about the API under load, or over TLS | Every request made against the running binary was a single sequential `curl` on loopback, plaintext HTTP. No concurrency, no TLS termination, no reverse proxy |
| The review web pages rendering against a live API | `npm run build` compiles them and `apps/web/lib/review.test.ts` exercises the client against a mocked `fetch`. Nobody has loaded `/review` or `/review/[id]` against a running server. The **public** pages were served this way at the close of Phase 3 — `/`, `/vendors`, `/vendors/{slug}`, `/products/{slug}` for all six devices and the operating system, `/search`, `/status`, all `200` — but the review surface was not among them |
| Any page rendered in an actual **browser** | Pages were fetched with `curl` against `next start` and their HTML inspected. That proves the server rendered without throwing and that the expected strings are in the markup; it says nothing about client-side hydration, layout, or how any of it looks |
| **Which firmware release applies to a specific hardware model** | Deferred, not established — see the limitation section above. FirmScout records that a device runs RouterOS and deliberately records **no** mapping from any release to any device or family. `firmwareApplicability.basis = runs_os_unverified` is the API saying so |
| The three device pages **not** independently re-fetched by the integrator (`hap_be_lite`, `RB433`, `RB1100AHx2`) | Three of six were spot-checked against the live vendor pages and matched exactly. The other three rest on the implementing owner's fetch, which recorded a URL, a timestamp, a byte count and verbatim excerpts for each. Their byte counts were reported consistently with the three that were checked, but they were not re-fetched here |
| Whether the six registered devices are representative of MikroTik's range | They are six models chosen because their pages were fetched, out of an estimated 210–225 real devices among the 561 URLs in `mikrotik.com/sitemap.xml`. Cataloguing the rest is the collector's job in a later phase; nothing here extrapolates from these six |
| The `sort` and `order` parameters behaving as a consumer would expect across pages | They are page-local by construction and now documented as such (§9). No test asserts cross-page ordering, because there is no cross-page ordering to assert |
| Whether any non-FirmScout internal tool still calls `POST /internal/review/items/{id}/{accept,reject}` | Not knowable from this repository. Stated as an open item rather than assumed either way |
| **Seven more `required`-versus-optional divergences between `apps/web/lib/api.ts` and `openapi.yaml`, deliberately left open** | The repair pass corrected the *document* to match what the API sends, and corrected the client for `aliases`, `category` and `ownReleases`. Seven remain, all typed **required in TypeScript and correctly not required in the contract**: `Release.product` (genuinely absent from every `/latest` response — measured), `Release.channel`, `Release.firstObservedAt`, `Release.lastVerifiedAt`, `ReleaseSummary.channel`, `Evidence.retrievedAt`, `Vendor.website`. None is live-wrong today: a live sweep of all eight products found `lastVerifiedAt` and `releaseType` to be the only `Product` keys ever omitted, all five vendors send `website`, and no page reads `latestRelease.product`. They were left because correcting them properly means guarding the release and vendor page renders, which is a sweep through code no defect in this pass touched — a bounded, named piece of follow-up work rather than an unreviewed edit. `Release.product` is the one to do first: it is measurably absent today |
| **`truncateOnRuneBoundary` exists in two copies**, one in `internal/application/queries.go` and one in `internal/adapters/postgres/db.go` | Deliberate, and re-decided rather than inherited. The application copy is unexported, so the adapter cannot call it; the adapter owner specified exporting it as the preferred remedy. The integrator declined: it widens `internal/application`'s exported surface for a six-line string utility, the adapter must hold the byte bound itself in any case, both copies are byte-identical in logic and separately tested, and both bounds are `200`. **Nothing pins the two bounds equal**, which is the residual risk and is stated rather than hidden |
| Whether the corrected `SyncRegistry` retraction ever deletes a row **in the development database** | The retraction paths are pinned by `TestSyncRetractsRelationshipsWhenADocumentDeclaresNone` and its alias sibling, and `registry sync` was run live against the development database (`6 product relationship(s) written; 8 product summary(ies) refreshed.`). But no device document had its `runs:` block deleted and no sync was run to observe the edge disappear from a real database. The behaviour is proved by test, not by operation |
| The race detector, this pass | **Not re-run.** The out-of-tree GCC toolchain described above did not outlive its session, and rebuilding it was not attempted. The entry above stands as evidence from the close of Phase 3, not from the repair pass |
| Any page rendered against the API by the **development** server (`next dev`) | The repair pass rendered `/`, `/vendors`, `/vendors/mikrotik`, `/products/mikrotik-crs328-24p-4s-rm`, `/products/mikrotik-routeros` and `/search?q=CRS328-24P-4S%2BRM` against `next start` on `127.0.0.1:3123`, pointed at the live API, and read the HTML. `next dev` refuses to start a second instance while another holds its port, so nothing was checked under the dev server's own rendering path |

## 2. Diagrams and the code that implements them

29 diagrams. The middle column names the packages a reader should open to see the
diagram in code; "conceptual" means the diagram documents an intended design with no
implementation behind it yet.

| Diagram | Implementing code | State |
| --- | --- | --- |
| [system-context](../diagrams/system-context.md) | whole repository | partial: manufacturer sources and GitHub are real; CVE sources, AWS and the AI provider are not connected |
| [high-level-architecture](../diagrams/high-level-architecture.md) | `apps/*`, `internal/*` | implemented except the AI agent box |
| [clean-architecture](../diagrams/clean-architecture.md) | `internal/{domain,application,adapters,platform}`, enforced by `internal/archtest` | **implemented and enforced** |
| [module-dependencies](../diagrams/module-dependencies.md) | `internal/archtest/arch_test.go` | **implemented and enforced** |
| [bootstrap-flow](../diagrams/bootstrap-flow.md) | `dataset/`, `internal/application/registry.go`, `internal/domain/source.go` | conceptual: no discovery agent exists. The *refusal* the diagram describes is real and now demonstrated — Dell is registered, disabled, and `registry sync` names robots.txt as its reason |
| [update-decision-tree](../diagrams/update-decision-tree.md) | `internal/application/{checksource,ingest,review}.go`, `internal/domain/validation.go`, `internal/adapters/fetch` | implemented including gate 10 and the human decision edge; AI escalation conceptual |
| [adaptive-scheduling](../diagrams/adaptive-scheduling.md) | `internal/domain/scheduling.go` | implemented; the defaults are untuned |
| [source-repair-flow](../diagrams/source-repair-flow.md) | — | conceptual |
| [ai-cost-escalation](../diagrams/ai-cost-escalation.md) | — | conceptual |
| [release-state-machine](../diagrams/release-state-machine.md) | `internal/domain/candidate.go` | **implemented**, illegal transitions rejected |
| [source-health-state-machine](../diagrams/source-health-state-machine.md) | `internal/domain/source.go` | **implemented**, `retired` is terminal |
| [request-protection-flow](../diagrams/request-protection-flow.md) | `internal/adapters/httpapi/{middleware,ratelimit}.go` | partial: in-process rate limiting only, no edge, no abuse scoring |
| [api-sequence](../diagrams/api-sequence.md) | `internal/adapters/httpapi`, `internal/application/{queries,entitlements}.go`, `internal/domain/partialdate.go`, `internal/adapters/postgres/release_repo.go` | partial: auth, quota, metering, `Vary` and plan-windowed history exist — the window now compares a reduced-precision date at its period end in SQL as well as in the domain; CloudFront, WAF, API Gateway and billing events do not. Three of `api.md` §2's ten `/api/v1` rows (`advisories`, `POST /lookup`, `usage`) are design and not routed |
| [collector-sequence](../diagrams/collector-sequence.md) | `internal/application/{checksource,ingest}.go`, `internal/adapters/collectors` | **implemented** for all three engines and covered end to end |
| [data-model](../diagrams/data-model.md) | `database/migrations/0000{1,2}_*.sql`, `internal/adapters/postgres` | implemented for 32 tables; CVEs, lifecycle and corrections deferred |
| [deployment](../diagrams/deployment.md) | — | conceptual, nothing deployed. `infrastructure/terraform/` is an **empty directory** (`find infrastructure/terraform -type f` returns nothing), not a skeleton, which is what this row used to call it |
| [contribution-flow](../diagrams/contribution-flow.md) | `.github/workflows`, `CONTRIBUTING.md` | implemented except staging validation |
| [observability](../diagrams/observability.md) | `internal/adapters/telemetry`, `infrastructure/observability` | code implemented; the stack has never been started |
| [product-analytics](../diagrams/product-analytics.md) | `internal/domain/events.go`, `internal/platform/wire.go` | partial: events are emitted to logs, the privacy filter and analytics tables are not written |
| [cost-aware-request](../diagrams/cost-aware-request.md) | `internal/adapters/httpapi/middleware.go`, `product_summaries` | partial: the precomputed-summary path is real, the CDN is not |
| [storage-lifecycle](../diagrams/storage-lifecycle.md) | `internal/adapters/postgres/artifact_repo.go`, `internal/adapters/artifact` | partial: content addressing and deduplication implemented, the retention sweep is not |
| [cost-containment-incident](../diagrams/cost-containment-incident.md) | — | conceptual runbook |
| [cve-correlation](../diagrams/cve-correlation.md) | — | conceptual |
| [version-normalization](../diagrams/version-normalization.md) | `internal/domain/version.go` | **implemented**, including the plausibility check |
| [alias-resolution](../diagrams/alias-resolution.md) | `internal/adapters/postgres/product_repo.go`, `internal/application/ingest.go` | implemented for exact and alias matching; trigram fuzzy matching is in search only |
| [multi-source-conflict](../diagrams/multi-source-conflict.md) | `internal/domain/conflict.go`, `internal/application/ingest.go` (`reconcileConflict`), `internal/adapters/postgres/conflict_repo.go` | **implemented**, except the same-tier recency and confidence tie-breakers, which [ADR-0020](../adr/0020-multi-source-conflict-detection.md) declines to build |
| [entitlement-evaluation](../diagrams/entitlement-evaluation.md) | `internal/application/{queries,entitlements}.go`, `internal/adapters/httpapi/middleware.go` | partial: quota evaluation and plan-based history windowing implemented, plan management and billing are not |
| [dataset-correction](../diagrams/dataset-correction.md) | — | conceptual |
| [review-prioritization](../diagrams/review-prioritization.md) | `internal/domain/review.go`, `internal/application/{review,review_queries}.go`, `internal/adapters/httpapi/review_handlers.go`, `apps/cli/main.go`, `apps/web/app/review` | **implemented**, minus three scoring inputs the diagram marks as not built. Decisions are the CLI's alone (`review accept`/`reject`); `apps/web`'s pages are read-only by construction; `review show` is the only production reader of a conflict's parked candidates |

## 3. Components implemented versus conceptual

**Implemented and exercised by tests:** the domain (entities, value objects, both state
machines, the ten validation gates, adaptive scheduling, the conflict assessment and the
review scoring); the application layer (ports, the check/extract/validate/publish
pipeline, conflict reconciliation, the review read and decision use cases, registry sync,
the read-side queries, plan entitlements); the PostgreSQL adapter (fourteen repository, queue and store types — `AuditRepo`, `CandidateRepo`, `CategoryRepo`, `ConflictRepo`, `EvidenceRepo`, `ProductRepo`, `Queue`, `ReleaseRepo`, `ReviewRepo`, `SourceRepo`, `SummaryRepo`, `UsageRepo`, `VendorRepo` and `ArtifactStore` — the
migration runner, the `SKIP LOCKED` queue, the observation projection, the conflict and
audit tables); the guarded fetcher; content normalisation and section hashing; the three
configuration-driven collector engines and the fixture harness; the content-addressed
artifact store; the HTTP API with its presenters, middleware and the switched-off
internal reviewer surface; OpenTelemetry setup; the three binaries; the Next.js site
including the review pages, which are read-only by construction.

**Where "implemented" is weaker than it reads.** Nothing here has been deployed: no image
built, no container run, no process bound to a real port. `implemented` in this section
means the code exists and its tests pass on a developer machine. Three specific places
where an intent is stronger than its implementation, recorded rather than rounded up:
extraction has no wall-clock deadline and no concurrency cap even though the fetch side
has both; the API-key *generation* format (`fs_live_<id8>_<secret>`) is specified and has
no producer, so only the verification half exists; and key comparison is a `WHERE
key_hash = $1` equality in PostgreSQL, which is fine for a 256-bit secret but is not the
constant-time comparison `security.md` §4.6 used to describe.

**Conceptual — designed, documented, not built:** all five AI agents (ports and schemas
only, no runtime); CVE correlation and security advisories as a data model; lifecycle and
EOL tracking; the community correction workflow; billing events and plan management;
webhooks and alerts; inventory upload; SSO and RBAC; the entire AWS deployment.

## 4. Security controls: implemented versus planned

| Control | State |
| --- | --- |
| SSRF guard: scheme restriction, blocked CIDRs, address validation at `net.Dialer.Control`, redirect revalidation | **implemented**, tested against loopback, link-local, RFC1918, CGNAT, IPv6 unique-local and IPv4-mapped IPv6 |
| Destination port allow-list | **implemented** (relaxing it requires constructing the guard differently, not configuration) |
| robots.txt fetched, parsed, cached, honoured | **implemented** |
| Content size limit, MIME validation, timeouts | **implemented** |
| Per-host concurrency limiting and `Retry-After` | **implemented** |
| Compliance gating before dispatch | **implemented** in the domain, the dispatch query and a partial index; a test asserts no committed source is collectable |
| RE2-only regex in collector configs | **implemented** (Go's `regexp` has no backtracking) |
| Collectors structurally unable to publish | **implemented**, enforced by `archtest` |
| API keys hashed at rest with a display prefix | **partially implemented, and the missing half is the producing one.** Verification exists end to end: the middleware SHA-256s the presented token and `UsageRepo.ResolveByHash` looks it up, with `Create`, `Revoke` and `TouchLastUsed` alongside. Nothing anywhere generates a key — `grep -rn "fs_live\|crypto/rand" internal apps --include=*.go` (excluding tests) returns only request-id and ULID generation, never the `fs_live_<id8>_<secret>` format `security.md` §4.6 specifies. So the format is a specification with no producer, and comparison is a database equality on a digest rather than the constant-time comparison that section used to claim. |
| Distinct 401 for "no credential" and "revoked or unknown key" | **implemented** ([ADR-0022](../adr/0022-canonical-problem-types.md)); a client can tell which one it hit |
| Cache partitioning by entitlement (T-13): `private, no-store` on every response to an authenticated caller, plus `Vary: Authorization, X-API-Key` on every cacheable response, 304 included | **implemented**, and the implementation changed during this integration. `cachePolicyFor` now replaces the route's public policy with `private, no-store` whenever the caller presented a credential, which is the control [security.md §4.10](security.md#410-cache-and-rendering-threats-t-13-t-14) actually asks for; `Vary` is the second line, not the first. Observed directly: `/releases` and `/vendors` answer an anonymous request with `public, max-age=300` / `public, max-age=3600` and the same request bearing a key with `private, no-store`. Two committed tests pin the authenticated branch: `TestAuthenticatedResponsesAreNotStorable` (which proves the premise first, by showing the same URL yields two different bodies to two callers) and `TestNoCacheableRouteIsPublicForAKeyedCaller` (every cacheable route plus the 304 path). Both were verified to fail when `cachePolicyFor` is reverted. An earlier draft of this row recorded a gap here; that draft was written before those tests landed and was wrong when read. |
| The internal reviewer surface is off by default and behind **three** switches on **two** hosts | **implemented, and this row previously undercounted them.** On the API server: `FIRMSCOUT_REVIEW_API_ENABLED` defaults to false *and* all three use cases must be non-nil, or the routes are not registered at all. On the public web app: `FIRMSCOUT_REVIEW_UI_ENABLED`, unset by default, which gates a read-only viewer of the same data in a different process on a different host. The surface has no API key, no quota, no metering and no cache headers, and must never face the internet ([ADR-0021](../adr/0021-asserted-reviewer-identity.md), [api.md §11](api.md)). The undercount is not a wording slip — it is the ledger's half of the confused-deputy defect recorded in §10 and §9 item 14. |
| The public web app cannot write to the reviewer surface | **implemented structurally, not by policy.** `apps/web/lib/review.ts` exposes only a `GET` client: `InternalRequestOptions` has no `method`, `headers` or `body` field and `internalFetch` hardcodes `method: "GET"`. A repository-wide grep for `use server`, `method: "POST"`, `decideReviewItem`, `acceptReviewItem` and `rejectReviewItem` across `apps/web` returns only three comment lines saying the write path was deliberately removed. Accept and reject are `apps/cli`'s alone, calling `application.DecideReviewItem` against the database rather than the HTTP surface. |
| Audit event write paths | **implemented for review decisions**: every accept and reject writes an `audit_events` row with the actor, the reason, the before and after state, and `actor_authenticated = false`. Nothing else writes the table yet. |
| Reviewer authentication | **not implemented and honestly recorded rather than faked.** There is no login. The actor on a decision is a name the caller asserted in `X-FirmScout-Actor` and nobody verified. This is the reason the surface is off by default. |
| `govulncheck`, gitleaks, CodeQL, dependency review | **implemented** in `.github/workflows/security.yml` and no longer able to pass by doing nothing. Never observed running: no CI run has been watched from this environment |
| GitHub Actions pinned to commit SHAs | **implemented**: across all four workflows, `grep -h "uses:" .github/workflows/*.yml \| grep -vc "@[0-9a-f]\{40\}"` returns 0 — every `uses:` names a 40-character SHA |
| Extraction deadlines and concurrency caps | **partial, and asymmetric.** The fetch side has both (`DefaultTimeout = 30s`, `DefaultPerHostConcurrency = 2`); the extraction side has neither. `grep -rn "WithTimeout\|WithDeadline\|Deadline" internal/adapters/collectors/*.go internal/adapters/normalize/*.go` excluding tests returns nothing, so a pathological document is bounded by its byte size and by nothing else. |
| Readiness probe actually probing | **implemented**: `apps/api/main.go:125` passes `Ready: c.DB.Ping` as the `/readyz` gate. It previously passed nothing, so `/readyz` answered 200 from an instance whose database was unreachable. |
| Decompression ratio limits | **implemented, and this row said the opposite.** `internal/adapters/fetch/fetcher.go` enforces both: a byte cap (`DefaultMaxBytes = 16 << 20`) and a decompressed:compressed ratio cap (`DefaultMaxDecompressionRatio = 100`, applied above a 1 MiB floor so a small body cannot trip it on rounding). `TestFetchDecompressionIsCapped` and `TestFetchDecompressesWithinTheCap` cover the byte cap on a decompressed body; the **ratio** branch specifically has no test of its own, which is the honest remaining gap — implemented and unexercised, not absent. |
| Prompt-injection defence | conceptual (no agent runtime exists) |
| Row-level security for tenant isolation | planned |
| Release signing and build provenance | planned |
| CloudFront, WAF, Shield | planned; no AWS account exists |

## 5. Observability: implemented versus planned

**Implemented:** OpenTelemetry traces, metrics and logs with OTLP export; a Prometheus
exporter so `/metrics` works without a collector; W3C trace context propagation
including through the job queue; `slog` JSON with trace and span correlation; the metric
name catalogue as Go constants shared with the alert rules and dashboards; graceful
degradation when no collector is reachable.

**Written but never run:** the OpenTelemetry Collector configuration with its secret
redaction and tail sampling, Prometheus with 13 alert rules
(`grep -c "^\s*- alert:" infrastructure/observability/prometheus/alerts.yml` → 13), Loki,
Tempo, and Grafana with 7 dashboard files — three fully specified and four stubs. Nothing
in `infrastructure/observability/` has ever been started, because starting it needs Docker.

**Planned:** the analytics privacy filter and its tables (events currently go to logs
only), the remaining four dashboards, and continuous profiling. The three Phase 2 domain
events — a conflict detected, a conflict resolved, a review item resolved — are published
through the same log-only publisher as every other event, so nothing alerts on them.

**One event that was defined and never emitted is now emitted.** `VendorViewed` had a
constant, a publisher in `application.GetVendor` and no caller, because the HTTP handler
read the repository directly — so for the whole of Phase 2 the only signal FirmScout had
about which vendors readers open produced nothing. The handler now goes through the use
case and `TestVendorViewIsPublishedOnce` fails if it stops. The event still lands in the
log-only publisher along with every other, so nothing aggregates it yet.

## 6. Cost controls: implemented versus planned

**Implemented:** conditional requests with ETag and Last-Modified, so an unchanged
source transfers no body and runs no parser; targeted section hashing, so a page whose
furniture rotates does not trigger extraction; content-addressed artifact deduplication,
so unchanged content is stored once; precomputed product summaries, so a product page is
one indexed read; adaptive scheduling; bounded search input; per-host concurrency limits;
plan-based history windowing applied as a query bound rather than a filter over a fetched
page, so a windowed caller never reads rows it is not entitled to — and the bound is now
correct as well as cheap: it compares the end of the period a reduced-precision date
denotes, so it no longer silently drops the releases it was meant only to window.

**Planned:** the retention sweep, S3 lifecycle policies, AI budgets and circuit breakers,
CDN caching, and every AWS-specific control.

## 7. Decisions requiring legal review

Unchanged from [ADR-0009](../adr/0009-code-and-data-licensing.md) and
[ADR-0018](../adr/0018-source-compliance-policy.md); itemised in
[licensing.md](licensing.md). The ten questions are all open. Five are now concrete
because the registry entries exist — and until Phase 2's closing pass, three of the five
did not. **The previous version of this section repeated `DATA_SOURCES.md`'s claim that
Dell was "registered and disabled". No such row existed anywhere in `dataset/`.** The
project's flagship honesty demonstration — *it stays in the registry, visibly disabled* —
was described in four documents and performed in none of them. The rows now exist and the
counts below are the tool's own output, not prose:

```
$ go run ./apps/cli registry validate
Registry is valid: 5 vendor(s), 7 category(ies), 0 family(ies), 2 product(s), 6 source(s).
0 of 6 source(s) are currently collectable; the rest await a compliance decision (ADR-0018).
```

- **Collecting from a host whose robots.txt disallows it.** Dell **is now registered and
  disabled** — `dataset/vendors/dell.yaml` and `dataset/sources/dell/catalog.yaml`, with
  `robots_policy_status: disallowed`, `terms_review_status: pending`, `enabled: false`,
  `health: disabled`. The verdict is mechanical rather than a judgement: `downloads.dell.com/robots.txt`
  was re-measured on 2026-09-05 and its four lines are `User-agent: *` / `Allow: /manuals` /
  `Allow: /topicspdf` / `Disallow: /`, so the only rule matching `/catalog/Catalog.xml.gz`
  is the disallow. `registry sync` names robots.txt in Dell's reason and in no other
  source's. The platform cannot currently be made to fetch it, by configuration or by flag.
  **No Dell product is registered**, and ADR-0018's claim that "its products" were has been
  corrected there: establishing which products the catalogue covers means reading the
  catalogue, which is the fetch robots.txt forbids. The source is registered
  catalogue-scoped with an empty `ProductID`.
- **Using an undocumented vendor update API.** Ubiquiti **is now registered** —
  `dataset/vendors/ubiquiti.yaml`, `dataset/sources/ubiquiti/firmware-latest.yaml` —
  `enabled: false`, `terms_review_status: pending`, `robots_policy_status: allowed`
  (`fw-update.ubnt.com/robots.txt` answers HTTP 404, which RFC 9309 §2.3.1.3 and this
  project's own `RobotsCache` both treat as a full allow). The previous version of this
  section said it "has not been registered"; that is no longer true. Its terms still have
  not been reviewed, which is the part that keeps it uncollectable.
- **Poly/HP.** Registered at the documentation portal root
  (`dataset/sources/poly/documentation-portal.yaml`), `robots_policy_status: allowed`
  measured through the `docs.poly.com → support.hp.com` redirect the RFC requires be
  followed, `health: relocated`, `enabled: false`, terms pending. It is registered at the
  **portal root and not at a release-notes document** because `support.hp.com` returns a
  byte-identical 7087-byte SPA shell with the same `ETag` for every path, including an
  invented one — so an HTTP 200 there is no evidence a document exists, and HP's declared
  sitemaps do not index `/bundle/`.
- **Fortinet's PSIRT advisory feed.** `enabled: false`, `terms_review_status: pending`.
  `robots.txt` was measured and permits the registered path — a mechanical fact. Fortinet's
  "Limited License" clause is not a mechanical fact and has not been read by anyone
  qualified.
- **MikroTik.** Two sources, both `enabled: false`, terms pending. This is the vendor
  §11 recommends enabling first, and enabling it is a terms decision nobody has made.

**A tension inside the project's own compliance example, recorded rather than resolved.**
ADR-0018 states measured payload facts about Dell's catalogue (1.4 MB, well-formed, serving
both `ETag` and `Last-Modified`, covering BIOS, drivers and firmware). Those can only have
been obtained by fetching the path the same ADR declares off-limits. The closing pass did
**not** re-fetch it in any form and did not delete the facts either; they are kept with
their 2026-09-03 attribution and the provenance is now stated in ADR-0018's follow-through
note and in `DATA_SOURCES.md`. The honest reading is that the evaluation which produced the
policy predates the policy.

## 8. Decisions requiring product input

1. **Enabling any source at all.** All **six** ship `enabled: false` with
   `terms_review_status: pending` (`grep -rn "^  enabled:" dataset/` returns six lines and
   every one of them is `false`). Someone must read the terms and decide. Until then
   FirmScout collects nothing. Two of the six are additionally blocked on something a
   terms decision cannot lift: Dell on `robots.txt`, and Ubiquiti on the `json_path`
   collector engine, which is reserved in the config schema and not implemented — adding
   its config today fails `TestLoadDirLoadsTheShippedConfigs` with
   `spec.engine: "json_path" is a reserved roadmap engine that the MVP loader does not implement`.
2. **The GitHub organisation.** The module path assumes `github.com/macimottin/firmscout`.
3. **Conduct and security contact addresses**, left as `TODO:` in `CODE_OF_CONDUCT.md`
   and `SECURITY.md`.
4. **Plan pricing and the value metric.** The FinOps document recommends charging by
   products monitored rather than request volume; no prices are set.
5. **~~How much release history the free tier sees.~~ Decided and implemented.** Anonymous
   and Free callers see the most recent twelve months; Professional and above see the
   whole archive. The response says which it returned, in a `window` object, so a
   truncated archive never looks complete. The *boundary* is now code; the *price* of
   crossing it is still item 4.
6. **Whether the epoch in MikroTik's pointer file may ever be published as a date.** The
   collector currently publishes no date from it, on the grounds that the vendor does
   not document what it means.
7. **Registering `firmscout.dev` and `api.firmscout.dev`.** [ADR-0019](../adr/0019-public-domain-shape.md)
   settles the *shape* of the public URLs; the domain itself is not registered and no
   DNS zone exists.
8. **Who may decide a review item, and from where.** The reviewer surface performs
   unauthenticated writes, so today the answer is "whoever can reach the process".
   That is acceptable only while it is off. Turning it on for anyone outside a laptop
   requires deciding on an authentication mechanism first ([ADR-0021](../adr/0021-asserted-reviewer-identity.md)).
9. **Whether the `POST /internal/review/items/{id}/{accept,reject}` endpoints should exist
   at all.** They are unauthenticated and rely entirely on network placement, and the CLI
   no longer needs them: `firmscout review accept` and `review reject` call the use case
   directly. Removing them would delete the whole confused-deputy class rather than
   documenting around it. That is a code and architecture decision nobody has made, and
   `api.md` §11 and `security.md` §4.11 both raise it rather than assume it.

## 9. Known limitations

> **Open correctness defect: the job queue can lease one job to two workers at once.**
> Measured on a 28-core machine under 56 spinners: roughly one run in 600 of
> `TestConcurrentDequeuesDoNotOverlap` ends with all twenty jobs leased twice. The
> diagnostic captured at failure is `per-worker counts: [0 20 0 20]` with every row at
> `status=running attempts=2` — two workers each took the whole queue, the second one
> leasing rows that were already running under a lease with fifty-nine seconds left. That
> is not the at-least-once redelivery this design accepts and handlers are idempotent
> for; it is two workers checking the same manufacturer source simultaneously.
>
> **The root cause is not known.** Two hypotheses were tested and refuted by measurement:
> repeating the eligibility predicate in the outer `UPDATE` (2 failures in 1200 runs
> after), and marking the CTE `AS MATERIALIZED` (0 failures in 1200, then 3 in the next
> 1800 — the clean run was luck, and the change was reverted rather than left implying a
> fix). The repeated predicate was kept because it is correct on its own terms.
>
> This is recorded here rather than in a comment alone because it has already been
> mistaken for a flaky test once and the test weakened on that reading. The test is a
> real detector. It is failing because the queue is wrong.


The previous report listed ten. Four are closed, one is half closed, five stand. They are
renumbered here, with the old numbering named so the two reports can be compared.

**Closed in Phase 2's closing pass**

- ~~**The public web app could publish to the catalogue from the internet.**~~ **Closed,
  structurally.** This is the phase's most serious security finding and it appeared
  nowhere in this document until now — not in §4's control table, not in this list, not in
  §10. `apps/web` carried `lib/review.ts#decideReviewItem` and a Next.js Server Action
  bound to the review page's Accept and Reject buttons, which issued
  `POST /internal/review/items/{id}/{accept,reject}` from `apps/web`'s **own server**. Both
  halves of a deployment could follow their documented story exactly — API server on a
  private subnet with `FIRMSCOUT_REVIEW_API_ENABLED=true`, public site with
  `FIRMSCOUT_REVIEW_UI_ENABLED=true` — and the composite would be an internet-reachable,
  unauthenticated publish, because a client that proxies for a surface necessarily sits
  somewhere that can reach it. Closed by **deleting the write path from `apps/web`** rather
  than warning about it: `InternalRequestOptions` has no `method`/`headers`/`body`,
  `internalFetch` hardcodes `method: "GET"`, and accept/reject moved to `apps/cli`, which
  calls `application.DecideReviewItem` against the database and has never depended on the
  HTTP surface running. See §10 for how it was found and §4 for what the controls are now.
- ~~**`FIRMSCOUT_REVIEW_UI_ENABLED` was named in no architecture document.**~~ **Closed.**
  It had existed in `apps/web` since the review UI was built, while ADR-0021, `api.md` §11,
  `docs/diagrams/api-sequence.md` and this report all described exactly two switches, both
  on the API server. All four now name three, on two hosts, with the host and the default
  for each.

**Closed since the last report**

- ~~*(old 2)* **The multi-source conflict gate has nothing to compare.**~~ **Closed.**
  `source_observations` records what each eligible source currently claims, gate 10
  compares against it, an unresolved disagreement becomes a `source_conflicts` row and
  one review item, and the whole path is proved end to end against a real database by
  `TestTwoSourcesDisagreeAndTheDecisionReachesAHuman`. The same-tier tie-breakers remain
  deliberately unbuilt (ADR-0020).
- ~~*(old 6)* **The review queue has no interface.**~~ **Closed.** `ListReviewQueue` and
  `GetReviewItem` read it, `DecideReviewItem` closes it, four `/internal/review` routes
  serve it, two Next.js pages render it, and `firmscout review list` prints it. The HTTP
  surface is off by default.
- ~~*(old 8)* **Problem-type URIs diverge between `api.md` and the implementation.**~~
  **Closed.** One catalogue of eleven URIs, taken from `api.md`, with three renames and
  two additions. `TestProblemTypesAreCanonical` fails if a pre-Phase-2 spelling comes
  back. See ADR-0022.
- ~~**`/readyz` never probed anything.**~~ **Closed this session.** Not in the old list
  because nobody had noticed it.
- ~~**Official source lists and a release's `source`/`evidence` were absent rather than
  fabricated; `hasSourceConflict` carried no detail.**~~ **Closed this session.** This
  was worse than a documentation gap: `PresentRelease` never set `dto.Source` or
  `dto.Evidence` at all, so `GET /releases/{id}` returned an HTTP 500 in production —
  found by loading the release detail page, not by reading a diagram. `Release.source`
  and `Release.evidence` on every release response (`GET /releases/{id}`, a product's
  history, and the embedded `latestRelease`) are now joined from the real `evidence` and
  `sources` rows through `releases.evidence_id` (`release_repo.go`'s `releaseColumns` /
  `releaseJoins`), guaranteed non-null by that column's `NOT NULL ... ON DELETE RESTRICT`
  rather than treated as optional. `Product.officialSources` is a real, non-empty list of
  the sources that have actually contributed a currently-mapped, non-withdrawn release —
  not every source registered for the vendor — computed by `RefreshProductSummary`'s
  `official_sources` JSONB column (migration `00004`). And `hasSourceConflict` gained a
  sibling, additive field: `conflict` (`Product` and `LatestReleaseResponse`) now carries
  the open conflict's real channel, disputed versions, source count and detection time,
  read from `source_conflicts` via the same migration's four `conflict_*` columns, `null`
  whenever there is nothing real to report rather than a fabricated placeholder. All three
  render through one shared mapping, `domain.PublicSourceKind`, so a release's `source.kind`
  and a product's `officialSources[].kind` can never disagree about the same underlying
  source. Proved end to end against a real database by
  `TestTwoSourcesDisagreeAndTheDecisionReachesAHuman`'s step 4a (`internal/integration/slice_test.go`),
  which reads a real open conflict and a real published release's source back through the
  public API — sabotaged by removing each field from the presenter, which failed the test
  both times, then reverted. See `docs/architecture/api.md` §3.2–§3.4 and item 5 below.
- ~~**`GET /releases/{id}` crashed a second, previously undiagnosed way even after the
  fix above landed.**~~ **Closed this session, found only by loading the page against
  the real dev database.** `handleGetRelease` always called `PresentRelease(release,
  nil)`: unlike a product's own history or its latest release, this one request path
  resolves a release with no product already in hand, so `product` was always nil and,
  with `ReleaseDTO.Product` carrying `omitempty`, always an omitted key — and the
  release page dereferences `release.product.slug` with no guard, same as the
  `source`/`evidence` crash this closed pass fixed, just for a field the task's own
  background did not name. Closed by a new `ReleaseRepository.ProductRefForRelease`
  (`release_repo.go`), a plain join over `release_product_mappings` and `products` that
  `handleGetRelease` now calls, ordered by slug so a release mapped to more than one
  product resolves the same way every time. Proved by
  `TestGetReleaseCarriesARealProduct` (`internal/adapters/httpapi/handlers_test.go`) and
  `TestProductRefForRelease` (`internal/adapters/postgres/ingest_test.go`, against a real
  database) — both sabotaged and confirmed to fail before this fix, then reverted — and
  by curling the real dev database: `GET /releases/{id}` and `curl -o /dev/null -w
  '%{http_code}' http://localhost:3000/releases/{id}` both returned a working page only
  after this fix, not after the `source`/`evidence` fix alone.

**Half closed**

1. **`ProductSummary.HasSourceConflict` is now set; `AdvisoryCount` is still never set.**
   *(old 3.)* `RefreshProductSummary` derives the conflict flag from
   `EXISTS (… source_conflicts … state = 'open')`. There is still no advisories table, so
   `advisory_count` is preserved rather than reset on refresh and will read 0 forever
   until Phase 4 builds one.

**Still open**

2. **Five vendors, two products, six sources, none enabled.** *(old 1, moved forward
   rather than closed; the counts are `registry validate`'s own, re-run for this report.)*
   The catalogue is empty by design. Everything that has run end to end has run against a
   recorded fixture on loopback. FirmScout has still never fetched from a manufacturer.
   Two of the five vendors carry no product at all — Dell because establishing coverage
   means reading a catalogue robots.txt forbids, Ubiquiti because its endpoint is
   catalogue-scoped over 224 opaque vendor codes like `2WA` and seeding a public catalogue
   with 224 unmapped codes would be worse than an empty field.
3. **Rate limiting is per instance.** *(old 4.)* With N API instances the effective limit
   is N times the configured one. Acceptable at MVP instance counts, and documented in the
   middleware.
4. **Quota enforcement fails open** when the usage store errors, on the reasoning that a
   metering outage should not become a customer outage. *(old 5.)*
5. **Two OpenAPI fields remain absent rather than fabricated** where the read model
   cannot supply them: vendor product counts and family slugs. The API omits the key
   rather than inventing a value. *(old 7.; narrowed this session — official source
   lists and the product/source objects on a release, both listed here previously, are
   now populated rather than absent; see "Closed since the last report" above.)*
6. **`go list ./...` picks up a vendored Go file inside `apps/web/node_modules`.** *(old
   9.)* It is harmless and absent from a fresh checkout, but
   `apps/web/node_modules/flatted/golang/pkg/flatted` appears in every local `go build`
   and `go test` listing, which means the Go toolchain is compiling a JavaScript
   package's vendored sample code.
7. **The adaptive scheduling defaults are guesses.** *(old 10.)* They are labelled as such
   throughout and should be tuned against measured publication cadence.

**New**

8. **No Fortinet advisory can be published by the pipeline, and that is the design.**
   The PSIRT feed states that an advisory exists and its date, and names no affected
   product, so the collector config scores every candidate 0.55 against a 0.85 threshold
   and gate 8 routes all of them to a human. Advisory-to-product correlation is Phase 4.
   Until then the advisory release type is exercised end to end but produces no published
   release except by explicit human acceptance — and even then an advisory is never
   `is_latest_observed`, because `Release.EligibleForLatest` excludes it.
9. **An unwired conflict port fails silently.** `IngestDeps.Conflicts` is optional, so a
   deployment that omits it does not error: gate 10 becomes a gate that can only pass, and
   the pipeline publishes contested versions while recording "all gates passed". This is
   exactly the state `internal/platform/wire.go` was in when Phase 2's seven work streams
   landed. `internal/platform/wire_test.go` now fails if the ports stop reaching the use
   case, but the port itself is still optional by type.
10. **`sqlc` was chosen in ADR-0004 and never adopted.** There is no `sqlc.yaml`,
    `database/queries/` is empty, and every repository is hand-written SQL over pgx. The
    definition of done lists `sqlc diff` as a gate that therefore cannot be run. Either
    ADR-0004 should be superseded or the tool should be adopted; leaving a chosen tool
    absent means one of the two documents is lying to the next contributor.
11. **Three definition-of-done gates name things that do not exist:**
    `firmscout collector test`, `firmscout registry sync --dry-run`, and the package
    `internal/adapters/queue` in the race-detector gate. The document is a gate list; a
    gate nobody can run is indistinguishable from a gate nobody ran.
12. **The audit trail records the actor but nothing verifies it.** Recorded honestly in
    the schema and in ADR-0021, and the surface is off by default, but it remains true
    that anyone who can reach the process can write any name into the audit trail.
13. **The fetcher ignores `X-RateLimit-*` headers that sources are already sending.**
    Measured 2026-09-05: every response from `upgrade.mikrotik.com` now carries
    `x-ratelimit-limit` and `x-ratelimit-remaining`. `internal/adapters/fetch` honours
    `Retry-After` and reacts to a 429, but reads neither header, so a source that is
    publishing its own budget is only understood after FirmScout has spent it and been
    refused. Backing off on a falling `x-ratelimit-remaining` is strictly politer than
    discovering the limit by hitting it, and this is the first pilot source observed
    advertising one. Recorded, not fixed.
14. **The `POST /internal/review/items/{id}/{accept,reject}` endpoints are unchanged:
    still present, still unauthenticated, still relying on network placement alone.** The
    confused-deputy path to them is closed — no FirmScout-authored client calls them from
    a public deployment — but the endpoints themselves were not touched, because deleting
    HTTP routes is an architecture decision and not one a repair pass makes unasked. The
    CLI no longer needs them, which is the argument for removing them; see §8 item 9.
    Whether some other internal tool still calls them is not knowable from this repository.
15. **`sort` and `order` on release history are page-local.** Pagination is always by
    first-observed time descending, which is the pair the cursor encodes; the handler
    re-sorts each fetched page afterwards. So `order=asc` returns the newest page first
    with its rows ascending inside it. The same is true of `channel` and `releaseType`,
    which filter after the cursor is minted, so a filtered page can be empty while
    `nextCursor` is non-null. All of this is now stated in `api.md` §2 and in
    `openapi.yaml` rather than implied away; making `sort` real needs a query ordered by
    the requested key with a cursor built from it, which is a repository change.
16. **`ReleaseHistoryQuery` carries only `Slug`, `Limit`, `Cursor` and `Plan`.** The four
    documented parameters `channel`, `releaseType`, `sort` and `order` are applied in
    `internal/adapters/httpapi/handlers.go` and nowhere else, so a non-HTTP caller cannot
    filter or sort a history at all. This is the general shape the closing pass kept
    finding — the handler as the de-facto application layer — and it is the one instance
    of it left open, because closing it means changing `ReleaseListOptions`, the
    `ReleaseRepository` port, the SQL and the handler together.
17. **`covers_products` is parsed and then ignored.** `internal/adapters/registry/yaml.go`
    reads it into `RegistryDocument.SourceProducts`, and `SyncRegistry` never writes the
    `source_products` rows it describes. The table, the port method
    (`SourceRepository.ProductsForSource`) and the adapter query all exist and are tested
    directly; nothing in production populates the table and no use case reads it. A
    contributor filling the field in today gets no effect and no warning. Now labelled as
    inert in `internal/application/registry.go` and in `packages/schemas/source.schema.json`
    rather than left to be discovered.
18. **The compliance vocabulary cannot express "not established".** `authentication_type`
    is `none|api_key|account_required|entitlement_required`, and `none` is also the column
    default, so a source whose authentication could not be measured — Dell, because
    measuring means fetching a disallowed path — is indistinguishable from one measured as
    open. It changes nothing operationally here (the row is undispatchable on robots
    alone), but it is the same class of error the `PartialDate` precision column exists to
    prevent, in a different column.

## 10. Defects found by executing rather than reading

Recorded because they are the argument for the verification discipline, not for the
architecture. Most of these would not have been caught by review alone — and the first
entry below is the exception that proves why the discipline needs a second half:
**it was found by reading, and no gate this repository runs would ever have caught it.**

The blocks are: the phase's security finding; defects carried over from the previous
report; defects found during Phase 2's integration; and defects found in the closing pass,
which audited the four owners' repairs and the tree they left.

**The security finding**

| Defect | How it surfaced |
| --- | --- |
| **A public deputy for a private surface: `apps/web` could publish to the catalogue from the internet.** The public catalogue site carried `lib/review.ts#decideReviewItem` and a Next.js Server Action bound to the review page's Accept and Reject buttons, issuing `POST /internal/review/items/{id}/{accept,reject}` from `apps/web`'s own server. Both halves of a deployment could follow their documented story exactly — the API server on a private subnet with `FIRMSCOUT_REVIEW_API_ENABLED=true`, the public site with `FIRMSCOUT_REVIEW_UI_ENABLED=true` — and the composite was an internet-reachable unauthenticated publish, with neither documented switch set wrongly and the API never reachable from outside its subnet. The enabling switch was named in no architecture document at the time. **Closed by deleting the write path from `apps/web`** — `internalFetch` now hardcodes `method: "GET"` and `InternalRequestOptions` has no `method`, `headers` or `body` — with accept and reject moving to `apps/cli`, which calls `application.DecideReviewItem` against the database and never needed the HTTP surface. ADR-0021's amendment records the reasoning; `api.md` §11 and `security.md` §4.11 and T-16 record the controls and what is left. | **Reading the composition, not running anything.** No gate in this repository looks at the *pair* — the Go side saw two correctly-defaulted switches, the TypeScript side saw a server-side `fetch` to a configured URL, every test passed, `golangci-lint` and `eslint` were clean, and the vulnerability lived in the sentence "the API is on a private subnet" being true of one process and irrelevant to the other. It was written, reviewed and integrated into Phase 2's working tree; it never reached a deployment because there has never been one, and it was removed inside the same phase before anything was committed. That is the honest scope: no user was exposed, and nothing about the deployment story would have prevented it. |
| **Four documents undercounted the switches on that surface** — ADR-0021, `api.md` §11, `docs/diagrams/api-sequence.md` and this report — each saying "two independent switches", each meaning the API server's pair, none naming `FIRMSCOUT_REVIEW_UI_ENABLED`. This report's omission was the widest: the ledger whose purpose is to make gaps visible had no row for the phase's most serious finding, in any of its three relevant sections. | Auditing the four owners' repairs against the documents they claimed to have corrected, and grepping the whole repository for the phrase rather than the files the earlier pass had edited. The same scope error as the invented-vocabulary defect below, one pass later. |

**Found in Phase 3's integration pass, by running the thing rather than reading it**

Every one of these was invisible to `go build`, `go vet`, `golangci-lint`, `tsc --noEmit`,
`eslint`, `next build` and the whole Go and web test suites, all of which were green in
every owner's tree and green again in the merged tree **before** the API was started and a
page was actually requested.

| Defect | How it surfaced |
| --- | --- |
| **`firmscout registry sync` never refreshed a single summary, so every device would have 404ed in production.** ADR-0024 D6 is explicit that a registry-only product gets no `product_summaries` row and is therefore a 404 on the public API and absent from search, and the owner who built the use case correctly provided `SyncRegistry.WithSummaries`. Nothing called it: `apps/cli/main.go` still constructed the sync with the seven-argument constructor. The builder-method design that kept the tree compiling at every intermediate state is exactly what let the wiring be silently omitted — there was no compile error to notice. `internal/integration/slice_test.go` had the same gap in both of its sync sites, so the integration suite proved a graph no binary assembled. | Running `go run ./apps/cli registry sync` against the development database and reading the output, which reported products upserted and said nothing about summaries. Fixed by wiring `.WithSummaries(c.Releases)` in the CLI and both test sites, and by printing `%d product relationship(s) written; %d product summary(ies) refreshed.` so the next reader sees the number rather than inferring it |
| **The product page and the search page returned HTTP 500 for every device.** `new Date(product.lastVerifiedAt).toISOString()` throws `RangeError: Invalid time value` rather than returning a placeholder, and in a React server component that is a 500 for the entire page. `lastVerifiedAt` is `omitempty` on the wire and **no source has ever verified a hardware model**, so the key is absent for every device — the first products in the catalogue's history for which it is. The search page failed the same way inside its `Array.map`, meaning one device in a result set 500ed the whole page, including its software hits | `curl -o /dev/null -w "%{http_code}"` against `next start`, immediately after the API was serving devices correctly. Fixed with `formatUtcMinute`/`formatUtcDay` in `lib/release-display.ts` — the module whose own doc comment already named this bug class — plus four regression tests |
| **`docs/api/openapi.yaml` listed `releaseType` and `lastVerifiedAt` as `required` on `Product`, and the API omits both for a device.** The contract documented a guarantee the handler does not honour, and `apps/web/lib/api.ts` had typed both non-optional *because* the spec said required — which is why `tsc` was happy about code that could not run. A latent lie that only devices made reachable, the same shape as the family-link bug ADR-0024 D7 predicted | Comparing the real `curl` response body against `Product.required` after the 500s were traced. Fixed by removing both from `required` with a comment explaining why a device makes them optional, and by typing them optional in the client |
| **The vendor page returned HTTP 500 for every vendor, and had been doing so before this phase.** Same crash class, unrelated cause: `PresentVendor` has never sent `productCount` or `lastVerifiedAt` — `VendorDTO` types both as `omitempty` pointers on purpose, since `domain.Vendor` carries neither — while `openapi.yaml` listed both as required and the page formatted the timestamp unguarded. Pre-existing and **not** caused by the device work; found only because this pass loaded pages instead of trusting `next build` | Requesting `/vendors/mikrotik` while sweeping every route for 500s. The render is fixed and the contract corrected to match what the API actually sends; **supplying the two aggregates is still not done**, and is recorded as an open gap rather than papered over with a zero |

**Carried over from the previous report**

| Defect | How it surfaced |
| --- | --- |
| `array_to_string` is only `STABLE`, so the generated `search_vector` column made the entire migration fail | first real `migrate up` |
| `TruncateExcerpt` returned up to 4002 bytes, because the ellipsis is three bytes in UTF-8, and `Evidence.Validate` rejects over 4000 | flagged by an implementer, then pinned with a test |
| `publication_date` had no precision column while `release_date` did, so a month-precision publication date would read back as a real day | schema review against ADR-0017 |
| `CheckSource` stored the *normalised* text as the artifact, so extraction found nothing | end-to-end run |
| Nothing wrote the `source_artifacts` metadata row, so the first check violated a foreign key | end-to-end run |
| `ResolveByAlias` compared a normalised hint against a raw slug, silently matching nothing | end-to-end run |
| GitHub Actions pinned to mutable tags, one to `@main`, and a `govulncheck` job that could pass without running | threat-model review |
| A domain test compared an expression with itself and therefore asserted nothing | `staticcheck` |
| A Mermaid diagram used backslash-escaped quotes, which GitHub renders as an error box | real Mermaid parser, after a structural lint had passed it |
| `Dockerfile.go` was compiled by the Go toolchain and broke `go build ./...` | full-module build |

**New in this integration**

| Defect | How it surfaced |
| --- | --- |
| **`internal/platform/wire.go` never wired `ConflictRepository` or `AuditRepository`.** Every binary therefore ran with gate 10 unable to see any disagreement. The tree compiled, every unit test passed, and the whole of W1 was inert in production. | Writing the end-to-end conflict test against the real container wiring. Confirmed by mutation: with the port set to nil, the contested version publishes and the pipeline reports `decision = "passed"`. |
| **`/readyz` answered 200 unconditionally.** `apps/api` never set `Deps.Ready`, so the readiness gate was nil. `postgres.DB.Ping` has carried the comment "for readiness probes" since the first slice and was called by nothing. | Reading the composition root while wiring the review use cases. |
| **A test string literal carried a raw U+0007 BEL character**, invisible in a diff and in a review, where `"alex\a approved"` was meant. | `golangci-lint` (`staticcheck` ST1018) |
| **An integration assertion asserted the opposite of the specification.** The first draft of the Fortinet end-to-end test required an advisory to publish. ADR-0018's D18 requires precisely the reverse: the feed names no affected product, so nothing from it may publish automatically. | Running the test. It failed with `no advisory passed validation`, and the failure was the correct answer. |
| **`database/queries/` is empty and there is no `sqlc.yaml`,** while `data-model.md` claimed the directory held sqlc query definitions and ADR-0004 chose the tool. | Attempting to run the `sqlc diff` gate. |
| **The `newest-stable` source recorded `robots.txt` evidence from the wrong host.** Its YAML documented a measurement of `mikrotik.com/robots.txt`, but the source is served from `upgrade.mikrotik.com`, and `robots.txt` is scoped to a scheme, host and port — the apex's file says nothing about the subdomain. The recorded status happened to survive the correction, which is the uncomfortable part: the value was right by luck, and the evidence behind it was for a host the source never contacts. | Re-fetching `robots.txt` for the source's own host on 2026-09-05 and getting **HTTP 403** where the file was documented as a permissive 200. |
| **A review item's age could be rendered negative.** `now.Sub(created_at)` was returned unfloored, so any skew between the writing clock and the reading clock produced an age like `-3m0s`. It reads as a bug in whatever is displaying it rather than as the clock skew it is. | Running `firmscout review list` against a live database on a host whose clock had just been corrected backwards. Now floored at zero and pinned by `TestQueueAgeIsNeverNegative`. || **`TestConcurrentDequeuesDoNotOverlap` asserted a guarantee PostgreSQL does not make,** and failed about one run in eighty. It required four racing workers to lease all twenty available jobs between them. `SELECT … FOR UPDATE SKIP LOCKED` promises only that a locked row is passed over — never that the scan returns for it — so a worker that takes a large batch leaves the others scanning a queue in which everything is locked, and a row every worker skipped stays `pending`. The test was corrected to the real invariant (nothing leased twice, nothing lost: leased + pending = total), not the queue bent to the assertion. | A full-suite run that failed with `workers leased 18 jobs in total, want 20`, then direct measurement inside the race: a worker that leased nothing could **see 18 pending rows and lock none of them** (`visible=18 lockableRetry=0`). Three earlier hypotheses about the cause were each disproved by measurement before this one was confirmed. |
| **The dequeue statement re-locked its rows a second time without `SKIP LOCKED`.** `UPDATE … WHERE id IN (SELECT … FOR UPDATE SKIP LOCKED)` plans as a Hash Semi Join whose enclosing `UPDATE` takes its own locks, so one worker could block behind another instead of stepping around it — the opposite of the point. Rewritten as a materialised CTE joined into the `UPDATE`. Both forms were measured leasing the same rows, so this is a contention fix and **not** the cause of the flake above; the doc comment says so, because an earlier draft of that comment claimed it was and that claim was wrong. | `EXPLAIN (VERBOSE)` on both forms while investigating the flake. |
| **The invented compliance vocabulary survived in six more documents** after the pass that was supposed to remove it. `DATA_SOURCES.md` was corrected to the implemented values, but `robots: not_checked` and `terms: reviewed_ok` / `reviewed_blocked` — none of which exist in `internal/domain/source.go` or any migration — were still being stated as the vocabulary in `definition-of-done.md`, `system-context.md` (three places, including the enabling rule), `collector-sdk.md`, `first-sprint-backlog.md`, `update-pipeline.md` and `risks.md`. Several also stated an enabling rule narrower than the implemented one, omitting `not_applicable` and `restricted` entirely. All corrected; a repo-wide grep for the three invented values now returns nothing. | Re-running the vocabulary check across the **whole repository** rather than only across the files the previous pass had edited. The earlier check was sound and its scope was not, which is the more common way a documentation gate misses something. |


**Found in the closing pass** — the audit of the four owners' repairs, and of the tree they left

| Defect | How it surfaced |
| --- | --- |
| **The release-history window compared a reduced-precision date at its canonical anchor, so a release straddling the boundary was silently dropped.** A release dated `2025` anchors at 1 January and vanished from a window opening 2025-09-05; so did one dated `2025-09`, anchored at day 1. Those are hidden releases, which is the one failure a window may not produce. Fixed in `internal/adapters/postgres/release_repo.go` by comparing the **end** of the period the stored precision denotes, with `domain.PartialDate.PeriodEnd()` added as the domain's expression of the same rule. | Writing a test that checks the *query's* verdict for every seeded row against `domain.PeriodEnd` rather than against a hand-written expectation — so the SQL and the domain rule cannot drift apart without the failure saying so. Reverting the SQL fails it with `the query says in-window = false, domain.PeriodEnd (2025-12-31) says true; the SQL and the domain rule have diverged`. |
| **The window boundary was reduced to a date in the *session's* time zone.** `($5::timestamptz)::date` resolves in `TimeZone`, and the verification database runs `America/Sao_Paulo`, so a boundary of `2025-09-05T00:00:00Z` became the date `2025-09-04` and a release dated one day before the window came back. East of UTC it would have narrowed the window and hidden releases instead. Now `(($5::timestamptz) AT TIME ZONE 'UTC')::date`. | Found while proving the fix above, not looked for. Probed literally: `TimeZone=America/Sao_Paulo  ::date=2025-09-04  AT TIME ZONE UTC ::date=2025-09-05`. Stated plainly: the revert test for this one only fails on a database whose session time zone is not UTC. |
| **Three page-size contracts disagreed with the one `api.md` documents, and none of the three numbers appeared in any document.** `ListReleases` clamped absent-or-too-large to 50 where the contract says default 20 / max 100; `postgres.ListForProduct` restated the bounds as `clampLimit(opts.Limit, 50, 500)`; `ListReviewQueue` sent a caller asking for 500 back with 50 rather than the documented maximum of 200. All were masked from HTTP because the handler clamps first, which is exactly the shape that lets a handler become the de-facto contract. | Reading each read use case against the row in `api.md` §2 and §11 that documents it, then pinning both the clamp *and* the constants — a correct clamp can point at the wrong numbers. |
| **`SearchProducts` enforced its documented bounds in the wrong units.** The minimum used `len(text) < 2` (bytes), so a single three-byte ideograph passed the check its own doc comment says rejects single-character catalogue sweeps; the maximum used `text[:200]`, a raw byte slice that can cut a rune in half and hand invalid UTF-8 to the trigram index. | The same read-through. The revert of the truncation half fails with `query reached the repository as invalid UTF-8 ("xéééé...éé\xc3"); the cut split a rune`. |
| **`application.GetVendor` had zero callers**, so `domain.EventVendorViewed` — the only signal FirmScout has about which vendors readers open — had never been published by a running system. `handleGetVendor` called `VendorRepository.GetBySlug` directly. | `grep -rn "GetVendor\|NewGetVendor\|VendorPage" --include=*.go .` returned only the definition and a separately-named handler, and `go build ./...` still succeeded after the use case was rewritten. A use case nobody calls still compiles and still passes its own unit tests, which is why the detector for this had to be written at the HTTP layer: `TestVendorViewIsPublishedOnce` drives the router and asserts on the event. Reverting the handler to the repository call fails it with `vendor_viewed events = 0, want exactly 1`. |
| **`GetLatestRelease` and `ListReleases` answered a malformed slug with 404 instead of 400.** Both trimmed and lowercased and then handed the string to the projection, so `Not_A_Slug` — which cannot name anything — came back as `ErrNotFound`. The documented mapping in `api.md` §6 held only because the HTTP handler validated first, making the contract a property of one caller. `GetProduct` had validated all along. | Following the page-size drift to its general form and checking every read use case's error mapping against §6 rather than against the handler. |
| **The in-memory `ReleaseRepository` double carried the same anchor bug as the SQL**, so an application-level test could have passed on a rule the adapter did not implement. | Fixing the adapter and then asking what the fake would have said. |
| **`TestConcurrentDequeuesDoNotOverlap` could not detect a `Dequeue` that ignored its batch limit.** Inserting `limit = 1` after the clamp left the whole suite green. Concurrency is irrelevant to testing a clamp; a deterministic single-worker test now asserts that asking for 7 of 20 leases exactly 7. | Sabotage: the mutation was applied and the suite run, rather than the test being read and judged adequate. |
| **`PayloadKeyConflictCandidates` documented a guarantee nothing enforced.** The port says a candidate parked in `human_review_required` is always reachable from the queue even when it is not the item's subject; the only reader was a test helper whose fallback branch never fired, because every scenario it checked also satisfied the primary `item.SubjectID == c.ID`. Sabotaging `disputingCandidateIDs` to return nil left the suite green. Closed by making the guarantee real — `firmscout review show --id <id>` is now its first and only production reader — rather than by weakening the comment. | Sabotage again, then a live end-to-end run against a seeded real conflict, which printed the other source's candidate id: reachable only through the payload key. |
| **A 12-line ADR-0021 doc comment was attached to the wrong function.** It described accept/reject moving into the CLI and sat, with no blank line, above `runConflicts` — a read-only command — instead of `runReview`. `go doc -all -u ./apps/cli` showed the whole block rendered as `runConflicts`'s documentation and `runReview` with none. | Running `go doc` on the package rather than reading the source. A misattached comment is invisible in a diff and obvious in the rendered docs. |
| **`ListReleases`'s doc comment described an ordering the code has never had.** It claimed "release date descending with unknown dates last, then first-observed descending"; `ListForProduct` has always ordered by `first_observed_at DESC, id DESC` and nothing reorders. The claim came from the sort the *handler* applies to a fetched page — a different thing, and one that does not survive pagination. | Tracing the window fix down to the SQL and reading the `ORDER BY` that was actually there. |
| **`latestOutsideWindow` and the window query would have contradicted each other** once the query was fixed: the flag compared a period end against the raw boundary instant, while the query compares it against the boundary's UTC calendar day, so a release dated on the boundary day would be returned by the page and simultaneously declared unable to appear in it. | Asking, after the SQL changed, whether the two comparisons were still the same comparison. They were not, and the case that separates them — a boundary carrying a time of day, which is the shape `HistoryWindow` actually produces — is now pinned as its own assertion. |
| **`DATA_SOURCES.md`, ADR-0018, §7 of this report and `bootstrap-flow.md` all asserted Dell was registered and disabled. No such row existed.** Poly and Ubiquiti were asserted too and were equally absent. The project's flagship honesty demonstration rested on rows that were not there. | Running `go run ./apps/cli registry validate` and reading `2 vendor(s) … 3 source(s)` against four documents that said otherwise, then `grep -ril -e dell -e poly -e ubiquiti dataset/ collectors/` returning nothing at all. |
| **`support.hp.com` returns a byte-identical SPA shell with the same `ETag` for every path, including invented ones.** Both `/bundle/g7500-release-notes/title-page` and `/anything/deep/path` answer HTTP 200 with 7087 bytes and sha256 `539f9caf…`. So an HTTP 200 there is no evidence a document exists, and a conditional GET would answer 304 forever while the release notes changed. Poly is therefore registered at the portal root and not at a release-notes document. | Measuring a path that cannot exist and comparing the bodies, rather than measuring only the path that was expected to work. |
| **The recommended "clean API" substitute has no conditional-request validator at all.** `fw-update.ubnt.com/api/firmware-latest` answers 200 with 744990 bytes and no `ETag`, no `Last-Modified`, no `Cache-Control`, no `Accept-Ranges`; a `Range: bytes=0-0` request is answered with the whole body. Every check transfers 745 KB and content hashing is the only change signal. `DATA_SOURCES.md` recommended it without that qualification. | Sending the `HEAD` and reading the headers that were absent, rather than the ones that were present. |

**Found by the repair pass on the device-first catalogue (2026-09-06)**

Two adversarial reviews of the shipped device-first catalogue found thirteen defects; four
owners closed them and an integrator reconciled the four trees and re-verified against a
running binary. A separate fabricated-data audit re-fetched all six device pages
independently and came back **clean** — byte counts identical, every recorded value literally
on the page — so none of the below is invented data. They are correctness, coherence and
honesty-of-presentation defects, each reproduced by execution.

| Defect | How it surfaced, and how it is now pinned |
| --- | --- |
| **`GET /products/{slug}/latest` answered `404` whenever `?channel` was omitted — the endpoint's own documented default call.** `LatestForProduct`'s predicate was `AND COALESCE(m.channel, '') = $2` unconditionally, so an omitted channel became a demand for a mapping carrying *no* channel at all. Every product whose releases all carry a channel — RouterOS, 44 published releases across stable/long_term/testing — answered 404. **This broke the fleet-manager journey at its last step**: paste the chassis code, open the device, follow `runs[]` to RouterOS, and the firmware call fails. Now `AND ($2::text = '' OR COALESCE(m.channel, '') = $2)` | Executing the call the OpenAPI document names as the default, against a real database, instead of the call every test happened to make. Every handler test registered its release under channel `""`, so all of them passed. Pinned twice: `TestLatestForProductWithoutAChannelAnswersAcrossChannels` (adapter, real database) and `TestLatestReleaseAnswersItsOwnDefaultCallAcrossChannels` (HTTP, registering *nothing* under `""`, so it fails if the empty-channel reading returns) |
| **The product page's headline latest and `/latest` answered different questions.** `RefreshProductSummary` picked the latest across all channels by release date; `LatestForProduct` picked per channel with no tie-breaker. Once the fix above let several flagged channels compete, the two could name different releases for the same product in the same second, and neither response admitted a second answer existed. One shared `latestOrdering(alias)` now spells the rule for all three sites — release date, then first-observed time, then id for determinism, **never a version string** (ADR-0017) | `TestLatestForProductOrdersLikeDomainLatestComparison` folds the expected answer out of `domain.LatestComparison` itself rather than hardcoding it, so the SQL and the Go rule cannot drift without the test saying so. Confirmed live: `/latest` and the product page's `latestRelease` name the same release id, `rel_06G77600G0JMXCB539TCCD1JSW` |
| **`/latest` served a withdrawn release as the answer to "what version should I be on?"** No `withdrawn = false` predicate, so a release the vendor pulled after it was flagged latest was still served — while the product summary, which does exclude withdrawn rows, correctly reported no latest release for the same product. `domain.Release.Serveable()` had **zero callers anywhere in the codebase**; it now has one | A partial revert proves the SQL predicate is load-bearing and not redundant with the Go check: without it the ordering settles on the withdrawn release and never looks at the serveable one behind it in another channel |
| **The search query was truncated with a raw byte slice, cutting multi-byte runes in half.** The invalid UTF-8 reached `websearch_to_tsquery` and `similarity()`, which PostgreSQL rejects outright: a `500` instead of a search, for any accented or CJK model name pasted out of an asset register | A table-driven unit test plus a real-database test using both an odd-offset two-byte case and a CJK case, since three-byte runes never align with the 200-byte bound |
| **`firmwareApplicability` collapsed two independent claims into one scalar.** A product that is both a hardware model and its own release stream — the exact shape ADR-0024 commits to supporting, *"a rack server is a device AND publishes its own BIOS versions"* — reported `{verified: true, basis: "own_releases"}` and dropped the `runs_os` caveat entirely. Its own BIOS-shaped release vouched for an operating-system applicability nobody had checked. `ownReleases {mapped, releaseCount}` is now a separate always-present member, and the runs edge is tested **before** the release count, so a mapping row can never retract a caveat | Asking what the response says for a product that is both, rather than for the two kinds the pilot dataset happens to contain. Recorded as an amendment to ADR-0024, which had written the object as `{verified, basis}` |
| **`SyncRegistry` could never retract a `runs_os` edge or an alias.** Both passes guarded on `len(d.X) > 0`, so deleting a device's `runs:` block left the edge in the database and on the device page forever — a replace that had become an append. An unretractable alias is a search hit that cannot be withdrawn | The reading adopted: a document carrying a `Product` manages that product's *entire* relationship and alias set, and an absent block means "declares none". The rejected alternative — nil means unmanaged, empty means declares-none — puts a row-deleting retraction behind a distinction no YAML author can see and no reviewer can spot in a diff |
| **`ProductDTO` carried no `aliases` field, so the device page printed "No aliases recorded" for a device whose model-number alias produced the search hit that brought the reader there.** ADR-0024 makes model-number search work *through* a `model_number` alias, so the response was withholding the very string the reader had pasted | Now on the wire as an always-present array of raw alias text (never the normalised form). Confirmed live: the CRS328 device page renders `Aliases CRS328-24P-4S+RM` |
| **Eight keys were declared `required` in `openapi.yaml` and omitted by the API, and five always-sent keys were not declared.** This class had been hand-corrected three times already and found by human YAML-versus-struct-tag reading every time | Replaced with a gate: `contract_test.go`'s `TestEveryRequiredKeyIsActuallyAlwaysSent` renders the *emptiest* value the presenter can produce for each schema and fails on any `required` key the JSON omits. A schema with neither a sample nor an explicit "no response value" declaration also fails, so a new schema cannot arrive unchecked |
| **`hap-lite.yaml`'s provenance comment falsely claimed the byte sequence `6.49` does not occur anywhere in the fetched HTML.** It occurs 11 times, every one an SVG path coordinate. ADR-0024's own phrasing was already correct; the dataset comment contradicted it | Re-fetching `https://mikrotik.com/product/RB941-2nD` (335694 bytes, byte-identical to the recorded fetch) and scanning for the literal byte offsets, rather than re-reading the sentence. The stricter `6\.49\.[0-9]+` still returns zero hits and `7.24.2` appears 189 times, so the *conclusion* held and only the measurement was overstated |
| **The registry loader defaulted an omitted `spec.lifecycle_status` to `active`** — silently recording the one lifecycle claim no MikroTik product page supports. Absence of a lifecycle line is absence of evidence, not evidence of "active". Now `unknown`, with `packages/schemas/product.schema.json` corrected to stop advertising the same false default | Reading the loader's defaults against house rule 4 rather than against the tests, which asserted nothing about it |
| **The device page asserted a catalogue fact from a fact about one response.** "No aliases recorded" rendered whenever `aliases` was absent, which is silent about the catalogue and says only that this reply omitted the field. `aliasesLabel` now distinguishes absent (`"Not returned by this response"`) from an affirmative empty array (`"No aliases recorded"`) | The same false-precision defect the sibling "Source quality: Not yet assessed" row was already careful to avoid |
| **The search page rendered an operating system's NAME in the "Latest version" column, in the same monospace styling as a real version string** — a non-version presented as a version, on the first screen a fleet manager reaches after pasting a model number. The results page also carried no unverified-applicability signal at all. Now an em dash, matching every other column's missing-value convention, plus a compact `Firmware fit unverified` badge on the row | Confirmed live: the CRS328 search row renders `CRS328-24P-4S+RM  Device  Firmware fit unverified  MikroTik  —  —  —  —` |
| **The vendor page rendered "Products tracked: Not recorded" directly above the list of the seven products it was at that moment rendering** — each half individually true, the viewport self-contradictory. `vendor.productCount` is genuinely never sent, so the row now reports the one number the page can state without inventing an aggregate: how many rows are in the list below it, relabelled to say exactly that. The same never-sent field was also being interpolated into every vendor page's live `<meta description>`, rendering the literal word `undefined` | Confirmed live: `Website https://mikrotik.com  Products listed below 7  Last verified Not recorded`, above seven products, and the meta description no longer contains `undefined` |

**Found by the integrator while reconciling the four owners' trees**

Every one of these is a gap *between* owners rather than inside one, which is the class a
per-owner review cannot see.

| Defect | How it surfaced |
| --- | --- |
| **Two in-memory fakes still implemented the old per-channel-equality semantics and never filtered withdrawn releases, so they lied about the port the SQL had just been corrected against.** `apptest.Releases.LatestForProduct` tested `m.Applicability.Channel == channel` and returned the first match; `httpapi`'s `fakeReleases` keyed a map on `productID+"|"+channel` with no empty-channel fallback. **This is the hole the original defect came through**: a fake that answers a narrower question than the adapter does not stand in for it, it certifies the bug. Both now fold candidates with `domain.LatestComparison` and skip `!rel.Serveable()` | The owner who fixed the SQL flagged both as outside their ownership and specified the exact changes; nobody applied them, because the two files belong to a third owner who was not asked. Found by reading the "not fixed" section of one report against the files another owner had touched |
| **The port contract said none of what the adapter now implements.** `application.ReleaseRepository.LatestForProduct`'s doc said only "for a product and channel" — not what an empty channel means, not that withdrawn releases are excluded, not how several flagged channels are ordered against each other. That silence is what let the adapter and the fakes disagree in the first place. Now stated in full, including the sentence that the two fakes had disagreed with it | Same reading. A contract that does not say which of two readings it means is not a contract |
| **The `/latest` 404 blamed a channel the caller never named.** `"This product has no published release on that channel."` was returned for a call with no `?channel` — pointing a caller at a filter they had not applied, which is exactly the wrong diagnosis and exactly what the endpoint said while it was 404ing its own default call. A pre-existing test *asserted* the misleading wording, so correcting it required correcting the test that codified it | Reading the live `404` body from the device's own `/latest` after the critical fix landed. The two 404s still read differently; only the one that names no channel stopped naming one |
| **The web client never received the reshaped applicability contract.** `apps/web/lib/api.ts` had no `ownReleases` member at all, so the client could not see the second claim; `describeApplicability`'s `runs_os_unverified` branch — which now also fires for a product that publishes its own releases — still opened with *"This product publishes no release stream of its own"*, which is simply false for such a product. The API owner handed over the exact JSON and the exact rendering consequence; the web owner's brief named only three other defects | Diffing the API owner's stated wire format against the TypeScript interface, rather than trusting the handover note. Pinned by three tests, one of which asserts the old sentence is still used when it is still true |
| **`aliases` was typed optional and documented as "not guaranteed by the current documented contract"** after it had become `required` in `openapi.yaml` and always sent by the presenter. `category` was typed *required* while being `omitempty` and correctly absent from `Product.required` — the same defect in the other direction, one row above the `releaseType` field whose comment already warns about exactly this | The same diff. Both corrected, and the product page's Category row now states the absence the way its Release type neighbour already did |
| **The vendor *list* page rendered a countless subtitle on every card.** `{vendor.productCount} product{s}` unguarded, and because React renders `undefined` as nothing, the visible result was a subtitle reading `" products"` — a count claim with the count missing. Flagged by the web owner as out of their brief's scope and left; it is the same never-sent field and the same defect class as the vendor *detail* page's row that was in scope | Sweeping every reader of the field the in-scope fix was about, rather than only the file the brief named |

## 11. Recommended next implementation step

**Enable one source and let the pipeline run against the real internet, once.**

This is unchanged from the previous report and is now more overdue, not less. Phase 2
doubled the number of paths through the pipeline — a conflict path, a review path, a
third collector engine — and every one of them has been proved only against recorded
bytes served from loopback. The gap between a recorded fixture and a live page is exactly
where this class of project fails, and nothing built since the last report has narrowed
it. The sequence:

1. Review mikrotik.com's terms of use and record the decision in
   `dataset/sources/mikrotik/changelogs.yaml`.
2. If approved, set `enabled: true`, run `firmscout registry sync`, and run
   `firmscout check-source --vendor mikrotik --slug changelogs` by hand.
3. Compare what it publishes against the page in a browser, and fix what differs.
4. Only then start the worker on a schedule.

After that, in order:

1. **Resolve the sqlc question** (§9 item 10). It is an hour of work either way and it
   currently makes one of two committed documents false.
2. **Get Docker or an equivalent running in CI**, so the Compose stack, the two
   Dockerfiles and the observability configuration stop being the largest block of code
   in the repository that has never executed.
3. **A second collector archetype — and it is Ubiquiti, not Poly.** The previous report
   recommended Poly's PDF release notes. Measurement on 2026-09-05 argues against it:
   `support.hp.com` answers HTTP 200 with a byte-identical 7087-byte SPA shell for every
   path including invented ones, so there is no document URL that can be verified to
   exist, its `ETag` belongs to the shell rather than to any release note, and HP's eight
   declared sitemaps do not index `/bundle/`. The engine it would need, `pdf_text`, is
   reserved and unimplemented. Ubiquiti's `fw-update.ubnt.com/api/firmware-latest` is the
   better return: `robots.txt` permits it (404 = full allow), it is official, well-formed
   JSON, and it is the **first pilot source that can produce `exact_day` dates under
   ADR-0017 with no inference at all** — it publishes RFC 3339 `created` and `updated`
   per entry. Its only blocker is that `json_path` is reserved and unimplemented, which is
   one engine rather than one engine plus an unverifiable URL. Its cost is worth stating:
   the endpoint serves no `ETag`, `Last-Modified`, `Cache-Control` or `Accept-Ranges`, so
   every check transfers 745 KB and content hashing is the only change signal — hence
   `min_frequency_seconds: 21600` on the registered row.
4. **Decide whether `POST /internal/review/items/{id}/{accept,reject}` should exist**
   (§8 item 9, §9 item 14). The CLI no longer needs them. Removing them deletes the whole
   confused-deputy class rather than documenting around it, and it is cheaper than the
   authentication below.
5. **Authentication for the reviewer surface**, which is the only thing standing between
   the review queue as built and the review queue as usable by more than one person on
   one laptop.
6. **Push `channel`, `releaseType`, `sort` and `order` into the release-history query**
   (§9 items 15 and 16). It closes the last instance of the handler acting as the
   application layer, and it turns two documented-but-surprising behaviours into
   unsurprising ones.

Do not start the AWS deployment or the AI agents. Neither is on the critical path to
knowing whether the catalogue can be maintained, which is the only question the MVP
exists to answer.
