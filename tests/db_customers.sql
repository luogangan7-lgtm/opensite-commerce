-- Customer/B2B data contract; depends on the customers_b2b forward migration.
-- Called by check_pg on disposable PG18; all fixtures roll back.
-- Expected failures identify the exact guard, never an unrelated SQL exception.
-- BEGIN CUSTOMERS_PASSWORD_UPDATED_AT_PROBE: independent of buyer-role seeding.
BEGIN;
DO $password_fixture$
DECLARE t uuid:=uuidv7(); customer uuid:=uuidv7();
 phc text:='$argon2id$v=19$m=19456,t=2,p=1$YWJjZGVmZ2hpamtsbW5vcA$YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXowMTIzNDU';
BEGIN
 INSERT INTO app.tenants(id,name) VALUES(t,'customers-password-timestamp');
 INSERT INTO app.customers(tenant_id,id,email) VALUES(t,customer,'timestamp@example.invalid');
 INSERT INTO app.customer_credentials(tenant_id,customer_id,password_hash,updated_at)
 VALUES(t,customer,phc,'2000-01-01 00:00:00+00');
 PERFORM set_config('test.customers.password_tenant',t::text,true);
 PERFORM set_config('test.customers.password_customer',customer::text,true);
 PERFORM set_config('test.customers.password_phc',phc,true);
 PERFORM set_config('app.tenant_id',t::text,true);
END $password_fixture$;
SET LOCAL ROLE app_customer_auth;
DO $password_timestamp$
DECLARE t uuid:=current_setting('test.customers.password_tenant')::uuid;
 customer uuid:=current_setting('test.customers.password_customer')::uuid;
 phc text:=current_setting('test.customers.password_phc');
 old_timestamp timestamptz; new_timestamp timestamptz; changed integer;
 before_change timestamptz;
BEGIN
 SELECT updated_at INTO STRICT old_timestamp FROM app.customer_credentials
 WHERE tenant_id=t AND customer_id=customer;
 UPDATE app.customer_credentials SET password_hash=phc WHERE tenant_id=t AND customer_id=customer;
 SELECT updated_at INTO STRICT new_timestamp FROM app.customer_credentials
 WHERE tenant_id=t AND customer_id=customer;
 IF new_timestamp IS DISTINCT FROM old_timestamp THEN
  RAISE EXCEPTION 'CUSTOMERS_PASSWORD_SAME_HASH_UPDATED_AT';
 END IF;
 before_change:=clock_timestamp();
 UPDATE app.customer_credentials SET password_hash=replace(phc,'m=19456','m=19457')
 WHERE tenant_id=t AND customer_id=customer;
 GET DIAGNOSTICS changed=ROW_COUNT;
 SELECT updated_at INTO STRICT new_timestamp FROM app.customer_credentials
 WHERE tenant_id=t AND customer_id=customer;
 IF changed<>1 OR new_timestamp IS NOT DISTINCT FROM old_timestamp
  OR new_timestamp<before_change OR new_timestamp>clock_timestamp() THEN
  RAISE EXCEPTION 'CUSTOMERS_PASSWORD_UPDATED_AT';
 END IF;
 BEGIN
  UPDATE app.customer_credentials SET updated_at=old_timestamp
  WHERE tenant_id=t AND customer_id=customer;
  RAISE EXCEPTION 'CUSTOMERS_AUTH_TIMESTAMP_UPDATE_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $password_timestamp$;
RESET ROLE;
ROLLBACK;
-- END CUSTOMERS_PASSWORD_UPDATED_AT_PROBE.

BEGIN;
DO $customers$
DECLARE
 t uuid := uuidv7(); other_t uuid := uuidv7();
 customer uuid := uuidv7(); other_customer uuid := uuidv7(); guest uuid := uuidv7();
 other_guest uuid := uuidv7();
 company uuid := uuidv7(); second_company uuid := uuidv7(); other_company uuid := uuidv7();
 phc text := '$argon2id$v=19$m=19456,t=2,p=1$YWJjZGVmZ2hpamtsbW5vcA$YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXowMTIzNDU';
 duplicate_email text; invalid_hash text; invalid_country text;
 guard text; n integer; row_record record; role_record record;
