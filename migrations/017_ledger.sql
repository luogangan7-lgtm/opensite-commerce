-- Responsibility: single-currency, append-only double-entry ledger and atomic posting.
-- Dependencies: app.tenants and hardened runtime roles; called by forward migrator.
-- Invariants: tenant/currency composite keys, permanent source identity, immutable
-- posted journals, positive bigint minor units, final balanced nonempty journals.
-- Existing tenants remain unconfigured; provisioning explicitly chooses currency.
-- Rollback: forward repair; never delete or rewrite posted financial history.
ALTER TABLE app.tenants ADD COLUMN settlement_currency char(3),
 ADD CONSTRAINT tenant_settlement_currency CHECK (settlement_currency IS NULL OR settlement_currency::text ~ '^[A-Z]{3}$'),
 ADD CONSTRAINT tenant_settlement_identity UNIQUE(id,settlement_currency);

CREATE TABLE app.ledger_accounts (
 tenant_id uuid NOT NULL,
 id uuid NOT NULL DEFAULT uuidv7(),
 currency char(3) NOT NULL CHECK (currency::text ~ '^[A-Z]{3}$'),
 code text NOT NULL CHECK (length(code) BETWEEN 1 AND 128 AND btrim(code)<>''),
 account_type text NOT NULL CHECK (account_type IN ('asset','liability','equity','revenue','expense')),
 status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,id), UNIQUE(tenant_id,id,currency), UNIQUE(tenant_id,code),
 FOREIGN KEY(tenant_id,currency) REFERENCES app.tenants(id,settlement_currency)
);
CREATE TABLE app.ledger_transactions (
 tenant_id uuid NOT NULL,
 id uuid NOT NULL DEFAULT uuidv7(),
 currency char(3) NOT NULL CHECK (currency::text ~ '^[A-Z]{3}$'),
 source_type text NOT NULL CHECK (length(source_type) BETWEEN 1 AND 128 AND btrim(source_type)<>''),
 source_id text NOT NULL CHECK (length(source_id) BETWEEN 1 AND 256 AND btrim(source_id)<>''),
 event text NOT NULL CHECK (length(event) BETWEEN 1 AND 128 AND btrim(event)<>''),
 status text NOT NULL DEFAULT 'posting' CHECK (status IN ('posting','posted')),
 revision bigint NOT NULL DEFAULT 0 CHECK (revision>=0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 posted_at timestamptz,
 PRIMARY KEY(tenant_id,id), UNIQUE(tenant_id,id,currency),
 CONSTRAINT ledger_source_event UNIQUE(tenant_id,source_type,source_id,event),
 FOREIGN KEY(tenant_id,currency) REFERENCES app.tenants(id,settlement_currency),
 CHECK ((status='posted')=(posted_at IS NOT NULL))
);
CREATE TABLE app.ledger_entries (
 tenant_id uuid NOT NULL,
 transaction_id uuid NOT NULL,
 line_no integer NOT NULL CHECK (line_no BETWEEN 1 AND 1000),
 account_id uuid NOT NULL,
 currency char(3) NOT NULL,
 side text NOT NULL CHECK (side IN ('debit','credit')),
 amount bigint NOT NULL CHECK (amount>0),
 PRIMARY KEY(tenant_id,transaction_id,line_no),
 FOREIGN KEY(tenant_id,transaction_id,currency) REFERENCES app.ledger_transactions(tenant_id,id,currency),
 FOREIGN KEY(tenant_id,account_id,currency) REFERENCES app.ledger_accounts(tenant_id,id,currency)
);
CREATE INDEX ledger_entries_account ON app.ledger_entries(tenant_id,account_id,transaction_id);

DO $$ BEGIN
 IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='app_ledger_executor') THEN CREATE ROLE app_ledger_executor; END IF;
 IF EXISTS(SELECT FROM pg_auth_members WHERE member='app_ledger_executor'::regrole OR roleid='app_ledger_executor'::regrole) THEN
  RAISE EXCEPTION 'LEDGER_ROLE_MEMBERSHIP_UNSAFE';
 END IF;
END $$;
ALTER ROLE app_ledger_executor NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT;
GRANT USAGE ON SCHEMA app TO app_ledger_executor;
REVOKE ALL ON app.ledger_accounts,app.ledger_transactions,app.ledger_entries FROM PUBLIC,app_runtime,app_ledger_executor;
GRANT SELECT ON app.ledger_accounts,app.ledger_transactions,app.ledger_entries TO app_runtime,app_ledger_executor;
GRANT INSERT(tenant_id,id,code,currency,account_type) ON app.ledger_accounts TO app_runtime;
GRANT INSERT(tenant_id,id,currency,source_type,source_id,event) ON app.ledger_transactions TO app_ledger_executor;
GRANT INSERT(tenant_id,transaction_id,line_no,account_id,currency,side,amount) ON app.ledger_entries TO app_ledger_executor;
GRANT UPDATE(status,posted_at,revision) ON app.ledger_transactions TO app_ledger_executor;
DO $rls$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['ledger_accounts','ledger_transactions','ledger_entries'] LOOP
  EXECUTE format('ALTER TABLE app.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE app.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_scope ON app.%I TO app_runtime,app_ledger_executor USING(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',t);
 END LOOP;
