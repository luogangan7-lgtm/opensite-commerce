-- Real PostgreSQL privacy gate; synthetic tenant/customer fixtures roll back.
-- Depends on 024 controlled functions. Called by check_pg; no production data.
-- Asserts scope, credentials exclusion, single use, erasure and runtime fences.
\set ON_ERROR_STOP on
BEGIN;
INSERT INTO app.tenants(name,settlement_currency) VALUES('privacy-A','USD') RETURNING id AS t \gset
INSERT INTO app.tenants(name) VALUES('privacy-B') RETURNING id AS foreign_t \gset
INSERT INTO app.customers(tenant_id,email,display_name) VALUES(:'t','privacy@example.invalid','Secret Person') RETURNING id AS c \gset
INSERT INTO app.customers(tenant_id,email) VALUES(:'foreign_t','foreign@example.invalid') RETURNING id AS foreign_c \gset
INSERT INTO app.customer_addresses(tenant_id,customer_id,recipient,address_line1,city,country_code)
 VALUES(:'t',:'c','Secret Person','Private street','Private city','US');
INSERT INTO app.customer_credentials VALUES(:'t',:'c','$argon2id$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA',now());
SELECT set_config('app.tenant_id',:'t',true),set_config('test.privacy_customer',:'c',true);
-- Real sealed publication has no customer ownership and must survive erasure.
DO $media$
DECLARE t uuid:=current_setting('test.privacy_seed_t',true)::uuid; o uuid;site uuid;r uuid;b uuid;a uuid;
BEGIN
 t:=(SELECT tenant_id FROM app.customers WHERE email='privacy@example.invalid');
 INSERT INTO app.organizations(tenant_id,display_name) VALUES(t,'Privacy fixture') RETURNING id INTO o;
 INSERT INTO app.sites(tenant_id,organization_id,name) VALUES(t,o,'Privacy fixture') RETURNING id INTO site;
 INSERT INTO app.site_releases(tenant_id,site_id,release_no,renderer_build_id,manifest,manifest_hash)
  VALUES(t,site,1,'test','{}',sha256('manifest'::bytea)) RETURNING id INTO r;
 INSERT INTO app.media_blobs(tenant_id,backend,storage_key,sha256,size_bytes,status) VALUES(t,'local','privacy-unowned',sha256('media'::bytea),1,'ready') RETURNING id INTO b;
 INSERT INTO app.media_assets(tenant_id,blob_id,filename,mime_type,visibility) VALUES(t,b,'public-unowned','image/png','public') RETURNING id INTO a;
 PERFORM set_config('app.tenant_id',t::text,true);
 INSERT INTO app.site_release_media(tenant_id,site_id,release_id,media_id) VALUES(t,site,r,a);
 PERFORM set_config('test.privacy_release',r::text,true);PERFORM set_config('test.privacy_site',site::text,true);
 PERFORM set_config('test.privacy_media',a::text,true);PERFORM set_config('test.privacy_blob',b::text,true);
END $media$;
SET LOCAL ROLE app_publisher;
SELECT app.publish_release(:'t',current_setting('test.privacy_site')::uuid,current_setting('test.privacy_release')::uuid,NULL);
RESET ROLE;
INSERT INTO app.products(tenant_id,product_code) VALUES(:'t','privacy') RETURNING id AS p \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(:'t',:'p','privacy-sku','default') RETURNING id AS sku \gset
SELECT set_config('test.privacy_sku',:'sku',true);
INSERT INTO app.ledger_accounts(tenant_id,code,currency,account_type) VALUES(:'t','privacy-asset','USD','asset') RETURNING id AS debit \gset
INSERT INTO app.ledger_accounts(tenant_id,code,currency,account_type) VALUES(:'t','privacy-revenue','USD','revenue') RETURNING id AS credit \gset
SET LOCAL ROLE app_inventory_manager;
SELECT app.inventory_adjust(:'sku',5,'privacy synthetic fixture',uuidv7(),'inbound');
SET LOCAL ROLE app_runtime;
SELECT app.post_ledger(:'t','USD','order',:'c','paid',ARRAY[:'debit'::uuid,:'credit'::uuid],ARRAY['debit','credit'],ARRAY[130,130]::bigint[]);
SELECT set_config('test.privacy_ledger_before',(SELECT jsonb_agg(to_jsonb(l) ORDER BY transaction_id,line_no)::text FROM app.ledger_entries l),true);
DO $order$
DECLARE sku uuid:=current_setting('test.privacy_sku')::uuid;oid uuid:=uuidv7();r app.inventory_reservations;o app.orders;l jsonb;
BEGIN
 r:=app.inventory_reserve(sku,1,'order',oid,'privacy-reserve',900);
 l:=jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id',sku,'sku','frozen-sku','quantity',1,'unit_price_minor',100,'line_net_minor',100,'line_tax_minor',10,'line_total_minor',110,'reservation_id',r.id,'tax_snapshot',jsonb_build_object('net_minor',100,'tax_minor',10,'gross_minor',110,'currency','USD','basis_points','Secret Person','private','Private street')));
 o:=app.orders_create(oid,'privacy-create',sha256('fixture'::bytea),current_setting('test.privacy_customer')::uuid,NULL,'USD',20,'{"amount_minor":20,"currency":"USD","recipient":"Secret Person","address":"Private street"}',l,'[]');
 PERFORM set_config('test.privacy_order',oid::text,true);
 PERFORM set_config('test.privacy_order_before',(to_jsonb(o)-'shipping_snapshot')::text,true);
 PERFORM set_config('test.privacy_line_before',(SELECT (to_jsonb(x)-'tax_snapshot')::text FROM app.order_lines x WHERE order_id=oid),true);
