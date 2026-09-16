#!/usr/bin/env bash
# FirmScout local-stack helper for Cursor agents and humans.
# Prefers Podman; falls back to Docker. Supports a hybrid host+Postgres mode.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../../../.." && pwd)"
COMPOSE_DIR="$ROOT_DIR/infrastructure/docker"
COMPOSE_FILE="$COMPOSE_DIR/docker-compose.yml"
ENV_FILE="$COMPOSE_DIR/.env"
ENV_EXAMPLE="$COMPOSE_DIR/.env.example"
RUN_DIR="${FIRMSCOUT_RUN_DIR:-/tmp/firmscout-run}"
DEFAULT_DATABASE_URL='postgres://firmscout:firmscout-dev-only-not-a-real-secret@localhost:5432/firmscout?sslmode=disable'

log() { printf '+ %s\n' "$*"; }
die() { printf 'Error: %s\n' "$*" >&2; exit 1; }

detect_engine() {
  if command -v podman >/dev/null 2>&1 && podman info >/dev/null 2>&1; then
    echo podman
    return 0
  fi
  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    echo docker
    return 0
  fi
  return 1
}

require_engine() {
  local engine
  if ! engine="$(detect_engine)"; then
    die "neither Podman nor Docker is available/reachable"
  fi
  echo "$engine"
}

compose_cmd() {
  local engine="$1"
  shift
  cd "$COMPOSE_DIR"
  case "$engine" in
    podman)
      # Avoid non-interactive short-name TTY prompts on Fedora.
      CONTAINERS_SHORT_NAME_MODE="${CONTAINERS_SHORT_NAME_MODE:-disabled}" \
        podman compose -f docker-compose.yml "$@"
      ;;
    docker)
      # Do not pass --project-directory <repo-root>: relative paths in the
      # Compose file are meant to resolve from infrastructure/docker/.
      docker compose -f docker-compose.yml "$@"
      ;;
    *)
      die "unknown engine: $engine"
      ;;
  esac
}

ensure_env() {
  if [[ ! -f "$ENV_FILE" ]]; then
    [[ -f "$ENV_EXAMPLE" ]] || die "missing $ENV_EXAMPLE"
    log "cp $ENV_EXAMPLE $ENV_FILE"
    cp "$ENV_EXAMPLE" "$ENV_FILE"
  fi
}

ensure_web_prereqs() {
  mkdir -p "$ROOT_DIR/apps/web/public"
  if [[ ! -e "$ROOT_DIR/apps/web/public/.gitkeep" && -z "$(ls -A "$ROOT_DIR/apps/web/public" 2>/dev/null || true)" ]]; then
    touch "$ROOT_DIR/apps/web/public/.gitkeep"
  fi
  if ! grep -q 'output:[[:space:]]*"standalone"' "$ROOT_DIR/apps/web/next.config.ts" 2>/dev/null; then
    die "apps/web/next.config.ts must set output: \"standalone\" for web.Dockerfile"
  fi
}

database_url_from_env() {
  if [[ -n "${FIRMSCOUT_DATABASE_URL:-}" ]]; then
    printf '%s\n' "$FIRMSCOUT_DATABASE_URL"
    return
  fi
  if [[ -f "$ENV_FILE" ]]; then
    # shellcheck disable=SC1090
    set -a
    # Only export the vars we need; .env has comments and is safe for local use.
    # Prefer sourcing carefully — values are local-dev defaults.
    POSTGRES_USER="$(grep -E '^POSTGRES_USER=' "$ENV_FILE" | head -1 | cut -d= -f2-)"
    POSTGRES_PASSWORD="$(grep -E '^POSTGRES_PASSWORD=' "$ENV_FILE" | head -1 | cut -d= -f2-)"
    POSTGRES_DB="$(grep -E '^POSTGRES_DB=' "$ENV_FILE" | head -1 | cut -d= -f2-)"
    POSTGRES_PORT="$(grep -E '^POSTGRES_PORT=' "$ENV_FILE" | head -1 | cut -d= -f2-)"
    set +a
    POSTGRES_USER="${POSTGRES_USER:-firmscout}"
    POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-firmscout-dev-only-not-a-real-secret}"
    POSTGRES_DB="${POSTGRES_DB:-firmscout}"
    POSTGRES_PORT="${POSTGRES_PORT:-5432}"
    printf 'postgres://%s:%s@localhost:%s/%s?sslmode=disable\n' \
      "$POSTGRES_USER" "$POSTGRES_PASSWORD" "$POSTGRES_PORT" "$POSTGRES_DB"
    return
  fi
  printf '%s\n' "$DEFAULT_DATABASE_URL"
}

