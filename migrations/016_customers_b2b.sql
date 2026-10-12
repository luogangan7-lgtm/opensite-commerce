-- Customer owns customers/credentials/addresses; B2B owns buyer companies/roles/members.
-- Depends on identity 001 and money_pricing; called by the privileged migrator.
-- Tenant keys never reference employee users or seller organizations. Runtime reads
-- nonsecret data only; a separate scoped capability registers/verifies customers.
-- Migrator default privileges would pre-grant the tables created below before the
-- narrow grants; refuse them (global entries only beyond the built-in defaults).
DO $default_privileges$
DECLARE found text;
BEGIN
 SELECT string_agg(DISTINCT format('%s %s on objtype %s %s',
   CASE WHEN a.grantee=0 THEN 'PUBLIC' ELSE a.grantee::regrole::text END,a.privilege_type,
   d.defaclobjtype,CASE WHEN d.defaclnamespace=0 THEN 'globally' ELSE 'in schema app' END),'; ')
  INTO found FROM pg_default_acl d CROSS JOIN LATERAL (
   SELECT grantee,privilege_type FROM aclexplode(d.defaclacl)
   EXCEPT SELECT grantee,privilege_type FROM aclexplode(acldefault(d.defaclobjtype,d.defaclrole))
    WHERE d.defaclnamespace=0) a
  WHERE d.defaclrole=current_user::regrole AND d.defaclnamespace IN (0,'app'::regnamespace)
   AND (a.grantee=0 OR a.grantee IN (SELECT oid FROM pg_roles WHERE starts_with(rolname,'app_')));
 IF found IS NOT NULL THEN
  RAISE EXCEPTION 'CUSTOMERS_DEFAULT_PRIVILEGES: migrator default privileges grant %',found
   USING HINT='ALTER DEFAULT PRIVILEGES ... REVOKE these grants for the migrator role, then rerun.';
 END IF;
