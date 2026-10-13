#!/usr/bin/env python3
"""Contract tests for the explicit column registry and canary coverage gate.

Depends only on Python stdlib and scripts/privacy_registry.py. CI check_all
calls this file; these small synthetic declarations test failure boundaries,
while the PG runner must validate the migrated catalog and real fixture cells.
"""
import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]


def personal(classification="personal", **extra):
    return {
        "classification": classification,
        "erase": {"strategy": "scrub"},
        "export": "include",
        **extra,
    }


def document():
    return {
        "version": 1,
        "module": "orders",
        "structures": {
            "PostalAddress": "Nested postal address snapshot, including contact text.",
            "ReceiptBytes": "UTF-8 encoded receipt containing a contact canary.",
        },
        "tables": {
            "orders": {
                "subject": {
                    "tenant_key": "tenant_id",
                    "identity_path": "orders.customer_id -> customers.id",
                },
                "columns": {
                    "id": {"classification": "operational"},
                    "guest_email": personal("contact"),
                    "address_snapshot": personal(structure="PostalAddress"),
                    "receipt": personal(structure="ReceiptBytes"),
                },
            },
        },
    }


def catalog():
    return [
        {"table_name": "orders", "column_name": "id", "data_type": "uuid", "udt_name": "uuid"},
        {"table_name": "orders", "column_name": "guest_email", "data_type": "text", "udt_name": "text"},
        {"table_name": "orders", "column_name": "address_snapshot", "data_type": "jsonb", "udt_name": "jsonb"},
        {"table_name": "orders", "column_name": "receipt", "data_type": "bytea", "udt_name": "bytea"},
    ]


class PrivacyRegistryTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        script = ROOT / "scripts/privacy_registry.py"
        if not script.is_file():
            raise AssertionError("PRIVACY_REGISTRY_IMPLEMENTATION_MISSING: scripts/privacy_registry.py")
        spec = importlib.util.spec_from_file_location("privacy_registry_under_test", script)
        cls.gate = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cls.gate)

    def load(self, docs=None, raw=None):
        with tempfile.TemporaryDirectory(prefix="privacy-registry-test-") as tmp:
            root = Path(tmp)
            directory = root / "modules/privacy"
            directory.mkdir(parents=True)
            if raw is not None:
                (directory / "orders.json").write_text(raw)
            else:
                for name, body in (docs or {"orders": document()}).items():
                    (directory / f"{name}.json").write_text(json.dumps(body))
            return self.gate.load_registry(root)

    def test_valid_registry_catalog_and_real_cells(self):
        registry = self.load()
        self.assertEqual(set(registry), {
            "orders.id", "orders.guest_email", "orders.address_snapshot", "orders.receipt",
        })
        self.gate.validate_catalog(registry, catalog())
        template = (
            "SELECT {{canary:orders.guest_email}} || '@canary.invalid', "
            "jsonb_build_object('nested', jsonb_build_array({{canary:orders.address_snapshot}})), "
            "convert_to({{canary:orders.receipt}}, 'UTF8');"
        )
        sql, canaries = self.gate.render_fixture(registry, template, "validrun")
        self.assertNotIn("{{canary:", sql)
        self.assertEqual(set(canaries), {
            "orders.guest_email", "orders.address_snapshot", "orders.receipt",
        })
        self.assertEqual(len(set(canaries.values())), 3, "Every PII column needs a distinct canary")
        for canary in canaries.values():
            self.assertIn("'" + canary + "'", sql, "Substitution must produce a SQL text literal")
        self.gate.validate_fixture(registry, {
            "orders.guest_email": [canaries["orders.guest_email"] + "@canary.invalid"],
            "orders.address_snapshot": [{"nested": [canaries["orders.address_snapshot"]]}],
            "orders.receipt": [canaries["orders.receipt"].encode("utf-8")],
        }, canaries)

    def test_catalog_rejects_undeclared_actual_column(self):
        registry = self.load()
        rows = catalog() + [{
            "table_name": "orders", "column_name": "new_unclassified_column",
            "data_type": "text", "udt_name": "text",
        }]
        with self.assertRaisesRegex(ValueError, "orders\\.new_unclassified_column"):
            self.gate.validate_catalog(registry, rows)

    def test_catalog_rejects_declared_nonexistent_column(self):
        with self.assertRaisesRegex(ValueError, "orders\\.receipt"):
            self.gate.validate_catalog(self.load(), catalog()[:-1])

    def test_catalog_does_not_guess_from_column_name(self):
        body = document()
        body["tables"]["orders"]["columns"]["guest_email"]["classification"] = "operational"
        # Explicit classification is authoritative, even for a contact-like name.
        self.gate.validate_catalog(self.load({"orders": body}), catalog())

    def test_pii_policy_is_required_and_actionable(self):
        mutations = {
            "classification": lambda table: table["columns"]["guest_email"].pop("classification"),
            "subject": lambda table: table.pop("subject"),
            "erase": lambda table: table["columns"]["guest_email"].pop("erase"),
            "export": lambda table: table["columns"]["guest_email"].pop("export"),
            "retain_reason": lambda table: table["columns"]["guest_email"].update(erase={"strategy": "retain"}),
            "unknown_classification": lambda table: table["columns"]["guest_email"].update(classification="guess_from_name"),
            "unknown_erase": lambda table: table["columns"]["guest_email"].update(erase={"strategy": "erase_eventually"}),
            "unknown_export": lambda table: table["columns"]["guest_email"].update(export="maybe"),
        }
        for label, mutate in mutations.items():
            with self.subTest(label=label), self.assertRaises(ValueError):
                body = document()
                mutate(body["tables"]["orders"])
                self.gate.validate_catalog(self.load({"orders": body}), catalog())

    def test_subject_requires_identity_path_or_explicit_null_policy(self):
        body = document()
        body["tables"]["orders"]["subject"].pop("identity_path")
        with self.assertRaises(ValueError):
            self.gate.validate_catalog(self.load({"orders": body}), catalog())

    def test_column_subject_overrides_table_subject(self):
        body = document()
        override = {"tenant_key": "tenant_id", "null_subject_policy": "unregistered_guest_contact"}
        body["tables"]["orders"]["columns"]["guest_email"]["subject"] = override
        registry = self.load({"orders": body})
        self.assertEqual(registry["orders.guest_email"]["subject"], override)
        self.gate.validate_catalog(registry, catalog())

    def test_jsonb_and_bytea_require_named_structure_even_if_operational(self):
        for column in ("address_snapshot", "receipt"):
            with self.subTest(column=column), self.assertRaisesRegex(ValueError, column):
                body = document()
                body["tables"]["orders"]["columns"][column] = {"classification": "operational"}
                self.gate.validate_catalog(self.load({"orders": body}), catalog())

    def test_duplicate_json_keys_are_rejected(self):
        raw = json.dumps(document()).replace(
            '"classification": "operational"',
            '"classification": "operational", "classification": "personal"',
            1,
        )
        with self.assertRaisesRegex(ValueError, "(?i)duplicate"):
            self.load(raw=raw)

    def test_duplicate_column_ownership_is_rejected(self):
        first = document()
        second = copy.deepcopy(first)
        second["module"] = "other"
        with self.assertRaisesRegex(ValueError, "(?i)duplicate|ownership"):
            self.load({"orders": first, "other": second})

    def test_invalid_sql_identifiers_are_rejected(self):
        for label in ("orders;DROP TABLE app.orders", "orders.elsewhere", "orders\n"):
            with self.subTest(label=label), self.assertRaises(ValueError):
                body = document()
                body["tables"][label] = body["tables"].pop("orders")
                self.load({"orders": body})

    def fixture(self):
        registry = self.load()
        template = "SELECT " + ", ".join(
            "{{canary:" + name + "}}" for name, field in registry.items()
            if field["classification"] in ("personal", "contact")
        ) + ";"
        _, canaries = self.gate.render_fixture(registry, template, "coverage")
        actual = {name: [canary] for name, canary in canaries.items()}
        return registry, actual, canaries

    def test_fixture_requires_real_cell_for_each_pii_column(self):
        for empty in ([], [None], ["ordinary content"], [{"covered": True}]):
            with self.subTest(empty=empty), self.assertRaisesRegex(ValueError, "orders\\.address_snapshot"):
                registry, actual, canaries = self.fixture()
                actual["orders.address_snapshot"] = empty
                self.gate.validate_fixture(registry, actual, canaries)

    def test_fixture_cannot_count_canary_in_another_column(self):
        registry, actual, canaries = self.fixture()
        actual["orders.guest_email"].append(canaries["orders.address_snapshot"])
        actual["orders.address_snapshot"] = []
        with self.assertRaisesRegex(ValueError, "orders\\.address_snapshot"):
            self.gate.validate_fixture(registry, actual, canaries)

    def test_fixture_rejects_missing_canary_declaration(self):
        registry, actual, canaries = self.fixture()
        canaries.pop("orders.guest_email")
        with self.assertRaisesRegex(ValueError, "orders\\.guest_email"):
            self.gate.validate_fixture(registry, actual, canaries)

    def test_fixture_rejects_duplicate_canary_values(self):
        registry, actual, canaries = self.fixture()
        canaries["orders.address_snapshot"] = canaries["orders.guest_email"]
        actual["orders.address_snapshot"] = [canaries["orders.guest_email"]]
        with self.assertRaisesRegex(ValueError, "(?i)duplicate|unique"):
            self.gate.validate_fixture(registry, actual, canaries)

    def test_render_fixture_rejects_missing_pii_placeholder(self):
        with self.assertRaisesRegex(ValueError, "orders\\.address_snapshot|orders\\.receipt"):
            self.gate.render_fixture(self.load(), "SELECT {{canary:orders.guest_email}};", "partial")

    def test_render_fixture_rejects_unknown_placeholder(self):
        with self.assertRaisesRegex(ValueError, "orders\\.does_not_exist"):
            self.gate.render_fixture(self.load(), (
                "SELECT {{canary:orders.guest_email}}, {{canary:orders.address_snapshot}}, "
                "{{canary:orders.receipt}}, {{canary:orders.does_not_exist}};"
            ), "unknown")

    def test_render_fixture_prefix_cannot_inject_sql(self):
        for prefix in ("bad'quote", "bad;sql", "bad\nline"):
            with self.subTest(prefix=prefix), self.assertRaises(ValueError):
                self.gate.render_fixture(self.load(), (
                    "SELECT {{canary:orders.guest_email}}, {{canary:orders.address_snapshot}}, "
                    "{{canary:orders.receipt}};"
                ), prefix)


if __name__ == "__main__":
    unittest.main(verbosity=2)
