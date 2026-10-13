-- PostgreSQL 18 behavior checks, not just grant metadata.
-- Must run as privileged migrator against an EXPENDABLE database.
BEGIN;
INSERT INTO app.tenants(name) VALUES ('boundary-roles') RETURNING id AS tenant_id \gset
INSERT INTO app.users(email,password_hash) VALUES ('boundary-demo@example.invalid','test-non-password')
 RETURNING id AS user_id \gset
INSERT INTO app.tenant_memberships(tenant_id,user_id,role)
 VALUES (:'tenant_id',:'user_id','viewer');
INSERT INTO app.api_keys(tenant_id,issued_by,key_prefix,key_hash,scopes)
 VALUES (:'tenant_id',:'user_id','test',decode(repeat('31',32),'hex'),ARRAY['site.read']);
INSERT INTO app.organizations(tenant_id,display_name) VALUES (:'tenant_id','Boundary Org')
 RETURNING id AS organization_id \gset
INSERT INTO app.sites(tenant_id,organization_id,name)
 VALUES (:'tenant_id',:'organization_id','Boundary Site') RETURNING id AS site_id \gset
INSERT INTO app.site_domains(tenant_id,site_id,hostname,verified_at,status)
 VALUES (:'tenant_id',:'site_id','verified.example.invalid',now(),'verified');
INSERT INTO app.site_domains(tenant_id,site_id,hostname,status)
 VALUES (:'tenant_id',:'site_id','pending.example.invalid','pending');
INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash)
 VALUES (:'tenant_id',:'site_id',1,'test-renderer','{}',decode(repeat('41',32),'hex'))
 RETURNING id AS release_id \gset
UPDATE app.sites SET published_release_id=:'release_id'
 WHERE tenant_id=:'tenant_id' AND id=:'site_id';
SELECT set_config('test.boundary_tenant',:'tenant_id',true);
SELECT set_config('test.boundary_user',:'user_id',true);

SET ROLE app_auth;
DO $test$
DECLARE n integer;
BEGIN
 SELECT count(*) INTO n FROM app.tenant_memberships
  WHERE tenant_id=current_setting('test.boundary_tenant')::uuid;
 IF n <> 1 THEN RAISE EXCEPTION 'app_auth membership lookup missing: %',n; END IF;
 SELECT count(*) INTO n FROM app.api_keys
  WHERE tenant_id=current_setting('test.boundary_tenant')::uuid;
 IF n <> 1 THEN RAISE EXCEPTION 'app_auth API key lookup missing: %',n; END IF;
 BEGIN
  PERFORM count(*) FROM app.products;
  RAISE EXCEPTION 'app_auth unexpectedly reads products';
 EXCEPTION WHEN insufficient_privilege THEN NULL;
 END;
END $test$;
RESET ROLE;

SELECT set_config('app.public_hostname','verified.example.invalid',true);
SET ROLE app_domain_resolver;
DO $test$
DECLARE n integer;
BEGIN
 SELECT count(*) INTO n FROM app.site_domains d
  JOIN app.sites s ON s.tenant_id=d.tenant_id AND s.id=d.site_id
  WHERE d.tenant_id=current_setting('test.boundary_tenant')::uuid;
 IF n <> 1 THEN RAISE EXCEPTION 'expected 1 public verified+published domain, got %',n; END IF;
 PERFORM set_config('app.public_hostname','unknown.example.invalid',true);
 SELECT count(*) INTO n FROM app.site_domains d JOIN app.sites s
  ON (s.tenant_id,s.id)=(d.tenant_id,d.site_id);
 IF n <> 0 THEN RAISE EXCEPTION 'unknown public hostname exposed % rows',n; END IF;
 PERFORM set_config('app.public_hostname','verified.example.invalid',true);
 BEGIN
  PERFORM count(*) FROM app.users;
  RAISE EXCEPTION 'domain resolver unexpectedly reads users';
 EXCEPTION WHEN insufficient_privilege THEN NULL;
 END;
 BEGIN
  PERFORM count(*) FROM app.media_assets;
  RAISE EXCEPTION 'domain resolver unexpectedly reads media';
 EXCEPTION WHEN insufficient_privilege THEN NULL;
 END;
END $test$;
RESET ROLE;
ROLLBACK;
SELECT 'PASS: auth lookup and verified/published domain behavior' AS result;