END $default_privileges$;
DO $customer_auth_role$
DECLARE m record; t record; columns text; residual text;
BEGIN
 IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='app_customer_auth') THEN
  CREATE ROLE app_customer_auth NOLOGIN;
 END IF;
 ALTER ROLE app_customer_auth NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB
  NOCREATEROLE NOREPLICATION NOINHERIT;
 -- pg_auth_members is cluster-wide. Revoke every outbound membership per actual
 -- grantor: a superuser's plain REVOKE skips grants made by a non-superuser.
 FOR m IN SELECT parent.rolname parent_name,grantor.rolname grantor_name
  FROM pg_auth_members a JOIN pg_roles parent ON parent.oid=a.roleid
  JOIN pg_roles grantor ON grantor.oid=a.grantor
  WHERE a.member='app_customer_auth'::regrole LOOP
  EXECUTE format('REVOKE %I FROM app_customer_auth GRANTED BY %I CASCADE',m.parent_name,m.grantor_name);
 END LOOP;
 -- Inbound memberships are NOT stripped: dedicated customer-auth logins are
 -- provisioned after this migration (possibly for another database on this
 -- cluster) and are legitimate. Only other repository app_* roles never are.
 FOR m IN SELECT child.rolname child_name,grantor.rolname grantor_name
  FROM pg_auth_members a JOIN pg_roles child ON child.oid=a.member
  JOIN pg_roles grantor ON grantor.oid=a.grantor
  WHERE a.roleid='app_customer_auth'::regrole AND child.rolname LIKE 'app\_%' LOOP
  EXECUTE format('REVOKE app_customer_auth FROM %I GRANTED BY %I CASCADE',m.child_name,m.grantor_name);
 END LOOP;
 REVOKE ALL ON SCHEMA app FROM app_customer_auth;
 REVOKE ALL ON ALL TABLES IN SCHEMA app FROM app_customer_auth;
 REVOKE ALL ON ALL FUNCTIONS IN SCHEMA app FROM app_customer_auth;
 -- Table REVOKE does not remove independently granted column capabilities.
 FOR t IN SELECT c.oid,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname='app' AND c.relkind IN ('r','p') LOOP
  SELECT string_agg(format('%I',attname),',' ORDER BY attnum) INTO columns
   FROM pg_attribute WHERE attrelid=t.oid AND attnum>0 AND NOT attisdropped;
  EXECUTE format('REVOKE SELECT (%s),INSERT (%s),UPDATE (%s),REFERENCES (%s) ON app.%I FROM app_customer_auth',
   columns,columns,columns,columns,t.relname);
 END LOOP;
 -- Owner REVOKE cannot remove privileges a third party granted WITH GRANT
 -- OPTION. Fail closed so the operator removes them explicitly.
 SELECT string_agg(format('%s by %s',obj,g.rolname),'; ') INTO residual FROM (
  SELECT 'schema app' obj,(aclexplode(nspacl)).* FROM pg_namespace WHERE nspname='app'
  UNION ALL SELECT format('table app.%I',c.relname),(aclexplode(c.relacl)).* FROM pg_class c
   WHERE c.relnamespace='app'::regnamespace
  UNION ALL SELECT format('column app.%I.%I',c.relname,a.attname),(aclexplode(a.attacl)).*
   FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid WHERE c.relnamespace='app'::regnamespace
  UNION ALL SELECT format('function app.%s',p.oid::regprocedure),(aclexplode(p.proacl)).* FROM pg_proc p
   WHERE p.pronamespace='app'::regnamespace
 ) acl JOIN pg_roles g ON g.oid=acl.grantor WHERE acl.grantee='app_customer_auth'::regrole;
 SELECT concat_ws('; ',residual,string_agg(format('membership in %s by %s',r.rolname,g.rolname),'; '))
  INTO residual FROM pg_auth_members a JOIN pg_roles r ON r.oid=a.roleid
  JOIN pg_roles g ON g.oid=a.grantor WHERE a.member='app_customer_auth'::regrole;
 SELECT concat_ws('; ',NULLIF(residual,''),string_agg(format('effective membership in %s',rolname),'; '))
  INTO residual FROM pg_roles WHERE rolname LIKE 'app\_%' AND rolname<>'app_customer_auth'
  AND pg_has_role('app_customer_auth',oid,'MEMBER');
 IF NULLIF(residual,'') IS NOT NULL THEN
  RAISE EXCEPTION 'CUSTOMERS_AUTH_RESIDUAL_PRIVILEGE: app_customer_auth still holds %',residual
   USING HINT='Revoke each listed privilege as its grantor (or REVOKE ... GRANTED BY), then rerun.';
 END IF;
 -- Inbound members outside app_* are kept, so an internal role can still reach
 -- the capability through a bridge group. Fail closed for the roles that
 -- tests/db_role_matrix.sql forbids from holding app_customer_auth.
 SELECT string_agg(rolname,', ' ORDER BY rolname) INTO residual FROM pg_roles
  WHERE rolname=ANY(ARRAY['app_runtime','app_auth','app_domain_resolver',
   'app_domain_verifier','app_media_service','app_publisher','app_publish_executor',
   'app_outbox_worker','app_reliability_executor','app_inventory_manager','app_inventory_executor'])
  AND (pg_has_role(oid,'app_customer_auth'::regrole,'MEMBER')
   OR pg_has_role(oid,'app_customer_auth'::regrole,'SET')
   OR pg_has_role(oid,'app_customer_auth'::regrole,'USAGE'));
 IF residual IS NOT NULL THEN
  RAISE EXCEPTION 'CUSTOMERS_AUTH_FORBIDDEN_PATH: % can still reach app_customer_auth',residual
   USING HINT='Remove the intermediate role membership chain (e.g. a legacy bridge group), then rerun.';
 END IF;
