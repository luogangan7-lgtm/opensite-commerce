#!/usr/bin/env python3
"""Local isolated compose backup/volume-loss/restore acceptance, never production.

Depends on Docker, ops_deploy and ops_backup; CI/manual operators call the shell
wrapper. Only a fresh random project is destroyed; all writes stop before dump.
"""
import argparse
import base64
import hashlib
import hmac
from http.cookies import CookieError, SimpleCookie
import json
import math
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.request
import urllib.error
import urllib.parse
import uuid
from unittest.mock import patch

from ops_common import ROOT, OpsError, compose, compose_command, pg_command, psql, quote_ident
from ops_deploy import DeployError, LOGIN_ROLES, preflight
from ops_backup import backup, compare_tables, digest, read_manifest, restore, table_manifest


def require(value, message):
    if not value: raise OpsError(message)


def live_url(service='api', port='8080'):
    binding = compose(['port', service, port]).stdout.decode().strip()
    require(binding.startswith('127.0.0.1:'), 'API must bind loopback only')
    return 'http://' + binding


def healthy(url):
    for _ in range(60):
        try:
            with urllib.request.urlopen(url + '/healthz', timeout=3) as response:
                if response.status == 204: return
        except OSError: pass
        time.sleep(0.2)  # health polling, never concurrent invariant ordering
    raise OpsError('API liveness timed out')


def staff_login_reachable(url):
    # Compose must install staff login on the auth DSN; a 404 here is the
    # silent-omission regression (PR #43 thread on cmd/api/main.go).
    with urllib.request.urlopen(url + '/api/v1/auth/login', timeout=5) as response:
        body = json.loads(response.read())
        require(response.status == 200 and isinstance(body.get('csrf_token'), str) and body['csrf_token'],
                'staff login challenge unavailable in compose deployment')


def start_api():
    compose(['up', '-d', '--build', '--wait', '--wait-timeout', '120', 'api', 'worker', 'mailpit'])
    url = live_url()
    healthy(url)
    healthy(live_url('worker','8081'))
    staff_login_reachable(url)
    return url


CUSTOMER_SESSION = '__Host-osc_customer_session'
CUSTOMER_CHALLENGE = '__Host-osc_customer_challenge'


def witness_json(url, path, expected=200, body=None, cookies=None, csrf=None):
    """Real loopback transport, bounded responses, no secret-bearing diagnostics.

    The stack intentionally has no TLS terminator. Carry validated Secure cookie
    values explicitly on loopback only; browser cookie/TLS behavior has its own
    E2E gate. Every write still carries the pinned HTTPS Origin and signed CSRF.
    """
    target = urllib.parse.urlsplit(url)
    require(target.scheme == 'http' and target.hostname == '127.0.0.1'
            and target.path == '' and not target.query and not target.fragment
            and target.username is None and path.startswith('/'), 'unsafe witness transport')
    headers = {'Accept': 'application/json'}
    if cookies:
        require(set(cookies) <= {CUSTOMER_SESSION, CUSTOMER_CHALLENGE}
                and all(re.fullmatch(r'[A-Za-z0-9_.-]+', value) for value in cookies.values()),
                'invalid customer witness cookie')
        headers['Cookie'] = '; '.join(f'{key}={value}' for key, value in cookies.items())
    payload = None
    if body is not None:
        headers.update({'Content-Type': 'application/json',
                        'Origin': os.environ['OPENSITE_STOREFRONT_ORIGIN'],
                        'X-CSRF-Token': csrf or ''})
        payload = json.dumps(body).encode()
    request = urllib.request.Request(url + path, data=payload, headers=headers)
    try:
        response = urllib.request.urlopen(request, timeout=5)
    except urllib.error.HTTPError as error:
        response = error
    except OSError:
        raise OpsError('customer witness transport unavailable') from None
    with response:
        require(response.status == expected, 'customer witness returned unexpected status')
        raw = response.read(65537)
        require(len(raw) <= 65536, 'customer witness response too large')
        try:
            data = json.loads(raw) if raw else {}
        except (ValueError, UnicodeError):
            raise OpsError('customer witness returned invalid JSON') from None
        require(isinstance(data, dict), 'customer witness response must be an object')
        return data, response.headers


