#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
python3 tests/test_foundation_lanes.py
python3 tests/test_stripe_live_workflow.py
python3 tests/test_stripe_live_output.py
python3 tests/test_batch_hygiene.py
bash scripts/check_gofmt.sh
# Reconcile erasure declarations before broader runtime checks can mask their RED.
python3 tests/test_privacy_registry.py
GOPROXY=off GOSUMDB=off go test -count=1 ./...
GOPROXY=off GOSUMDB=off go vet ./...
GOPROXY=off GOSUMDB=off go run ./cmd/preflight fixtures/home-valid.json
if GOPROXY=off GOSUMDB=off go run ./cmd/preflight fixtures/home-invalid.json; then
 echo "ERROR: invalid fixture was accepted" >&2; exit 1
fi
bun scripts/e2e/check-assertion-messages.ts
bun tests/e2e/summary-reporter.test.ts
python3 scripts/check_contract.py
python3 scripts/check_ci_pins.py
python3 scripts/check_modules.py
python3 tests/test_modules.py
python3 tests/test_module_sql.py
python3 tests/test_migration_runner.py
python3 tests/test_pending_migrations.py
python3 tests/test_check_pg_runner.py
python3 tests/test_reliability_contract_static.py
python3 tests/test_sqlc_contract_static.py
python3 tests/test_shipping_contract_static.py
python3 tests/test_api_contract.py
python3 tests/test_customer_auth_contract.py
python3 tests/test_fulfillment_contract.py
python3 tests/test_quotes_contract.py
python3 tests/test_notifications_contract_static.py
python3 tests/test_notifications_api.py
python3 scripts/privacy_registry.py
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest tests.test_ops_deploy tests.test_ops_backup tests.test_ops_drill tests.test_ops_connection_policy
python3 scripts/check_oapi_codegen.py
python3 tests/test_generated_patch.py
python3 tests/test_identity_login_contract.py
for script in scripts/*.sh; do
 bash -n "$script"
done
bash scripts/check_frontend.sh
python3 tests/test_pg_role_fixture.py
echo 'PASS: offline checks (does not test PostgreSQL or production API)'