BEGIN
 INSERT INTO app.tenants(id,name) VALUES(t,'customers-A'),(other_t,'customers-B');
 -- Privileged tenant creation alone seeds the complete fixed tenant-local role set.
 FOR row_record IN SELECT fixture.tenant_id FROM (VALUES(t),(other_t)) fixture(tenant_id) LOOP
  IF (SELECT array_agg(code ORDER BY code) FROM app.buyer_roles
    WHERE tenant_id=row_record.tenant_id) IS DISTINCT FROM ARRAY['admin','approver','purchaser']::text[] THEN
   RAISE EXCEPTION 'CUSTOMERS_NEW_TENANT_ROLE_SEED: %',row_record.tenant_id;
  END IF;
 END LOOP;
 -- Staff and storefront identifiers are independent, even for the same email.
 INSERT INTO app.users(email,password_hash)
 VALUES('mixed.case@example.invalid','non-secret-test-fixture');
 INSERT INTO app.customers(tenant_id,id,email)
 VALUES(t,customer,E'\t  Mixed.Case@example.invalid \n'),
       (other_t,other_customer,'mixed.case@example.invalid'),
       (other_t,other_guest,'foreign-guest@example.invalid'),
       (t,guest,'guest@example.invalid');
 IF (SELECT email_normalized FROM app.customers WHERE tenant_id=t AND id=customer)
    IS DISTINCT FROM 'mixed.case@example.invalid' THEN
  RAISE EXCEPTION 'CUSTOMERS_EMAIL_NORMALIZATION';
 END IF;
 -- A guest customer exists without creating any credential row.
 IF EXISTS(SELECT FROM app.customer_credentials WHERE tenant_id=t AND customer_id=guest) THEN
  RAISE EXCEPTION 'CUSTOMERS_GUEST_REQUIRES_CREDENTIAL';
 END IF;
 FOREACH duplicate_email IN ARRAY ARRAY['mixed.case@example.invalid',
   'MIXED.CASE@EXAMPLE.INVALID',E' \tMixed.Case@example.invalid\n '] LOOP
  BEGIN
   INSERT INTO app.customers(tenant_id,email) VALUES(t,duplicate_email);
   RAISE EXCEPTION 'CUSTOMERS_DUPLICATE_EMAIL_ACCEPTED';
  EXCEPTION WHEN unique_violation THEN
   GET STACKED DIAGNOSTICS guard=CONSTRAINT_NAME;
   IF guard IS DISTINCT FROM 'customers_tenant_email_unique' THEN
    RAISE EXCEPTION 'CUSTOMERS_WRONG_DUPLICATE_GUARD: %',guard;
   END IF;
  END;
 END LOOP;
 BEGIN
  INSERT INTO app.customers(tenant_id,email) VALUES(t,E' \t\n ');
  RAISE EXCEPTION 'CUSTOMERS_BLANK_EMAIL_ACCEPTED';
 EXCEPTION WHEN check_violation THEN
  GET STACKED DIAGNOSTICS guard=CONSTRAINT_NAME;
  IF guard IS DISTINCT FROM 'customers_email_check' THEN
   RAISE EXCEPTION 'CUSTOMERS_WRONG_EMAIL_GUARD: %',guard;
  END IF;
 END;
 BEGIN
  INSERT INTO app.customers(tenant_id,email,status) VALUES(t,'status@example.invalid','owner');
  RAISE EXCEPTION 'CUSTOMERS_INVALID_STATUS_ACCEPTED';
 EXCEPTION WHEN check_violation THEN
  GET STACKED DIAGNOSTICS guard=CONSTRAINT_NAME;
  IF guard IS DISTINCT FROM 'customers_status_check' THEN
   RAISE EXCEPTION 'CUSTOMERS_WRONG_STATUS_GUARD: %',guard;
  END IF;
 END;
 INSERT INTO app.customer_credentials(tenant_id,customer_id,password_hash)
 VALUES(t,customer,phc),(other_t,other_customer,phc);
 FOREACH invalid_hash IN ARRAY ARRAY['plaintext', '$argon2i$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA',
   '$argon2id$v=16$m=19456,t=2,p=1$c2FsdA$aGFzaA',
   '$argon2id$v=19$m=0,t=2,p=1$c2FsdA$aGFzaA',
   '$argon2id$v=19$m=19456,t=0,p=1$c2FsdA$aGFzaA',
   '$argon2id$v=19$m=19456,t=2,p=0$c2FsdA$aGFzaA',
   '$argon2id$v=19$m=19456,t=2,p=1$bad!$aGFzaA',repeat('x',1025)] LOOP
  BEGIN
   UPDATE app.customer_credentials SET password_hash=invalid_hash
   WHERE tenant_id=t AND customer_id=customer;
   RAISE EXCEPTION 'CUSTOMERS_INVALID_PHC_ACCEPTED';
  EXCEPTION WHEN check_violation THEN
   GET STACKED DIAGNOSTICS guard=CONSTRAINT_NAME;
   IF guard IS DISTINCT FROM 'customer_credentials_password_hash_check' THEN
    RAISE EXCEPTION 'CUSTOMERS_WRONG_PHC_GUARD: %',guard;
   END IF;
  END;
 END LOOP;
 BEGIN
  INSERT INTO app.customer_credentials(tenant_id,customer_id,password_hash)
  VALUES(t,other_customer,phc);
  RAISE EXCEPTION 'CUSTOMERS_FOREIGN_CREDENTIAL_ACCEPTED';
 EXCEPTION WHEN foreign_key_violation THEN
  GET STACKED DIAGNOSTICS guard=CONSTRAINT_NAME;
  IF guard IS DISTINCT FROM 'customer_credentials_customer_fk' THEN
   RAISE EXCEPTION 'CUSTOMERS_WRONG_CREDENTIAL_FK: %',guard;
  END IF;
 END;
 INSERT INTO app.customer_addresses(tenant_id,customer_id,recipient,address_line1,city,postal_code,country_code)
 VALUES(t,customer,'Fixture','Line 1','City','00000','US'),
       (other_t,other_customer,'Fixture','Line 1','City','00000','US');
 BEGIN
  INSERT INTO app.customer_addresses(tenant_id,customer_id,recipient,address_line1,city,postal_code,country_code)
  VALUES(t,other_customer,'Fixture','Line 1','City','00000','US');
  RAISE EXCEPTION 'CUSTOMERS_FOREIGN_ADDRESS_ACCEPTED';
 EXCEPTION WHEN foreign_key_violation THEN
  GET STACKED DIAGNOSTICS guard=CONSTRAINT_NAME;
  IF guard IS DISTINCT FROM 'customer_addresses_customer_fk' THEN
   RAISE EXCEPTION 'CUSTOMERS_WRONG_ADDRESS_FK: %',guard;
  END IF;
 END;
 FOREACH invalid_country IN ARRAY ARRAY['us','USA','U1','ZZ',''] LOOP
  BEGIN
   INSERT INTO app.customer_addresses(tenant_id,customer_id,recipient,address_line1,city,postal_code,country_code)
   VALUES(t,customer,'Fixture','Line 1','City','00000',invalid_country);
   RAISE EXCEPTION 'CUSTOMERS_INVALID_COUNTRY_ACCEPTED';
  EXCEPTION WHEN check_violation THEN
   GET STACKED DIAGNOSTICS guard=CONSTRAINT_NAME;
   IF guard IS DISTINCT FROM 'customer_addresses_country_code_check' THEN
    RAISE EXCEPTION 'CUSTOMERS_WRONG_COUNTRY_GUARD: %',guard;
   END IF;
  END;
 END LOOP;
 INSERT INTO app.buyer_companies(tenant_id,id,name)
 VALUES(t,company,'Purchasing A'),(t,second_company,'Purchasing A2'),(other_t,other_company,'Purchasing B');
 BEGIN
  INSERT INTO app.buyer_roles(tenant_id,code) VALUES(t,'owner');
  RAISE EXCEPTION 'CUSTOMERS_NON_MVP_ROLE_ACCEPTED';
 EXCEPTION WHEN check_violation THEN
  GET STACKED DIAGNOSTICS guard=CONSTRAINT_NAME;
  IF guard IS DISTINCT FROM 'buyer_roles_code_check' THEN
   RAISE EXCEPTION 'CUSTOMERS_WRONG_ROLE_GUARD: %',guard;
  END IF;
 END;
 INSERT INTO app.buyer_company_members(tenant_id,buyer_company_id,customer_id,role_code)
 VALUES(t,company,customer,'purchaser'),(t,second_company,customer,'approver'),
       (other_t,other_company,other_customer,'admin');
 IF (SELECT count(*) FROM app.buyer_company_members WHERE tenant_id=t AND customer_id=customer AND status='active')<>2 THEN
  RAISE EXCEPTION 'CUSTOMERS_MULTI_COMPANY_MEMBERSHIP';
 END IF;
 BEGIN
  INSERT INTO app.buyer_company_members(tenant_id,buyer_company_id,customer_id,role_code)
  VALUES(t,company,other_customer,'purchaser');
  RAISE EXCEPTION 'CUSTOMERS_FOREIGN_MEMBER_ACCEPTED';
 EXCEPTION WHEN foreign_key_violation THEN
  GET STACKED DIAGNOSTICS guard=CONSTRAINT_NAME;
  IF guard IS DISTINCT FROM 'buyer_company_members_customer_fk' THEN
   RAISE EXCEPTION 'CUSTOMERS_WRONG_MEMBER_FK: %',guard;
  END IF;
 END;
 BEGIN
  INSERT INTO app.buyer_company_members(tenant_id,buyer_company_id,customer_id,role_code)
  VALUES(t,other_company,customer,'purchaser');
  RAISE EXCEPTION 'CUSTOMERS_FOREIGN_COMPANY_ACCEPTED';
 EXCEPTION WHEN foreign_key_violation THEN
  GET STACKED DIAGNOSTICS guard=CONSTRAINT_NAME;
  IF guard IS DISTINCT FROM 'buyer_company_members_company_fk' THEN
   RAISE EXCEPTION 'CUSTOMERS_WRONG_COMPANY_FK: %',guard;
  END IF;
 END;
 -- A valid role in another tenant cannot satisfy this tenant's composite FK.
 DELETE FROM app.buyer_roles WHERE tenant_id=t AND code='admin';
 BEGIN
  INSERT INTO app.buyer_company_members(tenant_id,buyer_company_id,customer_id,role_code)
  VALUES(t,second_company,guest,'admin');
  RAISE EXCEPTION 'CUSTOMERS_FOREIGN_ROLE_ACCEPTED';
 EXCEPTION WHEN foreign_key_violation THEN
  GET STACKED DIAGNOSTICS guard=CONSTRAINT_NAME;
  IF guard IS DISTINCT FROM 'buyer_company_members_role_fk' THEN
   RAISE EXCEPTION 'CUSTOMERS_WRONG_ROLE_FK: %',guard;
  END IF;
 END;
 INSERT INTO app.buyer_roles(tenant_id,code) VALUES(t,'admin');
 BEGIN
  UPDATE app.buyer_company_members SET status='owner'
  WHERE tenant_id=t AND buyer_company_id=company AND customer_id=customer;
  RAISE EXCEPTION 'CUSTOMERS_INVALID_MEMBER_STATUS_ACCEPTED';
 EXCEPTION WHEN check_violation THEN
  GET STACKED DIAGNOSTICS guard=CONSTRAINT_NAME;
  IF guard IS DISTINCT FROM 'buyer_company_members_status_check' THEN
   RAISE EXCEPTION 'CUSTOMERS_WRONG_MEMBER_STATUS_GUARD: %',guard;
  END IF;
 END;
 UPDATE app.buyer_company_members SET status='revoked'
 WHERE tenant_id=t AND buyer_company_id=company AND customer_id=customer;
 IF EXISTS(SELECT FROM app.buyer_company_members WHERE tenant_id=t
   AND buyer_company_id=company AND customer_id=customer AND status='active') THEN
  RAISE EXCEPTION 'CUSTOMERS_REVOKED_MEMBER_ACTIVE';
 END IF;
 DELETE FROM app.buyer_company_members
 WHERE tenant_id=t AND buyer_company_id=second_company AND customer_id=customer;
 IF EXISTS(SELECT FROM app.buyer_company_members WHERE tenant_id=t
   AND buyer_company_id=second_company AND customer_id=customer) THEN
  RAISE EXCEPTION 'CUSTOMERS_DELETED_MEMBER_PRESENT';
 END IF;
 INSERT INTO app.buyer_company_members(tenant_id,buyer_company_id,customer_id,role_code)
 VALUES(t,second_company,customer,'approver');
 INSERT INTO app.price_lists(tenant_id,currency,starts_at,audience,buyer_company_id)
 VALUES(t,'USD','2026-01-01Z','buyer_company',company),
       (other_t,'USD','2026-01-01Z','buyer_company',other_company);
 BEGIN
  INSERT INTO app.price_lists(tenant_id,currency,starts_at,audience,buyer_company_id)
  VALUES(t,'USD','2026-01-01Z','buyer_company',other_company);
  RAISE EXCEPTION 'CUSTOMERS_FOREIGN_PRICE_COMPANY_ACCEPTED';
 EXCEPTION WHEN foreign_key_violation THEN
  GET STACKED DIAGNOSTICS guard=CONSTRAINT_NAME;
  IF guard IS DISTINCT FROM 'price_lists_buyer_company_fk' THEN
   RAISE EXCEPTION 'CUSTOMERS_WRONG_PRICE_COMPANY_FK: %',guard;
  END IF;
 END;
 BEGIN
  INSERT INTO app.price_lists(tenant_id,currency,starts_at,audience,buyer_company_id)
  VALUES(t,'USD','2026-01-01Z','buyer_company',uuidv7());
  RAISE EXCEPTION 'CUSTOMERS_UNKNOWN_PRICE_COMPANY_ACCEPTED';
 EXCEPTION WHEN foreign_key_violation THEN
  GET STACKED DIAGNOSTICS guard=CONSTRAINT_NAME;
  IF guard IS DISTINCT FROM 'price_lists_buyer_company_fk' THEN
   RAISE EXCEPTION 'CUSTOMERS_WRONG_UNKNOWN_PRICE_COMPANY_FK: %',guard;
  END IF;
 END;
 -- No customer identity or purchase-company FK may point to a staff user,
 -- nor may a staff user acquire a new customer-domain FK.
 IF EXISTS(SELECT FROM pg_constraint c WHERE c.contype='f' AND
   ((c.conrelid IN ('app.customers'::regclass,'app.customer_credentials'::regclass,
     'app.customer_addresses'::regclass,'app.buyer_companies'::regclass,
     'app.buyer_roles'::regclass,'app.buyer_company_members'::regclass)
     AND c.confrelid='app.users'::regclass)
    OR (c.conrelid='app.users'::regclass AND c.confrelid IN
     ('app.customers'::regclass,'app.customer_credentials'::regclass,
      'app.customer_addresses'::regclass,'app.buyer_companies'::regclass,
      'app.buyer_roles'::regclass,'app.buyer_company_members'::regclass)))) THEN
  RAISE EXCEPTION 'CUSTOMERS_STAFF_IDENTITY_FK';
 END IF;
 FOR row_record IN SELECT oid,relname,relrowsecurity,relforcerowsecurity FROM pg_class
 WHERE oid IN ('app.customers'::regclass,'app.customer_credentials'::regclass,
   'app.customer_addresses'::regclass,'app.buyer_companies'::regclass,
   'app.buyer_roles'::regclass,'app.buyer_company_members'::regclass) LOOP
  IF NOT row_record.relrowsecurity OR NOT row_record.relforcerowsecurity THEN
   RAISE EXCEPTION 'CUSTOMERS_RLS_NOT_FORCED: %',row_record.relname;
  END IF;
 END LOOP;
 SELECT * INTO role_record FROM pg_roles WHERE rolname='app_customer_auth';
 IF NOT FOUND OR role_record.rolcanlogin OR role_record.rolsuper OR role_record.rolbypassrls
   OR role_record.rolcreatedb OR role_record.rolcreaterole OR role_record.rolreplication
   OR role_record.rolinherit THEN RAISE EXCEPTION 'CUSTOMERS_AUTH_ROLE_HYGIENE'; END IF;
 IF EXISTS(SELECT FROM pg_auth_members WHERE member=role_record.oid)
   OR pg_has_role('app_customer_auth','app_auth','USAGE')
   OR pg_has_role('app_customer_auth','app_auth','SET')
   OR pg_has_role('app_auth','app_customer_auth','USAGE')
   OR pg_has_role('app_auth','app_customer_auth','SET')
   OR pg_has_role('app_runtime','app_customer_auth','USAGE')
   OR pg_has_role('app_runtime','app_customer_auth','SET') THEN
  RAISE EXCEPTION 'CUSTOMERS_STAFF_PERMISSION_INHERITANCE';
 END IF;
 FOR row_record IN SELECT a.attname FROM pg_attribute a
 WHERE a.attrelid='app.customer_credentials'::regclass AND a.attnum>0 AND NOT a.attisdropped LOOP
  IF has_column_privilege('app_runtime','app.customer_credentials',row_record.attname,'SELECT')
    OR has_column_privilege('app_auth','app.customer_credentials',row_record.attname,'SELECT') THEN
   RAISE EXCEPTION 'CUSTOMERS_SENSITIVE_COLUMN_READ: %',row_record.attname;
  END IF;
 END LOOP;
 PERFORM set_config('test.customers.tenant',t::text,true);
 PERFORM set_config('test.customers.other_tenant',other_t::text,true);
 PERFORM set_config('test.customers.customer',customer::text,true);
 PERFORM set_config('test.customers.other_customer',other_customer::text,true);
 PERFORM set_config('test.customers.other_guest',other_guest::text,true);
 PERFORM set_config('test.customers.phc',phc,true);
 PERFORM set_config('app.tenant_id',t::text,true);
