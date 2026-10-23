-- stable-id: fix-returns-grant
-- depends-on: c02c-tenant-auth
-- depends-on: c20a-fulfillment
-- Responsibility: let the isolated returns coordinator revalidate staff authority.
-- Dependencies: tenant boolean credential capability and returns role; migration runner.
-- Invariants: exact EXECUTE only; no credential/table grants or membership changes.
GRANT EXECUTE ON FUNCTION app.auth_revalidate_tenant(uuid,uuid,text,uuid,text) TO app_returns_service WITH GRANT OPTION;

-- CI-only fault probe: excessive delegation must fail the exact ACL gate.
