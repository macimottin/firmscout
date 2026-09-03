#!/usr/bin/env bash
# Validate every fenced ```mermaid block in the repository.
#
# Validation uses Mermaid's own parser through scripts/mermaid-parse.mjs, which runs
# the real grammar in jsdom. It needs no headless browser, so it works in CI and in
# containers where the usual mermaid-cli approach fails and leaves diagrams unchecked.
#
# If node or the dependencies are missing, this script says so and FAILS rather than
# passing silently. A diagram check that reports success without checking anything is
# worse than no check at all.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

if ! command -v node >/dev/null 2>&1; then
  echo "FAIL: node is required to validate mermaid diagrams (install Node 22 or later)."
  echo "RESULT: FAILED"
  exit 1
fi

if [ ! -d node_modules/mermaid ] || [ ! -d node_modules/jsdom ]; then
  echo "Installing mermaid validation dependencies (mermaid, jsdom) ..."
  if ! npm install --no-audit --no-fund >/dev/null 2>&1; then
    echo "FAIL: could not install the mermaid validation dependencies."
    echo "      Run 'npm install' in the repository root and try again."
    echo "RESULT: FAILED"
    exit 1
  fi
fi

exec node scripts/mermaid-parse.mjs "$ROOT"
