-- Responsibility: clock migration behavior on existing global identity tables.
-- Dependencies: all forward migrations; callers: PG runner and populated upgrade.
-- Invariants: expired devices cannot accumulate failures; auth ACL stays exact.
BEGIN;
INSERT INTO app.users(email,password_hash) VALUES('clock-sql@example.test','fixture-only') RETURNING id AS u \gset
INSERT INTO app.known_devices(user_id,cookie_hash,created_at,expires_at)
 VALUES(:'u',sha256('clock-sql'::bytea),clock_timestamp()-interval '2 hours',clock_timestamp()-interval '1 hour') RETURNING id AS d \gset
SET LOCAL ROLE app_auth;
SELECT app.auth_fail_known_device(:'d');
RESET ROLE;
DO $gate$ BEGIN
 IF EXISTS(SELECT FROM app.known_devices WHERE cookie_hash=sha256('clock-sql'::bytea) AND (failure_count<>0 OR revoked_at IS NOT NULL)) THEN
  RAISE EXCEPTION 'CLOCK_EXPIRED_DEVICE_MUTATED'; END IF;
END $gate$;
ROLLBACK;
