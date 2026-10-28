#!/usr/bin/env bash
# Applies the `generated-patch` artifact uploaded by CI (foundation-contracts /
# generated-patch job) so generated code — OpenAPI Go, sqlc, TS client — is never
# edited or merged by hand. Depends on gh + git. Refuses any patch that touches a
# path outside the generated outputs (the artifact comes from untrusted PR code).
set -euo pipefail
[[ $# == 1 && $1 =~ ^[0-9]+$ ]] || { echo "usage: $0 <run-id>" >&2; exit 64; }
cd "$(dirname "${BASH_SOURCE[0]}")/.."
dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT
# The run lives in the base repository, also when working from a fork clone.
# Every attempt uploads its own immutable artifact; a full re-run leaves one
# directory per attempt, so take the highest attempt (the latest patch) instead
# of guessing which attempt produced the drift.
gh run download "$1" -R "${OPENSITE_GH_REPO:-luogangan7-lgtm/opensite-commerce}" \
  -p "generated-patch-$1-*" -D "$dir"
patch=''
best=-1
for candidate in "$dir"/generated-patch-"$1"-*/generated.patch; do
  [[ -f $candidate ]] || continue
  attempt="${candidate%/generated.patch}"
  attempt="${attempt##*-}"
  [[ $attempt =~ ^[0-9]+$ ]] || { echo "unexpected generated-patch artifact name: $candidate" >&2; exit 1; }
  if (( attempt > best )); then best=$attempt; patch=$candidate; fi
done
[[ -n $patch && -s $patch ]] || { echo "generated.patch missing or empty in run $1" >&2; exit 1; }
echo "Using generated-patch attempt $best" >&2
# Exact generator-owned files only (hand-written tests live next to them), and
# plain create/delete/modify only: any rename, copy, mode change or symlink
# (mode 120000) is refused before the work tree is touched.
allowed='^(internal/db/(db|models|[a-z0-9_]+\.sql)\.go|internal/platform/httpapi/generated/api\.gen\.go|packages/api-client/src/schema\.d\.ts)$'
while IFS=$'\t' read -r _ _ path; do
  [[ $path =~ $allowed ]] || { echo "Refusing: patch touches non-generated path $path" >&2; exit 3; }
done < <(git apply --numstat "$patch")
while read -r line; do
  [[ $line =~ ^(create|delete)\ mode\ 100644\ (.*)$ && ${BASH_REMATCH[2]} =~ $allowed ]] ||
    { echo "Refusing: patch summary not a plain generated-file change: $line" >&2; exit 3; }
done < <(git apply --summary "$patch")
git apply "$patch"
echo "Applied generated-patch from run $1; review with git diff and commit."
