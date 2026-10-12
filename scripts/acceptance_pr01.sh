#!/usr/bin/env bash
# PR-01 blocking gates. Does not silently skip absent tools/services.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
echo 'GATE 1/3: PostgreSQL 18 schema + RLS/FK/role behavior'
./scripts/check_pg.sh
echo 'GATE 2/3: sqlc generation and pgx compile'
./scripts/check_codegen.sh
echo 'GATE 3/3: offline Go / OpenAPI / migration orchestration checks'
./scripts/check_all.sh
echo 'PASS: PR-01 all blocking gates'
