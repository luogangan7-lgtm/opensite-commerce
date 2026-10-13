-- Responsibility: quote/inquiry reads and atomic quote commands for quotes store.
-- Dependencies: forward quote commands, shared receipts and tax/shipping rules; caller:quotes module.
-- Invariants: actor-filtered reads, tenant transaction, shared fenced receipts;
-- JSON rule fields match pure calculators; no customer contact goes in outbox.
-- name: SubmitInquiry :one
INSERT INTO app.inquiries(tenant_id,site_id,buyer_email,buyer_name,message,product_id,quantity)
VALUES(sqlc.arg(tenant_id)::uuid,sqlc.arg(site_id)::uuid,sqlc.arg(email)::text,sqlc.narg(name)::text,sqlc.arg(message)::text,sqlc.narg(product_id)::uuid,sqlc.narg(quantity)::numeric)
RETURNING id,site_id,status;

-- name: CreateQuote :one
SELECT app.create_quote(sqlc.arg(tenant_id)::uuid,sqlc.arg(site_id)::uuid,sqlc.arg(inquiry_id)::uuid,
 sqlc.arg(buyer_company_id)::uuid,sqlc.arg(staff_actor_id)::uuid,sqlc.arg(currency)::text,
 sqlc.arg(subtotal)::bigint,sqlc.arg(tax)::bigint,sqlc.arg(shipping)::bigint,sqlc.arg(total)::bigint,
 sqlc.arg(snapshot)::jsonb,sqlc.arg(expires_at)::timestamptz,sqlc.arg(lines)::jsonb)::uuid AS quote_id;

-- name: GetQuote :one
-- Buyers see a quote only after it is sent; drafts stay seller-internal.
SELECT q.* FROM app.quotes q WHERE q.tenant_id=sqlc.arg(tenant_id)::uuid AND q.id=sqlc.arg(quote_id)::uuid
 AND (sqlc.arg(actor_kind)::text<>'customer' OR q.status<>'draft')
 AND app.quote_actor_access(q.tenant_id,q.buyer_company_id,sqlc.arg(actor_kind)::text,sqlc.arg(actor_id)::uuid,'read');

-- name: GetAcceptedQuote :one
-- One statement decides authority, approval and DB-clock expiry and returns
-- that same row version, so a concurrent commit cannot pair a stale row with a fresh check.
SELECT q.* FROM app.quotes q WHERE q.tenant_id=sqlc.arg(tenant_id)::uuid AND q.id=sqlc.arg(quote_id)::uuid
 AND q.status='approved' AND q.expires_at>clock_timestamp()
 AND app.quote_actor_access(q.tenant_id,q.buyer_company_id,sqlc.arg(actor_kind)::text,sqlc.arg(actor_id)::uuid,'read');

-- name: GetQuoteLines :many
SELECT * FROM app.quote_lines WHERE tenant_id=sqlc.arg(tenant_id)::uuid AND quote_id=sqlc.arg(quote_id)::uuid ORDER BY line_no;

-- name: LockQuoteConversion :one
SELECT * FROM app.quote_conversion_lock(sqlc.arg(tenant_id)::uuid,sqlc.arg(quote_id)::uuid,
 sqlc.arg(company_id)::uuid,sqlc.arg(customer_id)::uuid);

-- name: BindQuoteConversion :one
SELECT * FROM app.quote_conversion_bind(sqlc.arg(tenant_id)::uuid,sqlc.arg(quote_id)::uuid,
 sqlc.arg(company_id)::uuid,sqlc.arg(customer_id)::uuid,sqlc.arg(order_id)::uuid,
 sqlc.arg(quote_version)::bigint,sqlc.arg(idempotency_key)::text,sqlc.arg(request_hash)::bytea);

-- name: GetQuoteConversion :one
SELECT c.*,o.state FROM app.quote_conversion_get(sqlc.arg(tenant_id)::uuid,sqlc.arg(quote_id)::uuid,
 sqlc.narg(company_id)::uuid,sqlc.arg(actor_kind)::text,sqlc.arg(actor_id)::uuid) c
 JOIN app.orders o ON(o.tenant_id,o.id)=(c.tenant_id,c.order_id);

-- name: GetQuoteConversionSKU :one
-- This is only a label lookup. Negotiated amounts never read live price lists.
SELECT sku FROM app.product_variants WHERE tenant_id=sqlc.arg(tenant_id)::uuid
 AND product_id=sqlc.arg(product_id)::uuid AND id=sqlc.arg(variant_id)::uuid;

