#!/usr/bin/env python3
"""Offline *orchestration* test; intentionally does not parse or execute SQL."""
from pathlib import Path
import os, subprocess, tempfile, shutil

root = Path(__file__).resolve().parents[1]
migrations = sorted((root/'migrations').glob('[0-9][0-9][0-9]_*.sql'))
assert len(migrations) == 9, f'expected 9 migration files, got {len(migrations)}'
with tempfile.TemporaryDirectory() as temp:
    bindir = Path(temp)
    log = bindir/'psql_calls.txt'
    psql = bindir/'psql'
    psql.write_text('''#!/usr/bin/env python3
import os, sys
from pathlib import Path
payload=sys.stdin.read()
if 'SHOW server_version_num' in payload:
    print('180002')
else:
    with Path(os.environ['FAKE_PSQL_LOG']).open('a') as log:
        log.write('=== NEXT MIGRATION ===\\n'+payload+'\\n')
''')
    psql.chmod(0o755)
    env = os.environ.copy()
    env.update(DATABASE_URL='postgres://fake/opensite',FAKE_PSQL_LOG=str(log),PATH=str(bindir)+os.pathsep+env['PATH'])
    result = subprocess.run(['bash', str(root/'scripts/migrate.sh')], cwd=root, env=env, capture_output=True, text=True)
    assert result.returncode==0,(result.stdout,result.stderr)
    content = log.read_text()
    assert content.count('=== NEXT MIGRATION ===') == len(migrations)+1
    assert content.count('pg_advisory_xact_lock') == len(migrations)+1
    assert content.count('INSERT INTO public.schema_migrations') == len(migrations)
    assert content.count('MIGRATION_CHECKSUM_DRIFT') == len(migrations)
    assert content.count('\\if :m_installed') == len(migrations)
    assert content.count('MIGRATION_OUT_OF_ORDER') == len(migrations)
    assert 'MIGRATION_MISSING_SOURCE_FILE' in content
    for path in migrations:
        assert f'\\i {path}' in content, path
    # Missing source migration numbers must fail before applying any SQL.
    broken = bindir/'broken-repo'
    (broken/'scripts').mkdir(parents=True)
    (broken/'migrations').mkdir()
    shutil.copy2(root/'scripts/migrate.sh',broken/'scripts/migrate.sh')
    for migration in migrations:
        if migration.name.startswith('005_'):
            continue
        shutil.copy2(migration,broken/'migrations'/migration.name)
    previous_size = log.stat().st_size
    missing = subprocess.run(['bash',str(broken/'scripts/migrate.sh')],env=env,capture_output=True,text=True)
    assert missing.returncode==2 and 'numbering must be contiguous' in missing.stderr, (missing.stdout,missing.stderr)
    assert log.stat().st_size == previous_size, 'unexpected migration DDL submitted before numbering failure'
    print('PASS: migration runner orchestration + missing-number fail-fast (mock psql; NO SQL executed)')
