# Idempotency transaction protocol

This package supplies request validation/fingerprinting, SQL011 claim/completion,
and durable response replay. HTTP handlers authorize first and supply the
resolved tenant, actor kind/ID and registered operation; no scope field comes
from untrusted JSON. The key is scoped by the complete existing primary key:
`(tenant_id, actor_kind, actor_id, operation, idempotency_key)`.

`ValidateKey` accepts exactly one header value across case-insensitive header
names. It preserves the key and requires 1..128 ASCII bytes in `0x21..0x7e`
(printable, no spaces). Missing, empty, duplicate or invalid keys are HTTP400
`invalid_idempotency_key`.

`FingerprintRequest` validates method, route template and URL and calls
`url.ParseQuery` so malformed percent escapes/semicolons fail HTTP400
`invalid_request`; callers using the lower-level `Fingerprint` must parse and
validate RawQuery themselves. Do not use URL.Query's error-discarding behavior.
The versioned SHA256 fingerprint length-frames method, registered route template,
sorted resolved path parameters, query keys and sorted values (retaining
multiplicity), and the exact raw body bytes. Query ordering/encoding is
canonicalized; JSON bodies are never parsed/re-encoded, so whitespace changes
matter. Read the route's bounded raw body once and include it even when empty.

With `-tags pgx`, call `ExecuteTx` inside the existing `tenanttx.Run` lifecycle.
The transaction must have the authorized `app.tenant_id` set before any domain
query. Claim, business database writes and completed response are one pgx.Tx.
The package does not begin/commit/rollback; the caller must roll back every
error, panic and canceled command and commit before emitting HTTP bytes. Domain
callbacks must not perform irreversible external writes; use the transaction's
outbox for external work. `ClaimTx`/`CompleteTx` expose the same primitives when
a caller needs a split callback. Claim ownership fields are private.

The fingerprint is compared before interpreting state: a changed request is
HTTP422 `idempotency_key_reused`, including after expiry/completion; an active
matching claim is HTTP409 `idempotency_in_progress`; a matching completed request
replays the exact saved status, repeated header values and binary body. Expired
matching in-progress claims can be taken over with a new token. SQL completion
sets transaction-local `app.idempotency_claim` and predicates on current token,
request hash and full scope. A stale owner returns `ErrStaleClaim`.

Configure a `ResponseStore`; its implementations must be immutable, durable and
bound both Put/Get body sizes. `NewLocalResponseStore(root,maxBodyBytes)` is the
included adapter, using `storage/local` private fsync-backed content-addressed
objects. Use a persistent root across process restarts and configure the command
response admission limit. SQL stores the opaque reference (<=2048 bytes) and
status; response JSON/body stays out of SQL. Put occurs before SQL completion and
commit: rollback can leave an unreferenced immutable object; only records in SQL
can authorize replay. Garbage collection is a separate concern.

Responses require final status200..599; 204/304 bodies must be empty. Header
names/values are validated, invalid UTF-8/control values and ambiguous case
aliases are rejected, total headers are capped at64KiB, and Content-Length must
be one decimal value matching body length. Header names are canonicalized while
all admitted values and their order are preserved. Set-Cookie and hop-by-hop
headers (Connection, Keep-Alive, Proxy-Authenticate, Proxy-Authorization, TE,
Trailer, Transfer-Encoding, Upgrade) are rejected, never silently stripped.
They are connection/session concerns and must not be command results. Local Get
verifies object digest, envelope version/shape and configured limits. Replay
also checks the durable saved status against the SQL status. Missing/corrupt
objects return errors; they never rerun business work.

# Acceptance and fault injection

Default
`go test ./internal/platform/idempotency` covers key128/129, every fingerprint
axis, raw/empty body, canonical query, malformed query, durable restart/new-store
replay, exact binary/repeated headers, mutation isolation and malformed/tampered
responses.

`DATABASE_URL=<disposable migrated admin connection> go test -tags pgx
./internal/platform/idempotency -run TestPGIdempotency -count=1 -timeout=120s`
requires a disposable migrated PG18 database and app_runtime role. Missing DSN
is a NOT_RUN skip; IDEMPOTENCY_REQUIRE_PG=1 makes it a failure in gates. The suite creates random tenants and
cleans its own rows. It checks exact replay/one domain write, full scope isolation,
409/422, rollback on domain/store errors and cancellation, tenant-runner panic
cleanup, expired takeover and direct SQLSTATE55000 fencing. Two concurrent cases
hold a first transaction with an advisory lock, then observe owner and competitor
Lock waits via pg_stat_activity/pg_blocking_pids before release. No sleep proves
ordering; contexts bound background work and cleanup.

For a behavioral mutation gate on a disposable copy, change only
`if !bytes.Equal(record.RequestHash, hash[:]) {` in postgres.go to
`if !bytes.Equal(record.RequestHash, hash[:]) && false {`. Run
`-run TestPGIdempotencyExactReplayAndScope`: a changed completed request incorrectly
replays, and the expected422 assertion must turn red. Restore the file, bind its
hash, rerun green and retain both logs. This mutation compiles and changes the
semantic branch rather than producing a compile-only red. A second simple
HTTP-boundary mutation is validKey's max128 to129; TestValidateKey must turn red
on the129-byte case, then restore and rerun.
