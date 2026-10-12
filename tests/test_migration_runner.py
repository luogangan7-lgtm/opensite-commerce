#!/usr/bin/env python3
"""Offline *orchestration* test; intentionally does not parse or execute SQL."""
from pathlib import Path
import os, subprocess, tempfile, shutil

source_root = Path(__file__).resolve().parents[1]
migrations = sorted((source_root/'migrations').glob('[0-9][0-9][0-9]_*.sql'))
assert len(migrations) == 9, f'expected 9 migration files, got {len(migrations)}'
with tempfile.TemporaryDirectory() as temp:
    bindir = Path(temp)
    root = bindir/'repository with spaces'
    (root/'scripts').mkdir(parents=True)
    (root/'migrations').mkdir()
    shutil.copy2(source_root/'scripts/migrate.sh', root/'scripts/migrate.sh')
    for migration in migrations:
        shutil.copy2(migration, root/'migrations'/migration.name)
    migrations = sorted((root/'migrations').glob('*.sql'))
    log = bindir/'psql_calls.txt'
    psql = bindir/'psql'
    psql.write_text(r'''#!/usr/bin/env python3
import os, sys, shlex
from pathlib import Path
payload=sys.stdin.read()
variables = dict(arg.split('=', 1) for arg in sys.argv[1:] if '=' in arg)
if 'SHOW server_version_num' in payload:
    print('180002')
else:
    includes = []
    for line in payload.splitlines():
        if line.strip().startswith('\\i '):
            if line.strip() == "\\i :m_include":
                path = variables['m_include']
            else:
                args = shlex.split(line.strip())
                if len(args) != 2:
                    sys.exit('unquoted migration path with spaces')
                path = args[1]
            if not Path(path).is_file():
                sys.exit('migration include file missing')
            includes.append(path)
    with Path(os.environ['FAKE_PSQL_LOG']).open('a') as log:
        log.write('=== NEXT MIGRATION ===\n'+payload+'\n')
        for path in includes:
            log.write('INCLUDE='+path+'\n')
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
        assert f'INCLUDE={path}' in content, path
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
