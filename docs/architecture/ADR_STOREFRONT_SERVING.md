# ADR: Storefront serving projection (C-15b / C-15c)

- Status: accepted 2026-10-13 with the amendments in §7; C-15b implemented in the same PR
- Date: 2026-10-13
- Base: `origin/main` b296a2a
- Supersedes: the design in PR #55 (live SKU joins on the anonymous read path). #55 will be
  closed when the C-15b implementation lands.
- Owners: C-15b (publish-time serving projection), C-15c (refresh jobs and fallback)

## 1. Context

The storefront needs purchasable SKUs to add items to a cart:
`{sku_id, sku_code, option_values, price_minor, currency, available}`.
The public read must also return the host-resolved `tenant_id` so a hostname-only storefront
can call `/api/v1/t/{tenant_id}/carts`.

PR #55 built these from live joins at read time. The joins ran as the anonymous
`app_domain_resolver` role over catalog, pricing and inventory tables, and access was
granted through chained RLS policies. Twenty-one review threads on #55 fell into four
recurring classes:

| Class | Examples from #55 |
|---|---|
| RLS scope, recursion and performance | variant ↔ price-entry policy recursion forced looser entry visibility; correlated policies re-validated the 024 release per row and hit the 5 s statement timeout at 4,001 SKUs |
| Cardinality and limits on live data | global 5,000-SKU sentinel, 100 SKUs per product, price rows per SKU, unpriced rows counted toward `LIMIT` |
| Read-time validation of mutable data | one bad row returned 503 for the whole site: uppercase or duplicate product ids, `±infinity` list bounds, oversized codes or options, currency mismatches |
| Publish-time vs read-time budget mismatch | `tenant_id` and SKUs were added after the publish-time 1 MiB check |

Root cause: an immutable, validated, bounded snapshot was mixed with unbounded live
relational work on the anonymous path. Every new live input added a new failure mode on a
path that can take the whole storefront down.

## 2. Decision

### 2.1 Immutable release, including SKU membership

- A release seals its product views (as today) and the purchasable SKU membership of each
  product: `sku_id`, `sku_code` and `option_values` (option code and label). Membership is
  fixed at publish time.
- Adding SKUs, or offering a SKU that had no eligible price when the release was sealed,
  requires a republish. Removing or archiving a SKU after publish does not change the
  release. The serving projection marks it not purchasable (§2.3); cart and checkout
  re-validate it.
- Storage: a new immutable table `app.site_release_skus(tenant_id, site_id, release_id,
  product_id, sku_id, ordinal, sku_code, option_values jsonb)`, written inside the publish
  transaction. The same guard trigger as other release children blocks writes once the
  release is sealed. The version-1 manifest and the 024 manifest shape stay unchanged, so
  restoring an older release remains possible.
- A release sealed before C-15b has no SKU rows. Restoring it serves the release with
  `skus: []` until it is republished.

### 2.2 Rebuildable serving row per (tenant, site)

New table `app.storefront_serving`, one row per `(tenant_id, site_id)`. It is a derived
artifact that can be rebuilt from the release plus live price and stock.

