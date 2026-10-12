#!/usr/bin/env python3
"""Fail if CI is not hermetic: unpinned actions/images/Go, `go get`, unhashed pip. Stdlib only.
Depends on: .github/workflows/*.yml, go.mod. Called by: scripts/check_all.sh.
Invariant: line-based scan (comments stripped), no YAML parser needed."""
import re, sys
from pathlib import Path

root = Path(__file__).resolve().parents[1]
gomod = re.search(r'^go (\S+)$', (root / 'go.mod').read_text(), re.M)
go_pin = gomod.group(1) if gomod else None
errs = []
for wf in sorted((root / '.github/workflows').glob('*.yml')):
    for n, raw in enumerate(wf.read_text().splitlines(), 1):
        line = re.sub(r'\s#.*$', '', raw).strip()
        where = f'{wf.name}:{n}'
        if m := re.match(r'-?\s*uses:\s*(\S+)', line):
            if not re.fullmatch(r'[\w.-]+/[\w./-]+@[0-9a-f]{40}', m[1]):
                errs.append(f'{where}: action not pinned to 40-hex SHA: {m[1]}')
        if m := re.match(r'image:\s*(\S+)', line):
            if '@sha256:' not in m[1]:
                errs.append(f'{where}: image lacks @sha256 digest: {m[1]}')
        if m := re.match(r'go-version:\s*[\'"]?([^\'"\s]+)', line):
            if not re.fullmatch(r'\d+\.\d+\.\d+', m[1]):
                errs.append(f'{where}: go-version not exact x.y.z: {m[1]}')
            elif m[1] != go_pin:
                errs.append(f'{where}: go-version {m[1]} != go.mod go {go_pin}')
        if re.search(r'\bgo get\b', line):
            errs.append(f'{where}: `go get` forbidden in CI')
        if re.search(r'\bpip3? install\b', line) and '--require-hashes' not in line:
            errs.append(f'{where}: pip install without --require-hashes')
if errs:
    print('FAIL: CI pin check\n' + '\n'.join(errs), file=sys.stderr)
    sys.exit(1)
print(f'PASS: CI pins (go {go_pin})')
