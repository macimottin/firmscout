#!/usr/bin/env bash
# Developer helper for the tasks people run every day.
#
# Every subcommand prints the command it is about to run, so that when something fails
# you can copy the line and debug it directly rather than debugging this script.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/infrastructure/docker/docker-compose.yml"

run() {
  echo "+ $*"
  "$@"
}

require_docker() {
  if ! command -v docker >/dev/null 2>&1; then
    cat >&2 <<'EOF'
Error: docker is not available on PATH.

On Windows with WSL, this usually means Docker Desktop is not running, or its WSL
integration is not enabled for this distribution (Settings -> Resources -> WSL
integration).

Everything that does not need a database still works without Docker:
  scripts/dev.sh check
  go run ./apps/cli registry validate
EOF
    exit 1
  fi
  if ! docker info >/dev/null 2>&1; then
    echo "Error: the docker daemon is not reachable. Is Docker Desktop running?" >&2
    exit 1
  fi
}

require_go() {
  if ! command -v go >/dev/null 2>&1; then
    echo "Error: the go toolchain is not on PATH (Go 1.27 or later is required)." >&2
    exit 1
  fi
  if [ ! -f "$ROOT_DIR/go.mod" ]; then
    echo "Error: go.mod not found; run this from inside the repository." >&2
    exit 1
  fi
}

compose() {
  require_docker
  run docker compose -f "$COMPOSE_FILE" --project-directory "$ROOT_DIR" "$@"
}

cmd_up() {
  compose up -d --build
  echo
  echo "Website: http://localhost:3000"
  echo "API:     http://localhost:8080/healthz"
  echo
  echo "Add the observability profile for Grafana, Prometheus, Loki and Tempo:"
  echo "  docker compose -f $COMPOSE_FILE --profile observability up -d"
}

cmd_down()    { compose down; }
cmd_logs()    { compose logs -f; }
cmd_migrate() { require_go; cd "$ROOT_DIR" && run go run ./apps/cli migrate up; }
cmd_test()    { require_go; cd "$ROOT_DIR" && run go test ./... -count=1; }
cmd_lint()    { require_go; cd "$ROOT_DIR" && run golangci-lint run ./...; }
cmd_fmt()     { require_go; cd "$ROOT_DIR" && run gofmt -w .; }
cmd_generate() { require_go; cd "$ROOT_DIR" && run sqlc generate; }
cmd_mermaid() { run bash "$SCRIPT_DIR/check-mermaid.sh"; }
cmd_schemas() { run python3 "$SCRIPT_DIR/check-schemas.py"; }

# cmd_check is what CI approximates and what you should run before opening a pull
# request. It stops at the first failure rather than reporting a wall of them.
cmd_check() {
  require_go
  cd "$ROOT_DIR" || exit 1

  echo "=== gofmt ==="
  local unformatted
  unformatted="$(gofmt -l . | grep -v node_modules || true)"
  if [ -n "$unformatted" ]; then
    echo "These files need formatting (run 'scripts/dev.sh fmt'):"
    echo "$unformatted"
    exit 1
  fi
  echo "clean"

  echo
  echo "=== go vet ==="
  run go vet ./... || exit 1

  echo
  echo "=== golangci-lint ==="
  if command -v golangci-lint >/dev/null 2>&1; then
    run golangci-lint run ./... || exit 1
  else
    echo "skipped: golangci-lint is not installed"
    echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2"
  fi

  echo
  echo "=== go test ==="
  # PostgreSQL-backed tests skip themselves unless FIRMSCOUT_TEST_DATABASE_URL is set,
  # and say so, so a run without a database is honest rather than falsely green.
  run go test ./... -count=1 || exit 1

  echo
  echo "=== architecture dependency rule ==="
  run bash "$SCRIPT_DIR/archcheck.sh" || exit 1

  echo
  echo "=== mermaid diagrams ==="
  run bash "$SCRIPT_DIR/check-mermaid.sh" || exit 1

  echo
  echo "=== registry schemas ==="
  if python3 -c "import jsonschema, yaml" >/dev/null 2>&1; then
    run python3 "$SCRIPT_DIR/check-schemas.py" || exit 1
  else
    echo "skipped: python3 with jsonschema and pyyaml is required"
    echo "  pip install jsonschema pyyaml"
  fi

  echo
  echo "All checks passed."
}

usage() {
  cat <<'EOF'
Usage: scripts/dev.sh <command>

  up        Build and start the stack (PostgreSQL, API, worker, website)
  down      Stop the stack
  logs      Follow the stack's logs
  migrate   Apply pending database migrations
  test      Run the Go test suite
  lint      Run golangci-lint
  fmt       Format Go code
  generate  Regenerate sqlc code
  mermaid   Validate every Mermaid diagram
  schemas   Validate the Git-managed registry against its JSON Schemas
  check     Everything above that is a check, in order. Run this before a pull request.
  help      Show this message

Commands that need Docker say so clearly when it is unavailable, and the rest still work.
EOF
}

case "${1:-help}" in
  up)       cmd_up ;;
  down)     cmd_down ;;
  logs)     cmd_logs ;;
  migrate)  cmd_migrate ;;
  test)     cmd_test ;;
  lint)     cmd_lint ;;
  fmt)      cmd_fmt ;;
  generate) cmd_generate ;;
  mermaid)  cmd_mermaid ;;
  schemas)  cmd_schemas ;;
  check)    cmd_check ;;
  help|-h|--help) usage ;;
  *) echo "Unknown command: $1" >&2; echo; usage; exit 1 ;;
esac
