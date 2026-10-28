# ADR: Storefront serving projection (C-15b / C-15c)

- Status: accepted 2026-10-13; revised after design review (§8). C-15b and C-15c are implemented in this PR.
- Date: 2026-10-13
- Base: `origin/main` b296a2a
- Supersedes: the design in PR #55 (live SKU joins on the anonymous read path). #55 will be
  closed when the C-15b implementation lands.
- Owners: C-15b (publish-time serving projection) and C-15c (refresh and invalidation), folded into one card

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

- A release seals its product views and the purchasable SKU membership of each product:
  `sku_id`, `sku_code` and `option_values` (option code and label). Membership is fixed at
  publish time.
- Unpriced or non-ready SKUs at publish are not members. If they become priced or ready
  later, they need a republish. A member SKU that is archived or priced out after publish
  stays in membership but is omitted from the served `skus` by the next build (§2.6).
- Storage: an immutable table `app.site_release_skus(tenant_id, site_id, release_id,
  product_id, sku_id, ordinal, sku_code, option_values jsonb)`.
- Writing it is atomic with the seal. The release is created, sealed and made current by
  `cms_publish` in the publish transaction. Membership is written in **that same
  transaction**, before commit, by `app.cms_seal_skus`. That routine admits only the
  current release created by the calling transaction (`site_releases.created_at = now()`,
  the transaction timestamp) and only when the release has no membership yet. It does not
  rely on `guard_release_children`, so there is no `RELEASE_SEALED` abort. A trigger
  rejects every UPDATE and DELETE, and no later transaction can add rows. Any failure rolls
  back the release, the pointer move and the membership together.
- A release sealed before this change has no membership rows; it serves `skus: []`. The
  version-1 manifest and the 024 manifest shape stay unchanged.

### 2.2 Rebuildable serving row per (tenant, site)

`app.storefront_serving` has one row per `(tenant_id, site_id)`. It is derived data,
rebuilt from the release, its membership, and live price, stock and readiness.

| Column | Meaning |
|---|---|
| `tenant_id, site_id` | primary key |
| `release_id` | release the body was built from |
| `publication_revision` | `sites.publication_revision` at build time (031 epoch) |
| `serving_generation` | +1 on every successful write; compare-and-swap token |
| `body` | exact `getPublishedSite` response bytes (UTF-8 JSON plus the trailing newline) |
| `fallback_body` | same pages and products with `skus: []` everywhere (never stale) |
| `body_sha256`, `fallback_sha256` | SHA-256 of each representation; the ETag source |
| `valid_until` | `min(next price-list boundary of a member SKU, built_at + max_lifetime)` |
| `built_through_seq` | highest invalidation `seq` folded into this build (observability; §2.6) |
| `built_at` | build time (the DB transaction time) |

A separate **append-only** table holds invalidations:
`app.serving_invalidations(tenant_id, site_id, seq identity, reason, created_at)`, with an
index on `(tenant_id, site_id, created_at)`. It has no foreign keys, so an insert takes no
lock on a shared parent row.

Public read rule: serve `body` and `body_sha256` while `now() < valid_until` **and** the
site has no unfolded invalidation older than the 30-second `grace` (one indexed `EXISTS`).
Otherwise serve `fallback_body` and `fallback_sha256`. A stale row degrades to "no purchasable SKUs".
It never returns a site-wide 503 and never shows a price beyond its bound. `max_lifetime`
defaults to 15 minutes.

### 2.3 Build (publish, restore, refresh): one Go builder

All three entry points use the same builder inside a tenant transaction with normal tenant
RLS:

1. **Lock the publication.** `SELECT ... FROM app.sites ... FOR NO KEY UPDATE` before
   reading any input. This serializes concurrent refreshers. A refresh waits for an
   in-flight publish (which holds the row `FOR UPDATE`), then reads the committed revision,
   or drops itself if the revision changed. Publish and restore already hold the row lock.
