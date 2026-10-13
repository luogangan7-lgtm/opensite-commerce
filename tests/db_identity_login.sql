-- Responsibility: real SET ROLE staff-session admission/touch/ACL gate.
-- Dependencies: migrations through 016; caller: disposable privileged PG18 gate.
-- Invariants: expired/revoked/disabled/idle/future sessions never touch; older
-- arrivals preserve newer activity and may rotate; DB execution clock admits.
-- UPDATE bodies mirror production queries/identity/login.sql (only binds/RETURNING differ).
\set ON_ERROR_STOP on
BEGIN;
INSERT INTO app.users(email,password_hash) VALUES('c02b-active@example.test','fixture-only') RETURNING id AS active_user \gset
INSERT INTO app.users(email,password_hash,status) VALUES('c02b-disabled@example.test','fixture-only','disabled') RETURNING id AS disabled_user \gset
SELECT set_config('test.login_active',:'active_user',true);
SELECT set_config('test.login_disabled',:'disabled_user',true);
INSERT INTO app.auth_sessions(user_id,token_hash,created_at,last_seen_at,expires_at,revoked_at) VALUES
 (:'active_user',sha256('login-live'::bytea),clock_timestamp()-interval '10 minutes',clock_timestamp()-interval '5 minutes',clock_timestamp()+interval '1 day',NULL),
 (:'active_user',sha256('login-expired'::bytea),clock_timestamp()-interval '10 minutes',clock_timestamp()-interval '5 minutes',clock_timestamp()-interval '1 second',NULL),
 (:'active_user',sha256('login-idle'::bytea),clock_timestamp()-interval '30 minutes',clock_timestamp()-interval '11 minutes',clock_timestamp()+interval '1 day',NULL),
 (:'active_user',sha256('login-revoked'::bytea),clock_timestamp()-interval '10 minutes',clock_timestamp()-interval '5 minutes',clock_timestamp()+interval '1 day',clock_timestamp()-interval '4 minutes'),
 (:'disabled_user',sha256('login-disabled'::bytea),clock_timestamp()-interval '10 minutes',clock_timestamp()-interval '5 minutes',clock_timestamp()+interval '1 day',NULL),
 (:'active_user',sha256('login-future-seen'::bytea),clock_timestamp()-interval '10 minutes',clock_timestamp()+interval '5 minutes',clock_timestamp()+interval '1 day',NULL),
 (:'active_user',sha256('login-future-created'::bytea),clock_timestamp()+interval '5 minutes',clock_timestamp()+interval '6 minutes',clock_timestamp()+interval '1 day',NULL);
SET LOCAL ROLE app_auth;
DO $admission$
DECLARE admitted app.auth_sessions; n bigint; payload text:='login-live'; arrival timestamptz:=clock_timestamp(); previous_seen timestamptz;
BEGIN
UPDATE app.auth_sessions s SET last_seen_at=greatest(s.last_seen_at,least(arrival::timestamptz,clock_timestamp()))
 FROM app.users u WHERE s.token_hash=sha256(payload::bytea)::bytea AND u.id=s.user_id AND u.status='active'
 AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
 AND isfinite(s.expires_at) AND isfinite(s.created_at) AND isfinite(s.last_seen_at)
 AND s.last_seen_at>=s.created_at AND s.expires_at>s.created_at
 AND s.created_at<=clock_timestamp() AND s.last_seen_at<=clock_timestamp()
 AND s.last_seen_at>clock_timestamp()-600::bigint*interval '1 second'
 RETURNING s.* INTO admitted;
 IF admitted.id IS NULL OR admitted.last_seen_at<arrival THEN RAISE EXCEPTION 'LOGIN_GATE live session touch'; END IF;
 previous_seen:=admitted.last_seen_at;
 -- A newer request has touched this session before this older arrival executes.
 arrival:=arrival-interval '3 seconds';
UPDATE app.auth_sessions s SET last_seen_at=greatest(s.last_seen_at,least(arrival::timestamptz,clock_timestamp()))
 FROM app.users u WHERE s.token_hash=sha256(payload::bytea)::bytea AND u.id=s.user_id AND u.status='active'
 AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
 AND isfinite(s.expires_at) AND isfinite(s.created_at) AND isfinite(s.last_seen_at)
 AND s.last_seen_at>=s.created_at AND s.expires_at>s.created_at
 AND s.created_at<=clock_timestamp() AND s.last_seen_at<=clock_timestamp()
 AND s.last_seen_at>clock_timestamp()-600::bigint*interval '1 second'
 RETURNING s.* INTO admitted;
 IF admitted.id IS NULL OR admitted.last_seen_at<>previous_seen THEN RAISE EXCEPTION 'LOGIN_CLOCK_GATE older arrival lost admission or activity monotonicity'; END IF;
 -- App/DB skew in an input timestamp cannot write future activity.
 arrival:=clock_timestamp()+interval '1 hour';
