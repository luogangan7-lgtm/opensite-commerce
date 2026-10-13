#!/usr/bin/env python3
"""Validate OAS and A2/A3/A4 policy; check_all/tests call this import-safe gate.

Depends on pinned PyYAML, jsonschema and openapi-spec-validator; reads only the
API and page fixtures. Local refs, explicit access/status and cookie-or-key
prevent declaration drift; RouteSpec parity and runtime auth are separate gates.
"""
from pathlib import Path

import yaml
from jsonschema import Draft202012Validator, FormatChecker
from jsonschema.exceptions import ValidationError
from openapi_spec_validator import validate

ROOT = Path(__file__).resolve().parents[1]
HTTP_METHODS = {"get", "post", "patch", "put", "delete", "options", "head", "trace"}
PROBLEM_RESPONSES = {
    "401": "Unauthorized", "403": "Forbidden", "409": "Conflict",
    "412": "PreconditionFailed", "413": "PayloadTooLarge",
    "415": "UnsupportedMediaType", "422": "UnprocessableEntity",
    "428": "PreconditionRequired", "429": "TooManyRequests",
}
CART_OPERATIONS = {"createCart", "getCart", "setCartLine", "deleteCartLine"}
CART_PATHS = {
    "createCart": ("/api/v1/t/{tenant_id}/carts", "post"),
    "getCart": ("/api/v1/t/{tenant_id}/carts/{cart_id}", "get"),
    "setCartLine": ("/api/v1/t/{tenant_id}/carts/{cart_id}/lines/{sku_id}", "put"),
    "deleteCartLine": ("/api/v1/t/{tenant_id}/carts/{cart_id}/lines/{sku_id}", "delete"),
}
SELF_OPERATIONS = {"deleteAdminSession", "getCurrentActor", "listAdminSessions", "revokeAdminSession",
                   "logoutCurrentStaffSession", "getAdminSession"}
STAFF_OPERATIONS = {
    "getAdminLoginChallenge": ("/api/v1/auth/login", "get", "Public"),
    "createStaffLoginSession": ("/api/v1/auth/login", "post", "Public"),
    "logoutCurrentStaffSession": ("/api/v1/auth/logout", "post", "AuthenticatedSelf"),
    "getAdminSession": ("/api/v1/auth/session", "get", "AuthenticatedSelf"),
}
COOKIE = [{"adminSession": []}]
COOKIE_OR_KEY = [{"adminSession": []}, {"ApiKeyBearer": []}]
CUSTOMER_OPERATIONS = {
    'registerCustomer', 'verifyCustomerEmail', 'getCustomerSession',
    'createCustomerSession', 'deleteCustomerSession',
    'requestCustomerPasswordReset', 'confirmCustomerPasswordReset',
}
# Buyer identity is supplied by the customer-auth seam; Public here describes
# the platform router access class, never permission to bypass trusted actors.
BUYER_QUOTES = {
    "getBuyerQuote": ("get", "/api/public/v1/quotes/{quote_id}"),
    "acceptQuote": ("post", "/api/public/v1/quotes/{quote_id}/accept"),
    "approveQuote": ("post", "/api/public/v1/quotes/{quote_id}/approve"),
    "rejectQuote": ("post", "/api/public/v1/quotes/{quote_id}/reject"),
}
SELLER_QUOTES = {
    "createQuote": ("post", "/api/v1/t/{tenant_id}/quotes"),
    "getQuote": ("get", "/api/v1/t/{tenant_id}/quotes/{quote_id}"),
    "sendQuote": ("post", "/api/v1/t/{tenant_id}/quotes/{quote_id}/send"),
    "withdrawQuote": ("post", "/api/v1/t/{tenant_id}/quotes/{quote_id}/withdraw"),
    "expireQuote": ("post", "/api/v1/t/{tenant_id}/quotes/{quote_id}/expire"),
}
QUOTE_OPERATIONS = SELLER_QUOTES.keys() | BUYER_QUOTES.keys() | {"submitInquiry"}


class UniqueKeyLoader(yaml.SafeLoader):
    """A duplicate path/schema/security key must not silently replace its peer."""