END $order$;
RESET ROLE;
SELECT set_config('app.tenant_id',:'t',true),set_config('test.privacy_customer',:'c',true),set_config('test.privacy_foreign',:'foreign_c',true);
SET LOCAL ROLE app_runtime;
DO $gate$
DECLARE c uuid:=current_setting('test.privacy_customer')::uuid; r uuid:=uuidv7(); j jsonb; x jsonb;
BEGIN
 j:=app.data_rights_request_export(r,c,'user',uuidv7());
 IF j->>'status'<>'pending' THEN RAISE EXCEPTION 'PRIVACY_GATE pending'; END IF;
 x:=app.data_rights_build_export(r);
 IF x::text LIKE '%password_hash%' OR x::text LIKE '%argon2id%' OR x::text LIKE '%foreign@example%' THEN
  RAISE EXCEPTION 'PRIVACY_GATE sensitive export'; END IF;
 IF x->'customer'->>'display_name'<>'Secret Person' OR jsonb_array_length(x->'addresses')<>1 THEN
  RAISE EXCEPTION 'PRIVACY_GATE incomplete export'; END IF;
 PERFORM app.data_rights_complete_export(r,sha256('token'::bytea),sha256(x::text::bytea),app.data_rights_download_deadline(r));
 PERFORM app.data_rights_consume_export(r,c,sha256('token'::bytea));
 BEGIN PERFORM app.data_rights_consume_export(r,c,sha256('token'::bytea)); RAISE EXCEPTION 'PRIVACY_GATE replay'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN PERFORM app.data_rights_request_export(uuidv7(),current_setting('test.privacy_foreign')::uuid,'user',uuidv7());
  RAISE EXCEPTION 'PRIVACY_GATE cross tenant'; EXCEPTION WHEN no_data_found THEN NULL; END;
 r:=uuidv7();PERFORM app.data_rights_request_export(r,c,'service',uuidv7());
 x:=app.data_rights_erase_customer(c,'user',uuidv7());
 IF (SELECT jsonb_agg(to_jsonb(l) ORDER BY transaction_id,line_no)::text FROM app.ledger_entries l)<>current_setting('test.privacy_ledger_before') THEN RAISE EXCEPTION 'PRIVACY_GATE ledger changed'; END IF;
 IF (SELECT (to_jsonb(o)-'shipping_snapshot')::text FROM app.orders o WHERE id=current_setting('test.privacy_order')::uuid)<>current_setting('test.privacy_order_before') OR (SELECT (to_jsonb(l)-'tax_snapshot')::text FROM app.order_lines l WHERE order_id=current_setting('test.privacy_order')::uuid)<>current_setting('test.privacy_line_before') THEN RAISE EXCEPTION 'PRIVACY_GATE financial identity changed'; END IF;
 IF (SELECT shipping_snapshot::text FROM app.orders WHERE id=current_setting('test.privacy_order')::uuid) LIKE '%Secret%' OR (SELECT tax_snapshot FROM app.order_lines WHERE order_id=current_setting('test.privacy_order')::uuid)<>'{"erased":true}'::jsonb THEN RAISE EXCEPTION 'PRIVACY_GATE snapshot PII residue'; END IF;
 IF x->'counts'->>'credentials'<>'1' OR x->'counts'->>'addresses'<>'1' THEN RAISE EXCEPTION 'PRIVACY_GATE erase counts'; END IF;
 IF (SELECT status FROM app.customers WHERE id=c)<>'disabled' OR (SELECT display_name FROM app.customers WHERE id=c)<>'[erased]' THEN
  RAISE EXCEPTION 'PRIVACY_GATE customer barrier'; END IF;
 BEGIN PERFORM app.data_rights_build_export(r); RAISE EXCEPTION 'PRIVACY_GATE revoked build'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN PERFORM app.data_rights_request_export(uuidv7(),c,'user',uuidv7()); RAISE EXCEPTION 'PRIVACY_GATE disabled export'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN PERFORM password_hash FROM app.customer_credentials; RAISE EXCEPTION 'PRIVACY_GATE runtime credentials'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN DELETE FROM app.customer_credentials; RAISE EXCEPTION 'PRIVACY_GATE runtime delete'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN UPDATE app.data_rights_exports SET status='ready'; RAISE EXCEPTION 'PRIVACY_GATE raw export mutation'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $gate$;
