-- Publisher cannot seal using an old snapshot that misses committed children.
-- Dependencies: migration010 publisher capability. Called by check_pg.
-- READ COMMITTED is the publish contract; ledger isolation is unaffected.
\if :{?serializable}
\else
\set serializable false
\endif
BEGIN;
INSERT INTO app.tenants(name) VALUES('W2a-isolation') RETURNING id AS tenant_id \gset
INSERT INTO app.organizations(tenant_id,display_name) VALUES(:'tenant_id','W2a') RETURNING id AS org_id \gset
INSERT INTO app.sites(tenant_id,organization_id,name) VALUES(:'tenant_id',:'org_id','W2a') RETURNING id AS site_id \gset
INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash)
 VALUES(:'tenant_id',:'site_id',1,'test','{}',decode(repeat('bb',32),'hex')) RETURNING id AS release_id \gset
SELECT set_config('test.iso.tenant',:'tenant_id',false);
SELECT set_config('test.iso.site',:'site_id',false);
SELECT set_config('test.iso.release',:'release_id',false);
COMMIT;
\if :serializable
BEGIN ISOLATION LEVEL SERIALIZABLE;
\else
BEGIN ISOLATION LEVEL REPEATABLE READ;
\endif
SELECT set_config('app.tenant_id',:'tenant_id',true);
SET LOCAL ROLE app_publisher;
DO $test$
BEGIN
 BEGIN
  PERFORM app.publish_release(current_setting('test.iso.tenant')::uuid,
   current_setting('test.iso.site')::uuid,current_setting('test.iso.release')::uuid,NULL);
  RAISE EXCEPTION 'W2A_PUBLISH_OLD_SNAPSHOT_ACCEPTED';
 EXCEPTION WHEN invalid_transaction_state THEN NULL; END;
 BEGIN
  PERFORM app.seal_release(current_setting('test.iso.tenant')::uuid,
   current_setting('test.iso.site')::uuid,current_setting('test.iso.release')::uuid);
  RAISE EXCEPTION 'W2A_SEAL_OLD_SNAPSHOT_ACCEPTED';
 EXCEPTION WHEN invalid_transaction_state THEN NULL; END;
END $test$;
ROLLBACK;
BEGIN;
DELETE FROM app.site_releases WHERE tenant_id=:'tenant_id' AND site_id=:'site_id' AND id=:'release_id';
DELETE FROM app.sites WHERE tenant_id=:'tenant_id' AND id=:'site_id';
DELETE FROM app.organizations WHERE tenant_id=:'tenant_id' AND id=:'org_id';
DELETE FROM app.tenants WHERE id=:'tenant_id';
COMMIT;
SELECT 'PASS: non-READ-COMMITTED seal/publish rejected' AS result;
