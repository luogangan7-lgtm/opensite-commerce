#!/usr/bin/env bash
# Integrator only, once per migration at landing: move migrations/pending/<id>_<name>.sql
# to the next contiguous migrations/NNN_<name>.sql and rewrite registry/sqlc references.
# The final landing commit/PR must carry [final-landing] so CI runs
# scripts/check_landing.py in final mode (zero pending, contiguous numbering,
# exact head SHA); unmarked commits stay proposal-mode validation only.
set -euo pipefail
(($# == 1)) || { echo "usage: $0 <stable-id>" >&2; exit 64; }
exec python3 "$(dirname "$0")/pending_migrations.py" land "$1"