RESET ROLE;
-- Check expired capability independently from request expiry and single-use.
INSERT INTO app.customers(tenant_id,email) VALUES(:'t','expiry@example.invalid') RETURNING id AS expiry_customer \gset
SELECT uuidv7() AS expiry_request \gset
SET LOCAL ROLE app_runtime;
SELECT app.data_rights_request_export(:'expiry_request',:'expiry_customer','service',uuidv7());
SELECT app.data_rights_complete_export(:'expiry_request',sha256('expiry-token'::bytea),sha256('expiry-content'::bytea),app.data_rights_download_deadline(:'expiry_request'));
RESET ROLE;
UPDATE app.data_rights_exports SET download_expires_at=clock_timestamp()-interval '1 second' WHERE id=:'expiry_request';
SELECT set_config('test.privacy_expiry_request',:'expiry_request',true),set_config('test.privacy_expiry_customer',:'expiry_customer',true);
SET LOCAL ROLE app_runtime;
DO $expiry$
BEGIN
 BEGIN PERFORM app.data_rights_consume_export(current_setting('test.privacy_expiry_request')::uuid,current_setting('test.privacy_expiry_customer')::uuid,sha256('expiry-token'::bytea)); RAISE EXCEPTION 'PRIVACY_GATE expired capability'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 PERFORM set_config('app.tenant_id','',true);
 BEGIN PERFORM app.data_rights_status(current_setting('test.privacy_expiry_request')::uuid,current_setting('test.privacy_expiry_customer')::uuid); RAISE EXCEPTION 'PRIVACY_GATE missing tenant'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 PERFORM set_config('app.tenant_id','not-a-uuid',true);
 BEGIN PERFORM app.data_rights_status(current_setting('test.privacy_expiry_request')::uuid,current_setting('test.privacy_expiry_customer')::uuid); RAISE EXCEPTION 'PRIVACY_GATE malformed tenant'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $expiry$;
RESET ROLE;
SELECT set_config('app.tenant_id',:'t',true);
DO $authority$
BEGIN
 IF EXISTS(SELECT FROM pg_roles WHERE rolname='app_data_rights_executor' AND (rolcanlogin OR rolbypassrls OR rolsuper))
 OR EXISTS(SELECT FROM pg_auth_members WHERE roleid='app_data_rights_executor'::regrole OR member='app_data_rights_executor'::regrole) THEN RAISE EXCEPTION 'PRIVACY_GATE executor reachable'; END IF;
 IF NOT (SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid='app.data_rights_exports'::regclass) THEN RAISE EXCEPTION 'PRIVACY_GATE forced RLS'; END IF;
 IF EXISTS(SELECT FROM app.customer_credentials WHERE customer_id=current_setting('test.privacy_customer')::uuid) THEN RAISE EXCEPTION 'PRIVACY_GATE credential residue'; END IF;
 IF NOT EXISTS(SELECT FROM app.site_releases WHERE id=current_setting('test.privacy_release')::uuid AND sealed) OR NOT EXISTS(SELECT FROM app.media_assets WHERE id=current_setting('test.privacy_media')::uuid AND visibility='public') OR NOT EXISTS(SELECT FROM app.media_blobs WHERE id=current_setting('test.privacy_blob')::uuid AND status='ready') THEN RAISE EXCEPTION 'PRIVACY_GATE unowned sealed media changed'; END IF;
 BEGIN UPDATE app.customers SET status='active' WHERE id=current_setting('test.privacy_customer')::uuid; RAISE EXCEPTION 'PRIVACY_GATE customer resurrection'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 BEGIN INSERT INTO app.customer_credentials VALUES(current_setting('app.tenant_id')::uuid,current_setting('test.privacy_customer')::uuid,'$argon2id$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA',now()); RAISE EXCEPTION 'PRIVACY_GATE credential resurrection'; EXCEPTION WHEN object_not_in_prerequisite_state THEN NULL; END;
 -- Current schema has no customer sessions or customer-owned media binding:
 -- erasure never maps customer UUIDs to staff users or media_uploads.actor_id.
END $authority$;
ROLLBACK;