| Column | Meaning |
|---|---|
| `tenant_id, site_id` | primary key |
| `release_id` | release the body was built from |
| `publication_revision` | `sites.publication_revision` at build time (031 epoch) |
| `serving_generation` | bigint, +1 on every successful write; compare-and-swap token |
| `body` | `bytea`: exact HTTP response body of `getPublishedSite` (UTF-8 JSON plus the trailing newline) |
| `fallback_body` | `bytea`: same pages and products with `skus: []` everywhere (never stale) |
| `body_sha256` | `sha256(body)`; check value and ETag source |
| `valid_until` | `timestamptz`: in C-15b, the next start or end boundary of any price list (in the product's currency) that prices a member SKU; `NULL` if none. C-15c adds a staleness cap (§7) |
| `built_at` | build time (the transaction's `now()`) |

Public read rule: serve `body` while `now() < valid_until` (or `valid_until IS NULL`),
otherwise serve `fallback_body`. A stale serving row therefore degrades to "no purchasable
SKUs". It never returns a site-wide 503 and never shows an expired price.

### 2.3 Built in Go, inside the publish (and restore) tenant transaction

`WriteStore` already runs `cms_publish` and `cms_restore` and then `projectSealed` in one
tenant transaction. C-15b adds one step in the same transaction, before commit:

1. **Read inputs** under the tenant transaction with normal tenant RLS, for the release's
   products only. Inputs: ready variants and option labels, quantity-1 entries of active
   `audience='all'` lists in the product's release currency, and stock rows.
   A separate **input scan budget** applies (default 20,000 variant rows and
   100,000 price rows). Exceeding it rejects the publish with a typed error
   (`CMS_SERVING_INPUT_BOUNDS`, HTTP 422). It never causes a 503 on the live site.
2. **Price** each SKU with the single pricing rule through the existing Pricing Port
   (`internal/pricing.Quote`): anonymous (no customer group or buyer company),
   quantity 1, the product's release currency, at the transaction time, with the window
   `[starts_at, ends_at)` and `±infinity` bounds allowed, priority descending, then list id
   ascending. Indexing is per SKU, so the work is linear.
3. **Filter before caps.** Drop unpriced and non-ready SKUs first. Only then apply the
   output caps: at most 100 SKUs per product and 5,000 per site, plus option and text
   limits. Exceeding an output cap rejects the publish (422). Unpriced rows never consume
   a cap.
4. **Seal** the surviving membership into `site_release_skus` (publish only; restore reads
   the target release's rows).
5. **Encode** the exact wire bytes for `body` and `fallback_body` from one Go DTO
   (§2.5) with `encoding/json`, then check the budget on the **exact final bytes**. The
   bytes include the trailing newline and the 36-byte `tenant_id`. Fields that a refresh
   may change are measured at their maximum width: `price_minor` at 19 digits and
   `available` at `false`. Over 1 MiB rejects the publish (422). A later refresh can
   therefore never push the body over budget.
6. **Write** the serving row through one narrow routine,
   `app.cms_serving_put(tenant, site, release, publication_revision,
   expected_generation, body, fallback_body, sha, valid_until)`. It compares and swaps on
   `(publication_revision = sites.publication_revision AND serving_generation =
   expected_generation)`. Any error rolls back the whole publish (atomicity).

`available` is computed as `on_hand − reserved > 0` (012 counts active reservations in
`reserved`). It is a display hint: the cart and checkout reserve against stock
authoritatively.

### 2.4 Anonymous resolver role: three relations only

`app_domain_resolver` keeps SELECT on the domain map (`site_domains` columns), on
publication (`sites.tenant_id, id, published_release_id, publication_revision`) and on the
serving row columns `body, fallback_body, valid_until, publication_revision, tenant_id,
site_id`. RLS on `storefront_serving` admits only the verified host's site, and only while
the row's `publication_revision` equals the current one.

- No grants on catalog, pricing or inventory tables. The PR #55 grants never merged; the
  C-15b migration asserts their absence and `db_role_matrix.sql` stays exact.
- The 024 grants on `site_releases`, `site_release_routes` and `page_revisions` are
  revoked once the serving row replaces the snapshot query.
- `pgxadapter.OpenPublic`'s capability audit is narrowed the same way.
- Public read: one statement, one bounded row. The handler writes the stored bytes as-is
  (no re-encoding) with `ETag: "<body_sha256>"` and `Cache-Control: no-store`. A missing
  row is 404; a row present for the wrong revision is 404 (unpublished).

### 2.5 Separate DTOs

- **Public DTO** `PublishedSite`: `tenant_id`, pages, products with `skus`. It is built
  only by the serving builder.
- **CMS Preview DTO** `SitePreview`: pages and products, with no `tenant_id` and no
  `skus`. It is produced by the draft projection and is unaffected by serving rows.
  `getSitePreview` switches to `SitePreview` in the OpenAPI spec.
- The publish-time draft and release validators keep using the preview projection. The
  public budget is enforced only by the serving builder (§2.3 step 5).

### 2.6 Refresh: C-15c (described here, implemented later)

- Triggers: price-list and price-entry changes, stock and reservation changes (debounced),
  and `valid_until` boundaries. All are queued as idempotent River jobs keyed by
  `(tenant, site)`.
- A refresh job reads the current publication, the release, its `site_release_skus` and
  live price and stock under the tenant transaction. It rebuilds `body`, `valid_until` and
  `body_sha256` with the same builder (`fallback_body` is unchanged), then writes through
  `cms_serving_put` with the generation it read. A stale writer can never
  overwrite a newer publish or newer refresh, because CAS fails on
  `publication_revision` or `serving_generation`; a failed CAS drops the result.
- A SKU archived or priced out after publish stays in membership but is omitted from
  `skus` by the refresh. Republishing is the only way to add SKUs.
- If the worker is down, the read serves `fallback_body` once `valid_until` passes; the
  site stays up.
- Live price default: the storefront shows the asynchronously refreshed serving price.
  Cart and checkout re-price authoritatively (existing cart quote). The owner may revisit
  this (for example a per-SKU live price lookup).

## 3. Acceptance gates (C-15b unless noted)

Each gate must go red when the property is broken; record a fault-injection red→green.

| # | Gate | Test (CI) |
|---|---|---|
| G1 | Publish atomicity | inject a failure in the serving build or CAS write: no new release, no pointer move, no SKU rows, old serving row intact |
| G2 | Exact 1 MiB wire budget | serving body at exactly 1 MiB with max-width variable fields and `tenant_id` is accepted; one byte more is rejected at publish (422); served bytes equal stored bytes (sha) |
| G3 | Unpriced rows don't consume output caps | 6,000 unpriced or ineligible variants plus 10 priced ones publishes; exactly 10 SKUs are served; input budget overrun returns a typed 422, never a 503 |
| G4 | Anonymous SELECT on live tables fails | as the public login: SELECT on `product_variants`, `price_lists`, `price_entries`, `stock_levels`, `inventory_reservations`, `site_release_skus` and other tenants' serving rows returns 42501 or 0 rows; `db_role_matrix.sql` is exact; OpenPublic rejects any extra grant |
| G5 | Stale refresh cannot overwrite a newer publish | (C-15b primitive, C-15c caller) a write with an old `publication_revision` or `serving_generation` fails CAS and leaves the newer row byte-identical |
| G6 | Preview unaffected | preview works with no serving row; it returns `SitePreview` with no `tenant_id` or `skus`; publish failure does not change preview |
| G7 | Fallback, not 503 | after `valid_until`, the read returns `fallback_body` (200, `skus: []`) |
| G8 | Isolation | two tenants with identical product and SKU UUIDs; each host serves only its own body; unknown, unverified or unpublished hosts return 404 |
| G9 | Pricing rule parity | serving `price_minor` equals `pricing.Quote` for qty 1 and the release currency (priority, tiebreak, window, `±infinity`); uppercase manifest ids rejected or canonicalized at seal |
| G10 | Restore | restoring a pre-C-15b release serves `skus: []`; restoring a C-15b release rebuilds from its sealed SKU rows |
| G11 | Refresh correctness (C-15c) | stale `available=true` never oversells (checkout reservation fails cleanly); expired price never quoted; worker down → fallback, site up |

## 4. Reuse from PR #55

- Contract types `SKU` and `OptionValue`, the OpenAPI `PublishedSku` schema, `tenant_id`,
  and the regenerated TS client shape (already consumed by #53's cart pages).
- The one-pass `pricing.Quote` attachment and its 5,000 × 20,000 bounded-work unit test.
  It moves from the read path to the builder.
- The pgx fixtures: two tenants with shared UUIDs, archived, unpriced, unlisted and
  uppercase ids, infinite list bounds, 41 products × 5 currencies × 4,001 SKUs. They
  become publish and serving tests.
- The tenant-placeholder budget test (becomes G2).
- Dropped: the 033 RLS policies and grants, the `stock_levels.in_stock` generated column,
  the live `LiveSKUSQL` path and the per-request Go projection on the public path.

## 5. Consequences

- The anonymous path does O(1) bounded work and has no live joins. Publish does more
  work, but all of it is bounded and fails as a typed 422 the merchant can act on.
- SKU and price changes reach the storefront through republish (membership) or refresh
  (price and availability, C-15c). Until C-15c lands, prices and availability are as of
  publish time, valid until `valid_until`, after which the fallback is served.
- One more derived artifact (the serving row) must stay in sync. That is handled by
  generation CAS plus an always-correct fallback.
- Module ownership: `publish` owns `site_release_skus` and `storefront_serving`. It reads
  catalog, pricing and inventory only inside tenant transactions, through declared
  dependencies (`catalog`, `pricing`, `inventory`), and calls the Pricing Port as its only
  pricing logic.

## 6. Out of scope / open questions

- Per-product offers split or a static snapshot CDN (post-MVP, on measured need).
- Live per-SKU price lookup instead of the async serving price (owner decision; §2.6).
- Migration number: next contiguous at push, or `migrations/pending/<stable-id>` once
  INF-MIG lands.

## 7. Amendments at acceptance and implementation notes (C-15b)

1. **valid_until in C-15b is the next price-list boundary only.** It has no maximum-staleness
   cap: without the C-15c worker, a cap would push every site to the fallback. C-15c sets
   `valid_until = min(next boundary, built_at + max_staleness)` and ships the refresh worker
   at the same time. **Until C-15c lands, the displayed price and availability are as of the
   last publish or restore and can be stale until a republish. Checkout is the authority**:
   the cart re-quotes and checkout reserves stock against live data.
2. **Unpriced or non-ready SKUs at publish are not members.** If they become priced or ready
   later, they need a republish (§2.1).
3. **The public body is host-independent.** A site can have several verified hostnames, and
   one serving row per (tenant, site) must be valid for all of them, so `PublishedSite` no
   longer carries `hostname`. The storefront SSR already uses its request URL. The CMS
   preview (`SitePreview`) keeps `hostname`.
4. **Routines.** SKU membership is written by `app.cms_seal_skus`. It is write-once, only for
   the current release created by the same transaction (`created_at = now()`), and an
   immutability trigger blocks later changes. The serving row is written only by
   `app.cms_serving_put`, which does the compare-and-swap on publication revision and
   generation. Both are `SECURITY DEFINER` routines owned by `app_publish_executor` and
   executable only by `app_runtime`. They are covered by the existing CMS routine matrix.
5. **Legacy releases.** Releases published outside the CMS write path (pre-C-15b data, SQL
   fixtures, `app.publish_release` callers) have no serving row. The public read serves their
   sealed projection with `skus: []` and `tenant_id`, which is byte-identical in shape to a
   fallback body. Revoking the 024 release, route and page grants is therefore deferred until
   C-15c backfills serving rows. `app_domain_resolver` still has no catalog, pricing,
   inventory or membership grants (G4).
6. **Transport.** `contract.Reader.Serve` returns the stored bytes. The HTTP handler writes
   them as-is, with `ETag` set to the body's SHA-256. Errors keep the generated
   problem responses.

