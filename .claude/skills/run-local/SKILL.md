---
name: run-local
description: Launch FirmScout locally and drive it — the Docker Compose stack (postgres, migrate, api, worker, web) or, when Docker is unavailable, native processes. Use when asked to run, start, boot, serve or smoke-test the app, to see a change working in the real application rather than in tests, or to bring the stack back up after a reboot.
---

# Running FirmScout locally

Two paths. **Docker Compose is the real one** and the one to reach for by default:
it runs what production runs. The native path exists because Docker is not always
available in this WSL environment, and it is how the app was run before the Compose
stack had ever been executed.

Both paths were verified end to end on 2026-09-06. Every command below has been run.

---

## Path A — Docker Compose (preferred)

### 1. Is the daemon up?

```bash
docker info >/dev/null 2>&1 && echo up || echo down
```

**If it is down**, the usual cause is that Docker Desktop is not running on Windows,
not that WSL integration is misconfigured. Check both before changing any setting:

```bash
/mnt/c/Windows/System32/tasklist.exe | grep -ic docker   # 0 == not running
ls /mnt/wsl/                                             # docker-desktop == integration mounted
```

`/usr/bin/docker` is a symlink into `/mnt/wsl/docker-desktop/cli-tools/...`, which only
exists while Docker Desktop runs with WSL integration enabled for this distro. A broken
symlink therefore means "not running" far more often than "not configured".

Start it and wait — it takes about a minute:

```bash
nohup "/mnt/c/Program Files/Docker/Docker/Docker Desktop.exe" >/dev/null 2>&1 &
until docker info >/dev/null 2>&1; do sleep 3; done && echo ready
```

Run that `until` loop with Bash `run_in_background: true` rather than a bare `sleep`.

Only if `/mnt/wsl/docker-desktop` never appears is it genuinely an integration problem:
Docker Desktop → Settings → Resources → WSL Integration → enable for this distro →
Apply & Restart.

### 2. Free the ports

The stack publishes **8080**, **3000** and **5432**. A native run (Path B) holds the
first two, and Compose fails to bind rather than telling you why.

```bash
for p in 8080 3000 5432; do ss -ltn | grep -q ":$p " && echo "$p busy" || echo "$p free"; done
pkill -f firmscout-api; pkill -f next-server      # only if Path B is running
```

### 3. Up

```bash
cd infrastructure/docker
cp -n .env.example .env          # env_file requires it; .env is gitignored
docker compose up -d --build
docker compose ps
```

First build is slow (several minutes, mostly the Next.js image). Expect:

```
SERVICE    STATUS                   PORTS
api        Up (healthy)             0.0.0.0:8080->8080/tcp
postgres   Up (healthy)             0.0.0.0:5432->5432/tcp
web        Up (healthy)             0.0.0.0:3000->3000/tcp
worker     Up
```

`worker` shows **Up**, never *healthy*, and that is correct: it serves no HTTP, so it
has no probe. See "Known gaps" below.

`migrate` runs once and exits 0. `api` and `worker` wait for it via
`service_completed_successfully`, so a failed migration stops the whole stack — read
`docker compose logs migrate` first whenever nothing comes up.

### 4. Load the catalogue

```bash
docker compose run --rm migrate registry sync      # the vocabulary: vendors, products, devices, sources
docker compose run --rm migrate snapshot import    # the facts: published releases with their evidence
```

`registry sync` will report that every source "will not be checked". **That is correct,
not a failure** — every committed source ships `enabled: false` with terms review
pending, so cloning this repository and running it can never, by itself, reach a
manufacturer's servers. See `docs/adr/0018-source-compliance-policy.md`.

`snapshot import` is what makes the catalogue useful without fetching anything: it loads
`data/snapshot/releases.ndjson`, the committed record of what vendors published, each
line carrying the evidence that justifies it (ADR-0025). It resolves everything by slug
against the local registry, so `registry sync` has to run first, and it never overwrites
a release that is already there — re-running it is a no-op.

Collecting fresh data from a manufacturer is a deliberate, separate act — see "Collecting
for real" below.

### 5. Drive it

```bash
curl -s localhost:8080/healthz                                   # {"status":"ok"}
curl -s localhost:8080/readyz                                    # pings the database
curl -s localhost:8080/api/v1/vendors | python3 -m json.tool
curl -s -o /dev/null -w '%{http_code}\n' localhost:3000/vendors
```

Then open **http://localhost:3000** in a Windows browser — WSL forwards localhost.

### Stopping

```bash
docker compose down            # keeps the database volume
docker compose down -v         # deletes it; next up starts from an empty catalogue
```

---