2. **Read inputs** for the release's products only:
   - live readiness (variant and product `lifecycle_status`);
   - quantity-1 entries of active `audience='all'` lists that have not ended;
   - stock (`on_hand − reserved > 0`; 012 counts active reservations in `reserved`).

   Publish reads ready variants and their option labels. Restore and refresh read the
   sealed membership and join live readiness.

   A separate **input scan budget** applies: 20,000 variant rows and 100,000 price rows.
   Exceeding it rejects a publish with a typed 422; a refresh writes the fallback instead.
   `FOR NO KEY UPDATE` does not conflict with foreign-key `KEY SHARE` checks from other
   modules.
3. **Price** each SKU with the single pricing rule through the Pricing Port
   (`internal/pricing.Quote`): anonymous, quantity 1, the product's release currency, at
   DB transaction time, within the window `[starts_at, ends_at)` with `±infinity`
   allowed, priority descending then list id ascending. Indexing is per SKU, so the work
   is linear.
4. **Filter before caps.** Drop unpriced and non-ready SKUs first, then apply the output
   caps: 100 SKUs per product, 5,000 per site, plus option and text limits. Exceeding a cap
   rejects a publish (422). Unpriced rows never consume a cap.
5. **Seal** the surviving membership (publish only, §2.1).
6. **Encode** the exact wire bytes for `body` and `fallback_body` from one Go DTO (§2.5),
   then check the 1 MiB budget on the **exact final bytes**. Refresh-variable fields count
   at maximum width: 19 digits per `price_minor`, `false` per `available`, plus the
   36-byte `tenant_id`.
7. **Write** through `app.cms_serving_put(tenant, site, release, publication_revision,
   expected_generation, body, fallback_body, valid_until)`. It locks the site row,
   compares and swaps on `(publication_revision, serving_generation)`, stores both hashes,
   and checks that the bodies name the row's `tenant_id`, `site_id` and `release_id`. In
   publish and restore, any error rolls back the whole transaction. In a refresh, a failed CAS drops the result.

### 2.4 Anonymous resolver role

`app_domain_resolver` reads:
- the domain map (`site_domains` columns);
- publication (`sites.tenant_id, id, published_release_id, publication_revision`);
- the serving row: `tenant_id, site_id, publication_revision, body, fallback_body,
  body_sha256, fallback_sha256, valid_until`. RLS admits only the verified host's site
  while the row's revision is current.

`pgxadapter.OpenPublic`'s exact capability audit lists the same columns.

- There are no grants on catalog, pricing, inventory or membership tables.
  `db_role_matrix.sql` is exact.
- **Compatibility, no 404 for existing hosts.** A published site without a current serving
  row (published before this change, or published by SQL outside the CMS path) is
  projected from its sealed release with `skus: []`. Its `tenant_id` and content come
  from **one snapshot query**. The refresh sweeper (§2.6) backfills serving rows for such
  sites. The 024 release, route and page grants are revoked only in a later card, after
  the backfill is proven complete in production.
- Public read: one statement, one bounded row. The handler writes the selected stored
  bytes as-is with `ETag: "<sha256 of those bytes>"` and `Cache-Control: no-store`.

### 2.5 Separate DTOs; host-independent public body

- **Public DTO** `PublishedSite` contains `tenant_id`, `site_id`, `release_id`, `locale`,
  `title`, `description`, `navigation`, `pages` and `products`. Each public product has
  `id`, `slug`, `name`, `description` and `skus`.
  - **No `hostname`.** A site has several verified hostnames, and one stored body must be
    valid for all of them. The caller already knows the host it asked for.
  - **No product-level `unit_amount` or `currency`.** These were the immutable price at
    publish time. Displayed prices come only from the refreshed `skus`; the storefront
    shows the lowest purchasable SKU price, or "unavailable" when `skus` is empty.
- **CMS Preview DTO** `SitePreview` keeps the previous projection (with `hostname` and
  release product prices). It has no `tenant_id` and no `skus`, and it is unaffected by
  serving rows.

