-- Responsibility: executable quote state, authority, immutability and tenant gates.
-- Dependencies: forward quotes migration and isolated PostgreSQL18 fixture data.
-- Caller: privileged check_pg runner; mutations use the real app_runtime commands.
-- Invariants: no self approval, expiry/CAS refusal, immutable snapshots and atomic outbox.
\set ON_ERROR_STOP on
DO $schema$ BEGIN
 IF to_regclass('app.quotes') IS NULL OR to_regclass('app.quote_lines') IS NULL
 OR to_regclass('app.quote_approvals') IS NULL THEN RAISE EXCEPTION 'QUOTES_SCHEMA_REQUIRED'; END IF;
END $schema$;
BEGIN;
INSERT INTO app.tenants(name) VALUES('quotes-A') RETURNING id AS ta \gset
INSERT INTO app.tenants(name) VALUES('quotes-B') RETURNING id AS tb \gset
INSERT INTO app.organizations(tenant_id,display_name) VALUES(:'ta','quotes') RETURNING id AS organization \gset
INSERT INTO app.sites(tenant_id,organization_id,name) VALUES(:'ta',:'organization','quotes') RETURNING id AS site \gset
INSERT INTO app.users(email,password_hash) VALUES(uuidv7()::text||'@example.invalid','fixture') RETURNING id AS staff \gset
INSERT INTO app.tenant_memberships(tenant_id,user_id,role) VALUES(:'ta',:'staff','admin');
INSERT INTO app.customers(tenant_id,email) VALUES(:'ta',uuidv7()::text||'@example.invalid') RETURNING id AS buyer \gset
INSERT INTO app.customers(tenant_id,email) VALUES(:'ta',uuidv7()::text||'@example.invalid') RETURNING id AS approver \gset
INSERT INTO app.customers(tenant_id,email) VALUES(:'ta',uuidv7()::text||'@example.invalid') RETURNING id AS outsider \gset
INSERT INTO app.buyer_companies(tenant_id,name) VALUES(:'ta','buyer-one') RETURNING id AS company \gset
INSERT INTO app.buyer_companies(tenant_id,name) VALUES(:'ta','buyer-two') RETURNING id AS company2 \gset
INSERT INTO app.buyer_companies(tenant_id,name) VALUES(:'tb','foreign') RETURNING id AS foreign_company \gset
INSERT INTO app.buyer_company_members(tenant_id,buyer_company_id,customer_id,role_code)
VALUES(:'ta',:'company',:'buyer','approver'),(:'ta',:'company',:'approver','approver'),(:'ta',:'company2',:'outsider','approver');
INSERT INTO app.inquiries(tenant_id,site_id,buyer_email,message) VALUES(:'ta',:'site','contact@example.invalid','contact') RETURNING id AS inquiry \gset
SELECT set_config('quotes.t',:'ta',true),set_config('quotes.foreign_t',:'tb',true),set_config('quotes.site',:'site',true),
 set_config('quotes.staff',:'staff',true),set_config('quotes.buyer',:'buyer',true),set_config('quotes.approver',:'approver',true),
 set_config('quotes.outsider',:'outsider',true),set_config('quotes.company',:'company',true),set_config('quotes.foreign_company',:'foreign_company',true),set_config('quotes.inquiry',:'inquiry',true);
CREATE FUNCTION pg_temp.quote_reject(command text,wanted text,label text) RETURNS void LANGUAGE plpgsql AS $$ BEGIN
 BEGIN EXECUTE command;
 EXCEPTION WHEN OTHERS THEN
  IF SQLSTATE=wanted THEN RETURN; END IF;
  RAISE EXCEPTION 'QUOTES_WRONG_REFUSAL % expected % got % (%)',label,wanted,SQLSTATE,SQLERRM;
 END;
 RAISE EXCEPTION 'QUOTES_EXPECTED_REFUSAL % (%)',label,wanted;
