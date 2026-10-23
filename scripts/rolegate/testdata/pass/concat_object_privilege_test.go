// Test fixture: a foldable expression is judged as a whole, so the "GRANT "/"REVOKE "
// prefix of a concatenated object-privilege statement is not flagged on its own.
package pass

var objectPrivilegeStatements = []string{
	"GRANT " + "SELECT ON app.users TO app_runtime",
	"REVOKE " + "SELECT ON app.users FROM app_runtime",
}