END $rls$;

CREATE FUNCTION app.guard_ledger_account() RETURNS trigger LANGUAGE plpgsql
SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 RAISE EXCEPTION 'LEDGER_ACCOUNT_IMMUTABLE' USING ERRCODE='55000';
END $$;
CREATE TRIGGER ledger_account_immutable BEFORE UPDATE OR DELETE ON app.ledger_accounts
 FOR EACH ROW EXECUTE FUNCTION app.guard_ledger_account();
CREATE FUNCTION app.guard_ledger_transaction() RETURNS trigger LANGUAGE plpgsql
SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 IF TG_OP='DELETE' OR OLD.status='posted' THEN
  RAISE EXCEPTION 'LEDGER_TRANSACTION_IMMUTABLE' USING ERRCODE='55000';
 END IF;
 IF ROW(NEW.tenant_id,NEW.id,NEW.currency,NEW.source_type,NEW.source_id,NEW.event,NEW.created_at)
 IS DISTINCT FROM ROW(OLD.tenant_id,OLD.id,OLD.currency,OLD.source_type,OLD.source_id,OLD.event,OLD.created_at)
 OR NOT ((NEW.status='posting' AND NEW.posted_at IS NULL AND NEW.revision=OLD.revision+1)
      OR (NEW.status='posted' AND NEW.posted_at IS NOT NULL AND NEW.revision=OLD.revision)) THEN
  RAISE EXCEPTION 'LEDGER_TRANSACTION_TRANSITION' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER ledger_transaction_immutable BEFORE UPDATE OR DELETE ON app.ledger_transactions
 FOR EACH ROW EXECUTE FUNCTION app.guard_ledger_transaction();
CREATE FUNCTION app.guard_ledger_entry() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 -- No editable draft API: UPDATE/DELETE, including parent moves, always fail.
 IF TG_OP<>'INSERT' THEN
  RAISE EXCEPTION 'LEDGER_ENTRY_IMMUTABLE' USING ERRCODE='55000';
 END IF;
 -- A real parent write both serializes with finalization and invalidates stale
 -- repeatable-read snapshots. The header guard refuses writes after posting.
 UPDATE app.ledger_transactions SET revision=revision+1
  WHERE tenant_id=NEW.tenant_id AND id=NEW.transaction_id AND status='posting';
 IF NOT FOUND THEN RAISE EXCEPTION 'LEDGER_ENTRY_PARENT_POSTED_OR_MISSING' USING ERRCODE='55000'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER ledger_entry_immutable BEFORE INSERT OR UPDATE OR DELETE ON app.ledger_entries
 FOR EACH ROW EXECUTE FUNCTION app.guard_ledger_entry();

CREATE FUNCTION app.assert_ledger_balance() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE j app.ledger_transactions; n bigint; debits bigint; credits bigint;
BEGIN
 SELECT * INTO j FROM app.ledger_transactions
 WHERE tenant_id=NEW.tenant_id AND id=NEW.id FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'LEDGER_FINAL_JOURNAL_MISSING' USING ERRCODE='23514'; END IF;
 SELECT count(*),coalesce(sum(amount) FILTER(WHERE side='debit'),0)::bigint,
  coalesce(sum(amount) FILTER(WHERE side='credit'),0)::bigint INTO n,debits,credits
 FROM app.ledger_entries WHERE tenant_id=j.tenant_id AND transaction_id=j.id;
 IF j.status<>'posted' OR n<2 OR debits<=0 OR debits<>credits THEN
  RAISE EXCEPTION 'LEDGER_FINAL_BALANCE' USING ERRCODE='23514';
 END IF;
 RETURN NULL;
END $$;
-- The header INSERT queues a final check for all subsequent entry INSERTs;
-- finalization queues a second check. Revision writes do not repeat the sum.
CREATE CONSTRAINT TRIGGER ledger_final_balance AFTER INSERT OR UPDATE OF status ON app.ledger_transactions
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION app.assert_ledger_balance();

CREATE FUNCTION app.post_ledger(p_tenant uuid,p_currency text,p_source_type text,p_source_id text,p_event text,
 p_accounts uuid[],p_sides text[],p_amounts bigint[]) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE result_id uuid; j app.ledger_transactions; i integer; debits bigint:=0; credits bigint:=0;
 old_accounts uuid[]; old_sides text[]; old_amounts bigint[];