END $$;
CREATE FUNCTION pg_temp.quote_create(company uuid DEFAULT NULL,expires timestamptz DEFAULT clock_timestamp()+interval '1 hour',subtotal bigint DEFAULT 100) RETURNS uuid
LANGUAGE sql AS $$ SELECT app.create_quote(current_setting('quotes.t')::uuid,current_setting('quotes.site')::uuid,current_setting('quotes.inquiry')::uuid,
 coalesce(company,current_setting('quotes.company')::uuid),current_setting('quotes.staff')::uuid,'USD',subtotal,10,5,subtotal+15,
 '{"tax":{"rate":1000},"shipping":{"method":"fixed"},"totals":{"net":100,"tax":10,"shipping":5,"total":115}}',expires,
 '[{"line_no":1,"description":"negotiated","quantity":2,"unit_price":50,"line_total":100,"snapshot":{"amounts":{"net":100,"tax":10,"gross":110}}}]'::jsonb); $$;
CREATE FUNCTION pg_temp.quote_create_padded(pad integer) RETURNS uuid
LANGUAGE sql AS $$ SELECT app.create_quote(current_setting('quotes.t')::uuid,current_setting('quotes.site')::uuid,current_setting('quotes.inquiry')::uuid,
 current_setting('quotes.company')::uuid,current_setting('quotes.staff')::uuid,'USD',100,10,5,115,jsonb_build_object('pad',repeat('x',pad)),clock_timestamp()+interval '1 hour',
 '[{"line_no":1,"description":"negotiated","quantity":2,"unit_price":50,"line_total":100,"snapshot":{"amounts":{"net":100,"tax":10,"gross":110}}}]'::jsonb); $$;
SET LOCAL ROLE app_runtime;
DO $missing$ BEGIN
 IF EXISTS(SELECT FROM app.quotes) OR EXISTS(SELECT FROM app.quote_lines) OR EXISTS(SELECT FROM app.quote_approvals) THEN RAISE EXCEPTION 'QUOTES_UNSET_SCOPE'; END IF;
 PERFORM pg_temp.quote_reject('SELECT pg_temp.quote_create()','42501','missing tenant');