## Path B — native processes (no Docker)

Faster to iterate on, and the fallback when Docker is unavailable. It needs a
PostgreSQL that Docker is not providing.

**PostgreSQL** lives at `~/.local/fspg` — never under `/tmp`, which is wiped between
days on this machine and has already taken a server, its data and an unpacked toolchain
with it. If it is missing, `~/.claude/.../memory/firmscout-verification-environment.md`
has the rebuild (zonky binaries from Maven Central; extract with Python's `zipfile`,
because `unzip` is not installed).

```bash
SOCK=$(mktemp -d /tmp/fspg.XXXX)
~/.local/fspg/pg/bin/pg_ctl -D ~/.local/fspg/data \
  -o "-k $SOCK -h 127.0.0.1 -p 55432" -l ~/.local/fspg/pg.log start
```

Do not trust `pgrep -f "postgres -D ..."` to tell you it is alive: a compound Bash
command containing that pattern matches itself. Check the port, or `ps aux | grep
"[p]ostgres"`.

```bash
export FIRMSCOUT_DATABASE_URL="postgres://postgres@127.0.0.1:55432/firmscout_dev?sslmode=disable"

go run ./apps/cli migrate up
go run ./apps/cli registry sync

go build -o ~/.local/fsbin/firmscout-api ./apps/api
FIRMSCOUT_HTTP_ADDR=":8080" nohup ~/.local/fsbin/firmscout-api > ~/.local/fsbin/api.log 2>&1 &

cd apps/web && npm run build && nohup npm run start -- -p 3000 > ~/.local/fsbin/web.log 2>&1 &
```

Build binaries into `~/.local/fsbin`, not `/tmp`, for the same reason.

---

## Collecting for real

Publishing anything requires a source that is enabled, terms-approved and health-active.
The committed dataset deliberately satisfies none of those, and
`TestEverySourceShipsPendingTermsReview` enforces that it never will.

Enabling collection is **the repository owner's decision**, not something to do while
running the app. When it has been given, it applies to the local database only — never
by editing the committed YAML, which would flip the default for every clone:

```bash
go run ./apps/cli sources activate --slug <slug> --vendor <vendor>
go run ./apps/cli check-source --slug <slug> --vendor <vendor> --force
```

`--force` overrides scheduling, never compliance. There is no flag that makes FirmScout
fetch from a source it has not been permitted to fetch from.

Compliance (`enabled`, `terms_review_status`) is re-applied from the YAML on every
`registry sync`, so a sync undoes a local override.

After collecting, put the result back in the repository so the next clone gets it:

```bash
go run ./apps/cli snapshot export      # writes data/snapshot/, deterministic
git diff --stat data/snapshot/         # a re-export of an unchanged catalogue is empty
```

The export is byte-identical for an unchanged catalogue — ordered by (vendor, release id)
with timestamps in UTC — so a diff here means the catalogue actually changed.

---

## Known gaps — expected, not bugs to chase

- **`worker` is never `healthy`.** `apps/worker` serves no HTTP. It also means
  `prometheus.yml`'s `firmscout-worker` job scrapes `worker:9090`, which nothing
  answers; that target is down by design until the worker gets a metrics server.
- **No source is collectable out of the box**, so a running stack fetches nothing until
  somebody decides otherwise. The catalogue is not empty, though: `snapshot import` loads
  the committed facts.
- **The snapshot can go stale.** It is a projection of the database, refreshed by a
  maintainer running `snapshot export`; nothing yet enforces that they do.
- **The observability profile is separate**: `docker compose --profile observability up
  -d` adds otel-collector, prometheus, loki, tempo and grafana (Grafana on
  **3001**). The default five are what a collector change needs.

## Troubleshooting, from failures actually hit

| Symptom | Cause |
| --- | --- |
| `service "migrate" didn't complete successfully: exit 1` | Read `docker compose logs migrate`. The container carries `dataset/` and `collectors/config/` baked in, and `FIRMSCOUT_ARTIFACT_DIR` pointing at a writable path — both were missing on the first real run. |
| `/app/.next/standalone: not found` during the web build | `apps/web/next.config.ts` must set `output: "standalone"`. |
| `env file .env not found` | `cp .env.example .env` in `infrastructure/docker/`. |
| Compose cannot bind 8080 or 3000 | A native run (Path B) holds them. |
| `api` up but never `healthy` | The probe is `/firmscout healthcheck --url=...`, a subcommand of `apps/api`. If it is gone, the container reports unhealthy while serving traffic perfectly. |
| Every database test fails at connect | `/tmp` was cleared and took PostgreSQL with it. Rebuild under `~/.local/fspg`. |
