# Dispatcher contract

Trusted composition supplies a tenant and an app_outbox_worker pool to PGStore.
Each claim/complete/fail uses a separate committed tenant transaction and the SQL
lease functions (011, then the additive reliability-limits migration). Dispatch sends through an injected Sender only after claim
commit. It sends one claimed event at a time with a deadline bounded by the lease.
Sender must honor cancellation; its errors are persisted only as the stable
OUTBOX_SEND_FAILED code with bounded exponential backoff.
Senders may additionally implement `LeaseSender` to receive the unmodified
worker/attempt/expiry fence and a copied payload for controlled acceptance.
Dispatcher prefers that capability; ordinary `Sender` implementations still
receive `Delivery` through the existing path. Both must honor cancellation.

Delivery is at-least-once. A consumer must atomically deduplicate
TenantID + EventID + Target (or TenantID + Target + DedupKey) with its side effect.
A successful send followed by a crash or ambiguous acknowledgment leaves the
event for lease takeover. Old owner/attempt/expiry confirmations are rejected;
false acknowledgment is an error, never reported as completion.

Attempts are capped at ten. Failure of attempt ten enters `dead_letter`; an
expired final lease is retired by the next tenant claim without an eleventh send.
A live final attempt may still complete. Tenant-scoped readers can query terminal
rows with `WHERE status = 'dead_letter'`; this card adds no redrive command.

`Runner` discovers pending/processing/failed tenant IDs through the controlled
`app.outbox_active_tenants` broker. The broker has only two SELECT columns,
`tenant_id` and `status`, an explicit cross-tenant SELECT policy, and no role
memberships. The outbox worker can execute discovery but cannot read its table or
payload directly. Cursor pages resume on each poll and reset after an empty page;
each dispatch sets and clears tenant context in its independent transaction.

The cycle timeout bounds discovery and new claims, not an event already claimed.
Budget exhaustion yields until the next poll without stopping queue jobs. Cursor
progress survives the yield: a tenant with a finished claim advances the cursor,
and its remaining events wait for the next traversal so later tenants get a turn.
An active send and its successful acknowledgment use the explicit parent context
with a deadline capped by the returned lease and configured lease duration.
Parent cancellation still stops the active send. Send failures retain the bounded
detached failure-recording budget and lease fencing; ambiguous acknowledgments
and genuine store errors still stop dispatch and leave takeover to the lease.

`cmd/worker` calls the shared startup path. Queue processing and dispatch use
separate role pools. Outbox defaults off (`OUTBOX_ENABLED=false`); opt-in needs
`OUTBOX_DATABASE_URL` and an injected reviewed Sender. The current executable
has no real Sender: enabling it fails before opening pools or claiming events.
Tests exercise that same startup path with a test Sender and prove dynamic tenant
discovery against PG18.6. A later adapter card must register validated destinations
and consumer deduplication before production dispatch can be enabled.

Known limits: payload handling and real network adapters are deferred. Send
deadlines use the application clock capped by the returned database lease;
clock skew can cause retries or ambiguous sends, so consumer deduplication remains
required. Dead-letter redrive and alerting need an explicit operator workflow.
