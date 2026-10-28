#!/usr/bin/env python3
"""Generated code is never hand-merged (INF-GEN), offline: no gh/network/generators run.

Checks the read-only CI `generated-patch` job contract, and that
scripts/apply_generated_patch.sh applies a downloaded patch exactly while
refusing any patch that touches a non-generated path.
"""
from pathlib import Path
import json, os, shutil, subprocess, tempfile
import yaml

ROOT = Path(__file__).resolve().parents[1]
GENERATED = ('internal/platform/httpapi/generated', 'internal/db', 'packages/api-client/src/schema.d.ts')

workflow = yaml.safe_load((ROOT / '.github/workflows/checks.yml').read_text())
job = workflow['jobs']['generated-patch']
assert job['permissions'] == {'contents': 'read'}, 'untrusted PR code: read-only token only'
assert job['if'] == "github.event_name == 'pull_request'", 'job must run on (and only on) pull_request'
# Workflow-level trigger/env/defaults apply to this job too (PyYAML reads the `on` key as True).
outer = json.dumps({key: value for key, value in workflow.items() if key != 'jobs'}, default=str)
assert 'pull_request_target' not in outer and 'secrets.' not in outer and 'workflow_run' not in outer, outer
text = json.dumps(job)  # no line folding, unlike yaml.safe_dump
for forbidden in ('secrets.', 'git push', 'git commit', 'pull_request_target', 'contents: write'):
    assert forbidden not in text, forbidden
checkout = next(s for s in job['steps'] if s.get('uses', '').startswith('actions/checkout@'))
assert checkout.get('with', {}).get('persist-credentials') is False
# The patch must apply to the contributor's branch, not the synthetic merge commit.
assert checkout['with'].get('ref') == '${{ github.event.pull_request.head.sha }}'
regenerate = next(s['run'] for s in job['steps'] if 'sqlc generate' in s.get('run', ''))
# sqlc never deletes outputs of removed/renamed query files: clear generator-owned outputs first.
clean = 'rm -f internal/db/db.go internal/db/models.go internal/db/*.sql.go'
assert clean in regenerate and regenerate.index(clean) < regenerate.index('sqlc generate'), regenerate
upload = next(s for s in job['steps'] if s.get('uses', '').startswith('actions/upload-artifact@'))
# Immutable artifact names: a full re-run must not collide with attempt 1.
assert upload['with']['name'] == 'generated-patch-${{ github.run_id }}-${{ github.run_attempt }}', upload['with']
assert upload['with']['path'] == 'generated.patch'
assert 'sha256sum -c' in text and 'oapi-codegen' in text and 'sqlc generate' in text and 'openapi-typescript' in text
assert all(path in text for path in GENERATED)
assert 'apply_generated_patch.sh' in text and 'generated-patch' in text

FAKE_GH = '''#!/usr/bin/env bash
# fake: gh run download <id> -R <base repo> -p generated-patch-<id>-* -D <dir> (base repo also from a fork clone)
[[ "$1 $2 $3 $4 $5 $6 $7 $8" == "run download 4242 -R luogangan7-lgtm/opensite-commerce -p generated-patch-4242-* -D" ]] || exit 9
mkdir -p "$9/generated-patch-4242-1"
cp "$FAKE_PATCH" "$9/generated-patch-4242-1/generated.patch"
'''


def git(cwd, *args):
    return subprocess.run(['git', *args], cwd=cwd, check=True, capture_output=True, text=True).stdout


