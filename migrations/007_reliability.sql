-- 007 / Idempotency, audit, transactional integration events.
CREATE TABLE app.idempotency_records (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 actor_kind text NOT NULL CHECK (actor_kind IN ('user','api_key','service')),
 actor_id uuid NOT NULL,
 operation text NOT NULL,
 idempotency_key text NOT NULL,
 request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
 state text NOT NULL DEFAULT 'in_progress' CHECK (state IN ('in_progress','completed','failed')),
 response_status integer,
 response_body jsonb,
 expires_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,actor_kind,actor_id,operation,idempotency_key)
);
CREATE TABLE app.audit_events (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 id uuid NOT NULL DEFAULT uuidv7(),
 actor_kind text NOT NULL,
 actor_id uuid,
 operation text NOT NULL,
 resource_type text NOT NULL,
 resource_id uuid,
 before_revision bigint,
 after_revision bigint,
 payload jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(payload)='object'),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,id)
);
CREATE TABLE app.outbox_events (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 id uuid NOT NULL DEFAULT uuidv7(),
 event_type text NOT NULL,
 payload jsonb NOT NULL CHECK (jsonb_typeof(payload)='object'),
 attempts integer NOT NULL DEFAULT 0 CHECK (attempts>=0),
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','processing','delivered','failed')),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,id)
);
CREATE INDEX outbox_pending ON app.outbox_events(status,next_attempt_at) WHERE status='pending';