cmd_detect() {
  local engine
  engine="$(require_engine)"
  echo "engine=$engine"
  echo "compose_dir=$COMPOSE_DIR"
  echo "compose_file=$COMPOSE_FILE"
}

cmd_up() {
  local engine
  engine="$(require_engine)"
  ensure_env
  ensure_web_prereqs
  log "using engine=$engine"
  compose_cmd "$engine" up -d --build
  echo
  echo "Website: http://localhost:3000"
  echo "API:     http://localhost:8080/healthz"
  echo
  echo "If Postgres fails with Permission denied on /docker-entrypoint-initdb.d,"
  echo "run: bash $SCRIPT_DIR/local-stack.sh up-hybrid"
}

wait_postgres() {
  local engine="$1"
  local i
  for i in $(seq 1 40); do
    if "$engine" exec firmscout-postgres pg_isready -U firmscout -d firmscout >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  die "Postgres did not become ready"
}

cmd_up_hybrid() {
  local engine db_url
  engine="$(require_engine)"
  ensure_env
  ensure_web_prereqs
  db_url="$(database_url_from_env)"

  command -v go >/dev/null 2>&1 || die "Go is required for hybrid mode"
  command -v node >/dev/null 2>&1 || die "Node is required for hybrid mode"
  command -v npm >/dev/null 2>&1 || die "npm is required for hybrid mode"

  export GOTOOLCHAIN="${GOTOOLCHAIN:-auto}"
  export FIRMSCOUT_DATABASE_URL="$db_url"

  log "stopping any previous Compose stack (best-effort)"
  compose_cmd "$engine" down >/dev/null 2>&1 || true
  "$engine" rm -f firmscout-postgres firmscout-migrate firmscout-api firmscout-worker firmscout-web >/dev/null 2>&1 || true

  "$engine" network exists firmscout >/dev/null 2>&1 || "$engine" network create firmscout >/dev/null
  "$engine" volume exists firmscout_postgres_data >/dev/null 2>&1 || "$engine" volume create firmscout_postgres_data >/dev/null

  if "$engine" ps -a --format '{{.Names}}' | grep -qx firmscout-postgres; then
    log "$engine start firmscout-postgres"
    "$engine" start firmscout-postgres >/dev/null
  else
    log "starting Postgres container (no init bind-mount)"
    "$engine" run -d --name firmscout-postgres \
      --network firmscout \
      -e POSTGRES_USER=firmscout \
      -e POSTGRES_PASSWORD=firmscout-dev-only-not-a-real-secret \
      -e POSTGRES_DB=firmscout \
      -p 5432:5432 \
      -v firmscout_postgres_data:/var/lib/postgresql/data \
      docker.io/library/postgres:17.6-alpine >/dev/null
  fi

  wait_postgres "$engine"

  mkdir -p "$RUN_DIR"
  cd "$ROOT_DIR"
  log "go run ./apps/cli migrate up"
  go run ./apps/cli migrate up
  log "go run ./apps/cli registry sync"
  go run ./apps/cli registry sync

  log "go build api + worker"
  go build -o "$RUN_DIR/api" ./apps/api
  go build -o "$RUN_DIR/worker" ./apps/worker

  if [[ -f "$RUN_DIR/api.pid" ]] && kill -0 "$(cat "$RUN_DIR/api.pid")" 2>/dev/null; then
    kill "$(cat "$RUN_DIR/api.pid")" 2>/dev/null || true
  fi
  if [[ -f "$RUN_DIR/worker.pid" ]] && kill -0 "$(cat "$RUN_DIR/worker.pid")" 2>/dev/null; then
    kill "$(cat "$RUN_DIR/worker.pid")" 2>/dev/null || true
  fi
  if [[ -f "$RUN_DIR/web.pid" ]] && kill -0 "$(cat "$RUN_DIR/web.pid")" 2>/dev/null; then
    kill "$(cat "$RUN_DIR/web.pid")" 2>/dev/null || true
  fi

  nohup env FIRMSCOUT_DATABASE_URL="$db_url" "$RUN_DIR/api" >"$RUN_DIR/api.log" 2>&1 &
  echo $! >"$RUN_DIR/api.pid"
  nohup env FIRMSCOUT_DATABASE_URL="$db_url" "$RUN_DIR/worker" >"$RUN_DIR/worker.log" 2>&1 &
  echo $! >"$RUN_DIR/worker.pid"

  cd "$ROOT_DIR/apps/web"
  if [[ ! -d node_modules ]]; then
    log "npm ci"
    npm ci
  fi
  nohup env FIRMSCOUT_API_URL=http://localhost:8080 npm run dev >"$RUN_DIR/web.log" 2>&1 &
  echo $! >"$RUN_DIR/web.pid"

  echo
  echo "Hybrid stack started (engine=$engine)."
  echo "Website: http://localhost:3000"
  echo "API:     http://localhost:8080/healthz"
  echo "Logs:    $RUN_DIR/*.log"
  echo "PIDs:    $RUN_DIR/*.pid"
}

