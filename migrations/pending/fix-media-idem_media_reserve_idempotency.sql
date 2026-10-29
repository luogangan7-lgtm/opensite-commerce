-- stable-id: fix-media-idem
-- depends-on: admin-ui-3-media-auth
-- Responsibility: give media reserve the existing shared C-04 command protocol.
-- Dependencies: reliability claims, media role and tenant authorization; caller:
-- forward migration runner. Invariants: only this operation in the current tenant,
-- no new table, no identity mutation or deletion, and existing UUID CHECKs stay.
GRANT EXECUTE ON FUNCTION app.idempotency_claim(uuid,text,uuid,text,text,bytea,interval,uuid) TO app_media_service;
-- The invoker-rights claim returns the complete record, requiring SELECT.
GRANT SELECT ON app.idempotency_records TO app_media_service;
GRANT INSERT(tenant_id,actor_kind,actor_id,operation,idempotency_key,request_hash,expires_at,claim_token,created_at)
 ON app.idempotency_records TO app_media_service;
GRANT UPDATE(claim_token,expires_at,state,response_status,response_body_reference,completed_at)
 ON app.idempotency_records TO app_media_service;
CREATE POLICY media_reserve_idempotency ON app.idempotency_records TO app_media_service
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND operation='createMediaUpload')
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid AND operation='createMediaUpload');
