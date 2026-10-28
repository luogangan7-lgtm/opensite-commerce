-- Responsibility: typed private authentication persistence and admission.
-- Dependencies: global identity schema; callers: private login store only.
-- Invariants: exact dedicated LOGIN authority, active user, atomic rotation.
-- Admission uses volatile DB time, including row-lock EPQ rechecks; arrival
-- timestamps only advance activity monotonically and cannot set future activity.
-- name: ValidateAuthConnection :one
SELECT current_user=session_user
 AND EXISTS(SELECT FROM pg_catalog.pg_roles r WHERE r.rolname=current_user
  AND r.rolcanlogin AND r.rolinherit AND NOT (r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication))
 AND pg_catalog.pg_has_role(current_user,'app_auth','USAGE')
 AND NOT EXISTS(SELECT FROM pg_catalog.pg_roles r WHERE r.rolname NOT IN (current_user,'app_auth')
  AND pg_catalog.pg_has_role(current_user,r.oid,'MEMBER'))
 AND NOT pg_catalog.has_schema_privilege(current_user,'app','CREATE')
 AND NOT EXISTS(SELECT FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
  CROSS JOIN (VALUES ('SELECT'),('INSERT'),('UPDATE'),('DELETE'),('TRUNCATE'),('REFERENCES'),('TRIGGER'),('MAINTAIN')) AS p(privilege)
  WHERE n.nspname='app' AND c.relkind IN ('r','p') AND
   (pg_catalog.has_table_privilege(current_user,c.oid,p.privilege) IS DISTINCT FROM pg_catalog.has_table_privilege('app_auth',c.oid,p.privilege)
    OR pg_catalog.has_table_privilege(current_user,c.oid,p.privilege||' WITH GRANT OPTION')))
 AND NOT EXISTS(SELECT FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid
  JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
  CROSS JOIN (VALUES ('SELECT'),('INSERT'),('UPDATE'),('REFERENCES')) AS p(privilege)
  WHERE n.nspname='app' AND c.relkind IN ('r','p') AND a.attnum>0 AND NOT a.attisdropped AND
   (pg_catalog.has_column_privilege(current_user,c.oid,a.attnum,p.privilege) IS DISTINCT FROM pg_catalog.has_column_privilege('app_auth',c.oid,a.attnum,p.privilege)
    OR pg_catalog.has_column_privilege(current_user,c.oid,a.attnum,p.privilege||' WITH GRANT OPTION')))
 AND NOT EXISTS(SELECT FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname='app' AND
   (pg_catalog.has_function_privilege(current_user,p.oid,'EXECUTE') IS DISTINCT FROM pg_catalog.has_function_privilege('app_auth',p.oid,'EXECUTE')
    OR pg_catalog.has_function_privilege(current_user,p.oid,'EXECUTE WITH GRANT OPTION'))) AS allowed;

-- name: LoginCredential :one
SELECT id,password_hash,status FROM app.users WHERE email_normalized=sqlc.arg(identifier)::text;

-- name: LoginCooldowns :one
SELECT (SELECT CASE WHEN isfinite(cooldown_until) THEN cooldown_until ELSE clock_timestamp() END FROM app.auth_account_attempts WHERE identifier_normalized=sqlc.arg(identifier)::text AND cooldown_until>clock_timestamp())::timestamptz AS account_until,
 (SELECT CASE WHEN isfinite(cooldown_until) THEN cooldown_until ELSE clock_timestamp() END FROM app.auth_login_attempts WHERE identifier_normalized=sqlc.arg(identifier)::text AND ip=sqlc.arg(ip)::inet AND cooldown_until>clock_timestamp())::timestamptz AS ip_until;

-- name: LoginRecordFailure :exec
SELECT app.auth_record_failure(sqlc.arg(identifier)::text,sqlc.arg(ip)::inet);