### 2.6 Refresh and invalidation (in this PR)

- **Append-only invalidation.** Triggers fire on every mutation that can change a served
  value:
  - `price_lists`, `price_entries`, `product_variants` lifecycle and `products`
    lifecycle (every committed site of the tenant, including publication/restore candidates);
  - `stock_levels`, **only when the public availability boolean of a SKU flips**
    (`on_hand > reserved` changes). The MVP has a single warehouse, so the per-row flip
    equals the aggregate.

  In the mutating transaction they **only INSERT** into `serving_invalidations`. They never
  UPDATE or lock `storefront_serving`, `sites` or any other shared row. Catalog, pricing,
  inventory and checkout transactions therefore gain no lock and no change to their lock
  order. The triggers are `SECURITY DEFINER` routines owned by `app_publish_executor`,
  with INSERT-only access to the log and read access to tenant sites and serving.
- **Refresher.** A sweeper inside the API process runs every 10 s
  (`publish.Refresher`). For each tenant on the deployment-authorized list
  `OPENSITE_SERVING_REFRESH_TENANTS`, it finds:
  - sites with unfolded invalidations;
  - rows within 60 s of `valid_until`;
  - published sites without a current serving row (the backfill).

  Each site is rebuilt in its own tenant transaction (runtime role, tenant RLS):
  1. Lock the publication row (`FOR NO KEY UPDATE`; see §2.3 step 1).
  2. Read the invalidation rows of the site visible in the transaction's snapshot: the
     *folded set*.
  3. Build from live inputs read after step 2.
  4. CAS-write the row with `built_through_seq = max(folded seq)`.
  5. **Delete exactly the folded rows**, in the same transaction.

  An invalidation that commits during the build is not in the folded set and survives, so
  the row stays dirty. This fixes the review's lost invalidation (P1-1).

  **Why a folded set rather than a pure high-water mark:** identity values are allocated
  at insert but become visible at commit. A transaction holding a lower `seq` can commit
  after the refresher read `max(seq)`. A rule of "dirty iff `seq > built_through_seq`"
  would then lose it. Deleting exactly the rows that were seen has no such ordering gap.
  The refresher is the only pruner, and its deletes touch only log rows, never rows the
  money path locks.

  **Publication candidates.** Every committed tenant site is a possible next publish
  or restore target. Collecting only current membership or current published listings
  misses new members, restored historical members, and listings that disappear during
  a build. Input mutations therefore append once per tenant site, covering OLD and NEW
  associations without relying on either association being visible. A change between
  a publisher's live input read and membership seal stays outside its exact folded set
  and survives the publish, just as during refresh. Site creation commits before the
  CMS publish command; unpublished sites retain events until their first build folds them.
  This deliberately trades extra per-site log rows for a complete candidate set; use a
  durable per-tenant journal only if measured fanout justifies that added state. Collection
  uses snapshot reads only and adds no lock to any writer.

  A lost CAS is skipped. Several API replicas are safe: the publication lock serializes
  them.
  **Implementation note:** it is not a River job. The River worker role has no catalog,
  pricing or inventory capability, and adding one would need a new cross-tenant capability
  role. The trusted-tenant-list model mirrors `JOB_TENANTS`. The only new seam is
  `QueryRunner.RunTrustedTx`, a tenant write transaction composed solely behind the
  refresher.
- **Deployment.** `OPENSITE_SERVING_REFRESH_TENANTS` is wired into `deploy/compose.yaml`,
  `.env.example`, the deploy README and the e2e runner. The API logs a warning at startup
  when the runtime pool is configured but the list is empty. `RunTrustedTx` may be
  referenced only by `internal/bootstrap`; a `check_modules` rule enforces this.
- **Tenant context required.** The invalidation triggers rely on `app.tenant_id`, which
  every application write path sets. A maintenance write without tenant context is not
  invalidated and is bounded only by `max_lifetime`.
