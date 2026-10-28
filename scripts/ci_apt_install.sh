#!/usr/bin/env bash
# Responsibility: install CI-only apt packages behind hard time bounds so a stalled
#   mirror cannot eat a lane's timeout (incident: foundation-pg run 39989676720 hung
#   >21 min inside an unbounded `apt-get update`).
# Dependencies: bash, coreutils `timeout`, apt-get, sudo only when not root
#   (Playwright container jobs already run as root).
# Called by: .github/workflows/checks.yml, stripe-sandbox.yml, inventory-race-soak.yml,
#   checkout-consent.yml, fulfillment-review.yml.
# Invariants: a no-op when every requested command is already on PATH; every apt
#   network call has a wall-clock bound; the whole install phase has one total
#   budget that shrinks each attempt and every backoff; failure is fatal.
set -euo pipefail

# One total budget below the shortest caller job (20 min) leaves the lane its
# test time even when update and install both run slowly. The per-attempt caps
# are still hard bounds; they are clipped to the remaining total on every round.
TOTAL_BUDGET=${CI_APT_TOTAL_BUDGET:-300}
UPDATE_TIMEOUT=${CI_APT_UPDATE_TIMEOUT:-180}
INSTALL_TIMEOUT=${CI_APT_INSTALL_TIMEOUT:-300}
ATTEMPTS=${CI_APT_ATTEMPTS:-3}
RETRY_BACKOFF=${CI_APT_RETRY_BACKOFF:-10}
NO_RECOMMENDS=()

require_positive() {
  local name=$1 value=$2
  if ! [[ $value =~ ^[0-9]+$ ]] || [ "$value" -le 0 ]; then
    echo "FAIL: ci_apt_install: $name must be a positive integer, got '$value'" >&2
    exit 2
  fi
}

require_nonnegative() {
  local name=$1 value=$2
  if ! [[ $value =~ ^[0-9]+$ ]]; then
    echo "FAIL: ci_apt_install: $name must be a non-negative integer, got '$value'" >&2
    exit 2
  fi
}

require_positive CI_APT_TOTAL_BUDGET "$TOTAL_BUDGET"
require_positive CI_APT_UPDATE_TIMEOUT "$UPDATE_TIMEOUT"
require_positive CI_APT_INSTALL_TIMEOUT "$INSTALL_TIMEOUT"
require_positive CI_APT_ATTEMPTS "$ATTEMPTS"
require_nonnegative CI_APT_RETRY_BACKOFF "$RETRY_BACKOFF"

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

started=$SECONDS
remaining_budget() {
  local left=$((TOTAL_BUDGET - (SECONDS - started)))
  if [ "$left" -lt 0 ]; then left=0; fi
  printf '%s' "$left"
}

attempt=1
while [ "$attempt" -le "$ATTEMPTS" ]; do
  left="$(remaining_budget)"
  if [ "$left" -le 0 ]; then
    echo "FAIL: apt: total budget ${TOTAL_BUDGET}s exhausted before attempt ${attempt}" >&2
    exit 1
  fi
  update_budget=$(( left < UPDATE_TIMEOUT ? left : UPDATE_TIMEOUT ))
  echo "apt: attempt ${attempt}/${ATTEMPTS}: update (<=${update_budget}s) + install, remaining budget ${left}s"
  if timeout "$update_budget" "${SUDO[@]}" apt-get update -qq -o Acquire::Retries=3 -o Acquire::http::Timeout=20; then
    left="$(remaining_budget)"
    if [ "$left" -gt 0 ]; then
      install_budget=$(( left < INSTALL_TIMEOUT ? left : INSTALL_TIMEOUT ))
      if timeout "$install_budget" "${SUDO[@]}" apt-get install -y -qq "${NO_RECOMMENDS[@]}" "${missing[@]}"; then
        echo "apt: attempt ${attempt}/${ATTEMPTS}: installed ${missing[*]} ($((SECONDS - started))s elapsed)"
        exit 0
      fi
    fi
  fi
  echo "apt: attempt ${attempt}/${ATTEMPTS} failed" >&2
  attempt=$((attempt + 1))
  if [ "$attempt" -le "$ATTEMPTS" ]; then
    left="$(remaining_budget)"
    if [ "$left" -le 0 ]; then
      echo "FAIL: apt: total budget ${TOTAL_BUDGET}s exhausted after a failed attempt; not retrying" >&2
      break
    fi
    backoff=$((RETRY_BACKOFF * (attempt - 1)))
    if [ "$backoff" -gt "$left" ]; then backoff=$left; fi
    if [ "$backoff" -gt 0 ]; then sleep "$backoff"; fi
  fi
done

echo "FAIL: apt: could not install ${missing[*]} after $((attempt - 1)) attempt(s) within ${TOTAL_BUDGET}s total budget (elapsed $((SECONDS - started))s; update per-attempt <=${UPDATE_TIMEOUT}s, install <=${INSTALL_TIMEOUT}s). Check the apt mirror or retry the lane." >&2
exit 1