def witness_cookies(headers, cookies):
    """Keep only the customer cookies after checking their actual wire flags."""
    for header in headers.get_all('Set-Cookie', []):
        parsed = SimpleCookie()
        try:
            parsed.load(header)
        except CookieError:
            raise OpsError('invalid customer witness cookie') from None
        require(bool(parsed) and set(parsed) <= {CUSTOMER_SESSION, CUSTOMER_CHALLENGE},
                'unexpected customer witness cookie')
        for name, value in parsed.items():
            require(value['secure'] and value['httponly'] and value['path'] == '/'
                    and value['samesite'].lower() == 'lax' and not value['domain'],
                    'customer witness cookie lost security attributes')
            if value['max-age'] == '0' or not value.value:
                cookies.pop(name, None)
            else:
                cookies[name] = value.value


def proof_from_message(message, recipient, tenant):
    """Only consume this synthetic recipient's token from the browser fragment."""
    recipients = message.get('To', []) if isinstance(message, dict) else []
    require(len(recipients) == 1 and recipients[0].get('Address') == recipient,
            'proof mailbox recipient mismatch')
    lines = str(message.get('Text', '')).splitlines()
    require(len(lines) == 2, 'proof mailbox link missing')
    link = urllib.parse.urlsplit(lines[1])
    origin = urllib.parse.urlsplit(os.environ['OPENSITE_STOREFRONT_ORIGIN'])
    try:
        fragment = urllib.parse.parse_qs(link.fragment, strict_parsing=True)
    except ValueError:
        raise OpsError('proof mailbox link contract mismatch') from None
    require(link.scheme == origin.scheme and link.netloc == origin.netloc
            and link.path == '/customer/verify' and not link.query
            and set(fragment) == {'tenant', 'token'} and fragment['tenant'] == [tenant]
            and len(fragment['token']) == 1, 'proof mailbox link contract mismatch')
    token = fragment['token'][0]
    require(re.fullmatch(r'[A-Za-z0-9_-]{43}', token) is not None
            and base64.urlsafe_b64encode(base64.urlsafe_b64decode(token + '=')).decode().rstrip('=') == token,
            'invalid proof mailbox token')
    return token


def mailbox_ready():
    mailbox = live_url('api', '8025')
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        try:
            witness_json(mailbox, '/api/v1/messages?limit=1')
            return
        except OpsError as error:
            if str(error) != 'customer witness transport unavailable':
                raise
        time.sleep(0.2)
    raise OpsError('local proof mailbox readiness timed out')


def witness_id(value):
    try:
        valid = isinstance(value, str) and str(uuid.UUID(value)) == value
    except ValueError:
        valid = False
    require(valid, 'invalid customer witness ID')
    return value


def mailbox_proof(recipient, tenant):
    """Wait for local capture readiness/delivery; never fetch the secret URL."""
    mailbox = live_url('api', '8025')
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        try:
            listing, _ = witness_json(mailbox, '/api/v1/messages?limit=50')
            for entry in listing.get('messages', []):
                if not any(item.get('Address') == recipient for item in entry.get('To', [])):
                    continue
                message_id = entry.get('ID', '')
                require(isinstance(message_id, str) and re.fullmatch(r'[A-Za-z0-9-]{1,128}', message_id),
                        'invalid proof mailbox message ID')
                message, _ = witness_json(mailbox, '/api/v1/message/' + message_id)
                return proof_from_message(message, recipient, tenant)
        except OpsError as error:
            if str(error) != 'customer witness transport unavailable':
                raise
        time.sleep(0.2)  # bounded readiness polling, not invariant ordering
    raise OpsError('local proof mailbox delivery timed out')


