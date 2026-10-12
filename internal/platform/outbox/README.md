# Dispatcher contract

Trusted composition supplies a tenant and an app_outbox_worker pool to PGStore.
Each claim/complete/fail uses a separate committed tenant transaction and the SQL
011 lease functions. Dispatch sends through an injected Sender only after claim
commit. It sends one claimed event at a time with a deadline bounded by the lease.
Sender must honor cancellation; its errors are persisted only as the stable
OUTBOX_SEND_FAILED code with bounded exponential backoff.

Delivery is at-least-once. A consumer must atomically deduplicate
TenantID + EventID + Target (or TenantID + Target + DedupKey) with its side effect.
A successful send followed by a crash or ambiguous acknowledgment leaves the
event for lease takeover. Old owner/attempt/expiry confirmations are rejected;
false acknowledgment is an error, never reported as completion.

This card supplies a test Sender and a callable Dispatcher skeleton. It does not
configure real webhooks, tenant enumeration, network destinations or production
credentials. Those adapters must provide destination validation and the consumer
deduplication contract when introduced.
