#!/usr/bin/env python3
"""Local isolated compose backup/volume-loss/restore acceptance, never production.

Depends on Docker, ops_deploy and ops_backup; CI/manual operators call the shell
wrapper. Only a fresh random project is destroyed; all writes stop before dump.
"""
import argparse
import hashlib
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

from ops_common import ROOT, OpsError, compose, compose_command, pg_command, psql, quote_ident
from ops_deploy import DeployError, preflight
from ops_backup import backup, compare_tables, digest, read_manifest, restore, table_manifest


def require(value, message):
    if not value: raise OpsError(message)


def live_url():
    binding = compose(['port', 'api', '8080']).stdout.decode().strip()
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
    compose(['up', '-d', '--build', '--wait', '--wait-timeout', '120', 'api'])
    url = live_url()
    healthy(url)
    return url


def permissions():
    # Check effective memberships and database ACL, independent of provisioning SQL.
    require(psql("SELECT NOT EXISTS(SELECT 1 FROM aclexplode((SELECT datacl FROM pg_database WHERE datname=current_database())) WHERE grantee=0 AND privilege_type='CONNECT');").strip() == 't', 'PUBLIC still has CONNECT')
    for kind, group in [('RUNTIME','app_runtime'), ('AUTH','app_auth'), ('DOMAIN','app_domain_resolver'), ('MEDIA','app_media_service')]:
        login = 'opensite_' + kind.lower() + '_login'
        require(psql(f"SELECT rolinherit AND rolcanlogin AND NOT (rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolreplication) FROM pg_roles WHERE rolname='{login}';").strip() == 't', 'unsafe LOGIN attributes')
        require(psql(f"SELECT string_agg(r.rolname,',' ORDER BY r.rolname) FROM pg_auth_members m JOIN pg_roles r ON r.oid=m.roleid JOIN pg_roles u ON u.oid=m.member WHERE u.rolname='{login}';").strip() == group, 'unexpected LOGIN capability')
        # Real password-authenticated connection, including restricted DDL/read probes.
        env_secret = os.environ[f'OPENSITE_{kind}_DB_PASSWORD']
        args = ['exec','-T','postgres','sh','-c','IFS= read -r PGPASSWORD; export PGPASSWORD; exec psql -X -qAt -h127.0.0.1 -U "$1" -d opensite -v ON_ERROR_STOP=1','ops-login',login]
        sql = "SELECT current_user;\nDO $$ BEGIN BEGIN CREATE TABLE public.ops_forbidden(id int); RAISE EXCEPTION 'DDL_ALLOWED'; EXCEPTION WHEN insufficient_privilege THEN NULL; END; END $$;\n"
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
COMMIT;""")
    compose(['exec','-T','postgres','sh','-c','cat > /media/ops-witness.bin; chown 65532:65532 /media/ops-witness.bin'], input=blob)
    return blob, content_hash


def drill(receipt_dir):
    receipt_dir.mkdir(parents=True, exist_ok=True)
    started = time.monotonic()
    project = 'opensite-drill-' + secrets.token_hex(6)
    work = ROOT / 'work' / 'C23a'
    work.mkdir(parents=True, exist_ok=True)
    # No inherited target: each invocation owns a new project and private secrets.
    with tempfile.TemporaryDirectory(prefix='drill-', dir=work) as temp:
        env_file = Path(temp) / '.env'
        variables = {'POSTGRES_PASSWORD': secrets.token_urlsafe(32), 'OPENSITE_DB':'opensite',
                     'OPENSITE_API_PORT':'0', 'COMPOSE_PROJECT_NAME':project,
                     **{f'OPENSITE_{kind}_DB_PASSWORD':secrets.token_urlsafe(32) for kind in ('RUNTIME','AUTH','DOMAIN','MEDIA')}}
        env_file.write_text(''.join(f'{key}={value}\n' for key,value in variables.items()))
        env_file.chmod(0o600)
        saved = dict(os.environ)
        os.environ.update(variables, OPENSITE_ENV_FILE=str(env_file), OPENSITE_OPS_TIMEOUT='900')
        steps = []
        try:
            url = start_api()
            require(psql('SHOW server_version_num;').strip() == '180006', 'PG18.6 required')
            permissions()
            print('PASS: compose PG18.6 → preflight/migrations → restricted LOGINs → API health', flush=True)
            steps.append('compose-and-login-boundaries')
            psql('CREATE DATABASE ops_second;', 'postgres')
            psql('CREATE SCHEMA app; CREATE TABLE public.schema_migrations(version text);', 'ops_second')
            result = subprocess.run(compose_command(['run','--rm','--no-deps','migrate','preflight']), cwd=ROOT, capture_output=True, timeout=60)
            require(result.returncode != 0 and b'SECOND_APPLICATION_DATABASE' in result.stderr, 'second app database not refused')
            psql('DROP DATABASE ops_second;', 'postgres')
            preflight('opensite', fetch=psql)
            print('PASS: second application database RED; removed owned fixture → GREEN', flush=True)
            steps.append('second-app-database-red-green')
            blob, blob_hash = seed()
            with urllib.request.urlopen(url+'/metrics', timeout=3) as response:
                metrics = response.read().decode()
            require('opensite_http_requests_total' in metrics and 'opensite_http_request_duration_seconds' in metrics, 'missing HTTP metrics')
            # Quiesced API and absent worker give one DB/blob cut; no live uploads.
            compose(['stop','api'])
            archive, manifest_path = backup(Path(temp)/'backups')
            manifest = read_manifest(archive, manifest_path)
            require(manifest['tables']['app.products']['rows'] == 1, 'missing positive product witness')
            require(manifest['tables']['app.media_blobs']['rows'] == 1, 'missing positive blob witness')
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
            restored = restore(archive, manifest_path)
            require(digest(media_archive) == media_hash, 'blob archive digest mismatch')
            compose(['exec','-T','postgres','tar','-C','/media','-xf','-'], input=media_archive.read_bytes())
            compose(['exec','-T','postgres','chown','-R','65532:65532','/media'])
            recovered_blob = compose(['exec','-T','postgres','cat','/media/ops-witness.bin']).stdout
            require(recovered_blob == blob and hashlib.sha256(recovered_blob).hexdigest() == blob_hash, 'restored blob mismatch')
            compare_tables(manifest['tables'], table_manifest())
            # Login password rotation is reapplied only AFTER successful restore.
            url = start_api()
            permissions()
            steps.append('volume-loss-restore-all-tables-and-blob-api-health')
            receipt = {'head':subprocess.check_output(['git','rev-parse','HEAD'],cwd=ROOT,text=True).strip(),
                       'project':project, 'pg':'18.6', 'tables_verified':len(restored['tables']),
                       'archive_sha256':manifest['archive_sha256'], 'media_archive_sha256':media_hash,
                       'blob_sha256':blob_hash, 'tables':restored['tables'], 'steps':steps,
                       'seconds':round(time.monotonic()-started,2), 'exit_code':0,
                       'worker_outbox':'NOT_RUN: PR25 pending', 'storefront':'NOT_RUN: C09a pending'}
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
