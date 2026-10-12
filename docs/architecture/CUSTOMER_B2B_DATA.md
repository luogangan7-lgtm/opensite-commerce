# Customer and B2B data contract (C-05a)

This implements the identity split in V1_DECISIONS §1 and the data boundary of
APP_LAYER_DECISIONS A3. Customer owns `customers`, `customer_credentials` and
`customer_addresses`; B2B owns `buyer_companies`, `buyer_roles` and
`buyer_company_members`. Their tenant keys reference neither employee `users`
nor seller `organizations`. The migration depends on identity and Pricing.

Customer email is lowercased after trimming outer POSIX whitespace and is unique
within a tenant. The same email can identify an employee and independent customers
in several tenants. A guest has a customer row with an email and no credential.
The email check enforces a bounded, nonblank address without internal whitespace;
it does not prove delivery or mailbox ownership. NFC/NFKC normalization, zero-width
characters and collation stability are explicit follow-ups for C-05 authentication;
this data card does not treat visually similar addresses as identical. Addresses accept the project's
249 assigned ISO 3166-1 alpha-2 country codes; subdivision remains optional text.

Each customer/company pair has one current role: `purchaser`, `approver` or
`admin`. Multiple companies per customer are allowed. Membership checks must read
the relation with `status='active'` in the authorized tenant on each request;
revoked rows and deleted rows confer no company membership. This card does not
implement role-to-action mappings, approval operations, caches or sessions.
The migration seeds the fixed role catalog for existing tenants. An AFTER INSERT
trigger also seeds these three rows whenever a trusted provisioner creates a
tenant, including after the migration. The trigger is SECURITY INVOKER, so it
uses that provisioner's existing authority and creates no elevated application
capability; a future tenant-creation service must explicitly own provisioning
rights for both tables, including applicable RLS policies for a non-bypass
provisioner. Failed provisioning rolls back the new tenant.

Every new table has composite tenant keys and ENABLE + FORCE RLS, with blank
tenant settings selecting no rows. The capability matrix is deliberately small:

| Capability | Reads | Writes |
| --- | --- | --- |
| `app_runtime` | Customer/B2B tables except credentials | None |
| `app_customer_auth` | Scoped customers and credentials | Register customers (`tenant_id,id,email,display_name`), create credentials (`tenant_id,customer_id,password_hash`), update `password_hash` only |
| Employee `app_auth` and other application service roles | None of these new tables | None |
| Privileged migration/maintenance owner | Provisioning and controlled management | Status changes, addresses, buyer catalog and memberships |

`app_customer_auth` is a separate NOLOGIN capability with no outward membership,
schema CREATE, staff-table access or foreign app-function EXECUTE. The forward
migration sanitizes existing role attributes, memberships and both table/column
ACLs before granting its exact permissions. Provision its dedicated login after
the migration; never reuse the employee auth pool. The PHC check accepts an
Argon2id v19 envelope with positive parameters, base64 fields and bounded text;
the future authenticator must validate decoded lengths, safe parameter limits
and password work. This is data validation, not password authentication.
A SECURITY INVOKER BEFORE UPDATE trigger changes `updated_at` on actual
`password_hash` changes, using `clock_timestamp()`; identical hashes preserve the
timestamp. The dedicated authenticator still cannot write the timestamp column.
Both trigger functions pin `search_path=pg_catalog,app,pg_temp` and have no PUBLIC
or application-role EXECUTE capability.

Before attaching credentials to an existing guest customer, the future customer
authentication flow must prove ownership of that customer's email address. A row
lookup or possession of its UUID is insufficient; an independent verified-email
challenge is a prerequisite to guest-to-registered conversion. This data layer
does not implement or claim that ownership check.

Pricing's `(tenant_id,buyer_company_id)` now references purchasing companies.
An unresolved legacy company-specific list blocks the entire migration with
`CUSTOMERS_UPGRADE_BUYER_COMPANY_RECONCILIATION_REQUIRED`; an operator must
reconcile it explicitly before retrying. The migration never clears its target
or invents a company. `customer_group_id` remains without an FK until its owning
module introduces customer groups. `modules/registry.yaml` registers Customer
and B2B as the respective owners of their three tables, with no HTTP operations.

Acceptance uses `db_customers.sql`, the exact table/column role matrix, two
advisory-lock registration races and populated/orphan-target upgrade cases.
All migration discovery uses logical suffixes/globs so integration can renumber
the provisional migration. Named-constraint and privilege/RLS mutations must fail
for their expected markers before the restored database passes. Frozen 001–010
remain unchanged; integration also preserves frozen 011–015 from main. HTTP, login/cookies, checkout, orders and approval integration
are outside this data-only PR; the separate customer cookie remains required.

Rollback uses the preceding application version with these additive tables left
in place; applied migration history is never rewritten. A failed transactional
upgrade leaves legacy data and schema unchanged. No production rollout is included.
