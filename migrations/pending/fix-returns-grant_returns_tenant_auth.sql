-- stable-id: fix-returns-grant
-- depends-on: c02c-tenant-auth
-- depends-on: c20a-fulfillment
-- Responsibility: let the isolated returns coordinator revalidate staff authority.
-- Dependencies: tenant boolean credential capability and returns role; migration runner.
-- Invariants: exact EXECUTE only; no credential/table grants or membership changes.
GRANT EXECUTE ON FUNCTION app.auth_revalidate_tenant(uuid,uuid,text,uuid,text) TO app_returns_service;

-- CI-only fault probe: remove the exact capability under regression.
REVOKE EXECUTE ON FUNCTION app.auth_revalidate_tenant(uuid,uuid,text,uuid,text) FROM app_returns_service;