END $missing$;
SELECT set_config('app.tenant_id','',true);
SELECT pg_temp.quote_reject('SELECT pg_temp.quote_create()','42501','blank tenant');
SELECT set_config('app.tenant_id',:'ta',true);
-- Public inquiry requires no buyer account/company, while tenant/site remain FK-bound.
INSERT INTO app.inquiries(tenant_id,site_id,buyer_email,buyer_name,message)
VALUES(:'ta',:'site','public@example.invalid','public','request') RETURNING id;
DO $runtime$ DECLARE t uuid:=current_setting('quotes.t')::uuid; q uuid; draft uuid; withdrawn uuid; rejected uuid; v bigint;
 staff uuid:=current_setting('quotes.staff')::uuid; buyer uuid:=current_setting('quotes.buyer')::uuid;
 approver uuid:=current_setting('quotes.approver')::uuid; outsider uuid:=current_setting('quotes.outsider')::uuid; BEGIN
 PERFORM pg_temp.quote_reject(format('SELECT pg_temp.quote_create(%L)',current_setting('quotes.foreign_company')),'42501','cross tenant company');
 PERFORM pg_temp.quote_reject('SELECT pg_temp.quote_create(NULL,clock_timestamp()-interval ''1 second'')','22023','expired creation');
 PERFORM pg_temp.quote_reject('SELECT pg_temp.quote_create(NULL,clock_timestamp()+interval ''1 hour'',99)','23514','line subtotal mismatch');
 IF EXISTS(SELECT FROM app.quotes) OR EXISTS(SELECT FROM app.outbox_events WHERE event_type LIKE 'quotes.%') THEN RAISE EXCEPTION 'QUOTES_FAILED_CREATE_LEFT_ROWS'; END IF;
 q:=pg_temp.quote_create(); PERFORM set_config('quotes.q',q::text,true);
 IF NOT app.quote_actor_access(t,current_setting('quotes.company')::uuid,'customer',buyer,'read')
 OR app.quote_actor_access(t,current_setting('quotes.company')::uuid,'customer',outsider,'read') THEN RAISE EXCEPTION 'QUOTES_COMPANY_READ'; END IF;
 IF EXISTS(SELECT FROM app.quotes WHERE id=q AND app.quote_actor_access(tenant_id,buyer_company_id,'customer',outsider,'read')) THEN RAISE EXCEPTION 'QUOTES_COMPANY_READ_LEAK'; END IF;
 PERFORM pg_temp.quote_reject(format('UPDATE app.quotes SET status=''approved'' WHERE id=%L',q),'42501','direct status write');
 PERFORM pg_temp.quote_reject(format('INSERT INTO app.quote_lines(tenant_id,quote_id,line_no,description,quantity,unit_price,line_total,snapshot) VALUES(%L,%L,2,''extra'',1,1,1,''{}'')',t,q),'42501','append line');
 -- Draft terms are seller-internal: buyers can neither read nor act on them.
 PERFORM pg_temp.quote_reject(format('SELECT app.transition_quote(%L,%L,1,''approve'',''customer'',%L)',t,q,approver),'P0002','draft approve hidden');
 PERFORM pg_temp.quote_reject(format('SELECT app.transition_quote(%L,%L,1,''accept'',''customer'',%L)',t,q,buyer),'P0002','draft accept hidden');
 v:=app.transition_quote(t,q,1,'send','staff',staff);
 PERFORM pg_temp.quote_reject(format('SELECT app.transition_quote(%L,%L,1,''accept'',''customer'',%L)',t,q,buyer),'P0003','stale version');
 PERFORM pg_temp.quote_reject(format('SELECT app.transition_quote(%L,%L,2,''accept'',''customer'',%L)',t,q,outsider),'P0002','other company accept');
 PERFORM pg_temp.quote_reject(format('SELECT app.transition_quote(%L,%L,2,''approve'',''customer'',%L)',t,q,approver),'23514','sent approve');
 v:=app.transition_quote(t,q,2,'accept','customer',buyer);
 PERFORM pg_temp.quote_reject(format('SELECT app.transition_quote(%L,%L,3,''approve'',''customer'',%L)',t,q,buyer),'42501','self approval');
 v:=app.transition_quote(t,q,3,'approve','customer',approver);
 IF v<>4 OR (SELECT status FROM app.quotes WHERE id=q)<>'approved'
 OR (SELECT count(*) FROM app.quote_approvals WHERE quote_id=q AND requested_by=buyer AND decided_by=approver AND decision='approved')<>1
 OR (SELECT count(*) FROM app.outbox_events WHERE payload->>'quote_id'=q::text)<>4 THEN RAISE EXCEPTION 'QUOTES_APPROVAL_OUTBOX_ATOMIC'; END IF;
 PERFORM pg_temp.quote_reject(format('SELECT app.transition_quote(%L,%L,4,''withdraw'',''staff'',%L)',t,q,staff),'23514','terminal transition');
 withdrawn:=pg_temp.quote_create(); PERFORM app.transition_quote(t,withdrawn,1,'send','staff',staff); PERFORM app.transition_quote(t,withdrawn,2,'withdraw','staff',staff);
 rejected:=pg_temp.quote_create(); PERFORM app.transition_quote(t,rejected,1,'send','staff',staff); PERFORM app.transition_quote(t,rejected,2,'accept','customer',buyer); PERFORM app.transition_quote(t,rejected,3,'reject','customer',approver);
 draft:=pg_temp.quote_create();
 -- A 100-line quote with maximal descriptions needs far more than 64 KiB; 1 MiB stays a hard cap.
 PERFORM pg_temp.quote_create_padded(800000);
 PERFORM pg_temp.quote_reject('SELECT pg_temp.quote_create_padded(1048577)','23514','snapshot hard cap');
 IF (SELECT count(*) FROM app.quotes WHERE status='approved')<>1 THEN RAISE EXCEPTION 'QUOTES_ACCEPTED_PORT_ONLY_APPROVED'; END IF;
 -- Shared receipts preserve claim fencing and return a response in this transaction.
 PERFORM app.idempotency_claim(t,'customer',buyer,'quotes.accept','same',sha256('same'::bytea),interval '1 hour',q);
 PERFORM pg_temp.quote_reject(format('UPDATE app.idempotency_records SET state=''completed'',response_status=200,response_body=''{}'',completed_at=clock_timestamp() WHERE tenant_id=%L',t),'55000','receipt fence');
 PERFORM set_config('app.idempotency_claim',q::text,true);
 PERFORM pg_temp.quote_reject(format('UPDATE app.idempotency_records SET state=''completed'',response_status=200,response_body=''[]'',completed_at=clock_timestamp() WHERE tenant_id=%L',t),'23514','receipt shape');
 PERFORM pg_temp.quote_reject(format('UPDATE app.idempotency_records SET state=''completed'',response_status=200,response_body=jsonb_build_object(''x'',repeat(''x'',1048577)),completed_at=clock_timestamp() WHERE tenant_id=%L',t),'23514','receipt size');
 UPDATE app.idempotency_records SET state='completed',response_status=200,response_body=jsonb_build_object('id',q,'status','approved'),completed_at=clock_timestamp() WHERE tenant_id=t;
 IF (app.idempotency_claim(t,'customer',buyer,'quotes.accept','same',sha256('same'::bytea),interval '1 hour',uuidv7())).response_body->>'id'<>q::text THEN RAISE EXCEPTION 'QUOTES_RECEIPT_REPLAY'; END IF;
 PERFORM app.idempotency_claim(t,'user',staff,'legacy.action','legacy',sha256('legacy'::bytea),interval '1 hour',draft);
 PERFORM set_config('app.idempotency_claim',draft::text,true);
 PERFORM pg_temp.quote_reject(format('UPDATE app.idempotency_records SET state=''completed'',response_status=200,response_body=''{}'',completed_at=clock_timestamp() WHERE tenant_id=%L AND operation=''legacy.action''',t),'23514','legacy inline denied');
