# Module governance

`registry.yaml` uses the JSON subset of YAML 1.2 so the checker needs only the
Python standard library. Keep this format when editing; arbitrary YAML aliases
and flow/block combinations are not accepted.

- `status` describes database ownership on this branch. `planned` modules list
  candidate tables from open PRs; these are excluded from the live catalog's
  expected tables. Promote the module and add test pointers when its tables land.
- `go_path` reserves `internal/modules/<name>`. `go_status` separately describes
  the Go implementation. Existing SQL data does not imply a Go service exists.
- Operations have `operation_id` and `status`. An `implemented` operation must
  exist under an inline OpenAPI `paths/<path>/<method>/operationId`. All current
  operations are planned; the registry does not claim handlers or HTTP acceptance.
- `owns_tables` contains unqualified `app` table names, each with one owner.
  Cross-cutting `migrations` record an owner and `touches_tables`, without changing
  business table ownership. Migration filenames refer to this branch's inventory.
- `dependencies` is a directed acyclic graph. A module may import another only
  through `internal/modules/<other>/contract`, with that dependency declared.
  Its own private packages remain allowed. `tests` contains repository-relative
  file pointers, not commands for the checker to execute.

`bash scripts/check_all.sh` runs the registry/operation/DAG checks, real
`go list -e -deps -json -test ./...` in both default and `pgx` modes, and regression
fault probes. Package-load errors fail the gate, even with `go list -e` exit 0.
OpenAPI extraction accepts this repository's inline block mappings and literal
identifiers; referenced/merged Path Items and unsupported path structures fail.

`scripts/check_pg.sh` runs `python3 scripts/check_modules.py --tables` after the
migrations and SQL suite. It queries `pg_catalog` for ordinary and partitioned
tables in `app`, then rejects unknown, duplicate-owned, or missing tables. It uses
the runner's `DATABASE_URL`/psql or Docker Compose connection. Connection errors
and malformed responses fail; an offline fake psql runner test is only flow proof.

For a disposable database, `--registry <file>` permits an alternate registry for
fault probes without editing the checked source. No database writes are performed
by the checker. The Go/SQL gates still run their complete suites; downstream test
selection and impact routing belong to C-03e. The API skeleton and shared HTTP
platform paths belong to C-03ac and are unchanged here.
