#!/usr/bin/env python3
"""Responsibility: CI-only disposable full-stack MVP browser acceptance.

Depends on the existing Compose/ops transport, frozen Bun lock and Playwright;
called only by mvp-e2e.yml. Guard precedes imports/IO, credentials stay private,
all waits are bounded and finally destroys only this invocation's own project.
"""
import argparse
import base64
import hashlib
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
    "real admin login rejects malformed and unknown credentials without a session",
    "real guarded catalog route sends anonymous visitors to login without private reads",
    "real bootstrapped owner logs in, holds a session and signs out",
    "real API denies anonymous catalog reads and creation before data access",
    "real owner session reads and creates tenant catalog through production authorization",
}
PAYMENT_TESTS = {"real signed payment webhook rejects forgery and deduplicates concurrent facts"}
ACTIVE_TESTS |= PAYMENT_TESTS
GAP_TESTS = {
    "FIXME staff create SKU → media upload → publish",
    "FIXME admin orders list and detail on the real orders read API",
    "FIXME guest storefront buy → cart → checkout → Fakepay",
}


class CIOnlyError(RuntimeError):
    """Fail before any project import, filesystem write or subprocess."""


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
                        "payments.spec.ts" if spec.get("title") in PAYMENT_TESTS else (
                        "admin.spec.ts" if spec.get("title", "").startswith("real ") else "storefront.spec.ts"))
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
                                   {"expected": 26, "skipped": 6, "unexpected": 0, "flaky": 0}.items()):
        raise RuntimeError("MVP E2E requires the exact desktop/mobile assertion and declared FIXME inventory")
    return stats


PAYMENT_WITNESS_SQL = """
WITH fixtures(project,n) AS (VALUES('desktop',1),('mobile',2)),
ids AS (SELECT project,n,('c1900010-0000-4000-8000-'||lpad(n::text,12,'0'))::uuid AS tenant,
 ('c1900003-0000-4000-8000-'||lpad(n::text,12,'0'))::uuid AS order_id,
 ('c1900002-0000-4000-8000-'||lpad(n::text,12,'0'))::uuid AS sku,
 ('c1900004-0000-4000-8000-'||lpad(n::text,12,'0'))::uuid AS checkout FROM fixtures)
SELECT jsonb_agg(jsonb_build_object('project',f.project,'order_state',o.state,'phase',c.phase,
 'on_hand',s.on_hand,'reserved',s.reserved,
 'inbox',(SELECT count(*) FROM app.webhook_inbox WHERE tenant_id=f.tenant),
 'processed',(SELECT count(*) FROM app.webhook_inbox WHERE tenant_id=f.tenant AND provider='fake' AND provider_account_id='c19-e2e-'||f.project AND environment='test' AND provider_event_id='c19-e2e-'||f.project||'-captured' AND status='processed' AND processed_at IS NOT NULL),
 'hash',(SELECT encode(payload_hash,'hex') FROM app.webhook_inbox WHERE tenant_id=f.tenant AND provider_event_id='c19-e2e-'||f.project||'-captured'),
 'events',(SELECT count(*) FROM app.payment_events WHERE tenant_id=f.tenant),
 'applied',(SELECT count(*) FROM app.payment_events WHERE tenant_id=f.tenant AND provider_account_id='c19-e2e-'||f.project AND claimed_order_id=f.order_id AND provider_ref='fake_c19_e2e_'||f.n AND status='captured' AND amount_minor=270 AND currency='USD' AND applied AND error_code IS NULL),
 'journals',(SELECT count(*) FROM app.ledger_transactions WHERE tenant_id=f.tenant AND source_id=f.order_id::text),
 'debits',(SELECT coalesce(sum(amount),0) FROM app.ledger_entries WHERE tenant_id=f.tenant AND side='debit'),
 'credits',(SELECT coalesce(sum(amount),0) FROM app.ledger_entries WHERE tenant_id=f.tenant AND side='credit'),
 'cash',(SELECT coalesce(sum(CASE WHEN e.side='debit' THEN e.amount ELSE -e.amount END),0) FROM app.ledger_entries e JOIN app.ledger_accounts a ON (a.tenant_id,a.id)=(e.tenant_id,e.account_id) WHERE e.tenant_id=f.tenant AND a.code='cash'),
 'receivable',(SELECT coalesce(sum(CASE WHEN e.side='debit' THEN e.amount ELSE -e.amount END),0) FROM app.ledger_entries e JOIN app.ledger_accounts a ON (a.tenant_id,a.id)=(e.tenant_id,e.account_id) WHERE e.tenant_id=f.tenant AND a.code='accounts_receivable'),
 'paid_outbox',(SELECT count(*) FROM app.outbox_events WHERE tenant_id=f.tenant AND event_type='order.paid' AND payload->>'order_id'=f.order_id::text)))
FROM ids f LEFT JOIN app.orders o ON (o.tenant_id,o.id)=(f.tenant,f.order_id)
LEFT JOIN app.checkout_sessions c ON (c.tenant_id,c.id)=(f.tenant,f.checkout)
LEFT JOIN app.stock_levels s ON s.tenant_id=f.tenant AND s.sku_id=f.sku;
"""


