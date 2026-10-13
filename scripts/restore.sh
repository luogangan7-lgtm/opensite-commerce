#!/usr/bin/env sh
# Fresh DB only; never drop/clean existing objects. See ops_backup's restore gate.
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
exec python3 "$ROOT/scripts/ops_backup.py" restore "$@"
