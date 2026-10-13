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
SELF_OPERATIONS = {"deleteAdminSession", "getCurrentActor", "listAdminSessions", "revokeAdminSession"}
COOKIE = [{"adminSession": []}]
COOKIE_OR_KEY = [{"adminSession": []}, {"ApiKeyBearer": []}]


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
    require(SELF_OPERATIONS | {"createAdminSession", "health"} <= operations.keys(), "identity/health operation missing")
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
        if access == "Public":
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
                require(ident in {"health", "createAdminSession", "submitInquiry"} and security == [] and
                        "x-required-scope" not in op and not tenant, f"public operation must be anonymous and scope-free: {ident}")
        elif access == "AuthenticatedSelf":
            require(ident in SELF_OPERATIONS and not tenant and "tenant_id" not in names and "x-required-scope" not in op,
                    f"AuthenticatedSelf cannot carry scope/tenant: {ident}")
            require(security == COOKIE, f"AuthenticatedSelf requires only adminSession: {ident}")
        elif access == "AdminSession":
            require(security == COOKIE and isinstance(scope, str) and bool(scope.strip()), f"admin session/scope required: {ident}")
        elif access == "AdminOrAPIKey":
            require(tenant and isinstance(scope, str) and bool(scope.strip()), f"business tenant/scope required: {ident}")
            require(security in (COOKIE_OR_KEY, list(reversed(COOKIE_OR_KEY))), f"cookie OR API key required: {ident}")
        elif access == "InternalService":
            # C-18a has no production storefront middleware. This exact trusted
            # resolver seam cannot expand to an arbitrary anonymous tenant API.
            require((ident, path, method, scope) == ("createCheckout", "/api/v1/t/{tenant_id}/checkout", "post", "checkout.submit") and
                    security == [{"StorefrontSession": []}, {"GuestCapability": []}],
                    "checkout requires the exact trusted storefront resolver contract")
            for name, cookie_name in (("StorefrontSession", "__Host-osc_customer_session"),
                                      ("GuestCapability", "__Host-osc_guest_capability")):
                scheme = schemes.get(name, {})
                require((scheme.get("type"), scheme.get("in"), scheme.get("name")) ==
                        ("apiKey", "cookie", cookie_name), "storefront credential scheme drift")
        else:
            raise ValueError(f"access policy missing or unsupported: {ident}")
        if ident in SELF_OPERATIONS:
            require(access == "AuthenticatedSelf", f"self identity access drift: {ident}")
        if access in {"AdminSession", "AdminOrAPIKey"}:
            require(tenant, f"private business operation requires tenant path: {ident}")
        if ident == "createAdminSession":
            require(access == "Public" and method == "post" and "x-required-scope" not in op, "login must be Public with pre-login CSRF")
        if ident == "health":
            require((path, method, access, op["x-opensite-status"]) == ("/healthz", "get", "Public", "implemented"), "health declaration drift")
            require(set(op["responses"]) == {"204"} and "content" not in op["responses"]["204"], "health is a bodyless 204")
            continue
        for status, name in PROBLEM_RESPONSES.items():
            require(op["responses"].get(status) == {"$ref": "#/components/responses/" + name}, f"missing shared {status} mapping: {ident}")
        if method in {"post", "put", "patch", "delete"} and (access != "Public" or ident == "createAdminSession"):
            headers = {p["name"]: p for p in parameters if p["in"] == "header"}
            required = access != "AdminOrAPIKey"
            for header in ("Origin", "X-CSRF-Token"):
                require(header in headers and headers[header].get("required", False) == required,
                        f"cookie write {header} contract missing/incorrect: {ident}")
        if any(p["name"] == "If-Match" for p in parameters):
            require({"412", "428"} <= op["responses"].keys(), f"If-Match mappings missing: {ident}")
        if any(p["name"] == "Idempotency-Key" for p in parameters):
            require({"409", "422"} <= op["responses"].keys(), f"idempotency mappings missing: {ident}")
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
