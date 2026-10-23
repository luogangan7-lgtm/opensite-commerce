#!/usr/bin/env bash
# Responsibility: install CI-only apt packages behind hard time bounds so a stalled
#   mirror cannot eat a lane's timeout (incident: foundation-pg run 39989676720 hung
#   >21 min inside an unbounded `apt-get update`).
# Dependencies: bash, coreutils `timeout`, apt-get, sudo only when not root
#   (Playwright container jobs already run as root).
# Called by: .github/workflows/checks.yml, stripe-sandbox.yml, inventory-race-soak.yml,
#   checkout-consent.yml, fulfillment-review.yml.
# Invariants: a no-op when every requested command is already on PATH; every apt
#   network call has a wall-clock bound; at most 3 attempts; failure is fatal.
set -euo pipefail

UPDATE_TIMEOUT=180
INSTALL_TIMEOUT=300
ATTEMPTS=3
RETRY_BACKOFF=10
NO_RECOMMENDS=()

usage() {
  echo "usage: $0 [--no-install-recommends] <package>..." >&2
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --no-install-recommends) NO_RECOMMENDS=(--no-install-recommends); shift ;;
    -h|--help) usage; exit 0 ;;
    --) shift; break ;;
    -*) echo "FAIL: ci_apt_install: unknown option: $1" >&2; usage; exit 2 ;;
    *) break ;;
  esac
done

if [ "$#" -eq 0 ]; then
  echo "FAIL: ci_apt_install: no packages requested" >&2
  usage
  exit 2
fi

# Package name -> the command that proves it is already installed.
command_for_package() {
  case "$1" in
    postgresql-client|postgresql) echo 'psql' ;;
    *) echo "$1" ;;
  esac
}

missing=()
for package in "$@"; do
  binary="$(command_for_package "$package")"
  if ! command -v "$binary" >/dev/null 2>&1; then
    missing+=("$package")
  fi
done

if [ "${#missing[@]}" -eq 0 ]; then
  echo "SKIP apt: already present"
  exit 0
fi

if [ "$(id -u)" -eq 0 ]; then
  SUDO=()
else
  SUDO=(sudo)
fi

attempt=1
while [ "$attempt" -le "$ATTEMPTS" ]; do
  echo "apt: attempt ${attempt}/${ATTEMPTS}: update + install ${missing[*]}"
  if timeout "$UPDATE_TIMEOUT" "${SUDO[@]}" apt-get update -qq -o Acquire::Retries=3 -o Acquire::http::Timeout=20 \
     && timeout "$INSTALL_TIMEOUT" "${SUDO[@]}" apt-get install -y -qq "${NO_RECOMMENDS[@]}" "${missing[@]}"; then
    echo "apt: attempt ${attempt}/${ATTEMPTS}: installed ${missing[*]}"
    exit 0
  fi
  echo "apt: attempt ${attempt}/${ATTEMPTS} failed" >&2
  attempt=$((attempt + 1))
  if [ "$attempt" -le "$ATTEMPTS" ]; then
    sleep $((RETRY_BACKOFF * (attempt - 1)))
  fi
done

echo "FAIL: apt: could not install ${missing[*]} after ${ATTEMPTS} attempts (update bounded at ${UPDATE_TIMEOUT}s, install at ${INSTALL_TIMEOUT}s). Check the apt mirror or retry the lane." >&2
exit 1
