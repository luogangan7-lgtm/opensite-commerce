#!/usr/bin/env bash
# Responsibility: offline frontend source gates; depends on frozen Bun workspace; called by CI/developers.
set -euo pipefail
cd "$(dirname "$0")/.."
test "$(bun --version)" = '1.3.12' || { echo 'FAIL: Bun must be 1.3.12'; exit 1; }
bun scripts/frontend/check-api-client.ts
bun run tokens:check
bun run typecheck
bun run --filter @opensite/storefront check
bun run lint:styles
bun run contrast
bun run build:admin
bun run build:gallery
bun run build:storefront
bun run test:frontend
bun test tests/frontend/renderer-lifecycle.test.ts
bun test web/storefront/src/lib
bun test web/admin/src
echo 'PASS: frontend source gates'
