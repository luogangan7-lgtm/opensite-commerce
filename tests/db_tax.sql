-- Responsibility: real-PG tax constraints, tenant isolation and runtime capabilities.
-- Dependencies: all forward migrations including the logical tax migration.
-- Caller: check_pg.sh on an expendable PG18 database. Every expected refusal
-- checks its exact SQLSTATE; a broken fixture or permission cannot satisfy it.
\set ON_ERROR_STOP on
BEGIN;
SET LOCAL ROLE app_runtime;
DO $unset$ DECLARE tab text; n integer; BEGIN
 FOREACH tab IN ARRAY ARRAY['tax_zones','tax_zone_locations','tax_categories','tax_rates','tax_settings'] LOOP
  EXECUTE format('SELECT count(*) FROM app.%I',tab) INTO n;
  IF n<>0 THEN RAISE EXCEPTION 'TAX_UNSET_CONTEXT %',tab; END IF;
 END LOOP;
END $unset$;
RESET ROLE;
INSERT INTO app.tenants(name) VALUES('tax-A') RETURNING id AS ta \gset
INSERT INTO app.tenants(name) VALUES('tax-B') RETURNING id AS tb \gset
INSERT INTO app.tax_zones(tenant_id,name) VALUES(:'ta','country') RETURNING id AS za \gset
INSERT INTO app.tax_zones(tenant_id,name) VALUES(:'ta','subdivision') RETURNING id AS zs \gset
INSERT INTO app.tax_zones(tenant_id,name) VALUES(:'tb','foreign') RETURNING id AS zb \gset
INSERT INTO app.tax_categories(tenant_id,code,name,is_exempt) VALUES
 (:'ta','standard','Standard',false),(:'ta','zero','Zero',false),(:'ta','exempt','Exempt',true),
 (:'tb','foreign','Foreign',false);
INSERT INTO app.tax_zone_locations(tenant_id,zone_id,country_code,subdivision) VALUES
 (:'ta',:'za','US',''),(:'ta',:'zs','US','US-CA'),(:'tb',:'zb','CA','');
INSERT INTO app.tax_settings(tenant_id) VALUES(:'ta'),(:'tb');
INSERT INTO app.tax_rates(tenant_id,zone_id,category_code,rate_bp,valid_from,valid_until) VALUES
 (:'ta',:'za','standard',500,'2026-01-01Z','2027-01-01Z'),
 (:'ta',:'za','standard',600,'2027-01-01Z',NULL),
 (:'ta',:'zs','standard',700,'2026-01-01Z',NULL),
 (:'ta',:'za','zero',0,'2026-01-01Z',NULL),
 (:'tb',:'zb','foreign',10000,'2026-01-01Z',NULL);
SELECT set_config('tax.ta',:'ta',true),set_config('tax.tb',:'tb',true),
 set_config('tax.za',:'za',true),set_config('tax.zs',:'zs',true),set_config('tax.zb',:'zb',true);
CREATE FUNCTION pg_temp.tax_reject(command text,wanted text,label text) RETURNS void
LANGUAGE plpgsql AS $$ BEGIN
 BEGIN
  EXECUTE command;
 EXCEPTION WHEN OTHERS THEN
  IF SQLSTATE=wanted THEN RETURN; END IF;
  RAISE EXCEPTION 'TAX_WRONG_REFUSAL %: expected %, got % (%)',label,wanted,SQLSTATE,SQLERRM;
 END;
 RAISE EXCEPTION 'TAX_EXPECTED_REFUSAL % (%)',label,wanted;
END $$;
DO $guards$ DECLARE
 t uuid:=current_setting('tax.ta')::uuid; b uuid:=current_setting('tax.tb')::uuid;
 z uuid:=current_setting('tax.za')::uuid; s uuid:=current_setting('tax.zs')::uuid;
 foreign_z uuid:=current_setting('tax.zb')::uuid; bad_country text;
