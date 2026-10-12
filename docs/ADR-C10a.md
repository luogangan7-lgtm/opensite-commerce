# C-10a: exact money and deterministic tier pricing

Pricing owns `price_lists` and `price_entries`; `product_variants` is the existing
SKU identity. Entries already describe an offer at a quantity tier, so a duplicate
`offers` table adds no MVP capability. Separate offers await a distinct channel or
publication requirement. Entries inherit their list's currency.

Money stores signed int64 minor units and three uppercase ASCII currency letters.
Currency syntax is validated; this package does not maintain an ISO currency
catalog or infer currency decimal scales. Add/subtract reject currency mismatch,
quantity multiplication accepts nonnegative integers, and final int64 overflow
returns an error. Exact intermediate products and weight sums use standard
`math/big`; neither floats nor SQL numeric amounts enter the calculation.

Rounding is **half-up by magnitude (half ties away from zero)**: +1/2 becomes +1,
-1/2 becomes -1. `Scale` applies one nonnegative rational with a positive divisor.
Allocation does not independently round each share: it floors absolute proportional
shares, assigns leftover units to the largest fractional remainders, breaks equal
remainders by input index, then restores the sign. Caller input order is therefore
part of the allocation contract. Signed totals, including MinInt64, are conserved.

Price validity is `[starts_at, ends_at)` with a nullable unlimited end. Quote
requires a positive integer quantity and explicit tenant/SKU/currency/time.
Among active matching all/group/company lists having an eligible tier, higher
priority wins; equal priorities use the smaller canonical UUID, **not input order**.
Within that list the greatest min_quantity <= requested quantity wins. SQL uses
`ORDER BY priority DESC, id ASC, min_quantity DESC`; Go uses the same comparator.
Result carries unit/line money, list ID, priority and chosen tier as its explanation.
The pure function neither authorizes audience IDs nor reads the clock/database.
Callers authenticate and scope inputs before supplying lists and audience IDs.

Runtime receives only SELECT on the two RLS/FORCE-protected pricing tables.
This card adds no price-management HTTP API or service-role write capability.
Customer-group and buyer-company tables are absent on this base: their nullable
UUID columns have shape checks but no referential authority yet. Their owner
modules must add `(tenant_id, target_id)` foreign keys in a forward migration
before those targets are exposed. A fabricated target ID grants no authentication.

Migration numbers are temporary until integration (§9 P1-a). Merged 001–011 stay
unchanged; tests find pricing by table/module name, not the temporary number.
Gates cover exact money/overflow/conservation, tie/window/audience/tier decisions,
duplicate-tier/FK/check/RLS rejection, actual SQL reads and advisory-synchronized
duplicate inserts, fresh install and existing-schema upgrade. SQLC inputs and
generated Go must be regenerated after any integration renumbering.
The pgx integration test calls the generated production query in a scoped runtime
transaction and compares its rows/winner to fixed expectations and pure `Quote`.
CI supplies an expendable migrated `DATABASE_URL`; without it this tagged test
reports `NOT_RUN`. Both pricing tables have foreign-tenant and blank-context
witnesses. Predicate/RLS fault injection must fail before restoration passes.

HTTP, taxes, shipping, discounts/promotions and FX are outside C-10a.
