-- Cart boundary gate; privileged runner seeds fixtures then exercises runtime.
-- Depends on customer/catalog/cart DDL. Caller check_pg; all data rolls back.
-- Owner/tenant failures cannot expose a row; grants and CAS are independently checked.
\set ON_ERROR_STOP on
BEGIN;
INSERT INTO app.tenants(name) VALUES('cart-A') RETURNING id AS ta \gset
INSERT INTO app.tenants(name) VALUES('cart-B') RETURNING id AS tb \gset
INSERT INTO app.customers(tenant_id,email) VALUES(:'ta','a@example.test') RETURNING id AS ca \gset
INSERT INTO app.customers(tenant_id,email) VALUES(:'ta','other@example.test') RETURNING id AS cb \gset
INSERT INTO app.customers(tenant_id,email) VALUES(:'tb','b@example.test') RETURNING id AS cc \gset
INSERT INTO app.products(tenant_id,product_code,lifecycle_status) VALUES(:'ta','cart','ready') RETURNING id AS pa \gset
INSERT INTO app.products(tenant_id,product_code) VALUES(:'tb','cart') RETURNING id AS pb \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(:'ta',:'pa','cart','default') RETURNING id AS sa \gset
INSERT INTO app.product_variants(tenant_id,product_id,sku,option_key) VALUES(:'tb',:'pb','cart','default') RETURNING id AS sb \gset
INSERT INTO app.carts(tenant_id,customer_id,currency) VALUES(:'ta',:'ca','USD') RETURNING id AS c \gset
INSERT INTO app.carts(tenant_id,customer_id,currency) VALUES(:'ta',:'cb','USD') RETURNING id AS other \gset
INSERT INTO app.carts(tenant_id,guest_token_hash,currency) VALUES(:'ta',sha256('guest'::bytea),'USD') RETURNING id AS guest \gset
INSERT INTO app.carts(tenant_id,customer_id,currency) VALUES(:'tb',:'cc','USD') RETURNING id AS foreign_cart \gset
INSERT INTO app.cart_lines VALUES(:'ta',:'c',:'sa',3),(:'ta',:'other',:'sa',7),(:'ta',:'guest',:'sa',2),(:'tb',:'foreign_cart',:'sb',9);
SELECT set_config('test.cart_tenant',:'ta',true),set_config('test.cart_customer',:'ca',true),
 set_config('test.cart_id',:'c',true),set_config('test.cart_other',:'other',true),set_config('test.cart_guest',:'guest',true),
 set_config('test.cart_sku',:'sa',true),set_config('test.cart_foreign_sku',:'sb',true),
 set_config('test.cart_foreign_customer',:'cc',true);
