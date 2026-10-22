-- stable-id: c20a-fulfillment-scopes
-- depends-on: c02c-tenant-auth
-- depends-on: c20a-fulfillment
-- Responsibility: project C-20a scopes into the existing tenant authority fence.
-- Dependencies: tenant auth capabilities; consumed by issuance and write rechecks.
-- Invariants: only owner/admin may fulfill or initiate refunds, explicit key
-- scopes remain mandatory, and CREATE OR REPLACE preserves the owner and ACL.
-- Compatible with existing data; rollback requires a reviewed forward migration.
CREATE OR REPLACE FUNCTION app.auth_role_scopes(p_role text) RETURNS text[]
LANGUAGE sql IMMUTABLE SECURITY INVOKER SET search_path=pg_catalog,app,pg_temp AS $scope$
 SELECT CASE p_role
 WHEN 'owner' THEN ARRAY['organization.create','site.create','catalog:read','catalog:write','media:write','cms:write','cms:publish','quotes:read','quotes:write','notifications:write','orders:read','apikeys:write','fulfillment:write','returns:write']
 WHEN 'admin' THEN ARRAY['organization.create','site.create','catalog:read','catalog:write','media:write','cms:write','cms:publish','quotes:read','quotes:write','notifications:write','orders:read','apikeys:write','fulfillment:write','returns:write']
 WHEN 'catalog_editor' THEN ARRAY['catalog:read','catalog:write','media:write']
 WHEN 'content_editor' THEN ARRAY['catalog:read','cms:write','cms:publish','media:write']
 WHEN 'viewer' THEN ARRAY['catalog:read','fulfillment:write'] ELSE ARRAY[]::text[] END;
$scope$;
REVOKE ALL ON FUNCTION app.auth_role_scopes(text) FROM PUBLIC;