END $customers$;

SET LOCAL ROLE app_runtime;
DO $runtime$
DECLARE row_record record; n integer; changed integer;
BEGIN
 -- Exact visible fixture counts for every readable Customer/B2B tenant table.
 FOR row_record IN SELECT * FROM (VALUES('customers',2),('customer_addresses',1),
   ('buyer_companies',2),('buyer_roles',3),('buyer_company_members',2)) expected(table_name,row_count) LOOP
  EXECUTE format('SELECT count(*) FROM app.%I',row_record.table_name) INTO n;
  IF n<>row_record.row_count THEN
   RAISE EXCEPTION 'CUSTOMERS_RUNTIME_TENANT_READ: % = %',row_record.table_name,n;
  END IF;
 END LOOP;
 BEGIN
  PERFORM password_hash FROM app.customer_credentials;
  RAISE EXCEPTION 'CUSTOMERS_RUNTIME_CREDENTIAL_READ_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  INSERT INTO app.customers(tenant_id,email)
  VALUES(current_setting('test.customers.tenant')::uuid,'runtime@example.invalid');
  RAISE EXCEPTION 'CUSTOMERS_RUNTIME_REGISTRATION_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  UPDATE app.buyer_company_members SET status='active';
  RAISE EXCEPTION 'CUSTOMERS_RUNTIME_MEMBER_WRITE_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 -- Member authorization uses an active relation; revoked rows confer no membership.
 SELECT count(*) INTO n FROM app.buyer_company_members WHERE status='active';
 IF n<>1 THEN RAISE EXCEPTION 'CUSTOMERS_RUNTIME_ACTIVE_MEMBERSHIP: %',n; END IF;
 PERFORM set_config('app.tenant_id','',true);
 FOR row_record IN SELECT unnest(ARRAY['customers','customer_addresses','buyer_companies',
   'buyer_roles','buyer_company_members']) table_name LOOP
  EXECUTE format('SELECT count(*) FROM app.%I',row_record.table_name) INTO n;
  IF n<>0 THEN RAISE EXCEPTION 'CUSTOMERS_RUNTIME_BLANK_TENANT: %',row_record.table_name; END IF;
 END LOOP;