def customer_witness(url, tenant):
    """Register, prove, login and create an owned cart through deployed routes."""
    witness_id(tenant)
    mailbox_ready()
    base = f'/api/v1/t/{tenant}/customer'
    cookies = {}
    challenge, headers = witness_json(url, base + '/session')
    witness_cookies(headers, cookies)
    csrf = challenge.get('csrf_token')
    require(challenge.get('customer') is None and isinstance(csrf, str) and csrf
            and cookies.get(CUSTOMER_CHALLENGE) == csrf, 'customer challenge unavailable')
    recipient = 'buyer-' + uuid.uuid4().hex + '@drill.test'
    credential = secrets.token_urlsafe(32)
    credentials = dict(email=recipient, password=credential)
    pending, _ = witness_json(url, base + '/registrations', 202, credentials, cookies, csrf)
    require(pending.get('status') == 'verification_pending', 'registration proof not pending')
    witness_json(url, base + '/session', 401, credentials, cookies, csrf)
    # SMTP send returns only after local capture acknowledges the envelope.
    proof = mailbox_proof(recipient, tenant)
    witness_json(url, base + '/verifications', 204, {'token': proof}, cookies, csrf)
    login, headers = witness_json(url, base + '/session', 200, credentials, cookies, csrf)
    witness_cookies(headers, cookies)
    current = login.get('customer', {})
    customer = current.get('customer_id', '')
    session = current.get('session_id', '')
    for value in (tenant, customer, session):
        witness_id(value)
    require(current.get('tenant_id') == tenant and 'token' not in current
            and CUSTOMER_SESSION in cookies and CUSTOMER_CHALLENGE not in cookies,
            'customer login did not create isolated session')
    csrf = login.get('csrf_token')
    require(isinstance(csrf, str) and csrf, 'customer session CSRF missing')
    created, headers = witness_json(url, f'/api/v1/t/{tenant}/carts', 201,
                                    {'currency': 'USD'}, cookies, csrf)
    cart = created.get('cart', {}).get('id', '')
    witness_id(cart)
    require('cart_token' not in created and headers.get('ETag', '').startswith('"'),
            'registered cart witness missing')
    witness = {'tenant': tenant, 'customer': customer, 'session': session,
               'cart': cart, 'cookies': cookies}
    restored_customer_witness(url, witness)
    print('PASS: real customer registration → local SMTP proof → login → owned cart', flush=True)
    return witness


def restored_customer_witness(url, witness):
    """The same restored session must identify the same customer and cart."""
    tenant, customer, session, cart = (witness_id(witness[key]) for key in ('tenant', 'customer', 'session', 'cart'))
    current, _ = witness_json(url, f'/api/v1/t/{tenant}/customer/session', cookies=witness['cookies'])
    require(current.get('customer', {}).get('customer_id') == customer
            and current['customer'].get('session_id') == session, 'restored customer session mismatch')
    view, _ = witness_json(url, f'/api/v1/t/{tenant}/carts/{cart}', cookies=witness['cookies'])
    require(view.get('id') == cart, 'restored customer cart unavailable')
    require(psql(f"SELECT count(*) FROM app.carts k JOIN app.customers c ON c.tenant_id=k.tenant_id AND c.id=k.customer_id WHERE k.tenant_id='{tenant}' AND k.id='{cart}' AND k.customer_id='{customer}' AND k.guest_token_hash IS NULL AND c.verified_at IS NOT NULL;").strip() == '1',
            'customer cart ownership or email proof not persisted')


def permissions():
    # Check effective memberships and database ACL, independent of provisioning SQL.
    require(psql("SELECT NOT EXISTS(SELECT 1 FROM aclexplode((SELECT datacl FROM pg_database WHERE datname=current_database())) WHERE grantee=0 AND privilege_type='CONNECT');").strip() == 't', 'PUBLIC still has CONNECT')
    for kind, (login, group) in LOGIN_ROLES.items():
        require(psql(f"SELECT rolinherit AND rolcanlogin AND NOT (rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolreplication) FROM pg_roles WHERE rolname='{login}';").strip() == 't', 'unsafe LOGIN attributes')
        require(psql(f"SELECT string_agg(r.rolname,',' ORDER BY r.rolname) FROM pg_auth_members m JOIN pg_roles r ON r.oid=m.roleid JOIN pg_roles u ON u.oid=m.member WHERE u.rolname='{login}';").strip() == group, 'unexpected LOGIN capability')
        # Real password-authenticated connection, including restricted DDL/read probes.
        env_secret = os.environ[f'OPENSITE_{kind}_DB_PASSWORD']
        args = ['exec','-T','postgres','sh','-c','IFS= read -r PGPASSWORD; export PGPASSWORD; exec psql -X -qAt -h127.0.0.1 -U "$1" -d opensite -v ON_ERROR_STOP=1','ops-login',login]
        sql = "SELECT current_user;\nDO $$ BEGIN BEGIN CREATE TABLE public.ops_forbidden(id int); RAISE EXCEPTION 'DDL_ALLOWED'; EXCEPTION WHEN insufficient_privilege THEN NULL; END; END $$;\n"
        sql += "DO $$ BEGIN BEGIN CREATE TABLE queue.ops_forbidden(id int); RAISE EXCEPTION 'QUEUE_DDL_ALLOWED'; EXCEPTION WHEN insufficient_privilege THEN NULL; END; END $$;\n"
        if kind != 'AUTH':
            sql += "DO $$ BEGIN BEGIN PERFORM password_hash FROM app.users; RAISE EXCEPTION 'CREDENTIAL_READ_ALLOWED'; EXCEPTION WHEN insufficient_privilege THEN NULL; END; END $$;\n"
        if kind != 'CUSTOMER_AUTH':
            sql += "DO $$ BEGIN BEGIN PERFORM password_hash FROM app.customer_credentials; RAISE EXCEPTION 'CUSTOMER_CREDENTIAL_READ_ALLOWED'; EXCEPTION WHEN insufficient_privilege THEN NULL; END; END $$;\n"
        result = compose(args, input=(env_secret+'\n'+sql).encode())
        require(login in result.stdout.decode(), 'login probe did not connect')


