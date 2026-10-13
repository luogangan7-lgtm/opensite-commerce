# Published read context

`getPublishedSite` resolves one verified DNS hostname to its current sealed release.
The dedicated `OSC_PUBLIC_DATABASE_URL` login must inherit only
`app_domain_resolver`, with no elevated attributes, write privileges, private
column grants, object ownership or application routine execution. Startup checks
that capability; there is no runtime/admin pool fallback. All public connection
handling belongs to `pgxadapter`. Each request uses a local host scope and role,
a read-only repeatable-read transaction, and one snapshot SQL statement.

The storefront forward migration extends the existing 009 resolver role without a new table,
SECURITY DEFINER routine or RLS bypass. The visibility dependency chain is domains
→ sites → current sealed release → routes → exact pinned page revisions. Existing
FORCE RLS remains in force. Unknown, unverified, disabled and unpublished hosts
return the same 404. Malformed hostname authorities are 400; unavailable or
invalid snapshots are 503. Forwarded host headers are ignored.

Version 1 release manifests contain only `version`, `locale`, `title`,
`description`, `navigation` and `products`. Navigation is `{label,href}` and
products are `{id,slug,name,description,unit_amount,currency}`. Amounts are decimal
strings, never JavaScript numbers. The manifest locale must equal the site's
current default locale or projection fails closed. Product data is an immutable
publish snapshot derived from catalog published listings and pinned content revisions.
The sample fixture constructs it from real catalog rows. Later catalog drafts
never replace a sealed snapshot. Production publish generation belongs to C-09b;
there is no mutable catalog fallback.

A pinned page document contains only `{title,description,blocks}`, with blocks
`{kind:'heading'|'paragraph',text}`. There is no raw HTML field or HTML rendering
contract. SQL policies also reject unknown nested fields and nontext block kinds.
Go validation bounds collections/text and accepts canonical local paths only.
Navigation targets must resolve within the returned pages or product views.

Tests: `go test ./internal/modules/publish/... ./internal/platform/httpapi`;
`go test -tags pgx ./internal/modules/publish/...` with a migrated disposable
`DATABASE_URL`; `tests/db_storefront.sql` via `check_pg.sh`. The pgx test seeds the
C09 fixture through the adapter and provisions/cleans a random dedicated public
login. Setting DATABASE_URL makes failed setup fatal; no setup failure is skipped.
Security fault probes broaden current-release visibility or pinned-revision
visibility and must make the SQL regression fail before restoring the policy.
