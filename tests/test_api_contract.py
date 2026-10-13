#!/usr/bin/env python3
"""Contract regression gate; check_all calls it, never a runtime auth proof.

Uses the real OpenAPI document and check_contract with independent negative
mutations. A2/A3/A4 require cookie-or-key, tenant-scoped business and self-only
session operations; no database, handler or production dependencies.
"""
import copy
from pathlib import Path
import sys
import json
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))
import check_contract as contract


def operations(api):
    return {operation["operationId"]: (path, method, operation)
            for path, item in api["paths"].items()
            for method, operation in item.items()
            if method in {"get", "post", "put", "patch", "delete", "options", "head", "trace"}}


class APIContractTests(unittest.TestCase):
    def setUp(self):
        self.api = yaml.safe_load((ROOT / "api/openapi.yaml").read_text())

    def test_cart_is_strict_storefront_contract(self):
        ops = operations(self.api)
        for name in ("createCart", "getCart", "setCartLine", "deleteCartLine"):
            _, method, op = ops[name]
            self.assertEqual(op["x-opensite-access"], "Storefront")
            self.assertEqual(op["x-opensite-status"], "implemented")
            self.assertNotIn("x-required-scope", op)
            params = [contract.resolve(self.api, p["$ref"]) if "$ref" in p else p
                      for p in op.get("parameters", [])]
            headers = {p["name"]: p for p in params if p["in"] == "header"}
            self.assertEqual("If-Match" in headers, method in {"put", "delete"})
            if "If-Match" in headers:
                self.assertTrue(headers["If-Match"]["required"])
            for code in ("400", "404", "503"):
                self.assertIn(code, op["responses"])
        for name in ("CreateCart", "SetCartLine", "Cart", "CartLine"):
            self.assertFalse(self.api["components"]["schemas"][name]["additionalProperties"])

    def test_cart_absolute_quantity_schema(self):
        validator = contract.Draft202012Validator(self.api["components"]["schemas"]["SetCartLine"])
        for value in (0, 1, 1000000):
            validator.validate({"quantity": value})
        for body in ({}, {"quantity": None}, {"quantity": -1}, {"quantity": 1000001}, {"quantity": 1.5}):
            with self.subTest(body=body), self.assertRaises(contract.ValidationError):
                validator.validate(body)

    def test_cart_unavailable_quote_is_recoverable_view(self):
        schemas = self.api["components"]["schemas"]
        line = {"sku_id": "33333333-3333-4333-8333-333333333333", "quantity": 1,
                "available_quantity": 2, "sellable": True, "pricing_error": "price_unavailable"}
        cart = {"id": "22222222-2222-4222-8222-222222222222", "currency": "USD",
                "version": 1, "lines": [line], "pricing_error": "price_unavailable"}
        validator = contract.Draft202012Validator(
            {"$ref": "#/components/schemas/Cart", "components": {"schemas": schemas}},
            format_checker=contract.FormatChecker())
        validator.validate(cart)
        cart["pricing_error"] = line["pricing_error"] = "amount_overflow"
        validator.validate(cart)
        line["pricing_error"] = "unknown_error"
        with self.assertRaises(contract.ValidationError):
            validator.validate(cart)
        line.pop("pricing_error")
        line.update(unit_amount=0, line_amount=0, price_list_id="44444444-4444-4444-8444-444444444444",
                    priority=0, min_quantity=1)
        cart.pop("pricing_error")
        cart["subtotal"] = 0
        validator.validate(cart)
        # Priced and failed shapes are mutually exclusive alternatives.
        impossible = [
            lambda c: c.pop("subtotal"),
            lambda c: c.update(pricing_error="price_unavailable"),
            lambda c: c["lines"][0].pop("unit_amount"),
            lambda c: c["lines"][0].update(pricing_error="price_unavailable"),
            lambda c: c.update(lines=[c["lines"][0]] * 1001),
        ]
        for index, mutate in enumerate(impossible):
            broken = json.loads(json.dumps(cart))
            mutate(broken)
            with self.subTest(impossible=index), self.assertRaises(contract.ValidationError):
                validator.validate(broken)
        self.assertEqual(set(schemas["CartLine"]["required"]), {"sku_id", "quantity", "available_quantity", "sellable"})
        self.assertEqual(set(schemas["Cart"]["required"]), {"id", "currency", "version", "lines"})

    def test_published_product_slug_fits_safe_path(self):
        # Review thread rsvIP: Project rejects "/products/"+slug paths over 512 bytes.
        slug = self.api["components"]["schemas"]["PublishedProduct"]["properties"]["slug"]
        self.assertEqual(slug.get("maxLength"), 512 - len("/products/"))

    def test_cookie_and_bearer_are_alternatives(self):
        self.assertEqual(self.api["security"], [{"adminSession": []}, {"ApiKeyBearer": []}])
        cookie = self.api["components"]["securitySchemes"]["adminSession"]
        self.assertEqual((cookie["type"], cookie["in"], cookie["name"]),
                         ("apiKey", "cookie", "__Host-osc_admin_session"))

    def test_planned_identity_and_implemented_foundation(self):
        ops = operations(self.api)
        for name in ("createAdminSession", "deleteAdminSession", "getCurrentActor",
                     "listAdminSessions", "revokeAdminSession"):
            with self.subTest(name=name):
                self.assertIn(name, ops)
                self.assertEqual(ops[name][2]["x-opensite-status"], "planned")
        self.assertEqual(ops["getPublishedSite"][:2], ("/api/v1/public/sites/current", "get"))
        self.assertEqual(ops["getPublishedSite"][2]["x-opensite-access"], "Public")
        self.assertEqual(ops["getPublishedSite"][2]["security"], [])
        self.assertEqual(ops["health"][:2], ("/healthz", "get"))
        self.assertEqual(set(ops["health"][2]["responses"]), {"204"})
        self.assertNotIn("content", ops["health"][2]["responses"]["204"])
        expected = {"health", "getPublishedSite", "createProduct", "listProducts", "getProduct", "updateProduct", "archiveProduct",
                    "createVariant", "listVariants", "getVariant", "updateVariant", "archiveVariant",
                    "createMediaUpload", "putMediaUploadContent", "completeMediaUpload", "linkProductMedia", "deleteMedia"}
        cart = {"createCart", "getCart", "setCartLine", "deleteCartLine"}
        quote_operations = {"submitInquiry", "createQuote", "getQuote", "sendQuote", "withdrawQuote", "expireQuote", "getBuyerQuote", "acceptQuote", "approveQuote", "rejectQuote"}
        orders = {"listOrders", "getOrder"}
        self.assertEqual({name for name, (_, _, op) in ops.items()
                          if op["x-opensite-status"] == "implemented"}, expected | cart | quote_operations | orders |
                         {"listNotificationFailures", "redriveNotification", "getAdminLoginChallenge",
                          "createStaffLoginSession", "logoutCurrentStaffSession", "getAdminSession"})
        for name in orders:
            path, method, operation = ops[name]
            self.assertTrue(path.startswith("/api/v1/t/{tenant_id}/orders"))
            self.assertEqual((method, operation["x-opensite-access"], operation["x-required-scope"]),
                             ("get", "AdminOrAPIKey", "orders:read"))
        for name in expected - {"health", "getPublishedSite"}:
            path, method, operation = ops[name]
            self.assertEqual(operation["x-opensite-access"], "AdminOrAPIKey")
            self.assertIn(operation["x-required-scope"], {"catalog:read", "catalog:write", "media:write"})
            if method != "get":
                self.assertIn({"$ref": "#/components/parameters/IdempotencyKey"}, operation["parameters"])
            if name in {"updateProduct", "archiveProduct", "updateVariant", "archiveVariant"}:
                self.assertIn({"$ref": "#/components/parameters/IfMatch"}, operation["parameters"])


    def test_all_business_paths_include_tenant_and_allow_cookie_or_key(self):
        ops = operations(self.api)
        self.assertEqual(ops["createProduct"][0], "/api/v1/t/{tenant_id}/products")
        self.assertEqual(ops["createOrganization"][0], "/api/v1/t/{tenant_id}/organizations")
        self.assertEqual(ops["createOrganization"][2].get("security", self.api["security"]),
                         [{"adminSession": []}, {"ApiKeyBearer": []}])
        self.assertEqual(ops["createOrganization"][2]["x-opensite-access"], "AdminOrAPIKey")
        self.assertEqual(ops["createOrganization"][2]["x-required-scope"], "organization.create")
        self.assertEqual(ops["submitInquiry"][2]["security"], [])

    def test_quotes_have_cas_scope_and_trusted_buyer_contract(self):
        ops = operations(self.api)
        for name in ("createQuote", "getQuote", "sendQuote", "withdrawQuote", "expireQuote"):
            path, method, op = ops[name]
            self.assertTrue(path.startswith("/api/v1/t/{tenant_id}/quotes"))
            self.assertEqual(op["x-opensite-access"], "AdminOrAPIKey")
            self.assertEqual(op["x-required-scope"], "quotes:read" if method == "get" else "quotes:write")
        for name in ("getBuyerQuote", "acceptQuote", "approveQuote", "rejectQuote"):
            path, method, op = ops[name]
            self.assertTrue(path.startswith("/api/public/v1/quotes/{quote_id}"))
            self.assertEqual(op["x-opensite-trusted-buyer"], "required")
            self.assertEqual(op["security"], [])
            self.assertNotIn("x-required-scope", op)
        for name in ("createQuote", "sendQuote", "withdrawQuote", "expireQuote", "acceptQuote", "approveQuote", "rejectQuote"):
            params = ops[name][2]["parameters"]
            self.assertIn({"$ref": "#/components/parameters/IfMatch"}, params)
            self.assertIn({"$ref": "#/components/parameters/IdempotencyKey"}, params)
            self.assertIn("428", ops[name][2]["responses"])
            self.assertIn("412", ops[name][2]["responses"])
        inquiry = self.api["components"]["schemas"]["CreateInquiry"]
        self.assertEqual(set(inquiry["properties"]), {"product_id", "buyer_email", "buyer_name", "quantity", "message"})
        self.assertFalse(inquiry["additionalProperties"])

    def test_quote_operation_status_and_presence_mutations_are_rejected(self):
        names = ("submitInquiry", "createQuote", "getQuote", "sendQuote", "withdrawQuote",
                 "expireQuote", "getBuyerQuote", "acceptQuote", "approveQuote", "rejectQuote")
        for name in names:
            with self.subTest(name=name, mutation="planned"):
                api = copy.deepcopy(self.api)
                operations(api)[name][2]["x-opensite-status"] = "planned"
                with self.assertRaisesRegex(ValueError, "quote operation status drift"):
                    contract.validate_contract(api)
            with self.subTest(name=name, mutation="missing"):
                api = copy.deepcopy(self.api)
                path, method, _ = operations(api)[name]
                del api["paths"][path][method]
                with self.assertRaisesRegex(ValueError, "quote operation missing"):
                    contract.validate_contract(api)

    def test_self_identity_has_no_scope_or_tenant(self):
        ops = operations(self.api)
        for name in ("deleteAdminSession", "getCurrentActor", "listAdminSessions", "revokeAdminSession"):
            path, _, op = ops[name]
            self.assertNotIn("{tenant_id}", path)
            self.assertNotIn("x-required-scope", op)
            self.assertEqual(op["x-opensite-access"], "AuthenticatedSelf")
            self.assertEqual(op["security"], [{"adminSession": []}])

    def test_checker_accepts_current_contract(self):
        contract.validate_contract(self.api)

    def test_auth_path_status_and_error_mutations_are_rejected(self):
        def op(api, name):
            return operations(api)[name][2]

        mutations = {
            "buyer missing trusted marker": lambda a: op(a, "acceptQuote").pop("x-opensite-trusted-buyer"),
            "trusted marker outside buyer": lambda a: op(a, "submitInquiry").update({"x-opensite-trusted-buyer": "required"}),
            "seller quote wrong scope": lambda a: op(a, "sendQuote").update({"x-required-scope": "quotes:read"}),
            "quote missing IfMatch": lambda a: op(a, "acceptQuote").update(parameters=[p for p in op(a, "acceptQuote")["parameters"] if p.get("$ref") != "#/components/parameters/IfMatch"]),
            "quote missing IdempotencyKey": lambda a: op(a, "createQuote").update(parameters=[p for p in op(a, "createQuote")["parameters"] if p.get("$ref") != "#/components/parameters/IdempotencyKey"]),
            "cart pricing subtotal required": lambda a: a["components"]["schemas"]["Cart"]["required"].append("subtotal"),
            "cart pricing unit amount required": lambda a: a["components"]["schemas"]["CartLine"]["required"].append("unit_amount"),
            "cart pricing error enum drift": lambda a: a["components"]["schemas"]["CartLine"]["properties"]["pricing_error"].update(enum=["unknown"]),
            "cart quantity upper bound missing": lambda a: a["components"]["schemas"]["SetCartLine"]["properties"]["quantity"].pop("maximum"),
            "cart integer bound missing": lambda a: a["components"]["schemas"]["Cart"]["properties"]["subtotal"].pop("minimum"),
            "cart staff access": lambda a: op(a, "getCart").update({"x-opensite-access": "AdminOrAPIKey"}),
            "cart staff scope": lambda a: op(a, "getCart").update({"x-required-scope": "catalog.read"}),
            "cart missing IfMatch": lambda a: op(a, "setCartLine").update(parameters=[p for p in op(a,"setCartLine")["parameters"] if p.get("$ref") != "#/components/parameters/IfMatch"]),
            "cart unbounded schema": lambda a: a["components"]["schemas"]["SetCartLine"].update(additionalProperties=True),
            "cart token cookie": lambda a: a["components"]["securitySchemes"]["CartToken"].update(**{"in":"cookie"}),
            "cart missing 503": lambda a: op(a,"createCart")["responses"].pop("503"),
            "cookie AND bearer": lambda a: a.update(security=[{"adminSession": [], "ApiKeyBearer": []}]),
            "cookie name drift": lambda a: a["components"]["securitySchemes"]["adminSession"].update(name="session"),
            "tenant scope missing": lambda a: op(a, "createProduct").pop("x-required-scope"),
            "tenant public": lambda a: op(a, "createProduct").update(security=[], **{"x-opensite-access": "Public"}),
            "self scope": lambda a: op(a, "getCurrentActor").update({"x-required-scope": "catalog.read"}),
            "self empty scope": lambda a: op(a, "getCurrentActor").update({"x-required-scope": ""}),
            "self bearer": lambda a: op(a, "getCurrentActor").update(security=[{"ApiKeyBearer": []}]),
            "anonymous organization": lambda a: op(a, "createOrganization").update(security=[]),
            "organization bearer only loses browser auth": lambda a: op(a, "createOrganization").update(security=[{"ApiKeyBearer": []}]),
            "missing 401": lambda a: op(a, "createProduct")["responses"].pop("401"),
            "wrong Problem media": lambda a: a["components"]["responses"]["Unauthorized"].update(content={"application/json": {"schema": {"type": "object"}}}),
            "unknown status": lambda a: op(a, "getCurrentActor").update({"x-opensite-status": "unknown"}),
            "login skips csrf": lambda a: op(a, "createAdminSession").update(parameters=[]),
            "staff login skips csrf": lambda a: op(a, "createStaffLoginSession").update(parameters=[]),
            "staff challenge optional": lambda a: a["components"]["parameters"]["LoginChallengeCookie"].update(required=False),
            "staff login status planned": lambda a: op(a, "createStaffLoginSession").update({"x-opensite-status": "planned"}),
            "staff self bearer": lambda a: op(a, "getAdminSession").update(security=[{"ApiKeyBearer": []}]),
            "staff self scope": lambda a: op(a, "logoutCurrentStaffSession").update({"x-required-scope": "identity.write"}),
            "cookie write headers missing": lambda a: op(a, "createProduct").update(parameters=[]),
            "cookie-only headers unconditional": lambda a: a["components"]["parameters"]["CSRFTokenIfCookie"].update(required=True),
            "responses missing": lambda a: op(a, "createProduct").pop("responses"),
            "responses empty": lambda a: op(a, "createProduct").update(responses={}),
            "tenant not uuid": lambda a: a["components"]["parameters"]["TenantId"].update(schema={"type": "string"}),
            "duplicate operation id": lambda a: op(a, "createProduct").update(operationId="createSite"),
        }
        for label, mutate in mutations.items():
            with self.subTest(label=label):
                api = copy.deepcopy(self.api)
                mutate(api)
                with self.assertRaises((AssertionError, ValueError)):
                    contract.validate_contract(api)

    def test_operation_parameter_overrides_path_item(self):
        # OAS 3.1 Operation.parameters override path-level ones with the same (name, in).
        def override(schema):
            api = copy.deepcopy(self.api)
            op = operations(api)["createProduct"][2]
            op.setdefault("parameters", []).append(
                {"name": "tenant_id", "in": "path", "required": True, "schema": schema})
            return api
        with self.assertRaises((AssertionError, ValueError)):
            contract.validate_contract(override({"type": "string"}))
        contract.validate_contract(override({"type": "string", "format": "uuid"}))

    def test_legacy_tenant_path_is_rejected(self):
        for suffix in ("products", "organizations"):
            with self.subTest(suffix=suffix):
                api = copy.deepcopy(self.api)
                api["paths"]["/api/v1/" + suffix] = api["paths"].pop("/api/v1/t/{tenant_id}/" + suffix)
                with self.assertRaises((AssertionError, ValueError)):
                    contract.validate_contract(api)

    def test_duplicate_yaml_keys_are_rejected(self):
        with self.assertRaises(ValueError):
            contract.load_api_text("openapi: 3.1.2\npaths: {}\npaths: {}\n")

    def test_oas31_union_discriminator_and_scalar_types_preserved(self):
        schemas = self.api["components"]["schemas"]
        page_op = schemas["PageOperation"]
        self.assertEqual(len(page_op["oneOf"]), 3)
        self.assertEqual(page_op["discriminator"]["propertyName"], "type")
        self.assertEqual(schemas["ComponentAdd"]["properties"]["type"],
                         {"type": "string", "const": "component.add"})
        self.assertEqual(schemas["PageDocument"]["properties"]["schema_version"],
                         {"type": "integer", "const": 1})
        self.assertEqual(schemas["Site"]["properties"]["published_release_id"]["type"], ["string", "null"])


if __name__ == "__main__":
    unittest.main()
