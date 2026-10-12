#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
GOPROXY=off GOSUMDB=off go test -count=1 ./...
GOPROXY=off GOSUMDB=off go vet ./...
GOPROXY=off GOSUMDB=off go run ./cmd/preflight fixtures/home-valid.json
if GOPROXY=off GOSUMDB=off go run ./cmd/preflight fixtures/home-invalid.json; then
 echo "ERROR: invalid fixture was accepted" >&2; exit 1
fi
python3 scripts/check_contract.py
python3 scripts/check_ci_pins.py
python3 scripts/check_modules.py
python3 tests/test_modules.py
python3 tests/test_module_sql.py
python3 tests/test_migration_runner.py
python3 tests/test_check_pg_runner.py
python3 tests/test_sqlc_contract_static.py
python3 tests/test_shipping_contract_static.py
python3 tests/test_api_contract.py
python3 scripts/check_oapi_codegen.py
bash -n scripts/*.sh
echo 'PASS: offline checks (does not test PostgreSQL or production API)'
