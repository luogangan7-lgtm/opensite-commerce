// Responsibility: inert adapter export review inventory; never executable code.
// Dependencies: export_surface_test.go; caller: adapter type/authority gate.
// Invariant: check_modules accepts only this literal declaration in this file.
package pgxadapter_test

// Deliberately list each public object/member, including inert alias members.
// New exports require a visible review change here, even if their type is safe.
var reviewedAdapterExports = []string{
	"type:CommandTag",
	"type:CustomerRunner",
	"type:IdentityRunner",
	"type:PgError",
	"type:QueryRunner",
	"type:QueueDB",
	"type:QueuePool",
	"func:InQueueTenant",
	"func:NewCommandTag",
	// Bounded authority runner keeps its connection pool private.
	"func:OpenAuthorization",
	"func:OpenCustomerRunner",
	"func:OpenIdentity",
	"func:OpenMigrationPool",
	// Payments ingress exposes checked transactions only; no raw pool or connection.
	"func:OpenPaymentsRunner",
	"func:OpenQueryRunner",
	"func:OpenQueuePool",
	// Returns coordinator exposes checked transactions only; its pool stays private.
	"func:OpenReturnsRunner",
	// Dedicated bounded staff lane exposes a runner, never a raw connection.
	"func:OpenStaffAuthentication",
	"method:CommandTag.Delete",
	"method:CommandTag.Insert",
	"method:CommandTag.RowsAffected",
	"method:CommandTag.Select",
	"method:CommandTag.String",
	"method:CommandTag.Update",
	"method:CustomerRunner.Close",
	"method:CustomerRunner.InTenant",
	"method:IdentityRunner.Close",
	"method:IdentityRunner.InMaintenance",
	// Exact-role admission lends generated queries only, never the private connection.
	"method:IdentityRunner.RunAuthentication",
	"method:PgError.Error",
	"method:PgError.SQLState",
	"method:QueryRunner.Close",
	"method:QueryRunner.Queries",
	"method:QueryRunner.RunAuthorized",
	"method:QueryRunner.RunAuthorizedSnapshot",
	"method:QueryRunner.RunAuthorizedTx",
	"method:QueryRunner.RunAuthorizedTxSnapshot",
	"method:QueryRunner.RunCapabilitySnapshot",
	// Fake admission lends only a mapped app_runtime transaction plus DB time;
	// both build variants return error only and never export the private pool.
	"method:QueryRunner.RunFakePaymentTx",
	// Stripe/test admission lends only the account-mapped runtime tx, never the connection.
	"method:QueryRunner.RunStripePaymentTx",
	// Serving refresher seam: a tenant-scoped app_runtime tx under FORCE RLS (tenant from
	// deployment config); callers are pinned by check_modules TRUSTED_TX_ALLOWED.
	"method:QueryRunner.RunTrustedTx",
	"method:QueueDB.Begin",
	"method:QueueDB.Exec",
	"method:QueueDB.Query",
	"method:QueueDB.QueryRow",
	"method:QueuePool.Begin",
	"method:QueuePool.Close",
	"method:QueuePool.Exec",
	"method:QueuePool.Query",
	"method:QueuePool.QueryRow",
	"method:QueuePool.RiverDriver",
	"field:PgError.Code",
	"field:PgError.ColumnName",
	"field:PgError.ConstraintName",
	"field:PgError.DataTypeName",
	"field:PgError.Detail",
	"field:PgError.File",
	"field:PgError.Hint",
	"field:PgError.InternalPosition",
	"field:PgError.InternalQuery",
	"field:PgError.Line",
	"field:PgError.Message",
	"field:PgError.Position",
	"field:PgError.Routine",
	"field:PgError.SchemaName",
	"field:PgError.Severity",
	"field:PgError.SeverityUnlocalized",
	"field:PgError.TableName",
	"field:PgError.Where",
}
