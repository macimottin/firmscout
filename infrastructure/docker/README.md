# FirmScout local development stack

> **This stack has not been executed in the environment that authored it.**
> Docker was unavailable (Docker Desktop's WSL integration was off) while
> `docker-compose.yml`, `go.Dockerfile`, `web.Dockerfile`, and everything
> under `infrastructure/observability/` were written. Every file here has
> been validated statically — YAML/JSON parse, Compose structure is
> internally consistent (every `depends_on` target exists, every volume is
> declared, every build context/Dockerfile path exists), metric names used
> in alerts and dashboards were cross-checked against
> [`docs/architecture/observability.md`](../../docs/architecture/observability.md)
> §5. **`docker compose up` has never actually been run against this
> file.** Treat it as reviewed-but-unexecuted until someone runs it for
> real and reports back. In particular: whether `api`/`worker` actually
> implement the `healthcheck` subcommand and `/metrics`/`/healthz` HTTP
> surface this file assumes, whether `apps/cli` accepts `migrate up` as
> written, and whether the observability containers' healthchecks (which
> assume `wget` is present in each upstream image) actually pass, are all
> unverified.

## What's here

| File | Purpose |
| --- | --- |
| `docker-compose.yml` | The whole local stack: default profile (postgres, migrate, api, worker, web) plus an `observability` profile (otel-collector, prometheus, loki, tempo, grafana). |
| `go.Dockerfile` | Multi-stage build for `apps/api`, `apps/worker`, `apps/cli` — one Dockerfile, selected via `--build-arg BINARY=api\|worker\|cli`. Named `go.Dockerfile` (not `Dockerfile.go`) deliberately: `go build ./...` treats *any* `.go`-suffixed file under the module as a Go source file regardless of content, so `Dockerfile.go` breaks the Go build for the entire repository. |
| `web.Dockerfile` | Multi-stage build for `apps/web` (Next.js, `output: "standalone"`). |
| `postgres/init/01-extensions.sql` | Runs once against a fresh Postgres data volume; creates `pg_trgm` ahead of migrations (belt-and-suspenders alongside the same `CREATE EXTENSION IF NOT EXISTS` in `database/migrations/00001_initial.sql`). |
| `.env.example` | Every environment variable this stack reads, documented, with obviously-fake local defaults. Copy to `.env` before running anything. |

`infrastructure/observability/` (one level up) holds the Collector,
Prometheus, Loki, Tempo, and Grafana configuration this stack's
`observability` profile mounts in — see that directory's own files for
detail; this README covers running the stack, not the pipeline internals.

## Prerequisites this file assumes but cannot verify here

- Docker Engine + Compose v2 (`docker compose`, not the standalone
  `docker-compose` v1 binary — this file uses `service_completed_successfully`
  healthcheck conditions and `profiles:`, both Compose v2 features).
- `apps/api`, `apps/worker`, and `apps/cli` have `main.go` files. **They may
  not exist yet** — this Dockerfile and Compose file were written
  concurrently with those binaries, per this change's brief, and will fail
  to build until each `apps/<binary>` package exists.
- `apps/web` has a `next.config.*` setting `output: "standalone"`, and a
  `public/` directory (even an empty one) — `web.Dockerfile` copies
  `.next/standalone` and `public/` unconditionally.

## Quick start

```bash
cd infrastructure/docker
cp .env.example .env        # edit if you need non-default ports/credentials

# Default profile: postgres, migrate, api, worker, web
docker compose up --build

# ...or with the observability stack too:
docker compose --profile observability up --build
```

`docker compose` picks up `.env` in this directory automatically for
variable substitution inside `docker-compose.yml`; the `env_file: .env`
entry on each service additionally passes those variables into the
containers themselves.

A fresh clone gets a working schema with no manual step: the `migrate`
service runs `firmscout migrate up` once, before `api`/`worker` start
(`depends_on: migrate: condition: service_completed_successfully`).

## Services and ports

### Default profile

| Service | Image / build | Host port | Purpose |
| --- | --- | --- | --- |
| `postgres` | `postgres:17.6-alpine` | `${POSTGRES_PORT:-5432}` | The only stateful dependency — database, job queue, and artifact metadata store (blueprint §18). |
| `migrate` | `go.Dockerfile`, `BINARY=cli` | — (one-shot, exits) | Runs `firmscout migrate up` against `postgres`, then exits. Not restarted; `api`/`worker` wait for it to exit `0`. |
| `api` | `go.Dockerfile`, `BINARY=api` | `${API_PORT:-8080}` | Go HTTP API. `/healthz` per `docs/architecture/api.md`. |
| `worker` | `go.Dockerfile`, `BINARY=worker` | — (internal `:9090` only, for `/metrics`+`/healthz`) | Scheduler loop and job runner. No published port by default — nothing outside the Compose network needs to reach it directly. |
| `web` | `web.Dockerfile` | `${WEB_PORT:-3000}` | Next.js public site. Talks to `api` at `http://api:8080` inside the Compose network. |

### `observability` profile

Started with `docker compose --profile observability up`, in addition to
the five above — four containers, not nine more, matching blueprint §18's
stated reason: a contributor working on a collector shouldn't need the
whole stack running.