END $customer_auth_role$;
GRANT USAGE ON SCHEMA app TO app_customer_auth;

CREATE TABLE app.customers (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 id uuid NOT NULL DEFAULT uuidv7(),
 email text NOT NULL,
 email_normalized text GENERATED ALWAYS AS
  (lower(regexp_replace(email,'^[[:space:]]+|[[:space:]]+$','','g'))) STORED,
 display_name text NOT NULL DEFAULT '',
 status text NOT NULL DEFAULT 'active',
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,id),
 CONSTRAINT customers_tenant_email_unique UNIQUE(tenant_id,email_normalized),
 CONSTRAINT customers_email_check CHECK(length(email)<=320 AND length(email_normalized)<=320
  AND email_normalized ~ '^[^[:space:]@]+@[^[:space:]@]+$'),
 CONSTRAINT customers_status_check CHECK(status IN ('active','disabled'))
);
-- Guest-to-registered conversion must first prove ownership of the customer
-- email in the future authentication flow; the FK alone is not identity proof.
CREATE TABLE app.customer_credentials (
 tenant_id uuid NOT NULL,
 customer_id uuid NOT NULL,
 password_hash text NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,customer_id),
 CONSTRAINT customer_credentials_customer_fk FOREIGN KEY(tenant_id,customer_id)
  REFERENCES app.customers(tenant_id,id),
 -- Envelope validation only: the later auth module must bound/verify decoded
 -- Argon2 parameters, salt/hash lengths and password work. No plaintext allowed.
 CONSTRAINT customer_credentials_password_hash_check CHECK(length(password_hash)<=1024
  AND password_hash ~ '^\$argon2id\$v=19\$m=[1-9][0-9]*,t=[1-9][0-9]*,p=[1-9][0-9]*\$[A-Za-z0-9+/]+\$[A-Za-z0-9+/]+$')
);
CREATE TABLE app.customer_addresses (
 tenant_id uuid NOT NULL,
 id uuid NOT NULL DEFAULT uuidv7(),
 customer_id uuid NOT NULL,
 recipient text NOT NULL CHECK(btrim(recipient)<>''),
 address_line1 text NOT NULL CHECK(btrim(address_line1)<>''),
 address_line2 text NOT NULL DEFAULT '',
 city text NOT NULL CHECK(btrim(city)<>''),
 subdivision text,
 postal_code text NOT NULL DEFAULT '',
 country_code text NOT NULL,
 PRIMARY KEY(tenant_id,id),
 CONSTRAINT customer_addresses_customer_fk FOREIGN KEY(tenant_id,customer_id)
  REFERENCES app.customers(tenant_id,id),
 CONSTRAINT customer_addresses_country_code_check CHECK(country_code = ANY(ARRAY[
  'AD','AE','AF','AG','AI','AL','AM','AO','AQ','AR','AS','AT','AU','AW','AX',
  'AZ','BA','BB','BD','BE','BF','BG','BH','BI','BJ','BL','BM','BN','BO','BQ',
  'BR','BS','BT','BV','BW','BY','BZ','CA','CC','CD','CF','CG','CH','CI','CK',
  'CL','CM','CN','CO','CR','CU','CV','CW','CX','CY','CZ','DE','DJ','DK','DM',
  'DO','DZ','EC','EE','EG','EH','ER','ES','ET','FI','FJ','FK','FM','FO','FR',
  'GA','GB','GD','GE','GF','GG','GH','GI','GL','GM','GN','GP','GQ','GR','GS',
  'GT','GU','GW','GY','HK','HM','HN','HR','HT','HU','ID','IE','IL','IM','IN',
  'IO','IQ','IR','IS','IT','JE','JM','JO','JP','KE','KG','KH','KI','KM','KN',
  'KP','KR','KW','KY','KZ','LA','LB','LC','LI','LK','LR','LS','LT','LU','LV',
  'LY','MA','MC','MD','ME','MF','MG','MH','MK','ML','MM','MN','MO','MP','MQ',
  'MR','MS','MT','MU','MV','MW','MX','MY','MZ','NA','NC','NE','NF','NG','NI',
  'NL','NO','NP','NR','NU','NZ','OM','PA','PE','PF','PG','PH','PK','PL','PM',
  'PN','PR','PS','PT','PW','PY','QA','RE','RO','RS','RU','RW','SA','SB','SC',
  'SD','SE','SG','SH','SI','SJ','SK','SL','SM','SN','SO','SR','SS','ST','SV',
  'SX','SY','SZ','TC','TD','TF','TG','TH','TJ','TK','TL','TM','TN','TO','TR',
  'TT','TV','TW','TZ','UA','UG','UM','US','UY','UZ','VA','VC','VE','VG','VI',
  'VN','VU','WF','WS','YE','YT','ZA','ZM','ZW']::text[]))
);
CREATE INDEX customer_addresses_customer ON app.customer_addresses(tenant_id,customer_id);
CREATE TABLE app.buyer_companies (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 id uuid NOT NULL DEFAULT uuidv7(),
 name text NOT NULL CHECK(btrim(name)<>''),
 status text NOT NULL DEFAULT 'active' CHECK(status IN ('active','disabled')),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,id)
);
CREATE TABLE app.buyer_roles (
 -- Automatically provisioned vocabulary must not retain an empty tenant.
 tenant_id uuid NOT NULL REFERENCES app.tenants(id) ON DELETE CASCADE,
 code text NOT NULL,
 PRIMARY KEY(tenant_id,code),
 CONSTRAINT buyer_roles_code_check CHECK(code IN ('purchaser','approver','admin'))
);
INSERT INTO app.buyer_roles(tenant_id,code)
 SELECT t.id,r.code FROM app.tenants t CROSS JOIN
  (VALUES('purchaser'),('approver'),('admin')) AS r(code);