def verify_payment_witness(rows):
    """Require exactly one persisted payment per independently seeded project."""
    if (not isinstance(rows, list) or len(rows) != 2 or any(not isinstance(r, dict) for r in rows)
            or {r.get("project") for r in rows} != {"desktop", "mobile"}):
        raise RuntimeError("MVP signed payment persistence witness inventory differs")
    for row in rows:
        project = row["project"]
        n = 1 if project == "desktop" else 2
        raw = json.dumps({"event_id": f"c19-e2e-{project}-captured", "account_id": f"c19-e2e-{project}",
                          "order_id": f"c1900003-0000-4000-8000-{n:012d}", "order_version": 1,
                          "provider_ref": f"fake_c19_e2e_{n}", "status": "captured",
                          "amount_minor": 270, "currency": "USD"}, separators=(",", ":"))
        expected = {"order_state": "paid", "phase": "paid", "on_hand": 8, "reserved": 0,
                    "inbox": 1, "processed": 1, "hash": hashlib.sha256(raw.encode()).hexdigest(),
                    "events": 1, "applied": 1, "journals": 1, "cash": 270, "receivable": 0, "paid_outbox": 1}
        if any(row.get(key) != value for key, value in expected.items()) or not isinstance(row.get("debits"), int) or row["debits"] <= 0 or row["debits"] != row.get("credits"):
            raise RuntimeError("MVP signed payment inbox/hash/order/stock/ledger witness differs")
    return {"status": "PASS", "projects": rows}


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
               "status": "RUNNING", "steps": [], "browser_exit": None, "cleanup": "NOT_RUN",
               "payment_webhook": {"status": "NOT_RUN"},
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
        credentials = {"POSTGRES_PASSWORD": secrets.token_hex(32), "JOB_SIGNING_KEY": secrets.token_hex(32),
                       "OPENSITE_AUTH_SIGNING_KEY": secrets.token_hex(32),
                       "OPENSITE_FAKE_WEBHOOK_KEY": base64.urlsafe_b64encode(secrets.token_bytes(32)).decode().rstrip("="),
                       "OPENSITE_CUSTOMER_CSRF_SECRET": secrets.token_urlsafe(32),
                       "OPENSITE_E2E_OWNER_PASSWORD": secrets.token_hex(32),
                       "OPENSITE_E2E_BOOTSTRAP_PASSWORD": secrets.token_hex(32),
                       **{f"OPENSITE_{kind}_DB_PASSWORD": secrets.token_hex(32) for kind in LOGIN_ROLES}}
        variables = dict(credentials, COMPOSE_PROJECT_NAME=project, OPENSITE_DB="opensite",
                         OPENSITE_API_PORT="8080", OPENSITE_WORKER_PORT="8081", JOB_TENANTS=TENANT,
                         OPENSITE_SERVING_REFRESH_TENANTS=TENANT,
                         # Staff login requires an HTTPS Origin; the admin preview proxy presents this
                         # exact value to the API (CI-only), see scripts/e2e/admin.vite.config.ts.
                         OPENSITE_ADMIN_ORIGIN="https://admin.e2e.test",
                         OPENSITE_STOREFRONT_ORIGIN="https://store.e2e.test",
                         OPENSITE_E2E_OWNER_EMAIL="c24-owner@example.test")
        env_file.write_text("".join(f"{key}={value}\n" for key, value in variables.items()))
        env_file.chmod(0o600)
        os.environ.update(variables, OPENSITE_ENV_FILE=str(env_file), OPENSITE_OPS_TIMEOUT="900")
        persist()
        print("E2E_PROJECT: " + project, flush=True)
        clean_env = {key: value for key, value in saved.items()
                     if key in {"PATH", "HOME", "TMPDIR", "LANG", "CI", "GITHUB_ACTIONS", "GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT"}}
        clean_env["OSC_STOREFRONT_ALLOWED_HOSTS"] = "127.0.0.1,localhost"
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
            command(compose_command(["up", "-d", "--wait", "--wait-timeout", "120", "storefront", "admin"]), "frontends-start", timeout=300)
            wait_http("http://127.0.0.1:4321/", 200, b"Made for everyday use")
            wait_http("http://127.0.0.1:4173/", 200, b"<div id=\"root\"")
            receipt["steps"].append("real-ssr-and-admin-positive-readiness")
            persist()
            result = ROOT / "test-results/e2e/results.json"
            result.unlink(missing_ok=True)  # only this CI harness's own stale result
            receipt["browser_exit"] = command(compose_command(["run", "--rm", "--no-deps", "browser"]), "browser", timeout=900, check=False)
            if receipt["browser_exit"] != 0:
                raise RuntimeError("MVP E2E browser assertions failed")
            receipt["browser_stats"] = verify_browser_report(json.loads(result.read_text()))
            # Independent persistence witness runs before declaring the browser run PASS.
            payment_rows = json.loads(ops_common.psql(PAYMENT_WITNESS_SQL).strip())
            receipt["payment_webhook"] = {"status": "FAIL", "projects": payment_rows}
            persist()
            receipt["payment_webhook"] = verify_payment_witness(payment_rows)
            receipt["steps"].append("signed-webhook-account-inbox-ledger-order-witness")
            receipt["status"] = "PASS"
            receipt["steps"].append("browser-and-accessibility-assertions")
        except BaseException:
            receipt["status"] = "FAIL"
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
