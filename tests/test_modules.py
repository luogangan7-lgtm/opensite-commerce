#!/usr/bin/env python3
"""Governance regression gates: real Go fixtures, registry/API/table fault probes.

Depends on the stdlib checker and Go; called by check_all. Catalog fixtures test
the comparison logic; check_pg separately verifies the real PostgreSQL catalog.
"""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("check_modules", ROOT / "scripts/check_modules.py")
checker = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(checker)


def module(name, dependencies=(), tables=()):
    return dict(name=name, status="implemented", go_path=f"internal/modules/{name}",
                go_status="implemented", owns_tables=list(tables), operations=[],
                dependencies=list(dependencies), tests=[], scopes=[])


class GovernanceTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        (self.root / "go.mod").write_text("module example.test/shop\n\ngo 1.27.2\n")
        (self.root / "api").mkdir()
        (self.root / "api/openapi.yaml").write_text(
            "openapi: 3.1.0\npaths:\n  /things:\n    get:\n      operationId: listThings\n")
        self.registry = {"version": 1, "modules": [module("a", ["b"], ["things"]),
                                                  module("b")], "migrations": []}
        self.source("internal/modules/b/contract/port.go", "package contract\nconst Value = 1\n")
        self.source("internal/modules/b/internal/store/store.go", "package store\nconst Value = 1\n")
        self.source("internal/modules/a/module.go", 'package a\nimport "example.test/shop/internal/modules/b/contract"\nvar Value = contract.Value\n')

    def source(self, name, text):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)

    def run_gate(self):
        self.source("modules/registry.yaml", json.dumps(self.registry))
        return subprocess.run(["python3", str(ROOT / "scripts/check_modules.py"),
                               "--root", str(self.root)], capture_output=True, text=True,
                              env=dict(os.environ, GOPROXY="off", GOSUMDB="off"), timeout=90)

    def assert_red(self, marker):
        result = self.run_gate()
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn(marker, result.stderr)

    def test_real_contract_import_is_green(self):
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("default/pgx including tests", result.stdout)

    def test_illegal_private_import_red(self):
        self.source("internal/modules/a/module.go", 'package a\nimport "example.test/shop/internal/modules/b/internal/store"\nvar Value = store.Value\n')
        self.assert_red("MODULE_PRIVATE_IMPORT")

    def test_test_only_private_import_red(self):
        self.source("internal/modules/a/module_test.go", 'package a\nimport "example.test/shop/internal/modules/b/internal/store"\nvar testValue = store.Value\n')
        self.assert_red("MODULE_PRIVATE_IMPORT")

    def test_external_test_contract_import_is_green(self):
        self.source("internal/modules/a/external_test.go", 'package a_test\nimport "example.test/shop/internal/modules/b/contract"\nvar Value = contract.Value\n')
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_external_test_private_import_red(self):
        self.source("internal/modules/a/external_test.go", 'package a_test\nimport "example.test/shop/internal/modules/b/internal/store"\nvar Value = store.Value\n')
        self.assert_red("MODULE_PRIVATE_IMPORT")

    def test_external_test_undeclared_import_red(self):
        self.source("internal/modules/a/module.go", "package a\n")
        self.source("internal/modules/a/external_test.go", 'package a_test\nimport "example.test/shop/internal/modules/b/contract"\nvar Value = contract.Value\n')
        self.registry["modules"][0]["dependencies"] = []
        self.assert_red("MODULE_UNDECLARED_DEPENDENCY")

    def test_own_private_import_is_green(self):
        self.source("internal/modules/b/module.go", 'package b\nimport "example.test/shop/internal/modules/b/internal/store"\nvar Value = store.Value\n')
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_openapi_block_scalar_decoy_does_not_count(self):
        self.registry["modules"][0]["operations"] = [{"operation_id": "decoy", "status": "implemented"}]
        with (self.root / "api/openapi.yaml").open("a") as output:
            output.write("      description: |\n        operationId: decoy\n")
        self.assert_red("MODULE_MISSING_OPERATION")

    def test_openapi_reference_fails_closed(self):
        (self.root / "api/openapi.yaml").write_text("paths:\n  /things:\n    $ref: './other.yaml'\n")
        self.assert_red("MODULE_OPENAPI_INVALID")

    def test_openapi_duplicate_path_fails_closed(self):
        with (self.root / "api/openapi.yaml").open("a") as output:
            output.write("  /things:\n    get:\n      operationId: anotherThings\n")
        self.assert_red("MODULE_OPENAPI_INVALID")

    def test_openapi_multiple_path_parameters_are_green(self):
        (self.root / "api/openapi.yaml").write_text(
            "paths:\n  /things:\n    parameters:\n      - name: first\n        in: path\n      - name: second\n        in: path\n    get:\n      operationId: listThings\n")
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_openapi_extension_decoy_does_not_count(self):
        self.registry["modules"][0]["operations"] = [{"operation_id": "decoy", "status": "implemented"}]
        with (self.root / "api/openapi.yaml").open("a") as output:
            output.write("  x-example:\n    get:\n      operationId: decoy\n")
        self.assert_red("MODULE_MISSING_OPERATION")

    def test_openapi_operation_without_id_fails_closed(self):
        with (self.root / "api/openapi.yaml").open("a") as output:
            output.write("  /missing:\n    get:\n      summary: No operation ID\n")
        self.assert_red("MODULE_OPENAPI_INVALID")

    def test_pgx_only_private_import_red(self):
        self.source("internal/modules/a/pgx.go", '//go:build pgx\n\npackage a\nimport "example.test/shop/internal/modules/b/internal/store"\nvar pgxValue = store.Value\n')
        self.assert_red("MODULE_PRIVATE_IMPORT")

    def test_cross_module_root_import_red(self):
        self.source("internal/modules/b/module.go", "package b\nconst Value = 1\n")
        self.source("internal/modules/a/module.go", 'package a\nimport "example.test/shop/internal/modules/b"\nvar Value = b.Value\n')
        self.assert_red("MODULE_NONCONTRACT_IMPORT")

    def test_undeclared_dependency_red(self):
        self.registry["modules"][0]["dependencies"] = []
        self.assert_red("MODULE_UNDECLARED_DEPENDENCY")

    def test_unknown_module_red(self):
        self.source("internal/modules/unknown/module.go", "package unknown\n")
        self.assert_red("MODULE_UNREGISTERED_PACKAGE")

    def test_registry_dependency_cycle_red(self):
        self.registry["modules"][1]["dependencies"] = ["a"]
        self.assert_red("MODULE_DEPENDENCY_CYCLE")

    def test_implemented_operation_absent_red(self):
        self.registry["modules"][0]["operations"] = [{"operation_id": "missing", "status": "implemented"}]
        self.assert_red("MODULE_MISSING_OPERATION")

    def test_components_operation_does_not_count(self):
        self.registry["modules"][0]["operations"] = [{"operation_id": "decoy", "status": "implemented"}]
        with (self.root / "api/openapi.yaml").open("a") as output:
            output.write("components:\n  examples:\n    fake:\n      operationId: decoy\n")
        self.assert_red("MODULE_MISSING_OPERATION")

    def test_present_implemented_and_missing_planned_operations_green(self):
        self.registry["modules"][0]["operations"] = [
            {"operation_id": "listThings", "status": "implemented"},
            {"operation_id": "future", "status": "planned"}]
        self.assertEqual(self.run_gate().returncode, 0)

    def test_go_list_errors_fail_closed(self):
        self.source("internal/modules/a/broken.go", 'package a\nimport _ "example.test/unavailable"\n')
        self.assert_red("MODULE_GO_LIST_ERROR")

    def test_ghost_table_red(self):
        with self.assertRaisesRegex(checker.GovernanceError, "MODULE_GHOST_TABLE"):
            checker.check_tables(self.registry, [])

    def test_unregistered_table_red(self):
        with self.assertRaisesRegex(checker.GovernanceError, "MODULE_UNOWNED_TABLE"):
            checker.check_tables(self.registry, ["things", "new_table"])

    def test_duplicate_table_owner_red(self):
        self.registry["modules"][1]["owns_tables"] = ["things"]
        with self.assertRaisesRegex(checker.GovernanceError, "MODULE_DUPLICATE_OWNER"):
            checker.check_tables(self.registry, ["things"])

    def test_planned_table_not_a_live_owner(self):
        planned = module("future", tables=["next_table"])
        planned.update(status="planned", go_status="planned")
        self.registry["modules"].append(planned)
        checker.check_tables(self.registry, ["things"])
        with self.assertRaisesRegex(checker.GovernanceError, "MODULE_UNOWNED_TABLE"):
            checker.check_tables(self.registry, ["things", "next_table"])

    def test_database_error_never_reports_success(self):
        self.source("bin/psql", "#!/bin/sh\ncat >/dev/null\necho invalid >&2\nexit 31\n")
        psql = self.root / "bin/psql"
        psql.chmod(0o755)
        env = dict(os.environ, PATH=str(psql.parent) + os.pathsep + os.environ["PATH"],
                   DATABASE_URL="postgresql://secret@example.invalid/test")
        with mock.patch.dict(os.environ, env):
            with self.assertRaisesRegex(checker.GovernanceError, "MODULE_DATABASE_ERROR") as error:
                checker.catalog_tables(self.root)
        self.assertNotIn("secret", str(error.exception))

    def test_query_ownership_and_ci_reachability(self):
        site = (ROOT / "queries/site.sql").read_text()
        catalog = (ROOT / "queries/catalog.sql").read_text()
        self.assertIn("-- name: GetPublishedSite", site)
        self.assertNotIn("-- name: GetProductVariants", site)
        self.assertIn("-- name: GetProductVariants", catalog)
        self.assertNotIn("-- name: GetPublishedSite", catalog)
        workflows = list((ROOT / ".github/workflows").glob("*.yaml")) + list((ROOT / ".github/workflows").glob("*.yml"))
        self.assertTrue(any("bash scripts/check_all.sh" in workflow.read_text() for workflow in workflows))
        self.assertIn("python3 scripts/check_modules.py", (ROOT / "scripts/check_all.sh").read_text())
        self.assertIn("python3 scripts/check_modules.py --tables", (ROOT / "scripts/check_pg.sh").read_text())


if __name__ == "__main__":
    unittest.main(verbosity=2)