cmd_down() {
  local engine
  engine="$(detect_engine || true)"
  if [[ -n "${engine:-}" ]]; then
    compose_cmd "$engine" down || true
    "$engine" rm -f firmscout-postgres firmscout-migrate firmscout-api firmscout-worker firmscout-web >/dev/null 2>&1 || true
  fi
  for f in api worker web; do
    if [[ -f "$RUN_DIR/$f.pid" ]]; then
      kill "$(cat "$RUN_DIR/$f.pid")" 2>/dev/null || true
      rm -f "$RUN_DIR/$f.pid"
    fi
  done
  echo "stopped"
}

cmd_status() {
  local engine
  if engine="$(detect_engine)"; then
    echo "engine=$engine"
    "$engine" ps -a --filter name=firmscout --format 'table {{.Names}}\t{{.Status}}\t{{.Ports}}' || true
  else
    echo "engine=(none)"
  fi
  echo
  echo "host processes:"
  for f in api worker web; do
    if [[ -f "$RUN_DIR/$f.pid" ]] && kill -0 "$(cat "$RUN_DIR/$f.pid")" 2>/dev/null; then
      echo "  $f pid=$(cat "$RUN_DIR/$f.pid") running"
    else
      echo "  $f not running"
    fi
  done
}

cmd_logs() {
  local engine
  engine="$(require_engine)"
  if "$engine" ps -a --format '{{.Names}}' | grep -q '^firmscout-'; then
    compose_cmd "$engine" logs -f "$@"
  elif [[ -d "$RUN_DIR" ]]; then
    tail -f "$RUN_DIR"/*.log
  else
    die "no firmscout containers or $RUN_DIR logs found"
  fi
}

cmd_smoke() {
  local ok=0
  if curl -sf http://localhost:8080/healthz | grep -q '"status"[[:space:]]*:[[:space:]]*"ok"'; then
    echo "api: ok"
  else
    echo "api: FAIL (http://localhost:8080/healthz)"
    ok=1
  fi
  code="$(curl -sf -o /dev/null -w '%{http_code}' http://localhost:3000/ || true)"
  if [[ "$code" == "200" ]]; then
    echo "web: ok"
  else
    echo "web: FAIL (http://localhost:3000/ → HTTP ${code:-none})"
    ok=1
  fi
  exit "$ok"
}

usage() {
  cat <<EOF
Usage: local-stack.sh <command>

  detect      Print which container engine will be used (podman preferred)
  up          Build and start the full Compose stack
  up-hybrid   Postgres in a container; API/worker/web on the host
  down        Stop Compose services and hybrid host processes
  status      Show containers and hybrid PIDs
  logs        Follow Compose logs (or hybrid log files)
  smoke       Curl API /healthz and the website
  help        Show this message

Repo root: $ROOT_DIR
EOF
}

main() {
  case "${1:-help}" in
    detect) shift; cmd_detect "$@" ;;
    up) shift; cmd_up "$@" ;;
    up-hybrid) shift; cmd_up_hybrid "$@" ;;
    down) shift; cmd_down "$@" ;;
    status) shift; cmd_status "$@" ;;
    logs) shift; cmd_logs "$@" ;;
    smoke) shift; cmd_smoke "$@" ;;
    help|-h|--help) usage ;;
    *) die "unknown command: ${1:-}"; usage; exit 1 ;;
  esac
}

main "$@"
