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
gh run download "$1" -n generated-patch -D "$dir"
patch="$dir/generated.patch"
[[ -s $patch ]] || { echo "generated.patch missing or empty in run $1" >&2; exit 1; }
while IFS=$'\t' read -r _ _ path; do
  case "$path" in
    *..*) echo "Refusing: suspicious path $path" >&2; exit 3 ;;
    internal/platform/httpapi/generated/*|internal/db/*|packages/api-client/src/schema.d.ts) ;;
    *) echo "Refusing: patch touches non-generated path $path" >&2; exit 3 ;;
  esac
done < <(git apply --numstat "$patch")
git apply "$patch"
echo "Applied generated-patch from run $1; review with git diff and commit."