CREATE TABLE app.buyer_company_members (
 tenant_id uuid NOT NULL,
 buyer_company_id uuid NOT NULL,
 customer_id uuid NOT NULL,
 role_code text NOT NULL,
 status text NOT NULL DEFAULT 'active',
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,buyer_company_id,customer_id),
 CONSTRAINT buyer_company_members_company_fk FOREIGN KEY(tenant_id,buyer_company_id)
  REFERENCES app.buyer_companies(tenant_id,id),
 CONSTRAINT buyer_company_members_customer_fk FOREIGN KEY(tenant_id,customer_id)
  REFERENCES app.customers(tenant_id,id),
 CONSTRAINT buyer_company_members_role_fk FOREIGN KEY(tenant_id,role_code)
  REFERENCES app.buyer_roles(tenant_id,code),
 CONSTRAINT buyer_company_members_status_check CHECK(status IN ('active','revoked'))
);
CREATE INDEX buyer_company_members_customer ON app.buyer_company_members(tenant_id,customer_id);

-- Unresolved legacy company-specific lists must be reconciled explicitly; do
-- not silently widen their audience or invent purchasing companies on upgrade.
DO $pricing_targets$
BEGIN
 IF EXISTS(SELECT FROM app.price_lists l LEFT JOIN app.buyer_companies c
  ON (c.tenant_id,c.id)=(l.tenant_id,l.buyer_company_id)
  WHERE l.buyer_company_id IS NOT NULL AND c.id IS NULL) THEN
  RAISE EXCEPTION 'CUSTOMERS_UPGRADE_BUYER_COMPANY_RECONCILIATION_REQUIRED'
   USING ERRCODE='23503', HINT='Reconcile unresolved company-specific price lists before retrying this forward migration.';
 END IF;
END $pricing_targets$;
ALTER TABLE app.price_lists ADD CONSTRAINT price_lists_buyer_company_fk
 FOREIGN KEY(tenant_id,buyer_company_id) REFERENCES app.buyer_companies(tenant_id,id);