END $runtime$;
RESET ROLE;

SET LOCAL ROLE app_auth;
DO $staff_auth$
BEGIN
 BEGIN
  PERFORM password_hash FROM app.customer_credentials;
  RAISE EXCEPTION 'CUSTOMERS_STAFF_CREDENTIAL_READ_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  PERFORM id FROM app.customers;
  RAISE EXCEPTION 'CUSTOMERS_STAFF_CUSTOMER_READ_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $staff_auth$;
RESET ROLE;

SET LOCAL ROLE app_customer_auth;
DO $customer_auth$
DECLARE t uuid:=current_setting('test.customers.tenant')::uuid;
 other_t uuid:=current_setting('test.customers.other_tenant')::uuid;
 customer uuid:=current_setting('test.customers.customer')::uuid;
 other_guest uuid:=current_setting('test.customers.other_guest')::uuid;
 new_customer uuid:=uuidv7(); phc text:=current_setting('test.customers.phc');
 n integer; changed integer;
BEGIN
 PERFORM set_config('app.tenant_id',t::text,true);
 SELECT count(*) INTO n FROM app.customers;
 IF n<>2 THEN RAISE EXCEPTION 'CUSTOMERS_AUTH_TENANT_READ: %',n; END IF;
 SELECT count(*) INTO n FROM app.customer_credentials;
 IF n<>1 THEN RAISE EXCEPTION 'CUSTOMERS_AUTH_CREDENTIAL_TENANT_READ: %',n; END IF;
 INSERT INTO app.customers(tenant_id,id,email,display_name)
 VALUES(t,new_customer,'registered@example.invalid','Fixture customer');
 INSERT INTO app.customer_credentials(tenant_id,customer_id,password_hash)
 VALUES(t,new_customer,phc);
 UPDATE app.customer_credentials SET password_hash=phc
 WHERE tenant_id=t AND customer_id=customer;
 GET DIAGNOSTICS changed=ROW_COUNT;
 IF changed<>1 THEN RAISE EXCEPTION 'CUSTOMERS_AUTH_PASSWORD_WRITE'; END IF;
 BEGIN
  INSERT INTO app.customers(tenant_id,id,email,display_name)
  VALUES(other_t,uuidv7(),'foreign@example.invalid','Fixture customer');
  RAISE EXCEPTION 'CUSTOMERS_AUTH_FOREIGN_REGISTRATION_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  INSERT INTO app.customer_credentials(tenant_id,customer_id,password_hash)
  -- This foreign customer exists and has no credential: only RLS rejects the write.
  VALUES(other_t,other_guest,phc);
  RAISE EXCEPTION 'CUSTOMERS_AUTH_FOREIGN_CREDENTIAL_WRITE_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 UPDATE app.customer_credentials SET password_hash=phc
 WHERE tenant_id=other_t;
 GET DIAGNOSTICS changed=ROW_COUNT;
 IF changed<>0 THEN RAISE EXCEPTION 'CUSTOMERS_AUTH_FOREIGN_PASSWORD_UPDATE'; END IF;
 BEGIN
  INSERT INTO app.customers(tenant_id,id,email,display_name,status)
  VALUES(t,uuidv7(),'status-overwrite@example.invalid','Fixture','disabled');
  RAISE EXCEPTION 'CUSTOMERS_AUTH_STATUS_INSERT_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  UPDATE app.customers SET status='disabled' WHERE tenant_id=t AND id=customer;
  RAISE EXCEPTION 'CUSTOMERS_AUTH_STATUS_UPDATE_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  UPDATE app.customer_credentials SET customer_id=new_customer
  WHERE tenant_id=t AND customer_id=customer;
  RAISE EXCEPTION 'CUSTOMERS_AUTH_CREDENTIAL_IDENTITY_UPDATE_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  PERFORM password_hash FROM app.users;
  RAISE EXCEPTION 'CUSTOMERS_AUTH_STAFF_CREDENTIAL_READ_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  PERFORM user_id FROM app.tenant_memberships;
  RAISE EXCEPTION 'CUSTOMERS_AUTH_STAFF_MEMBERSHIP_READ_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  PERFORM token_hash FROM app.auth_sessions;
  RAISE EXCEPTION 'CUSTOMERS_AUTH_STAFF_SESSION_READ_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  PERFORM key_hash FROM app.api_keys;
  RAISE EXCEPTION 'CUSTOMERS_AUTH_STAFF_API_KEY_READ_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN
  PERFORM role_code FROM app.buyer_company_members;
  RAISE EXCEPTION 'CUSTOMERS_AUTH_BUYER_MEMBERSHIP_READ_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 PERFORM set_config('app.tenant_id','',true);
 SELECT count(*) INTO n FROM app.customers;
 IF n<>0 THEN RAISE EXCEPTION 'CUSTOMERS_AUTH_BLANK_TENANT'; END IF;
 SELECT count(*) INTO n FROM app.customer_credentials;
 IF n<>0 THEN RAISE EXCEPTION 'CUSTOMERS_AUTH_BLANK_CREDENTIAL_TENANT'; END IF;
 BEGIN
  INSERT INTO app.customers(tenant_id,id,email,display_name)
  VALUES(t,uuidv7(),'blank-tenant@example.invalid','Fixture');
  RAISE EXCEPTION 'CUSTOMERS_AUTH_BLANK_TENANT_REGISTRATION_ACCEPTED';
 EXCEPTION WHEN insufficient_privilege THEN NULL; END;
END $customer_auth$;
RESET ROLE;
ROLLBACK;
SELECT 'PASS: customers email normalization, guest credentials, automatic tenant roles, password timestamps, buyer tenant FKs, revocation, credential role isolation and RLS' AS result;
