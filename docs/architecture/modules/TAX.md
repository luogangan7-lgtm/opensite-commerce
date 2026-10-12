# Regional Tax calculation contract (C-12a)

Tax owns `tax_zones`, normalized `tax_zone_locations`, `tax_categories`,
`tax_rates` and `tax_settings`. It depends on tenant identity only. The pure
`internal/tax` package depends on the standard library and `internal/money`, and receives one tenant's
already scoped rules and an explicit time. Callers must load `prices_include_tax`
explicitly; a missing settings row is not permission to assume a tax policy.
Settings use the natural singleton `tenant_id` primary key; entities and
relationships use tenant-composite keys and foreign keys.

A country can have a country-wide zone and specific subdivision zones. Exact
subdivision wins over the country fallback, independent of input order. An exact
zone with a missing rate does **not** fall back to a country rate. No matching
zone, unknown category, missing active rate and ambiguous rules are explicit
errors. Active rates use `[valid_from, valid_until)`; a null upper bound is open.
Finite rule dates must have UTC years 1..9999, preserving snapshot JSON serialization.
The database exclusion constraint rejects simultaneous overlapping inserts and
updates for the same tenant, zone and category. Exemption is explicit category
configuration; it returns zero tax with an `exempt` snapshot rather than relying
on the absence of a rate. An ordinary zero-rated category needs an explicit
active 0 bp rule.

Countries use a pinned 249-code assigned alpha-2 snapshot. Subdivisions validate
country prefix and uppercase alphanumeric syntax, **not** membership in an
ISO 3166-2 subdivision registry. See [ISO's country-code definition](https://www.iso.org/iso-3166-country-codes.html).
Registry updates require a reviewed forward migration and corresponding Go/pin
updates. Tax and Shipping retain separate pinned registries; neither module depends on the
other. A country-registry update must coordinate their forward SQL and Go changes.

## Rounding decision: per-line half-up

All prices and amounts are nonnegative `int64` minor units, and quantity is a
positive integer. Multiply unit amount by quantity first, then round tax once
per line, with ties rounded upward. For exclusive prices,
`tax = half_up(net * rate_bp / 10000)` and `gross = net + tax`. For inclusive
prices, `tax = half_up(gross * rate_bp / (10000 + rate_bp))`, then
`net = gross - tax`. Thus the original gross is conserved exactly. Totals sum
these already rounded lines; they never recompute tax on the aggregate.

For example, two separate 1-unit net lines at 50% each incur 1 unit of tax,
so invoice tax is 2. Rounding the 2-unit aggregate would produce 1. We accept this
line-versus-aggregate difference so each invoice line is independently auditable.
Each line supplies an explicit three-uppercase-letter currency. The calculator
rejects invalid or mixed currencies and returns that currency in the result and
rate snapshots. Its boundary amounts remain `int64` minor units; multiplication,
scaling, subtraction and sums use shared `internal/money` operations. Money.Scale
implements half-up with wide intermediates, so representable results remain valid
even when the intermediate numerator exceeds `int64`. Final line values and
totals return the shared Money overflow sentinel.

Each returned line includes its net/tax/gross amounts and a JSON-serializable
snapshot: currency, zone, category, rate ID, basis points, validity bounds, inclusion mode,
rounding policy and selection reason. Snapshots copy time bounds and do not alias
mutable input rules. Orders must persist these results, and Billing must consume
them rather than recalculate from subsequently edited rates. Persistence,
HTTP, orders/invoices, VAT identification, global filing and currency exchange
are outside this card.

Runtime can SELECT and INSERT explicit configuration columns, and UPDATE only
zone/category display data, exemption, rate values/windows and tax inclusion
mode. It cannot change identities, delete/truncate rows or access another tenant.
Removing/replacing a location requires a separate privileged maintenance path;
that path is outside this data-and-calculation card. Every new table has forced
RLS; no new SECURITY DEFINER function or elevated application role is introduced.

## Acceptance

`check_all.sh`, real sqlc v1.31.1 generation, full pgx-tagged Go race tests,
PG18.6 `check_pg.sh`, advisory-lock synchronized `check_pg_races.sh` and populated
`check_pg_upgrade.sh` are required. The upgrade locates `_tax.sql` dynamically,
applies every predecessor, seeds existing catalog data, applies Tax, then applies
any successors before the exact whole-schema privilege matrix. SQL failures
check exact SQLSTATE so invalid fixtures cannot masquerade as rejection proof.
Mutation receipts remove guards transactionally and require the corresponding
red marker followed by restored green. Final receipts bind a commit and source
hashes; integration renumbering requires new receipts on the final commit.
