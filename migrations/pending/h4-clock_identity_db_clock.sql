-- stable-id: h4-clock
-- Responsibility: post-lock DB timestamps for staff failure/device writes.
-- Dependencies: identity auth/login; caller: private store; preserve owner/ACL.
-- Invariant: a queued write receives its full DB-clock backoff after release.
CREATE OR REPLACE FUNCTION app.auth_record_failure(p_identifier text,p_ip inet)
RETURNS app.auth_account_attempts LANGUAGE plpgsql SECURITY INVOKER
SET search_path=pg_catalog,app,pg_temp AS $failure$
DECLARE k text:=lower(btrim(p_identifier)); t timestamptz; a app.auth_account_attempts; i app.auth_login_attempts;
BEGIN
 IF k IS NULL OR octet_length(k) NOT BETWEEN 1 AND 320 OR p_ip IS NULL OR
  masklen(p_ip)<>(CASE family(p_ip) WHEN 4 THEN 32 ELSE 128 END) THEN
  RAISE EXCEPTION 'AUTH_FAILURE_INVALID_INPUT' USING ERRCODE='22023'; END IF;
 IF current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'AUTH_REQUIRES_READ_COMMITTED' USING ERRCODE='25000'; END IF;
 PERFORM pg_advisory_xact_lock(hashtextextended('opensite:auth:'||k,0));
 -- A transaction-start timestamp would consume the backoff while queued.
 t:=clock_timestamp();
 INSERT INTO app.auth_account_attempts AS old(identifier_normalized,failure_count,window_started_at,cooldown_until,last_failed_at)
 VALUES(k,1,t,t+interval '1 second',t)
 ON CONFLICT(identifier_normalized) DO UPDATE SET
  failure_count=CASE WHEN t>=old.window_started_at+interval '15 minutes' THEN 1 ELSE least(1000000,old.failure_count+1) END,
  window_started_at=CASE WHEN t>=old.window_started_at+interval '15 minutes' THEN t ELSE old.window_started_at END,
  cooldown_until=CASE WHEN old.cooldown_until>t THEN old.cooldown_until ELSE t+
   least(60,1 << least(6,CASE WHEN t>=old.window_started_at+interval '15 minutes' THEN 0 ELSE old.failure_count END))*interval '1 second' END,
  last_failed_at=greatest(old.last_failed_at,t)
 RETURNING * INTO a;
 INSERT INTO app.auth_login_attempts AS old(identifier_normalized,ip,failure_count,window_started_at,cooldown_until,last_failed_at)
 VALUES(k,p_ip,1,t,t,t)
 ON CONFLICT(identifier_normalized,ip) DO UPDATE SET
  failure_count=CASE WHEN t>=old.window_started_at+interval '15 minutes' THEN 1 ELSE least(1000000,old.failure_count+1) END,
  window_started_at=CASE WHEN t>=old.window_started_at+interval '15 minutes' THEN t ELSE old.window_started_at END,
  cooldown_until=CASE WHEN old.cooldown_until>t THEN old.cooldown_until
   WHEN t>=old.window_started_at+interval '15 minutes' OR old.failure_count<4 THEN t ELSE t+interval '60 seconds' END,
  last_failed_at=greatest(old.last_failed_at,t)
 RETURNING * INTO i;
 RETURN a;
END $failure$;

CREATE OR REPLACE FUNCTION app.auth_fail_known_device(p_id uuid)
RETURNS app.known_devices LANGUAGE plpgsql SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $device$
DECLARE d app.known_devices; t timestamptz;
BEGIN
 SELECT * INTO d FROM app.known_devices WHERE id=p_id FOR UPDATE;
 t:=clock_timestamp();
 IF NOT FOUND OR d.revoked_at IS NOT NULL OR d.expires_at<=t THEN RETURN NULL; END IF;
 UPDATE app.known_devices SET failure_count=least(5,failure_count+1),
  revoked_at=CASE WHEN failure_count>=4 THEN t ELSE revoked_at END WHERE id=p_id RETURNING * INTO d;
 RETURN d;
END $device$;
