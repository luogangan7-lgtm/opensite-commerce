#!/usr/bin/env python3
"""C-20 transport contract gate; depends on OpenAPI and module metadata.

check_all calls this; opaque payment/account/amount inputs cannot enter refunds.
Runtime authorization, quantities and provider recovery have separate Go/PG gates.
"""
from pathlib import Path
import unittest
import yaml

ROOT = Path(__file__).resolve().parents[1]


class FulfillmentContractTests(unittest.TestCase):
    def setUp(self):
        self.api = yaml.safe_load((ROOT / "api/openapi.yaml").read_text())

    def test_four_scoped_versioned_idempotent_commands(self):
        ops = {op["operationId"]: (path, op) for path, item in self.api["paths"].items()
               for method, op in item.items() if method == "post"}
        for name, scope in (("createShipment", "fulfillment:write"),
                            ("createReturn", "returns:write"),
                            ("receiveReturn", "returns:write"),
                            ("refundReturn", "returns:write")):
            with self.subTest(name=name):
                self.assertIn(name, ops, "C20_CONTRACT_MISSING_COMMAND")
                path, op = ops[name]
                self.assertTrue(path.startswith("/api/v1/t/{tenant_id}/"))
                self.assertEqual(op["x-opensite-status"], "implemented")
                self.assertEqual(op["x-opensite-access"], "AdminOrAPIKey")
                self.assertEqual(op["x-required-scope"], scope)
                for header in ("IfMatch", "IdempotencyKey"):
                    self.assertIn({"$ref": "#/components/parameters/" + header}, op["parameters"])

    def test_refund_never_accepts_client_payment_or_account_proof(self):
        schema = self.api["components"]["schemas"].get("RefundReturn")
        self.assertIsNotNone(schema, "C20_CONTRACT_MISSING_REFUND")
        self.assertFalse(schema["additionalProperties"])
        self.assertEqual(schema["properties"], {})


if __name__ == "__main__":
    unittest.main()
