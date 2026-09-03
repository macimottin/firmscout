#!/usr/bin/env bash
# Enforce FirmScout's Clean Architecture dependency rule from the command line.
#
# This is the same rule internal/archtest enforces as a Go test; the script exists so
# it can run in a pre-commit hook or a shell without invoking the test binary. When the
# two disagree, the Go test is authoritative -- it walks the same import graph with
# richer output.
#
# The rule, innermost first:
#   internal/domain       -> the Go standard library only
#   internal/application  -> internal/domain and the standard library only
#   internal/adapters/*   -> may not import internal/platform, nor each other
#   internal/platform     -> may import everything; it is the composition root
#
# See ADR-0002 and docs/architecture/clean-architecture.md.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

if [ ! -f go.mod ]; then
  echo "skipped: go.mod not found, no Go module to check yet."
  echo "RESULT: PASSED"
  exit 0
fi

if ! command -v go >/dev/null 2>&1; then
  echo "FAIL: the go toolchain is required to check the dependency rule."
  echo "RESULT: FAILED"
  exit 1
fi

MODULE="$(go list -m 2>/dev/null)"
if [ -z "$MODULE" ]; then
  echo "FAIL: could not determine the module path."
  echo "RESULT: FAILED"
  exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# Direct imports only, one "package<TAB>import" pair per line. Transitive dependencies
# are deliberately not considered: the standard library's own internals (crypto/sha256
# pulling crypto/internal/... for instance) are not a layering violation, and treating
# them as one produces noise that trains people to ignore this check.
if ! go list -f '{{$p := .ImportPath}}{{range .Imports}}{{$p}}	{{.}}
{{end}}' ./... > "$WORK/imports.txt" 2> "$WORK/err.txt"; then
  echo "FAIL: go list failed:"
  sed 's/^/      /' "$WORK/err.txt" | head -20
  echo "RESULT: FAILED"
  exit 1
fi

fail=0
report() {
  echo "FAIL $1 imports $2"
  echo "     $3"
  fail=1
}

# An import path is standard library when its first segment contains no dot. A dot in a
# later segment (crypto/internal/entropy/v1.0.0) is still the standard library.
is_stdlib() {
  case "${1%%/*}" in
    *.*) return 1 ;;
    *)   return 0 ;;
  esac
}

adapter_of() {
  local rest="${1#"$MODULE"/internal/adapters/}"
  echo "${rest%%/*}"
}

while IFS=$'\t' read -r pkg imp; do
  [ -z "$pkg" ] && continue

  case "$pkg" in
    "$MODULE"/internal/domain*)
      if ! is_stdlib "$imp" && [[ "$imp" != "$MODULE"/internal/domain* ]]; then
        report "$pkg" "$imp" "the domain may import only the Go standard library"
      fi
      ;;
    "$MODULE"/internal/application*)
      if ! is_stdlib "$imp" \
        && [[ "$imp" != "$MODULE"/internal/domain* ]] \
        && [[ "$imp" != "$MODULE"/internal/application* ]]; then
        report "$pkg" "$imp" "the application layer may import only internal/domain and the standard library"
      fi
      ;;
    "$MODULE"/internal/adapters/*)
      if [[ "$imp" == "$MODULE"/internal/platform* ]]; then
        report "$pkg" "$imp" "an adapter must not reach into the composition root"
      elif [[ "$imp" == "$MODULE"/internal/adapters/* ]]; then
        if [ "$(adapter_of "$pkg")" != "$(adapter_of "$imp")" ]; then
          report "$pkg" "$imp" "adapters must not depend on each other; share through a port instead"
        fi
      fi
      ;;
  esac
done < "$WORK/imports.txt"

if [ "$fail" -ne 0 ]; then
  echo
  echo "See ADR-0002 and docs/architecture/clean-architecture.md."
  echo "RESULT: FAILED"
  exit 1
fi

echo "Dependency rule holds across every package in $MODULE."
echo "RESULT: PASSED"
