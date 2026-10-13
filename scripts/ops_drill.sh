#!/usr/bin/env bash
# Local disposable project only; Python owns safeguards and cleanup.
set -euo pipefail
exec python3 "$(dirname "$0")/ops_drill.py" "$@"
