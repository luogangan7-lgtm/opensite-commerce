//go:build pgx

// Responsibility: open only the dedicated returns coordinator capability.
// Dependencies: checked QueryRunner and PostgreSQL catalogs; caller: bootstrap.
// Invariants: no mixed/administrative capability or extra effective grants;
// request callbacks use RunAuthorizedTx, never a raw connection or pool.
package pgxadapter

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

func OpenReturnsRunner(ctx context.Context, dsn string) (*QueryRunner, error) {
	return openCoordinatorRunner(ctx, dsn, "app_returns_service")
}

func OpenPaymentsRunner(ctx context.Context, dsn string) (*QueryRunner, error) {
	return openCoordinatorRunner(ctx, dsn, "app_payments_ingress")
}

func openCoordinatorRunner(ctx context.Context, dsn, capability string) (*QueryRunner, error) {
	runner, err := OpenQueryRunner(ctx, dsn)
	if err != nil {
		return nil, errors.New("returns database is unavailable or misconfigured")
	}
	err = runner.runner.inMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var safe bool
		err := tx.QueryRow(ctx, `SELECT
   r.rolcanlogin AND r.rolinherit AND current_user=session_user
   AND NOT (r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication)
   AND NOT (g.rolcanlogin OR g.rolinherit OR g.rolsuper OR g.rolbypassrls OR g.rolcreatedb OR g.rolcreaterole OR g.rolreplication)
   AND EXISTS (SELECT FROM pg_auth_members m WHERE m.member=r.oid AND m.roleid=g.oid AND m.set_option AND m.inherit_option AND NOT m.admin_option)
   AND NOT EXISTS (SELECT FROM pg_auth_members m WHERE m.member=g.oid OR m.roleid=r.oid OR (m.member=r.oid AND (m.roleid<>g.oid OR m.admin_option)))
   AND NOT EXISTS (SELECT FROM pg_namespace n WHERE n.nspowner IN(r.oid,g.oid)
    OR has_schema_privilege(r.oid,n.oid,'USAGE') IS DISTINCT FROM has_schema_privilege(g.oid,n.oid,'USAGE')
    OR has_schema_privilege(r.oid,n.oid,'CREATE')
    OR has_schema_privilege(r.oid,n.oid,'USAGE WITH GRANT OPTION'))
   AND NOT EXISTS (SELECT FROM pg_class c WHERE c.relowner IN(r.oid,g.oid))
   AND NOT EXISTS (SELECT FROM pg_proc f WHERE f.proowner IN(r.oid,g.oid))
   AND NOT EXISTS (SELECT FROM pg_database d WHERE d.datdba=r.oid)
   AND NOT EXISTS (SELECT FROM pg_policy p WHERE r.oid=ANY(p.polroles))
   AND NOT EXISTS (SELECT FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
    CROSS JOIN (VALUES('SELECT'),('INSERT'),('UPDATE'),('DELETE'),('TRUNCATE'),('REFERENCES'),('TRIGGER'),('MAINTAIN')) p(priv)
    WHERE n.nspname IN('app','queue','public') AND c.relkind IN('r','p','v','m','f')
    AND (has_table_privilege(r.oid,c.oid,p.priv) IS DISTINCT FROM has_table_privilege(g.oid,c.oid,p.priv)
     OR has_table_privilege(r.oid,c.oid,p.priv||' WITH GRANT OPTION')))
   AND NOT EXISTS (SELECT FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
    JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped
    CROSS JOIN (VALUES('SELECT'),('INSERT'),('UPDATE'),('REFERENCES')) p(priv)
    WHERE n.nspname IN('app','queue','public') AND c.relkind IN('r','p','v','m','f')
    AND (has_column_privilege(r.oid,c.oid,a.attnum,p.priv) IS DISTINCT FROM has_column_privilege(g.oid,c.oid,a.attnum,p.priv)
     OR has_column_privilege(r.oid,c.oid,a.attnum,p.priv||' WITH GRANT OPTION')))
   AND NOT EXISTS (SELECT FROM pg_proc f JOIN pg_namespace n ON n.oid=f.pronamespace
    WHERE n.nspname IN('app','queue','public') AND
     (has_function_privilege(r.oid,f.oid,'EXECUTE') IS DISTINCT FROM has_function_privilege(g.oid,f.oid,'EXECUTE')
      OR has_function_privilege(r.oid,f.oid,'EXECUTE WITH GRANT OPTION')))
   AND NOT EXISTS (SELECT FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
    WHERE n.nspname IN('app','queue','public') AND c.relkind='S' AND CASE WHEN c.relkind='S' THEN
     has_sequence_privilege(r.oid,c.oid,'USAGE,SELECT,UPDATE') IS DISTINCT FROM has_sequence_privilege(g.oid,c.oid,'USAGE,SELECT,UPDATE')
     OR has_sequence_privilege(r.oid,c.oid,'USAGE WITH GRANT OPTION,SELECT WITH GRANT OPTION,UPDATE WITH GRANT OPTION') ELSE false END)
   FROM pg_roles r CROSS JOIN pg_roles g WHERE r.rolname=session_user AND g.rolname=$1`, capability).Scan(&safe)
		if err != nil || !safe {
			return errors.New("returns database capability is invalid")
		}
		return nil
	})
	if err != nil {
		runner.Close()
		return nil, errors.New("returns database capability is invalid")
	}
	runner.paymentIngress = capability == "app_payments_ingress"
	return runner, nil
}
