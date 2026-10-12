-- Responsibility: Shipping owns templates, zones, destinations and rate tiers.
-- Dependencies: tenant identity (001), PostgreSQL 18 btree_gist; no carrier APIs.
-- Caller: privileged migration runner; runtime has reads only after hardening.
-- Invariants: tenant-scoped references, exact integer prices, disjoint half-open
-- weight tiers, one fixed/free rule per template and zone, SELECT-only runtime.
-- Tentative number: integrator assigns final consecutive number before merge.
CREATE TABLE app.shipping_zones (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 id uuid NOT NULL DEFAULT uuidv7(), name text NOT NULL,
 PRIMARY KEY(tenant_id,id)
);
CREATE TABLE app.shipping_zone_destinations (
 tenant_id uuid NOT NULL, zone_id uuid NOT NULL,
 country_code char(2) NOT NULL, subdivision_code text NOT NULL DEFAULT '',
 PRIMARY KEY(tenant_id,zone_id,country_code,subdivision_code)
);
CREATE TABLE app.shipping_profiles (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 id uuid NOT NULL DEFAULT uuidv7(), name text NOT NULL, currency char(3) NOT NULL,
 PRIMARY KEY(tenant_id,id), UNIQUE(tenant_id,id,currency)
);
CREATE TABLE app.shipping_rates (
 tenant_id uuid NOT NULL, id uuid NOT NULL DEFAULT uuidv7(),
 profile_id uuid NOT NULL, zone_id uuid NOT NULL, method text NOT NULL,
 currency char(3) NOT NULL, amount bigint NOT NULL,
 min_weight_g bigint, max_weight_g bigint, free_over_amount bigint,
 PRIMARY KEY(tenant_id,id)
);

-- btree_gist provides UUID equality alongside the built-in int8range overlap.
-- It is shipped with PostgreSQL, not a new Go/network dependency.
CREATE EXTENSION IF NOT EXISTS btree_gist;
ALTER TABLE app.shipping_zones ADD CONSTRAINT shipping_zone_name CHECK(btrim(name)<>'');
ALTER TABLE app.shipping_profiles ADD CONSTRAINT shipping_profile_name CHECK(btrim(name)<>'');
ALTER TABLE app.shipping_profiles ADD CONSTRAINT shipping_profile_currency CHECK(currency::text ~ '^[A-Z]{3}$');
ALTER TABLE app.shipping_zone_destinations
 ADD CONSTRAINT shipping_destination_zone_fk FOREIGN KEY(tenant_id,zone_id)
 REFERENCES app.shipping_zones(tenant_id,id),
 ADD CONSTRAINT shipping_destination_country CHECK(country_code::text = ANY(ARRAY[
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
  'VN','VU','WF','WS','YE','YT','ZA','ZM','ZW']::text[])),
 -- Country-wide destinations use ''. Subdivisions use the full prefixed code.
 -- This checks ISO3166-2 shape/prefix; registry membership is a future admin concern.
 ADD CONSTRAINT shipping_destination_subdivision CHECK(subdivision_code='' OR
  (subdivision_code ~ '^[A-Z]{2}-[A-Z0-9]{1,3}$' AND
   left(subdivision_code,2)=country_code::text));
ALTER TABLE app.shipping_rates
 ADD CONSTRAINT shipping_rate_zone_fk FOREIGN KEY(tenant_id,zone_id)
 REFERENCES app.shipping_zones(tenant_id,id),
 ADD CONSTRAINT shipping_rate_profile_fk FOREIGN KEY(tenant_id,profile_id,currency)
 REFERENCES app.shipping_profiles(tenant_id,id,currency),
 ADD CONSTRAINT shipping_rate_amount CHECK(amount>=0),
 ADD CONSTRAINT shipping_rate_shape CHECK(
  (method='fixed' AND min_weight_g IS NULL AND max_weight_g IS NULL AND free_over_amount IS NULL) OR
  (method='by_weight' AND min_weight_g IS NOT NULL AND min_weight_g>=0 AND
   (max_weight_g IS NULL OR max_weight_g>min_weight_g) AND free_over_amount IS NULL) OR
  (method='free_over_amount' AND amount=0 AND min_weight_g IS NULL AND max_weight_g IS NULL AND
   free_over_amount IS NOT NULL AND free_over_amount>=0)),
 ADD CONSTRAINT shipping_weight_no_overlap EXCLUDE USING gist
  (tenant_id WITH =,profile_id WITH =,zone_id WITH =,
   int8range(min_weight_g,max_weight_g,'[)') WITH &&) WHERE(method='by_weight');
CREATE UNIQUE INDEX shipping_one_fixed ON app.shipping_rates(tenant_id,profile_id,zone_id) WHERE method='fixed';
CREATE UNIQUE INDEX shipping_one_free ON app.shipping_rates(tenant_id,profile_id,zone_id) WHERE method='free_over_amount';

ALTER TABLE app.shipping_zones ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.shipping_zones FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.shipping_zones TO app_runtime
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
GRANT SELECT ON app.shipping_zones TO app_runtime;

ALTER TABLE app.shipping_zone_destinations ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.shipping_zone_destinations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.shipping_zone_destinations TO app_runtime
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
GRANT SELECT ON app.shipping_zone_destinations TO app_runtime;

ALTER TABLE app.shipping_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.shipping_profiles FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.shipping_profiles TO app_runtime
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
GRANT SELECT ON app.shipping_profiles TO app_runtime;

ALTER TABLE app.shipping_rates ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.shipping_rates FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.shipping_rates TO app_runtime
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
GRANT SELECT ON app.shipping_rates TO app_runtime;
