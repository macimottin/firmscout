---
name: local-stack
description: >-
  Build and run the FirmScout local stack (PostgreSQL, API, worker, Next.js web)
  preferring Podman on Fedora, falling back to Docker, with a hybrid host+container
  mode when rootless Podman hits SELinux bind-mount issues. Use when the user asks
  to build locally, run the app, make up, start the stack, test locally, or mentions
  Podman, Docker, Compose, localhost:3000, or localhost:8080.
---

# FirmScout local stack

Bring up a testable local environment. Prefer **Podman**; use **Docker** only when
Podman is unavailable. Do not assume `make up` works as-is on Fedora/Podman.

## Default path

Run the skill script from the repo root (agent should execute it, not re-implement):

```bash
bash .cursor/skills/local-stack/scripts/local-stack.sh up
bash .cursor/skills/local-stack/scripts/local-stack.sh smoke
```

Other commands: `detect` · `down` · `status` · `up-hybrid` · `logs`.

## Engine selection

1. Prefer `podman` when `podman` is on PATH and `podman info` succeeds.
2. Else use `docker` when `docker` is on PATH and `docker info` succeeds.
3. On Fedora / rootless Podman, set `CONTAINERS_SHORT_NAME_MODE=disabled` for Compose
   pulls (short-name TTY prompts otherwise fail non-interactively).
4. Always invoke Compose from `infrastructure/docker/` **without**
   `--project-directory <repo-root>` — `podman-compose` rejects that flag, and relative
   paths in `docker-compose.yml` (`../..`, `./postgres/init`) are relative to that
   directory.

## Prerequisites the agent must ensure

Before `up`:

| Check | Action |
| --- | --- |
| `infrastructure/docker/.env` | Copy from `.env.example` if missing |
| `apps/web/public/` | Create (empty `.gitkeep` is fine) — `web.Dockerfile` copies it |
| `apps/web/next.config.ts` | Must set `output: "standalone"` for the web image |
| Go | `go.mod` requires Go 1.27+; use `GOTOOLCHAIN=auto` if the host Go is older |
| Node | 22+ for hybrid web (`apps/web`) |

## Compose vs hybrid

### Full Compose (`up`)

Builds images and starts `postgres`, `migrate`, `api`, `worker`, `web`.

If Postgres exits with `Permission denied` on `/docker-entrypoint-initdb.d/` (common
with rootless Podman + SELinux bind mounts), stop and use **hybrid** instead. Do not
keep retrying the same Compose postgres service.

### Hybrid (`up-hybrid`)

Used when full Compose cannot start Postgres:

1. Run only Postgres in a container **without** the `./postgres/init` bind mount
   (migrations already `CREATE EXTENSION IF NOT EXISTS pg_trgm`).
2. On the host: `migrate up`, `registry sync`, build/run `apps/api` + `apps/worker`.
3. On the host: `apps/web` via `npm run dev` with `FIRMSCOUT_API_URL=http://localhost:8080`.

Default DB URL for host processes:

```text
postgres://firmscout:firmscout-dev-only-not-a-real-secret@localhost:5432/firmscout?sslmode=disable
```

(values from `infrastructure/docker/.env` / `.env.example`)

## After start

1. Smoke: `GET http://localhost:8080/healthz` → `{"status":"ok"}`
2. Smoke: `GET http://localhost:3000/` → HTTP 200
3. Optional: `go run ./apps/cli registry sync` if vendors/products look empty
4. Tell the user the URLs; catalogue may be sparse — MikroTik sources ship disabled
   until terms review (expected).

## Do not

- Do not require Docker Desktop when Podman works.
- Do not use `make up` / `scripts/dev.sh up` as the Podman path (Docker-only today).
- Do not invent DB credentials; read `.env` / `.env.example`.
- Do not start the observability profile unless the user asks
  (`--profile observability`).

## URLs

| Service | URL |
| --- | --- |
| Website | http://localhost:3000 |
| API health | http://localhost:8080/healthz |
| API vendors | http://localhost:8080/api/v1/vendors |
