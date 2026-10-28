-- Responsibility: legal, persistent per-column privacy canaries for CI and data-rights tests.
-- Dependencies: the complete forward-migration schema and its real inventory/order/quote commands.
-- Caller: privacy registry runner, privileged disposable CI database ONLY.
-- Invariants: no disabled constraints/triggers; immutable snapshots are populated on creation;
-- every column-canary placeholder is rendered as a quoted SQL literal by the runner.
-- The caller owns BEGIN/COMMIT/ROLLBACK. Commit only in an isolated test database
-- when a subsequent erasure test needs the rows. Never run against production.
-- Generated normalized/FK email copies include both column-specific markers in
-- their source email; coverage checks the actual stored/generated values.
DO $privacy_fixture$
DECLARE
 t uuid := 'f0c10000-0000-4000-8000-000000000001';
 staff uuid := 'f0c10000-0000-4000-8000-000000000002';
 customer uuid := 'f0c10000-0000-4000-8000-000000000003';
 approver uuid := 'f0c10000-0000-4000-8000-000000000004';
 organization uuid := 'f0c10000-0000-4000-8000-000000000005';
 site uuid := 'f0c10000-0000-4000-8000-000000000006';
 company uuid := 'f0c10000-0000-4000-8000-000000000007';
 product uuid := 'f0c10000-0000-4000-8000-000000000008';
 sku uuid := 'f0c10000-0000-4000-8000-000000000009';
 blob uuid := 'f0c10000-0000-4000-8000-000000000010';
 registered_order uuid := 'f0c10000-0000-4000-8000-000000000011';
 guest_order uuid := 'f0c10000-0000-4000-8000-000000000012';
 cart uuid := 'f0c10000-0000-4000-8000-000000000013';
 checkout uuid := 'f0c10000-0000-4000-8000-000000000014';
 inquiry uuid := 'f0c10000-0000-4000-8000-000000000015';
 inline_claim uuid := 'f0c10000-0000-4000-8000-000000000016';
 reference_claim uuid := 'f0c10000-0000-4000-8000-000000000017';
 page uuid := 'f0c10000-0000-4000-8000-000000000018';
 draft uuid := 'f0c10000-0000-4000-8000-000000000019';
 release uuid := 'f0c10000-0000-4000-8000-000000000020';
 blog uuid := 'f0c10000-0000-4000-8000-000000000021';
 notification_order uuid := 'f0c10000-0000-4000-8000-000000000022';
 guest_cart uuid := 'f0c10000-0000-4000-8000-000000000029';
 guest_checkout uuid := 'f0c10000-0000-4000-8000-000000000030';
 fulfillment_order uuid := 'f0c10000-0000-4000-8000-000000000023';
 fulfillment_line uuid := 'f0c10000-0000-4000-8000-000000000024';
 income_account uuid := 'f0c10000-0000-4000-8000-000000000025';
 tax_account uuid := 'f0c10000-0000-4000-8000-000000000026';
 shipping_account uuid := 'f0c10000-0000-4000-8000-000000000027';
 cash_account uuid := 'f0c10000-0000-4000-8000-000000000028';
 shipment_receipt jsonb;
 return_receipt jsonb;
 shipment_line uuid;
 return_id uuid;
 quote uuid;
 reservation app.inventory_reservations;
 registered app.orders;
 account_identifier text;
 synthetic_phc text;