BEGIN
 PERFORM pg_temp.tax_reject(format('INSERT INTO app.tax_rates(tenant_id,zone_id,category_code,rate_bp,valid_from,valid_until) VALUES(%L,%L,''standard'',550,''2026-06-01Z'',''2027-06-01Z'')',t,z),'23P01','overlap_insert');
 PERFORM pg_temp.tax_reject(format('UPDATE app.tax_rates SET valid_from=''2026-12-31Z'' WHERE tenant_id=%L AND zone_id=%L AND rate_bp=600',t,z),'23P01','overlap_update');
 -- Endpoint adjacency was inserted above; unrelated category/zone/tenant coexists.
 PERFORM pg_temp.tax_reject(format('INSERT INTO app.tax_rates(tenant_id,zone_id,category_code,rate_bp,valid_from,valid_until) VALUES(%L,%L,''standard'',-1,''2025-01-01Z'',''2026-01-01Z'')',t,z),'23514','negative_rate');
 PERFORM pg_temp.tax_reject(format('INSERT INTO app.tax_rates(tenant_id,zone_id,category_code,rate_bp,valid_from,valid_until) VALUES(%L,%L,''standard'',10001,''2025-01-01Z'',''2026-01-01Z'')',t,z),'23514','high_rate');
 PERFORM pg_temp.tax_reject(format('INSERT INTO app.tax_rates(tenant_id,zone_id,category_code,rate_bp,valid_from,valid_until) VALUES(%L,%L,''standard'',500,''2025-01-01Z'',''2025-01-01Z'')',t,z),'23514','empty_window');
 PERFORM pg_temp.tax_reject(format('INSERT INTO app.tax_rates(tenant_id,zone_id,category_code,rate_bp,valid_from,valid_until) VALUES(%L,%L,''standard'',500,''2025-01-02Z'',''2025-01-01Z'')',t,z),'23514','reversed_window');
 PERFORM pg_temp.tax_reject(format('INSERT INTO app.tax_rates(tenant_id,zone_id,category_code,rate_bp,valid_from) VALUES(%L,%L,''standard'',500,''infinity'')',t,z),'23514','infinite_window');
 PERFORM pg_temp.tax_reject(format('INSERT INTO app.tax_rates(tenant_id,zone_id,category_code,rate_bp,valid_from) VALUES(%L,%L,''exempt'',500,''10000-01-01Z'')',t,z),'23514','snapshot_year');
 PERFORM pg_temp.tax_reject(format('INSERT INTO app.tax_rates(tenant_id,zone_id,category_code,rate_bp,valid_from) VALUES(%L,%L,''standard'',500,''2025-01-01Z'')',t,foreign_z),'23503','foreign_rate_zone');
 PERFORM pg_temp.tax_reject(format('INSERT INTO app.tax_rates(tenant_id,zone_id,category_code,rate_bp,valid_from) VALUES(%L,%L,''foreign'',500,''2025-01-01Z'')',t,z),'23503','foreign_rate_category');
 PERFORM pg_temp.tax_reject(format('INSERT INTO app.tax_zone_locations(tenant_id,zone_id,country_code) VALUES(%L,%L,''GB'')',t,foreign_z),'23503','foreign_location_zone');
 PERFORM pg_temp.tax_reject(format('INSERT INTO app.tax_zone_locations(tenant_id,zone_id,country_code) VALUES(%L,%L,''US'')',t,s),'23505','destination_unique');
 FOREACH bad_country IN ARRAY ARRAY['ZZ','AC','us','USA','U1',' US',''] LOOP
  PERFORM pg_temp.tax_reject(format('INSERT INTO app.tax_zone_locations(tenant_id,zone_id,country_code) VALUES(%L,%L,%L)',t,z,bad_country),'23514','country_'||bad_country);
 END LOOP;
 PERFORM pg_temp.tax_reject(format('INSERT INTO app.tax_zone_locations(tenant_id,zone_id,country_code,subdivision) VALUES(%L,%L,''US'',''CA-ON'')',t,z),'23514','subdivision_prefix');
 PERFORM pg_temp.tax_reject(format('INSERT INTO app.tax_zone_locations(tenant_id,zone_id,country_code,subdivision) VALUES(%L,%L,''US'',''US-TOOLONG'')',t,z),'23514','subdivision_shape');
 PERFORM pg_temp.tax_reject(format('INSERT INTO app.tax_categories(tenant_id,code,name) VALUES(%L,''bad code'',''Bad'')',t),'23514','category_shape');
 IF (SELECT prices_include_tax FROM app.tax_settings WHERE tenant_id=t) IS DISTINCT FROM false THEN
  RAISE EXCEPTION 'TAX_DEFAULT_PRICE_MODE';
 END IF;
