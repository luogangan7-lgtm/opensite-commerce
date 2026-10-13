#!/usr/bin/env python3
"""Offline test for migrations/pending/ (mock psql; NO SQL executed).

Covers: two pending files from different branches both apply after the landed
history; depends-on ordering; unknown dep / cycle → red before any SQL; pending
present without the dev/CI flag → refused before connecting; numbered
contiguity still enforced; land rewrites registry/sqlc references exactly once.
"""
from pathlib import Path
import os, shutil, subprocess, tempfile

SOURCE = Path(__file__).resolve().parents[1]
FAKE_PSQL = r'''#!/usr/bin/env python3
import os, sys
from pathlib import Path
payload = sys.stdin.read()
variables = dict(a.split('=', 1) for a in sys.argv[1:] if '=' in a)
if 'SHOW server_version_num' in payload:
    print('180002')
else:
    with Path(os.environ['FAKE_PSQL_LOG']).open('a') as log:
        if 'm_include' in variables:
            log.write('INCLUDE=' + Path(variables['m_include']).name + ' ' + Path(variables['m_include']).read_text().splitlines()[-1] + '\n')
'''


def repo(temp, pending, numbered=3):
    root = Path(temp) / 'repo with spaces'
    (root / 'scripts').mkdir(parents=True)
    (root / 'migrations' / 'pending').mkdir(parents=True)
    (root / 'modules').mkdir()
    for name in ('migrate.sh', 'pending_migrations.py', 'land_migration.sh'):
        shutil.copy2(SOURCE / 'scripts' / name, root / 'scripts' / name)
    for n in range(1, numbered + 1):
        (root / 'migrations' / f'{n:03d}_landed{n}.sql').write_text(f'SELECT {n};\n')
    for name, body in pending.items():
        (root / 'migrations' / 'pending' / name).write_text(body)
    refs = ''.join(f'  - "migrations/pending/{name}"\n' for name in pending)
    (root / 'sqlc.yaml').write_text('schema:\n' + refs)
    (root / 'modules' / 'registry.yaml').write_text(refs.replace('  - "', '  {"file": "').replace('"\n', '"}\n'))
    return root


def run(root, *args, allow=True):
    bindir = root.parent / 'bin'
    bindir.mkdir(exist_ok=True)
    (bindir / 'psql').write_text(FAKE_PSQL)
    (bindir / 'psql').chmod(0o755)
    log = root.parent / 'psql.log'
    env = dict(os.environ, DATABASE_URL='postgres://fake/opensite', FAKE_PSQL_LOG=str(log),
               PATH=str(bindir) + os.pathsep + os.environ['PATH'])
    env.pop('OPENSITE_ALLOW_PENDING_MIGRATIONS', None)
    if allow:
        env['OPENSITE_ALLOW_PENDING_MIGRATIONS'] = '1'
    result = subprocess.run(['bash', *args], cwd=root, env=env, capture_output=True, text=True)
    return result, (log.read_text() if log.exists() else '')


def includes(log):
    return [line.split('=', 1)[1] for line in log.splitlines() if line.startswith('INCLUDE=')]


# 1. Two branches each add one pending file; both apply, contiguously, sorted by stable-id.
with tempfile.TemporaryDirectory() as temp:
    root = repo(temp, {'c15b-serving_storefront_serving.sql': '-- c15b\nSELECT 15;\n',
                       'c09a-pii_pii_registry.sql': '-- c09a\nSELECT 9;\n'})
    result, log = run(root, 'scripts/migrate.sh')
    assert result.returncode == 0, (result.stdout, result.stderr)
    assert includes(log) == ['001_landed1.sql SELECT 1;', '002_landed2.sql SELECT 2;', '003_landed3.sql SELECT 3;',
                             '004_pii_registry.sql SELECT 9;', '005_storefront_serving.sql SELECT 15;'], log
    assert sorted(p.name for p in (root / 'migrations').iterdir()) == \
        ['001_landed1.sql', '002_landed2.sql', '003_landed3.sql', 'pending'], 'migrate.sh must not touch the source tree'

