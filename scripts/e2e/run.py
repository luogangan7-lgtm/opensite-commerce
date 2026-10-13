#!/usr/bin/env python3
"""Responsibility: CI-only disposable full-stack MVP browser acceptance.

Depends on the existing Compose/ops transport, frozen Bun lock and Playwright;
called only by mvp-e2e.yml. Guard precedes imports/IO, credentials stay private,
all waits are bounded and finally destroys only this invocation's own project.
"""
import argparse
import base64
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import secrets
import signal
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
TENANT = "c0900001-0000-4000-8000-000000000001"
ACTIVE_TESTS = {
    "published storefront home loads actual sealed release",
    "published storefront products loads actual sealed release",
    "published storefront product detail loads actual sealed release",
    "published storefront CMS loads actual sealed release",
    "published navigation and keyboard skip follow real CMS/product routes",
    "missing and unpublished CMS/product routes render accessible real 404",
    "public host resolution denies pending disabled unpublished and unknown domains",
    "real guest browse → cart quantity → checkout → Fake capture → order receipt",
    "real admin login rejects malformed and unknown credentials without a session",
    "real guarded catalog route sends anonymous visitors to login without private reads",
    "real bootstrapped owner logs in, holds a session and signs out",
    "real API denies anonymous catalog reads and creation before data access",
}
GAP_TESTS = {
    "FIXME staff create SKU → media upload → publish",
    "FIXME authenticated admin catalog list and create",
    "FIXME admin orders list and detail on the real orders read API",
}


class CIOnlyError(RuntimeError):
    """Fail before any project import, filesystem write or subprocess."""


class WitnessFailure(RuntimeError):
    """A fixed, non-secret assertion code for the disposable preview witness."""


def require_ci(environ=None):
    env = os.environ if environ is None else environ
    if (env.get("CI") != "true" or env.get("GITHUB_ACTIONS") != "true"
            or not re.fullmatch(r"[1-9][0-9]{0,19}", env.get("GITHUB_RUN_ID", ""))
            or not re.fullmatch(r"[1-9][0-9]{0,5}", env.get("GITHUB_RUN_ATTEMPT", ""))):
        raise CIOnlyError("MVP E2E execution requires GitHub Actions CI with a valid run identity")


