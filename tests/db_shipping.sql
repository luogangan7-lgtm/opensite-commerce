-- Responsibility: real-PG Shipping constraints and least-privilege acceptance.
-- Dependencies: all current migrations, privileged expendable test database.
-- Caller: scripts/check_pg.sh; fixtures rollback, no production execution.
-- Invariants: [min,max) tiers never overlap, tenant links cannot cross, no zero
-- shipping from absent rules; runtime reads only its tenant, including empty GUC.
\set ON_ERROR_STOP on
BEGIN;
DO $shipping$
DECLARE t uuid; other_t uuid; z uuid; other_z uuid; p uuid; other_p uuid;
 first_rate uuid; row_count int; tab text;
BEGIN
 INSERT INTO app.tenants(name) VALUES('shipping-A') RETURNING id INTO t;
 INSERT INTO app.tenants(name) VALUES('shipping-B') RETURNING id INTO other_t;
 INSERT INTO app.shipping_zones(tenant_id,name) VALUES(t,'US') RETURNING id INTO z;
 INSERT INTO app.shipping_zones(tenant_id,name) VALUES(other_t,'B') RETURNING id INTO other_z;
 INSERT INTO app.shipping_zone_destinations VALUES(t,z,'US',''),(other_t,other_z,'CA','');
 INSERT INTO app.shipping_profiles(tenant_id,name,currency) VALUES(t,'A','USD') RETURNING id INTO p;
 INSERT INTO app.shipping_profiles(tenant_id,name,currency) VALUES(other_t,'B','USD') RETURNING id INTO other_p;
 INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount,min_weight_g,max_weight_g)
 VALUES(t,p,z,'by_weight','USD',100,0,500) RETURNING id INTO first_rate;
 -- Adjacent half-open intervals and zero fee are valid.
 INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount,min_weight_g,max_weight_g)
 VALUES(t,p,z,'by_weight','USD',0,500,1000);
 BEGIN
  INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount,min_weight_g,max_weight_g)
  VALUES(t,p,z,'by_weight','USD',100,499,501);
  RAISE EXCEPTION 'SHIPPING_OVERLAP_ACCEPTED';
 EXCEPTION WHEN exclusion_violation THEN NULL; END;
 BEGIN
  UPDATE app.shipping_rates SET max_weight_g=501 WHERE id=first_rate AND tenant_id=t;
  RAISE EXCEPTION 'SHIPPING_UPDATE_OVERLAP_ACCEPTED';
 EXCEPTION WHEN exclusion_violation THEN NULL; END;
 -- Null upper bound is explicitly unbounded.
 INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount,min_weight_g)
 VALUES(t,p,z,'by_weight','USD',200,1000);
 BEGIN
  INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount,min_weight_g)
  VALUES(t,p,z,'by_weight','USD',100,2000);
  RAISE EXCEPTION 'SHIPPING_UNBOUNDED_OVERLAP_ACCEPTED';
 EXCEPTION WHEN exclusion_violation THEN NULL; END;
 BEGIN
  INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount) VALUES(t,p,z,'fixed','USD',-1);
  RAISE EXCEPTION 'SHIPPING_NEGATIVE_AMOUNT_ACCEPTED';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount,min_weight_g,max_weight_g)
  VALUES(t,p,z,'by_weight','USD',1,-1,0);
  RAISE EXCEPTION 'SHIPPING_NEGATIVE_WEIGHT_ACCEPTED';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount,min_weight_g,max_weight_g)
  VALUES(t,p,z,'by_weight','USD',1,2000,2000);
  RAISE EXCEPTION 'SHIPPING_EMPTY_RANGE_ACCEPTED';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount,min_weight_g)
  VALUES(t,p,z,'fixed','USD',1,0);
  RAISE EXCEPTION 'SHIPPING_BAD_SHAPE_ACCEPTED';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount,free_over_amount)
  VALUES(t,p,z,'free_over_amount','USD',1,0);
  RAISE EXCEPTION 'SHIPPING_NONZERO_FREE_ACCEPTED';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount,free_over_amount)
  VALUES(t,p,z,'free_over_amount','USD',0,-1);
  RAISE EXCEPTION 'SHIPPING_NEGATIVE_THRESHOLD_ACCEPTED';
 EXCEPTION WHEN check_violation THEN NULL; END;
 INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount)
 VALUES(t,p,z,'fixed','USD',500);
 BEGIN
  INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount) VALUES(t,p,z,'fixed','USD',1);
  RAISE EXCEPTION 'SHIPPING_DUPLICATE_FIXED_ACCEPTED';
 EXCEPTION WHEN unique_violation THEN NULL; END;
 INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount,free_over_amount)
 VALUES(t,p,z,'free_over_amount','USD',0,10000);
 BEGIN
  INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount,free_over_amount)
  VALUES(t,p,z,'free_over_amount','USD',0,20000);
  RAISE EXCEPTION 'SHIPPING_DUPLICATE_FREE_ACCEPTED';
 EXCEPTION WHEN unique_violation THEN NULL; END;
 BEGIN
  INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount) VALUES(t,p,other_z,'fixed','USD',1);
  RAISE EXCEPTION 'SHIPPING_CROSS_ZONE_ACCEPTED';
 EXCEPTION WHEN foreign_key_violation THEN NULL; END;
 BEGIN
  INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount) VALUES(t,other_p,z,'fixed','USD',1);
  RAISE EXCEPTION 'SHIPPING_CROSS_PROFILE_ACCEPTED';
 EXCEPTION WHEN foreign_key_violation THEN NULL; END;
 BEGIN
  INSERT INTO app.shipping_zone_destinations VALUES(t,other_z,'GB','');
  RAISE EXCEPTION 'SHIPPING_CROSS_DESTINATION_ACCEPTED';
 EXCEPTION WHEN foreign_key_violation THEN NULL; END;
 BEGIN
  UPDATE app.shipping_rates SET currency='EUR' WHERE tenant_id=t AND id=first_rate;
  RAISE EXCEPTION 'SHIPPING_CURRENCY_MISMATCH_ACCEPTED';
 EXCEPTION WHEN foreign_key_violation THEN NULL; END;
 BEGIN
  INSERT INTO app.shipping_profiles(tenant_id,name,currency) VALUES(t,'bad','usd');
  RAISE EXCEPTION 'SHIPPING_INVALID_CURRENCY_ACCEPTED';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO app.shipping_zone_destinations VALUES(t,z,'ZZ','');
  RAISE EXCEPTION 'SHIPPING_INVALID_COUNTRY_ACCEPTED';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO app.shipping_zone_destinations VALUES(t,z,'US','CA-ON');
  RAISE EXCEPTION 'SHIPPING_INVALID_SUBDIVISION_ACCEPTED';
 EXCEPTION WHEN check_violation THEN NULL; END;
 INSERT INTO app.shipping_zone_destinations VALUES(t,z,'US','US-CA');
 -- Expected row sets prove the valid fixtures exist before checking isolation.
 INSERT INTO app.shipping_rates(tenant_id,profile_id,zone_id,method,currency,amount)
 VALUES(other_t,other_p,other_z,'fixed','USD',1);
 PERFORM set_config('app.tenant_id',t::text,true);
 SET LOCAL ROLE app_runtime;
 IF (SELECT count(*) FROM app.shipping_zones)<>1 OR
    (SELECT count(*) FROM app.shipping_zone_destinations)<>2 OR
    (SELECT count(*) FROM app.shipping_profiles)<>1 OR
    (SELECT count(*) FROM app.shipping_rates)<>5 THEN
  RAISE EXCEPTION 'SHIPPING_TENANT_ISOLATION_FAILED';
 END IF;
 FOREACH tab IN ARRAY ARRAY['shipping_zones','shipping_zone_destinations','shipping_profiles','shipping_rates'] LOOP
  BEGIN
   EXECUTE format('INSERT INTO app.%I DEFAULT VALUES',tab);
   RAISE EXCEPTION 'SHIPPING_RUNTIME_WRITE_ACCEPTED %',tab;
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;
 END LOOP;
 PERFORM set_config('app.tenant_id','',true);
 FOREACH tab IN ARRAY ARRAY['shipping_zones','shipping_zone_destinations','shipping_profiles','shipping_rates'] LOOP
  EXECUTE format('SELECT count(*) FROM app.%I',tab) INTO row_count;
  IF row_count<>0 THEN RAISE EXCEPTION 'SHIPPING_EMPTY_CONTEXT_READ %',tab; END IF;
 END LOOP;
 RESET ROLE;
END $shipping$;
ROLLBACK;
SELECT 'PASS: shipping tiers, amount/shape/FK guards, ISO syntax, RLS and runtime read-only' AS result;