def connection_boundaries():
    """Exercise the fresh cluster's HBA over TCP4, TCP6 and Unix sockets."""
    require(psql('SELECT count(*) FROM pg_hba_file_rules WHERE error IS NOT NULL;',
                 'postgres').strip() == '0', 'invalid HBA rules')
    psql('CREATE DATABASE ops_plain;', 'postgres')
    try:
        preflight('opensite', fetch=psql)  # A plain DB is allowed by cluster preflight.
        for kind, (login, _) in LOGIN_ROLES.items():
            password = os.environ[f'OPENSITE_{kind}_DB_PASSWORD']
            for host in ('127.0.0.1', '::1', '/var/run/postgresql'):
                args = ['exec', '-T', 'postgres', 'sh', '-c',
                        'IFS= read -r PGPASSWORD; export PGPASSWORD; '
                        'exec psql -X -qAt -h "$1" -U "$2" -d "$3" -v ON_ERROR_STOP=1',
                        'ops-hba', host, login]
                good = compose(args + ['opensite'], input=(password+'\nSELECT current_user;\n').encode())
                require(good.stdout.decode().strip() == login, 'HBA target login failed')
                for database, candidate, reason in (
                    ('opensite', secrets.token_urlsafe(32), b'password authentication failed'),
                    ('postgres', password, b'pg_hba.conf rejects connection'),
                    ('ops_plain', password, b'pg_hba.conf rejects connection'),
                ):
                    result = subprocess.run(compose_command(args + [database]), cwd=ROOT,
                                            input=(candidate+'\nSELECT current_user;\n').encode(),
                                            capture_output=True, timeout=15)
                    require(result.returncode != 0 and reason in result.stderr,
                            'HBA did not enforce database/password boundary')
    finally:
        psql('DROP DATABASE ops_plain;', 'postgres')
    print('PASS: eight LOGINs authenticate only to app DB; wrong passwords and '
          'maintenance/plain DBs rejected over TCP4/TCP6/Unix', flush=True)


def tainted_login_refusal(archive=None, manifest_path=None, policy=False):
    """Direct ACL, RLS-target and ownership faults fail before password/restore writes."""
    login = 'opensite_runtime_login'
    fingerprint_sql = f"SELECT md5(rolpassword) FROM pg_authid WHERE rolname='{login}';"
    before = psql(fingerprint_sql, 'postgres').strip()
    if policy:
        psql(f'CREATE POLICY ops_stale_login ON app.price_lists TO {login} USING(true);')
        cleanup = lambda: psql('DROP POLICY ops_stale_login ON app.price_lists;')
    elif archive is None:
        psql(f'GRANT SELECT(password_hash) ON app.users TO {login};')
        cleanup = lambda: psql(f'REVOKE SELECT(password_hash) ON app.users FROM {login};')
    else:
        psql(f'CREATE TABLE public.ops_owned_witness(id integer); '
             f'ALTER TABLE public.ops_owned_witness OWNER TO {login};', 'postgres')
        cleanup = lambda: psql('DROP TABLE public.ops_owned_witness;', 'postgres')
    try:
        # A refused attempt must not rotate the login's password either.
        with patch.dict(os.environ, OPENSITE_RUNTIME_DB_PASSWORD=secrets.token_urlsafe(32)):
            if archive is None:
                result = subprocess.run(compose_command(['run', '--rm', '--no-deps',
                                                         'migrate', 'provision']), cwd=ROOT,
                                        capture_output=True, timeout=60)
                require(result.returncode != 0 and b'TAINTED_LOGIN_ROLE' in result.stderr,
                        'direct LOGIN capability was not refused before provisioning')
            else:
                try:
                    restore(archive, manifest_path)
                except OpsError as error:
                    require('TAINTED_LOGIN_ROLE' in str(error), 'ownership refused for unrelated reason')
                else:
                    raise OpsError('owned object was not refused before restore')
                require(psql("SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname='opensite');",
                             'postgres').strip() == 'f', 'role refusal created restore target')
        require(psql(fingerprint_sql, 'postgres').strip() == before,
                'refused operation rotated a login password')
    finally:
        cleanup()
    if archive is None:
        # The ordinary target-only nongrantable CONNECT is a permitted rerun.
        compose(['run', '--rm', '--no-deps', 'migrate', 'provision'])
        permissions()
        kind = 'direct RLS policy' if policy else 'direct credential ACL'
        print(f'PASS: {kind} rejected before writes; clean provision rerun accepted', flush=True)
    else:
        print('PASS: LOGIN object ownership rejected before restore target/password writes', flush=True)


