-- Responsibility: real-PG atomic ledger posting, final balance and immutable history gates.
-- Dependencies: all forward migrations including logical *_ledger.sql; trusted fixture
-- setup and true app_runtime calls. Caller: check_pg.sh and populated upgrade gate.
-- Invariant: exact refusals, permanent same-content replay, no unfinished journals,
-- single configured tenant currency, positive bounded integer units and append-only reversals.
\set ON_ERROR_STOP on
BEGIN;
INSERT INTO app.tenants(name,settlement_currency) VALUES('ledger-A','USD') RETURNING id AS ta \gset
INSERT INTO app.tenants(name,settlement_currency) VALUES('ledger-B','USD') RETURNING id AS tb \gset
INSERT INTO app.tenants(name) VALUES('ledger-unconfigured') RETURNING id AS tu \gset
INSERT INTO app.ledger_accounts(tenant_id,code,account_type,currency) VALUES(:'ta','cash','asset','USD') RETURNING id AS cash \gset
INSERT INTO app.ledger_accounts(tenant_id,code,account_type,currency) VALUES(:'ta','revenue','revenue','USD') RETURNING id AS revenue \gset
INSERT INTO app.ledger_accounts(tenant_id,code,account_type,currency) VALUES(:'tb','cash','asset','USD') RETURNING id AS foreign_cash \gset
SELECT set_config('ledger.ta',:'ta',true),set_config('ledger.tb',:'tb',true),set_config('ledger.tu',:'tu',true),
 set_config('ledger.cash',:'cash',true),set_config('ledger.revenue',:'revenue',true),set_config('ledger.foreign',:'foreign_cash',true);
CREATE FUNCTION pg_temp.ledger_reject(command text,wanted text,label text) RETURNS void
LANGUAGE plpgsql AS $$ BEGIN
 BEGIN EXECUTE command;
 EXCEPTION WHEN OTHERS THEN
  IF SQLSTATE=wanted THEN RETURN; END IF;
  RAISE EXCEPTION 'LEDGER_WRONG_REFUSAL %: expected %, got % (%)',label,wanted,SQLSTATE,SQLERRM;
 END;
 RAISE EXCEPTION 'LEDGER_EXPECTED_REFUSAL % (%)',label,wanted;
END $$;
-- Force deferred checks inside the same subtransaction as the malformed trusted
-- write: its refusal must roll back the journal and every entry atomically.
CREATE FUNCTION pg_temp.ledger_final_reject(command text,wanted text,label text) RETURNS void
LANGUAGE plpgsql AS $$ BEGIN
 PERFORM pg_temp.ledger_reject(command||'; SET CONSTRAINTS ALL IMMEDIATE',wanted,label);
 SET CONSTRAINTS ALL DEFERRED;
END $$;
SET LOCAL ROLE app_runtime;
DO $missing$ DECLARE t uuid:=current_setting('ledger.ta')::uuid; a uuid:=current_setting('ledger.cash')::uuid;
 b uuid:=current_setting('ledger.revenue')::uuid; tab text; n bigint; BEGIN
 FOREACH tab IN ARRAY ARRAY['ledger_accounts','ledger_transactions','ledger_entries'] LOOP
  EXECUTE format('SELECT count(*) FROM app.%I',tab) INTO n;
  IF n<>0 THEN RAISE EXCEPTION 'LEDGER_UNSET_CONTEXT %',tab; END IF;
 END LOOP;
 PERFORM pg_temp.ledger_reject(format('SELECT app.post_ledger(%L,''USD'',''order'',''missing'',''capture'',ARRAY[%L,%L]::uuid[],ARRAY[''debit'',''credit''],ARRAY[100,100]::bigint[])',t,a,b),'42501','missing_guc');
END $missing$;
SELECT set_config('app.tenant_id','',true);
DO $blank$ BEGIN
 PERFORM pg_temp.ledger_reject(format('SELECT app.post_ledger(%L,''USD'',''order'',''blank'',''capture'',ARRAY[%L,%L]::uuid[],ARRAY[''debit'',''credit''],ARRAY[100,100]::bigint[])',current_setting('ledger.ta')::uuid,current_setting('ledger.cash')::uuid,current_setting('ledger.revenue')::uuid),'42501','blank_guc');
END $blank$;
SELECT set_config('app.tenant_id',:'ta',true);
DO $runtime$ DECLARE
 t uuid:=current_setting('ledger.ta')::uuid; foreign_t uuid:=current_setting('ledger.tb')::uuid;
 a uuid:=current_setting('ledger.cash')::uuid; b uuid:=current_setting('ledger.revenue')::uuid;
 foreign_a uuid:=current_setting('ledger.foreign')::uuid; j uuid; replay uuid; reversal uuid; tab text; n bigint;
 base text:=format('SELECT app.post_ledger(%L,''USD'',''order'',''invalid'',''capture'',',t);
 pair text:=format('ARRAY[%L,%L]::uuid[],ARRAY[''debit'',''credit''],',a,b);
