-- Responsibility: exercise the deployed CHECKs for every caller-key storage column.
-- Dependencies: all prior forward migrations; caller: check_pg.sh on disposable PG18.
-- Invariant: neither sqlc, direct DML, nor an async writer can store contact text.
-- Isolate each column's real catalog CHECKs so FK/NOT NULL/trigger rejection cannot
-- masquerade as check_violation. No constraint implementation is copied into tests.
\set ON_ERROR_STOP on
BEGIN;
CREATE TEMP TABLE caller_key_failures(label text);
DO $caller_keys$
DECLARE target record; c record; bad text; rejected boolean;
 good text := 'abcdef01-2345-6789-abcd-ef0123456789';
BEGIN
 FOR target IN SELECT * FROM (VALUES
  ('idempotency_records','idempotency_key'),
  ('inventory_reservations','idempotency_key'),
  ('orders','creation_key'),
  ('shipments','creation_key'),
  ('returns','creation_key'),('returns','receive_key'),('returns','refund_key'),
  ('payment_return_refunds','attempt_key'),('payment_refund_attempts','attempt_key'),
  ('checkout_sessions','request_key'),('checkout_sessions','claim_key'),
  ('checkout_attempts','event_key'),
  ('notification_redrive_requests','idempotency_key'),
  ('quote_conversions','idempotency_key'),
  ('data_rights_erasures','scope')
 ) AS columns(table_name,column_name) LOOP
  EXECUTE format('CREATE TEMP TABLE caller_key_probe(%I text)',target.column_name);
  FOR c IN SELECT pg_get_constraintdef(con.oid) AS definition
   FROM pg_constraint con JOIN pg_attribute a ON a.attrelid=con.conrelid
    AND a.attname=target.column_name
   WHERE con.conrelid=format('app.%I',target.table_name)::regclass
    AND con.contype='c' AND con.convalidated AND con.conkey=ARRAY[a.attnum] LOOP
   EXECUTE 'ALTER TABLE caller_key_probe ADD '||c.definition;
  END LOOP;
  IF target.column_name='scope' THEN
   good := 'customer.profile,addresses,credentials,b2b,order_snapshots,exports';
  ELSE
   good := 'abcdef01-2345-6789-abcd-ef0123456789';
  END IF;
  EXECUTE format('INSERT INTO caller_key_probe(%I) VALUES($1)',target.column_name) USING good;
  FOREACH bad IN ARRAY ARRAY['alice@example.test',upper(good),'legacy-key',good||'.reserve'] LOOP
   rejected := false;
   BEGIN
    EXECUTE format('INSERT INTO caller_key_probe(%I) VALUES($1)',target.column_name) USING bad;
   EXCEPTION WHEN check_violation THEN rejected := true;
   END;
   IF NOT rejected THEN
    INSERT INTO caller_key_failures VALUES(target.table_name||'.'||target.column_name||' accepted '||bad);
   END IF;
  END LOOP;
  EXECUTE 'DROP TABLE caller_key_probe';
 END LOOP;
END $caller_keys$;
TABLE caller_key_failures;
DO $$ BEGIN
 IF EXISTS(SELECT FROM caller_key_failures) THEN
  RAISE EXCEPTION 'CALLER_KEY_CHECK_MISSING: contact text or noncanonical key reached storage';
 END IF;
END $$;
ROLLBACK;
