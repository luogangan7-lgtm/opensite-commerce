-- Cart owns carts and cart_lines; depends on customers/catalog/inventory/pricing.
-- Called by migrate and cart sqlc store; runtime is scoped by authenticated actor.
-- Ownership and currency are immutable; one absolute quantity per SKU; version +1.
CREATE TABLE app.carts (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 id uuid NOT NULL DEFAULT uuidv7(),
 customer_id uuid,
 guest_token_hash bytea,
 currency char(3) NOT NULL,
 version bigint NOT NULL DEFAULT 1,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,id),
 CONSTRAINT carts_customer_fk FOREIGN KEY(tenant_id,customer_id) REFERENCES app.customers(tenant_id,id),
 CONSTRAINT carts_owner_check CHECK((customer_id IS NOT NULL AND guest_token_hash IS NULL)
  OR (customer_id IS NULL AND octet_length(guest_token_hash)=32 AND guest_token_hash IS NOT NULL)),
 CONSTRAINT carts_currency_check CHECK(currency::text ~ '^[A-Z]{3}$'),
 CONSTRAINT carts_version_check CHECK(version>0)
);
CREATE TABLE app.cart_lines (
 tenant_id uuid NOT NULL,
 cart_id uuid NOT NULL,
 sku_id uuid NOT NULL,
 quantity bigint NOT NULL,
 PRIMARY KEY(tenant_id,cart_id,sku_id),
 CONSTRAINT cart_lines_cart_fk FOREIGN KEY(tenant_id,cart_id) REFERENCES app.carts(tenant_id,id) ON DELETE CASCADE,
 CONSTRAINT cart_lines_sku_fk FOREIGN KEY(tenant_id,sku_id) REFERENCES app.product_variants(tenant_id,id),
 CONSTRAINT cart_lines_quantity_check CHECK(quantity BETWEEN 1 AND 1000000)
);
-- Runtime cannot read the tenant roster. Reuse the already unreachable
-- inventory executor's existing tenant SELECT for a no-argument boolean only.
CREATE FUNCTION app.cart_tenant_active() RETURNS boolean
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $fn$
 SELECT EXISTS(SELECT FROM app.tenants
  WHERE id=nullif(current_setting('app.tenant_id',true),'')::uuid AND status='active');
$fn$;
GRANT CREATE ON SCHEMA app TO app_inventory_executor;
ALTER FUNCTION app.cart_tenant_active() OWNER TO app_inventory_executor;
REVOKE CREATE ON SCHEMA app FROM app_inventory_executor;
REVOKE ALL ON FUNCTION app.cart_tenant_active() FROM PUBLIC,app_runtime,app_auth,
 app_domain_resolver,app_domain_verifier,app_media_service,app_publisher,
 app_publish_executor,app_outbox_worker,app_reliability_executor,app_customer_auth,
 app_inventory_manager;
GRANT EXECUTE ON FUNCTION app.cart_tenant_active() TO app_runtime;
ALTER TABLE app.carts ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.carts FORCE ROW LEVEL SECURITY;
-- Both actor contexts populated is ambiguous and must fail closed. Cast-free
-- UUID text comparison and guarded hex decoding also make malformed context safe.
CREATE POLICY tenant_scope ON app.carts TO app_runtime
 USING(app.cart_tenant_active() AND tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND (
  (customer_id::text=nullif(current_setting('app.cart_customer_id',true),'')
   AND nullif(current_setting('app.cart_token_hash',true),'') IS NULL
   AND EXISTS(SELECT FROM app.customers c WHERE (c.tenant_id,c.id)=(carts.tenant_id,carts.customer_id) AND c.status='active'))
  OR (customer_id IS NULL AND nullif(current_setting('app.cart_customer_id',true),'') IS NULL
   AND guest_token_hash=CASE WHEN current_setting('app.cart_token_hash',true) ~ '^[0-9a-f]{64}$'
    THEN decode(current_setting('app.cart_token_hash',true),'hex') ELSE NULL END)))
 WITH CHECK(app.cart_tenant_active() AND tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND (
  (customer_id::text=nullif(current_setting('app.cart_customer_id',true),'')
   AND nullif(current_setting('app.cart_token_hash',true),'') IS NULL
   AND EXISTS(SELECT FROM app.customers c WHERE (c.tenant_id,c.id)=(carts.tenant_id,carts.customer_id) AND c.status='active'))
  OR (customer_id IS NULL AND nullif(current_setting('app.cart_customer_id',true),'') IS NULL
   AND guest_token_hash=CASE WHEN current_setting('app.cart_token_hash',true) ~ '^[0-9a-f]{64}$'
    THEN decode(current_setting('app.cart_token_hash',true),'hex') ELSE NULL END)));
ALTER TABLE app.cart_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.cart_lines FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.cart_lines TO app_runtime
 USING(app.cart_tenant_active() AND tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND EXISTS(SELECT FROM app.carts c WHERE (c.tenant_id,c.id)=(cart_lines.tenant_id,cart_lines.cart_id)))
 WITH CHECK(app.cart_tenant_active() AND tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid
  AND EXISTS(SELECT FROM app.carts c WHERE (c.tenant_id,c.id)=(cart_lines.tenant_id,cart_lines.cart_id)));
-- INVOKER adds no capability. Even a direct version write must be exactly +1;
-- the service CAS query additionally requires the previously locked version.
CREATE FUNCTION app.cart_guard_version() RETURNS trigger
 LANGUAGE plpgsql SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $fn$
BEGIN
 IF NEW.version <> OLD.version+1 THEN
  RAISE EXCEPTION 'CART_VERSION_STEP_REQUIRED' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $fn$;
REVOKE ALL ON FUNCTION app.cart_guard_version() FROM PUBLIC;
CREATE TRIGGER cart_version_step BEFORE UPDATE ON app.carts
 FOR EACH ROW EXECUTE FUNCTION app.cart_guard_version();
REVOKE ALL ON app.carts,app.cart_lines FROM PUBLIC,app_runtime;
GRANT SELECT ON app.carts,app.cart_lines TO app_runtime;
GRANT INSERT(tenant_id,id,customer_id,guest_token_hash,currency) ON app.carts TO app_runtime;
GRANT UPDATE(version) ON app.carts TO app_runtime;
GRANT INSERT(tenant_id,cart_id,sku_id,quantity),UPDATE(quantity) ON app.cart_lines TO app_runtime;
GRANT DELETE ON app.cart_lines TO app_runtime;