- **Failure modes.** If the refresher is down, a changed site serves its fallback once its
  oldest unfolded invalidation is older than `grace`, and every row expires within
  `max_lifetime`; the site stays up. A stale or concurrent writer can never overwrite a newer publish or refresh.
- **Authority.** Cart and checkout re-price and reserve against live data; the served price
  and availability are display values.

## 3. Acceptance gates

Each gate must go red when its property is broken. The independent reviewer runs the
fault probes.

| # | Gate | Test (CI) |
|---|---|---|
| G1 | Publish atomicity | a serving-build failure (cap overrun) leaves no new release, no pointer move, no SKU rows, and the old serving row intact |
| G2 | Exact 1 MiB wire budget | body at exactly 1 MiB with max-width variable fields and `tenant_id` accepted; one byte more rejected (422); served bytes equal stored bytes and the ETag is the served representation's hash |
| G3 | Unpriced rows don't consume caps | 6,000 unpriced variants plus priced ones publish; only priced SKUs are served; input budget overrun returns a typed 422 |
| G4 | Anonymous SELECT on live tables fails | public login: catalog, pricing, inventory and membership return 42501; `db_role_matrix.sql` exact; `OpenPublic` exact |
| G5 | Stale writer cannot overwrite | an old `publication_revision` or `serving_generation` fails CAS; the newer row stays byte-identical; a refresh blocks on the publication lock **before reading inputs** (observed in `pg_stat_activity`); an invalidation committed while a refresh is blocked in its write survives (barrier test, not sleeps) |
| G6 | Preview unaffected | `SitePreview` has no `tenant_id` or `skus`; it works without a serving row |
| G7 | Fallback, not 503 | after `valid_until` (boundary, max lifetime, or invalidation `grace`), the read returns `fallback_body` with its own ETag |
| G8 | Isolation | two tenants with identical product and SKU UUIDs each serve only their own body; unknown hosts return 404; the compatibility path returns tenant and content from one snapshot (a hook moves the hostname between the read's statements; the test fails if tenant and content come from different snapshots) |
| G9 | Pricing parity | served `price_minor` equals `pricing.Quote` (priority, tiebreak, window, `±infinity`) |
| G10 | Restore | a restored release without membership serves `skus: []`; a restored release with membership rebuilds from its sealed rows |
| G11 | Invalidation and refresh | price, availability flip, variant archive and product archive each append an invalidation in the mutating transaction; a reservation that does not flip availability appends none; the refresher, driven through the production `QueryRunner.RunTrustedTx`, rebuilds the row; with no refresher, the fallback is served after `grace`; a forced-short lifetime is refreshed so SKUs stay purchasable past expiry |
| G12 | No lock on the money path | two concurrent checkouts reserving overlapping member SKUs (c1: A then B; c2: B) both commit; no deadlock, and c2 never waits on serving data |
| G13 | Trusted seam governance | `check_modules` rejects any `RunTrustedTx` reference outside `internal/bootstrap` |

## 4. Reuse from PR #55

- Contract types `SKU` and `OptionValue`, the OpenAPI `PublishedSku` schema and
  `tenant_id`.
- The one-pass `pricing.Quote` attachment and its bounded-work test, moved to the builder.
- The pgx fixtures: two tenants with shared UUIDs, archived and unpriced SKUs, infinite
  list bounds.
- Dropped: the 033 RLS policies and grants, the `stock_levels.in_stock` column and the
  live `LiveSKUSQL` path.

## 5. Consequences

- The anonymous path does O(1) bounded work with no live joins. Publish does more work,
  but it is bounded and fails as a typed 422 the merchant can act on.
- Displayed prices and availability lag live data by at most `grace` after a change (or
  fall back), and never by more than `max_lifetime`. Checkout is the authority.
- Catalog, pricing and inventory writes pay a small trigger cost. It touches only the
  tenant's committed sites as publication/restore candidates (one append per site).
- `publish` owns `site_release_skus` and `storefront_serving`. It reads catalog, pricing
  and inventory only inside tenant transactions through declared dependencies, and uses
  the Pricing Port as its only pricing logic.

## 6. Out of scope

- **C-15d (follow-up card):** revoke the 024 snapshot grants on `site_releases`,
  `site_release_routes` and `page_revisions` from `app_domain_resolver`. Remove the
  compatibility projection after a verified production backfill (every published site
  has a current serving row).
- Refresh tenant discovery beyond the deployment list (a cross-tenant broker role), if
  operators need it.
- Deferred to C-15d from the PR #96 review:
  - Price-gap boundary (thread 4260912941). A sealed SKU between two price lists is
    hidden until the next refresh, at most `max_lifetime`.
  - The `site_release_skus → product_variants` foreign key blocks hard deletion of a
    published variant. Decide this with #44 data rights and CSV replace.
- A per-SKU live price lookup on the product page (owner decision).
- Splitting serving into a static snapshot plus per-product offers (post-MVP).

## 7. Superseded amendments

- The first refresh design had the triggers UPDATE `storefront_serving` (`dirty`,
  `valid_until`). Independent review (PR #96 @68b09ff) found that this lost invalidations
  (probe 96-4) and deadlocked overlapping checkouts (probe 96-5). It is replaced by the
  append-only log (§2.6).
- The earlier amendment "C-15b uses the price boundary only, no max staleness" is
  withdrawn: refresh and invalidation are in scope, so `max_lifetime` applies from the
  start.

## 8. Design review resolutions (PR #96)

| Thread | Resolution |
|---|---|
| 4259961107 | §2.1: membership is written by `cms_seal_skus` in the same transaction that creates and seals the release; it does not use the release-children guard |
| 4259961120 | §2.4: the compatibility path serves existing hosts from one snapshot; the sweeper backfills; grant revocation is deferred |
| 4259961129 | §2.6: synchronous invalidation in the mutation transaction plus a bounded lifetime |
| 4259961139 | §2.3/§2.6: refresh reads live readiness; variant and product lifecycle changes invalidate |
| 4259961189 | §2.3 step 1: the refresh locks the site row (`FOR NO KEY UPDATE`) before reading inputs |
| 4259961150 | §2.2/§2.4: `fallback_sha256`; the ETag is the hash of the bytes served |
| 4259961159 | §2.5: `hostname` removed from the public body; consumers updated |
| 4259961172 | §2.5: product-level `unit_amount` and `currency` removed from the public DTO; the storefront uses SKU prices |
| 4259961175 | §2.4: both hashes granted to the resolver and listed in the `OpenPublic` audit |
| 4260039759 | §2.4: the compatibility read returns `tenant_id` from the same snapshot row as the content (P1 security fix) |

## 9. Independent review round (PR #96 @68b09ff)

| Finding | Resolution |
|---|---|
| P1-1 lost invalidation (probe 96-4, thread 4260912932) | append-only log with folded-set deletion (§2.6); barrier gate in G5 |
| P1-2 hot-row lock and deadlock on checkout (probe 96-5) | triggers only INSERT the log; stock triggers fire only on an availability flip; G12 |
| P1-3 refresher not deployed | deploy, env and e2e wiring; startup warning; forced-short-lifetime gate in G11 |
| 96-1 `RunTrustedTx` untested | G11 drives the refresher through the real `QueryRunner.RunTrustedTx`; G13 |
| 96-3 host move not interleaved | statement-boundary hook; G8 |
| 96-6 early publication lock ungated | `pg_stat_activity` gate in G5 |
| 96-7 product archive not covered | G11 |
| P2-5 `cms_serving_put` stores arbitrary bytes | the put validates the body's tenant, site and release; `cms_seal_skus` checks code and product against the variant |
| P2-4 cross-module touches undeclared | `registry.yaml` `touches_tables` lists every table that carries a serving trigger |
| P2-1, P2 foreign key | deferred to C-15d (§6) |
