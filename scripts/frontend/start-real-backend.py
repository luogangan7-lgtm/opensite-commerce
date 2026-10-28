"""Own the CI-only visual stack; reuse migrations, E2E seeds and role provisioning.

No product code or external credentials. Go holds restricted database capabilities;
Astro holds HTTP capabilities only. Playwright owns this process and its children.
"""
import json
import os
from pathlib import Path
import secrets
import signal
import ssl
import subprocess
import sys
import time
import urllib.request
from urllib.parse import urlsplit, urlunsplit, unquote

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'scripts'))
from ops_deploy import LOGIN_ROLES, provision_sql
from e2e import purchase


def main():
    if os.environ.get('GITHUB_ACTIONS') != 'true' or not os.environ.get('GITHUB_RUN_ID'):
        raise RuntimeError('Real visual stack requires GitHub Actions')
    source = urlsplit(os.environ['DATABASE_URL'])
    if source.scheme not in ('postgres', 'postgresql') or source.hostname not in ('postgres', '127.0.0.1', 'localhost'):
        raise RuntimeError('Real visuals require the disposable CI PostgreSQL service')
    database = 'visual_real_' + secrets.token_hex(6)
    pg = dict(PGHOST=source.hostname, PGPORT=str(source.port or 5432), PGUSER=unquote(source.username or ''),
              PGPASSWORD=unquote(source.password or ''), PGDATABASE=source.path[1:], PGCONNECT_TIMEOUT='10')
    children = []
    provisioned = False
    bootstrap_owned = False
    owned = ROOT / '.work/visual-real'
    owned.mkdir(parents=True, exist_ok=True)

    def query(sql):
        result = subprocess.run(['psql', '-X', '-qAt', '-v', 'ON_ERROR_STOP=1'], input=sql,
                                text=True, capture_output=True, env={**os.environ, **pg}, timeout=60)
        if result.returncode:
            # SQL can contain generated passwords. Never include statement/DSN in diagnostics.
            raise RuntimeError('Real visual seed/query failed')
        return result.stdout

    def run(command, env):
        subprocess.run(command, cwd=ROOT, env=env, check=True, timeout=300)

    def spawn(command, env):
        proc = subprocess.Popen(command, cwd=ROOT, env=env)
        children.append(proc)
        return proc

    def wait(url, status=200, context=None):
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            if any(proc.poll() is not None for proc in children):
                raise RuntimeError('Real visual child exited before readiness')
            try:
                with urllib.request.urlopen(url, timeout=2, context=context) as response:
                    if response.status == status:
                        return
            except OSError:
                pass
            time.sleep(.2)
        raise RuntimeError('Real visual HTTP readiness timed out')

    def interrupted(_signal, _frame):
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, interrupted)
    # Serialize cluster role installation against the existing published-read fixture.
    wait('http://127.0.0.1:4175/healthz', 204)
    query(f'CREATE DATABASE {database};')
    pg['PGDATABASE'] = database
    try:
        migration = {**os.environ, 'DATABASE_URL': urlunsplit(source._replace(path='/' + database)),
                     'OPENSITE_ALLOW_PENDING_MIGRATIONS': '1', 'OSC_STOREFRONT_SAMPLE_SEED': '1'}
        run(['bash', 'scripts/migrate_river.sh'], migration)
        run(['bash', 'scripts/migrate.sh'], migration)
        for seed in ['tests/fixtures/storefront_seed.sql', 'scripts/e2e/seed.sql']:
            query((ROOT / seed).read_text())
        passwords = {kind: secrets.token_hex(32) for kind in LOGIN_ROLES}
        names = ','.join("'" + login + "'" for login, _ in LOGIN_ROLES.values())
        if query(f'SELECT count(*) FROM pg_roles WHERE rolname IN ({names});') != '0\n':
            raise RuntimeError('Real visual fixture refuses pre-existing runtime logins')
        query(provision_sql(database, passwords))
        provisioned = True
        # Only non-sensitive process configuration survives into the application.
        env = {key: os.environ[key] for key in ['PATH', 'HOME', 'GOTOOLCHAIN', 'GITHUB_ACTIONS', 'GITHUB_RUN_ID'] if key in os.environ}
        env.update(OPENSITE_DB_HOST=source.hostname, OPENSITE_DB=database,
                   OPENSITE_ENVIRONMENT='test', OSC_API_ADDR='127.0.0.1:4180',
                   OSC_ADMIN_ORIGIN='https://admin.visual.test', OSC_AUTH_SIGNING_KEY_HEX=secrets.token_hex(32),
                   OPENSITE_STOREFRONT_ORIGIN='https://store.visual.test', OPENSITE_STOREFRONT_KEY=secrets.token_urlsafe(32),
                   OPENSITE_CUSTOMER_CSRF_SECRET=secrets.token_urlsafe(32), OPENSITE_CMS_PREVIEW_KEY=secrets.token_urlsafe(32),
                   OPENSITE_STOREFRONT_TENANT_ID=purchase.TENANT, OPENSITE_SERVING_REFRESH_TENANTS=purchase.TENANT,
                   OPENSITE_FAKE_PSP='1', OPENSITE_FAKE_WEBHOOK_KEY=secrets.token_urlsafe(32),
                   NOTIFICATIONS_SMTP_ENABLED='true', NOTIFICATIONS_SMTP_HOST='127.0.0.1', NOTIFICATIONS_SMTP_PORT='4183',
                   OPENSITE_IDEMPOTENCY_ROOT=str(owned / database),
                   OPENSITE_CHECKOUT_RULE_METADATA=json.dumps({'Profiles': {purchase.TENANT: 'c2400014-0000-4000-8000-000000000001'},
                     'SKUs': {purchase.TENANT: {f'c2400008-0000-4000-8000-{n:012d}': {'TaxCategory': 'standard', 'WeightG': 100} for n in (1, 2)}}}))
        env.update({f'OPENSITE_{kind}_DB_PASSWORD': password for kind, password in passwords.items()})
        env['OSC_PUBLIC_DATABASE_URL'] = f"host={source.hostname} dbname={database} user=opensite_domain_login password={passwords['DOMAIN']} sslmode=disable"
        # Offline owner bootstrap uses its own temporary capability, never the runtime login.
        boot_password = secrets.token_hex(32)
        query(f"BEGIN; CREATE ROLE visual_bootstrap LOGIN PASSWORD '{boot_password}'; GRANT app_admin_bootstrap TO visual_bootstrap; GRANT CONNECT ON DATABASE {database} TO visual_bootstrap; COMMIT;")
        bootstrap_owned = True
        boot_env = {**env, 'OSC_BOOTSTRAP_DATABASE_URL': f'host={source.hostname} dbname={database} user=visual_bootstrap password={boot_password} sslmode=disable',
                    'OSC_BOOTSTRAP_PASSWORD': secrets.token_hex(32)}
        run(['go', 'run', './cmd/bootstrap-admin', '--email', 'visual-owner@example.test', '--tenant-name', 'Visual CI'], boot_env)
        query(f'REVOKE CONNECT ON DATABASE {database} FROM visual_bootstrap; DROP ROLE visual_bootstrap;')
        bootstrap_owned = False
        run(['go', 'build', '-tags', 'pgx', '-o', str(owned / 'api'), './cmd/api'], env)
        spawn([str(owned / 'api')], env)
        wait('http://127.0.0.1:4180/healthz', 204)
        purchase.publish(query, {}, 'http://127.0.0.1:4180')
        query(f"SELECT set_config('app.tenant_id','{purchase.TENANT}',false); SELECT app.inventory_adjust('{purchase.SKU}',1000,'Visual CI stock','c4490001-0000-4000-8000-000000000001','inbound');")
        runtime = {'pg': pg, 'cookieKey': secrets.token_urlsafe(32), 'control': secrets.token_urlsafe(32)}
        runtime_path = owned / 'runtime.json'
        runtime_path.write_text(json.dumps(runtime))
        runtime_path.chmod(0o600)
        spawn(['python3', 'scripts/frontend/notification-sink.py'], env)
        wait('http://127.0.0.1:4182/health', 204)
        spawn(['bun', 'scripts/frontend/real-proxy.ts'], env)
        wait('http://127.0.0.1:4181/__visual/health')
        # No database identity, runtime passwords, or bootstrap capability enters Astro.
        frontend = {key: env[key] for key in ['PATH', 'HOME', 'OPENSITE_ENVIRONMENT', 'OPENSITE_STOREFRONT_KEY',
                    'OPENSITE_STOREFRONT_ORIGIN', 'OPENSITE_CUSTOMER_CSRF_SECRET', 'OPENSITE_FAKE_PSP', 'OPENSITE_FAKE_WEBHOOK_KEY']}
        # Astro's standalone adapter supports native TLS. Keep __Host- cookie admission
        # identical to deployment; trust this disposable certificate only in this lane.
        certificate, private_key = owned / 'localhost.pem', owned / 'localhost-key.pem'
        run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
             '-subj', '/CN=127.0.0.1', '-addext', 'subjectAltName=IP:127.0.0.1',
             '-keyout', str(private_key), '-out', str(certificate)], env)
        private_key.chmod(0o600)
        frontend.update(HOST='127.0.0.1', PORT='4349', OSC_COMMERCE_MODE='backend',
                        SERVER_CERT_PATH=str(certificate), SERVER_KEY_PATH=str(private_key),
                        OSC_READ_API_ORIGIN='http://127.0.0.1:4181', OSC_COMMERCE_API_ORIGIN='http://127.0.0.1:4181',
                        OSC_COMMERCE_SITE='https://127.0.0.1:4349', OSC_COMMERCE_COOKIE_KEY=runtime['cookieKey'], OPENSITE_FAKE_ACCOUNT_ID='c09d-e2e')
        spawn(['bash', 'scripts/start_storefront_ssr.sh'], frontend)
        wait('https://127.0.0.1:4349/products/canvas-tote', context=ssl.create_default_context(cafile=str(certificate)))
        print('VISUAL-REAL: restricted Go + PostgreSQL 18.6 + transparent proxy + Astro ready', flush=True)
        while all(proc.poll() is None for proc in children):
            time.sleep(.2)
        raise RuntimeError('Real visual child exited unexpectedly')
    finally:
        for proc in reversed(children):
            proc.terminate()
        for proc in reversed(children):
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
        (owned / 'runtime.json').unlink(missing_ok=True)
        (owned / 'localhost.pem').unlink(missing_ok=True)
        (owned / 'localhost-key.pem').unlink(missing_ok=True)
        pg['PGDATABASE'] = source.path[1:]
        query(f'DROP DATABASE {database} WITH (FORCE);')
        if bootstrap_owned:
            query('DROP ROLE visual_bootstrap;')
        # Group roles are cluster-global: remove ONLY logins created by this invocation,
        # after its database/CONNECT grants are gone, before the writer's second launch.
        if provisioned:
            query(''.join(f'DROP ROLE {login};' for login, _ in LOGIN_ROLES.values()))


if __name__ == '__main__':
    try:
        main()
    except KeyboardInterrupt:
        pass