| Service | Image | Host port | Purpose |
| --- | --- | --- | --- |
| `otel-collector` | `otel/opentelemetry-collector-contrib:0.114.0` | `4317` (OTLP gRPC), `4318` (OTLP HTTP), `8888` (Collector's own metrics), `8889` (converted application metrics) | Receives OTLP from every FirmScout binary; batches, redacts secrets, filters cardinality, tail-samples traces, fans out to Prometheus/Loki/Tempo. See `../observability/otel-collector-config.yaml`. |
| `prometheus` | `prom/prometheus:v3.0.1` | `9090` | Metrics, 15-day local retention. Scrapes `api`/`worker` directly *and* the Collector's converted-metrics endpoint — see the comment in `../observability/prometheus/prometheus.yml` for why both. |
| `loki` | `grafana/loki:3.3.2` | `3100` | Logs, 14-day retention, filesystem storage. |
| `tempo` | `grafana/tempo:2.6.1` | `3200` (query API; OTLP receiver is internal-only, not published) | Traces, 7-day retention, filesystem storage. |
| `grafana` | `grafana/grafana:11.4.0` | `${GRAFANA_PORT:-3001}` (maps to the container's `3000`) | Dashboards, provisioned entirely as code from `../observability/grafana/`. Default login: `${GRAFANA_ADMIN_USER:-admin}` / `${GRAFANA_ADMIN_PASSWORD}` from `.env`. |

**Running without the `observability` profile is fully supported.**
`api`/`worker`/`web` always attempt to export OTLP to
`http://otel-collector:4317` regardless of whether that profile is running;
if it isn't, those calls simply fail to connect. OTel SDKs are designed to
drop telemetry and keep the application running rather than fail a request
over it, so `api`/`worker`/`web` start and serve traffic normally either
way — this has not been observed directly (no Docker here), it follows from
how OTel exporters are documented to behave, and should be the first thing
confirmed once this stack is actually run.

## Running migrations

Migrations run automatically on `docker compose up` via the `migrate`
service. To re-run them by hand against an already-running stack (e.g.
after pulling new migration files):

```bash
docker compose run --rm migrate
```

## Resetting the database

Postgres data lives in the named volume `firmscout_postgres_data`. To wipe
it and start from an empty database (re-running `01-extensions.sql` and
every migration from scratch on next `up`):

```bash
docker compose down
docker volume rm firmscout_postgres_data
docker compose up --build   # migrate runs again against the empty volume
```

`docker compose down -v` removes *all* of this stack's named volumes
(`firmscout_postgres_data`, `firmscout_prometheus_data`,
`firmscout_loki_data`, `firmscout_tempo_data`, `firmscout_grafana_data`) —
use it to reset everything at once, including observability history and any
Grafana state (there should be none worth keeping, since dashboards are
provisioned from files, not created by hand).

## What was verified, and how

Everything below was checked without Docker; none of it is a substitute for
actually running `docker compose up`.

- **YAML parses**: every `*.yml`/`*.yaml` under `infrastructure/` loads
  cleanly with `python3 -c "import yaml,sys; [yaml.safe_load(open(f)) for f in sys.argv[1:]]" <files>`.
- **JSON parses**: every dashboard under
  `infrastructure/observability/grafana/dashboards/*.json` loads cleanly
  with the equivalent `json.load` check.
- **Compose structural coherence**: every `depends_on` target names a
  service defined in the same file; every named volume referenced by a
  service is declared under top-level `volumes:`; every `env_file` path
  exists on disk; every `build.context`/`build.dockerfile` path exists.
- **Metric-name cross-check**: every `firmscout_*` metric referenced in
  `infrastructure/observability/prometheus/alerts.yml` and in the three real
  Grafana dashboards was checked against the metric names actually defined
  in `docs/architecture/observability.md` §5. See that section's
  cross-check output in the change's PR description / commit message for
  the literal script and result.

## What was NOT verified (no Docker available)

- Whether any image actually builds — `go.Dockerfile` cannot be tested at
  all until `apps/api`, `apps/worker`, `apps/cli` have real `main.go` files;
  `web.Dockerfile` cannot be tested until `apps/web` has a
  `next.config.*` with `output: "standalone"` and real page/route code.
- Whether `docker compose up` (with or without `--profile observability`)
  actually brings every service to a healthy state.
- Whether the `api`/`worker` healthchecks work as written — they assume
  each binary implements a `healthcheck` subcommand (a local HTTP GET
  against its own `/healthz`/`/metrics` surface), because
  `gcr.io/distroless/static-debian12` has no shell and no HTTP client to
  probe with any other exec form. If that subcommand doesn't exist,
  Compose will report those two services permanently `unhealthy` even
  though the process itself is fine — replace the healthcheck with
  `disable: true` in that case and rely on `curl -f http://localhost:8080/healthz`
  from the host instead, matching `docs/architecture/definition-of-done.md`'s
  smoke test.
- Whether the observability containers' own healthchecks pass — they
  assume `wget` is present in each upstream image (true for the images
  pinned here at the time this was written, to the best available
  knowledge, but not confirmed against a running container).
- Whether the OTel Collector config is accepted by
  `otel/opentelemetry-collector-contrib:0.114.0` without a startup error —
  validated only by hand-reading the schema against contrib's
  `redactionprocessor`/`tailsamplingprocessor` documentation, never by
  actually starting the container.
- Whether Grafana's provisioned datasources/dashboards actually load
  without error, and whether the Tempo trace-to-logs/trace-to-metrics
  correlation links actually work end to end.
- The full-stack smoke test in `docs/architecture/definition-of-done.md`
  (`docker compose up --build` + `curl -f http://localhost:8080/healthz`)
  has not been run.
