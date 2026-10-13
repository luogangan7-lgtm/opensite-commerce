-- Responsibility: global staff-session activity timestamp and minimal touch ACL.
-- Dependencies: migrations 001..015; callers: private identity login repository.
-- Invariants: no tenant/RLS changes; auth can update only revocation/activity.
ALTER TABLE app.auth_sessions ADD COLUMN last_seen_at timestamptz NOT NULL DEFAULT now();
-- Preserve previously-issued sessions without making their age disappear.
UPDATE app.auth_sessions SET last_seen_at=created_at;
ALTER TABLE app.auth_sessions ADD CONSTRAINT auth_sessions_finite_activity
 CHECK (isfinite(created_at) AND isfinite(last_seen_at) AND isfinite(expires_at)
        AND last_seen_at>=created_at AND expires_at>created_at) NOT VALID;
-- Existing malformed historical rows fail admission; new writes must be valid.
GRANT UPDATE(last_seen_at) ON app.auth_sessions TO app_auth;