BEGIN
 -- Runtime creates accounts only in its own configured tenant currency.
 INSERT INTO app.ledger_accounts(tenant_id,code,account_type,currency) VALUES(t,'expense','expense','USD');
 PERFORM pg_temp.ledger_reject(format('INSERT INTO app.ledger_accounts(tenant_id,code,account_type,currency) VALUES(%L,''bad'',''asset'',''USD'')',foreign_t),'42501','cross_account_insert');
 PERFORM pg_temp.ledger_reject(format('INSERT INTO app.ledger_accounts(tenant_id,code,account_type,currency) VALUES(%L,''bad'',''asset'',''EUR'')',t),'23503','account_currency');
 FOREACH tab IN ARRAY ARRAY['ledger_accounts','ledger_transactions','ledger_entries'] LOOP
  EXECUTE format('SELECT count(*) FROM app.%I WHERE tenant_id=%L',tab,foreign_t) INTO n;
  IF n<>0 THEN RAISE EXCEPTION 'LEDGER_FOREIGN_READ %',tab; END IF;
  PERFORM pg_temp.ledger_reject(format('DELETE FROM app.%I WHERE tenant_id=%L',tab,t),'42501','runtime_delete_'||tab);
 END LOOP;
 PERFORM pg_temp.ledger_reject(base||pair||'ARRAY[100,99]::bigint[])','23514','imbalance');
 PERFORM pg_temp.ledger_reject(base||format('ARRAY[%L]::uuid[],ARRAY[''debit''],ARRAY[100]::bigint[])',a),'22023','single_entry');
 PERFORM pg_temp.ledger_reject(base||pair||'ARRAY[-1,-1]::bigint[])','22023','negative_amount');
 PERFORM pg_temp.ledger_reject(base||pair||'ARRAY[0,0]::bigint[])','22023','zero_amount');
 PERFORM pg_temp.ledger_reject(base||format('ARRAY[%L,%L]::uuid[],ARRAY[''debit'',''credit''],ARRAY[100,100]::bigint[])',a,foreign_a),'23503','foreign_account');
 PERFORM pg_temp.ledger_reject(format('SELECT app.post_ledger(%L,''EUR'',''order'',''wrong-currency'',''capture'',',t)||pair||'ARRAY[100,100]::bigint[])','23503','wrong_currency');
 PERFORM pg_temp.ledger_reject(format('SELECT app.post_ledger(%L,''USD'',''order'',''foreign-tenant'',''capture'',',foreign_t)||pair||'ARRAY[100,100]::bigint[])','42501','foreign_tenant');
 PERFORM pg_temp.ledger_reject(base||'NULL::uuid[],ARRAY[''debit'',''credit''],ARRAY[100,100]::bigint[])','22023','null_array');
 PERFORM pg_temp.ledger_reject(base||pair||'ARRAY[100]::bigint[])','22023','different_lengths');
 PERFORM pg_temp.ledger_reject(base||format('ARRAY[NULL,%L]::uuid[],ARRAY[''debit'',''credit''],ARRAY[100,100]::bigint[])',b),'22023','null_account');
 PERFORM pg_temp.ledger_reject(base||format('ARRAY[%L,%L]::uuid[],ARRAY[''debit'',NULL],ARRAY[100,100]::bigint[])',a,b),'22023','null_side');
 PERFORM pg_temp.ledger_reject(base||pair||'ARRAY[100,NULL]::bigint[])','22023','null_amount');
 PERFORM pg_temp.ledger_reject(base||format('ARRAY[%L,%L]::uuid[],ARRAY[''other'',''credit''],ARRAY[100,100]::bigint[])',a,b),'22023','invalid_side');
 PERFORM pg_temp.ledger_reject(base||format('array_fill(%L::uuid,ARRAY[1001]),array_fill(''debit''::text,ARRAY[1001]),array_fill(1::bigint,ARRAY[1001]))',a),'22023','bounded_lines');
 PERFORM pg_temp.ledger_reject(base||format('array_fill(%L::uuid,ARRAY[2],ARRAY[0]),ARRAY[''debit'',''credit''],ARRAY[100,100]::bigint[])',a),'22023','array_origin');
 PERFORM pg_temp.ledger_reject(base||format('ARRAY[[%L,%L],[%L,%L]]::uuid[],ARRAY[''debit'',''credit'',''debit'',''credit''],ARRAY[1,1,1,1]::bigint[])',a,b,a,b),'22023','array_dimensions');
 PERFORM pg_temp.ledger_reject(base||format('ARRAY[%L,%L,%L,%L]::uuid[],ARRAY[''debit'',''debit'',''credit'',''credit''],ARRAY[9223372036854775807,1,9223372036854775807,1]::bigint[])',a,a,b,b),'22003','aggregate_overflow');
 IF EXISTS(SELECT FROM app.ledger_transactions) OR EXISTS(SELECT FROM app.ledger_entries) THEN
  RAISE EXCEPTION 'LEDGER_FAILED_POST_LEFT_ROWS';
 END IF;
 j:=app.post_ledger(t,'USD','order','successful','capture',ARRAY[a,b],ARRAY['debit','credit'],ARRAY[100,100]::bigint[]);
 replay:=app.post_ledger(t,'USD','order','successful','capture',ARRAY[a,b],ARRAY['debit','credit'],ARRAY[100,100]::bigint[]);
 IF replay<>j OR (SELECT count(*) FROM app.ledger_transactions)<>1 OR (SELECT count(*) FROM app.ledger_entries)<>2 THEN
  RAISE EXCEPTION 'LEDGER_REPLAY_DUPLICATED';
 END IF;
 PERFORM pg_temp.ledger_reject(format('SELECT app.post_ledger(%L,''USD'',''order'',''successful'',''capture'',',t)||pair||'ARRAY[101,101]::bigint[])','23505','changed_content');
 PERFORM pg_temp.ledger_reject(format('SELECT app.post_ledger(%L,''USD'',''order'',''successful'',''capture'',ARRAY[%L,%L]::uuid[],ARRAY[''credit'',''debit''],ARRAY[100,100]::bigint[])',t,a,b),'23505','changed_sides');
 PERFORM pg_temp.ledger_reject(format('UPDATE app.ledger_transactions SET status=''posting'',posted_at=NULL WHERE id=%L',j),'42501','runtime_header_rewrite');
 PERFORM pg_temp.ledger_reject(format('INSERT INTO app.ledger_transactions(tenant_id,currency,source_type,source_id,event) VALUES(%L,''USD'',''direct'',''new'',''capture'')',t),'42501','runtime_direct_header');
 PERFORM pg_temp.ledger_reject(format('UPDATE app.ledger_entries SET amount=101 WHERE transaction_id=%L',j),'42501','runtime_entry_rewrite');
 PERFORM pg_temp.ledger_reject(format('INSERT INTO app.ledger_entries(tenant_id,transaction_id,line_no,account_id,currency,side,amount) VALUES(%L,%L,3,%L,''USD'',''debit'',100)',t,j,a),'42501','runtime_posted_append');
 reversal:=app.post_ledger(t,'USD','order','successful','reversal',ARRAY[a,b],ARRAY['credit','debit'],ARRAY[100,100]::bigint[]);
 IF reversal=j OR (SELECT count(*) FROM app.ledger_transactions)<>2 OR (SELECT count(*) FROM app.ledger_entries)<>4
 OR EXISTS(SELECT account_id FROM app.ledger_entries GROUP BY account_id HAVING sum(CASE side WHEN 'debit' THEN amount::numeric ELSE -amount::numeric END)<>0) THEN
  RAISE EXCEPTION 'LEDGER_REVERSAL_NOT_ZERO';
 END IF;
 -- Maximum accepted buffer is balanced and durable, while 1001 was refused.
 PERFORM app.post_ledger(t,'USD','buffer','maximum','capture',array_fill(a,ARRAY[1000]),
  (SELECT array_agg(CASE WHEN seq.n<=500 THEN 'debit' ELSE 'credit' END ORDER BY seq.n) FROM generate_series(1,1000) AS seq(n)),array_fill(1::bigint,ARRAY[1000]));
 IF (SELECT count(*) FROM app.ledger_entries)<>1004 THEN RAISE EXCEPTION 'LEDGER_MAX_BUFFER'; END IF;
 PERFORM set_config('ledger.journal',j::text,true);
