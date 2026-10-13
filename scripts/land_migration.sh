#!/usr/bin/env bash
# Integrator only, once per migration at landing: move migrations/pending/<id>_<name>.sql
# to the next contiguous migrations/NNN_<name>.sql and rewrite registry/sqlc references.
set -euo pipefail
(($# == 1)) || { echo "usage: $0 <stable-id>" >&2; exit 64; }
exec python3 "$(dirname "$0")/pending_migrations.py" land "$1"
