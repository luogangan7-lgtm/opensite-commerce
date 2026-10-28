#!/usr/bin/env python3
"""Contract tests for the explicit column registry and canary coverage gate.

Depends only on Python stdlib and scripts/privacy_registry.py. CI check_all
calls this file; these small synthetic declarations test failure boundaries,
while the PG runner must validate the migrated catalog and real fixture cells.
"""
import copy
from contextlib import redirect_stderr, redirect_stdout
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch


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
                    "tenant_id": {"classification": "operational"},
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
        {"table_name": "orders", "column_name": "tenant_id", "data_type": "uuid", "udt_name": "uuid"},
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
            "orders.id", "orders.tenant_id", "orders.guest_email", "orders.address_snapshot", "orders.receipt",
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

    def test_financial_obligation_requires_explicit_retention_and_reason(self):
        valid = {"classification": "financial_obligation", "retention": "financial-obligation",
                 "reason": "Refund recovery/reconciliation and accounting"}
        body = document()
        body["tables"]["orders"]["columns"]["provider_ref"] = valid
        registry = self.load({"orders": body})
        self.assertNotIn("orders.provider_ref", self.gate.personal_fields(registry))
        for key in ("retention", "reason", "classification"):
            with self.subTest(key=key):
                broken = copy.deepcopy(body)
                del broken["tables"]["orders"]["columns"]["provider_ref"][key]
                with self.assertRaises(ValueError):
                    self.load({"orders": broken})
        for invalid in (None, "", " ", 42):
            broken = copy.deepcopy(body)
            broken["tables"]["orders"]["columns"]["provider_ref"]["reason"] = invalid
            with self.subTest(reason=invalid), self.assertRaises(ValueError):
                self.load({"orders": broken})

    def test_returns_retains_financial_authority_and_erases_personal_copies(self):
        registry = self.gate.load_registry(ROOT)
        provider = registry["returns.provider_ref"]
        self.assertEqual(provider["classification"], "financial_obligation")
        self.assertEqual(provider["retention"], "financial-obligation")
        self.assertTrue(provider["reason"].strip())
        for key in ("returns.reason", "returns.creation_response", "returns.receive_response",
                    "shipments.carrier", "shipments.tracking_number", "inventory_movements.reason"):
            with self.subTest(key=key):
                self.assertIn(registry[key]["classification"], ("personal", "contact"))
                self.assertEqual(registry[key]["erase"]["strategy"], "scrub")
        self.assertIn("returns", registry["inventory_movements.reason"]["subject"]["identity_path"])

    def test_catalog_rejects_declared_nonexistent_column(self):
        with self.assertRaisesRegex(ValueError, "orders\\.receipt"):
            self.gate.validate_catalog(self.load(), catalog()[:-1])

    def test_catalog_does_not_guess_from_column_name(self):
        body = document()
        body["tables"]["orders"]["columns"]["guest_email"]["classification"] = "operational"
        # Explicit classification is authoritative, even for a contact-like name.
        self.gate.validate_catalog(self.load({"orders": body}), catalog())

    def test_cms_publish_contact_copies_remain_personal(self):
        # CMS publish copies these authored strings from the contact-bearing
        # site/blog source rows. Public visibility cannot erase classification.
        registry = self.gate.load_registry(ROOT)
        for column in (
            "site_drafts.title", "site_drafts.description",
            "blog_post_revisions.title", "blog_post_revisions.excerpt",
        ):
            with self.subTest(column=column):
                self.assertIn(registry[column]["classification"], ("personal", "contact"))

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

    def test_non_global_subject_requires_nonempty_tenant_key(self):
        for tenant_key in ("", " ", 3):
            with self.subTest(tenant_key=tenant_key), self.assertRaises(ValueError):
                body = document()
                body["tables"]["orders"]["subject"]["tenant_key"] = tenant_key
                self.load({"orders": body})

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

    def test_named_structures_must_resolve_to_nonempty_declarations(self):
        for structures in ({}, {"PostalAddress": "", "ReceiptBytes": "binary receipt"}):
            with self.subTest(structures=structures), self.assertRaises(ValueError):
                body = document()
                body["structures"] = structures
                self.gate.validate_catalog(self.load({"orders": body}), catalog())

    def test_queue_schema_payload_is_independently_classified_and_covered(self):
        body = {
            "version": 1,
            "module": "reliability",
            "structures": {"RiverJobArgs": "Notification and other tenant job payloads."},
            "tables": {
                "river_job": {
                    "schema": "queue",
                    "subject": {
                        "tenant_key": None,
                        "null_subject_policy": "Unattributed system jobs have no customer subject.",
                    },
                    "columns": {"args": personal(structure="RiverJobArgs")},
                },
            },
        }
        registry = self.load({"reliability": body})
        self.assertEqual(set(registry), {"queue.river_job.args"})
        self.gate.validate_catalog(registry, [{
            "table_schema": "queue", "table_name": "river_job", "column_name": "args",
            "data_type": "jsonb", "udt_name": "jsonb",
        }])
        _, canaries = self.gate.render_fixture(
            registry, "SELECT jsonb_build_object('recipient', {{canary:queue.river_job.args}});", "queuejob",
        )
        self.gate.validate_fixture(registry, {
            "queue.river_job.args": [{"recipient": canaries["queue.river_job.args"]}],
        }, canaries)
        with self.assertRaisesRegex(ValueError, "queue\\.river_job\\.args"):
            self.gate.validate_fixture(registry, {"queue.river_job.args": [{}]}, canaries)

    def test_catalog_reports_all_unknown_columns_and_all_phantoms(self):
        registry = self.load()
        extra = [
            {"table_schema": "app", "table_name": "orders", "column_name": "new_contact",
             "data_type": "text", "udt_name": "text"},
            {"table_schema": "queue", "table_name": "river_job", "column_name": "new_payload",
             "data_type": "ARRAY", "udt_name": "_jsonb"},
        ]
        with self.assertRaises(ValueError) as unknown:
            self.gate.validate_catalog(registry, catalog() + extra)
        self.assertIn("orders.new_contact", str(unknown.exception))
        self.assertIn("queue.river_job.new_payload", str(unknown.exception))
        with self.assertRaises(ValueError) as phantom:
            self.gate.validate_catalog(registry, catalog()[:-2])
        self.assertIn("orders.address_snapshot", str(phantom.exception))
        self.assertIn("orders.receipt", str(phantom.exception))

    def test_jsonb_array_requires_structure(self):
        body = document()
        body["tables"]["orders"]["columns"]["audit_errors"] = {"classification": "operational"}
        registry = self.load({"orders": body})
        rows = catalog() + [{
            "table_name": "orders", "column_name": "audit_errors", "data_type": "ARRAY", "udt_name": "_jsonb",
        }]
        with self.assertRaisesRegex(ValueError, "orders\\.audit_errors"):
            self.gate.validate_catalog(registry, rows)
        body["tables"]["orders"]["columns"]["audit_errors"]["structure"] = "PostalAddress"
        self.gate.validate_catalog(self.load({"orders": body}), rows)

    def test_inet_uses_explicit_legal_canary_and_observed_cell(self):
        body = document()
        body["tables"]["orders"]["columns"]["source_ip"] = personal(canary_value="192.0.2.7")
        registry = self.load({"orders": body})
        self.gate.validate_catalog(registry, catalog() + [{
            "table_name": "orders", "column_name": "source_ip", "data_type": "inet", "udt_name": "inet",
        }])
        _, canaries = self.gate.render_fixture(registry, (
            "SELECT {{canary:orders.guest_email}}, {{canary:orders.address_snapshot}}, "
            "{{canary:orders.receipt}}, {{canary:orders.source_ip}}::inet;"
        ), "inetcase")
        self.assertEqual(canaries["orders.source_ip"], "192.0.2.7")
        actual = {name: [canary] for name, canary in canaries.items()}
        self.gate.validate_fixture(registry, actual, canaries)
        actual["orders.source_ip"] = ["192.0.2.8"]
        with self.assertRaisesRegex(ValueError, "orders\\.source_ip"):
            self.gate.validate_fixture(registry, actual, canaries)

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

    def test_fixture_rejects_empty_canary(self):
        registry, actual, canaries = self.fixture()
        canaries["orders.guest_email"] = ""
        actual["orders.guest_email"] = ["ordinary content"]
        with self.assertRaisesRegex(ValueError, "orders\\.guest_email"):
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

    def cli_root(self, tmp):
        root = Path(tmp)
        declarations = root / "modules/privacy"
        declarations.mkdir(parents=True)
        (declarations / "orders.json").write_text(json.dumps(document()))
        (root / "modules/registry.yaml").write_text(json.dumps({
            "modules": [{"name": "orders", "owns_tables": ["orders"]}],
        }))
        return root

    def test_secret_hash_must_declare_export_exclude(self):
        for export in ("include", "redact", None):
            with self.subTest(export=export), self.assertRaisesRegex(ValueError, "PRIVACY_SECRET_EXPORT"):
                body = document()
                spec = {"classification": "secret_hash"}
                if export:
                    spec["export"] = export
                body["tables"]["orders"]["columns"]["token_hash"] = spec
                self.load({"orders": body})

    def test_tenant_key_must_be_a_real_column_of_the_table(self):
        body = document()
        body["tables"]["orders"]["subject"]["tenant_key"] = "tenant_idd"
        with self.assertRaisesRegex(ValueError, "PRIVACY_TENANT_KEY_UNKNOWN"):
            self.gate.validate_catalog(self.load({"orders": body}), catalog())

    def test_manifest_alone_fails_loudly(self):
        with tempfile.TemporaryDirectory(prefix="privacy-cli-test-") as tmp:
            root = self.cli_root(tmp)
            stderr = io.StringIO()
            with patch.object(sys, "argv", ["privacy_registry.py", "--root", str(root),
                                            "--manifest", str(root / "m.json")]), \
                    redirect_stdout(io.StringIO()), redirect_stderr(stderr):
                self.assertEqual(self.gate.main(), 1)
            self.assertIn("--manifest requires", stderr.getvalue())

    def test_database_and_fixture_cli_refuse_non_github_execution(self):
        for option in ("--database", "--fixture"):
            with self.subTest(option=option), tempfile.TemporaryDirectory(prefix="privacy-cli-test-") as tmp:
                root = self.cli_root(tmp)
                stderr = io.StringIO()
                with patch.object(sys, "argv", ["privacy_registry.py", "--root", str(root), option]), \
                        patch.dict(os.environ, {"GITHUB_ACTIONS": "false"}), \
                        patch.object(self.gate, "psql") as database, \
                        redirect_stdout(io.StringIO()), redirect_stderr(stderr):
                    self.assertEqual(self.gate.main(), 1)
                database.assert_not_called()
                self.assertIn("PRIVACY_CI_ONLY", stderr.getvalue())

    def test_database_errors_never_log_dsn_raw_errors_or_cells(self):
        dsn = "postgresql://fixture@127.0.0.1/test?application_name=redaction_canary"
        raw_cell = "canary_real_cell_must_not_be_logged"
        errors = (
            subprocess.CompletedProcess(["psql", dsn], 1, raw_cell, "raw database error " + dsn),
            FileNotFoundError("psql missing: " + dsn + " " + raw_cell),
            subprocess.TimeoutExpired(["psql", dsn], 90, output=raw_cell, stderr=dsn),
        )
        for error in errors:
            with self.subTest(error=type(error).__name__), tempfile.TemporaryDirectory(prefix="privacy-cli-test-") as tmp:
                root = self.cli_root(tmp)
                stdout, stderr = io.StringIO(), io.StringIO()
                with patch.object(sys, "argv", ["privacy_registry.py", "--root", str(root), "--database"]), \
                        patch.dict(os.environ, {"GITHUB_ACTIONS": "true", "DATABASE_URL": dsn}), \
                        patch.object(self.gate.subprocess, "run") as command, \
                        redirect_stdout(stdout), redirect_stderr(stderr):
                    if isinstance(error, Exception):
                        command.side_effect = error
                    else:
                        command.return_value = error
                    self.assertEqual(self.gate.main(), 1)
                command.assert_called_once()
                captured = stdout.getvalue() + stderr.getvalue()
                self.assertIn("PRIVACY_DATABASE_", captured)
                self.assertNotIn(dsn, captured)
                self.assertNotIn(raw_cell, captured)
                self.assertNotIn("raw database error", captured)

    def test_database_failure_keeps_only_safe_state_and_fixture_line(self):
        dsn = "postgresql://fixture@127.0.0.1/test?application_name=redaction_canary"
        error = subprocess.CompletedProcess(["psql", dsn], 1, "", (
            "ERROR: 23514: sensitive constraint detail\n"
            "CONTEXT: PL/pgSQL function inline_code_block line 73 at SQL statement\n" + dsn
        ))
        with tempfile.TemporaryDirectory(prefix="privacy-cli-test-") as tmp:
            root = self.cli_root(tmp)
            stderr = io.StringIO()
            with patch.object(sys, "argv", ["privacy_registry.py", "--root", str(root), "--database"]), \
                    patch.dict(os.environ, {"GITHUB_ACTIONS": "true", "DATABASE_URL": dsn}), \
                    patch.object(self.gate.subprocess, "run", return_value=error), \
                    redirect_stdout(io.StringIO()), redirect_stderr(stderr):
                self.assertEqual(self.gate.main(), 1)
            self.assertIn("SQLSTATE=23514 fixture_line=73", stderr.getvalue())
            self.assertNotIn(dsn, stderr.getvalue())
            self.assertNotIn("sensitive constraint detail", stderr.getvalue())