with tempfile.TemporaryDirectory() as temp:
    repo = Path(temp) / 'repo'
    (repo / 'scripts').mkdir(parents=True)
    (repo / 'internal/db').mkdir(parents=True)
    shutil.copy2(ROOT / 'scripts/apply_generated_patch.sh', repo / 'scripts')
    generated = repo / 'internal/db/models.go'
    generated.write_text('package db\n\ntype A struct{}\n\ntype B struct{}\n')
    (repo / 'README.md').write_text('hello\n')
    handwritten = repo / 'internal/db/cart_integration_test.go'  # hand-written gate inside internal/db
    handwritten.write_text('package db\n\nfunc check() bool { return true }\n')
    git(repo, 'init', '-q')
    git(repo, '-c', 'user.name=t', '-c', 'user.email=t@t', 'add', '.')
    git(repo, '-c', 'user.name=t', '-c', 'user.email=t@t', 'commit', '-qm', 'base')
    good = Path(temp) / 'good.patch'
    generated.write_text('package db\n\ntype A struct{}\n')  # "hand-deleted" chunk
    git(repo, '-c', 'user.name=t', '-c', 'user.email=t@t', 'commit', '-qam', 'probe')
    git(repo, 'checkout', '-q', 'HEAD~1', '--', 'internal/db/models.go')
    good.write_text(git(repo, 'diff', '--binary', 'HEAD'))
    git(repo, 'checkout', '-q', 'HEAD', '--', 'internal/db/models.go')
    bad = Path(temp) / 'bad.patch'
    bad.write_text('diff --git a/README.md b/README.md\n--- a/README.md\n+++ b/README.md\n@@ -1 +1 @@\n-hello\n+owned\n')
    bindir = Path(temp) / 'bin'
    bindir.mkdir()
    (bindir / 'gh').write_text(FAKE_GH)
    (bindir / 'gh').chmod(0o755)
    env = dict(os.environ, PATH=f'{bindir}{os.pathsep}{os.environ["PATH"]}')

    refused = subprocess.run(['bash', 'scripts/apply_generated_patch.sh', '4242'], cwd=repo,
                             env=dict(env, FAKE_PATCH=str(bad)), capture_output=True, text=True)
    assert refused.returncode == 3 and 'non-generated path README.md' in refused.stderr, refused
    assert (repo / 'README.md').read_text() == 'hello\n'

    # Allowlist bypasses: every one must be refused before the work tree changes.
    handwritten.write_text('package db\n\nfunc check() bool { return false }\n')
    weaken = git(repo, 'diff', '--binary')
    git(repo, 'checkout', '-q', '--', '.')
    evil = {
        'rename-out-of-allowlist': 'diff --git a/README.md b/internal/db/stolen.sql.go\nsimilarity index 100%\n'
                                   'rename from README.md\nrename to internal/db/stolen.sql.go\n',
        'copy-into-allowlist': 'diff --git a/README.md b/internal/db/copied.sql.go\nsimilarity index 100%\n'
                               'copy from README.md\ncopy to internal/db/copied.sql.go\n',
        'symlink': 'diff --git a/internal/db/link.sql.go b/internal/db/link.sql.go\nnew file mode 120000\n'
                   '--- /dev/null\n+++ b/internal/db/link.sql.go\n@@ -0,0 +1 @@\n+../../README.md\n\\ No newline at end of file\n',
        'mode-change': 'diff --git a/internal/db/models.go b/internal/db/models.go\nold mode 100644\nnew mode 100755\n',
        'hand-written-under-internal-db': weaken,
    }
    for case, text in evil.items():
        patch_file = Path(temp) / f'{case}.patch'
        patch_file.write_text(text)
        checked = subprocess.run(['git', 'apply', '--check', str(patch_file)], cwd=repo, capture_output=True, text=True)
        assert checked.returncode == 0, (case, 'fixture must be a patch git would really apply', checked.stderr)
        result = subprocess.run(['bash', 'scripts/apply_generated_patch.sh', '4242'], cwd=repo,
                                env=dict(env, FAKE_PATCH=str(patch_file)), capture_output=True, text=True)
        assert result.returncode == 3, (case, result)
        assert git(repo, 'status', '--porcelain') == '', (case, 'work tree changed before refusal')

    usage = subprocess.run(['bash', 'scripts/apply_generated_patch.sh', '42; rm -rf /'], cwd=repo, env=env,
                           capture_output=True, text=True)
    assert usage.returncode == 64, usage

    applied = subprocess.run(['bash', 'scripts/apply_generated_patch.sh', '4242'], cwd=repo,
                             env=dict(env, FAKE_PATCH=str(good)), capture_output=True, text=True)
    assert applied.returncode == 0, applied
    assert git(repo, 'diff', 'HEAD~1', '--', 'internal/db') == '', 'patch must restore generated output exactly'

print('PASS: generated-patch CI job is read-only; apply script restores exactly and refuses non-generated paths')