DO $customer_rls$
DECLARE table_name text; role_name text;
BEGIN
 FOREACH table_name IN ARRAY ARRAY['customers','customer_credentials','customer_addresses',
  'buyer_companies','buyer_roles','buyer_company_members'] LOOP
  EXECUTE format('ALTER TABLE app.%I ENABLE ROW LEVEL SECURITY',table_name);
  EXECUTE format('ALTER TABLE app.%I FORCE ROW LEVEL SECURITY',table_name);
  -- Drop anything default privileges attached at CREATE before the narrow grants.
  EXECUTE format('REVOKE ALL ON app.%I FROM PUBLIC',table_name);
  FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
   EXECUTE format('REVOKE ALL ON app.%I FROM %I',table_name,role_name);
  END LOOP;
  IF table_name='customer_credentials' THEN
   EXECUTE format('CREATE POLICY tenant_scope ON app.%I TO app_customer_auth USING(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',table_name);
  ELSE
   EXECUTE format('CREATE POLICY tenant_scope ON app.%I TO app_runtime USING(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',table_name);
  END IF;
 END LOOP;
END $customer_rls$;
CREATE POLICY customer_auth_scope ON app.customers TO app_customer_auth
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
GRANT SELECT ON app.customers,app.customer_addresses,app.buyer_companies,
 app.buyer_roles,app.buyer_company_members TO app_runtime;
GRANT SELECT ON app.customers,app.customer_credentials TO app_customer_auth;
GRANT INSERT(tenant_id,id,email,display_name) ON app.customers TO app_customer_auth;
GRANT INSERT(tenant_id,customer_id,password_hash) ON app.customer_credentials TO app_customer_auth;
GRANT UPDATE(password_hash) ON app.customer_credentials TO app_customer_auth;

-- Trusted tenant provisioners already manage tenant-local reference data.
-- INVOKER deliberately keeps that authority: a less-privileged future creator
-- must acquire explicit provisioning rights, or the whole tenant insert fails.
CREATE FUNCTION app.buyer_roles_seed_for_tenant() RETURNS trigger
LANGUAGE plpgsql SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $fn$
BEGIN
 INSERT INTO app.buyer_roles(tenant_id,code)
 VALUES(NEW.id,'purchaser'),(NEW.id,'approver'),(NEW.id,'admin');
 RETURN NEW;
END $fn$;

-- Password changes update metadata without giving the authenticator permission
-- to choose timestamps. The trigger condition excludes identical-hash updates.
CREATE FUNCTION app.customer_credentials_touch_updated_at() RETURNS trigger
LANGUAGE plpgsql SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $fn$
BEGIN
 NEW.updated_at:=clock_timestamp();
 RETURN NEW;
END $fn$;

REVOKE ALL ON FUNCTION app.buyer_roles_seed_for_tenant(),
 app.customer_credentials_touch_updated_at() FROM PUBLIC;
DO $trigger_capabilities$
DECLARE role_name text;
BEGIN
 FOR role_name IN SELECT rolname FROM pg_roles WHERE starts_with(rolname,'app_') LOOP
  EXECUTE format('REVOKE ALL ON FUNCTION app.buyer_roles_seed_for_tenant(), app.customer_credentials_touch_updated_at() FROM %I',role_name);
 END LOOP;
END $trigger_capabilities$;

CREATE TRIGGER buyer_roles_seed_for_tenant AFTER INSERT ON app.tenants
FOR EACH ROW EXECUTE FUNCTION app.buyer_roles_seed_for_tenant();
CREATE TRIGGER customer_credentials_touch_updated_at
BEFORE UPDATE OF password_hash ON app.customer_credentials
FOR EACH ROW WHEN(OLD.password_hash IS DISTINCT FROM NEW.password_hash)
EXECUTE FUNCTION app.customer_credentials_touch_updated_at();