-- name: LoginDevice :one
SELECT id,user_id,expires_at,revoked_at FROM app.known_devices
 WHERE cookie_hash=sqlc.arg(cookie_hash)::bytea AND revoked_at IS NULL AND expires_at>clock_timestamp() AND isfinite(expires_at);

-- name: LoginFailDevice :exec
SELECT app.auth_fail_known_device(sqlc.arg(id)::uuid);

-- name: LoginRotateOldSession :execrows
UPDATE app.auth_sessions s SET revoked_at=clock_timestamp()
 FROM app.users u WHERE s.token_hash=sqlc.arg(token_hash)::bytea
 AND s.user_id=sqlc.arg(user_id)::uuid AND u.id=s.user_id AND u.status='active'
 AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
 AND isfinite(s.expires_at) AND isfinite(s.created_at) AND isfinite(s.last_seen_at)
 AND s.last_seen_at>=s.created_at AND s.expires_at>s.created_at
 AND s.created_at<=clock_timestamp() AND s.last_seen_at<=clock_timestamp()
 AND s.last_seen_at>clock_timestamp()-sqlc.arg(idle_seconds)::bigint*interval '1 second';

-- name: LoginLockUser :one
SELECT id FROM app.users WHERE id=sqlc.arg(user_id)::uuid
 AND app.auth_lock_login_user(sqlc.arg(user_id)::uuid,sqlc.arg(verified_password_hash)::text);

-- name: LoginLockSession :one
SELECT id FROM app.auth_sessions WHERE token_hash=sqlc.arg(token_hash)::bytea FOR UPDATE;

-- name: LoginRotationSession :one
SELECT EXISTS(SELECT FROM app.auth_sessions s JOIN app.users u ON u.id=s.user_id
 WHERE s.token_hash=sqlc.arg(token_hash)::bytea AND s.user_id=sqlc.arg(user_id)::uuid
 AND u.status='active' AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
 AND isfinite(s.expires_at) AND isfinite(s.created_at) AND isfinite(s.last_seen_at)
 AND s.last_seen_at>=s.created_at AND s.expires_at>s.created_at
 AND s.created_at<=clock_timestamp() AND s.last_seen_at<=clock_timestamp()
 AND s.last_seen_at>clock_timestamp()-sqlc.arg(idle_seconds)::bigint*interval '1 second') AS allowed;

-- name: LoginInsertSession :one
WITH instant AS MATERIALIZED (SELECT clock_timestamp()::timestamptz AS at)
INSERT INTO app.auth_sessions(user_id,token_hash,created_at,last_seen_at,expires_at)
 SELECT u.id,sqlc.arg(token_hash)::bytea,instant.at,instant.at,
 instant.at+(sqlc.arg(expires_at)::timestamptz-sqlc.arg(created_at)::timestamptz)
 FROM app.users u CROSS JOIN instant WHERE u.id=sqlc.arg(user_id)::uuid AND u.status='active'
 AND u.password_hash=sqlc.arg(verified_password_hash)::text
 RETURNING id,user_id,created_at,last_seen_at,expires_at,clock_timestamp()::timestamptz AS database_now;

-- name: LoginInsertDevice :exec
WITH instant AS MATERIALIZED (SELECT clock_timestamp()::timestamptz AS at)
INSERT INTO app.known_devices(user_id,cookie_hash,created_at,expires_at)
 SELECT sqlc.arg(user_id)::uuid,sqlc.arg(cookie_hash)::bytea,instant.at,
 instant.at+(sqlc.arg(expires_at)::timestamptz-sqlc.arg(created_at)::timestamptz) FROM instant;