END $runtime$;
RESET ROLE;
-- Fault probes: removing the corresponding guards must make these RED.
SELECT pg_temp.quote_reject(format('UPDATE app.quotes SET snapshot=''{}'' WHERE id=%L',current_setting('quotes.q')),'55000','immutable snapshot fault probe');
SELECT pg_temp.quote_reject(format('UPDATE app.quote_lines SET unit_price=1 WHERE quote_id=%L',current_setting('quotes.q')),'55000','immutable line fault probe');
SELECT pg_temp.quote_reject(format('DELETE FROM app.quote_approvals WHERE quote_id=%L',current_setting('quotes.q')),'55000','immutable approval');
-- Expiry fixture changes only the time under migrator authority. Commands remain
-- app_runtime and must reject accept/approve without leaving an approval/event.
SELECT pg_temp.quote_create() AS expired_sent \gset
SELECT pg_temp.quote_create() AS expired_pending \gset
SET LOCAL ROLE app_runtime;
SELECT app.transition_quote(:'ta',:'expired_sent',1,'send','staff',:'staff');
SELECT app.transition_quote(:'ta',:'expired_pending',1,'send','staff',:'staff');
SELECT app.transition_quote(:'ta',:'expired_pending',2,'accept','customer',:'buyer');
RESET ROLE;
ALTER TABLE app.quotes DISABLE TRIGGER quote_guard;
UPDATE app.quotes SET expires_at=clock_timestamp()-interval '1 second' WHERE id IN(:'expired_sent',:'expired_pending');
ALTER TABLE app.quotes ENABLE TRIGGER quote_guard;
SET LOCAL ROLE app_runtime;
SELECT pg_temp.quote_reject(format('SELECT app.transition_quote(%L,%L,2,''accept'',''customer'',%L)',:'ta',:'expired_sent',:'buyer'),'22023','expired accept');
SELECT pg_temp.quote_reject(format('SELECT app.transition_quote(%L,%L,3,''approve'',''customer'',%L)',:'ta',:'expired_pending',:'approver'),'22023','expired approve');
SELECT pg_temp.quote_reject(format('SELECT app.transition_quote(%L,%L,3,''reject'',''customer'',%L)',:'ta',:'expired_pending',:'approver'),'22023','expired reject');
SELECT app.transition_quote(:'ta',:'expired_sent',2,'expire','staff',:'staff');
SELECT app.transition_quote(:'ta',:'expired_pending',3,'expire','staff',:'staff');
RESET ROLE;
-- Outbox failure aborts the lifecycle statement, rather than committing state
-- first and attempting event delivery outside its transaction.
SET LOCAL ROLE app_runtime;
SELECT pg_temp.quote_create() AS atomic_quote \gset
SELECT app.transition_quote(:'ta',:'atomic_quote',1,'send','staff',:'staff');
SELECT app.transition_quote(:'ta',:'atomic_quote',2,'accept','customer',:'buyer');
RESET ROLE;
REVOKE INSERT(tenant_id,event_type,payload,delivery_target,dedup_key) ON app.outbox_events FROM app_quotes_executor;
SET LOCAL ROLE app_runtime;
SELECT pg_temp.quote_reject(format('SELECT app.transition_quote(%L,%L,3,''approve'',''customer'',%L)',:'ta',:'atomic_quote',:'approver'),'42501','outbox failure');
RESET ROLE;
GRANT INSERT(tenant_id,event_type,payload,delivery_target,dedup_key) ON app.outbox_events TO app_quotes_executor;
SELECT set_config('quotes.atomic',:'atomic_quote',true);
DO $atomic$ BEGIN
 IF (SELECT status FROM app.quotes WHERE id=current_setting('quotes.atomic')::uuid)<>'accepted_pending_approval'
 OR EXISTS(SELECT FROM app.quote_approvals WHERE quote_id=current_setting('quotes.atomic')::uuid)
 OR (SELECT count(*) FROM app.outbox_events WHERE payload->>'quote_id'=current_setting('quotes.atomic'))<>3 THEN RAISE EXCEPTION 'QUOTES_OUTBOX_FAILURE_COMMITTED_STATE'; END IF;