def unique_mapping(loader, node):
    mapping = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node)
        if key in mapping:
            raise ValueError(f"duplicate YAML key: {key}")
        mapping[key] = loader.construct_object(value_node)
    return mapping


UniqueKeyLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, unique_mapping)


def load_api_text(text):
    api = yaml.load(text, Loader=UniqueKeyLoader)
    if not isinstance(api, dict):
        raise ValueError("OpenAPI document must be an object")
    return api


def load_api(path):
    return load_api_text(Path(path).read_text())


def resolve(api, pointer):
    if not isinstance(pointer, str) or not pointer.startswith("#/"):
        raise ValueError(f"external/invalid reference: {pointer}")
    item = api
    for segment in pointer[2:].split("/"):
        segment = segment.replace("~1", "/").replace("~0", "~")
        if not isinstance(item, dict) or segment not in item:
            raise ValueError(f"broken $ref: {pointer}")
        item = item[segment]
    return item


def references(value):
    if isinstance(value, dict):
        if "$ref" in value:
            yield value["$ref"]
        for child in value.values():
            yield from references(child)
    elif isinstance(value, list):
        for child in value:
            yield from references(child)


def require(condition, message):
    if not condition:
        raise ValueError(message)


def operation_inventory(api):
    operations = {}
    for path, item in api["paths"].items():
        for method, op in item.items():
            if method not in HTTP_METHODS:
                continue
            ident = op.get("operationId")
            require(isinstance(ident, str) and bool(ident.strip()), f"operationId missing: {method} {path}")
            require(ident not in operations, f"duplicate operationId: {ident}")
            operations[ident] = (path, method, op)
    return operations