-- name: LoginLookupSession :one
-- Admission reads valid authority independently of optional activity updates.
-- Keep the caller timestamp argument for the existing port; only DB time renews.
WITH input AS MATERIALIZED (
 SELECT clock_timestamp() AS at_db,sqlc.arg(at)::timestamptz AS caller_at
), valid AS MATERIALIZED (
 SELECT s.id,s.user_id,s.created_at,s.last_seen_at,s.expires_at,i.at_db
 FROM app.auth_sessions s JOIN app.users u ON u.id=s.user_id CROSS JOIN input i
 WHERE s.token_hash=sqlc.arg(token_hash)::bytea AND u.status='active'
 AND s.revoked_at IS NULL AND s.expires_at>i.at_db
 AND isfinite(s.expires_at) AND isfinite(s.created_at) AND isfinite(s.last_seen_at)
 AND s.last_seen_at>=s.created_at AND s.expires_at>s.created_at
 AND s.created_at<=i.at_db AND s.last_seen_at<=i.at_db
 AND s.last_seen_at>i.at_db-sqlc.arg(idle_seconds)::bigint*interval '1 second'
), touchable AS MATERIALIZED (
 SELECT s.id FROM app.auth_sessions s JOIN valid v ON v.id=s.id
 WHERE s.last_seen_at<=v.at_db-greatest(1,least(60,sqlc.arg(idle_seconds)::bigint/4))*interval '1 second'
 FOR UPDATE OF s SKIP LOCKED
), touched AS (
 UPDATE app.auth_sessions s SET last_seen_at=clock_timestamp()
 FROM touchable c WHERE c.id=s.id
 AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
 AND isfinite(s.expires_at) AND isfinite(s.created_at) AND isfinite(s.last_seen_at)
 AND s.last_seen_at>=s.created_at AND s.expires_at>s.created_at
 AND s.created_at<=clock_timestamp() AND s.last_seen_at<=clock_timestamp()
 AND s.last_seen_at>clock_timestamp()-sqlc.arg(idle_seconds)::bigint*interval '1 second'
 RETURNING s.id,s.last_seen_at
)
SELECT v.id,v.user_id,v.created_at,coalesce(t.last_seen_at,v.last_seen_at)::timestamptz AS last_seen_at,v.expires_at,clock_timestamp()::timestamptz AS database_now
 FROM valid v LEFT JOIN touched t ON t.id=v.id
 WHERE v.expires_at>clock_timestamp()
 AND coalesce(t.last_seen_at,v.last_seen_at)>clock_timestamp()-sqlc.arg(idle_seconds)::bigint*interval '1 second';

-- name: LoginRevokeSession :execrows
-- Retain the caller timestamp port, but only DB time mutates state.
WITH input AS MATERIALIZED (SELECT sqlc.arg(at)::timestamptz AS caller_at)
UPDATE app.auth_sessions SET revoked_at=clock_timestamp()
 WHERE id=sqlc.arg(id)::uuid AND revoked_at IS NULL;

-- name: MerchantStepUpIdentifier :one
SELECT email_normalized FROM app.users WHERE id=sqlc.arg(user_id)::uuid AND status='active';

-- name: MerchantIssueProof :one
SELECT app.auth_issue_merchant_proof(sqlc.arg(tenant_id)::uuid,sqlc.arg(user_id)::uuid,
 sqlc.arg(session_id)::uuid,sqlc.arg(action)::text,sqlc.arg(proof_digest)::bytea,
 sqlc.arg(verified_hash)::text)::timestamptz AS expires_at;

-- name: MerchantConsumeProof :exec
SELECT app.auth_consume_merchant_proof(sqlc.arg(tenant_id)::uuid,sqlc.arg(user_id)::uuid,
 sqlc.arg(session_id)::uuid,sqlc.arg(action)::text,sqlc.arg(proof_digest)::bytea);

-- name: MerchantReceiptAllowed :one
SELECT app.auth_merchant_receipt(sqlc.arg(tenant_id)::uuid,sqlc.arg(request_id)::uuid,
 sqlc.arg(user_id)::uuid,sqlc.arg(session_id)::uuid)::boolean AS allowed;

-- name: MerchantEraseProofs :one
SELECT app.auth_erase_merchant_proofs(sqlc.arg(tenant_id)::uuid,sqlc.arg(request_id)::uuid,
 sqlc.arg(epoch)::bigint)::bigint AS erased;