DO $constraints$
DECLARE t uuid:=current_setting('test.cart_tenant')::uuid; c uuid:=current_setting('test.cart_id')::uuid; s uuid:=current_setting('test.cart_sku')::uuid;
BEGIN
 -- Privacy migration adds one tenant-only policy per table for its non-login
 -- executor. Runtime must retain the original owner policy and no other policy.
 IF (SELECT count(*) FROM pg_policy WHERE polrelid IN ('app.carts'::regclass,'app.cart_lines'::regclass))<>
    (CASE WHEN to_regclass('app.data_rights_exports') IS NULL THEN 2 ELSE 4 END)
  OR (SELECT count(*) FROM pg_policy WHERE polrelid IN ('app.carts'::regclass,'app.cart_lines'::regclass)
      AND polname='tenant_scope' AND polcmd='*' AND polpermissive
      AND polroles=ARRAY['app_runtime'::regrole::oid])<>2 THEN
  RAISE EXCEPTION 'CART_EXACT_OWNER_POLICIES_REQUIRED'; END IF;
 IF to_regclass('app.data_rights_exports') IS NOT NULL AND
   (SELECT count(*) FROM pg_policy WHERE polrelid IN ('app.carts'::regclass,'app.cart_lines'::regclass)
      AND polname='data_rights_scope' AND polcmd='*' AND polpermissive
      AND polroles=ARRAY['app_data_rights_executor'::regrole::oid]
      AND pg_get_expr(polqual,polrelid)=$expr$(tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)$expr$
      AND pg_get_expr(polwithcheck,polrelid)=pg_get_expr(polqual,polrelid))<>2 THEN
  RAISE EXCEPTION 'CART_PRIVACY_EXECUTOR_POLICY_REQUIRED'; END IF;
 IF EXISTS(SELECT FROM pg_class WHERE oid IN ('app.carts'::regclass,'app.cart_lines'::regclass) AND NOT(relrowsecurity AND relforcerowsecurity)) THEN
  RAISE EXCEPTION 'CART_RLS_FORCE_REQUIRED'; END IF;
 BEGIN INSERT INTO app.carts(tenant_id,customer_id,currency) VALUES(t,current_setting('test.cart_foreign_customer')::uuid,'USD');
  RAISE EXCEPTION 'CART_CROSS_CUSTOMER_ACCEPTED'; EXCEPTION WHEN foreign_key_violation THEN NULL; END;
 BEGIN INSERT INTO app.cart_lines VALUES(t,c,current_setting('test.cart_foreign_sku')::uuid,1);
  RAISE EXCEPTION 'CART_CROSS_SKU_ACCEPTED'; EXCEPTION WHEN foreign_key_violation THEN NULL; END;
 BEGIN INSERT INTO app.carts(tenant_id,currency) VALUES(t,'USD'); RAISE EXCEPTION 'CART_NO_OWNER_ACCEPTED'; EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN INSERT INTO app.carts(tenant_id,customer_id,guest_token_hash,currency) VALUES(t,current_setting('test.cart_customer')::uuid,sha256('x'::bytea),'USD');
  RAISE EXCEPTION 'CART_DUAL_OWNER_ACCEPTED'; EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN INSERT INTO app.carts(tenant_id,guest_token_hash,currency) VALUES(t,'\x01','USD'); RAISE EXCEPTION 'CART_SHORT_HASH_ACCEPTED'; EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN INSERT INTO app.carts(tenant_id,guest_token_hash,currency) VALUES(t,sha256('x'::bytea),'usd'); RAISE EXCEPTION 'CART_BAD_CURRENCY_ACCEPTED'; EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN UPDATE app.carts SET version=0 WHERE (tenant_id,id)=(t,c); RAISE EXCEPTION 'CART_VERSION_RESET_ACCEPTED'; EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN UPDATE app.cart_lines SET quantity=0 WHERE (tenant_id,cart_id,sku_id)=(t,c,s); RAISE EXCEPTION 'CART_ZERO_QUANTITY_ACCEPTED'; EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN UPDATE app.cart_lines SET quantity=1000001 WHERE (tenant_id,cart_id,sku_id)=(t,c,s); RAISE EXCEPTION 'CART_BIG_QUANTITY_ACCEPTED'; EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN INSERT INTO app.cart_lines VALUES(t,c,s,1); RAISE EXCEPTION 'CART_DUPLICATE_SKU_ACCEPTED'; EXCEPTION WHEN unique_violation THEN NULL; END;
