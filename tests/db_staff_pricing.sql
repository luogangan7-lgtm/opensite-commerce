-- Responsibility: exercise staff pricing functions as runtime on real PG18.
-- Dependencies: all forward migrations; caller: check_pg CI. No durable fixtures.
-- Invariants: currency authority, list-wide CAS, tenant isolation and atomicity.
\set ON_ERROR_STOP on
BEGIN;
INSERT INTO app.tenants(name,settlement_currency) VALUES('staff-pricing','USD') RETURNING id AS t \gset
INSERT INTO app.tenants(name) VALUES('staff-pricing-unset') RETURNING id AS other \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','STAFF-PRICING') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(:'t',:'p','STAFF-PRICE','') RETURNING id AS sku \gset
SELECT set_config('app.tenant_id',:'t',true),set_config('pricing.sku',:'sku',true),set_config('pricing.other',:'other',true);
CREATE FUNCTION pg_temp.pricing_reject(command text,expected text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
 BEGIN EXECUTE command; EXCEPTION WHEN OTHERS THEN
  IF SQLSTATE=expected THEN RETURN; END IF;
  RAISE EXCEPTION 'STAFF_PRICING_GATE unexpected % want %: %',SQLSTATE,expected,SQLERRM;
 END;
 RAISE EXCEPTION 'STAFF_PRICING_GATE expected refusal %',expected;
END $$;
SET LOCAL ROLE app_runtime;
DO $test$
DECLARE l jsonb; list_key uuid; sku uuid:=current_setting('pricing.sku')::uuid;
 body jsonb:='{"currency":"USD","status":"active","starts_at":"2026-01-01T00:00:00Z","ends_at":"2027-01-01T00:00:00Z","priority":-1,"audience":"all"}';
BEGIN
 l:=app.pricing_write_list(NULL,0,body); list_key:=(l->>'id')::uuid;
 IF (l->>'version')::bigint<>1 OR l->>'currency'<>'USD' THEN RAISE EXCEPTION 'STAFF_PRICING_GATE create'; END IF;
 PERFORM set_config('pricing.list',list_key::text,true);
 PERFORM pg_temp.pricing_reject(format('SELECT app.pricing_write_list(NULL,0,%L)',body||'{"currency":"EUR"}'::jsonb),'P4091');
 PERFORM pg_temp.pricing_reject(format('SELECT app.pricing_write_list(NULL,0,%L)',body||'{"ends_at":"2026-01-01T00:00:00Z"}'::jsonb),'23514');
 PERFORM pg_temp.pricing_reject(format('SELECT app.pricing_write_list(NULL,0,%L)',body||'{"starts_at":"infinity","ends_at":null}'::jsonb),'23514');
 PERFORM pg_temp.pricing_reject(format('SELECT app.pricing_write_list(NULL,0,%L)',body||'{"audience":"customer_group"}'::jsonb),'23514');
 l:=app.pricing_write_entry(list_key,sku,1,125,1);
 IF (l->>'version')::bigint<>2 OR (SELECT unit_amount FROM app.price_entries WHERE price_list_id=list_key)<>125 THEN RAISE EXCEPTION 'STAFF_PRICING_GATE persist'; END IF;
 PERFORM pg_temp.pricing_reject(format('SELECT app.pricing_write_entry(%L,%L,1,999,1)',list_key,sku),'P4120');
 PERFORM pg_temp.pricing_reject(format('SELECT app.pricing_write_entry(%L,%L,1,-1,2)',list_key,sku),'23514');
 PERFORM pg_temp.pricing_reject(format('SELECT app.pricing_write_entry(%L,%L,0,1,2)',list_key,sku),'23514');
 PERFORM pg_temp.pricing_reject(format('SELECT app.pricing_write_entry(%L,%L,1,9223372036854775808,2)',list_key,sku),'42883');
 PERFORM pg_temp.pricing_reject(format('UPDATE app.price_lists SET priority=3 WHERE id=%L',list_key),'42501');
 PERFORM pg_temp.pricing_reject('SELECT settlement_currency FROM app.tenants','42501');
 BEGIN
  PERFORM app.pricing_write_entry(list_key,sku,1,300,2);
  RAISE EXCEPTION 'ROLLBACK_WITNESS' USING ERRCODE='P7777';
 EXCEPTION WHEN SQLSTATE 'P7777' THEN NULL;
 END;
 IF (SELECT version FROM app.price_lists WHERE price_lists.id=list_key)<>2 OR (SELECT unit_amount FROM app.price_entries WHERE price_list_id=list_key)<>125 THEN RAISE EXCEPTION 'STAFF_PRICING_GATE rollback'; END IF;
 PERFORM app.pricing_write_entry(list_key,sku,1,NULL,2);
 IF EXISTS(SELECT FROM app.price_entries WHERE price_list_id=list_key) THEN RAISE EXCEPTION 'STAFF_PRICING_GATE clear'; END IF;
 PERFORM app.pricing_write_list(list_key,3,body||'{"status":"archived"}'::jsonb);
 PERFORM pg_temp.pricing_reject(format('SELECT app.pricing_write_entry(%L,%L,1,1,4)',list_key,sku),'P4092');
 PERFORM pg_temp.pricing_reject(format('SELECT app.pricing_write_list(%L,4,%L)',list_key,body),'P4092');
 PERFORM set_config('app.tenant_id',current_setting('pricing.other'),true);
 IF EXISTS(SELECT FROM app.price_lists WHERE price_lists.id=list_key) THEN RAISE EXCEPTION 'STAFF_PRICING_GATE cross tenant read'; END IF;
 PERFORM pg_temp.pricing_reject(format('SELECT app.pricing_write_list(NULL,0,%L)',body),'P4090');
 PERFORM pg_temp.pricing_reject(format('SELECT app.pricing_write_entry(%L,%L,1,1,4)',list_key,sku),'P0002');
END $test$;
RESET ROLE;
ROLLBACK;
SELECT 'PASS: STAFF_PRICING_GATE currency CAS window RLS amount rollback persistence';
