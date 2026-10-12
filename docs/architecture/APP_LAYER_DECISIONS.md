<!-- source: owner's ChatGPT architecture conversation 6acb3610, 2026-10-12, answer to orchestrator batch A (thought 19m); GPT read main 4bafc33. Condensed by the orchestrator; decisions are the design baseline, not implementation evidence. -->

# OpenSite Commerce — Application layer decisions (A1–A7)

**Baseline:** main `4bafc33` · **Date:** 2026-10-12 · **Scope:** PR-02 (Identity/Tenant auth API) and PR-03 (Module Registry, OpenAPI, module-boundary CI). Data model, PR order and 001–010 are unchanged (see `V1_DECISIONS.md`).

**Overall:** Go stdlib HTTP + OpenAPI-generated typed server + explicit business modules + PostgreSQL role separation + server-side sessions. No Gin/Echo, no DI framework, no ORM, no Redis, no external auth service.

PR-02 owns identity truth, sessions, tenant authorization and API keys. PR-03 owns module boundaries, the HTTP contract, route registration and CI rules. PR-02 may depend on contracts PR-03 freezes first; PR-03 never depends on the real auth implementation.

## Conflicts in the current repo (to fix in the cards below)

| Where | Problem | Card |
|---|---|---|
| `go.mod` | module path `example.com/...`, Go 1.25 | done in C-01 (PR #11) |
| `api/openapi.yaml` security | only API-key bearer; no cookie session, login/logout, tenant routes | 03b |
| `queries/site.sql` `GetProductVariants`; `queries/catalog.sql` `GetPublishedSite` | queries filed under the wrong owner module | 03d |
| `internal/tenanttx` | validates UUID shape only; no binding to an authorized actor | 02e |
| `internal/tenanttx/pgxadapter` | accepts any caller-supplied tenant id | 02e |
| `deploy/runtime-roles.md` | lists 3 roles; 010 added verifier/media/publisher/executor | 02a |
| `cmd/dev-api` | diagnostics only, not the production API | 03a |
| OpenAPI | `If-Match`/`Idempotency-Key`/problem+json exist but no auth error matrix | 03b |
| `docs/ADR-071.md` | historical "pending PG18" status text | add a status note pointing to PR01A evidence; do not rewrite history |

## A1 — Module layout, ports, registry

**Decision.** Single `go.mod`, modular monolith, static composition root. No per-module Go modules, no runtime plugin loading.

```
cmd/api, cmd/worker, cmd/bootstrap-admin
internal/bootstrap/            # static wiring
internal/platform/{httpapi,authentication,authorization,database,tenanttx,errors,clock}
internal/contracts/payment/v1/ # only truly shared, versioned contracts
internal/modules/<name>/{contract/, internal/{service,store,httpapi}/, module.go}
modules/registry.yaml          # governance manifest, not a plugin system
queries/<module>/*.sql
```

- Modules talk only via `contract/` or approved public ports; implementations live under the module's `internal/` (Go's internal-import rule enforces it). Consumers define the port they need; providers implement it.
- `modules/registry.yaml` declares per module: Go path, `owns_tables`, OpenAPI `operations`, scopes, allowed `dependencies`, test entry points. Handlers are still registered statically in Go.
- CI enforcement: `go list -deps -json` plus a small rule checker (no depguard/go-arch-lint for now). Table ownership is reconciled against a real PG18 `pg_catalog` listing of schema `app`: every table has exactly one owner, there are no unknown or phantom tables, migration proposals declare `owner` and `touches_tables`, and cross-cutting migrations (for example platform-security) don't change table ownership.
- **Rejected:** per-module go.mod (version/workspace overhead); directory convention alone (doesn't stop imports); depguard+go-arch-lint (duplicate tooling).
- **Gate (red→green):** each of these must fail CI: importing another module's private store or repository; a registry entry naming a nonexistent table; a new table with no owner; a dependency cycle; a module change that skips its downstream tests. The registry must also match the OpenAPI operation IDs.

## A2 — HTTP layer

**Decision.** `net/http.ServeMux` (Go 1.22+ patterns) plus an **oapi-codegen strict server**, pinned at v2.8.x after a compatibility POC against the current OAS 3.1 (`oneOf`, nullable, response mapping). If the POC fails, write a new ADR. Never fall back silently to unconstrained handlers.

- Every route carries a mandatory `RouteSpec{OperationID, Method, Pattern, Access, Scope, MaxBodySize}`.
- `Access ∈ {Public, AuthenticatedSelf, AdminSession, APIKey, AdminOrAPIKey, InternalService}`; unknown access types are rejected.
  - Tenant-scoped private routes (those with a `{tenant_id}` path parameter) must declare a scope. A missing scope fails at startup and in CI.
  - `AuthenticatedSelf` is for the caller's own identity and session: whoami/current actor, logout, session list/revoke. It must have **no** tenant path parameter and **no** scope, and it checks only a valid session plus an active user. Declaring a scope or a tenant parameter on it, or using it for anything else, fails registration.
  - Registration tests cover both directions.
- OpenAPI security lists cookie session OR API key as alternatives, not both required. Every authenticated call reaches the same domain command service, so agents and browsers share the same path.
- **Layering:**

  | Layer | Owns |
  |---|---|
  | HTTP platform | request id, logging, panic recovery, RFC 9457 `application/problem+json` mapping |
  | Transport | content type (415), size limit (`MaxBytesReader`, 1 MiB default; uploads are streamed separately) |
  | Strict JSON | `DisallowUnknownFields`, reject a second JSON value, depth/length limits |
  | Authentication | auth, CSRF, Origin |
  | Authorization | scope and tenant |
  | Domain | business validation |

  `ETag`/`If-Match` are parsed in HTTP and checked in application code. `Idempotency-Key` is extracted in HTTP but persisted inside the domain transaction (completed in PR-F). Cursor pagination is parsed in HTTP and executed in the query service.
- Errors carry a stable machine `code` for 400/401/403/404/409/412/413/415/422/428/429/500.
- **Rejected:** fully hand-written handlers (contract drift); ogen (second generator, adaptation cost); Gin/Echo (no need); reflective generic CRUD (cannot enforce domain authz/invariants).
- **Gate:** regenerating the server produces no diff. Each `implemented` operation has a real handler, and handler I/O conforms to the spec. A private route without auth or scope cannot register. Unknown field, oversize body and a second JSON value are all rejected. An ETag mismatch returns 412. A missing 401/403 mapping or a removed operation test fails CI. Unimplemented operations are marked `planned`.

## A3 — Staff authentication, sessions, API keys, DB pools

**Decision.** In-house light auth module; authentication ≠ authorization ≠ tenanttx.

- **Passwords.** Argon2id via `golang.org/x/crypto/argon2`, stored in versioned PHC format.
  - Floor parameters: m=19 MiB, t=2, p=1; measure on the target host.
  - Bound concurrent hashing to prevent memory DoS.
  - Unknown user, disabled user and wrong password all give the same response **and do the same work**. An unknown or disabled identifier still runs one Argon2id verification against a fixed dummy PHC hash, and goes through the same persistent account+IP failure bookkeeping, so response timing does not reveal account existence.
- **Sessions.** Uses the existing `001 auth_sessions(id,user_id,token_hash,expires_at,revoked_at,created_at)`; no tenant_id is needed, because a user can belong to many tenants.
  - Token: 256-bit CSPRNG; only its SHA-256 is stored.
  - New session at login; logout sets `revoked_at`; a password reset revokes all sessions.
  - Rotation is serialized on the old session. In one transaction: `UPDATE auth_sessions SET revoked_at=now() WHERE id=$old AND revoked_at IS NULL AND expires_at>now()`. Zero rows means the rotation lost the race (or the session is already dead): roll back and insert no replacement. Only then insert the new session. Two concurrent rotations of the same session must yield exactly one valid successor (concurrency test required).
  - Every request re-checks session and user status. Tenant-scoped routes (`/admin/t/{tenant}`, `/api/v1/t/{tenant}`) additionally re-check that tenant's membership and status after the tenant is resolved. No membership cache. High-risk writes re-check membership inside the transaction.
  - Global session endpoints (whoami/current actor, logout, session list/revoke) validate only the session and user. They never require a membership, so a user whose last membership was suspended can still log out: the server revokes the session and clears the cookie.
- **Cookie and CSRF.** Cookie `__Host-osc_admin_session`: Secure, HttpOnly, SameSite=Lax, Path=/, no Domain, on a dedicated admin host.
  - CSRF: an HMAC token bound to the session, sent as `X-CSRF-Token`, plus an exact Origin check. Pre-login CSRF uses short-lived anonymous material.
  - SameSite+Origin alone is not enough; unsigned double-submit is rejected.
  - API-key bearer calls need no CSRF token but still go through authn, authz and rate limiting.
- **Login rate limit.** No Redis. Three layers, all with atomic updates:
  - an in-process per-IP limiter;
  - a PG-persisted per account+IP failure count and cooldown (new table such as `auth_login_attempts`);
  - an independent, durable **account-wide** counter keyed by the normalized identifier, also covering unknown identifiers, so rotating source IPs does not reset it.

  The account-wide layer applies progressive, **bounded** backoff (a capped maximum delay, never a hard lock).
- **Anti-lockout.** A successful login issues a signed, long-lived known-device cookie, `__Host-osc_known_device`, bound to the user and revocable.
  - A login attempt that presents a valid known-device cookie for that account is exempt from the account-wide layer, but still subject to the per-IP and account+IP layers.
  - Those attempts are verified on a separately reserved Argon2 hashing slot, so a flood of attacker guesses cannot starve the owner.
  - Failures under a known-device cookie count against that cookie, and too many revoke it.
  - New devices during an attack get only the bounded backoff, so the owner can always log in from a known device, and from a new one after at most the cap.
- **API keys.** Public key id + 256-bit secret. Look up by id, hash the secret, compare in constant time, then check expired/revoked and that the issuer is still an active member **of the key's tenant**. A key is bound to its owning tenant (`api_keys.tenant_id`): the request must target that tenant (`key.tenant_id == route tenant_id`), or it is rejected before any membership or scope evaluation.
  - Effective scopes = RoleScopes ∩ KeyScopes ∩ ResourcePolicy.
  - The secret is shown once and only rotated, never retrievable. Do not store plaintext to replay a create response.
- **Pools.** Separate DB logins:
  - Auth pool (inherits `app_auth`): users, sessions, memberships, keys.
  - Runtime pool (inherits `app_runtime`): authorized tenant work.
  - Domain pool (inherits `app_domain_resolver`): anonymous host lookup.
  - Not one superuser plus `SET ROLE`. 010's service roles get pools when their modules land. This is permission partitioning inside one process, not process isolation.
- **Customers.** PR-04 customers get a separate auth entry and cookie; they never reuse the admin session.
- **Forward migration needed:**
  - login-attempt data;
  - a global unique index on the API-key public id;
  - a limited credential-writer role;
  - the minimal auth-role reads of tenant status.
  - `app_auth` today can read users/sessions/memberships/keys and create or revoke sessions, but must not be assumed to create users or issue keys.
- **Bootstrap.** Admin bootstrap is a controlled CLI (`cmd/bootstrap-admin`); there is never an anonymous owner-creation endpoint.
- **Rejected:** stateless JWT sessions (revocation/rotation); SameSite only; Redis sessions; a single high-privilege pool; sharing sessions with customers.
- **Gate (real PG18, not mocks):** two concurrent rotations of one session leave exactly one valid successor (the other rolls back); timing and failure bookkeeping for an unknown identifier match a wrong password on a real account; logout succeeds and revokes for a user with no remaining membership. In addition, each of these must be denied:
  - no account enumeration on bad credentials;
  - an old token replayed after rotation;
  - a revoked session, a suspended membership or a disabled user (on the next request);
  - a POST with a cookie but no CSRF token, or a cross-origin write;
  - a key with a narrowed scope, or whose issuer lost membership;
  - a key issued for tenant A used against tenant B, where the issuer is an active member of **both** tenants with overlapping scopes (so a membership-only check cannot pass);
  - distributed guessing against one account from many IPs, which must hit the account-wide cooldown;
  - while an attacker continuously renews the account-wide backoff, the owner with a known-device cookie still logs in, and a new device succeeds within the backoff cap (never locked indefinitely);
  - the runtime pool reading `password_hash`, or the auth pool writing catalog;
  - brute force (rate limited and audited).

## A4 — Tenant resolution and authorization

**Decision.** The tenant comes from the URL path: `/admin/t/{tenant_id}/...` and `/api/v1/t/{tenant_id}/...`. This is a deliberate revision of the draft OpenAPI. There is no tenant header and the tenant is not bound to the session. A single-site deployment may redirect to the default tenant but still authorizes fully.

- **Pipeline.** RouteSpec match → resolve session/key → validate user/key → parse tenant from the URL → tenant status → membership + scope → `AuthorizedActor` (private fields; handlers cannot construct it) → `RunAuthorized(ctx, actor, fn)` over tenanttx (`SET LOCAL app.tenant_id`) → sqlc/domain → commit or rollback. The URL tenant is a target, not a credential.
- **Roles.** Role→scope mapping lives in Go code to start: owner, admin, catalog_editor, content_editor, viewer. It must match `tenant_memberships.role` and the OpenAPI `x-required-scope`. No custom roles, OPA or ABAC yet.
- **Three guards.** Default-deny RouteSpec; domain commands re-verify resource ownership; PostgreSQL FORCE RLS as the backstop. A CI rule forbids business modules from touching a raw `pgxpool.Pool`.
- **Storefront.** It never reads the admin session. Tenant and site come from a verified hostname via `app_domain_resolver`; an arbitrary Host header is never trusted.
- **Rejected:** `X-Tenant-ID`; a single tenant stored on the session; manual `Authorize()` calls in handlers.
- **Gate:** each of these must hold:
  - another tenant's id in the URL returns 403/404 with no leak;
  - a viewer attempting `catalog.write` is denied;
  - a private route registered without a scope fails;
  - a handler that bypasses `RunAuthorized` fails the architecture check;
  - same-connection A→B tenant isolation;
  - a missing GUC returns zero rows and no cast error;
  - a forged or unverified Host gets no site context;
  - a revoked membership stops working on the next request.

## A5 — Frontend

**Decision.**

| Part | Choice |
|---|---|
| Admin | Vite + React + React Router + TanStack Query |
| API client | `openapi-typescript` + `openapi-fetch` (generated, single client; no hand-built auth headers) |
| Storefront | Astro SSR on the official Node adapter (standalone), React islands |
| Package manager | **Bun** workspace, single `bun.lock`, pinned Bun version, `bun install --frozen-lockfile` |

Bun builds and tests, Node runs Astro SSR. Adopting Bun SSR now is rejected as unnecessary risk.

```
web/admin/src/{app,features/{identity,tenant,catalog,media,cms,builder},shared}
web/storefront/src/{pages,runtime,modules/{site,catalog,company,cms,commerce}}
packages/{api-client,ui,design-tokens,component-contracts}
```

- **Data path.** Browser → Caddy → Astro SSR → restricted internal Read Context API → Go query service → PostgreSQL. Astro holds no DB credentials. Preview and publish use the same Astro renderer. Agents use the same domain API as everyone else.
- **Rejected:** Go rendering every page (reimplements templates and components); React SPA for everything (SSR/SEO); micro-frontends; pnpm (Bun is already the toolchain).
- **Gate:** API client regenerates with no diff; changing an API field without updating the client fails the TS build; the session never lands in localStorage; the storefront cannot reach PG; SSR renders a fixed product view model; the admin bundle never loads on the storefront; affected frontend tests run when a component changes.

## A6 — Test stack

**Decision.** Real PostgreSQL 18.6 in CI (fixed service; testcontainers only as a local convenience) + Go unit/integration + Playwright + axe-core + pinned visual baselines.

- **Isolation by test type:**

  | Test type | Isolation |
  |---|---|
  | Migration | temporary database |
  | RLS / roles | real roles and real pools |
  | Repository | transaction rolled back at the end |
  | Tests that must commit | database per suite |
  | River | separate queue schema / env |
  | Concurrency | multiple real connections |

  Per-test schema switching is rejected, because RLS, roles and SECURITY DEFINER depend on the real schema.
- **Operation coverage.** A machine-readable map `operation → tests` (e.g. `createAdminSession: [auth_login_success, auth_login_bad_password, auth_login_rate_limited]`). CI fails when an operation is implemented without executed tests: the tests must actually run, not merely exist.
- **Pages.** A browser smoke test per route type: home, product list/detail, blog list/detail, company, contact/inquiry, admin login, main admin pages. Use representative fixtures, not every SKU.
- **Visual / a11y.** Pinned browser, fonts, locale, timezone and viewport; reviewed golden baselines; keyboard tests. Baselines are produced in CI, never on a dev machine.
- **Blocking on PR-02/03:**
  - always blocking: compile/vet/race, OpenAPI generation and contract, module boundary, route coverage;
  - blocking when the PR touches DB/auth: PG18 roles/RLS;
  - blocking once frontend code exists: TS build;
  - blocking for PR-02: login/revocation/cross-tenant E2E;
  - blocking on UI changes: visual tests;
  - 300k-SKU load tests run separately.
- **Fault probes to prove gates:**
  - remove a route's auth;
  - grant `app_runtime` read on `users.password_hash`;
  - drop an operation's contract test;
  - drop a module's downstream test;
  - mobile horizontal overflow;
  - delete a form label;
  - weaken cookie flags.

## A7 — River

**Decision.** River lands in PR-F, not in PR-02/03. River Community + `riverpgxv5`, candidate **v0.49.0**, adopted after a PG18.6 + pgx 5.10.0 compatibility test.

- **Errors.** On a River error the app must roll back explicitly; don't rely on implicit savepoints.
- **Separate schema `queue`** via `Config.Schema`; the migrator and the client use the same schema. River's own migration history is separate from the app's `scripts/migrate.sh` numbering: never fold River migrations into 0xx, never touch 001–010.
- **Install order:** prepare the queue schema with a controlled migrator → pinned River migrations → app migrations → verify queue grants and versions → start API/worker. Queue grant changes go in a forward app migration that runs after the River tables exist. Test both a clean install and an upgrade.
- **Roles:**

  | Role | Rights |
  |---|---|
  | app migrator | business DDL |
  | River migrator | queue DDL |
  | runtime producer | insert-job only |
  | worker | job processing + controlled business operations |

  River tables have no `tenant_id`, so the business RLS template does not apply to them. Job args carry a trusted tenant context and a stable kind; the worker executes through controlled services, so a tenant id in the job JSON grants nothing.
- **Transactions.** `BEGIN; SET LOCAL app.tenant_id; domain write; River InsertTx; COMMIT`. On rollback no job exists. Delivery is at-least-once; idempotency comes from DB unique keys, event ids and state machines. No Pro-only features (workflows, DLQ). No Redis/Kafka.
- **Rejected:** a shared high-privilege login for River and runtime; River auto-DDL on startup; dependence on Pro features; Redis/Kafka.
- **Gate:**
  - pinned River migrates on PG18.6;
  - queue and app permissions stay isolated, and the runtime has no queue DDL;
  - a job is invisible after rollback and visible after commit;
  - a worker crash recovers;
  - a double execution has no duplicate effect;
  - an unknown kind is refused, and a forged tenant is denied;
  - both migration histories verify;
  - nothing depends on Pro features.

## Card split (one small mergeable PR each)

```
03a toolchain + module skeleton ─┬─ 03b OpenAPI auth/tenant routes + oapi-codegen POC ── 03c RouteSpec default-deny, RFC9457, JSON
                                 └─ 03d registry + import graph + SQL ownership ── 03e operation-coverage matrix + summary gate
02a auth migration proposal (login attempts, key index, credential-writer role) + bootstrap CLI
02b Argon2id, login/logout, session, rate limit ── 02c cookie/CSRF/Origin/rotation/revocation ── 02d API keys
02e /admin/t/{tenant} + AuthorizedActor + RunAuthorized + 3 PG pools ── 02f E2E (login, whoami, tenant switch, logout, API key, security) + runbook
```

| Card | Scope | Not in scope | Gate (red→green) |
|---|---|---|---|
| 03a | module path, pinned toolchain (mostly C-01), `cmd/api` + module skeleton | auth, business logic | clean build; old import path or version drift fails CI |
| 03b | OpenAPI cookie/bearer security, auth operations, tenant routes; oapi-codegen POC | real sessions | generated code compiles; undeclared field / missing response fails contract test |
| 03c | RouteSpec, default-deny auth middleware, `AuthenticatedSelf` class, RFC 9457, body size / strict JSON | business handlers | tenant route without scope fails; `AuthenticatedSelf` route with a scope or tenant param fails |
| 03d | `modules/registry.yaml`, import graph, SQL ownership reconcile; move the two misfiled queries | microservices | illegal import, cross-module table write, cycle each fail |
| 03e | affected-CI, operation coverage matrix, summary gate | all E2E pages | missing test, skipped required job, wrong impact each fail |
| 02a | forward migration: login attempts, key public-id index, credential-writer role, auth tenant-status read; `cmd/bootstrap-admin`; update `deploy/runtime-roles.md` | public signup | real PG18 role checks; wrong-role write and cross-tenant denied |
| 02b | Argon2id (dummy-hash path for unknown/disabled), identity lookup, session create/logout, errors, login rate limit (IP, account+IP, account-wide) | customer login | bad password, unknown user (same work/timing class), brute force, distributed multi-IP guessing hits account-wide cooldown, owner with known-device cookie logs in during a sustained attack, expired session, logout without membership |
| 02c | admin cookie, CSRF, Origin, conditional-revoke rotation, revocation | OAuth/OIDC | weak cookie flags, missing CSRF, replayed old token all fail; concurrent rotation yields one successor |
| 02d | API key issue/verify/revoke, tenant binding (`key.tenant_id == route tenant`), scope intersection, one-time secret | service accounts, dynamic roles | revoked/expired/issuer-lost/over-scope denied; key for A used on B denied even when the issuer is a member of both |
| 02e | `/admin/t/{tenant}` + tenant API routes, `AuthorizedActor`, `RunAuthorized`, three PG pools | catalog/order commands | real PG18 cross-tenant, revoked membership, connection reuse |
| 02f | E2E of login, whoami, tenant switch, logout, API key and security scenarios; deploy runbook | full admin UI | all auth paths, fault probes and role checks pass on one SHA |

**Constraints.** PR-02 does not open the existing organization/site/product write operations: they need PR-F idempotency. `internal/tenanttx` stays a low-level tool; handlers go through `RunAuthorized` only.