def wait_http(url, expected, marker=None, timeout=60):
    """Require a positive current response, not merely an open TCP socket."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=3) as response:
                body = response.read(1024 * 1024)
                if response.status == expected and (marker is None or marker in body):
                    return
        except (OSError, urllib.error.HTTPError):
            pass
        time.sleep(.2)
    raise RuntimeError("MVP E2E positive HTTP readiness timed out")


def verify_browser_report(report):
    """Lock the acceptance inventory: deletion or arbitrary skip is a failure."""
    observed = {}

    def visit(suites):
        for suite in suites:
            for spec in suite.get("specs", []):
                for test in spec.get("tests", []):
                    key = (test.get("projectName"), spec.get("title"))
                    if key in observed:
                        raise RuntimeError("MVP E2E duplicate browser assertion")
                    observed[key] = [item.get("status") for item in test.get("results", [])]
                    filename = Path(spec.get("file", "")).name
                    expected_file = "mvp-gaps.spec.ts" if spec.get("title") in GAP_TESTS else (
                        "admin.spec.ts" if spec.get("title", "").startswith("real ") else "storefront.spec.ts")
                    is_gap = spec.get("title") in GAP_TESTS
                    if (filename != expected_file or test.get("expectedStatus") != ("skipped" if is_gap else "passed")
                            or (is_gap and not any(item.get("type") == "fixme" for item in test.get("annotations", [])))):
                        raise RuntimeError("MVP E2E assertion file/status/FIXME annotation does not match acceptance")
            visit(suite.get("suites", []))

    visit(report.get("suites", []))
    expected = {(project, title): ["passed"] for project in ("desktop", "mobile") for title in ACTIVE_TESTS}
    expected.update({(project, title): ["skipped"] for project in ("desktop", "mobile") for title in GAP_TESTS})
    stats = report.get("stats", {})
    if observed != expected or any(stats.get(key) != value for key, value in
                                   {"expected": 24, "skipped": 6, "unexpected": 0, "flaky": 0}.items()):
        raise RuntimeError("MVP E2E requires the exact desktop/mobile assertion and declared FIXME inventory")
    return stats


def run(receipt_dir):
    # Import application-side tooling only after main's CI guard. Reuse its
    # stdin PostgreSQL secret transport and permission/job witnesses unchanged.
    sys.path.insert(0, str(ROOT / "scripts"))
    import ops_common
    base_compose_command = ops_common.compose_command

    def compose_command(args):
        return base_compose_command([]) + ["-f", str(ROOT / "scripts/e2e/compose.yaml")] + list(args)

    ops_common.compose_command = compose_command
    import ops_drill
    from ops_deploy import LOGIN_ROLES

    receipt_dir = receipt_dir.resolve()
    if not receipt_dir.is_relative_to(ROOT / "work/C24a"):
        raise RuntimeError("MVP E2E receipts must remain in work/C24a")
    receipt_dir.mkdir(parents=True, exist_ok=True)
    for output in (ROOT / "test-results/e2e", ROOT / "playwright-report/e2e"):
        output.mkdir(parents=True, exist_ok=True)
    project = "opensite-e2e-" + os.environ["GITHUB_RUN_ID"] + "-" + os.environ["GITHUB_RUN_ATTEMPT"] + "-" + secrets.token_hex(4)
    saved = dict(os.environ)
    receipt = {"schema": 1, "sha": os.environ.get("GITHUB_SHA"), "run_id": os.environ["GITHUB_RUN_ID"],
               "run_attempt": os.environ["GITHUB_RUN_ATTEMPT"], "project": project,
               "status": "RUNNING", "stage": "initializing", "steps": [], "browser_exit": None, "cleanup": "NOT_RUN",
               "owner_bootstrap": {"status": "NOT_RUN", "available": (ROOT / "cmd/bootstrap-admin").is_dir()},
               "seed_sha256": {str(path.relative_to(ROOT)): hashlib.sha256(path.read_bytes()).hexdigest()
                               for path in [ROOT / "tests/fixtures/storefront_seed.sql", ROOT / "scripts/e2e/seed.sql"]}}
    credentials = {}

    def persist():
        (receipt_dir / "receipt.json").write_text(json.dumps(receipt, indent=2) + "\n")

    def redact(payload):
        for value in credentials.values():
            payload = payload.replace(value.encode(), b"[REDACTED]")
        return payload

    def command(args, name, timeout=900, env=None, check=True):
        # Kill the entire owned CLI process group on timeout/interruption;
        # finally below also removes the invocation's containers and volumes.
        proc = subprocess.Popen(args, cwd=ROOT, env=env, stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, start_new_session=True)
        try:
            output, _ = proc.communicate(timeout=timeout)
        except BaseException:
            os.killpg(proc.pid, signal.SIGTERM)
            try:
                proc.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.communicate(timeout=5)
            raise
        (receipt_dir / (name + ".log")).write_bytes(redact(output))
        if check and proc.returncode:
            raise RuntimeError(f"MVP E2E {name} failed (exit {proc.returncode}; see service logs)")
        return proc.returncode

    # Store secrets outside the workspace mounted by frontend/browser services.
    # No inherited .env, target database/project/credentials or external URL.
    with tempfile.TemporaryDirectory(prefix="opensite-e2e-") as private:
        env_file = Path(private) / ".env"
        def isolated_key():
            return base64.urlsafe_b64encode(secrets.token_bytes(32)).decode().rstrip("=")

        credentials = {"POSTGRES_PASSWORD": secrets.token_hex(32), "JOB_SIGNING_KEY": secrets.token_hex(32),
                       "OPENSITE_AUTH_SIGNING_KEY": secrets.token_hex(32),
                       "OSC_COMMERCE_COOKIE_KEY": isolated_key(),
                       "OPENSITE_STOREFRONT_KEY": isolated_key(),
                       "OPENSITE_FAKE_WEBHOOK_KEY": isolated_key(),
                       "OPENSITE_E2E_OWNER_PASSWORD": secrets.token_hex(32),
                       "OPENSITE_E2E_BOOTSTRAP_PASSWORD": secrets.token_hex(32),
                       **{f"OPENSITE_{kind}_DB_PASSWORD": secrets.token_hex(32) for kind in LOGIN_ROLES}}
        variables = dict(credentials, COMPOSE_PROJECT_NAME=project, OPENSITE_DB="opensite",
                         OPENSITE_API_PORT="8080", OPENSITE_WORKER_PORT="8081", JOB_TENANTS=TENANT,
                         # Staff login requires an HTTPS Origin; the admin preview proxy presents this
                         # exact value to the API (CI-only), see scripts/e2e/admin.vite.config.ts.
                         OPENSITE_ADMIN_ORIGIN="https://admin.e2e.test",
                         OPENSITE_E2E_OWNER_EMAIL="c24-owner@example.test")
        env_file.write_text("".join(f"{key}={value}\n" for key, value in variables.items()))
        env_file.chmod(0o600)
        os.environ.update(variables, OPENSITE_ENV_FILE=str(env_file), OPENSITE_OPS_TIMEOUT="900")
        persist()
        print("E2E_PROJECT: " + project, flush=True)
        clean_env = {key: value for key, value in saved.items()
                     if key in {"PATH", "HOME", "TMPDIR", "LANG", "CI", "GITHUB_ACTIONS", "GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT"}}
        clean_env["OSC_STOREFRONT_ALLOWED_HOSTS"] = "127.0.0.1,localhost"

        def preview_readonly_witness():
            """Exercise the real checkout preview and compare durable PG rows exactly."""
            sku = "c2400008-0000-4000-8000-000000000001"
            api = f"http://127.0.0.1:8080/api/v1/t/{TENANT}"

            def request_json(path, method, payload, headers=None):
                request = urllib.request.Request(
                    api + path, data=json.dumps(payload, separators=(",", ":")).encode(),
                    method=method, headers={"Accept": "application/json", "Content-Type": "application/json", **(headers or {})})
                with urllib.request.urlopen(request, timeout=5) as response:
                    data = response.read(1_048_577)
                    if len(data) > 1_048_576:
                        raise WitnessFailure("response_too_large")
                    return response.status, response.headers, json.loads(data)

            receipt["stage"] = "preview.cart_create"
            status, headers, created = request_json("/carts", "POST", {"currency": "USD"})
            cart_id = created.get("cart", {}).get("id")
            token = created.get("cart_token")
            if (status != 201 or not re.fullmatch(r"[0-9a-f-]{36}", str(cart_id))
                    or not re.fullmatch(r"[A-Za-z0-9_-]{43}", str(token))):
                raise WitnessFailure("cart_create_unexpected")
            receipt["stage"] = "preview.cart_line"
            status, _, _ = request_json(f"/carts/{cart_id}/lines/{sku}", "PUT", {"quantity": 1},
                                         {"X-Cart-Token": token, "If-Match": headers.get("ETag", "")})
            if status != 200:
                raise WitnessFailure("cart_line_unexpected")

            snapshot_sql = f"""