END $constraints$;
SELECT set_config('app.tenant_id',:'ta',true),set_config('app.cart_customer_id',:'ca',true),set_config('app.cart_token_hash','',true);
SET LOCAL ROLE app_runtime;
DO $runtime$
DECLARE t uuid:=current_setting('test.cart_tenant')::uuid; c uuid:=current_setting('test.cart_id')::uuid; s uuid:=current_setting('test.cart_sku')::uuid; n int;
BEGIN
 IF NOT app.cart_tenant_active() THEN RAISE EXCEPTION 'CART_ACTIVE_TENANT_HELPER_FALSE'; END IF;
 IF (SELECT count(*) FROM app.carts)<>1 OR (SELECT count(*) FROM app.cart_lines)<>1 THEN RAISE EXCEPTION 'CART_OWNER_LEAK'; END IF;
 BEGIN UPDATE app.carts SET customer_id=NULL WHERE id=c; RAISE EXCEPTION 'CART_OWNER_WRITE_ACCEPTED'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN UPDATE app.carts SET guest_token_hash=sha256('changed'::bytea) WHERE id=c; RAISE EXCEPTION 'CART_TOKEN_WRITE_ACCEPTED'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN UPDATE app.carts SET currency='EUR' WHERE id=c; RAISE EXCEPTION 'CART_CURRENCY_WRITE_ACCEPTED'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN INSERT INTO app.carts(tenant_id,customer_id,currency,version) VALUES(t,current_setting('test.cart_customer')::uuid,'USD',99);
  RAISE EXCEPTION 'CART_INITIAL_VERSION_WRITE_ACCEPTED'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 BEGIN UPDATE app.carts SET version=99 WHERE id=c; RAISE EXCEPTION 'CART_VERSION_JUMP_ACCEPTED'; EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN INSERT INTO app.cart_lines VALUES(t,current_setting('test.cart_other')::uuid,s,1); RAISE EXCEPTION 'CART_OTHER_LINE_WRITE_ACCEPTED'; EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 UPDATE app.carts SET version=version+1 WHERE (tenant_id,id)=(t,c) AND version=1; GET DIAGNOSTICS n=ROW_COUNT;
 IF n<>1 THEN RAISE EXCEPTION 'CART_CAS_FIRST_FAILED'; END IF;
 UPDATE app.carts SET version=version+1 WHERE (tenant_id,id)=(t,c) AND version=1; GET DIAGNOSTICS n=ROW_COUNT;
 IF n<>0 OR (SELECT version FROM app.carts WHERE id=c)<>2 THEN RAISE EXCEPTION 'CART_STALE_CAS_ACCEPTED'; END IF;
 INSERT INTO app.cart_lines VALUES(t,c,s,1000000) ON CONFLICT(tenant_id,cart_id,sku_id) DO UPDATE SET quantity=excluded.quantity;
 IF (SELECT quantity FROM app.cart_lines WHERE cart_id=c)<>1000000 THEN RAISE EXCEPTION 'CART_ABSOLUTE_QUANTITY_FAILED'; END IF;
 DELETE FROM app.cart_lines WHERE cart_id=c;
 IF EXISTS(SELECT FROM app.cart_lines) THEN RAISE EXCEPTION 'CART_DELETE_FAILED'; END IF;
 PERFORM set_config('app.cart_customer_id','',true);
 IF EXISTS(SELECT FROM app.carts) OR EXISTS(SELECT FROM app.cart_lines) THEN RAISE EXCEPTION 'CART_BLANK_ACTOR_LEAK'; END IF;
 PERFORM set_config('app.cart_token_hash',encode(sha256('wrong'::bytea),'hex'),true);
 IF EXISTS(SELECT FROM app.carts) THEN RAISE EXCEPTION 'CART_WRONG_TOKEN_LEAK'; END IF;
 PERFORM set_config('app.cart_token_hash','malformed',true);
 IF EXISTS(SELECT FROM app.carts) THEN RAISE EXCEPTION 'CART_MALFORMED_TOKEN_LEAK'; END IF;
 PERFORM set_config('app.cart_token_hash',encode(sha256('guest'::bytea),'hex'),true);
 IF (SELECT count(*) FROM app.carts)<>1 OR (SELECT quantity FROM app.cart_lines)<>2 THEN RAISE EXCEPTION 'CART_GUEST_LOOKUP_FAILED'; END IF;
 PERFORM set_config('app.cart_customer_id',current_setting('test.cart_customer'),true);
 IF EXISTS(SELECT FROM app.carts) THEN RAISE EXCEPTION 'CART_AMBIGUOUS_ACTOR_LEAK'; END IF;
 PERFORM set_config('app.cart_token_hash','',true);
 PERFORM set_config('app.tenant_id','',true);
 IF app.cart_tenant_active() THEN RAISE EXCEPTION 'CART_BLANK_TENANT_HELPER_TRUE'; END IF;
 IF EXISTS(SELECT FROM app.carts) OR EXISTS(SELECT FROM app.cart_lines) THEN RAISE EXCEPTION 'CART_BLANK_TENANT_LEAK'; END IF;
END $runtime$;
RESET ROLE;
-- Revoke the tenant itself while both customer and guest remain otherwise valid.
-- Count zero is the database's not-found/404 boundary; blocked writes must be42501.
INSERT INTO app.cart_lines VALUES(:'ta',:'c',:'sa',3);
UPDATE app.tenants SET status='suspended' WHERE id=:'ta';
SET LOCAL ROLE app_runtime;
DO $suspended$
DECLARE t uuid:=current_setting('test.cart_tenant')::uuid; s uuid:=current_setting('test.cart_sku')::uuid;
 actor text; c uuid; n int; failures text[]:=ARRAY[]::text[];
