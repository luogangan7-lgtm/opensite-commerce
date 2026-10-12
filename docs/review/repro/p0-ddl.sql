-- Repro for REVIEW.md F-03 / F-04 on PostgreSQL 18. Run: psql -v ON_ERROR_STOP=0 -f p0-ddl.sql
\echo '== F-03 report DDL: two created payments (provider_payment_id NULL), same tenant+provider'
CREATE TABLE pay_report (tenant_id uuid, provider text, provider_payment_id text,
  UNIQUE NULLS NOT DISTINCT (tenant_id, provider, provider_payment_id));
INSERT INTO pay_report VALUES ('00000000-0000-0000-0000-000000000001','stripe',NULL);
INSERT INTO pay_report VALUES ('00000000-0000-0000-0000-000000000001','stripe',NULL); -- EXPECT ERROR (bug)
\echo '== F-03 fix: partial unique index'
CREATE TABLE pay_fix (tenant_id uuid, provider text, provider_payment_id text);
CREATE UNIQUE INDEX ON pay_fix (tenant_id, provider, provider_payment_id) WHERE provider_payment_id IS NOT NULL;
INSERT INTO pay_fix VALUES ('00000000-0000-0000-0000-000000000001','stripe',NULL),('00000000-0000-0000-0000-000000000001','stripe',NULL); -- EXPECT OK
INSERT INTO pay_fix VALUES ('00000000-0000-0000-0000-000000000001','stripe','pi_1'),('00000000-0000-0000-0000-000000000001','stripe','pi_1'); -- EXPECT ERROR (correct)
\echo '== F-04 report policy expr after SET LOCAL on same connection'
BEGIN; SET LOCAL app.tenant_id = '00000000-0000-0000-0000-000000000001'; COMMIT;
SELECT current_setting('app.tenant_id', true) = '' AS reset_to_empty_string;
SELECT current_setting('app.tenant_id', true)::uuid; -- EXPECT ERROR (bug)
\echo '== F-04 fix'
SELECT nullif(current_setting('app.tenant_id', true), '')::uuid IS NULL AS fix_yields_null; -- EXPECT t