def seed():
    blob = b'OpenSite local restore witness\n'
    content_hash = hashlib.sha256(blob).hexdigest()
    psql(fr"""BEGIN;
INSERT INTO app.tenants(name) VALUES('ops-drill-witness') RETURNING id AS t \gset
INSERT INTO app.users(email,password_hash) VALUES('local-witness@example.invalid','fixture-not-a-credential');
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','OPS-WITNESS') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(:'t',:'p','OPS-SKU','default');
INSERT INTO app.media_blobs(tenant_id,backend,storage_key,sha256,size_bytes,status)
VALUES(:'t','local','ops-witness.bin',decode('{content_hash}','hex'),{len(blob)},'ready');
INSERT INTO app.outbox_events(tenant_id,event_type,payload) VALUES(:'t','ops.drill','{{"fixture":true}}');
INSERT INTO app.outbox_events(tenant_id,event_type,payload,status,attempts) VALUES(:'t','ops.drill.dead','{{"fixture":true}}','dead_letter',10);
COMMIT;""")
    tenant = psql("SELECT id FROM app.tenants WHERE name='ops-drill-witness';").strip()
    job_id = enqueue_witness(tenant, future=True)
    compose(['exec','-T','postgres','sh','-c','cat > /media/ops-witness.bin; chown 65532:65532 /media/ops-witness.bin'], input=blob)
    return blob, content_hash, tenant, job_id


def enqueue_witness(tenant, future=False):
    require(str(uuid.UUID(tenant)) == tenant, 'invalid witness tenant')
    proof = hmac.new(bytes.fromhex(os.environ['JOB_SIGNING_KEY']),
                     f'opensite-job-v1\0inventory_expire_due\0{tenant}\0{1}'.encode(), hashlib.sha256).hexdigest()
    args = json.dumps(dict(tenant_id=tenant,batch=1,proof=proof),separators=(',',':'))
    scheduled = "clock_timestamp()+interval '1 day'" if future else 'clock_timestamp()'
    return psql(f"INSERT INTO queue.river_job(kind,args,scheduled_at,state) VALUES('inventory_expire_due','{args}'::jsonb,{scheduled},'available') RETURNING id;").strip()


def worker_metrics():
    url = live_url('worker','8081')
    with urllib.request.urlopen(url+'/metrics',timeout=5) as response: values = response.read().decode()
    expected = {'opensite_outbox_pending', 'opensite_outbox_dead_letters',
                'opensite_outbox_oldest_pending_age_seconds'}
    samples = {}
    for line in values.splitlines():
        fields = line.split()
        if fields and fields[0] in expected:
            require(len(fields) == 2 and fields[0] not in samples, 'invalid worker metric sample')
            samples[fields[0]] = fields[1]
    require(samples.get('opensite_outbox_pending') == '1'
            and samples.get('opensite_outbox_dead_letters') == '1', 'incorrect worker backlog/DLQ metrics')
    try:
        age = float(samples['opensite_outbox_oldest_pending_age_seconds'])
    except (KeyError, ValueError):
        raise OpsError('invalid worker backlog age') from None
    require(math.isfinite(age) and age >= 0, 'invalid worker backlog age')


def processed_witness(tenant):
    job_id = enqueue_witness(tenant)
    require(job_id.isdigit(), 'missing queued job witness')
    deadline = time.monotonic()+30
    while psql(f'SELECT state FROM queue.river_job WHERE id={job_id};').strip() != 'completed':
        if time.monotonic()>deadline: raise OpsError('River worker did not complete signed expiry witness')
        time.sleep(.2)  # bounded state polling; no concurrency ordering


def migration_predecessor(files):
    """The previous release is every sorted forward migration except the newest."""
    require(len(files) > 1, 'predecessor drill needs at least two forward migrations')
    return files[:-1]