BEGIN
 IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN
  RAISE EXCEPTION 'LEDGER_TENANT_SCOPE' USING ERRCODE='42501';
 END IF;
 IF p_currency IS NULL OR p_currency !~ '^[A-Z]{3}$'
 OR p_source_type IS NULL OR length(p_source_type) NOT BETWEEN 1 AND 128 OR btrim(p_source_type)=''
 OR p_source_id IS NULL OR length(p_source_id) NOT BETWEEN 1 AND 256 OR btrim(p_source_id)=''
 OR p_event IS NULL OR length(p_event) NOT BETWEEN 1 AND 128 OR btrim(p_event)=''
 OR p_accounts IS NULL OR p_sides IS NULL OR p_amounts IS NULL
 OR cardinality(p_accounts) NOT BETWEEN 2 AND 1000
 OR cardinality(p_accounts)<>cardinality(p_sides) OR cardinality(p_accounts)<>cardinality(p_amounts)
 OR array_ndims(p_accounts)<>1 OR array_ndims(p_sides)<>1 OR array_ndims(p_amounts)<>1
 OR array_lower(p_accounts,1)<>1 OR array_lower(p_sides,1)<>1 OR array_lower(p_amounts,1)<>1 THEN
  RAISE EXCEPTION 'LEDGER_ARGUMENT' USING ERRCODE='22023';
 END IF;
 FOR i IN 1..cardinality(p_accounts) LOOP
  IF p_accounts[i] IS NULL OR p_sides[i] IS NULL OR p_sides[i] NOT IN ('debit','credit')
   OR p_amounts[i] IS NULL OR p_amounts[i]<=0 THEN RAISE EXCEPTION 'LEDGER_ENTRY_ARGUMENT' USING ERRCODE='22023'; END IF;
 END LOOP;
 INSERT INTO app.ledger_transactions(tenant_id,currency,source_type,source_id,event)
 VALUES(p_tenant,p_currency,p_source_type,p_source_id,p_event)
 ON CONFLICT ON CONSTRAINT ledger_source_event DO NOTHING RETURNING id INTO result_id;
 IF result_id IS NULL THEN
  -- ON CONFLICT waits for competing inserters. At READ COMMITTED this next
  -- statement sees the winner; older snapshots must retry instead of fabricating.
  SELECT * INTO j FROM app.ledger_transactions
   WHERE tenant_id=p_tenant AND source_type=p_source_type AND source_id=p_source_id AND event=p_event;
  IF NOT FOUND THEN RAISE EXCEPTION 'LEDGER_RETRY_SNAPSHOT' USING ERRCODE='40001'; END IF;
  SELECT array_agg(account_id ORDER BY line_no),array_agg(side ORDER BY line_no),array_agg(amount ORDER BY line_no)
   INTO old_accounts,old_sides,old_amounts FROM app.ledger_entries WHERE tenant_id=p_tenant AND transaction_id=j.id;
  IF j.status<>'posted' OR j.currency::text IS DISTINCT FROM p_currency
   OR old_accounts IS DISTINCT FROM p_accounts OR old_sides IS DISTINCT FROM p_sides OR old_amounts IS DISTINCT FROM p_amounts THEN
   RAISE EXCEPTION 'LEDGER_IDEMPOTENCY_CONTENT' USING ERRCODE='23505',CONSTRAINT='ledger_idempotency_content';
  END IF;
  RETURN j.id;
 END IF;
 FOR i IN 1..cardinality(p_accounts) LOOP
  IF p_sides[i]='debit' THEN debits:=debits+p_amounts[i]; ELSE credits:=credits+p_amounts[i]; END IF;
 END LOOP;
 IF debits=0 OR debits<>credits THEN RAISE EXCEPTION 'LEDGER_BALANCE' USING ERRCODE='23514'; END IF;
 IF EXISTS(SELECT FROM unnest(p_accounts) a(id) LEFT JOIN app.ledger_accounts l
  ON l.tenant_id=p_tenant AND l.id=a.id AND l.currency::text=p_currency AND l.status='active' WHERE l.id IS NULL) THEN
  RAISE EXCEPTION 'LEDGER_ACCOUNT_SCOPE_CURRENCY_OR_STATUS' USING ERRCODE='23503';
 END IF;
 INSERT INTO app.ledger_entries(tenant_id,transaction_id,line_no,account_id,currency,side,amount)
 SELECT p_tenant,result_id,s.i,p_accounts[s.i],p_currency,p_sides[s.i],p_amounts[s.i] FROM generate_series(1,cardinality(p_accounts)) AS s(i);
 UPDATE app.ledger_transactions SET status='posted',posted_at=clock_timestamp()
 WHERE tenant_id=p_tenant AND id=result_id;
 RETURN result_id;
END $$;

ALTER FUNCTION app.guard_ledger_account() OWNER TO app_ledger_executor;
ALTER FUNCTION app.guard_ledger_transaction() OWNER TO app_ledger_executor;
ALTER FUNCTION app.guard_ledger_entry() OWNER TO app_ledger_executor;
ALTER FUNCTION app.assert_ledger_balance() OWNER TO app_ledger_executor;
ALTER FUNCTION app.post_ledger(uuid,text,text,text,text,uuid[],text[],bigint[]) OWNER TO app_ledger_executor;
REVOKE ALL ON FUNCTION app.guard_ledger_account(),app.guard_ledger_transaction(),app.guard_ledger_entry(),app.assert_ledger_balance(),
 app.post_ledger(uuid,text,text,text,text,uuid[],text[],bigint[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.post_ledger(uuid,text,text,text,text,uuid[],text[],bigint[]) TO app_runtime;
