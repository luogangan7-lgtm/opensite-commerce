-- Responsibility: reusable populated Orders graph for CI acceptance/upgrades/races.
-- Dependencies: real catalog/inventory/Orders capabilities; no trigger bypasses.
-- Invariant: unique UUID identities, guest and registered financial witnesses.
CREATE FUNCTION pg_temp.mdel03_fixture(p_registered boolean DEFAULT false) RETURNS jsonb
LANGUAGE plpgsql AS $$
DECLARE t uuid; p uuid; sku uuid; c uuid; o uuid:=uuidv7(); l uuid:=uuidv7(); a uuid:=uuidv7(); r app.inventory_reservations;
BEGIN
 INSERT INTO app.tenants(name) VALUES('mdel03-canary-tenant') RETURNING id INTO t;
 PERFORM set_config('app.tenant_id',t::text,true);
 INSERT INTO app.products(tenant_id,product_code) VALUES(t,'mdel03') RETURNING id INTO p;
 INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(t,p,'mdel03-canary-sku','default') RETURNING id INTO sku;
 IF p_registered THEN
  INSERT INTO app.customers(tenant_id,email,display_name) VALUES(t,'mdel03@invalid.test','mdel03-canary-customer') RETURNING id INTO c;
 END IF;
 PERFORM app.inventory_adjust(sku,20,'mdel03 stock',uuidv7(),'inbound');
 r:=app.inventory_reserve(sku,2,'order',o,uuidv4()::text,900);
 PERFORM app.orders_create(o,uuidv4()::text,sha256(o::text::bytea),c,
  CASE WHEN c IS NULL THEN '{"email":"mdel03@invalid.test","name":"mdel03-canary-guest"}'::jsonb END,'USD',20,
  '{"amount_minor":20,"currency":"USD","address":"mdel03-canary-address"}',
  jsonb_build_array(jsonb_build_object('id',l,'sku_id',sku,'sku','mdel03-canary-sku','quantity',2,
   'unit_price_minor',100,'line_net_minor',200,'line_tax_minor',20,'line_total_minor',220,
   'reservation_id',r.id,'tax_snapshot','{"category":"standard","contact":"mdel03-canary-tax"}'::jsonb)),
  jsonb_build_array(jsonb_build_object('id',a,'kind','surcharge','amount_minor',10,'reason','mdel03-canary-adjustment')));
 RETURN jsonb_build_object('t',t,'o',o,'l',l,'a',a,'sku',sku,'c',c);
END $$;
