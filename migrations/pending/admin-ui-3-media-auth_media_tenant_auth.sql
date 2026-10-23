-- stable-id: admin-ui-3-media-auth
-- depends-on: c02c-tenant-auth
-- Responsibility: let the isolated media service revalidate staff authority.
-- Dependencies: c02c tenant credential capability; caller: forward migration runner.
-- Invariants: boolean authority only; no credential reads, DML or new scopes.
GRANT EXECUTE ON FUNCTION app.auth_revalidate_tenant(uuid,uuid,text,uuid,text) TO app_media_service;
