-- Mutations are inside one rollback-only transaction.
-- Run via psql -X -v ON_ERROR_STOP=1 -f tests/db_isolation.sql.
BEGIN;
INSERT INTO app.tenants(name) VALUES ('smoke-A') RETURNING id AS tenant_a \gset
INSERT INTO app.tenants(name) VALUES ('smoke-B') RETURNING id AS tenant_b \gset
INSERT INTO app.products(tenant_id,product_code)
 VALUES (:'tenant_a','RLS-CHECK-A') RETURNING id AS prod_a \gset
INSERT INTO app.products(tenant_id,product_code)
 VALUES (:'tenant_b','RLS-CHECK-B') RETURNING id AS prod_b \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key)
 VALUES (:'tenant_a',:'prod_a','SHIRT-A','default');

-- Composite FK must reject a variant from a different tenant.
DO $test$
DECLARE t uuid; p uuid;
BEGIN
 SELECT tenant_id,id INTO t,p FROM app.products WHERE product_code='RLS-CHECK-A';
 BEGIN
  INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key)
  SELECT id,p,'INVALID-CROSS-TENANT','default' FROM app.tenants WHERE id <> t LIMIT 1;
  RAISE EXCEPTION 'cross-tenant foreign key unexpectedly accepted';
 EXCEPTION WHEN foreign_key_violation THEN NULL;
 END;
END $test$;

-- An unset app.tenant_id must fail closed: 0 visible rows, INSERT denied.
SET ROLE app_runtime;
DO $test$
DECLARE n integer;
BEGIN
 SELECT COUNT(*) INTO n FROM app.products WHERE product_code LIKE 'RLS-CHECK-%';
 IF n<>0 THEN RAISE EXCEPTION 'missing tenant context leaks % product(s)', n; END IF;
 BEGIN
  INSERT INTO app.products(tenant_id,product_code)
  VALUES ('0199f072-6372-7000-8000-000000000002'::uuid,'SHOULD-BE-DENIED');
  RAISE EXCEPTION 'tenantless INSERT unexpectedly accepted';
 EXCEPTION WHEN insufficient_privilege THEN NULL;
 END;
END $test$;
RESET ROLE;
SELECT set_config('test.foreign_tenant', :'tenant_b',true);
SELECT set_config('app.tenant_id', :'tenant_a',true);
SET ROLE app_runtime;
DO $test$
DECLARE n integer;
BEGIN
 SELECT COUNT(*) INTO n FROM app.products WHERE product_code LIKE 'RLS-CHECK-%';
 IF n<>1 THEN RAISE EXCEPTION 'expected 1 visible product, got %', n; END IF;
 BEGIN
  INSERT INTO app.products(tenant_id,product_code)
  VALUES (current_setting('test.foreign_tenant')::uuid,'SHOULD-BE-DENIED');
  RAISE EXCEPTION 'cross-tenant INSERT unexpectedly accepted';
 EXCEPTION WHEN insufficient_privilege THEN NULL;
 END;
END $test$;
RESET ROLE;
ROLLBACK;
SELECT 'PASS: RLS denies unset and foreign tenants; composite foreign keys' AS result;