BEGIN
 PERFORM set_config('app.tenant_id',t::text,true);
 IF app.cart_tenant_active() THEN failures:=array_append(failures,'helper_true_for_suspended'); END IF;
 FOREACH actor IN ARRAY ARRAY['guest','customer'] LOOP
  IF actor='guest' THEN
   c:=current_setting('test.cart_guest')::uuid;
   PERFORM set_config('app.cart_customer_id','',true);
   PERFORM set_config('app.cart_token_hash',encode(sha256('guest'::bytea),'hex'),true);
  ELSE
   c:=current_setting('test.cart_id')::uuid;
   PERFORM set_config('app.cart_customer_id',current_setting('test.cart_customer'),true);
   PERFORM set_config('app.cart_token_hash','',true);
  END IF;
  IF EXISTS(SELECT FROM app.carts) THEN failures:=array_append(failures,actor||'_cart_visible'); END IF;
  IF EXISTS(SELECT FROM app.cart_lines) THEN failures:=array_append(failures,actor||'_line_visible'); END IF;
  BEGIN
   IF actor='guest' THEN
    INSERT INTO app.carts(tenant_id,guest_token_hash,currency) VALUES(t,sha256('guest'::bytea),'USD');
   ELSE
    INSERT INTO app.carts(tenant_id,customer_id,currency) VALUES(t,current_setting('test.cart_customer')::uuid,'USD');
   END IF;
   failures:=array_append(failures,actor||'_create_accepted');
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;
  BEGIN
   INSERT INTO app.cart_lines VALUES(t,c,s,4) ON CONFLICT(tenant_id,cart_id,sku_id) DO UPDATE SET quantity=excluded.quantity;
   failures:=array_append(failures,actor||'_line_write_accepted');
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;
  UPDATE app.carts SET version=version+1 WHERE (tenant_id,id)=(t,c); GET DIAGNOSTICS n=ROW_COUNT;
  IF n<>0 THEN failures:=array_append(failures,actor||'_version_changed'); END IF;
  DELETE FROM app.cart_lines WHERE (tenant_id,cart_id)=(t,c); GET DIAGNOSTICS n=ROW_COUNT;
  IF n<>0 THEN failures:=array_append(failures,actor||'_line_deleted'); END IF;
 END LOOP;
 IF cardinality(failures)>0 THEN RAISE EXCEPTION 'CART_SUSPENDED_TENANT_BYPASS: %',array_to_string(failures,','); END IF;
END $suspended$;
RESET ROLE;
UPDATE app.tenants SET status='active' WHERE id=:'ta';
UPDATE app.customers SET status='disabled' WHERE (tenant_id,id)=(:'ta',:'ca');
SELECT set_config('app.tenant_id',:'ta',true),set_config('app.cart_customer_id',:'ca',true),set_config('app.cart_token_hash','',true);
SET LOCAL ROLE app_runtime;
DO $disabled$ BEGIN
 IF EXISTS(SELECT FROM app.carts) OR EXISTS(SELECT FROM app.cart_lines) THEN RAISE EXCEPTION 'CART_DISABLED_CUSTOMER_LEAK'; END IF;
END $disabled$;
RESET ROLE;
DO $function_acl$ BEGIN
 IF EXISTS(SELECT FROM pg_proc WHERE oid='app.cart_guard_version()'::regprocedure AND (prosecdef OR replace(proconfig[1],' ','')<>'search_path=pg_catalog,app,pg_temp'))
 OR has_function_privilege('app_runtime','app.cart_guard_version()','EXECUTE')
 OR EXISTS(SELECT FROM aclexplode((SELECT proacl FROM pg_proc WHERE oid='app.cart_guard_version()'::regprocedure)) WHERE grantee=0) THEN
 RAISE EXCEPTION 'CART_UNSAFE_TRIGGER_FUNCTION'; END IF;
END $function_acl$;
ROLLBACK;
SELECT 'PASS: CART_EXPECTED_REFUSAL ownership/tenant/FK/grants/version/absolute quantity gate' AS result;
