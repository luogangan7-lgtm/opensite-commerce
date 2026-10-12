-- 001 / Forward-only. Run as privileged migrator, never the application role.
CREATE SCHEMA IF NOT EXISTS app;
REVOKE ALL ON SCHEMA app FROM PUBLIC;

CREATE TABLE app.tenants (
 id uuid PRIMARY KEY DEFAULT uuidv7(),
 name text NOT NULL CHECK (btrim(name) <> ''),
 status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','suspended')),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE app.users (
 id uuid PRIMARY KEY DEFAULT uuidv7(),
 email text NOT NULL CHECK (btrim(email) <> ''),
 email_normalized text GENERATED ALWAYS AS (lower(btrim(email))) STORED,
 password_hash text NOT NULL,
 status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE (email_normalized)
);
CREATE TABLE app.tenant_memberships (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 user_id uuid NOT NULL REFERENCES app.users(id),
 role text NOT NULL CHECK (role IN ('owner','admin','catalog_editor','content_editor','viewer')),
 status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','suspended')),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,user_id)
);
CREATE TABLE app.auth_sessions (
 id uuid PRIMARY KEY DEFAULT uuidv7(),
 user_id uuid NOT NULL REFERENCES app.users(id),
 token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash)=32),
 expires_at timestamptz NOT NULL,
 revoked_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE app.api_keys (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 id uuid NOT NULL DEFAULT uuidv7(),
 issued_by uuid NOT NULL REFERENCES app.users(id),
 key_prefix text NOT NULL,
 key_hash bytea NOT NULL CHECK (octet_length(key_hash)=32),
 scopes text[] NOT NULL DEFAULT '{}',
 expires_at timestamptz,
 revoked_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,id),
 UNIQUE (tenant_id,key_hash)
);