def previous_version_refusal(directory):
    """Real predecessor archive: missing guards mutate the target; fixed guards refuse it.

    Only called after deleting this drill's fresh empty database. Build the
    complete predecessor from immutable migration files and the normal runner;
    the transient prefix runner exists only inside this owned private directory.
    """
    files = sorted((ROOT / 'migrations').glob('[0-9][0-9][0-9]_*.sql'))
    predecessor = migration_predecessor(files)
    prefix = directory / 'predecessor'
    (prefix / 'migrations').mkdir(parents=True)
    (prefix / 'scripts').mkdir()
    shutil.copy2(ROOT / 'scripts/migrate.sh', prefix / 'scripts/migrate.sh')
    for source in predecessor:
        shutil.copy2(source, prefix / 'migrations' / source.name)
    psql('CREATE DATABASE opensite TEMPLATE template0;', 'postgres')
    compose(['run', '--rm', '--no-deps', '--entrypoint', '/bin/sh', 'migrate', '-c',
             'export PGPASSWORD="$POSTGRES_PASSWORD"; '
             'export DATABASE_URL="host=postgres user=postgres dbname=opensite"; '
             'exec /usr/local/bin/river-migrate'])
    runner = Path('/workspace') / prefix.relative_to(ROOT) / 'scripts/migrate.sh'
    pg_command(['bash', '-c',
                'export DATABASE_URL="host=127.0.0.1 user=postgres dbname=opensite"; '
                'exec bash "$1"', 'ops-predecessor', str(runner)])
    archive, manifest_path = backup(directory / 'predecessor-backup')
    manifest = read_manifest(archive, manifest_path)
    history = manifest['application_migrations']
    require(history == {p.stem: digest(p) for p in predecessor},
            'predecessor fixture history does not match immutable source prefix')
    psql('DROP DATABASE opensite WITH (FORCE);', 'postgres')
    # Deliberate local fault injection reproduces the reviewed bug: disabling
    # both guards restores the older DB, commits a new migration, then fails.
    with patch('ops_backup.require_matching_application_history', return_value=None):
        try:
            restore(archive, manifest_path)
        except OpsError as error:
            require('all-table manifest mismatch' in str(error),
                    'missing history guards failed for an unrelated reason')
        else:
            raise OpsError('missing history guards did not expose the unsafe upgrade')
    require(psql('SELECT count(*) FROM public.schema_migrations;').strip() == str(len(files)),
            'fault probe did not commit the unwanted forward migration')
    psql('DROP DATABASE opensite WITH (FORCE);', 'postgres')
    try:
        restore(archive, manifest_path)
    except OpsError as error:
        require('application migration history mismatch' in str(error),
                'older archive refused for an unrelated reason')
    else:
        raise OpsError('older archive accepted by the current source version')
    require(psql("SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname='opensite');",
                 'postgres').strip() == 'f', 'version refusal created a target database')
    print('PASS: older archive with history guards disabled RED (unwanted upgrade); '
          'restored guards GREEN (target remains absent)', flush=True)
    return {'backup_last_migration': max(history), 'current_last_migration': files[-1].stem,
            'archive_sha256': manifest['archive_sha256'],
            'missing_guards': 'RED: forward migration committed before content mismatch',
            'fixed_guards': 'GREEN: version mismatch before target database creation'}