SELECT jsonb_build_object(
 'checkout_sessions',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY x.id),'[]'::jsonb) FROM app.checkout_sessions x WHERE x.tenant_id='{TENANT}'),
 'orders',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY x.id),'[]'::jsonb) FROM app.orders x WHERE x.tenant_id='{TENANT}'),
 'reservations',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY x.id),'[]'::jsonb) FROM app.inventory_reservations x WHERE x.tenant_id='{TENANT}'),
 'movements',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY x.id),'[]'::jsonb) FROM app.inventory_movements x WHERE x.tenant_id='{TENANT}'),
 'stock',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY x.warehouse_id,x.sku_id),'[]'::jsonb) FROM app.stock_levels x WHERE x.tenant_id='{TENANT}')
)::text;
"""
            receipt["stage"] = "preview.pg_before"
            before = ops_common.psql(snapshot_sql).strip()
            baseline = json.loads(before)
            tote_stock = [row for row in baseline["stock"] if row["sku_id"] == sku]
            if (any(baseline[name] for name in ("checkout_sessions", "orders", "reservations"))
                    or len(tote_stock) != 1 or tote_stock[0]["on_hand"] != 20 or tote_stock[0]["reserved"] != 0):
                raise WitnessFailure("pg_baseline_unexpected")
            origin = "https://shop.e2e.test"
            key = base64.urlsafe_b64decode(credentials["OPENSITE_STOREFRONT_KEY"] + "==")
            proof = base64.urlsafe_b64encode(hmac.new(key, f"checkout:{TENANT}:{token}:{origin}".encode(), hashlib.sha256).digest()).decode().rstrip("=")
            receipt["stage"] = "preview.request"
            status, _, preview = request_json("/checkout/preview", "POST", {
                "cart_id": cart_id, "shipping_address": {"country": "US", "postal_code": "10001"}},
                {"X-Cart-Token": token, "X-Cart-ID": cart_id, "Origin": origin, "X-CSRF-Token": proof})
            if status != 200 or any(preview.get(field) != amount for field, amount in {
                    "subtotal_minor": "2400", "tax_minor": "240", "shipping_minor": "500", "total_minor": "3140",
                    "currency": "USD"}.items()):
                raise WitnessFailure("amounts_unexpected")
            receipt["stage"] = "preview.pg_after"
            after = ops_common.psql(snapshot_sql).strip()
            if before != after:
                raise WitnessFailure("pg_rows_changed")
            return {"status": "PASS", "subtotal_minor": "2400", "total_minor": "3140",
                    "pg_snapshot_sha256": hashlib.sha256(before.encode()).hexdigest(),
                    "pg_row_counts": {name: len(rows) for name, rows in baseline.items()}}

        try:
            command(["git", "rev-parse", "HEAD"], "source-head", timeout=10, env=clean_env)
            actual_sha = (receipt_dir / "source-head.log").read_text().strip()
            if not re.fullmatch(r"[0-9a-f]{40}", actual_sha) or actual_sha != receipt["sha"]:
                raise RuntimeError("MVP E2E checked-out HEAD must match GitHub's exact SHA")
            command(["bun", "run", "build:storefront"], "storefront-build", env=clean_env)
            command(["bun", "run", "build:admin"], "admin-build", env=clean_env)
            receipt["steps"].append("locked-frontends-built")
            persist()
            command(compose_command(["up", "-d", "--build", "--wait", "--wait-timeout", "120", "api", "worker"]), "stack-start")
            if ops_common.psql("SHOW server_version_num;").strip() != "180006":
                raise RuntimeError("MVP E2E requires PostgreSQL 18.6")
            ops_drill.healthy("http://127.0.0.1:8080")
            ops_drill.healthy("http://127.0.0.1:8081")
            ops_drill.permissions()
            receipt["steps"].append("pg18.6-migrations-restricted-logins-api-worker-health")
            persist()
            for source in [ROOT / "tests/fixtures/storefront_seed.sql", ROOT / "scripts/e2e/seed.sql"]:
                ops_common.psql(source.read_text())
            # A second seed verifies ownership/value idempotency rather than
            # overwriting fixture drift or silently accepting conflicts.
            for source in [ROOT / "tests/fixtures/storefront_seed.sql", ROOT / "scripts/e2e/seed.sql"]:
                ops_common.psql(source.read_text())
            ops_drill.processed_witness(TENANT)
            # First owner via the real offline CLI (cmd/bootstrap-admin) on a temporary restricted
            # LOGIN holding only app_admin_bootstrap, removed afterwards as deploy/runtime-roles.md says.
            boot = credentials["OPENSITE_E2E_BOOTSTRAP_PASSWORD"]
            ops_common.psql("CREATE ROLE opensite_e2e_bootstrap LOGIN INHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB "
                            f"NOCREATEROLE NOREPLICATION PASSWORD '{boot}'; "
                            "GRANT app_admin_bootstrap TO opensite_e2e_bootstrap; "
                            "GRANT CONNECT ON DATABASE opensite TO opensite_e2e_bootstrap;")
            try:
                command(compose_command(["run", "--rm", "--no-deps", "bootstrap"]), "owner-bootstrap", timeout=600)
            finally:
                ops_common.psql("REVOKE CONNECT ON DATABASE opensite FROM opensite_e2e_bootstrap; "
                                "DROP ROLE IF EXISTS opensite_e2e_bootstrap;")
            owner_tenant = ops_common.psql("SELECT tenant_id FROM app.tenant_memberships WHERE role='owner';").strip()
            if not re.fullmatch(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}", owner_tenant):
                raise RuntimeError("MVP E2E owner bootstrap did not create exactly one owner tenant")
            os.environ["E2E_OWNER_TENANT_ID"] = owner_tenant
            receipt["owner_bootstrap"]["status"] = "PASS"
            receipt["steps"].append("deterministic-seed-rerun-and-real-signed-worker-job")
            persist()
            receipt["stage"] = "frontends.start"
            command(compose_command(["up", "-d", "--wait", "--wait-timeout", "120", "storefront", "admin"]), "frontends-start", timeout=300)
            receipt["stage"] = "frontends.readiness"
            wait_http("http://127.0.0.1:4321/", 200, b"Made for everyday use")
            wait_http("http://127.0.0.1:4173/", 200, b"<div id=\"root\"")
            receipt["preview_readonly"] = preview_readonly_witness()
            receipt["steps"].append("real-ssr-and-admin-positive-readiness")
            persist()
            receipt["stage"] = "browser"
            result = ROOT / "test-results/e2e/results.json"
            result.unlink(missing_ok=True)  # only this CI harness's own stale result
            receipt["browser_exit"] = command(compose_command(["run", "--rm", "--no-deps", "browser"]), "browser", timeout=900, check=False)
            if receipt["browser_exit"] != 0:
                raise RuntimeError("MVP E2E browser assertions failed")
            receipt["browser_stats"] = verify_browser_report(json.loads(result.read_text()))
            receipt["purchase_postconditions"] = json.loads(ops_common.psql("""