END $atomic$;
UPDATE app.buyer_company_members SET role_code='purchaser' WHERE tenant_id=:'ta' AND customer_id=:'approver';
SET LOCAL ROLE app_runtime;
SELECT pg_temp.quote_reject(format('SELECT app.transition_quote(%L,%L,3,''approve'',''customer'',%L)',:'ta',:'atomic_quote',:'approver'),'42501','purchaser cannot approve');
RESET ROLE;
UPDATE app.buyer_company_members SET role_code='approver' WHERE tenant_id=:'ta' AND customer_id=:'approver';
-- Active records are rechecked in the DB rather than trusting a service check.
UPDATE app.tenant_memberships SET status='suspended' WHERE tenant_id=:'ta' AND user_id=:'staff';
SET LOCAL ROLE app_runtime;
SELECT pg_temp.quote_reject('SELECT pg_temp.quote_create()','42501','suspended staff');
RESET ROLE;
UPDATE app.tenant_memberships SET status='active' WHERE tenant_id=:'ta' AND user_id=:'staff';
UPDATE app.users SET status='disabled' WHERE id=:'staff';
SET LOCAL ROLE app_runtime;
SELECT pg_temp.quote_reject('SELECT pg_temp.quote_create()','42501','disabled staff');
RESET ROLE;
UPDATE app.users SET status='active' WHERE id=:'staff';
UPDATE app.buyer_company_members SET status='revoked' WHERE tenant_id=:'ta' AND customer_id=:'buyer';
SET LOCAL ROLE app_runtime;
DO $revoked$ BEGIN
 IF app.quote_actor_access(current_setting('quotes.t')::uuid,current_setting('quotes.company')::uuid,'customer',current_setting('quotes.buyer')::uuid,'read') THEN RAISE EXCEPTION 'QUOTES_REVOKED_MEMBER'; END IF;
END $revoked$;
SELECT set_config('app.tenant_id',:'tb',true);
DO $foreign$ BEGIN
 IF EXISTS(SELECT FROM app.quotes) OR EXISTS(SELECT FROM app.quote_lines) OR EXISTS(SELECT FROM app.quote_approvals) THEN RAISE EXCEPTION 'QUOTES_FOREIGN_READ'; END IF;
END $foreign$;
RESET ROLE;
ROLLBACK;
SELECT 'PASS: quotes actor/tenant states, expiry/CAS, immutable snapshots/lines/approvals, fenced receipts and atomic outbox' AS result;
