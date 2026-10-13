#!/usr/bin/env sh
# Thin backup entrypoint; ops_backup owns all validation and bounded PG transport.
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
exec python3 "$ROOT/scripts/ops_backup.py" backup "$@"
