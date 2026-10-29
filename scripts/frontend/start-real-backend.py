"""Own the CI-only visual stack; reuse migrations, E2E seeds and role provisioning.

No product code or external credentials. Go holds restricted database capabilities;
Astro holds HTTP capabilities only. Playwright owns this process and its children.
"""
import json
import os
import re
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
    rma_tenant = 'c2300001-0000-4000-8000-000000000001'
    rma_site = 'c2300003-0000-4000-8000-000000000001'
    rma_sku = 'c2310008-0000-4000-8000-000000000001'
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
        result = subprocess.run(['psql', '-X', '-qAt', '-v', 'ON_ERROR_STOP=1', '-v', 'VERBOSITY=sqlstate'], input=sql,
                                text=True, capture_output=True, env={**os.environ, **pg}, timeout=60)
        if result.returncode:
            # SQL can contain generated passwords. Never include statement/DSN in diagnostics.
            code = re.search(r'ERROR:\s+([A-Z0-9]{5})\b', result.stderr)
            raise RuntimeError('Real visual seed/query failed SQLSTATE=' + (code.group(1) if code else 'unknown'))
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
            text = (ROOT / seed).read_text()
            # Separate the new RMA tenant/hostname from historical bigint fixtures.
            # Reuse the exact existing catalog/rule seeds with namespaced identities.
            query(text.replace("'localhost'", "'visual-main.localhost.test'"))
            rma_seed = text.replace('c090', 'c230').replace('c240', 'c231')
            rma_seed = rma_seed.replace("'localhost'", "'visual-rma.localhost.test'")
            rma_seed = rma_seed.replace("'127.0.0.1'", "'localhost'").replace('.c09.test', '.rma230.test')
            rma_seed = rma_seed.replace("'c09d-e2e'", "'visual-rma230-e2e'")
            query(rma_seed)
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
        rma_env = {**env, 'OSC_API_ADDR': '127.0.0.1:4185',
                   'OPENSITE_STOREFRONT_TENANT_ID': rma_tenant, 'OPENSITE_SERVING_REFRESH_TENANTS': rma_tenant,
                   'OPENSITE_CHECKOUT_RULE_METADATA': json.dumps({'Profiles': {rma_tenant: 'c2310014-0000-4000-8000-000000000001'},
                     'SKUs': {rma_tenant: {f'c2310008-0000-4000-8000-{n:012d}': {'TaxCategory': 'standard', 'WeightG': 100} for n in (1, 2)}}})}
        spawn([str(owned / 'api')], rma_env)
        wait('http://127.0.0.1:4185/healthz', 204)
        purchase.publish(query, {}, 'http://127.0.0.1:4185', tenant=rma_tenant, site=rma_site)
        # publish() revokes its temporary issuer membership in finally. B2B's
        # separately authorized test staff identity needs its own tenant binding.
        query(f"INSERT INTO app.tenant_memberships(tenant_id,user_id,role) SELECT '{purchase.TENANT}',id,'owner' FROM app.users WHERE email='visual-owner@example.test';")
        query(f"INSERT INTO app.tenant_memberships(tenant_id,user_id,role) SELECT '{rma_tenant}',id,'owner' FROM app.users WHERE email='visual-owner@example.test';")
        # Reuse the existing MVP B2B opt-in composition on the same owned DB.
        # Its existing-order checkout must not replace the cart checkout above.
        b2b = {**env, 'OSC_API_ADDR': '127.0.0.1:4184', 'OPENSITE_STOREFRONT_KEY': '',
               'OPENSITE_CHECKOUT_FAKE_ENABLED': 'true',
               'OPENSITE_CHECKOUT_FAKE_SIGNING_KEY': env['OPENSITE_FAKE_WEBHOOK_KEY'],
               'OPENSITE_SERVING_REFRESH_TENANTS': ''}
        spawn([str(owned / 'api')], b2b)
        wait('http://127.0.0.1:4184/healthz', 204)
        query(f"SELECT set_config('app.tenant_id','{purchase.TENANT}',false); SELECT app.inventory_adjust('{purchase.SKU}',1000,'Visual CI stock','c4490001-0000-4000-8000-000000000001','inbound');")
        query(f"SELECT set_config('app.tenant_id','{rma_tenant}',false); SELECT app.inventory_adjust('{rma_sku}',1000,'Visual RMA stock','c4490001-0000-4000-8000-000000000002','inbound');")
        runtime = {'pg': pg, 'cookieKey': secrets.token_urlsafe(32), 'control': secrets.token_urlsafe(32),
                   'staffPassword': boot_env['OSC_BOOTSTRAP_PASSWORD'],
                   'fakeWebhookKey': env['OPENSITE_FAKE_WEBHOOK_KEY']}
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
             '-subj', '/CN=127.0.0.1', '-addext', 'subjectAltName=IP:127.0.0.1,DNS:localhost',
             '-keyout', str(private_key), '-out', str(certificate)], env)
        private_key.chmod(0o600)
        spawn(['bun', 'scripts/frontend/real-admin.ts'], env)
        wait('https://127.0.0.1:4350/login', context=ssl.create_default_context(cafile=str(certificate)))
        frontend.update(HOST='127.0.0.1', PORT='4349', OSC_COMMERCE_MODE='backend',
                        SERVER_CERT_PATH=str(certificate), SERVER_KEY_PATH=str(private_key),
                        OSC_READ_API_ORIGIN='http://127.0.0.1:4181', OSC_COMMERCE_API_ORIGIN='http://127.0.0.1:4181',
                        OSC_COMMERCE_SITE='https://127.0.0.1:4349', OSC_COMMERCE_COOKIE_KEY=runtime['cookieKey'], OPENSITE_FAKE_ACCOUNT_ID='c09d-e2e')
        spawn(['bash', 'scripts/start_storefront_ssr.sh'], frontend)
        wait('https://127.0.0.1:4349/products/canvas-tote', context=ssl.create_default_context(cafile=str(certificate)))
        spawn(['bun', 'scripts/frontend/real-admin.ts'], {**env,
              'VISUAL_ADMIN_PORT': '4352', 'VISUAL_ADMIN_UPSTREAM': 'http://127.0.0.1:4185'})
        wait('https://localhost:4352/login', context=ssl.create_default_context(cafile=str(certificate)))
        spawn(['bash', 'scripts/start_storefront_ssr.sh'], {**frontend, 'PORT': '4351',
              'OSC_READ_API_ORIGIN': 'http://127.0.0.1:4185', 'OSC_COMMERCE_API_ORIGIN': 'http://127.0.0.1:4185',
              'OSC_COMMERCE_SITE': 'https://localhost:4351', 'OPENSITE_FAKE_ACCOUNT_ID': 'visual-rma230-e2e'})
        wait('https://localhost:4351/products/canvas-tote', context=ssl.create_default_context(cafile=str(certificate)))
        print('VISUAL-REAL: restricted Go + PostgreSQL 18.6 + transparent proxy + Astro ready', flush=True)
        while all(proc.poll() is None for proc in children):
            time.sleep(.2)
        raise RuntimeError('Real visual child exited unexpectedly')
    finally:
        errors = []

        def cleanup(label, action):
            try:
                action()
            except Exception as error:
                # Do not expose SQL, connection details, or process arguments.
                errors.append(f'{label}: {type(error).__name__}')

        def reap(proc):
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                cleanup('kill child', proc.kill)
                cleanup('wait for killed child', proc.wait)

        for proc in reversed(children):
            cleanup('terminate child', proc.terminate)
        for proc in reversed(children):
            cleanup('wait for child', lambda: reap(proc))
        for name in ('runtime.json', 'localhost.pem', 'localhost-key.pem'):
            cleanup(f'remove {name}', lambda: (owned / name).unlink(missing_ok=True))
        pg['PGDATABASE'] = source.path[1:]
        roles = (['visual_bootstrap'] if bootstrap_owned else []) + (
            [login for login, _ in LOGIN_ROLES.values()] if provisioned else [])
        # Remove database ACL dependencies even if DROP DATABASE later fails.
        for role in roles:
            cleanup(f'revoke connect {role}', lambda: query(f'REVOKE CONNECT ON DATABASE {database} FROM {role};'))
        cleanup('drop database', lambda: query(f'DROP DATABASE {database} WITH (FORCE);'))
        for role in roles:
            cleanup(f'drop role {role}', lambda: query(f'DROP ROLE {role};'))
        if errors:
            raise RuntimeError('Real visual cleanup failed: ' + '; '.join(errors))



if __name__ == '__main__':
    try:
        main()
    except KeyboardInterrupt:
        pass