UPDATE app.auth_sessions s SET last_seen_at=greatest(s.last_seen_at,least(arrival::timestamptz,clock_timestamp()))
 FROM app.users u WHERE s.token_hash=sha256(payload::bytea)::bytea AND u.id=s.user_id AND u.status='active'
 AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
 AND isfinite(s.expires_at) AND isfinite(s.created_at) AND isfinite(s.last_seen_at)
 AND s.last_seen_at>=s.created_at AND s.expires_at>s.created_at
 AND s.created_at<=clock_timestamp() AND s.last_seen_at<=clock_timestamp()
 AND s.last_seen_at>clock_timestamp()-600::bigint*interval '1 second'
 RETURNING s.* INTO admitted;
 IF admitted.id IS NULL OR admitted.last_seen_at>clock_timestamp() THEN RAISE EXCEPTION 'LOGIN_CLOCK_GATE future caller activity'; END IF;
 -- Even an old request sampled before expiry cannot admit a now-dead session.
 arrival:=clock_timestamp()-interval '20 minutes';
 FOREACH payload IN ARRAY ARRAY['login-expired','login-idle','login-revoked','login-disabled','login-future-seen','login-future-created'] LOOP
UPDATE app.auth_sessions s SET last_seen_at=greatest(s.last_seen_at,least(arrival::timestamptz,clock_timestamp()))
 FROM app.users u WHERE s.token_hash=sha256(payload::bytea)::bytea AND u.id=s.user_id AND u.status='active'
 AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
 AND isfinite(s.expires_at) AND isfinite(s.created_at) AND isfinite(s.last_seen_at)
 AND s.last_seen_at>=s.created_at AND s.expires_at>s.created_at
 AND s.created_at<=clock_timestamp() AND s.last_seen_at<=clock_timestamp()
 AND s.last_seen_at>clock_timestamp()-600::bigint*interval '1 second'
 RETURNING s.* INTO admitted;
  GET DIAGNOSTICS n=ROW_COUNT;
  IF n<>0 THEN RAISE EXCEPTION 'LOGIN_GATE admitted invalid %',payload; END IF;
UPDATE app.auth_sessions s SET revoked_at=clock_timestamp()
 FROM app.users u WHERE s.token_hash=sha256(payload::bytea)::bytea
 AND s.user_id=current_setting('test.login_active')::uuid::uuid AND u.id=s.user_id AND u.status='active'
 AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
 AND isfinite(s.expires_at) AND isfinite(s.created_at) AND isfinite(s.last_seen_at)
 AND s.last_seen_at>=s.created_at AND s.expires_at>s.created_at
 AND s.created_at<=clock_timestamp() AND s.last_seen_at<=clock_timestamp()
 AND s.last_seen_at>clock_timestamp()-600::bigint*interval '1 second';
  GET DIAGNOSTICS n=ROW_COUNT;
  IF n<>0 THEN RAISE EXCEPTION 'LOGIN_GATE rotated invalid %',payload; END IF;
 END LOOP;
 payload:='login-live';
UPDATE app.auth_sessions s SET revoked_at=clock_timestamp()
 FROM app.users u WHERE s.token_hash=sha256(payload::bytea)::bytea
 AND s.user_id=current_setting('test.login_active')::uuid::uuid AND u.id=s.user_id AND u.status='active'
 AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
 AND isfinite(s.expires_at) AND isfinite(s.created_at) AND isfinite(s.last_seen_at)
 AND s.last_seen_at>=s.created_at AND s.expires_at>s.created_at
 AND s.created_at<=clock_timestamp() AND s.last_seen_at<=clock_timestamp()
 AND s.last_seen_at>clock_timestamp()-600::bigint*interval '1 second';
 GET DIAGNOSTICS n=ROW_COUNT;
 IF n<>1 THEN RAISE EXCEPTION 'LOGIN_CLOCK_GATE older arrival prevented rotation'; END IF;
 IF EXISTS(SELECT FROM app.auth_sessions WHERE token_hash=sha256(payload::bytea) AND (revoked_at<last_seen_at OR revoked_at>clock_timestamp())) THEN RAISE EXCEPTION 'LOGIN_CLOCK_GATE invalid revocation time'; END IF;
 BEGIN UPDATE app.auth_sessions SET expires_at=clock_timestamp()+interval '1 year'; RAISE EXCEPTION 'LOGIN_GATE auth changed expiry';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN UPDATE app.auth_sessions SET token_hash=sha256('stolen'::bytea); RAISE EXCEPTION 'LOGIN_GATE auth changed token';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN UPDATE app.auth_sessions SET user_id=current_setting('test.login_disabled')::uuid; RAISE EXCEPTION 'LOGIN_GATE auth changed owner';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN UPDATE app.auth_sessions SET created_at=clock_timestamp()-interval '1 year'; RAISE EXCEPTION 'LOGIN_GATE auth changed creation';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $admission$;
SET LOCAL ROLE app_runtime;
DO $runtime$
BEGIN
 BEGIN UPDATE app.auth_sessions SET last_seen_at=now(); RAISE EXCEPTION 'LOGIN_GATE runtime touched session';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $runtime$;
RESET ROLE;
ROLLBACK;