class MerchantDeletionRegistryTests(unittest.TestCase):
    """MDEL-00: mandatory merchant policy is independent of customer retention."""
    setUpClass = PrivacyRegistryTests.__dict__["setUpClass"]

    def fixture(self, root):
        body = document()
        body["merchant_deletion"] = {
            "version": 1,
            "handlers": {"orders.scrub": {
                "depends_on": [], "tables": ["orders"],
                "lock_order": "tenant_fence -> artifact_uuid -> module",
                "clock": "database", "transaction": "one_item_and_checkpoint",
            }},
            "artifacts": {},
        }
        for name, field in body["tables"]["orders"]["columns"].items():
            field["merchant_deletion"] = {
                "action": "erase", "strategy": "delete_row",
                "reason": "Remove merchant-owned order copy: " + name,
                "handler": "orders.scrub",
                "ownership": {"kind": "tenant_column", "path": "orders.tenant_id"},
            }
        self.write(root, body)
        return body

    def write(self, root, body):
        folder = root / "modules/privacy"
        folder.mkdir(parents=True, exist_ok=True)
        (folder / "orders.json").write_text(json.dumps(body))

    def load(self, root):
        return self.gate.load_merchant_contract(root, self.gate.load_registry(root))

    def test_contract_is_versioned_deterministic_and_binds_policy_content(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            body = self.fixture(root)
            first = self.load(root)
            self.assertEqual(first["version"], 1)
            self.assertEqual(first["handler_order"], ["orders.scrub"])
            self.assertEqual(first["digest"], self.load(root)["digest"])
            body["tables"]["orders"]["columns"]["id"]["merchant_deletion"]["reason"] += " changed"
            self.write(root, body)
            self.assertNotEqual(first["digest"], self.load(root)["digest"])

    def test_retained_type_must_match_actual_catalog(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            body = self.fixture(root)
            # Keep all cells in the same witness row; no mixed row-delete plan.
            for field in body["tables"]["orders"]["columns"].values():
                field["merchant_deletion"].update(strategy="constant", value=None)
            body["tables"]["orders"]["columns"]["id"]["merchant_deletion"].update(
                action="retain", strategy="unchanged", retained_kind="financial", value_type="uuid")
            self.write(root, body)
            registry = self.gate.load_registry(root)
            self.gate.validate_catalog(registry, catalog())
            rows = copy.deepcopy(catalog())
            rows[0]["udt_name"] = "text"
            with self.assertRaisesRegex(ValueError, "MERCHANT_RETAIN_CATALOG_TYPE.*orders.id"):
                self.gate.validate_catalog(registry, rows)

    def test_owner_can_detach_fk_then_delete_same_table_in_separate_items(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            body = self.fixture(root)
            handlers = body["merchant_deletion"]["handlers"]
            handlers["orders.detach"] = copy.deepcopy(handlers["orders.scrub"])
            handlers["orders.scrub"]["depends_on"] = ["orders.detach"]
            body["tables"]["orders"]["columns"]["address_snapshot"]["merchant_deletion"].update(
                handler="orders.detach", strategy="constant", value={"erased": True})
            self.write(root, body)
            self.assertEqual(self.load(root)["handler_order"], ["orders.detach", "orders.scrub"])
            handlers["orders.detach"]["depends_on"] = ["orders.scrub"]
            self.write(root, body)
            with self.assertRaisesRegex(ValueError, "MERCHANT_DEPENDENCY_CYCLE"):
                self.load(root)

    def test_contract_schema_and_version_are_mandatory(self):
        for mutation in ("missing", "version", "unknown", "mixed_row"):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                body = self.fixture(root)
                if mutation == "missing":
                    body.pop("merchant_deletion")
                elif mutation == "version":
                    body["merchant_deletion"]["version"] = 2
                elif mutation == "unknown":
                    body["merchant_deletion"]["guess"] = True
                else:
                    body["tables"]["orders"]["columns"]["id"]["merchant_deletion"].update(strategy="constant", value=None)
                self.write(root, body)
                with self.assertRaises(ValueError):
                    self.load(root)

    def test_every_column_needs_independent_explicit_policy(self):
        for key in ("action", "strategy", "reason", "handler", "ownership"):
            with self.subTest(key=key), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                body = self.fixture(root)
                body["tables"]["orders"]["columns"]["address_snapshot"]["merchant_deletion"].pop(key)
                self.write(root, body)
                with self.assertRaisesRegex(ValueError, "MERCHANT_.*orders.address_snapshot"):
                    self.load(root)
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            body = self.fixture(root)
            body["tables"]["orders"]["columns"]["id"].pop("merchant_deletion")
            self.write(root, body)
            with self.assertRaisesRegex(ValueError, "MERCHANT_POLICY_MISSING.*orders.id"):
                self.load(root)

    def test_unknown_values_blank_reasons_and_forged_tenant_authority_fail(self):
        mutations = [
            {"action": "eventually"}, {"strategy": "guess_from_name"}, {"reason": " "},
            {"ownership": {"kind": "tenant_column", "path": "orders.missing"}},
            {"ownership": {"kind": "tenant_column", "path": "other.tenant_id"}},
            {"ownership": {"kind": "payload_tenant", "path": "args.tenant_id"}},
            {"handler": "other.scrub"},
            {"action": "retain", "strategy": "unchanged", "retained_kind": "financial"},
        ]
        for changes in mutations:
            with self.subTest(changes=changes), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                body = self.fixture(root)
                body["tables"]["orders"]["columns"]["address_snapshot"]["merchant_deletion"].update(changes)
                self.write(root, body)
                with self.assertRaises(ValueError):
                    self.load(root)

    def test_unknown_dependency_cycle_wrong_owner_and_incomplete_table_order_fail(self):
        for change in ("unknown", "cycle", "owner", "tables", "clock", "transaction", "lock"):
            with self.subTest(change=change), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                body = self.fixture(root)
                handler = body["merchant_deletion"]["handlers"]["orders.scrub"]
                if change == "unknown":
                    handler["depends_on"] = ["missing.handler"]
                elif change == "cycle":
                    handler["depends_on"] = ["orders.scrub"]
                elif change == "owner":
                    body["merchant_deletion"]["handlers"] = {"other.scrub": handler}
                elif change == "tables":
                    handler["tables"] = []
                elif change == "clock":
                    handler["clock"] = "application"
                elif change == "transaction":
                    handler["transaction"] = "network_inside_transaction"
                else:
                    handler["lock_order"] = "module -> tenant_fence"
                self.write(root, body)
                with self.assertRaises(ValueError):
                    self.load(root)

    def test_artifact_inventory_is_bidirectional_and_requires_a_scrub(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            body = self.fixture(root)
            source = root / "internal/export.go"
            source.parent.mkdir()
            source.write_text('package export\nimport "os"\n// privacy-artifact: orders.archive\nvar f, _ = os.CreateTemp(".", "private")\n')
            with self.assertRaisesRegex(ValueError, "MERCHANT_ARTIFACT_UNDECLARED"):
                self.load(root)
            body["merchant_deletion"]["artifacts"]["orders.archive"] = {
                "action": "erase", "strategy": "unlink_tenant_namespace",
                "reason": "Private order export bytes include derived contacts.",
                "handler": "orders.scrub", "source": "internal/export.go",
                "ownership": {"kind": "tenant_column", "path": "orders.tenant_id"},
                "paths": ["exports/<tenant_uuid>/<request_uuid>.json"],
                "guards": ["canonical_ids", "anchored_root", "no_symlinks", "fsync_parents", "verify_empty"],
            }
            self.write(root, body)
            self.load(root)
            body["merchant_deletion"]["artifacts"]["orders.archive"]["action"] = "retain"
            self.write(root, body)
            with self.assertRaisesRegex(ValueError, "MERCHANT_ARTIFACT"):
                self.load(root)
            body["merchant_deletion"]["artifacts"]["orders.archive"]["action"] = "erase"
            self.write(root, body)
            source.write_text("package export\n")
            with self.assertRaisesRegex(ValueError, "MERCHANT_ARTIFACT_PHANTOM"):
                self.load(root)

    def test_unclassified_filesystem_writer_fails(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            self.fixture(root)
            source = root / "internal/unregistered.go"
            source.parent.mkdir()
            source.write_text('package export\nvar f = os.CreateTemp(root, "private")\n')
            with self.assertRaisesRegex(ValueError, "MERCHANT_ARTIFACT_WRITER"):
                self.load(root)

    def test_root_writers_and_each_new_call_require_own_annotation(self):
        cases = [
            'package probe\nimport "os"\nfunc write(r *os.Root) { r.Create("new") }\n',
            'package probe\nimport "os"\nfunc write(r *os.Root) { r.WriteFile("new", nil, 0600) }\n',
            'package probe\nimport "os"\n// privacy-artifact: orders.archive\nvar f, _ = os.CreateTemp(".", "old")\nvar g, _ = os.Create("new")\n',
            'package probe\nimport "os"\n// privacy-artifact: orders.archive\nvar f, _ = os.CreateTemp(".", "old")\nfunc write(r *os.Root) { r.Create("new") }\n',
        ]
        for source in cases:
            with self.subTest(source=source), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                (root / "internal").mkdir()
                (root / "internal/probe.go").write_text(source)
                with self.assertRaisesRegex(ValueError, "MERCHANT_ARTIFACT_WRITER"):
                    self.gate.artifact_catalog(root)

    def test_os_root_types_aliases_and_inactive_files_are_inventoried(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "go.mod").write_text("module privacyprobe\ngo 1.27.2\n")
            (root / "internal").mkdir()
            source = root / "internal/probe.go"
            source.write_text('''package probe
import fs "os"
type business struct{}
func (business) Create(string) {}
func write(r *fs.Root, b business) {
 // privacy-artifact: orders.archive
 r.Create("private")
 b.Create("business")
 _ = "os.Create(quoted)"
}
''')
            self.assertEqual(self.gate.artifact_catalog(root), {"orders.archive": "internal/probe.go"})
            (root / "internal/tagged.go").write_text('''//go:build privacy_future
package probe
import "os"
func future(r *os.Root) { r.WriteFile("unregistered", nil, 0600) }
''')
            with self.assertRaisesRegex(ValueError, "MERCHANT_ARTIFACT_WRITER"):
                self.gate.artifact_catalog(root)

    def test_credential_or_personal_hash_cannot_claim_financial_retention(self):
        registry = self.gate.load_registry(ROOT)
        handlers = self.load(ROOT)["handlers"]
        for key in ("api_keys.key_hash", "customer_credentials.password_hash", "orders.guest_contact"):
            with self.subTest(key=key):
                field = registry[key]
                policy = copy.deepcopy(field["merchant_deletion"])
                policy.pop("preserve_null", None)
                policy.pop("value", None)
                policy.update(action="retain", strategy="unchanged", retained_kind="financial", value_type="digest")
                with self.assertRaisesRegex(ValueError, "MERCHANT_PERSONAL_RETAIN"):
                    self.gate.merchant_policy(key, policy, field["module"], registry, handlers)

    def test_reviewed_scrubs_match_constraints_and_immutable_receipt(self):
        fields = self.load(ROOT)["fields"]
        self.assertEqual((fields["product_variants.option_key"]["strategy"],
                          fields["product_variants.option_key"]["value"]),
                         ("row_id_placeholder", "erased:{id}"))
        self.assertEqual((fields["orders.guest_contact"]["strategy"], fields["orders.guest_contact"]["value"]),
                         ("row_json_template", {"email": "erased-{id}@invalid.test", "erased": True}))
        self.assertEqual(fields["orders.shipping_snapshot"]["strategy"], "row_json_template")
        self.assertEqual(fields["orders.shipping_snapshot"]["value"],
                         {"erased": True, "amount_minor": {"column": "shipping_minor"}, "currency": {"column": "currency"}})
        scope = fields["data_rights_erasures.scope"]
        self.assertEqual((scope["action"], scope["strategy"], scope["retained_kind"], scope["value_type"]),
                         ("retain", "unchanged", "control", "enum"))
        self.assertEqual(scope["values"], ["customer.profile,addresses,credentials,b2b,order_snapshots,exports"])

    def test_json_templates_reject_untrusted_or_cross_row_sources(self):
        registry = self.gate.load_registry(ROOT)
        handlers = self.load(ROOT)["handlers"]
        base = registry["orders.guest_contact"]["merchant_deletion"]
        for value in ({"email": "{guest_contact}"}, {"x": {"column": "guest_contact"}},
                      {"x": {"column": "customers.email"}}, {"x": {"sql": "current_user"}},
                      {"x": {"column": "missing"}}, ["{id}"]):
            with self.subTest(value=value):
                policy = {**base, "strategy": "row_json_template", "value": value}
                with self.assertRaisesRegex(ValueError, "MERCHANT_JSON_TEMPLATE"):
                    self.gate.merchant_policy("orders.guest_contact", policy, "orders", registry, handlers)

    def test_actual_registry_covers_derived_copies_and_global_exclusions(self):
        try:
            contract = self.load(ROOT)
        except ValueError as error:
            self.fail(str(error))
        registry = self.gate.load_registry(ROOT)
        self.assertEqual(set(contract["fields"]), set(registry))
        for name in ("tenants.name", "ledger_accounts.code", "inventory_movements.reason",
                     "storefront_serving.body", "storefront_serving.fallback_body", "queue.river_job.args"):
            self.assertEqual(contract["fields"][name]["action"], "erase", name)
        for name in ("users.email", "auth_sessions.token_hash"):
            self.assertEqual(contract["fields"][name]["ownership"]["kind"], "global_exclusion")
        # #403's typed receipt must survive with the retained financial order;
        # mailbox proof/profile data is never a substitute for this UUID witness.
        for column in ("tenant_id", "order_id", "customer_id"):
            name = "data_rights_erased_orders." + column
            policy = contract["fields"][name]
            self.assertEqual((policy["action"], policy["strategy"], policy["retained_kind"], policy["value_type"]),
                             ("retain", "unchanged", "witness", "uuid"), name)
            self.assertEqual(policy["handler"], "datarights.erase", name)
            self.assertEqual(policy["ownership"], {
                "kind": "tenant_column", "path": "data_rights_erased_orders.tenant_id",
            }, name)
        tables = contract["handlers"]["datarights.erase"]["tables"]
        self.assertLess(tables.index("data_rights_erased_orders"), tables.index("data_rights_erasures"))
        self.assertEqual(set(contract["artifacts"]), {
            "media.originals", "media.staging", "datarights.customer_export", "datarights.export_temp",
        })
        self.assertEqual(contract["artifacts"]["media.originals"]["prerequisite"], "MDEL-21a")
        pointer = contract["fields"]["sites.published_release_id"]
        self.assertEqual((pointer["handler"], pointer["strategy"], pointer["value"]),
                         ("site.detach", "constant", None))
        order = contract["handler_order"]
        self.assertLess(order.index("site.detach"), order.index("publish.erase"))
        self.assertLess(order.index("publish.erase"), order.index("site.erase"))
        self.assertEqual(set(contract["fields"]["payment_refund_terminals.order_target"]["values"]),
                         set(contract["fields"]["orders.state"]["values"]),
                         "Refund terminals preserve an existing Orders state or its quantity-derived compensation target")
        for name in ("orders.guest_contact", "checkout_sessions.start_result", "checkout_sessions.final_result",
                     "checkout_attempts.result", "returns.creation_response", "returns.receive_response", "returns.receive_key"):
            self.assertTrue(contract["fields"][name]["preserve_null"], name)


if __name__ == "__main__":
    unittest.main(verbosity=2)