-- name: GetQuoteApprovals :many
SELECT * FROM app.quote_approvals WHERE tenant_id=sqlc.arg(tenant_id)::uuid AND quote_id=sqlc.arg(quote_id)::uuid;

-- name: QuoteActorAccess :one
SELECT app.quote_actor_access(sqlc.arg(tenant_id)::uuid,sqlc.arg(buyer_company_id)::uuid,
 sqlc.arg(actor_kind)::text,sqlc.arg(actor_id)::uuid,sqlc.arg(operation)::text)::boolean AS allowed;

-- name: TransitionQuote :one
SELECT app.transition_quote(sqlc.arg(tenant_id)::uuid,sqlc.arg(quote_id)::uuid,sqlc.arg(expected_version)::bigint,
 sqlc.arg(action)::text,sqlc.arg(actor_kind)::text,sqlc.arg(actor_id)::uuid)::bigint AS version;

-- name: QuoteClaimIdempotency :one
SELECT r.* FROM app.idempotency_claim(sqlc.arg(tenant_id)::uuid,sqlc.arg(actor_kind)::text,sqlc.arg(actor_id)::uuid,
 sqlc.arg(operation)::text,sqlc.arg(idempotency_key)::text,sqlc.arg(request_hash)::bytea,
 make_interval(secs=>sqlc.arg(ttl_seconds)::double precision),sqlc.arg(claim_token)::uuid) r;

-- name: GetQuoteTaxRules :one
SELECT jsonb_build_object(
 'zones',coalesce((SELECT jsonb_agg(jsonb_build_object('id',z.id,'locations',coalesce((SELECT jsonb_agg(jsonb_build_object('country',l.country_code,'subdivision',l.subdivision) ORDER BY l.country_code,l.subdivision)
 FROM app.tax_zone_locations l WHERE l.tenant_id=z.tenant_id AND l.zone_id=z.id),'[]'::jsonb)) ORDER BY z.id) FROM app.tax_zones z WHERE z.tenant_id=s.tenant_id),'[]'::jsonb),
 'categories',coalesce((SELECT jsonb_agg(jsonb_build_object('code',c.code,'is_exempt',c.is_exempt) ORDER BY c.code) FROM app.tax_categories c WHERE c.tenant_id=s.tenant_id),'[]'::jsonb),
 'rates',coalesce((SELECT jsonb_agg(jsonb_build_object('id',r.id,'zone_id',r.zone_id,'category',r.category_code,'basis_points',r.rate_bp,'valid_from',r.valid_from,'valid_until',r.valid_until) ORDER BY r.id)
 FROM app.tax_rates r WHERE r.tenant_id=s.tenant_id),'[]'::jsonb))::jsonb AS rules,
 s.prices_include_tax FROM app.tax_settings s WHERE s.tenant_id=sqlc.arg(tenant_id)::uuid;

-- name: GetQuoteShippingRules :one
SELECT jsonb_build_object(
 'zones',coalesce((SELECT jsonb_agg(jsonb_build_object('ID',z.id,'Destinations',coalesce((SELECT jsonb_agg(jsonb_build_object('Country',d.country_code,'Subdivision',d.subdivision_code) ORDER BY d.country_code,d.subdivision_code)
 FROM app.shipping_zone_destinations d WHERE d.tenant_id=z.tenant_id AND d.zone_id=z.id),'[]'::jsonb)) ORDER BY z.id) FROM app.shipping_zones z WHERE z.tenant_id=p.tenant_id
  -- ADR-C13a: only zones this profile offers, so another profile's narrower zone cannot shadow them.
  AND EXISTS(SELECT FROM app.shipping_rates zr WHERE zr.tenant_id=z.tenant_id AND zr.profile_id=p.id AND zr.zone_id=z.id)),'[]'::jsonb),
 'rates',coalesce((SELECT jsonb_agg(jsonb_build_object('ID',r.id,'ProfileID',r.profile_id,'ZoneID',r.zone_id,'Method',r.method,'Currency',r.currency,'Amount',r.amount,'MinWeightG',coalesce(r.min_weight_g,0),'MaxWeightG',r.max_weight_g,'FreeOverAmount',r.free_over_amount) ORDER BY r.id)
 FROM app.shipping_rates r WHERE r.tenant_id=p.tenant_id AND r.profile_id=p.id),'[]'::jsonb))::jsonb AS rules
FROM app.shipping_profiles p WHERE p.tenant_id=sqlc.arg(tenant_id)::uuid AND p.id=sqlc.arg(profile_id)::uuid;