def validate_contract(api):
    # Reject external references before the validator could fetch them.
    refs = list(references(api))
    for ref in refs:
        resolve(api, ref)
    try:
        validate(api)
    except Exception as error:
        raise ValueError(f"OpenAPI validation failed: {error}") from error
    require(api["openapi"].startswith("3.1."), "OAS 3.1 required")
    schemes = api["components"]["securitySchemes"]
    cookie = schemes.get("adminSession", {})
    require((cookie.get("type"), cookie.get("in"), cookie.get("name")) ==
            ("apiKey", "cookie", "__Host-osc_admin_session"), "adminSession cookie scheme drift")
    key = schemes.get("ApiKeyBearer", {})
    require(key.get("type") == "http" and key.get("scheme") == "bearer", "API key bearer scheme drift")
    require(api.get("security") in (COOKIE_OR_KEY, list(reversed(COOKIE_OR_KEY))), "cookie OR API key required")
    for name in PROBLEM_RESPONSES.values():
        content = api["components"]["responses"][name].get("content", {})
        require(content == {"application/problem+json": {"schema": {"$ref": "#/components/schemas/Problem"}}},
                f"Problem response mapping drift: {name}")

    operations = operation_inventory(api)
    require(SELF_OPERATIONS | {"createAdminSession", "health"} | STAFF_OPERATIONS.keys() <= operations.keys(), "identity/health operation missing")
    notification_routes = {
        "listNotificationFailures": ("/api/v1/t/{tenant_id}/notifications/failures", "get"),
        "redriveNotification": ("/api/v1/t/{tenant_id}/notifications/{notification_id}/redrive", "post"),
    }
    require(notification_routes.keys() <= operations.keys(), "notification operation missing")
    for ident, expected in notification_routes.items():
        path, method, op = operations[ident]
        require((path, method) == expected and op.get("x-opensite-status") == "implemented" and
                op.get("x-opensite-access") == "AdminOrAPIKey" and
                op.get("x-required-scope") == "notifications:write", f"notification route/scope/status drift: {ident}")
        require("requestBody" not in op, f"notification body forbidden: {ident}")
        response = "NotificationFailures" if ident == "listNotificationFailures" else "NotificationRedriveReceipt"
        require(op["responses"].get("200", {}).get("content") ==
                {"application/json": {"schema": {"$ref": "#/components/schemas/" + response}}},
                f"notification success response drift: {ident}")
        for code, name in {"400": "BadRequest", "404": "NotFound", "500": "InternalServerError", "503": "ServiceUnavailable"}.items():
            require(op["responses"].get(code) == {"$ref": "#/components/responses/" + name},
                    f"notification error response drift: {ident}")
    list_op = operations["listNotificationFailures"][2]
    limit = next((p for p in list_op.get("parameters", []) if p.get("name") == "limit"), {})
    require(limit.get("in") == "query" and limit.get("schema") ==
            {"type": "integer", "format": "int32", "minimum": 1, "maximum": 100, "default": 25},
            "notification limit must be bounded")
    key = api["components"]["parameters"].get("NotificationIdempotencyKey", {})
    require(key.get("name") == "Idempotency-Key" and key.get("in") == "header" and key.get("required") is True and
            key.get("schema") == {"type": "string", "minLength": 1, "maxLength": 128, "pattern": "^[!-~]+$"},
            "notification redrive key must be bounded printable ASCII")
    require({"$ref": "#/components/parameters/NotificationIdempotencyKey"} in
            operations["redriveNotification"][2].get("parameters", []), "notification redrive key missing")
    schemas = api["components"]["schemas"]
    failure = schemas["NotificationFailure"]
    require(failure.get("additionalProperties") is False and set(failure["properties"]) ==
            {"id", "event_id", "template_key", "status", "attempts", "generation", "created_at", "updated_at", "safe_error"},
            "notification failure must expose only safe fields")
    require(failure["properties"]["safe_error"] ==
            {"type": "string", "minLength": 1, "maxLength": 128, "pattern": "^[A-Z0-9_]+$"},
            "notification error must be a bounded safe code")
    require(failure["properties"]["status"].get("enum") == ["failed", "dead_letter", "uncertain"],
            "notification failure states drift")
    receipt = schemas["NotificationRedriveReceipt"]
    require(receipt.get("additionalProperties") is False and set(receipt["properties"]) ==
            {"notification_id", "token", "status", "generation"} and
            receipt["properties"]["status"].get("enum") == ["pending"], "notification receipt must expose only queued safe fields")

    for ident, (path, method, op) in operations.items():
        require(op.get("x-opensite-status") in {"planned", "implemented"}, f"explicit operation status required: {ident}")
        require(isinstance(op.get("responses"), dict) and bool(op["responses"]), f"nonempty responses required: {ident}")
        access = op.get("x-opensite-access")
        scope = op.get("x-required-scope")
        security = op.get("security", api["security"])
        # OAS 3.1: operation parameters override path-level ones with the same (name, in).
        effective = {}
        for p in api["paths"][path].get("parameters", []) + op.get("parameters", []):
            p = resolve(api, p["$ref"]) if "$ref" in p else p
            effective[(p["name"], p["in"])] = p
        parameters = list(effective.values())
        names = {p["name"] for p in parameters if p["in"] == "path"}
        tenant = "{tenant_id}" in path
        require(not any(n.lower().startswith("tenant") and n != "tenant_id" for n in names), f"tenant alias forbidden: {ident}")
        require(not tenant or path.startswith("/api/v1/t/{tenant_id}/"), f"tenant prefix required: {ident}")
        if tenant:
            tenant_parameter = next((p for p in parameters if p["name"] == "tenant_id" and p["in"] == "path"), {})
            require(tenant_parameter.get("required") is True and tenant_parameter.get("schema") ==
                    {"type": "string", "format": "uuid"}, f"required tenant UUID parameter missing: {ident}")
        if access == "Public" and ident == "getSitePreview":
            # Staff authentication is bypassed only for this read-only signed
            # capability. The publish module verifies the bearer before SQL.
            require(path == "/api/v1/t/{tenant_id}/sites/{site_id}/drafts/{draft_id}/preview" and
                    method == "get" and security == [] and "x-required-scope" not in op,
                    "signed preview access/path policy drift")
            headers = {p["name"]: p for p in parameters if p["in"] == "header"}
            require(headers.get("Authorization", {}).get("required") is True,
                    "signed preview bearer must be required")
            success_headers = op["responses"].get("200", {}).get("headers", {})
            require(success_headers.get("Cache-Control", {}).get("schema", {}).get("const") == "no-store" and
                    success_headers.get("X-Robots-Tag", {}).get("schema", {}).get("const") == "noindex, nofollow",
                    "signed preview cache/index policy drift")
        elif access == "Public":
            if ident == "receiveFakePaymentEvent":
                require((path, method) == ("/api/v1/testing/payments/fake/webhook", "post") and
                        op.get("x-test-only") is True and security == [{"FakeWebhookSignature": []}] and
                        "x-required-scope" not in op and not tenant,
                        "Fake webhook must be signed, test-only and outside tenant business paths")
                require(schemes.get("FakeWebhookSignature") == {
                    "type": "apiKey", "in": "header", "name": "X-Fake-Signature",
                    "description": "HMAC SHA256 of exact request bytes with isolated test key; test/development only."},
                    "Fake signature scheme drift")
                signature = next((p for p in parameters if p.get("name") == "X-Fake-Signature"), {})
                require(signature.get("in") == "header" and signature.get("required") is True and
                        signature.get("schema") == {"type": "string", "pattern": "^[0-9a-f]{64}$"},
                        "Fake signature parameter missing/incorrect")
            else:
                customer = ident in CUSTOMER_OPERATIONS
                require(ident in {"health", "createAdminSession", "submitInquiry", "getPublishedSite", "getAdminLoginChallenge", "createStaffLoginSession"} | BUYER_QUOTES.keys() | CUSTOMER_OPERATIONS and security == [] and
                        "x-required-scope" not in op and (tenant if customer else not tenant), f"public operation must be anonymous and scope-free: {ident}")
                if customer:
                    require(path.startswith('/api/v1/t/{tenant_id}/customer/') and op['x-opensite-status'] == 'implemented',
                            'customer authentication route drift')
        elif access == "Storefront":
            if ident == "createCheckout":
                require((path, method) == ("/api/v1/t/{tenant_id}/checkout", "post") and tenant and
                        "x-required-scope" not in op and op["x-opensite-status"] == "implemented" and
                        security == [{"StorefrontSession": []}, {"GuestCapability": []}],
                        "checkout requires the exact scope-free storefront resolver contract")
                for name, cookie_name in (("StorefrontSession", "__Host-osc_customer_session"),
                                          ("GuestCapability", "__Host-osc_guest_capability")):
                    scheme = schemes.get(name, {})
                    require((scheme.get("type"), scheme.get("in"), scheme.get("name")) ==
                            ("apiKey", "cookie", cookie_name), "storefront credential scheme drift")
                headers = {p["name"]: p for p in parameters if p["in"] == "header"}
                for header, schema in {
                        "Origin": {"type": "string", "format": "uri"},
                        "X-CSRF-Token": {"type": "string", "minLength": 1, "maxLength": 512}}.items():
                    require(header in headers and headers[header].get("required") is True and
                            headers[header].get("schema") == schema,
                            f"checkout {header} admission contract missing/incorrect")
                require("Idempotency-Key" in headers and headers["Idempotency-Key"].get("required") is True,
                        "checkout idempotency header required")
            else:
                require(ident in CART_OPERATIONS and tenant and path.startswith("/api/v1/t/{tenant_id}/carts") and
                        "x-required-scope" not in op, f"storefront cart tenant/scope policy drift: {ident}")
                require(security == ([] if ident == "createCart" else [{"CartToken": []}]),
                        f"storefront credential declaration drift: {ident}")
                headers = {p["name"]: p for p in parameters if p["in"] == "header"}
                require(("If-Match" in headers) == (method in {"put", "delete"}), f"cart write precondition drift: {ident}")
                require(method not in {"put", "delete"} or headers["If-Match"].get("required") is True,
                        f"required cart precondition drift: {ident}")
                for code, name in {"400": "BadRequest", "404": "NotFound", "503": "ServiceUnavailable"}.items():
                    require(op["responses"].get(code) == {"$ref": "#/components/responses/" + name},
                            f"cart error declaration drift: {ident}")
        elif access == "AuthenticatedSelf":
            require(ident in SELF_OPERATIONS and not tenant and "tenant_id" not in names and "x-required-scope" not in op,
                    f"AuthenticatedSelf cannot carry scope/tenant: {ident}")
            require(security == COOKIE, f"AuthenticatedSelf requires only adminSession: {ident}")
        elif access == "AdminSession":
            require(security == COOKIE and isinstance(scope, str) and bool(scope.strip()), f"admin session/scope required: {ident}")
        elif access == "AdminOrAPIKey":
            require(tenant and isinstance(scope, str) and bool(scope.strip()), f"business tenant/scope required: {ident}")
            require(security in (COOKIE_OR_KEY, list(reversed(COOKIE_OR_KEY))), f"cookie OR API key required: {ident}")
        else:
            raise ValueError(f"access policy missing or unsupported: {ident}")
        if ident == "createCheckout":
            require(access == "Storefront", "checkout cannot use employee or service authentication")
        if ident in QUOTE_OPERATIONS:
            require(op["x-opensite-status"] == "implemented", f"quote operation status drift: {ident}")
        if ident == "submitInquiry":
            require((path, method, access) == ("/api/public/v1/inquiries", "post", "Public"),
                    "public inquiry route/access drift")
        if ident in BUYER_QUOTES:
            require((method, path) == BUYER_QUOTES[ident], f"buyer quote route drift: {ident}")
            require(access == "Public" and op.get("x-opensite-trusted-buyer") == "required",
                    f"trusted buyer declaration required: {ident}")
        elif "x-opensite-trusted-buyer" in op:
            raise ValueError(f"trusted buyer marker outside buyer quote operation: {ident}")
        if ident in SELLER_QUOTES:
            require((method, path) == SELLER_QUOTES[ident], f"seller quote route drift: {ident}")
            require(access == "AdminOrAPIKey" and scope == ("quotes:read" if ident == "getQuote" else "quotes:write"),
                    f"seller quotes scope required: {ident}")
        if ident in SELLER_QUOTES.keys() | BUYER_QUOTES.keys() and method != "get":
            headers = {p["name"]: p for p in parameters if p["in"] == "header"}
            for header in ("If-Match", "Idempotency-Key"):
                require(headers.get(header, {}).get("required") is True,
                        f"quote write {header} required: {ident}")
        if ident in CART_OPERATIONS:
            require((path, method) == CART_PATHS[ident] and access == "Storefront" and
                    op["x-opensite-status"] == "implemented", f"cart route/access/status drift: {ident}")
        if ident in SELF_OPERATIONS:
            require(access == "AuthenticatedSelf", f"self identity access drift: {ident}")
        if access in {"AdminSession", "AdminOrAPIKey"}:
            require(tenant, f"private business operation requires tenant path: {ident}")
        if ident in {"createAdminSession", "createStaffLoginSession"}:
            require(access == "Public" and method == "post" and "x-required-scope" not in op, "login must be Public with pre-login CSRF")
        if ident in STAFF_OPERATIONS:
            require((path, method, access) == STAFF_OPERATIONS[ident] and op["x-opensite-status"] == "implemented",
                    f"implemented staff declaration drift: {ident}")
        if ident == "createStaffLoginSession":
            challenge = next((p for p in parameters if p["in"] == "cookie" and p["name"] == "__Host-osc_login_csrf"), {})
            require(challenge.get("required") is True, "staff login requires anonymous challenge cookie")
        if ident == "health":
            require((path, method, access, op["x-opensite-status"]) == ("/healthz", "get", "Public", "implemented"), "health declaration drift")
            require(set(op["responses"]) == {"204"} and "content" not in op["responses"]["204"], "health is a bodyless 204")
            continue
        for status, name in PROBLEM_RESPONSES.items():
            require(op["responses"].get(status) == {"$ref": "#/components/responses/" + name}, f"missing shared {status} mapping: {ident}")
        if method in {"post", "put", "patch", "delete"} and access != "Storefront" and (access != "Public" or ident in {"createAdminSession", "createStaffLoginSession"} | CUSTOMER_OPERATIONS):
            headers = {p["name"]: p for p in parameters if p["in"] == "header"}
            required = access != "AdminOrAPIKey"
            for header in ("Origin", "X-CSRF-Token"):
                require(header in headers and headers[header].get("required", False) == required,
                        f"cookie write {header} contract missing/incorrect: {ident}")
        if any(p["name"] == "If-Match" for p in parameters):
            require({"412", "428"} <= op["responses"].keys(), f"If-Match mappings missing: {ident}")
        if any(p["name"] == "Idempotency-Key" for p in parameters):
            require({"409", "422"} <= op["responses"].keys(), f"idempotency mappings missing: {ident}")
    require(QUOTE_OPERATIONS <= operations.keys(), "quote operation missing")
    require(CART_OPERATIONS <= operations.keys(), "cart operation missing")
    require(CUSTOMER_OPERATIONS <= operations.keys(), "customer authentication operation missing")
    customer_cookie = schemes.get("customerSession", {})
    require((customer_cookie.get("type"), customer_cookie.get("in"), customer_cookie.get("name")) ==
            ("apiKey", "cookie", "__Host-osc_customer_session"), "customer session cookie scheme drift")
    for name in ("CustomerRegistrationInput", "CustomerCredentialsInput", "CustomerTokenInput",
                 "CustomerResetRequest", "CustomerResetConfirm", "CustomerRegistrationReceipt",
                 "CustomerResetReceipt", "CustomerSessionView", "CustomerSessionEnvelope"):
        require(schemas.get(name, {}).get("additionalProperties") is False,
                f"strict customer schema required: {name}")
    cart_token = schemes.get("CartToken", {})
    require((cart_token.get("type"), cart_token.get("in"), cart_token.get("name")) ==
            ("apiKey", "header", "X-Cart-Token"), "cart capability scheme drift")
    for name in ("CreateCart", "SetCartLine", "Cart", "CartLine", "CreatedCart"):
        require(api["components"]["schemas"][name].get("additionalProperties") is False,
                f"strict cart schema required: {name}")
    schemas = api["components"]["schemas"]
    quantity = schemas["SetCartLine"]["properties"]["quantity"]
    require(quantity.get("type") == "integer" and quantity.get("minimum") == 0 and
            quantity.get("maximum") == 1000000 and schemas["SetCartLine"]["required"] == ["quantity"],
            "cart absolute quantity bounds/presence drift")
    for name, required in {
            "Cart": {"id", "currency", "version", "lines"},
            "CartLine": {"sku_id", "quantity", "available_quantity", "sellable"}}.items():
        require(set(schemas[name]["required"]) == required, f"recoverable cart required fields drift: {name}")
        require(schemas[name]["properties"].get("pricing_error", {}).get("enum") ==
                ["price_unavailable", "amount_overflow"], f"cart pricing error enum drift: {name}")
    for name in ("Cart", "CartLine"):
        for field, schema in schemas[name]["properties"].items():
            if schema.get("type") == "integer":
                require(isinstance(schema.get("minimum"), int) and isinstance(schema.get("maximum"), int),
                        f"bounded cart integer required: {name}.{field}")
    for name in ("BadRequest", "NotFound", "ServiceUnavailable"):
        require(api["components"]["responses"][name].get("content") ==
                {"application/problem+json": {"schema": {"$ref": "#/components/schemas/Problem"}}},
                f"cart Problem media drift: {name}")
    for schema in api["components"]["schemas"].values():
        Draft202012Validator.check_schema(schema)
    return {"operations": len(operations), "references": len(refs), "schemas": len(api["components"]["schemas"])}


def validate_page_fixtures(api):
    schemas = api["components"]["schemas"]
    validator = Draft202012Validator({"$ref": "#/components/schemas/PageDocument", "components": {"schemas": schemas}},
                                    format_checker=FormatChecker())
    validator.validate(yaml.safe_load((ROOT / "fixtures/home-valid.json").read_text()))
    try:
        validator.validate(yaml.safe_load((ROOT / "fixtures/home-invalid.json").read_text()))
    except ValidationError:
        return
    raise ValueError("malicious component not rejected by schema")


def main():
    api = load_api(ROOT / "api/openapi.yaml")
    counts = validate_contract(api)
    validate_page_fixtures(api)
    print(f"PASS: full OAS 3.1 validation; {counts['operations']} operations; {counts['references']} local refs; "
          f"{counts['schemas']} JSON Schemas; A2/A3/A4 auth/path/error declarations; page fixtures.")
    print("NOT CHECKED: real identity/business handlers, runtime authorization or PostgreSQL.")


if __name__ == "__main__":
    main()
