// Test fixture: the locked helper and object privileges are not cluster role writes.
package pass

// CREATE ROLE and GRANT x TO y in comments are not string values.
func helperCalls(t, ctx, dsn, password any) {
	pgxtest.NewRole(t, ctx, dsn, "app_login", password, false, "app_runtime")
	pgxtest.NewRole(t, ctx, dsn, "app_login", password, true, "app_auth")
	role.Grant(t, ctx, "app_auth")
	role.Alter(t, ctx, "NOINHERIT")
	_ = "GRANT SELECT ON app.users TO app_runtime"
	_ = "REVOKE SELECT ON app.users FROM app_runtime"
	_ = "preview grant setup status=%d"
	_ = "idempotent revoke"
}