DO $verify$
DECLARE t uuid := 'c0900001-0000-4000-8000-000000000001';
        sku uuid := 'c2400008-0000-4000-8000-000000000001';
BEGIN
 IF (SELECT count(*) FROM app.checkout_sessions WHERE tenant_id=t AND phase='paid'
     AND amount_minor=5780 AND items_net_minor=4800 AND tax_minor=480 AND shipping_minor=500)<>2
 OR (SELECT count(*) FROM app.orders WHERE tenant_id=t AND state='paid' AND sealed
     AND total_minor=5780 AND items_net_minor=4800 AND tax_minor=480 AND shipping_minor=500)<>2
 OR (SELECT count(*) FROM app.order_lines l JOIN app.orders o ON (o.tenant_id,o.id)=(l.tenant_id,l.order_id)
     WHERE l.tenant_id=t AND o.state='paid' AND l.sku_id=sku AND l.quantity=2 AND l.line_total_minor=5280)<>2
 OR (SELECT count(*) FROM app.inventory_reservations WHERE tenant_id=t AND sku_id=sku AND state='committed' AND quantity=2)<>2
 OR EXISTS(SELECT FROM app.inventory_reservations WHERE tenant_id=t AND sku_id=sku AND state='active')
 OR (SELECT count(*) FROM app.stock_levels WHERE tenant_id=t AND sku_id=sku AND on_hand=16 AND reserved=0)<>1
 OR (SELECT count(*) FROM app.inventory_movements WHERE tenant_id=t AND sku_id=sku AND kind='commit' AND quantity=2)<>2
 OR (SELECT count(*) FROM app.ledger_transactions j JOIN app.orders o
     ON j.tenant_id=o.tenant_id AND j.source_type='order' AND j.source_id=o.id::text
     WHERE j.tenant_id=t AND j.status='posted' AND o.state='paid')<>2
 OR EXISTS(SELECT FROM app.ledger_transactions j JOIN app.orders o
     ON j.tenant_id=o.tenant_id AND j.source_type='order' AND j.source_id=o.id::text
     LEFT JOIN app.ledger_entries e ON (e.tenant_id,e.transaction_id)=(j.tenant_id,j.id)
     WHERE j.tenant_id=t AND o.state='paid'
     GROUP BY j.id HAVING count(e.line_no)<2 OR sum(e.amount) FILTER(WHERE e.side='debit')
                       IS DISTINCT FROM sum(e.amount) FILTER(WHERE e.side='credit')) THEN
   RAISE EXCEPTION 'C09D_REAL_PURCHASE_POSTCONDITION_FAILED';
 END IF;
