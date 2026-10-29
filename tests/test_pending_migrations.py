#!/usr/bin/env python3
"""Offline test for migrations/pending/ (stateful mock psql; NO SQL executed).

Covers: two pending files from different branches both apply after the landed
history; depends-on ordering, also across landing; unknown dep / cycle / missing
stable-id header → red before any SQL or write; pending present without the
dev/CI flag → refused before ANY psql call; numbered contiguity still enforced;
land is a byte-identical move (so a dev DB that applied the pending file sees it
as already installed) and rewrites registry/sqlc references.
"""
from pathlib import Path
import os, shutil, subprocess, tempfile

SOURCE = Path(__file__).resolve().parents[1]
# Every invocation is logged before branching; schema_migrations is persisted in a JSON state file.
FAKE_PSQL = r'''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
payload = sys.stdin.read()
v = dict(a.split('=', 1) for a in sys.argv[1:] if '=' in a)
log, state_file = Path(os.environ['FAKE_PSQL_LOG']), Path(os.environ['FAKE_PSQL_STATE'])
def emit(line):
    with log.open('a') as out:
        out.write(line + '\n')
emit('CALL')
if 'SHOW server_version_num' in payload:
    print('180002')
    sys.exit(0)
state = json.loads(state_file.read_text()) if state_file.exists() else {}
known = v.get('m_known_versions', '').split(',')
if any(version not in known for version in state):
    emit('MISSING_SOURCE_FILE')
    sys.exit(3)
if 'm_version' in v:
    if '\\i :m_include' not in payload:
        emit('NO_INCLUDE_COMMAND')
        sys.exit(3)
    if v['m_version'] in state:
        if state[v['m_version']] != v['m_checksum']:
            emit('CHECKSUM_DRIFT ' + v['m_version'])
            sys.exit(3)
        emit('ALREADY=' + v['m_version'])
    else:
        path = Path(v['m_include'])
        state[v['m_version']] = v['m_checksum']
        state_file.write_text(json.dumps(state))
        emit('INCLUDE=' + path.name + ' ' + path.read_text().splitlines()[-1])
'''


def repo(temp, pending, numbered=3, header=True):
    """Pending bodies get the required `-- stable-id: <id>` first line unless header=False."""
    root = Path(temp) / 'repo with spaces'
    (root / 'scripts').mkdir(parents=True)
    (root / 'migrations' / 'pending').mkdir(parents=True)
    (root / 'modules').mkdir()
    for name in ('migrate.sh', 'pending_migrations.py', 'land_migration.sh'):
        shutil.copy2(SOURCE / 'scripts' / name, root / 'scripts' / name)
    for n in range(1, numbered + 1):
        (root / 'migrations' / f'{n:03d}_landed{n}.sql').write_text(f'SELECT {n};\n')
    for name, body in pending.items():
        prefix = f'-- stable-id: {name.split("_")[0]}\n' if header else ''
        (root / 'migrations' / 'pending' / name).write_text(prefix + body)
    refs = ''.join(f'  - "migrations/pending/{name}"\n' for name in pending)
    (root / 'sqlc.yaml').write_text('schema:\n' + refs)
    (root / 'modules' / 'registry.yaml').write_text(refs.replace('  - "', '  {"file": "').replace('"\n', '"}\n'))
    return root


def run(root, *args, allow=True):
    """Runs a script; returns (result, psql log of THIS run). Fake DB history persists per temp dir."""
    bindir = root.parent / 'bin'
    bindir.mkdir(exist_ok=True)
    (bindir / 'psql').write_text(FAKE_PSQL)
    (bindir / 'psql').chmod(0o755)
    log = root.parent / 'psql.log'
    log.unlink(missing_ok=True)
    env = dict(os.environ, DATABASE_URL='postgres://fake/opensite', FAKE_PSQL_LOG=str(log),
               FAKE_PSQL_STATE=str(root.parent / 'psql.state'), PATH=str(bindir) + os.pathsep + os.environ['PATH'])
    env.pop('OPENSITE_ALLOW_PENDING_MIGRATIONS', None)
    if allow:
        env['OPENSITE_ALLOW_PENDING_MIGRATIONS'] = '1'
    result = subprocess.run(['bash', *args], cwd=root, env=env, capture_output=True, text=True)
    return result, (log.read_text() if log.exists() else '')


def includes(log):
    return [line.split('=', 1)[1] for line in log.splitlines() if line.startswith('INCLUDE=')]


def tree(root):
    return {str(p.relative_to(root)): p.read_bytes() for p in sorted(root.rglob('*')) if p.is_file()}


def refs_point_to(root, expected):
    for ref in ('sqlc.yaml', 'modules/registry.yaml'):
        text = (root / ref).read_text()
        assert 'pending' not in text, (ref, text)
        assert all(f'"migrations/{name}"' in text for name in expected), (ref, text)