END $runtime$;
-- Validate successful journals through the actual deferred trigger before any
-- trusted malformed fixture is introduced. A ROLLBACK alone would skip this.
SET CONSTRAINTS ALL IMMEDIATE;
SET CONSTRAINTS ALL DEFERRED;
RESET ROLE;
-- Trusted SQL is deliberately used here to bypass ACL and prove trigger defense,
-- not as a substitute for runtime tests above. Keep tenant GUC for SECDEF RLS.
DO $trusted$ DECLARE t uuid:=current_setting('ledger.ta')::uuid; a uuid:=current_setting('ledger.cash')::uuid;
 j uuid:=current_setting('ledger.journal')::uuid; u uuid:=current_setting('ledger.tu')::uuid;
 h text; BEGIN
 PERFORM pg_temp.ledger_reject(format('UPDATE app.tenants SET settlement_currency=''EUR'' WHERE id=%L',t),'23503','currency_frozen');
 PERFORM pg_temp.ledger_reject(format('INSERT INTO app.ledger_accounts(tenant_id,code,account_type,currency) VALUES(%L,''no-config'',''asset'',''USD'')',u),'23503','unconfigured_currency');
 PERFORM pg_temp.ledger_reject(format('UPDATE app.ledger_accounts SET account_type=''liability'' WHERE tenant_id=%L AND id=%L',t,a),'55000','trusted_account_rewrite');
 PERFORM pg_temp.ledger_reject(format('UPDATE app.ledger_transactions SET status=''posting'',posted_at=NULL WHERE tenant_id=%L AND id=%L',t,j),'55000','trusted_posted_rewrite');
 PERFORM pg_temp.ledger_reject(format('DELETE FROM app.ledger_transactions WHERE tenant_id=%L AND id=%L',t,j),'55000','trusted_posted_delete');
 PERFORM pg_temp.ledger_reject(format('UPDATE app.ledger_entries SET amount=101 WHERE tenant_id=%L AND transaction_id=%L',t,j),'55000','trusted_entry_rewrite');
 PERFORM pg_temp.ledger_reject(format('DELETE FROM app.ledger_entries WHERE tenant_id=%L AND transaction_id=%L',t,j),'55000','trusted_entry_delete');
 PERFORM pg_temp.ledger_reject(format('INSERT INTO app.ledger_entries(tenant_id,transaction_id,line_no,account_id,currency,side,amount) VALUES(%L,%L,3,%L,''USD'',''debit'',100)',t,j,a),'55000','trusted_posted_append');
 -- An orphan posting header cannot survive the final transaction checks.
 h:=format('INSERT INTO app.ledger_transactions(tenant_id,currency,source_type,source_id,event) VALUES(%L,''USD'',''trusted'',''orphan'',''capture'')',t);
 PERFORM pg_temp.ledger_final_reject(h,'23514','orphan_posting');
 h:=format('INSERT INTO app.ledger_transactions(tenant_id,currency,source_type,source_id,event,status,posted_at) VALUES(%L,''USD'',''trusted'',''empty-posted'',''capture'',''posted'',clock_timestamp())',t);
 PERFORM pg_temp.ledger_final_reject(h,'23514','empty_posted');
 IF EXISTS(SELECT FROM app.ledger_transactions WHERE source_type='trusted') THEN RAISE EXCEPTION 'LEDGER_ORPHAN_LEFT_ROWS'; END IF;
 -- A trusted transient draft may exist only while this transaction is open;
 -- identity/parent rewrites are refused before deferred commit checks execute.
 INSERT INTO app.ledger_transactions(tenant_id,currency,source_type,source_id,event)
 VALUES(t,'USD','trusted','draft','capture') RETURNING id INTO j;
 INSERT INTO app.ledger_entries(tenant_id,transaction_id,line_no,account_id,currency,side,amount)
 VALUES(t,j,1,a,'USD','debit',1);
 PERFORM pg_temp.ledger_reject(format('INSERT INTO app.ledger_entries(tenant_id,transaction_id,line_no,account_id,currency,side,amount) VALUES(%L,%L,2,%L,''USD'',''credit'',-1)',t,j,a),'23514','trusted_negative_entry');
 PERFORM pg_temp.ledger_reject(format('UPDATE app.ledger_transactions SET source_id=''rewritten'' WHERE tenant_id=%L AND id=%L',t,j),'55000','trusted_draft_identity');
 PERFORM pg_temp.ledger_reject(format('UPDATE app.ledger_entries SET transaction_id=%L WHERE tenant_id=%L AND transaction_id=%L',current_setting('ledger.journal')::uuid,t,j),'55000','trusted_parent_move');
 PERFORM pg_temp.ledger_reject(format('UPDATE app.ledger_entries SET amount=2 WHERE tenant_id=%L AND transaction_id=%L',t,j),'55000','trusted_draft_entry');
 -- Final balance must reject trusted posted state with one or unequal entries.
 PERFORM pg_temp.ledger_final_reject(format('UPDATE app.ledger_transactions SET status=''posted'',posted_at=clock_timestamp() WHERE tenant_id=%L AND id=%L',t,j),'23514','trusted_single_entry_post');
 INSERT INTO app.ledger_entries(tenant_id,transaction_id,line_no,account_id,currency,side,amount)
 VALUES(t,j,2,a,'USD','credit',2);
 PERFORM pg_temp.ledger_final_reject(format('UPDATE app.ledger_transactions SET status=''posted'',posted_at=clock_timestamp() WHERE tenant_id=%L AND id=%L',t,j),'23514','trusted_unbalanced_post');
 -- This draft is test-only and intentionally rolled back with the outer fixture.
END $trusted$;
ROLLBACK;
SELECT 'PASS: ledger runtime atomic posting, replay, reversal, input bounds, scope, deferred balance and immutable history';
