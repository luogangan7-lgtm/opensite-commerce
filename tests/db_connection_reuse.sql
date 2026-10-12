-- Responsibility: prove transaction-local tenant context expires on one connection.
-- Dependencies: app.tenants/products, app_runtime and the actual tenant_scope RLS.
-- Caller: one psql -X connection as privileged owner on an expendable database.
-- Invariant: a committed nonempty witness survives both transaction boundaries;
-- tenantless reuse has empty context, zero rows and an RLS-denied INSERT.
-- Gate injection: -v inject_session_leak=true changes ONE set_config to session
-- scope; the following assertion must report context survival/data leakage.
-- On failure the runner must discard its disposable DB (witness is committed).
\set ON_ERROR_STOP on
\if :{?inject_session_leak}
\else
 \set inject_session_leak false
\endif
RESET ROLE;
RESET app.tenant_id;
SELECT gen_random_uuid() AS witness_tenant,gen_random_uuid() AS witness_product \gset
BEGIN;
INSERT INTO app.tenants(id,name) VALUES (:'witness_tenant','connection-reuse-' || :'witness_tenant');
INSERT INTO app.products(tenant_id,id,product_code)
 VALUES (:'witness_tenant',:'witness_product','connection-reuse-' || :'witness_product');
COMMIT;
-- Test identifiers are session-local harness state, never authorization context.
SELECT set_config('test.reuse_tenant',:'witness_tenant',false);
SELECT set_config('test.reuse_product',:'witness_product',false);
DO $test$
BEGIN
 IF (SELECT count(*) FROM app.products WHERE tenant_id=current_setting('test.reuse_tenant')::uuid
     AND id=current_setting('test.reuse_product')::uuid) <> 1 THEN
  RAISE EXCEPTION 'committed witness missing before connection reuse';
 END IF;
END $test$;

BEGIN;
SELECT set_config('app.tenant_id',:'witness_tenant',NOT :'inject_session_leak'::boolean);
SET LOCAL ROLE app_runtime;
DO $test$
BEGIN
 IF current_user <> 'app_runtime' THEN RAISE EXCEPTION 'wrong runtime role'; END IF;
 IF (SELECT count(*) FROM app.products WHERE tenant_id=current_setting('test.reuse_tenant')::uuid
     AND id=current_setting('test.reuse_product')::uuid) <> 1 THEN
  RAISE EXCEPTION 'witness not visible in tenant COMMIT transaction';
 END IF;
END $test$;
COMMIT;

BEGIN;
SET LOCAL ROLE app_runtime;
DO $test$
BEGIN
 IF coalesce(current_setting('app.tenant_id',true),'') <> '' THEN
  RAISE EXCEPTION 'tenant setting survived COMMIT; connection context leakage';
 END IF;
 IF (SELECT count(*) FROM app.products) <> 0 THEN
  RAISE EXCEPTION 'tenant data leaked after COMMIT';
 END IF;
 BEGIN
  INSERT INTO app.products(tenant_id,product_code)
   VALUES (current_setting('test.reuse_tenant')::uuid,'denied-' || gen_random_uuid());
  RAISE EXCEPTION 'tenantless INSERT accepted after COMMIT';
 EXCEPTION WHEN insufficient_privilege THEN NULL;
 END;
END $test$;
ROLLBACK;

BEGIN;
SELECT set_config('app.tenant_id',:'witness_tenant',true);
SET LOCAL ROLE app_runtime;
DO $test$
BEGIN
 IF current_user <> 'app_runtime' THEN RAISE EXCEPTION 'wrong runtime role'; END IF;
 IF (SELECT count(*) FROM app.products WHERE tenant_id=current_setting('test.reuse_tenant')::uuid
     AND id=current_setting('test.reuse_product')::uuid) <> 1 THEN
  RAISE EXCEPTION 'witness not visible in tenant ROLLBACK transaction';
 END IF;
END $test$;
ROLLBACK;

BEGIN;
SET LOCAL ROLE app_runtime;
DO $test$
BEGIN
 IF coalesce(current_setting('app.tenant_id',true),'') <> '' THEN
  RAISE EXCEPTION 'tenant setting survived ROLLBACK; connection context leakage';
 END IF;
 IF (SELECT count(*) FROM app.products) <> 0 THEN
  RAISE EXCEPTION 'tenant data leaked after ROLLBACK';
 END IF;
 BEGIN
  INSERT INTO app.products(tenant_id,product_code)
   VALUES (current_setting('test.reuse_tenant')::uuid,'denied-' || gen_random_uuid());
  RAISE EXCEPTION 'tenantless INSERT accepted after ROLLBACK';
 EXCEPTION WHEN insufficient_privilege THEN NULL;
 END;
END $test$;
ROLLBACK;

-- SET LOCAL ROLE has ended; only the owner removes these precise UUID fixtures.
BEGIN;
DELETE FROM app.products WHERE tenant_id=:'witness_tenant' AND id=:'witness_product';
DELETE FROM app.tenants WHERE id=:'witness_tenant';
DO $test$
BEGIN
 IF EXISTS (SELECT 1 FROM app.products WHERE tenant_id=current_setting('test.reuse_tenant')::uuid)
    OR EXISTS (SELECT 1 FROM app.tenants WHERE id=current_setting('test.reuse_tenant')::uuid) THEN
  RAISE EXCEPTION 'connection reuse fixture cleanup incomplete';
 END IF;
END $test$;
COMMIT;
RESET app.tenant_id;
RESET test.reuse_tenant;
RESET test.reuse_product;
SELECT 'PASS: same connection COMMIT/ROLLBACK context expiry, witness and tenantless denial' AS result;