# 1. Two branches each add one pending file; both apply, contiguously, sorted by stable-id.
with tempfile.TemporaryDirectory() as temp:
    root = repo(temp, {'c15b-serving_storefront_serving.sql': '-- c15b\nSELECT 15;\n',
                       'c09a-pii_pii_registry.sql': '-- c09a\nSELECT 9;\n'})
    before = tree(root)
    result, log = run(root, 'scripts/migrate.sh')
    assert result.returncode == 0, (result.stdout, result.stderr)
    assert includes(log) == ['001_landed1.sql SELECT 1;', '002_landed2.sql SELECT 2;', '003_landed3.sql SELECT 3;',
                             '004_pii_registry.sql SELECT 9;', '005_storefront_serving.sql SELECT 15;'], log
    assert tree(root) == before, 'migrate.sh must not touch the source tree'

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
                         ({'Bad_Name.sql': 'SELECT 1;\n'}, 'Invalid pending migration'),
                         ({'aa_x.sql': '-- stable-id: bb\nSELECT 1;\n'}, 'stable-id'),
                         ({'aa_x.sql': 'SELECT 1;\n', 'header': False}, 'stable-id')):
    with tempfile.TemporaryDirectory() as temp:
        header = pending.pop('header', True)
        root = repo(temp, pending, header=header and not any(b.startswith('-- stable-id') for b in pending.values()))
        result, log = run(root, 'scripts/migrate.sh')
        assert result.returncode == 2 and message in result.stderr, (pending, result.stderr)
        assert log == '', 'psql invoked before pending validation failed'

# 4. Production/drill mode (no flag) refuses while pending/ is non-empty, before connecting.
with tempfile.TemporaryDirectory() as temp:
    root = repo(temp, {'aa_x.sql': 'SELECT 1;\n'})
    result, log = run(root, 'scripts/migrate.sh', allow=False)
    assert result.returncode == 7 and 'migrations/pending' in result.stderr, result.stderr
    assert log == '', 'production mode invoked psql with pending migrations present'
    # ...and an empty pending/ needs no flag at all.
    (root / 'migrations' / 'pending' / 'aa_x.sql').unlink()
    result, log = run(root, 'scripts/migrate.sh', allow=False)
    assert result.returncode == 0 and len(includes(log)) == 3, result.stderr

# 5. Numbered contiguity is still enforced with pending files present.
with tempfile.TemporaryDirectory() as temp:
    root = repo(temp, {'aa_x.sql': 'SELECT 1;\n'})
    (root / 'migrations' / '002_landed2.sql').unlink()
    result, log = run(root, 'scripts/migrate.sh')
    assert result.returncode == 2 and 'contiguous' in result.stderr and log == '', result.stderr

# 6. Landing moves exactly the next file and rewrites registry/sqlc references; ci-normalize lands all.
with tempfile.TemporaryDirectory() as temp:
    root = repo(temp, {'c15b-serving_storefront_serving.sql': 'SELECT 15;\n', 'c09a-pii_pii_registry.sql': 'SELECT 9;\n'})
    wrong, _ = run(root, 'scripts/land_migration.sh', 'c15b-serving')
    assert wrong.returncode == 2 and 'not the next' in wrong.stderr, wrong.stderr
    source_bytes = (root / 'migrations' / 'pending' / 'c09a-pii_pii_registry.sql').read_bytes()
    landed, _ = run(root, 'scripts/land_migration.sh', 'c09a-pii')
    assert landed.returncode == 0, landed.stderr
    # Byte-identical move: same checksum before and after landing.
    assert (root / 'migrations' / '004_pii_registry.sql').read_bytes() == source_bytes
    assert not (root / 'migrations' / 'pending' / 'c09a-pii_pii_registry.sql').exists()
    for ref in ('sqlc.yaml', 'modules/registry.yaml'):
        text = (root / ref).read_text()
        assert '"migrations/004_pii_registry.sql"' in text and 'c09a-pii' not in text, text
        assert 'migrations/pending/c15b-serving_storefront_serving.sql' in text, text
    normalized = subprocess.run(['python3', 'scripts/pending_migrations.py', 'ci-normalize'], cwd=root, capture_output=True, text=True)
    assert normalized.returncode == 0, normalized.stderr
    assert (root / 'migrations' / '005_storefront_serving.sql').is_file()
    assert list((root / 'migrations' / 'pending').iterdir()) == []
    refs_point_to(root, ['004_pii_registry.sql', '005_storefront_serving.sql'])
    result, log = run(root, 'scripts/migrate.sh', allow=False)
    assert result.returncode == 0 and len(includes(log)) == 5, result.stderr

