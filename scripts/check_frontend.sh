#!/usr/bin/env bash
# Responsibility: offline frontend source gates; depends on frozen Bun workspace; called by CI/developers.
set -euo pipefail
cd "$(dirname "$0")/.."
test "$(bun --version)" = '1.3.12' || { echo 'FAIL: Bun must be 1.3.12'; exit 1; }
bun run tokens:check
bun run typecheck
bun run lint:styles
bun run contrast
bun run build:admin
bun run build:gallery
bun run test:frontend
echo 'PASS: frontend source gates'