BEGIN
 -- Fixed IDs intentionally fail on a second load instead of silently reusing
 -- stale rows. The registry runner supplies a fresh database or transaction.
 -- The refund canary's USD accounts require explicit tenant currency provisioning.
 INSERT INTO app.tenants(id,name,settlement_currency) VALUES(t,'privacy-column-canaries','USD');
 PERFORM set_config('app.tenant_id',t::text,true);
 PERFORM set_config('privacy.fixture.tenant_id',t::text,true);
 PERFORM set_config('privacy.fixture.customer_id',customer::text,true);
 PERFORM set_config('privacy.fixture.checkout_id',checkout::text,true);

 INSERT INTO app.users(id,email,
  password_hash)
 VALUES(staff,{{canary:users.email}}||'.'||{{canary:users.email_normalized}}||'@canary.invalid',
  encode(sha256(uuidv7()::text::bytea),'hex'));
 INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES(t,staff,'admin');
 account_identifier := {{canary:auth_account_attempts.identifier_normalized}}||'.'||
  {{canary:auth_login_attempts.identifier_normalized}}||'@canary.invalid';
 INSERT INTO app.auth_account_attempts(identifier_normalized) VALUES(account_identifier);
 INSERT INTO app.auth_login_attempts(identifier_normalized,ip)
 VALUES(account_identifier,{{canary:auth_login_attempts.ip}}::inet);

 INSERT INTO app.organizations(tenant_id,id,display_name) VALUES(t,organization,'Privacy fixture seller');
 INSERT INTO app.sites(tenant_id,id,organization_id,name) VALUES(t,site,organization,'Privacy fixture site');
 INSERT INTO app.company_contacts(tenant_id,organization_id,contact_type,contact_value)
 VALUES(t,organization,'email',{{canary:company_contacts.contact_value}}||'@canary.invalid');
 INSERT INTO app.company_profile_revisions(tenant_id,organization_id,locale,revision,profile)
 VALUES(t,organization,'en',1,jsonb_build_object('contact',{{canary:company_profile_revisions.profile}}));

 -- Public contact-bearing content is also a persistent PII copy. These are
 -- legal legacy publication/draft inputs (is_cms=false), never unmanaged
 -- mutations of managed CMS drafts. Seal only after inserting release children.
 INSERT INTO app.pages(tenant_id,site_id,id,page_key) VALUES(t,site,page,'privacy');
 INSERT INTO app.page_revisions(tenant_id,site_id,page_id,locale,revision,document)
 VALUES(t,site,page,'en',1,jsonb_build_object('title',{{canary:page_revisions.document}},'blocks','[]'::jsonb));
 INSERT INTO app.blog_posts(tenant_id,site_id,id) VALUES(t,site,blog);
 INSERT INTO app.blog_post_revisions(tenant_id,site_id,post_id,locale,revision,title,excerpt,body_blocks)
 VALUES(t,site,blog,'en',1,{{canary:blog_post_revisions.title}},{{canary:blog_post_revisions.excerpt}},
  jsonb_build_array(jsonb_build_object('kind','paragraph','text',{{canary:blog_post_revisions.body_blocks}})));
 INSERT INTO app.site_drafts(tenant_id,site_id,id,title,description,navigation)
 VALUES(t,site,draft,{{canary:site_drafts.title}},{{canary:site_drafts.description}},
  jsonb_build_array(jsonb_build_object('label',{{canary:site_drafts.navigation}},'href','/privacy')));
 INSERT INTO app.draft_pages(tenant_id,site_id,draft_id,page_id,locale,path,document)
 VALUES(t,site,draft,page,'en','/privacy',
  jsonb_build_object('title',{{canary:draft_pages.document}},'blocks','[]'::jsonb));
 INSERT INTO app.draft_operations(tenant_id,site_id,draft_id,operation_id,actor_id,
  before_revision,after_revision,operation)
 VALUES(t,site,draft,uuidv7(),staff,1,2,
  jsonb_build_object('operation','set_metadata','payload',
   jsonb_build_object('title',{{canary:draft_operations.operation}})));
 INSERT INTO app.site_releases(tenant_id,site_id,id,release_no,renderer_build_id,manifest,manifest_hash)
 VALUES(t,site,release,1,'privacy-fixture',
  jsonb_build_object('version',1,'locale','en','title',{{canary:site_releases.manifest}},
   'description','','navigation','[]'::jsonb,'products','[]'::jsonb),sha256('privacy-release'::bytea));
 INSERT INTO app.site_release_routes(tenant_id,site_id,release_id,locale,path,page_id,page_revision)
 VALUES(t,site,release,'en','/privacy',page,1);
 INSERT INTO app.site_release_templates(tenant_id,site_id,release_id,locale,template_kind,document)
 VALUES(t,site,release,'en','product',
  jsonb_build_object('title',{{canary:site_release_templates.document}},'blocks','[]'::jsonb));
 PERFORM app.seal_release(t,site,release);
 INSERT INTO app.storefront_serving(tenant_id,site_id,release_id,publication_revision,serving_generation,body,fallback_body,body_sha256,fallback_sha256,valid_until,built_at)
 VALUES(t,site,release,1,1,convert_to({{canary:storefront_serving.body}},'UTF8'),convert_to({{canary:storefront_serving.fallback_body}},'UTF8'),
  sha256('privacy-serving'::bytea),sha256('privacy-serving-fallback'::bytea),now()+interval '5 minutes',now());

 INSERT INTO app.customers(tenant_id,id,email,display_name)
 VALUES(t,customer,{{canary:customers.email}}||'.'||{{canary:customers.email_normalized}}||'.'||
  {{canary:notification_deliveries.recipient}}||'.'||{{canary:customer_auth_tokens.email_normalized}}||'@canary.invalid',
  {{canary:customers.display_name}}),
 (t,approver,'privacy-fixture-approver@canary.invalid','Fixture approver');
 -- Valid PHC syntax with random, unknown material, never a reusable credential.
 -- Mailbox proof goes through the real command; the normalized token-email copy
 -- carries its own canary from the source email. No mail or login is performed.
 synthetic_phc :=
  '$argon2id$v=19$m=19456,t=2,p=1$'||
  left(encode(sha256(uuidv7()::text::bytea),'base64'),22)||'$'||
  left(encode(sha256(uuidv7()::text::bytea),'base64'),43);
 PERFORM app.customer_prepare_registration((SELECT email FROM app.customers WHERE tenant_id=t AND id=customer),
  synthetic_phc,sha256('privacy-verification-fixture'::bytea),clock_timestamp()+interval '1 hour');
 IF NOT app.customer_consume_verification(sha256('privacy-verification-fixture'::bytea)) THEN
  RAISE EXCEPTION 'PRIVACY_CUSTOMER_VERIFICATION_WITNESS_MISSING';
 END IF;
 INSERT INTO app.customer_addresses(tenant_id,customer_id,recipient,address_line1,address_line2,
  city,subdivision,postal_code,country_code)
 VALUES(t,customer,{{canary:customer_addresses.recipient}},{{canary:customer_addresses.address_line1}},
  {{canary:customer_addresses.address_line2}},{{canary:customer_addresses.city}},
  {{canary:customer_addresses.subdivision}},{{canary:customer_addresses.postal_code}},'US');
 INSERT INTO app.buyer_companies(tenant_id,id,name) VALUES(t,company,'Privacy fixture buyer');
 -- Existing tenant-provisioning trigger seeds the fixed vocabulary.
 INSERT INTO app.buyer_company_members(tenant_id,buyer_company_id,customer_id,role_code)
 VALUES(t,company,customer,'purchaser'),(t,company,approver,'approver');
 INSERT INTO app.inquiries(tenant_id,site_id,id,buyer_company_id,buyer_email,buyer_name,message)
 VALUES(t,site,inquiry,company,{{canary:inquiries.buyer_email}}||'@canary.invalid',
  {{canary:inquiries.buyer_name}},{{canary:inquiries.message}});

 INSERT INTO app.products(tenant_id,id,product_code,lifecycle_status) VALUES(t,product,'privacy-fixture','ready');
 INSERT INTO app.product_variants(tenant_id,id,product_id,sku,option_key,lifecycle_status)
 VALUES(t,sku,product,'privacy-fixture-sku','default','ready');
 INSERT INTO app.media_blobs(tenant_id,id,backend,storage_key,sha256,size_bytes,status)
 VALUES(t,blob,'local','privacy-fixture-no-file',sha256('privacy fixture blob'::bytea),0,'ready');
 INSERT INTO app.media_assets(tenant_id,blob_id,filename,mime_type)
 VALUES(t,blob,{{canary:media_assets.filename}}||'.txt','text/plain');
 INSERT INTO app.media_uploads(tenant_id,actor_id,original_filename,expires_at)
 VALUES(t,staff,{{canary:media_uploads.original_filename}}||'.txt',clock_timestamp()+interval '1 hour');

 -- App commands preserve their real ownership fences and emit derivative rows.
 PERFORM app.inventory_adjust(sku,20,'privacy-unowned-stock-control',uuidv7(),'inbound');
 reservation := app.inventory_reserve(sku,1,'order',registered_order,'976f7307-0975-5b80-9fef-0f54beefb77a',900);
 registered := app.orders_create(registered_order,'ebcb4599-59ca-5ee8-9ed9-43166edb6dc5',
  sha256('ebcb4599-59ca-5ee8-9ed9-43166edb6dc5'::bytea),customer,NULL,'USD',5,
  jsonb_build_object('method','fixed','contact',{{canary:orders.shipping_snapshot}}),
  jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id',sku,'sku','privacy-fixture-sku',
   'quantity',1,'unit_price_minor',100,'line_net_minor',100,'line_tax_minor',10,'line_total_minor',110,
   'reservation_id',reservation.id,'tax_snapshot',jsonb_build_object('category','standard','rate_bp',1000,
    'contact',{{canary:order_lines.tax_snapshot}}))),'[]'::jsonb);
 INSERT INTO app.carts(tenant_id,id,customer_id,currency) VALUES(t,cart,customer,'USD');
 INSERT INTO app.cart_lines(tenant_id,cart_id,sku_id,quantity) VALUES(t,cart,sku,1);
 INSERT INTO app.checkout_sessions(tenant_id,id,actor_kind,actor_id,request_key,request_hash,
  cart_id,cart_version,order_id,address_snapshot,currency,amount_minor,items_net_minor,tax_minor,
  shipping_minor,payable_version)
 VALUES(t,checkout,'customer',customer::text,'c29e1592-1a74-5264-8f0d-9282ee207187',sha256('c29e1592-1a74-5264-8f0d-9282ee207187'::bytea),
  cart,1,registered_order,jsonb_build_object('recipient',{{canary:checkout_sessions.address_snapshot}}),
  registered.currency,registered.total_minor,registered.items_net_minor,registered.tax_minor,
  registered.shipping_minor,registered.version);
 -- Checkout's immutable start/final receipts are set once during legal phases.
 UPDATE app.checkout_sessions SET phase='ready',provider_ref='privacy-local-provider-no-io',
  start_result=jsonb_build_object('contact',{{canary:checkout_sessions.start_result}}),
  updated_at=clock_timestamp() WHERE tenant_id=t AND id=checkout;
 UPDATE app.checkout_sessions SET phase='cancelling',claim_kind='cancel',claim_key='0500dffb-2fbd-5351-98af-6c3f55008cdf',
  updated_at=clock_timestamp() WHERE tenant_id=t AND id=checkout;
 PERFORM app.orders_transition(registered_order,'cancelled','0500dffb-2fbd-5351-98af-6c3f55008cdf');
 UPDATE app.checkout_sessions SET phase='cancelled',
  final_result=jsonb_build_object('contact',{{canary:checkout_sessions.final_result}}),
  updated_at=clock_timestamp() WHERE tenant_id=t AND id=checkout;
 INSERT INTO app.checkout_attempts(tenant_id,event_kind,event_key,checkout_id,request_hash)
 VALUES(t,'cancel','0500dffb-2fbd-5351-98af-6c3f55008cdf',checkout,sha256('0500dffb-2fbd-5351-98af-6c3f55008cdf'::bytea));
 UPDATE app.checkout_attempts SET result=jsonb_build_object('contact',{{canary:checkout_attempts.result}})
 WHERE tenant_id=t AND event_kind='cancel' AND event_key='0500dffb-2fbd-5351-98af-6c3f55008cdf';

 -- Create a paid registered-customer order so the notification recipient
 -- witness belongs to the verified customer, not only to an unrelated guest.
 -- No Sender is invoked by these database-only fixtures.
 reservation := app.inventory_reserve(sku,1,'order',notification_order,'239b4edd-bd2f-5e98-9ca6-5d80e7e20afb',900);
 PERFORM app.orders_create(notification_order,'a199898a-f3e2-589a-9959-d941a006eb7c',
  sha256('a199898a-f3e2-589a-9959-d941a006eb7c'::bytea),customer,NULL,'USD',0,'{}'::jsonb,
  jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id',sku,'sku','privacy-fixture-sku',
   'quantity',1,'unit_price_minor',100,'line_net_minor',100,'line_tax_minor',0,'line_total_minor',100,
   'reservation_id',reservation.id,'tax_snapshot','{"category":"standard"}'::jsonb)),'[]'::jsonb);
 PERFORM app.orders_transition(notification_order,'paid','privacy-notification-payment');
 IF NOT EXISTS(SELECT 1 FROM app.notification_deliveries n JOIN app.orders o
  ON (o.tenant_id,o.id)=(n.tenant_id,n.reference_id)
  WHERE n.tenant_id=t AND o.id=notification_order AND o.customer_id=customer
   AND position({{canary:notification_deliveries.recipient}} in n.recipient)>0) THEN
  RAISE EXCEPTION 'PRIVACY_NOTIFICATION_CUSTOMER_WITNESS_MISSING';
 END IF;

 -- Independent paid stock graph: the earlier checkout order is cancelled and
 -- cannot authorize fulfillment. Commands retain all real guards, quantity
 -- bounds and immutable receipt writes; no PSP, ledger post or worker is run.
 reservation := app.inventory_reserve(sku,2,'order',fulfillment_order,'53affb47-ebb8-5586-ae58-2e622fa3e863',900);
 registered := app.orders_create(fulfillment_order,'7149a2b6-fc2d-5356-a134-0b76858eaa41',
  sha256('7149a2b6-fc2d-5356-a134-0b76858eaa41'::bytea),customer,NULL,'USD',0,'{}'::jsonb,
  jsonb_build_array(jsonb_build_object('id',fulfillment_line,'sku_id',sku,'sku','privacy-fixture-sku',
   'quantity',2,'unit_price_minor',100,'line_net_minor',200,'line_tax_minor',0,'line_total_minor',200,
   'reservation_id',reservation.id,'tax_snapshot','{"category":"standard"}'::jsonb)),'[]'::jsonb);
 registered := app.orders_transition(fulfillment_order,'paid','privacy-fulfillment-payment');
 shipment_receipt := app.fulfillment_create_shipment(fulfillment_order,registered.version,
  '2bf3445d-a336-5edb-9453-6bc0c9229e9e',{{canary:shipments.carrier}},{{canary:shipments.tracking_number}},
  jsonb_build_array(jsonb_build_object('order_line_id',fulfillment_line,'quantity',1)));
 SELECT id INTO STRICT shipment_line FROM app.shipment_lines
 WHERE tenant_id=t AND shipment_id=(shipment_receipt->>'id')::uuid AND order_line_id=fulfillment_line;
 SELECT * INTO STRICT registered FROM app.orders WHERE tenant_id=t AND id=fulfillment_order;
 -- Each immutable response column has its own distinct token in the reason
 -- copied by fulfillment_return_json. The gate reads each persisted cell,
 -- rather than accepting a marker in only the authoritative reason column.
 return_receipt := app.returns_create(fulfillment_order,registered.version,'1bfb3e1a-a27d-5030-a5ed-5c776cc65aea',
  'shipment_return',{{canary:returns.reason}}||' '||{{canary:returns.creation_response}}||' '||
   {{canary:returns.receive_response}},
  jsonb_build_array(jsonb_build_object('order_line_id',fulfillment_line,'shipment_line_id',shipment_line,'quantity',1)));
 return_id := (return_receipt->>'id')::uuid;
 SELECT * INTO STRICT registered FROM app.orders WHERE tenant_id=t AND id=fulfillment_order;
 PERFORM app.returns_receive(return_id,registered.version,'bc24d441-823e-5559-84dc-8ac0a5faa166',true);
 IF NOT EXISTS(SELECT FROM app.inventory_movements WHERE tenant_id=t AND owner_type='return'
  AND owner_id=return_id AND reason='return_received') THEN
  RAISE EXCEPTION 'PRIVACY_GATE return restock copied personal text'; END IF;
 -- Historical/imported derived copy, inserted with every live constraint and
 -- immutable guard intact. Its paired stock delta keeps the ledger balanced.
 WITH stock AS (UPDATE app.stock_levels SET on_hand=on_hand+1 WHERE tenant_id=t AND sku_id=sku RETURNING *)
 INSERT INTO app.inventory_movements(tenant_id,warehouse_id,sku_id,owner_type,owner_id,kind,quantity,
  on_hand_delta,reserved_delta,on_hand_after,reserved_after,reason)
 SELECT t,stock.warehouse_id,sku,'return',return_id,'adjustment',1,1,0,stock.on_hand,stock.reserved,
  {{canary:inventory_movements.reason}}||' '||(return_receipt->>'reason') FROM stock;

 INSERT INTO app.ledger_accounts(tenant_id,id,code,currency,account_type) VALUES
  (t,income_account,'privacy-return-income','USD','revenue'),
  (t,tax_account,'privacy-return-tax','USD','liability'),
  (t,shipping_account,'privacy-return-shipping','USD','revenue'),
  (t,cash_account,'privacy-return-cash','USD','asset');
 SELECT * INTO STRICT registered FROM app.orders WHERE tenant_id=t AND id=fulfillment_order;
 PERFORM app.returns_prepare_refund(return_id,registered.version,'07e4bf79-6f27-5978-9811-4375a0803602',
  'opaque-privacy-refund-capture',1,income_account,tax_account,shipping_account,cash_account);
 PERFORM app.returns_fail_refund(return_id,'07e4bf79-6f27-5978-9811-4375a0803602',false);
 PERFORM set_config('privacy.fixture.return_id',return_id::text,true);

 -- Guest contact retains its own independent column canary.
 reservation := app.inventory_reserve(sku,1,'order',guest_order,'f0900b0a-08d4-5c64-b0f7-25e02c9f3247',900);
 PERFORM app.orders_create(guest_order,'2835155d-8253-5aec-a714-3d27fcaa020d',sha256('2835155d-8253-5aec-a714-3d27fcaa020d'::bytea),
  NULL,jsonb_build_object('email',{{canary:orders.guest_contact}}||'@canary.invalid'),'USD',0,'{}'::jsonb,
  jsonb_build_array(jsonb_build_object('id',uuidv7(),'sku_id',sku,'sku','privacy-fixture-sku',
   'quantity',1,'unit_price_minor',100,'line_net_minor',100,'line_tax_minor',0,'line_total_minor',100,
   'reservation_id',reservation.id,'tax_snapshot','{"category":"standard"}'::jsonb)),
  jsonb_build_array(jsonb_build_object('id',uuidv7(),'kind','surcharge','amount_minor',1,
   'reason',{{canary:order_adjustments.reason}}),
   jsonb_build_object('id',uuidv7(),'kind','discount','amount_minor',-1,'reason','fixture balancing discount')));
 -- The same column canaries must be removed from BOTH ownership paths. A
 -- linked guest checkout exercises all immutable result/address copies too.
 INSERT INTO app.carts(tenant_id,id,guest_token_hash,currency)
 VALUES(t,guest_cart,sha256('privacy-guest-fixture-token'::bytea),'USD');
 INSERT INTO app.checkout_sessions(tenant_id,id,actor_kind,actor_id,request_key,request_hash,
  cart_id,cart_version,order_id,address_snapshot,currency,amount_minor,items_net_minor,tax_minor,
  shipping_minor,payable_version)
 SELECT t,guest_checkout,'guest',encode(sha256('privacy-guest-fixture-token'::bytea),'hex'),'privacy-guest-checkout',sha256('privacy-guest-checkout'::bytea),
  guest_cart,1,id,jsonb_build_object('recipient',{{canary:checkout_sessions.address_snapshot}}),
  currency,total_minor,items_net_minor,tax_minor,shipping_minor,version
 FROM app.orders WHERE tenant_id=t AND id=guest_order;
 UPDATE app.checkout_sessions SET phase='ready',provider_ref='privacy-guest-local-no-io',
  start_result=jsonb_build_object('contact',{{canary:checkout_sessions.start_result}})
 WHERE tenant_id=t AND id=guest_checkout;
 UPDATE app.checkout_sessions SET phase='confirming',claim_kind='payment',claim_key='f898f73c-a708-5563-b5e9-b603ef4c0a4d'
 WHERE tenant_id=t AND id=guest_checkout;
 PERFORM app.orders_transition(guest_order,'paid','f898f73c-a708-5563-b5e9-b603ef4c0a4d');
 UPDATE app.checkout_sessions SET phase='paid',final_result=jsonb_build_object('contact',{{canary:checkout_sessions.final_result}})
 WHERE tenant_id=t AND id=guest_checkout;
 INSERT INTO app.checkout_attempts(tenant_id,event_kind,event_key,checkout_id,request_hash)
 VALUES(t,'payment','f898f73c-a708-5563-b5e9-b603ef4c0a4d',guest_checkout,sha256('f898f73c-a708-5563-b5e9-b603ef4c0a4d'::bytea));
 UPDATE app.checkout_attempts SET result=jsonb_build_object('contact',{{canary:checkout_attempts.result}})
 WHERE tenant_id=t AND event_kind='payment' AND event_key='f898f73c-a708-5563-b5e9-b603ef4c0a4d';
 -- Mailbox-proven link (customer_guest_order_links): the verified customer now
 -- owns this guest order, its contact copy, adjustment text and notifications.
 INSERT INTO app.customer_guest_order_links(tenant_id,customer_id,order_id) VALUES(t,customer,guest_order);

 quote := app.create_quote(t,site,inquiry,company,staff,'USD',100,10,5,115,
  jsonb_build_object('contact',{{canary:quotes.snapshot}}),clock_timestamp()+interval '1 hour',
  jsonb_build_array(jsonb_build_object('line_no',1,'product_id',product,'variant_id',sku,
   'description',{{canary:quote_lines.description}},'quantity',1,'unit_price',100,'line_total',100,
   'snapshot',jsonb_build_object('contact',{{canary:quote_lines.snapshot}}))));
 PERFORM set_config('privacy.fixture.quote_id',quote::text,true);
 -- DERIVED COPY: a staff create receipt predates the verified customer's acceptance.
 PERFORM app.idempotency_claim(t,'user',staff,'quotes.create','0069ff46-95a5-5778-8155-62116e1adbdb',
  sha256('privacy-staff-quote-receipt'::bytea),interval '1 hour',inline_claim);
 PERFORM set_config('app.idempotency_claim',inline_claim::text,true);
 UPDATE app.idempotency_records SET state='completed',response_status=201,
  response_body=jsonb_build_object('id',quote,'snapshot',jsonb_build_object(
   'contact',{{canary:idempotency_records.response_body}})),completed_at=clock_timestamp()
 WHERE tenant_id=t AND actor_kind='user' AND actor_id=staff
  AND operation='quotes.create' AND idempotency_key='0069ff46-95a5-5778-8155-62116e1adbdb';

 -- An unrelated staff receipt must survive; no quote ID ownership is inferred.
 PERFORM app.idempotency_claim(t,'user',staff,'quotes.create','b00781ad-8b81-5ca6-bbe0-a74316e10354',
  sha256('privacy-unrelated-staff'::bytea),interval '1 hour',uuidv7());
 PERFORM set_config('app.idempotency_claim',(SELECT claim_token::text FROM app.idempotency_records
  WHERE tenant_id=t AND actor_id=staff AND operation='quotes.create' AND idempotency_key='b00781ad-8b81-5ca6-bbe0-a74316e10354'),true);
 UPDATE app.idempotency_records SET state='completed',response_status=201,
  response_body=jsonb_build_object('id',uuidv7(),'snapshot','privacy-unrelated-staff-control'),completed_at=clock_timestamp()
 WHERE tenant_id=t AND actor_kind='user' AND actor_id=staff
  AND operation='quotes.create' AND idempotency_key='b00781ad-8b81-5ca6-bbe0-a74316e10354';
 PERFORM app.transition_quote(t,quote,1,'send','staff',staff);
 PERFORM app.transition_quote(t,quote,2,'accept','customer',customer);
 PERFORM app.transition_quote(t,quote,3,'approve','customer',approver);

 -- Shared derived copies are distinct from authoritative customer/profile rows.
 INSERT INTO app.audit_events(tenant_id,actor_kind,actor_id,operation,resource_type,resource_id,payload)
 VALUES(t,'customer',customer,'privacy.fixture','customer',customer,
  jsonb_build_object('contact',{{canary:audit_events.payload}}));
 INSERT INTO app.outbox_events(tenant_id,event_type,payload,delivery_target,dedup_key,status,last_error)
 VALUES(t,'privacy.fixture',jsonb_build_object('contact',{{canary:outbox_events.payload}}),
  'privacy.fixture','privacy-fixture','failed',{{canary:outbox_events.last_error}});
 INSERT INTO app.webhook_inbox(tenant_id,provider,provider_account_id,environment,provider_event_id,
  payload_hash,payload_reference)
 VALUES(t,'privacy-local-fixture','privacy-fixture','test','privacy-fixture',
  sha256('privacy-webhook'::bytea),{{canary:webhook_inbox.payload_reference}});
 -- River owns the queue schema; its payloads can still retain tenant contacts.
 -- This unused fixture kind has no registered worker and cannot send a message.
 INSERT INTO queue.river_job(kind,queue,args,errors,metadata,max_attempts)
 VALUES('privacy_canary_fixture','privacy_canary_fixture',
  jsonb_build_object('tenant_id',t,'contact',{{canary:queue.river_job.args}}),
  ARRAY[jsonb_build_object('error',{{canary:queue.river_job.errors}})],
  jsonb_build_object('contact',{{canary:queue.river_job.metadata}}),1);
 PERFORM app.idempotency_claim(t,'customer',customer,'quotes.privacy-fixture','28d4b13d-c3f7-5ecf-ade1-a5976a50aea2',
  sha256('privacy-inline-receipt'::bytea),interval '1 hour',inline_claim);
 PERFORM set_config('app.idempotency_claim',inline_claim::text,true);
 UPDATE app.idempotency_records SET state='completed',response_status=200,
  response_body=jsonb_build_object('contact',{{canary:idempotency_records.response_body}}),
  completed_at=clock_timestamp()
 WHERE tenant_id=t AND actor_kind='customer' AND actor_id=customer
  AND operation='quotes.privacy-fixture' AND idempotency_key='28d4b13d-c3f7-5ecf-ade1-a5976a50aea2';
 PERFORM app.idempotency_claim(t,'customer',customer,'privacy-fixture-reference','a32cdbb3-29cb-5695-8add-bebc2434fded',
  sha256('privacy-reference-receipt'::bytea),interval '1 hour',reference_claim);
 PERFORM set_config('app.idempotency_claim',reference_claim::text,true);
 UPDATE app.idempotency_records SET state='completed',response_status=200,
  response_body_reference={{canary:idempotency_records.response_body_reference}},completed_at=clock_timestamp()
 WHERE tenant_id=t AND actor_kind='customer' AND actor_id=customer
  AND operation='privacy-fixture-reference' AND idempotency_key='a32cdbb3-29cb-5695-8add-bebc2434fded';
END $privacy_fixture$;
