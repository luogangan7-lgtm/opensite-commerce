-- Responsibility: tenant-owned regional Tax configuration; no tax filing/payment logic.
-- Dependencies: all preceding forward migrations, app.tenants and app_runtime.
-- Caller: privileged migration runner only. New IDs have tenant-composite keys/FKs;
-- settings use their natural singleton tenant key. Runtime cannot change identities.
-- Invariants: [from,until) rate windows never overlap per zone/category, including
-- concurrent writes; exact subdivision mapping is unique, and overrides country.
-- Upgrade: additive, preserves existing tenants/data; absent settings are not
-- silently materialized. Application callers must explicitly load/configure mode.
-- Rollback: forward repair only; this DDL is committed atomically by migrate.sh.
CREATE EXTENSION IF NOT EXISTS btree_gist WITH SCHEMA public;

CREATE TABLE app.tax_zones (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 id uuid NOT NULL DEFAULT uuidv7(),
 name text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 128),
 PRIMARY KEY(tenant_id,id)
);
-- A set is normalized so duplicate/ambiguous destination membership is rejected.
-- Empty subdivision means the country-wide fallback. ISO 3166-2 membership is
-- not asserted: only country prefix and 1..3 uppercase alphanumeric suffix.
CREATE TABLE app.tax_zone_locations (
 tenant_id uuid NOT NULL,
 zone_id uuid NOT NULL,
 country_code text NOT NULL,
 subdivision text NOT NULL DEFAULT '',
 PRIMARY KEY(tenant_id,zone_id,country_code,subdivision),
 CONSTRAINT tax_location_unique UNIQUE(tenant_id,country_code,subdivision),
 CONSTRAINT tax_location_zone_fk FOREIGN KEY(tenant_id,zone_id) REFERENCES app.tax_zones(tenant_id,id),
 CONSTRAINT tax_country_assigned CHECK(country_code IN ('AD','AE','AF','AG','AI','AL','AM','AO','AQ','AR','AS','AT','AU','AW','AX','AZ','BA','BB','BD','BE','BF','BG','BH','BI','BJ','BL','BM','BN','BO','BQ','BR','BS','BT','BV','BW','BY','BZ','CA','CC','CD','CF','CG','CH','CI','CK','CL','CM','CN','CO','CR','CU','CV','CW','CX','CY','CZ','DE','DJ','DK','DM','DO','DZ','EC','EE','EG','EH','ER','ES','ET','FI','FJ','FK','FM','FO','FR','GA','GB','GD','GE','GF','GG','GH','GI','GL','GM','GN','GP','GQ','GR','GS','GT','GU','GW','GY','HK','HM','HN','HR','HT','HU','ID','IE','IL','IM','IN','IO','IQ','IR','IS','IT','JE','JM','JO','JP','KE','KG','KH','KI','KM','KN','KP','KR','KW','KY','KZ','LA','LB','LC','LI','LK','LR','LS','LT','LU','LV','LY','MA','MC','MD','ME','MF','MG','MH','MK','ML','MM','MN','MO','MP','MQ','MR','MS','MT','MU','MV','MW','MX','MY','MZ','NA','NC','NE','NF','NG','NI','NL','NO','NP','NR','NU','NZ','OM','PA','PE','PF','PG','PH','PK','PL','PM','PN','PR','PS','PT','PW','PY','QA','RE','RO','RS','RU','RW','SA','SB','SC','SD','SE','SG','SH','SI','SJ','SK','SL','SM','SN','SO','SR','SS','ST','SV','SX','SY','SZ','TC','TD','TF','TG','TH','TJ','TK','TL','TM','TN','TO','TR','TT','TV','TW','TZ','UA','UG','UM','US','UY','UZ','VA','VC','VE','VG','VI','VN','VU','WF','WS','YE','YT','ZA','ZM','ZW')),
 CONSTRAINT tax_subdivision_valid CHECK(subdivision='' OR (subdivision ~ '^[A-Z]{2}-[A-Z0-9]{1,3}$' AND left(subdivision,2)=country_code))
);
CREATE TABLE app.tax_categories (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 code text NOT NULL CHECK(code ~ '^[a-z][a-z0-9_]{0,63}$'),
 name text NOT NULL CHECK(length(btrim(name)) BETWEEN 1 AND 128),
 is_exempt boolean NOT NULL DEFAULT false,
 PRIMARY KEY(tenant_id,code)
);
CREATE TABLE app.tax_rates (
 tenant_id uuid NOT NULL,
 id uuid NOT NULL DEFAULT uuidv7(),
 zone_id uuid NOT NULL,
 category_code text NOT NULL,
 rate_bp integer NOT NULL,
 valid_from timestamptz NOT NULL,
 valid_until timestamptz,
 PRIMARY KEY(tenant_id,id),
 CONSTRAINT tax_rate_zone_fk FOREIGN KEY(tenant_id,zone_id) REFERENCES app.tax_zones(tenant_id,id),
 CONSTRAINT tax_rate_category_fk FOREIGN KEY(tenant_id,category_code) REFERENCES app.tax_categories(tenant_id,code),
 CONSTRAINT tax_rate_bp_valid CHECK(rate_bp BETWEEN 0 AND 10000),
 CONSTRAINT tax_rate_window_valid CHECK(isfinite(valid_from) AND extract(year FROM valid_from AT TIME ZONE 'UTC') BETWEEN 1 AND 9999 AND (valid_until IS NULL OR (isfinite(valid_until) AND extract(year FROM valid_until AT TIME ZONE 'UTC') BETWEEN 1 AND 9999 AND valid_until>valid_from))),
 CONSTRAINT tax_rate_no_overlap EXCLUDE USING gist (
  tenant_id WITH =,zone_id WITH =,category_code WITH =,
  tstzrange(valid_from,valid_until,'[)') WITH &&)
);
CREATE TABLE app.tax_settings (
 tenant_id uuid PRIMARY KEY REFERENCES app.tenants(id),
 prices_include_tax boolean NOT NULL DEFAULT false
);

