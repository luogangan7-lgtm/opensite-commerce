//go:build pgx

// Responsibility: open the isolated customer-auth capability and tenant transaction.
// Dependencies: pgxadapter Runner, sqlc and tenanttx; bootstrap owns its lifecycle.
// Invariants: dedicated NOADMIN login, app_customer_auth only, no runtime/staff
// authority, local role/tenant scope and no DSN/backend errors in public failures.
package pgxadapter

import (
	"context"
	"errors"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

type CustomerRunner struct {
	runner Runner
	close  func()
	once   sync.Once
}

func OpenCustomerRunner(ctx context.Context, dsn string) (*CustomerRunner, error) {
	r, closePool, err := Open(ctx, dsn)
	if err != nil {
		return nil, errors.New("customer database is unavailable or misconfigured")
	}
	err = r.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var safe bool
		err := tx.QueryRow(ctx, `WITH expected_functions AS (
 SELECT unnest(ARRAY[
  'app.customer_claim_guest_orders(uuid)',
  'app.customer_prepare_registration(text,text,bytea,timestamp with time zone)',
  'app.customer_consume_verification(bytea)',
  'app.customer_prepare_reset(text,bytea,timestamp with time zone)',
  'app.customer_consume_reset(bytea,text)',
  'app.customer_save_session(uuid,text,bytea,bytea,timestamp with time zone,timestamp with time zone)',
  'app.customer_session(bytea,bigint)','app.customer_revoke_session(bytea)',
  'app.customer_admit(bytea,bytea,timestamp with time zone)','app.customer_failed(bytea,bytea)'])::regprocedure AS oid
 ) SELECT
 NOT (r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication)
 AND r.rolcanlogin AND current_user=session_user
 AND EXISTS (SELECT FROM pg_roles p WHERE p.rolname='app_customer_auth'
  AND NOT (p.rolcanlogin OR p.rolsuper OR p.rolbypassrls OR p.rolcreatedb OR p.rolcreaterole OR p.rolreplication OR p.rolinherit))
 AND EXISTS (SELECT FROM pg_roles p WHERE p.rolname='app_customer_executor'
  AND NOT (p.rolcanlogin OR p.rolsuper OR p.rolbypassrls OR p.rolcreatedb OR p.rolcreaterole OR p.rolreplication OR p.rolinherit))
 AND NOT EXISTS (SELECT FROM pg_auth_members m WHERE m.member='app_customer_auth'::regrole)
 AND NOT EXISTS (SELECT FROM pg_auth_members m WHERE m.member='app_customer_executor'::regrole OR m.roleid='app_customer_executor'::regrole)
 AND EXISTS (SELECT FROM pg_auth_members m JOIN pg_roles p ON p.oid=m.roleid
  WHERE m.member=r.oid AND p.rolname='app_customer_auth' AND m.set_option AND NOT m.admin_option)
 AND NOT EXISTS (SELECT FROM pg_auth_members m JOIN pg_roles p ON p.oid=m.roleid
  WHERE m.member=r.oid AND (p.rolname<>'app_customer_auth' OR m.admin_option))
 AND NOT EXISTS (SELECT FROM pg_namespace n WHERE n.nspname='app' AND n.nspowner IN(r.oid,'app_customer_auth'::regrole))
 AND NOT EXISTS (SELECT FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname='app' AND c.relowner IN(r.oid,'app_customer_auth'::regrole))
 AND NOT has_schema_privilege(session_user,'app','CREATE')
 AND NOT has_schema_privilege('app_customer_auth','app','CREATE')
 AND NOT has_schema_privilege(session_user,'app','USAGE WITH GRANT OPTION')
 AND NOT has_schema_privilege('app_customer_auth','app','USAGE WITH GRANT OPTION')
 AND NOT EXISTS (SELECT FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname='app' AND c.relkind IN('r','p')
   AND c.relname IN('customers','customer_credentials','customer_auth_tokens','customer_sessions','customer_guest_order_links','customer_auth_limits')
   AND NOT (c.relrowsecurity AND c.relforcerowsecurity))
 AND NOT EXISTS (SELECT FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  CROSS JOIN (VALUES(session_user::text),('app_customer_auth')) AS subject(name)
  WHERE n.nspname='app' AND c.relkind IN('r','p') AND
   (has_table_privilege(subject.name,c.oid,'INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER,MAINTAIN')
    OR has_any_column_privilege(subject.name,c.oid,'UPDATE,REFERENCES')
    OR has_any_column_privilege(subject.name,c.oid,'SELECT WITH GRANT OPTION,INSERT WITH GRANT OPTION,UPDATE WITH GRANT OPTION,REFERENCES WITH GRANT OPTION')
    OR (c.relname NOT IN('customers','customer_credentials','customer_auth_tokens','customer_sessions','customer_guest_order_links')
        AND has_any_column_privilege(subject.name,c.oid,'SELECT'))
    OR (c.relname<>'customers' AND has_any_column_privilege(subject.name,c.oid,'INSERT'))))
 AND NOT EXISTS (SELECT FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped
  CROSS JOIN (VALUES(session_user::text),('app_customer_auth')) AS subject(name)
  WHERE n.nspname='app' AND c.relname='customers'
   AND has_column_privilege(subject.name,c.oid,a.attnum,'INSERT')
   AND a.attname NOT IN('tenant_id','id','email','display_name'))
 AND NOT EXISTS (SELECT FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  CROSS JOIN (VALUES(session_user::text),('app_customer_auth')) AS subject(name)
  WHERE n.nspname='app' AND c.relkind='S' AND has_sequence_privilege(subject.name,c.oid,'USAGE,SELECT,UPDATE'))
 AND NOT EXISTS (SELECT FROM pg_proc f JOIN pg_namespace n ON n.oid=f.pronamespace
  CROSS JOIN (VALUES(session_user::text),('app_customer_auth')) AS subject(name)
  WHERE n.nspname='app' AND (has_function_privilege(subject.name,f.oid,'EXECUTE WITH GRANT OPTION')
   OR (has_function_privilege(subject.name,f.oid,'EXECUTE') AND f.oid NOT IN(SELECT oid FROM expected_functions))))
 AND NOT EXISTS (SELECT FROM pg_proc f JOIN expected_functions e ON e.oid=f.oid
  WHERE f.proowner<>'app_customer_executor'::regrole OR NOT f.prosecdef
   OR f.proconfig IS DISTINCT FROM ARRAY['search_path=pg_catalog']::text[]
   OR NOT has_function_privilege('app_customer_auth',f.oid,'EXECUTE')
   OR EXISTS(SELECT FROM aclexplode(coalesce(f.proacl,acldefault('f',f.proowner)))
    WHERE grantee=0 AND privilege_type='EXECUTE'))
 FROM pg_roles r WHERE r.rolname=session_user`).Scan(&safe)
		if err != nil || !safe {
			return errors.New("unsafe customer database capability")
		}
		return nil
	})
	if err != nil {
		closePool()
		return nil, errors.New("customer database login must be dedicated to app_customer_auth")
	}
	return &CustomerRunner{runner: r, close: closePool}, nil
}

// Close is idempotent. Bootstrap calls it after all tenant callbacks have drained.
func (r *CustomerRunner) Close() {
	if r != nil {
		r.once.Do(func() {
			if r.close != nil {
				r.close()
			}
		})
	}
}

// InTenant deliberately accepts anonymous authentication work without the staff
// router stamp. Shape validation grants no customer identity: proof-bearing SQL
// commands still verify credentials/token ownership within the selected tenant.
func (r *CustomerRunner) InTenant(ctx context.Context, tenant string, fn func(*db.Queries) error) error {
	if !tenanttx.ValidTenantID(tenant) {
		return tenanttx.ErrInvalidTenant
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if r == nil || r.runner.pool == nil || fn == nil {
		return errors.New("customer database is unavailable")
	}
	var callbackErr error
	err := r.runner.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE app_customer_auth`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('statement_timeout','5000',true),set_config('app.public_hostname','',true)`); err != nil {
			return err
		}
		callbackErr = fn(db.New(tx))
		return callbackErr
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if callbackErr != nil {
		return callbackErr
	}
	if err != nil {
		return errors.New("customer database is unavailable")
	}
	return nil
}
