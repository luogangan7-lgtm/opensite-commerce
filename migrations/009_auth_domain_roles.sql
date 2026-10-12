-- 009 / Forward-only role hardening. Do not edit migrations 001..008.
-- Separate credential lookup and anonymous domain resolution from app_runtime.
-- These NOLOGIN roles must only be granted to dedicated, private application
-- login roles provisioned with secrets OUTSIDE the repository.
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='app_auth') THEN
  CREATE ROLE app_auth NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
 END IF;
 IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='app_domain_resolver') THEN
  CREATE ROLE app_domain_resolver NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
 END IF;
END $$;

GRANT USAGE ON SCHEMA app TO app_auth, app_domain_resolver;

-- app_auth resolves passwords/token hashes and memberships. It must never be
-- exposed to an untrusted client, Agent SQL or a public Query API.
GRANT SELECT ON app.users, app.auth_sessions, app.api_keys,
                app.tenant_memberships TO app_auth;
GRANT INSERT ON app.auth_sessions TO app_auth;
GRANT UPDATE (revoked_at) ON app.auth_sessions TO app_auth;

-- Memberships and keys already have FORCE RLS from migration 008.
-- Credential verification deliberately spans tenants; use a separate role.
CREATE POLICY auth_membership_read ON app.tenant_memberships
 FOR SELECT TO app_auth USING (true);
CREATE POLICY auth_keys_read ON app.api_keys
 FOR SELECT TO app_auth USING (true);

-- Anonymous routing is restricted to already verified hostnames and already
-- published sites; no credential or private company document access.
GRANT SELECT (tenant_id,site_id,normalized_hostname,status,verified_at)
 ON app.site_domains TO app_domain_resolver;
GRANT SELECT (tenant_id,id,published_release_id,default_locale)
 ON app.sites TO app_domain_resolver;
CREATE POLICY public_verified_domains ON app.site_domains
 FOR SELECT TO app_domain_resolver
 USING (status='verified' AND verified_at IS NOT NULL);
CREATE POLICY public_published_sites ON app.sites
 FOR SELECT TO app_domain_resolver
 USING (published_release_id IS NOT NULL);

-- Every newly installed tenant business table remains default-deny unless
-- an explicitly audited new migration grants its policy and role privileges.
-- No DELETE / TRUNCATE / DDL is granted to any application role.
