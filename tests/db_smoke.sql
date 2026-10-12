-- Run AFTER all migrations, as postgres superuser, on an expendable test DB.
DO $$ BEGIN
 IF (SELECT count(*) FROM pg_tables WHERE schemaname='app') < 40 THEN
  RAISE EXCEPTION 'incomplete app schema (expected >=40 tables)';
 END IF;
 IF EXISTS (
  SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname='app' AND c.relkind='r'
   AND EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid=c.oid AND a.attname='tenant_id' AND NOT a.attisdropped)
   AND (NOT c.relrowsecurity OR NOT c.relforcerowsecurity)
 ) THEN RAISE EXCEPTION 'tenant table missing ENABLE/FORCE RLS'; END IF;
 IF has_table_privilege('app_runtime','app.users','SELECT')
  OR has_table_privilege('app_runtime','app.api_keys','SELECT') THEN
    RAISE EXCEPTION 'runtime can read credentials';
 END IF;
END $$;

BEGIN;
DO $$
DECLARE t1 uuid; t2 uuid; p uuid;
BEGIN
 INSERT INTO app.tenants(name) VALUES ('smoke-A') RETURNING id INTO t1;
 INSERT INTO app.tenants(name) VALUES ('smoke-B') RETURNING id INTO t2;
 INSERT INTO app.products(tenant_id,product_code) VALUES (t1,'shirt') RETURNING id INTO p;
 BEGIN
  INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key)
    VALUES (t2,p,'SKU-B','default');
  RAISE EXCEPTION 'cross-tenant FK was accepted';
 EXCEPTION WHEN foreign_key_violation THEN
  NULL;
 END;
END $$;
ROLLBACK;

-- RLS behavior: grant is inherited from app_runtime; database connection must
-- set app.tenant_id in a transaction using server-authenticated tenant identity.
BEGIN;
INSERT INTO app.tenants(name) VALUES ('rls-A') RETURNING id AS tenant_a \gset
INSERT INTO app.tenants(name) VALUES ('rls-B') RETURNING id AS tenant_b \gset
INSERT INTO app.products(tenant_id,product_code) VALUES (:'tenant_a','RLS-CHECK-A');
INSERT INTO app.products(tenant_id,product_code) VALUES (:'tenant_b','RLS-CHECK-B');
SET ROLE app_runtime;
SELECT set_config('app.tenant_id', :'tenant_a', true);
DO $$ DECLARE n int; BEGIN
 SELECT COUNT(*) INTO n FROM app.products WHERE product_code LIKE 'RLS-CHECK-%';
 IF n<>1 THEN RAISE EXCEPTION 'expected 1 visible product, got %', n; END IF;
END $$;
RESET ROLE;
ROLLBACK;