ALTER TABLE app.tax_zones ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.tax_zones FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.tax_zones
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
REVOKE ALL ON app.tax_zones FROM PUBLIC;
GRANT SELECT ON app.tax_zones TO app_runtime;
GRANT INSERT(tenant_id,id,name) ON app.tax_zones TO app_runtime;
GRANT UPDATE(name) ON app.tax_zones TO app_runtime;

ALTER TABLE app.tax_zone_locations ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.tax_zone_locations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.tax_zone_locations
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
REVOKE ALL ON app.tax_zone_locations FROM PUBLIC;
GRANT SELECT ON app.tax_zone_locations TO app_runtime;
GRANT INSERT(tenant_id,zone_id,country_code,subdivision) ON app.tax_zone_locations TO app_runtime;

ALTER TABLE app.tax_categories ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.tax_categories FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.tax_categories
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
REVOKE ALL ON app.tax_categories FROM PUBLIC;
GRANT SELECT ON app.tax_categories TO app_runtime;
GRANT INSERT(tenant_id,code,name,is_exempt) ON app.tax_categories TO app_runtime;
GRANT UPDATE(name,is_exempt) ON app.tax_categories TO app_runtime;

ALTER TABLE app.tax_rates ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.tax_rates FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.tax_rates
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
REVOKE ALL ON app.tax_rates FROM PUBLIC;
GRANT SELECT ON app.tax_rates TO app_runtime;
GRANT INSERT(tenant_id,id,zone_id,category_code,rate_bp,valid_from,valid_until) ON app.tax_rates TO app_runtime;
GRANT UPDATE(rate_bp,valid_from,valid_until) ON app.tax_rates TO app_runtime;

ALTER TABLE app.tax_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.tax_settings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.tax_settings
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
REVOKE ALL ON app.tax_settings FROM PUBLIC;
GRANT SELECT ON app.tax_settings TO app_runtime;
GRANT INSERT(tenant_id,prices_include_tax) ON app.tax_settings TO app_runtime;
GRANT UPDATE(prices_include_tax) ON app.tax_settings TO app_runtime;