END $verify$;
SELECT json_build_object(
 'paid_orders',(SELECT count(*) FROM app.orders WHERE tenant_id='c0900001-0000-4000-8000-000000000001' AND state='paid'),
 'posted_journals',(SELECT count(*) FROM app.ledger_transactions WHERE tenant_id='c0900001-0000-4000-8000-000000000001' AND status='posted'),
 'tote_on_hand',(SELECT on_hand FROM app.stock_levels WHERE tenant_id='c0900001-0000-4000-8000-000000000001'
                AND sku_id='c2400008-0000-4000-8000-000000000001')
)::text;
""").strip())
            receipt["status"] = "PASS"
            receipt["steps"].append("browser-accessibility-and-real-pg-purchase-postconditions")
        except BaseException as error:
            receipt["status"] = "FAIL"
            failure = {"stage": receipt["stage"], "kind": "interrupted" if isinstance(error, KeyboardInterrupt)
                       else "http_status" if isinstance(error, urllib.error.HTTPError)
                       else "assertion" if isinstance(error, WitnessFailure) else "runner_error"}
            if isinstance(error, urllib.error.HTTPError):
                failure["http_status"] = error.code
            elif isinstance(error, WitnessFailure):
                failure["assertion"] = str(error)
            receipt["failure"] = failure
            print("FAIL_STAGE: " + json.dumps(failure, sort_keys=True), file=sys.stderr, flush=True)
            raise
        finally:
            # Collect bounded/sanitized logs before removing only OUR stack.
            cleanup_errors = []
            for args, name in [(["logs", "--no-color", "--tail", "400"], "services"),
                               (["ps", "--all", "--format", "json"], "services-state")]:
                try:
                    command(compose_command(args), name, timeout=30)
                except Exception:
                    cleanup_errors.append(name)
            try:
                command(compose_command(["down", "--volumes", "--remove-orphans", "--timeout", "10"]), "cleanup", timeout=60)
                receipt["cleanup"] = "PASS"
            except Exception:
                receipt["cleanup"] = "FAIL"
                cleanup_errors.append("cleanup")
            if cleanup_errors:
                receipt["status"] = "FAIL"
                receipt["collection_errors"] = cleanup_errors
            persist()
            os.environ.clear()
            os.environ.update(saved)
            ops_common.compose_command = base_compose_command
            if cleanup_errors:
                raise RuntimeError("MVP E2E service evidence or owned cleanup failed")
    print("PASS: MVP E2E in GitHub Actions; exact SHA/run and browser counts in receipt.json", flush=True)


def main():
    try:
        require_ci()
    except CIOnlyError as exc:
        print(str(exc), file=sys.stderr)
        return 2
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--receipt-dir", type=Path, default=ROOT / "work/C24a/ci")
    args = parser.parse_args()
    # Actions cancellation sends SIGTERM; convert it to an exception so the
    # same finally cleanup path runs instead of leaving a silent stack.
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
    try:
        run(args.receipt_dir)
    except (Exception, KeyboardInterrupt):
        print("FAIL: MVP E2E stopped; inspect its sanitized CI receipt and service logs", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