def drill(receipt_dir):
    receipt_dir.mkdir(parents=True, exist_ok=True)
    started = time.monotonic()
    project = 'opensite-drill-' + secrets.token_hex(6)
    print('DRILL_PROJECT: '+project, flush=True)  # identifies only this owned stack on interruption
    work = ROOT / 'work' / 'C23a'
    work.mkdir(parents=True, exist_ok=True)
    # No inherited target: each invocation owns a new project and private secrets.
    with tempfile.TemporaryDirectory(prefix='drill-', dir=work) as temp:
        env_file = Path(temp) / '.env'
        variables = {'POSTGRES_PASSWORD': secrets.token_urlsafe(32), 'OPENSITE_DB':'opensite',
                     'OPENSITE_API_PORT':'0', 'OPENSITE_WORKER_PORT':'0', 'OPENSITE_MAIL_PORT':'0', 'COMPOSE_PROJECT_NAME':project,
                     'JOB_SIGNING_KEY':secrets.token_hex(32),'JOB_TENANTS':'',
                     'OPENSITE_ADMIN_ORIGIN':'https://admin.drill.test','OPENSITE_AUTH_SIGNING_KEY':secrets.token_hex(32),
                     'OPENSITE_STOREFRONT_ORIGIN':'https://store.drill.test',
                     'OPENSITE_CUSTOMER_CSRF_SECRET':secrets.token_urlsafe(32),
                     **{f'OPENSITE_{kind}_DB_PASSWORD':secrets.token_urlsafe(32) for kind in LOGIN_ROLES}}
        env_file.write_text(''.join(f'{key}={value}\n' for key,value in variables.items()))
        env_file.chmod(0o600)
        saved = dict(os.environ)
        os.environ.update(variables, OPENSITE_ENV_FILE=str(env_file), OPENSITE_OPS_TIMEOUT='900')
        steps = []
        try:
            url = start_api()
            require(psql('SHOW server_version_num;').strip() == '180006', 'PG18.6 required')
            permissions()
            print('PASS: compose PG18.6 → preflight/River/app migrations → restricted LOGINs → API/worker health → staff login challenge', flush=True)
            steps.append('compose-and-login-boundaries')
            connection_boundaries()
            steps.append('eight-logins-hba-tcp4-tcp6-unix-target-only-and-password-rejection')
            tainted_login_refusal()
            steps.append('direct-login-credential-acl-prewrite-refusal-and-clean-rerun')
            tainted_login_refusal(policy=True)
            steps.append('direct-login-permissive-rls-policy-prewrite-refusal-and-clean-rerun')
            psql('CREATE DATABASE ops_second;', 'postgres')
            psql('CREATE SCHEMA app; CREATE TABLE public.schema_migrations(version text);', 'ops_second')
            result = subprocess.run(compose_command(['run','--rm','--no-deps','migrate','preflight']), cwd=ROOT, capture_output=True, timeout=60)
            require(result.returncode != 0 and b'SECOND_APPLICATION_DATABASE' in result.stderr, 'second app database not refused')
            psql('ALTER DATABASE ops_second ALLOW_CONNECTIONS false;', 'postgres')
            result = subprocess.run(compose_command(['run', '--rm', '--no-deps', 'migrate', 'preflight']),
                                    cwd=ROOT, capture_output=True, timeout=60)
            require(result.returncode != 0 and b'DATABASE_OPERATION_FAILED' in result.stderr,
                    'connections-disabled database was silently skipped')
            psql('ALTER DATABASE ops_second ALLOW_CONNECTIONS true;', 'postgres')
            psql('DROP DATABASE ops_second;', 'postgres')
            preflight('opensite', fetch=psql)
            print('PASS: second application database RED; removed owned fixture → GREEN', flush=True)
            steps.append('second-app-and-connections-disabled-database-red-green')
            blob, blob_hash, tenant, job_id = seed()
            require(job_id.isdigit(), 'missing persistent River witness')
            worker_metrics()
            with urllib.request.urlopen(url+'/metrics', timeout=3) as response:
                metrics = response.read().decode()
            require('opensite_http_requests_total' in metrics and 'opensite_http_request_duration_seconds' in metrics, 'missing HTTP metrics')
            buyer = customer_witness(url, tenant)
            steps.append('real-customer-registration-local-mail-proof-login-owned-cart')
            # Quiesce both writers before the shared DB/blob snapshot.
            compose(['stop','api','worker'])
            archive, manifest_path = backup(Path(temp)/'backups')
            manifest = read_manifest(archive, manifest_path)
            require(manifest['tables']['app.products']['rows'] == 1, 'missing positive product witness')
            require(manifest['tables']['app.media_blobs']['rows'] == 1, 'missing positive blob witness')
            require(manifest['tables']['queue.river_job']['rows'] >= 1, 'missing positive River job witness')
            require(manifest['tables']['queue.river_migration']['rows'] > 0, 'missing River history witness')
            for table in ('app.customers', 'app.customer_credentials', 'app.customer_auth_tokens', 'app.customer_sessions', 'app.carts'):
                require(manifest['tables'][table]['rows'] >= 1, 'missing positive customer recovery witness')
            media_archive = Path(temp)/'media.tar'
            with media_archive.open('wb') as output:
                compose(['exec','-T','postgres','tar','-C','/media','-cf','-','.'], stdout=output)
            media_hash = digest(media_archive)
            # A corrupted archive must fail before any restore database writes.
            corrupted = archive.with_name('corrupted.dump')
            shutil.copyfile(archive, corrupted)
            with corrupted.open('ab') as output:
                output.write(b'fault')
            corrupted_manifest = manifest_path.with_name('corrupted.dump.manifest.json')
            faulty = dict(manifest, archive=corrupted.name)
            corrupted_manifest.write_text(json.dumps(faulty))
            try:
                restore(corrupted, corrupted_manifest)
            except OpsError as error:
                require('digest mismatch' in str(error), 'corrupt restore failed for unrelated reason')
            else: raise OpsError('corrupt archive accepted')
            try: restore(archive, manifest_path)
            except OpsError as error:
                require('fresh empty' in str(error), 'nonempty restore failed for unrelated reason')
            else: raise OpsError('nonempty restore accepted')
            steps.append('corrupt-and-nonempty-restore-red')
            print('PASS: corrupted archive and nonempty target rejected', flush=True)
            print('ROLLBACK_POINT: verified private dump+manifest+blob archive before destroying owned volumes', flush=True)
            # Authorized local synthetic-data loss only: this exact random project.
            compose(['down','--volumes','--remove-orphans'])
            compose(['up','-d','--wait','--wait-timeout','120','postgres'])
            # Bootstrap cluster-wide capability roles; discard only this empty DB.
            compose(['run','--rm','migrate','install'])
            require(psql('SELECT count(*) FROM app.products;').strip() == '0', 'fresh cluster retained products')
            psql('DROP DATABASE opensite WITH (FORCE);', 'postgres')
            version_refusal = previous_version_refusal(Path(temp))
            steps.append('predecessor-archive-history-guards-red-green-before-target-creation')
            tainted_login_refusal(archive, manifest_path)
            steps.append('login-owned-object-prewrite-restore-refusal-with-target-absent')
            restored = restore(archive, manifest_path)
            require(digest(media_archive) == media_hash, 'blob archive digest mismatch')
            with media_archive.open('rb') as stream:
                compose(['exec','-T','postgres','tar','-C','/media','-xf','-'], stdin=stream)
            compose(['exec','-T','postgres','chown','-R','65532:65532','/media'])
            recovered_blob = compose(['exec','-T','postgres','cat','/media/ops-witness.bin']).stdout
            require(recovered_blob == blob and hashlib.sha256(recovered_blob).hexdigest() == blob_hash, 'restored blob mismatch')
            compare_tables(manifest['tables'], table_manifest())
            require(psql(f'SELECT count(*) FROM queue.river_job WHERE id={job_id};').strip()=='1', 'River witness not restored')
            # Login password rotation is reapplied only AFTER successful restore.
            url = start_api()
            permissions()
            worker_metrics()
            processed_witness(tenant)
            restored_customer_witness(url, buyer)
            steps.append('restored-customer-proof-session-and-cart-ownership')
            steps.append('volume-loss-restore-all-tables-and-blob-api-worker-health-and-signed-job')
            receipt = {'head':subprocess.check_output(['git','rev-parse','HEAD'],cwd=ROOT,text=True).strip(),
                       'project':project, 'pg':'18.6', 'tables_verified':len(restored['tables']),
                       'archive_sha256':manifest['archive_sha256'], 'media_archive_sha256':media_hash,
                       'blob_sha256':blob_hash, 'tables':restored['tables'], 'steps':steps,
                       'restore_version_gate':version_refusal,
                       'seconds':round(time.monotonic()-started,2), 'exit_code':0,
                       'worker':'PASS: River restart and signed expiry job completed',
                       'outbox_metrics':'PASS: pending/DLQ aggregate witnesses',
                       'customer_auth':'PASS: local SMTP proof, independent login capability, session and customer cart restored',
                       'customer_order':'NOT_RUN: C-09d merge is required before adding the real customer order association drill',
                       'external_outbox_delivery':'NOT_RUN: no external Sender in local stack',
                       'storefront':'NOT_RUN: C09a pending'}
            (receipt_dir/'drill.json').write_text(json.dumps(receipt,indent=2,sort_keys=True)+'\n')
            print(f"PASS: restored {len(restored['tables'])} table counts/hashes + blob bytes; migration checksums + API health", flush=True)
            return receipt
        finally:
            try:
                compose(['down','--volumes','--remove-orphans'])
                print('PASS: owned disposable compose resources removed', flush=True)
            finally:
                os.environ.clear(); os.environ.update(saved)


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--receipt-dir',type=Path,default=ROOT/'work/C23a/evidence')
    args=parser.parse_args()
    try: drill(args.receipt_dir)
    except (OpsError, DeployError) as error:
        print('FAIL: local ops drill: ' + str(error),file=sys.stderr)
        return 1
    except (OSError, subprocess.SubprocessError):
        print('FAIL: local ops drill system operation (client diagnostics suppressed)',file=sys.stderr)
        return 1
    return 0

if __name__=='__main__':sys.exit(main())
