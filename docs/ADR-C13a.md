# C-13a Shipping data and pure quote contract

Status: implementation proposal, based on V1_DECISIONS §5 and §9. The task card
permits a tentative next-number migration; integration assigns its final number.
Migrations 001–010 remain frozen.

Shipping owns `shipping_profiles`, `shipping_zones`,
`shipping_zone_destinations` and `shipping_rates`. A profile is one shipping
calculation template in one currency. Zones are reusable within a tenant;
normalized destination rows represent each zone's country/subdivision set.
All references include `tenant_id`. Rate currency participates in the profile
foreign key; amounts and thresholds are `bigint` minor units, weights are integer
grams. `app_runtime` has SELECT only. Editing rules needs a future trusted writer
contract; this PR does not expose writes or an HTTP endpoint.

Country codes use a pinned 249-code ISO 3166-1 alpha-2 allowlist. SQL and Go must
match in the offline gate, which also pins the reviewed registry digest independently
of either production copy. Subdivision codes check the full `CC-XXX` form, a
1–3 uppercase ASCII alphanumeric suffix and matching country prefix. This MVP
validates subdivision syntax, not membership in the changing ISO 3166-2 registry.
Admin registry validation and country-code updates require a later forward
migration and synchronized Go update. See the [ISO authority](https://www.iso.org/iso-3166-country-codes.html).

Quote callers supply one authorized tenant's profile-relevant zone set, complete
rate configuration and a profile ID. Loaders must select the zones offered by
that profile, so an unrelated template's more-specific zone cannot shadow it;
the pure package performs no authentication, SQL, network, clock or filesystem
access. Subdivision matches override country-wide matches. Multiple matching
zones at the same specificity return `ErrAmbiguousZone`. No zone returns
`ErrNoZone`; a zone without an applicable rate returns `ErrNoRate`. Errors return
an empty result, so absent rules cannot become an accidental zero fee.

Each profile/zone allows at most one fixed and one free-over-amount rate.
A weight rate charges its stored tier amount, not an amount per gram. Weight
segments are `[min_weight_g,max_weight_g)`; a NULL maximum means unbounded.
`btree_gist` plus EXCLUDE protects overlap under concurrent inserts and updates.
This is a bundled PostgreSQL extension, with no additional Go dependency; see
[PG18 range constraints](https://www.postgresql.org/docs/18/rangetypes.html) and
[UUID GiST operators](https://www.postgresql.org/docs/18/btree-gist.html).

Selection order is explicit: an inclusive free threshold (`subtotal >= threshold`)
returns its zero amount first. Otherwise, if any weight tiers are configured,
exactly one matching tier is required: gaps and weights at/above a finite final
upper bound return `ErrNoRate`, even when a fixed rule exists. Fixed pricing is
used only without weight tiers. A free rule alone refuses below-threshold orders.
The result includes the chosen zone, rate ID, method and currency, so its basis
can be explained. Currency mismatch and malformed configuration are errors.

`internal/money` is absent on the branch base, so the card-authorized minimal
`shipping.Money` holds only Amount/Currency. Shipping performs comparisons and
selects stored fees; it performs no rounding, multiplication, allocation or FX.
Replace this type with shared Money after that module merges, with caller tests.
In pure Go, MinWeightG is zero for fixed/free rows; a future database adapter must
map their SQL NULL weight minimum to zero and preserve by-weight zero normally.

Acceptance: offline checks, actual sqlc generation, real PostgreSQL 18.6 migrations
and role matrix, SQL shipping gate, four advisory-synchronized overlap races,
pre-existing-tenant upgrade, Go table tests and race detector. Guard mutation
receipts demonstrate semantic red failures and green restoration. These gates
cover this data/pure-calculation unit; independent review and integrated checkout,
carrier API, multi-warehouse, tax, HTTP and production deployment are outside scope.