# 6b. A dev DB that applied a pending file sees it as already installed after landing (persisted history).
with tempfile.TemporaryDirectory() as temp:
    root = repo(temp, {'c09a-pii_pii_registry.sql': 'SELECT 9;\n'})
    first, log = run(root, 'scripts/migrate.sh')
    assert first.returncode == 0 and includes(log)[-1] == '004_pii_registry.sql SELECT 9;', (first.stderr, log)
    landed, _ = run(root, 'scripts/land_migration.sh', 'c09a-pii')
    assert landed.returncode == 0, landed.stderr
    second, log = run(root, 'scripts/migrate.sh', allow=False)
    assert second.returncode == 0 and includes(log) == [] and 'ALREADY=004_pii_registry' in log, (second.stderr, log)


# 7. depends-on survives landing: reverse-lexicographic dep via ci-normalize (and a stable re-run) ...
with tempfile.TemporaryDirectory() as temp:
    root = repo(temp, {'aa_first.sql': '-- depends-on: zz\nSELECT 1;\n', 'zz_second.sql': 'SELECT 2;\n'})
    normalized = subprocess.run(['python3', 'scripts/pending_migrations.py', 'ci-normalize'], cwd=root, capture_output=True, text=True)
    assert normalized.returncode == 0, normalized.stderr
    assert (root / 'migrations' / '004_second.sql').read_text().endswith('SELECT 2;\n')
    assert (root / 'migrations' / '005_first.sql').read_text().endswith('SELECT 1;\n')
    refs_point_to(root, ['004_second.sql', '005_first.sql'])
    before = tree(root)
    again = subprocess.run(['python3', 'scripts/pending_migrations.py', 'ci-normalize'], cwd=root, capture_output=True, text=True)
    assert again.returncode == 0 and tree(root) == before, again.stderr
    result, log = run(root, 'scripts/migrate.sh', allow=False)
    assert result.returncode == 0 and len(includes(log)) == 5, result.stderr

# ... and a three-node chain landed one PR at a time with consecutive land calls.
with tempfile.TemporaryDirectory() as temp:
    root = repo(temp, {'aa_one.sql': '-- depends-on: mm\nSELECT 1;\n', 'mm_two.sql': '-- depends-on: zz\nSELECT 2;\n',
                       'zz_three.sql': 'SELECT 3;\n'})
    for stable_id in ('zz', 'mm', 'aa'):
        landed, _ = run(root, 'scripts/land_migration.sh', stable_id)
        assert landed.returncode == 0, (stable_id, landed.stderr)
    assert [p.name for p in sorted((root / 'migrations').glob('0*.sql'))][3:] == ['004_three.sql', '005_two.sql', '006_one.sql']
    refs_point_to(root, ['004_three.sql', '005_two.sql', '006_one.sql'])

# Unknown deps and cycles still fail before ANY write, even next to valid pending files.
for pending in ({'aa_ok.sql': 'SELECT 1;\n', 'bb_bad.sql': '-- depends-on: nope\n'},
                {'aa_ok.sql': 'SELECT 1;\n', 'bb_x.sql': '-- depends-on: cc\n', 'cc_y.sql': '-- depends-on: bb\n'}):
    with tempfile.TemporaryDirectory() as temp:
        root = repo(temp, pending)
        before = tree(root)
        for args in (['python3', 'scripts/pending_migrations.py', 'ci-normalize'], ['bash', 'scripts/land_migration.sh', 'aa']):
            failed = subprocess.run(args, cwd=root, capture_output=True, text=True)
            assert failed.returncode == 2 and tree(root) == before, (pending, args, failed.stderr)

# Malformed dependency declarations must fail before SQL or filesystem writes.
for declaration in ('aa, zz', 'aa,zz', 'aa trailing-junk', '', 'nope'):
    with tempfile.TemporaryDirectory() as temp:
        root = repo(temp, {'aa_ok.sql': 'SELECT 1;\n', 'zz_ok.sql': 'SELECT 2;\n',
                           'bb_bad.sql': '-- depends-on: ' + declaration + '\nSELECT 3;\n'})
        before = tree(root)
        for args in (['python3', 'scripts/pending_migrations.py', 'ci-normalize'],
                     ['bash', 'scripts/land_migration.sh', 'aa']):
            failed = subprocess.run(args, cwd=root, capture_output=True, text=True)
            assert failed.returncode == 2 and tree(root) == before, (
                'DEPENDENCY_DECLARATION_MUST_FAIL_CLOSED', declaration, args, failed.stderr)
        failed, log = run(root, 'scripts/migrate.sh')
        assert failed.returncode != 0 and not log and tree(root) == before, (
            'DEPENDENCY_DECLARATION_REACHED_SQL', declaration, failed.stderr)

print('PASS: pending migrations order/apply/land + production refusal (mock psql; NO SQL executed)')
