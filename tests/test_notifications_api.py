#!/usr/bin/env python3
"""Notification admin contract gates, independent of generated/runtime handlers.

Reads OAS and registry via the existing contract validator. Mutations must reject
scope/operation drift and private fields; runtime auth is covered by Go tests.
"""
import copy
import json
from pathlib import Path
import sys
import unittest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))
import check_contract as contract


class NotificationAPITests(unittest.TestCase):
    def setUp(self):
        self.api = contract.load_api_text((ROOT / "api/openapi.yaml").read_text())

    def test_exact_scope_and_operations_are_required(self):
        contract.validate_contract(self.api)
        for name in ("listNotificationFailures", "redriveNotification"):
            for change in ("missing scope", "wrong scope", "missing operation", "planned", "wrong access", "missing error", "wrong response"):
                with self.subTest(name=name, change=change):
                    api = copy.deepcopy(self.api)
                    path, method, op = contract.operation_inventory(api)[name]
                    if change == "missing scope":
                        op.pop("x-required-scope")
                    elif change == "wrong scope":
                        op["x-required-scope"] = "catalog:write"
                    elif change == "missing operation":
                        del api["paths"][path][method]
                    elif change == "planned":
                        op["x-opensite-status"] = "planned"
                    elif change == "wrong access":
                        op["x-opensite-access"] = "AdminSession"
                    elif change == "missing error":
                        op["responses"].pop("404")
                    else:
                        op["responses"]["200"]["content"]["application/json"]["schema"] = {"$ref": "#/components/schemas/Problem"}
                    with self.assertRaises((AssertionError, ValueError)):
                        contract.validate_contract(api)

    def test_private_fields_and_unbounded_inputs_are_rejected(self):
        for change in ("contact", "body", "limit", "key", "unsafe error"):
            with self.subTest(change=change):
                api = copy.deepcopy(self.api)
                schemas = api["components"]["schemas"]
                if change in {"contact", "body"}:
                    schemas["NotificationFailure"]["properties"][change] = {"type": "string"}
                elif change == "limit":
                    op = contract.operation_inventory(api)["listNotificationFailures"][2]
                    op["parameters"][0]["schema"]["maximum"] = 1000
                elif change == "key":
                    api["components"]["parameters"]["NotificationIdempotencyKey"]["schema"].pop("format")
                else:
                    schemas["NotificationFailure"]["properties"]["safe_error"].pop("pattern")
                with self.assertRaises((AssertionError, ValueError)):
                    contract.validate_contract(api)

    def test_registry_matches_both_notification_operations(self):
        registry = json.loads((ROOT / "modules/registry.yaml").read_text())
        module = next(m for m in registry["modules"] if m["name"] == "notifications")
        self.assertEqual(module["owns_tables"], ["notification_deliveries", "notification_redrive_requests"])
        self.assertEqual(set(module["dependencies"]), {"tenant", "reliability", "orders", "customer"})
        self.assertEqual(module["scopes"], ["notifications:write"])
        self.assertEqual({op["operation_id"] for op in module["operations"]},
                         {"listNotificationFailures", "redriveNotification"})
        for op in module["operations"]:
            self.assertEqual(op["status"], "implemented")
            self.assertEqual(contract.operation_inventory(self.api)[op["operation_id"]][2]["x-opensite-status"], "implemented")


if __name__ == "__main__":
    unittest.main()