# 2. depends-on overrides stable-id order.
with tempfile.TemporaryDirectory() as temp:
    root = repo(temp, {'aa_first.sql': '-- depends-on: zz\nSELECT 1;\n', 'zz_second.sql': 'SELECT 2;\n'})
    result, log = run(root, 'scripts/migrate.sh')
    assert result.returncode == 0, result.stderr
    assert [i.split()[0] for i in includes(log)][3:] == ['004_second.sql', '005_first.sql'], log

# 3. Unknown depends-on and cycles are red before any SQL is sent.
for pending, message in (({'aa_x.sql': '-- depends-on: nope\nSELECT 1;\n'}, 'unknown depends-on nope'),
                         ({'aa_x.sql': '-- depends-on: bb\n', 'bb_y.sql': '-- depends-on: aa\n'}, 'cycle'),
                         ({'aa_landed1.sql': 'SELECT 1;\n'}, 'Duplicate migration name'),
                         ({'Bad_Name.sql': 'SELECT 1;\n'}, 'Invalid pending migration')):
    with tempfile.TemporaryDirectory() as temp:
        root = repo(temp, pending)
        result, log = run(root, 'scripts/migrate.sh')
        assert result.returncode == 2 and message in result.stderr, (pending, result.stderr)
        assert includes(log) == [], 'SQL submitted before pending validation failed'

# 4. Production/drill mode (no flag) refuses while pending/ is non-empty, before connecting.
with tempfile.TemporaryDirectory() as temp:
    root = repo(temp, {'aa_x.sql': 'SELECT 1;\n'})
    result, log = run(root, 'scripts/migrate.sh', allow=False)
    assert result.returncode == 7 and 'migrations/pending' in result.stderr, result.stderr
    assert log == '', 'production mode contacted the database with pending migrations present'
    # ...and an empty pending/ needs no flag at all.
    (root / 'migrations' / 'pending' / 'aa_x.sql').unlink()
    result, log = run(root, 'scripts/migrate.sh', allow=False)
    assert result.returncode == 0 and len(includes(log)) == 3, result.stderr

# 5. Numbered contiguity is still enforced with pending files present.
with tempfile.TemporaryDirectory() as temp:
    root = repo(temp, {'aa_x.sql': 'SELECT 1;\n'})
    (root / 'migrations' / '002_landed2.sql').unlink()
    result, log = run(root, 'scripts/migrate.sh')
    assert result.returncode == 2 and 'contiguous' in result.stderr and includes(log) == [], result.stderr

# 6. Landing moves exactly the next file and rewrites registry/sqlc references; ci-normalize lands all.
with tempfile.TemporaryDirectory() as temp:
    root = repo(temp, {'c15b-serving_storefront_serving.sql': 'SELECT 15;\n', 'c09a-pii_pii_registry.sql': 'SELECT 9;\n'})
    wrong, _ = run(root, 'scripts/land_migration.sh', 'c15b-serving')
    assert wrong.returncode == 2 and 'not the next' in wrong.stderr, wrong.stderr
    landed, _ = run(root, 'scripts/land_migration.sh', 'c09a-pii')
    assert landed.returncode == 0, landed.stderr
    assert (root / 'migrations' / '004_pii_registry.sql').read_text() == 'SELECT 9;\n'
    assert not (root / 'migrations' / 'pending' / 'c09a-pii_pii_registry.sql').exists()
    for ref in ('sqlc.yaml', 'modules/registry.yaml'):
        text = (root / ref).read_text()
        assert '"migrations/004_pii_registry.sql"' in text and 'c09a-pii' not in text, text
        assert 'migrations/pending/c15b-serving_storefront_serving.sql' in text, text
    normalized = subprocess.run(['python3', 'scripts/pending_migrations.py', 'ci-normalize'], cwd=root, capture_output=True, text=True)
    assert normalized.returncode == 0, normalized.stderr
    assert (root / 'migrations' / '005_storefront_serving.sql').is_file()
    assert list((root / 'migrations' / 'pending').iterdir()) == []
    assert 'pending' not in (root / 'sqlc.yaml').read_text()
    result, log = run(root, 'scripts/migrate.sh', allow=False)
    assert result.returncode == 0 and len(includes(log)) == 5, result.stderr

print('PASS: pending migrations order/apply/land + production refusal (mock psql; NO SQL executed)')
