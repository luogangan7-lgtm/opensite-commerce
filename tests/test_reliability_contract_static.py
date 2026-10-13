#!/usr/bin/env python3
"""Verify locked River decisions and release limits; check_all calls this gate."""
from pathlib import Path
import re
root=Path(__file__).resolve().parents[1]
mod=(root/'go.mod').read_text();doc=(root/'docs/architecture/APP_LAYER_DECISIONS.md').read_text()
version=re.search(r'github.com/riverqueue/river (v[\d.]+)',mod).group(1)
assert f'adopted **{version}**' in doc, 'RIVER_DECISION_VERSION_DRIFT'
assert re.search(r'github.com/riverqueue/river/riverdriver/riverpgxv5 '+re.escape(version)+r'\b',mod), 'RIVER_DRIVER_VERSION_DRIFT'
print('PASS: River decision and driver match locked '+version)
