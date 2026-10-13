#!/usr/bin/env python3
"""Local isolated compose backup/volume-loss/restore acceptance, never production.

Depends on Docker, ops_deploy and ops_backup; CI/manual operators call the shell
wrapper. Only a fresh random project is destroyed; all writes stop before dump.
"""
import argparse
import hashlib
import hmac
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.request
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


def start_api():
    compose(['up', '-d', '--build', '--wait', '--wait-timeout', '120', 'api', 'worker'])
    url = live_url()
    healthy(url)
    healthy(live_url('worker','8081'))
    return url


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
        result = compose(args, input=(env_secret+'\n'+sql).encode())
        require(login in result.stdout.decode(), 'login probe did not connect')


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
    require('opensite_outbox_pending 1' in values and 'opensite_outbox_dead_letters 1' in values
            and 'opensite_outbox_oldest_pending_age_seconds' in values, 'incorrect worker backlog/DLQ metrics')


def processed_witness(tenant):
    job_id = enqueue_witness(tenant)
    require(job_id.isdigit(), 'missing queued job witness')
    deadline = time.monotonic()+30
    while psql(f'SELECT state FROM queue.river_job WHERE id={job_id};').strip() != 'completed':
        if time.monotonic()>deadline: raise OpsError('River worker did not complete signed expiry witness')
        time.sleep(.2)  # bounded state polling; no concurrency ordering


def previous_version_refusal(directory):
    """Real predecessor archive: missing guards mutate the target; fixed guards refuse it.

    Only called after deleting this drill's fresh empty database. Build the
    complete predecessor from immutable migration files and the normal runner;
    the transient prefix runner exists only inside this owned private directory.
    """
    files = sorted((ROOT / 'migrations').glob('[0-9][0-9][0-9]_*.sql'))
    require(len(files) > 1 and files[-1].name.endswith('_ops_metrics.sql'),
            'ops metrics must be the next contiguous forward migration')
    prefix = directory / 'predecessor'
    (prefix / 'migrations').mkdir(parents=True)
    (prefix / 'scripts').mkdir()
    shutil.copy2(ROOT / 'scripts/migrate.sh', prefix / 'scripts/migrate.sh')
    for source in files[:-1]:
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
    require(history == {p.stem: digest(p) for p in files[:-1]},
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
                     'OPENSITE_API_PORT':'0', 'OPENSITE_WORKER_PORT':'0', 'COMPOSE_PROJECT_NAME':project,
                     'JOB_SIGNING_KEY':secrets.token_hex(32),'JOB_TENANTS':'',
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
            print('PASS: compose PG18.6 → preflight/River/app migrations → restricted LOGINs → API/worker health', flush=True)
            steps.append('compose-and-login-boundaries')
            psql('CREATE DATABASE ops_second;', 'postgres')
            psql('CREATE SCHEMA app; CREATE TABLE public.schema_migrations(version text);', 'ops_second')
            result = subprocess.run(compose_command(['run','--rm','--no-deps','migrate','preflight']), cwd=ROOT, capture_output=True, timeout=60)
            require(result.returncode != 0 and b'SECOND_APPLICATION_DATABASE' in result.stderr, 'second app database not refused')
            psql('DROP DATABASE ops_second;', 'postgres')
            preflight('opensite', fetch=psql)
            print('PASS: second application database RED; removed owned fixture → GREEN', flush=True)
            steps.append('second-app-database-red-green')
            blob, blob_hash, tenant, job_id = seed()
            require(job_id.isdigit(), 'missing persistent River witness')
            worker_metrics()
            with urllib.request.urlopen(url+'/metrics', timeout=3) as response:
                metrics = response.read().decode()
            require('opensite_http_requests_total' in metrics and 'opensite_http_request_duration_seconds' in metrics, 'missing HTTP metrics')
            # Quiesce both writers before the shared DB/blob snapshot.
            compose(['stop','api','worker'])
            archive, manifest_path = backup(Path(temp)/'backups')
            manifest = read_manifest(archive, manifest_path)
            require(manifest['tables']['app.products']['rows'] == 1, 'missing positive product witness')
            require(manifest['tables']['app.media_blobs']['rows'] == 1, 'missing positive blob witness')
            require(manifest['tables']['queue.river_job']['rows'] >= 1, 'missing positive River job witness')
            require(manifest['tables']['queue.river_migration']['rows'] > 0, 'missing River history witness')
            media_archive = Path(temp)/'media.tar'
            with media_archive.open('wb') as output:
                compose(['exec','-T','postgres','tar','-C','/media','-cf','-','.'], stdout=output)
            media_hash = digest(media_archive)
            # A corrupted archive must fail before any restore database writes.
            corrupted = archive.with_name('corrupted.dump')
            corrupted.write_bytes(archive.read_bytes()+b'fault')
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
            restored = restore(archive, manifest_path)
            require(digest(media_archive) == media_hash, 'blob archive digest mismatch')
            compose(['exec','-T','postgres','tar','-C','/media','-xf','-'], input=media_archive.read_bytes())
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
            steps.append('volume-loss-restore-all-tables-and-blob-api-worker-health-and-signed-job')
            receipt = {'head':subprocess.check_output(['git','rev-parse','HEAD'],cwd=ROOT,text=True).strip(),
                       'project':project, 'pg':'18.6', 'tables_verified':len(restored['tables']),
                       'archive_sha256':manifest['archive_sha256'], 'media_archive_sha256':media_hash,
                       'blob_sha256':blob_hash, 'tables':restored['tables'], 'steps':steps,
                       'restore_version_gate':version_refusal,
                       'seconds':round(time.monotonic()-started,2), 'exit_code':0,
                       'worker':'PASS: River restart and signed expiry job completed',
                       'outbox_metrics':'PASS: pending/DLQ aggregate witnesses',
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
