# rolegate

`scripts/rolegate` is an offline AST gate that rejects cluster-role SQL written
directly in Go tests: `CREATE/ALTER/DROP ROLE`, role `GRANT ... TO` /
`REVOKE ... FROM`, and `pg_auth_members` writes. It is driven by
`tests/test_pg_role_fixture.py`; run it with `go run ./scripts/rolegate -root .`.

It folds string literals (including raw strings and escapes), `+` concatenation,
parentheses, `string(x)` conversions, and named string constants — package-level
ones across files of the same package, and `const` declarations inside function
bodies. A foldable expression is judged as a whole and its constituent literals
are not re-checked, so `"GRANT " + "SELECT ON app.users TO app_runtime"` (an
object privilege) passes while `"GRANT " + member + " TO app_runtime"` still
reports the bare `"GRANT "` fragment.

## Known limitations

This gate catches **accidental** bypasses only. It is not a security boundary and
must not be treated as one.

- **`fmt.Sprintf` / `strings.Join` and other helper calls are not evaluated.**
  Only literals, `+`, parentheses, constants and `string(...)` are folded, so
  `fmt.Sprintf("GRANT %s TO %s", role, member)` is invisible to the gate. This is
  deliberate: evaluating arbitrary string building would be a full interpreter.
- Constants from *other* packages are not resolved; only the file's own package
  (including function bodies) is folded.
- One function body shares one constant scope, so block-local shadowing between
  two constants of the same name in a single body is resolved coarsely (the
  foldable value wins). Shadowing across functions does not leak.
- `testdata/`, `vendor/` and `node_modules/` are skipped, and `-allow` prefixes
  are trusted.

**The actual control is `pgxtest.NewRole`** (`internal/platform/pgxtest`): it owns
the shared advisory lock and is the only place allowed to write roles. Role SQL
that deliberately evades this scanner still has to pass that helper at review
time. The gate exists to fail the ordinary "I'll just exec the SQL here" mistake
before CI, not to stop an author who is actively working around it.
