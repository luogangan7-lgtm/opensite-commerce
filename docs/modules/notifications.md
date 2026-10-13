# Notifications

`internal/modules/notifications/contract` exposes the staff failure-list and
idempotent redrive service, immutable failure/receipt DTOs, and frozen sender
messages. `notifications.New` receives an already authorized tenant runner.
`NewWorker` receives only the app_outbox_worker QueueDB capability and a Sender;
`OutboxStore` claims only `notifications.email`, preserving other consumers.

Notifications owns `notification_deliveries` and append-only
`notification_redrive_requests`. The forward notifications migration fans out orders' paid, shipment
and refund events, plus optional quotes' sent/approved events. SQL resolves
contacts from authoritative orders/customers and linked quote/inquiry/customer
records; event payloads provide reference IDs, never recipients. The recipient,
source event, template, locale and reference are frozen. Invalid authoritative
contacts produce a visible terminal failure without rolling back the business
transaction. Missing optional quote schema produces no dispatch until installed.

A dedicated `notification.dispatch` outbox event contains only notification_id.
The worker supplies its current lease owner and attempt to the token admission
command, commits, then renders and sends outside the transaction. Completion
checks the token and live lease. A sent row replay completes without another
send. A leftover sending row or ambiguous SMTP DATA acknowledgment becomes
uncertain and never automatically resends. Known failures reuse the outbox's
bounded backoff and ten-attempt dead-letter cap. Staff may redrive a known
terminal failure with an explicit key; the append-only request preserves its
receipt token and generation across retries. Uncertain and missing-contact
failures reject redrive for manual investigation.

Templates are fixed code for English and Chinese, with plaintext plus escaped
HTML. Dedup keys hash source event + canonical recipient + template, so retries
and manual generations retain a stable Message-ID. Fake is concurrency safe,
honors cancellation and deduplicates this key; its returned snapshots copy data.
SMTP cannot guarantee exactly-once delivery: acceptance after DATA may be unknown.

The executable's local SMTP adapter is disabled unless
`NOTIFICATIONS_SMTP_ENABLED=true`; configuration requires
`NOTIFICATIONS_SMTP_HOST` (loopback IP or localhost pinned to 127.0.0.1) and
`NOTIFICATIONS_SMTP_PORT`. Optional USER/PASSWORD are read only from environment
variables, never returned or logged. APP_ENV/ENVIRONMENT=production is refused.
Only `.test`, `.invalid`, and `localhost` recipient domains are admitted by SMTP,
with fixed `notifications@safe.test` sender. MIME is multipart/alternative with
quoted-printable bodies, encoded subjects and stable Message-ID. Context and
socket deadlines bound the whole session. Failure after DATA is uncertain;
QUIT failure after the 250 DATA response does not undo accepted delivery.

Gate commands: Go 1.27.2 `go test -race ./internal/modules/notifications/...`
and, against an expendable fully migrated PG18 database,
`go test -tags pgx -race ./internal/modules/notifications/...`. The latter proves
fanout, payload recipient rejection, canonical contact, dedicated target claims,
crash-after-send replay, ten known failures, idempotent redrive, cross-tenant
invisibility/404, and uncertain-send no-auto-resend/409. A missing DATABASE_URL
is explicitly NOT_RUN. Loopback SMTP protocol tests prove pre-DATA rejection,
post-DATA ambiguity, actual MIME/Message-ID and cancellation without external
mail. Two semantic probes remove SMTP destination restrictions and uncertain
replay protection: both must go red, then restore byte-identical files and green.

The admin HTTP port uses `AdminOrAPIKey` and `notifications:write`; redrive
requires an ASCII printable `Idempotency-Key` of 1–128 characters and no body.
Bootstrap accepts an existing authenticator and authorized service. Current
executable composition has no staff session implementation and defaults to 401;
this card does not invent identity or cookie/CSRF authority.

Quote compatibility is verified in an independent database with PR #36's real
migration and events. 报价通知随 #36 合入接线/再验收. If quotes are installed after
notifications, integration must add the precise quote/inquiry column grants
and tenant policies shown in the notifications migration before enabling fanout;
notifications deliberately refuses unpermissioned optional quote sources.
