//go:build pgx

// Responsibility: dedicate anonymous reads to a least-privilege login and transaction.
// Dependencies: pgxadapter pool; callers: publish store. Invariants: no admin fallback,
// read-only repeatable snapshot, local host scope/role reset at transaction boundary.
package pgxadapter

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
)

func OpenPublic(ctx context.Context, dsn string) (Runner, func(), error) {
	r, close, err := Open(ctx, dsn)
	if err != nil {
		return Runner{}, nil, err
	}
	err = r.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var safe bool
		err := tx.QueryRow(ctx, `SELECT NOT (r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication)
   AND r.rolcanlogin AND r.rolinherit AND current_user=session_user
   AND EXISTS (SELECT FROM pg_roles p WHERE p.rolname='app_domain_resolver' AND NOT (p.rolcanlogin OR p.rolsuper OR p.rolbypassrls OR p.rolcreatedb OR p.rolcreaterole OR p.rolreplication OR p.rolinherit))
   AND NOT EXISTS (SELECT FROM pg_auth_members m WHERE m.member='app_domain_resolver'::regrole)
   AND NOT EXISTS (SELECT FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='app' AND c.relname IN ('site_domains','sites','site_releases','site_release_routes','page_revisions','products','product_variants','product_options','product_option_values','variant_option_values','price_lists','price_entries','stock_levels') AND NOT (c.relrowsecurity AND c.relforcerowsecurity))
   AND EXISTS (SELECT FROM pg_auth_members m JOIN pg_roles p ON p.oid=m.roleid WHERE m.member=r.oid AND p.rolname='app_domain_resolver' AND m.set_option AND NOT m.admin_option)
   AND NOT EXISTS (SELECT FROM pg_auth_members m WHERE m.member=r.oid AND m.admin_option)
   AND NOT EXISTS (SELECT FROM pg_auth_members m JOIN pg_roles p ON p.oid=m.roleid WHERE m.member=r.oid AND p.rolname<>'app_domain_resolver')
   AND NOT EXISTS (SELECT FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='app' AND c.relowner=r.oid)
   AND NOT EXISTS (SELECT FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
    JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped
    WHERE n.nspname='app' AND c.relkind IN ('r','p')
     AND has_column_privilege(session_user,c.oid,a.attnum,'SELECT')
     AND NOT a.attname::text=ANY(CASE c.relname
      WHEN 'site_domains' THEN ARRAY['tenant_id','site_id','normalized_hostname','status','verified_at']
      WHEN 'sites' THEN ARRAY['tenant_id','id','published_release_id','default_locale']
      WHEN 'site_releases' THEN ARRAY['tenant_id','site_id','id','sealed','manifest']
      WHEN 'site_release_routes' THEN ARRAY['tenant_id','site_id','release_id','locale','path','page_id','page_revision']
      WHEN 'page_revisions' THEN ARRAY['tenant_id','site_id','page_id','locale','revision','document']
      WHEN 'products' THEN ARRAY['tenant_id','id','lifecycle_status']
      WHEN 'product_variants' THEN ARRAY['tenant_id','id','product_id','sku','lifecycle_status']
      WHEN 'product_options' THEN ARRAY['tenant_id','product_id','id','code']
      WHEN 'product_option_values' THEN ARRAY['tenant_id','product_id','option_id','id','label']
      WHEN 'variant_option_values' THEN ARRAY['tenant_id','product_id','variant_id','option_id','value_id']
      WHEN 'price_lists' THEN ARRAY['tenant_id','id','currency','status','starts_at','ends_at','priority','audience']
      WHEN 'price_entries' THEN ARRAY['tenant_id','price_list_id','sku_id','min_quantity','unit_amount']
      WHEN 'stock_levels' THEN ARRAY['tenant_id','sku_id','in_stock']
      ELSE ARRAY[]::text[] END))
   AND NOT has_schema_privilege(session_user,'app','CREATE')
   AND NOT EXISTS (SELECT FROM pg_proc f JOIN pg_namespace n ON n.oid=f.pronamespace WHERE n.nspname='app' AND has_function_privilege(session_user,f.oid,'EXECUTE'))
   AND NOT EXISTS (SELECT FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='app' AND c.relkind IN ('r','p') AND
    (has_table_privilege(session_user,c.oid,'INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER,MAINTAIN')
     OR has_any_column_privilege(session_user,c.oid,'INSERT,UPDATE,REFERENCES')
     OR (c.relname NOT IN ('site_domains','sites','site_releases','site_release_routes','page_revisions','products','product_variants','product_options','product_option_values','variant_option_values','price_lists','price_entries','stock_levels') AND has_any_column_privilege(session_user,c.oid,'SELECT'))))
   FROM pg_roles r WHERE r.rolname=session_user`).Scan(&safe)
		if err != nil {
			return err
		}
		if !safe {
			return errors.New("public database login must be dedicated to app_domain_resolver")
		}
		return nil
	})
	if err != nil {
		close()
		return Runner{}, nil, err
	}
	return r, close, nil
}
func (r Runner) InPublicRead(ctx context.Context, host string, fn func(context.Context, pgx.Tx) error) error {
	if r.pool == nil {
		return errors.New("pgx pool is nil")
	}
	return pgx.BeginTxFunc(ctx, r.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE app_domain_resolver`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id','',true),set_config('app.public_hostname',$1,true),set_config('statement_timeout','5000',true)`, host); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}
