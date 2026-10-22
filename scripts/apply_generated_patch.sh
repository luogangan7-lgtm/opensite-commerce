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
gh run download "$1" -R "${OPENSITE_GH_REPO:-luogangan7-lgtm/opensite-commerce}" -n generated-patch -D "$dir"
patch="$dir/generated.patch"
[[ -s $patch ]] || { echo "generated.patch missing or empty in run $1" >&2; exit 1; }
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
