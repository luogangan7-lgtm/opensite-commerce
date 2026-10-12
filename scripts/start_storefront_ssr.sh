#!/usr/bin/env bash
# Responsibility: run standalone Astro without database identity in its environment.
# Dependencies: built Node adapter entry; callers: local/CI storefront launcher.
# Invariant: the storefront receives only its restricted HTTP Read API origin.
set -euo pipefail
cd "$(dirname "$0")/.."
unset DATABASE_URL OSC_PUBLIC_DATABASE_URL PGHOST PGHOSTADDR PGPORT PGUSER PGPASSWORD PGDATABASE PGSERVICE PGSERVICEFILE PGPASSFILE PGOPTIONS
exec node web/storefront/dist/server/entry.mjs
