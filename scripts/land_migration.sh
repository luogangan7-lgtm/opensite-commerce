#!/usr/bin/env bash
# Integrator only, once per migration at landing: move migrations/pending/<id>_<name>.sql
# to the next contiguous migrations/NNN_<name>.sql and rewrite registry/sqlc references.
# CI selects landing mode from trusted repository changes (a removed proposal
# or changed numbered history) or FINAL_LANDING_POLICY; the optional
# [final-landing] marker may only add final validation. Final mode requires zero
# pending, contiguous numbering, the exact head SHA and per-lane checkout
# identities, so an unmarked landing can never fall back to proposal validation.
set -euo pipefail
(($# == 1)) || { echo "usage: $0 <stable-id>" >&2; exit 64; }
exec python3 "$(dirname "$0")/pending_migrations.py" land "$1"