END $guards$;
SELECT set_config('app.tenant_id',:'ta',true);
SET LOCAL ROLE app_runtime;
DO $runtime$ DECLARE
 t uuid:=current_setting('tax.ta')::uuid; b uuid:=current_setting('tax.tb')::uuid;
 z uuid:=current_setting('tax.za')::uuid; tab text; n integer;
BEGIN
 FOREACH tab IN ARRAY ARRAY['tax_zones','tax_zone_locations','tax_categories','tax_rates','tax_settings'] LOOP
  EXECUTE format('SELECT count(*) FROM app.%I WHERE tenant_id=%L',tab,b) INTO n;
  IF n<>0 THEN RAISE EXCEPTION 'TAX_FOREIGN_READ %',tab; END IF;
  EXECUTE format('SELECT count(*) FROM app.%I WHERE tenant_id=%L',tab,t) INTO n;
  IF n=0 THEN RAISE EXCEPTION 'TAX_OWN_READ %',tab; END IF;
  PERFORM pg_temp.tax_reject(format('DELETE FROM app.%I WHERE tenant_id=%L',tab,t),'42501','delete_'||tab);
  PERFORM pg_temp.tax_reject(format('UPDATE app.%I SET tenant_id=%L WHERE tenant_id=%L',tab,b,t),'42501','identity_'||tab);
 END LOOP;
 PERFORM pg_temp.tax_reject(format('INSERT INTO app.tax_zones(tenant_id,name) VALUES(%L,''cross'')',b),'42501','rls_zone_write');
 PERFORM pg_temp.tax_reject(format('INSERT INTO app.tax_settings(tenant_id) VALUES(%L)',uuidv7()),'42501','rls_settings_write');
 PERFORM pg_temp.tax_reject(format('INSERT INTO app.tax_categories(tenant_id,code,name) VALUES(%L,''cross'',''Cross'')',b),'42501','rls_category_write');
 PERFORM pg_temp.tax_reject(format('INSERT INTO app.tax_zone_locations(tenant_id,zone_id,country_code) VALUES(%L,%L,''GB'')',b,z),'42501','rls_location_write');
 PERFORM pg_temp.tax_reject(format('INSERT INTO app.tax_rates(tenant_id,zone_id,category_code,rate_bp,valid_from) VALUES(%L,%L,''standard'',1,''2025-01-01Z'')',b,z),'42501','rls_rate_write');
 INSERT INTO app.tax_categories(tenant_id,code,name) VALUES(t,'reduced','Reduced');
 INSERT INTO app.tax_rates(tenant_id,zone_id,category_code,rate_bp,valid_from) VALUES(t,z,'reduced',250,'2025-01-01Z');
 UPDATE app.tax_rates SET rate_bp=300 WHERE tenant_id=t AND category_code='reduced';
 UPDATE app.tax_settings SET prices_include_tax=true WHERE tenant_id=t;
 IF (SELECT rate_bp FROM app.tax_rates WHERE tenant_id=t AND category_code='reduced')<>300
 OR NOT (SELECT prices_include_tax FROM app.tax_settings WHERE tenant_id=t) THEN RAISE EXCEPTION 'TAX_RUNTIME_OWN_UPDATE'; END IF;
 -- Empty or absent context must filter every table, not attempt an empty UUID cast.
 PERFORM set_config('app.tenant_id','',true);
 FOREACH tab IN ARRAY ARRAY['tax_zones','tax_zone_locations','tax_categories','tax_rates','tax_settings'] LOOP
  EXECUTE format('SELECT count(*) FROM app.%I',tab) INTO n;
  IF n<>0 THEN RAISE EXCEPTION 'TAX_BLANK_CONTEXT %',tab; END IF;
 END LOOP;
END $runtime$;
RESET ROLE;
ROLLBACK;
SELECT 'PASS: tax windows, composite FKs, country/subdivision, rate bounds, RLS and runtime writes';
