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
import sys
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))
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
        self.registry["modules"][0]["operations"] = [{"operation_id": "listThings", "status": "planned"}]
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

    def test_nested_go_module_red(self):
        self.source("internal/modules/b/internal/store/go.mod", "module x.test/bstore\n\ngo 1.27.2\n")
        with (self.root / "go.mod").open("a") as output:
            output.write("\nrequire x.test/bstore v0.0.0\nreplace x.test/bstore => ./internal/modules/b/internal/store\n")
        self.source("internal/modules/a/module.go", 'package a\nimport "x.test/bstore"\nvar Value = store.Value\n')
        self.assert_red("MODULE_NESTED_GO_MODULE")

    def test_assembly_source_red(self):
        # Assembly reads another module's private symbol without import or linkname.
        self.source("internal/modules/a/peek_amd64.s", "#include \"textflag.h\"\nTEXT ·peek(SB),NOSPLIT,$0-8\n\tRET\n")
        self.assert_red("MODULE_NON_GO_SOURCE")

    def test_cgo_import_red(self):
        self.source("internal/modules/a/cgo.go", 'package a\n/*\nint x;\n*/\nimport "C"\n')
        self.assert_red("MODULE_NON_GO_SOURCE")

    def test_go_work_red(self):
        self.source("go.work", "go 1.27.2\n\nuse .\n")
        self.assert_red("MODULE_GO_WORK_FORBIDDEN")

    def test_nested_go_module_in_nonmodule_source_red(self):
        self.source("internal/bridge/go.mod", "module x.test/bridge\n\ngo 1.27.2\n")
        self.source("internal/bridge/bridge.go", "package bridge\n")
        self.assert_red("MODULE_NESTED_GO_MODULE")

    def test_root_receipt_artifacts_are_excluded_from_source(self):
        self.source("work/_probe/probe.go", 'package probe\nimport _ "unsafe"\n//go:linkname hidden x.test/other.hidden\nfunc hidden() int\n')
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_nested_go_module_in_vendor_red(self):
        self.source("vendor/x.test/hidden/go.mod", "module x.test/hidden\n\ngo 1.27.2\n")
        self.assert_red("MODULE_NESTED_GO_MODULE")

    def test_unreferenced_nested_receipt_module_red(self):
        self.source("work/probe/go.mod", "module x.test/probe\n\ngo 1.27.2\n")
        self.assert_red("MODULE_NESTED_GO_MODULE")

    def test_unused_in_repository_replace_red(self):
        # An unused replace has no package in go list: inspect go.mod too.
        for target in ("./internal/modules/b/internal/store",
                       str(self.root / "internal/modules/b/internal/store")):
            with self.subTest(target=target):
                (self.root / "go.mod").write_text(
                    "module example.test/shop\n\ngo 1.27.2\n" +
                    f"replace x.test/bstore => {target}\n")
                self.assert_red("MODULE_LOCAL_REPLACE")

    def test_vendor_linkname_bridge_red(self):
        self.vendor_private_bridge("internal/modules/b/internal/store")

    def test_vendor_private_package_root_linkname_red(self):
        self.vendor_private_bridge("internal/modules/b/internal")

    def test_vendor_private_symlink_linkname_red(self):
        self.vendor_private_bridge("internal/modules/b/internal/store", alias=True)

    def test_reversed_walk_keeps_owned_directory_alias_diagnostic(self):
        # Retain the real Go witness and the existing strict security marker.
        self.vendor_private_bridge("internal/modules/b/internal/store", alias=True)
        real_walk = checker.os.walk

        def reversed_walk(*args, **kwargs):
            for directory, dirs, files in real_walk(*args, **kwargs):
                # Filesystem order varies across macOS/Linux. Yield the same
                # mutable lists so the checker can control recursive ordering.
                dirs.sort(reverse=True)
                files.sort(reverse=True)
                yield directory, dirs, files

        with mock.patch.object(checker.os, "walk", reversed_walk):
            with self.assertRaisesRegex(checker.GovernanceError, "MODULE_SOURCE_DIRECTORY_ALIAS"):
                checker.check_go(self.registry, self.root.resolve())

    def test_vendor_foreign_directory_private_alias_red(self):
        self.vendor_private_bridge("internal/modules/b/internal/store", vendor_alias="directory")

    def test_vendor_foreign_file_private_alias_red(self):
        self.vendor_private_bridge("internal/modules/b/internal/store", vendor_alias="file")

    def vendor_private_bridge(self, private_directory, alias=False, vendor_alias=None):
        self.source(private_directory + "/store.go",
                    "package store\nfunc secret() int { return 713 }\n")
        target_package = "example.test/shop/" + private_directory
        if alias:
            (self.root / "internal/modules/a/alias").symlink_to(
                self.root / private_directory, target_is_directory=True)
            target_package = "example.test/shop/internal/modules/a/alias"
        if vendor_alias:
            target = self.root / "vendor/x.test/privatealias"
            target.parent.mkdir(parents=True)
            if vendor_alias == "directory":
                target.symlink_to(self.root / private_directory, target_is_directory=True)
            else:
                target.mkdir()
                (target / "store.go").symlink_to(self.root / private_directory / "store.go")
            target_package = "x.test/privatealias"
        self.source("internal/modules/b/module.go",
                    f'package b\nimport _ "{target_package}"\n')
        self.source("vendor/x.test/bridge/bridge.go",
                    f'package bridge\nimport _ "unsafe"\n//go:linkname hidden {target_package}.secret\nfunc hidden() int\nfunc Read() int {{ return hidden() }}\n')
        self.source("vendor/modules.txt",
                    "# x.test/bridge v0.0.0\n## explicit; go 1.27.2\nx.test/bridge\n" +
                    ("# x.test/privatealias v0.0.0\n## explicit; go 1.27.2\nx.test/privatealias\n" if vendor_alias else ""))
        with (self.root / "go.mod").open("a") as output:
            output.write("\nrequire x.test/bridge v0.0.0\n")
            if vendor_alias:
                output.write("require x.test/privatealias v0.0.0\n")
        self.source("internal/modules/a/module.go",
                    'package a\nimport "x.test/bridge"\nfunc Read() int { return bridge.Read() }\n')
        self.source("cmd/probe/main.go",
                    'package main\nimport ("fmt"; "example.test/shop/internal/modules/a"; _ "example.test/shop/internal/modules/b")\nfunc main() { fmt.Println(a.Read()) }\n')
        witness = subprocess.run(["go", "run", "-mod=vendor", "./cmd/probe"],
                                 cwd=self.root, capture_output=True, text=True,
                                 env=dict(os.environ, GOPROXY="off", GOSUMDB="off"), timeout=90)
        self.assertEqual(witness.returncode, 0, witness.stderr)
        self.assertEqual(witness.stdout.strip(), "713")
        self.assert_red("MODULE_SOURCE_DIRECTORY_ALIAS" if alias else
                        "MODULE_PRIVATE_SOURCE_ALIAS" if vendor_alias else "MODULE_LINKNAME_FORBIDDEN")

    def test_registered_go_root_cannot_alias_private_directory(self):
        (self.root / "internal/modules/a/module.go").unlink()
        (self.root / "internal/modules/a").rmdir()
        (self.root / "internal/modules/a").symlink_to(
            self.root / "internal/modules/b/internal/store", target_is_directory=True)
        self.assert_red("MODULE_REGISTRY_INVALID")

    def test_real_directory_private_alias_red(self):
        # A same-module import name can hide a symlink to another module's
        # private directory. Go permits it; resolved Dir must remain the owner.
        (self.root / "internal/modules/a/alias").symlink_to(
            self.root / "internal/modules/b/internal/store", target_is_directory=True)
        self.source("internal/modules/a/module.go",
                    'package a\nimport "example.test/shop/internal/modules/a/alias"\nvar Value = store.Value\n')
        witness = subprocess.run(["go", "test", "./internal/modules/a"],
                                 cwd=self.root, capture_output=True, text=True,
                                 env=dict(os.environ, GOPROXY="off", GOSUMDB="off"), timeout=90)
        self.assertEqual(witness.returncode, 0, witness.stderr)
        self.assert_red("MODULE_SOURCE_DIRECTORY_ALIAS")

    def test_vendor_without_private_linkname_green(self):
        self.source("vendor/x.test/bridge/bridge.go",
                    'package bridge\nimport _ "unsafe"\n//go:linkname timer runtime.nanotime\nfunc timer() int64\nconst Value=1\nconst Text=`//go:linkname hidden example.test/shop/internal/modules/b/internal/store.secret`\n/* //go:linkname hidden example.test/shop/internal/modules/b/internal/store.secret */\n')
        self.source("vendor/modules.txt", "# x.test/bridge v0.0.0\n## explicit; go 1.27.2\nx.test/bridge\n")
        with (self.root / "go.mod").open("a") as output:
            output.write("\nrequire x.test/bridge v0.0.0\n")
        self.source("internal/modules/a/module.go",
                    'package a\nimport "x.test/bridge"\nvar Value=bridge.Value\n')
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_imported_root_artifact_bridge_red(self):
        self.source("work/bridge/bridge.go", 'package bridge\nimport _ "unsafe"\n//go:linkname hidden example.test/shop/internal/modules/b/internal/store.hidden\nfunc hidden() int\nfunc Read() int { return hidden() }\n')
        self.source("internal/modules/a/module.go", 'package a\nimport "example.test/shop/work/bridge"\nvar Value = bridge.Read\n')
        self.assert_red("MODULE_ARTIFACT_SOURCE")

    def test_local_replace_nested_receipt_module_red(self):
        self.source("work/bridge/go.mod", "module x.test/bridge\n\ngo 1.27.2\n")
        self.source("work/bridge/bridge.go", "package bridge\nconst Value=1\n")
        with (self.root / "go.mod").open("a") as output:
            output.write("\nrequire x.test/bridge v0.0.0\nreplace x.test/bridge => ./work/bridge\n")
        self.source("internal/modules/a/module.go", 'package a\nimport "x.test/bridge"\nvar Value=bridge.Value\n')
        self.assert_red("MODULE_NESTED_GO_MODULE")

    def test_linkname_private_access_red(self):
        self.source("internal/modules/a/module.go", 'package a\nimport _ "unsafe"\n//go:linkname hidden example.test/shop/internal/modules/b/internal/store.hidden\nfunc hidden() int\n')
        self.assert_red("MODULE_LINKNAME_FORBIDDEN")

    def test_linkname_private_access_via_unregistered_bridge_red(self):
        self.source("internal/bridge/bridge.go", 'package bridge\nimport _ "unsafe"\n//go:linkname hidden example.test/shop/internal/modules/b/internal/store.hidden\nfunc hidden() int\nfunc Read() int { return hidden() }\n')
        self.source("internal/modules/a/module.go", 'package a\nimport "example.test/shop/internal/bridge"\nvar Value = bridge.Read\n')
        self.assert_red("MODULE_LINKNAME_FORBIDDEN")

    def test_linkname_text_in_literals_and_block_comments_is_green(self):
        self.source("internal/bridge/bridge.go", 'package bridge\n/*\n//go:linkname harmless\n*/\nconst Text = `\n//go:linkname harmless\n`\n')
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_raw_pgxpool_import_red(self):
        # A local fixture supplies the third-party path without a network fetch.
        self.source("vendor/github.com/jackc/pgx/v5/pgxpool/pool.go", "package pgxpool\ntype Pool struct{}\n")
        self.source("vendor/modules.txt", "# github.com/jackc/pgx/v5 v5.10.0\n## explicit; go 1.27.2\ngithub.com/jackc/pgx/v5/pgxpool\n")
        with (self.root / "go.mod").open("a") as output:
            output.write("\nrequire github.com/jackc/pgx/v5 v5.10.0\n")
        self.source("internal/modules/a/module.go", 'package a\nimport "github.com/jackc/pgx/v5/pgxpool"\nvar Pool *pgxpool.Pool\n')
        self.assert_red("MODULE_RAW_POOL_IMPORT")

    def vendor_pgxpool(self):
        self.source("vendor/github.com/jackc/pgx/v5/pgxpool/pool.go", "package pgxpool\ntype Pool struct{}\ntype Conn struct{}\n")
        self.source("vendor/modules.txt", "# github.com/jackc/pgx/v5 v5.10.0\n## explicit; go 1.27.2\ngithub.com/jackc/pgx/v5/pgxpool\n")
        with (self.root / "go.mod").open("a") as output:
            output.write("\nrequire github.com/jackc/pgx/v5 v5.10.0\n")

    def test_pool_reexport_from_adapter_red(self):
        # PR #31 regression: the one package allowed to hold a pool re-exported it.
        self.vendor_pgxpool()
        header = 'package pgxadapter\nimport (\n\t"context"\n\n\t"github.com/jackc/pgx/v5/pgxpool"\n)\nvar _ context.Context\n'
        for name, body in (
                ("alias", "// Pool is the adapter's connection pool.\ntype Pool = pgxpool.Pool\n"),
                ("defined", "type Pool pgxpool.Pool\n"),
                ("factory", "func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) { return nil, nil }\n"),
                ("method", "type Runner struct{ pool *pgxpool.Pool }\nfunc (r Runner) Raw() *pgxpool.Pool { return r.pool }\n"),
                ("field", "type Runner struct{ Pool *pgxpool.Pool }\n"),
                ("embedded", "type Runner struct{ *pgxpool.Pool }\n"),
                ("var", "var Shared *pgxpool.Pool\n")):
            with self.subTest(name=name):
                self.source("internal/tenanttx/pgxadapter/pool.go", header + body)
                self.assert_red("MODULE_RAW_POOL_EXPORT")

    def test_unexported_pool_in_adapter_green(self):
        self.vendor_pgxpool()
        self.source("internal/tenanttx/pgxadapter/pgxadapter.go",
                    'package pgxadapter\nimport "github.com/jackc/pgx/v5/pgxpool"\n'
                    'type Runner struct{ pool *pgxpool.Pool }\n'
                    'func open() (*pgxpool.Pool, error) { return nil, nil }\n'
                    'func Open() (Runner, func(), error) { p, err := open(); return Runner{pool: p}, func() {}, err }\n'
                    'func (r Runner) close() { f := func() *pgxpool.Pool { return r.pool }; _ = f }\n'
                    '// type Pool = pgxpool.Pool\nconst Text = "type Pool = pgxpool.Pool"\n')
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def vendor_pgx(self):
        # Real Go resolution, offline: no checkout go.mod or downloaded dependency.
        self.source("vendor/github.com/jackc/pgx/v5/pgx.go",
                    "package pgx\ntype Conn struct{}\ntype ConnConfig struct{}\n"
                    "type Tx interface{}\nfunc Connect() *Conn { return nil }\n"
                    "func ConnectConfig(*ConnConfig) *Conn { return nil }\n")
        self.source("vendor/modules.txt", "# github.com/jackc/pgx/v5 v5.10.0\n## explicit; go 1.27.2\ngithub.com/jackc/pgx/v5\n")
        with (self.root / "go.mod").open("a") as output:
            output.write("\nrequire github.com/jackc/pgx/v5 v5.10.0\n")

    def test_raw_pgx_connection_selectors_red(self):
        self.vendor_pgx()
        for alias in ("pgx", "driver", "驱动"):
            for expression in (f"(*{alias}.Conn)(nil)", f"{alias}.Connect", f"{alias}.ConnectConfig"):
                with self.subTest(alias=alias, expression=expression):
                    self.source("internal/modules/a/module.go",
                                f'package a\nimport {alias} "github.com/jackc/pgx/v5"\nvar Value = {expression}\n')
                    self.assert_red("MODULE_RAW_CONNECTION")

    def test_raw_pgx_connection_in_test_and_ignored_sources_red(self):
        self.vendor_pgx()
        for name, prefix in (("module_test.go", ""), ("driver.go", "//go:build pgx\n\n"),
                             ("future.go", "//go:build future\n\n")):
            with self.subTest(name=name):
                self.source("internal/modules/a/" + name,
                            prefix + 'package a\nimport driver "github.com/jackc/pgx/v5"\nvar Connection *driver.Conn\n')
                self.assert_red("MODULE_RAW_CONNECTION")
                (self.root / "internal/modules/a" / name).unlink()

    def test_raw_pgx_dot_import_red(self):
        self.vendor_pgx()
        self.source("internal/modules/a/module.go",
                    'package a\nimport . "github.com/jackc/pgx/v5"\nvar Value Tx\n')
        self.assert_red("MODULE_RAW_CONNECTION")

    def test_pgx_tx_and_connection_decoys_green(self):
        self.vendor_pgx()
        self.source("internal/modules/a/module.go",
                    'package a\nimport (driver "github.com/jackc/pgx/v5")\n'
                    'var Value driver.Tx\n// driver.Conn driver.Connect driver.ConnectConfig\n'
                    '/* driver.Conn */\nconst Text = `driver.Conn driver.Connect`\n'
                    'const Quoted = "driver.ConnectConfig"\n')
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_raw_pgx_pool_in_ignored_source_red(self):
        self.source("internal/modules/a/future.go",
                    '//go:build future\n\npackage a\nimport _ "github.com/jackc/pgx/v5/pgxpool"\n')
        self.assert_red("MODULE_RAW_POOL_IMPORT")

    def test_raw_pgx_connection_in_legacy_source_red(self):
        self.vendor_pgx()
        self.registry["modules"][0]["legacy_go_paths"] = ["internal/a"]
        self.source("internal/a/public.go", 'package a\nimport driver "github.com/jackc/pgx/v5"\nvar Value *driver.Conn\n')
        self.assert_red("MODULE_RAW_CONNECTION")

    def test_multihop_bridge_module_root_red(self):
        self.source("internal/modules/b/module.go", "package b\nconst Value=1\n")
        self.source("internal/bridge/bridge.go", 'package bridge\nimport "example.test/shop/internal/bridge2"\nvar Value=bridge2.Value\n')
        self.source("internal/bridge2/bridge.go", 'package bridge2\nimport "example.test/shop/internal/modules/b"\nvar Value=b.Value\n')
        self.source("internal/modules/a/module.go", 'package a\nimport "example.test/shop/internal/bridge"\nvar Value=bridge.Value\n')
        self.assert_red("MODULE_NONCONTRACT_IMPORT")

    def test_bridge_undeclared_contract_red(self):
        self.registry["modules"][0]["dependencies"] = []
        self.source("internal/bridge/bridge.go", 'package bridge\nimport "example.test/shop/internal/modules/b/contract"\nvar Value=contract.Value\n')
        self.source("internal/modules/a/module.go", 'package a\nimport "example.test/shop/internal/bridge"\nvar Value=bridge.Value\n')
        self.assert_red("MODULE_UNDECLARED_DEPENDENCY")

    def test_pgx_test_variant_bridge_undeclared_contract_red(self):
        self.registry["modules"][0]["dependencies"] = []
        self.source("internal/modules/a/module.go", "package a\n")
        self.source("internal/bridge/bridge.go", 'package bridge\nimport "example.test/shop/internal/modules/b/contract"\nvar Value=contract.Value\n')
        self.source("internal/modules/a/bridge_test.go", '//go:build pgx\n\npackage a_test\nimport "example.test/shop/internal/bridge"\nvar Value=bridge.Value\n')
        self.assert_red("MODULE_UNDECLARED_DEPENDENCY")

    def test_bridge_declared_contract_stops_at_foreign_module_green(self):
        self.source("internal/modules/b/contract/port.go", 'package contract\nimport "example.test/shop/internal/modules/b/internal/store"\nvar Value=store.Value\n')
        self.source("internal/bridge/bridge.go", 'package bridge\nimport "example.test/shop/internal/modules/b/contract"\nvar Value=contract.Value\n')
        self.source("internal/modules/a/module.go", 'package a\nimport "example.test/shop/internal/bridge"\nvar Value=bridge.Value\n')
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_bridge_approved_legacy_port_green(self):
        self.registry["modules"][1].update(legacy_go_paths=["internal/b"], approved_public_ports=["internal/b"])
        self.source("internal/b/public.go", "package b\nconst Value=1\n")
        self.source("internal/bridge/bridge.go", 'package bridge\nimport "example.test/shop/internal/b"\nvar Value=b.Value\n')
        self.source("internal/modules/a/module.go", 'package a\nimport "example.test/shop/internal/bridge"\nvar Value=bridge.Value\n')
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_go_status_planned_loaded_target_red(self):
        self.registry["modules"][0]["go_status"] = "planned"
        self.assert_red("MODULE_GO_STATUS_MISMATCH")

    def test_go_status_implemented_empty_target_red(self):
        self.registry["modules"].append(module("empty"))
        self.assert_red("MODULE_GO_STATUS_MISMATCH")

    def test_go_status_test_only_target_red(self):
        self.source("internal/modules/onlytests/only_test.go", "package onlytests\n")
        target = module("onlytests")
        self.registry["modules"].append(target)
        for status in ("implemented", "planned"):
            with self.subTest(status=status):
                target["go_status"] = status
                self.assert_red("MODULE_GO_STATUS_MISMATCH")

    def test_go_status_pgx_only_target(self):
        self.source("internal/modules/tagged/module.go", "//go:build pgx\n\npackage tagged\n")
        target = module("tagged")
        self.registry["modules"].append(target)
        target["go_status"] = "planned"
        self.assert_red("MODULE_GO_STATUS_MISMATCH")
        target["go_status"] = "implemented"
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_go_status_legacy_does_not_promote_target_green(self):
        target = module("legacy")
        target.update(go_status="planned", legacy_go_paths=["internal/legacy"])
        self.registry["modules"].append(target)
        self.source("internal/legacy/public.go", "package legacy\n")
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_go_status_custom_tag_does_not_claim_implemented(self):
        self.source("internal/modules/tagged/module.go", "//go:build future\n\npackage tagged\n")
        target = module("tagged")
        self.registry["modules"].append(target)
        self.assert_red("MODULE_GO_STATUS_MISMATCH")
        target["go_status"] = "planned"
        self.assert_red("MODULE_GO_STATUS_MISMATCH")

    def test_go_status_planned_ignored_source_red(self):
        self.source("internal/modules/future/ignored.go", "//go:build ignore\n\npackage future\n")
        target = module("future")
        target["go_status"] = "planned"
        self.registry["modules"].append(target)
        self.assert_red("MODULE_GO_STATUS_MISMATCH")

    def test_raw_pgx_default_and_raw_string_import_red(self):
        self.vendor_pgx()
        for imported in ('"github.com/jackc/pgx/v5"', '`github.com/jackc/pgx/v5`',
                         '"github.com/jackc/pgx/v5"'.replace('github', r'\x67ithub')):
            with self.subTest(imported=imported):
                self.source("internal/modules/a/module.go", f"package a\nimport {imported}\nvar Value *pgx /* space */ . Conn\n")
                self.assert_red("MODULE_RAW_CONNECTION")

    def test_multihop_bridge_private_import_red(self):
        self.source("internal/bridge/bridge.go", 'package bridge\nimport "example.test/shop/internal/bridge2"\nvar Value=bridge2.Value\n')
        self.source("internal/bridge2/bridge.go", 'package bridge2\nimport "example.test/shop/internal/modules/b/internal/store"\nvar Value=store.Value\n')
        self.source("internal/modules/a/module.go", 'package a\nimport "example.test/shop/internal/bridge"\nvar Value=bridge.Value\n')
        self.assert_red("MODULE_PRIVATE_IMPORT")

    def test_bridge_foreign_implementation_dependency_not_attributed_to_caller(self):
        self.registry["modules"].append(module("c"))
        self.registry["modules"][1]["dependencies"] = ["c"]
        self.source("internal/modules/c/contract/port.go", "package contract\nconst Value=1\n")
        self.source("internal/modules/b/contract/port.go", 'package contract\nimport "example.test/shop/internal/modules/c/contract"\nvar Value=contract.Value\n')
        self.source("internal/bridge/bridge.go", 'package bridge\nimport "example.test/shop/internal/modules/b/contract"\nvar Value=contract.Value\n')
        self.source("internal/modules/a/module.go", 'package a\nimport "example.test/shop/internal/bridge"\nvar Value=bridge.Value\n')
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_go_status_planned_ignored_filename_red(self):
        target = module("future")
        target["go_status"] = "planned"
        self.registry["modules"].append(target)
        self.source("internal/modules/future/_ignored.go", "package future\n")
        self.assert_red("MODULE_GO_STATUS_MISMATCH")

    def test_raw_pgx_connection_in_owned_source_file_alias_red(self):
        self.vendor_pgx()
        self.source("shared/connection.go", 'package a\nimport "github.com/jackc/pgx/v5"\nvar Connection *pgx.Conn\n')
        (self.root / "internal/modules/a/connection.go").symlink_to(self.root / "shared/connection.go")
        self.assert_red("MODULE_RAW_CONNECTION")

    def test_go_status_planned_directory_alias_red(self):
        (self.root / "internal/modules/a/module.go").unlink()
        self.registry["modules"][0]["go_status"] = "planned"
        self.source("internal/bridgeimpl/source.go", "package bridgeimpl\nconst Value=1\n")
        (self.root / "internal/modules/a/alias").symlink_to(self.root / "internal/bridgeimpl", target_is_directory=True)
        self.source("internal/modules/b/contract/port.go", 'package contract\nimport "example.test/shop/internal/modules/a/alias"\nconst Value=bridgeimpl.Value\n')
        self.assert_red("MODULE_SOURCE_DIRECTORY_ALIAS")

    def test_raw_pgx_connection_directory_alias_red(self):
        self.vendor_pgx()
        self.source("internal/bridgeimpl/source.go", 'package bridgeimpl\nimport "github.com/jackc/pgx/v5"\nvar Connection *pgx.Conn\n')
        (self.root / "internal/modules/a/alias").symlink_to(self.root / "internal/bridgeimpl", target_is_directory=True)
        self.source("internal/modules/a/module.go", 'package a\nimport "example.test/shop/internal/modules/a/alias"\nvar Value=bridgeimpl.Connection\n')
        # The first-party connection ban now rejects the bridge source itself,
        # before the alias rule (still covered by the other alias tests).
        result = self.run_gate()
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertRegex(result.stderr, "MODULE_(SOURCE_DIRECTORY_ALIAS|RAW_CONNECTION)")

    def test_legacy_source_directory_alias_red(self):
        self.registry["modules"][0]["legacy_go_paths"] = ["internal/a"]
        self.source("internal/a/public.go", "package a\n")
        self.source("internal/bridgeimpl/source.go", "package bridgeimpl\nconst Value=1\n")
        (self.root / "internal/a/alias").symlink_to(self.root / "internal/bridgeimpl", target_is_directory=True)
        self.assert_red("MODULE_SOURCE_DIRECTORY_ALIAS")

    def test_explicit_legacy_public_port_green(self):
        self.registry["modules"][1].update(legacy_go_paths=["internal/b"],
                                           approved_public_ports=["internal/b"])
        self.source("internal/b/public.go", "package b\nconst Value = 1\n")
        self.source("internal/modules/a/module.go", 'package a\nimport "example.test/shop/internal/b"\nvar Value = b.Value\n')
        self.assertEqual(self.run_gate().returncode, 0)

    def test_legacy_public_port_still_requires_declared_dependency(self):
        self.registry["modules"][1].update(legacy_go_paths=["internal/b"],
                                           approved_public_ports=["internal/b"])
        self.registry["modules"][0]["dependencies"] = []
        self.source("internal/b/public.go", "package b\nconst Value = 1\n")
        self.source("internal/modules/a/module.go", 'package a\nimport "example.test/shop/internal/b"\nvar Value = b.Value\n')
        self.assert_red("MODULE_UNDECLARED_DEPENDENCY")

    def test_legacy_private_port_red(self):
        self.registry["modules"][1].update(legacy_go_paths=["internal/b"],
                                           approved_public_ports=["internal/b"])
        self.source("internal/b/internal/store/private.go", "package store\nconst Value = 1\n")
        self.source("internal/modules/a/module.go", 'package a\nimport "example.test/shop/internal/b/internal/store"\nvar Value = store.Value\n')
        self.assert_red("MODULE_PRIVATE_IMPORT")

    def test_legacy_source_import_is_checked(self):
        self.registry["modules"][0].update(legacy_go_paths=["internal/a"])
        self.registry["modules"][0]["dependencies"] = []
        self.source("internal/a/public.go", 'package a\nimport "example.test/shop/internal/modules/b/contract"\nvar Value = contract.Value\n')
        self.assert_red("MODULE_UNDECLARED_DEPENDENCY")

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

    def test_openapi_unregistered_operation_red(self):
        with (self.root / "api/openapi.yaml").open("a") as output:
            output.write("  /other:\n    post:\n      operationId: unregisteredReviewProbe\n")
        self.assert_red("MODULE_UNREGISTERED_OPERATION")

    def test_renamed_planned_operation_leaves_existing_operation_unowned(self):
        self.registry["modules"][0]["operations"][0]["operation_id"] = "orphanThings"
        self.assert_red("MODULE_MISSING_OPERATION")

    def test_openapi_renamed_operation_red(self):
        path = self.root / "api/openapi.yaml"
        path.write_text(path.read_text().replace("listThings", "orphanThings"))
        self.assert_red("MODULE_MISSING_OPERATION")

    def test_duplicate_operation_owner_red(self):
        self.registry["modules"][1]["operations"] = [
            {"operation_id": "listThings", "status": "planned"}]
        self.assert_red("MODULE_REGISTRY_INVALID")

    def test_openapi_colon_and_quoted_path_keys_are_green(self):
        for key in ('/things/{id}:cancel', '"/things/{id}:cancel"', "'/things/{id}:cancel'"):
            with self.subTest(key=key):
                (self.root / "api/openapi.yaml").write_text(
                    f"paths:\n  {key}:\n    post:\n      operationId: listThings\n")
                result = self.run_gate()
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

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

    def test_missing_planned_operation_red(self):
        self.registry["modules"][0]["operations"] = [
            {"operation_id": "listThings", "status": "implemented"},
            {"operation_id": "future", "status": "planned"}]
        self.assert_red("MODULE_MISSING_OPERATION")

    def test_operation_status_does_not_change_id_accounting(self):
        self.registry["modules"][0]["operations"][0]["status"] = "implemented"
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
        self.assertIn("python3 tests/test_module_sql.py", (ROOT / "scripts/check_all.sh").read_text())
        self.assertIn("python3 scripts/check_modules.py --tables", (ROOT / "scripts/check_pg.sh").read_text())


    def test_openapi_action_path_quote_styles_and_double_colon_are_green(self):
        # A colon followed by a non-space is a path character, not YAML's
        # mapping separator. Each variant is an independent valid Path Item.
        cases = (
            ('/orders/{id}:cancel', 'post', 'cancelOrder'),
            ('"/orders/{id}:refund"', 'put', 'refundOrder'),
            ("'/orders/{id}:archive'", 'delete', 'archiveOrder'),
            ('/orders/{id}:preview:validate', 'patch', 'validateOrderPreview'),
            ('"/orders/{id}:preview:commit"', 'get', 'getOrderPreviewCommit'),
            ("'/orders/{id}:preview:discard'", 'head', 'headOrderPreviewDiscard'),
        )
        path = self.root / 'api/openapi.yaml'
        for key, method, operation in cases:
            with self.subTest(key=key, method=method):
                path.write_text(f'openapi: 3.1.0\npaths:\n  {key}:\n    {method}:\n      operationId: {operation}\n')
                self.assertEqual(set(checker.openapi_operations(path)), {operation})

    def test_openapi_distinct_action_paths_keep_independent_methods_and_ids(self):
        path = self.root / 'api/openapi.yaml'
        path.write_text(
            'paths:\n'
            '  /orders/{id}:cancel:\n    post:\n      operationId: cancelOrder\n'
            '  "/orders/{id}:refund":\n    put:\n      operationId: refundOrder\n'
            "  '/orders/{id}:archive':\n    delete:\n      operationId: archiveOrder\n"
            '  /orders/{id}:preview:validate:\n    patch:\n      operationId: validateOrderPreview\n')
        self.assertEqual(set(checker.openapi_operations(path)),
                         {'cancelOrder', 'refundOrder', 'archiveOrder', 'validateOrderPreview'})

    def test_openapi_action_path_operations_reconcile_both_directions(self):
        path = self.root / 'api/openapi.yaml'
        path.write_text(
            'paths:\n'
            '  /orders/{id}:cancel:\n    post:\n      operationId: cancelOrder\n'
            '  "/orders/{id}:preview:validate":\n    patch:\n      operationId: validateOrderPreview\n')
        operations = [{'operation_id': 'cancelOrder', 'status': 'planned'},
                      {'operation_id': 'validateOrderPreview', 'status': 'implemented'}]
        registry = {'modules': [{'operations': operations}]}
        checker.check_operations(registry, path)
        with self.assertRaisesRegex(checker.GovernanceError, 'MODULE_MISSING_OPERATION'):
            checker.check_operations({'modules': [{'operations': operations + [
                {'operation_id': 'missingOrderAction', 'status': 'planned'}]}]}, path)
        with self.assertRaisesRegex(checker.GovernanceError, 'MODULE_UNREGISTERED_OPERATION'):
            checker.check_operations({'modules': [{'operations': operations[:1]}]}, path)

    def vendor_pgx_and_pool(self):
        self.vendor_pgx()
        self.source("vendor/github.com/jackc/pgx/v5/pgxpool/pool.go", "package pgxpool\ntype Pool struct{}\n")
        with (self.root / "vendor/modules.txt").open("a") as output:
            output.write("github.com/jackc/pgx/v5/pgxpool\n")

    def test_raw_pool_in_unregistered_bridge_red(self):
        # Review probe on f72ff34: a first-party bridge outside business
        # modules held the pool for its caller and every gate stayed green.
        self.vendor_pgx_and_pool()
        self.source("internal/bridgex/bridge.go", 'package bridgex\nimport "github.com/jackc/pgx/v5/pgxpool"\nvar Pool *pgxpool.Pool\n')
        self.source("internal/modules/a/bridge.go", 'package a\nimport "example.test/shop/internal/bridgex"\nvar Pool = bridgex.Pool\n')
        self.assert_red("MODULE_RAW_POOL_IMPORT")

    def test_raw_connect_in_standalone_first_party_package_red(self):
        self.vendor_pgx()
        for name in ("internal/bridgez/bridge.go", "cmd/tool/main.go", "internal/db/db.go"):
            with self.subTest(name=name):
                self.source(name, 'package bridge\nimport "github.com/jackc/pgx/v5"\nvar Connect = pgx.Connect\n')
                self.assert_red("MODULE_RAW_CONNECTION")
                (self.root / name).unlink()

    def test_allowlisted_pool_adapter_and_db_integration_test_green(self):
        self.vendor_pgx_and_pool()
        self.source("internal/tenanttx/pgxadapter/adapter.go",
                    'package pgxadapter\nimport (\n\t"github.com/jackc/pgx/v5"\n\t"github.com/jackc/pgx/v5/pgxpool"\n)\n'
                    'type Runner struct{ pool *pgxpool.Pool }\nvar Connect = pgx.Connect\n')
        self.source("internal/db/db.go", 'package db\nimport "github.com/jackc/pgx/v5"\nvar Tx pgx.Tx\n')
        self.source("internal/db/db_test.go", 'package db\nimport "github.com/jackc/pgx/v5"\nvar Connect = pgx.Connect\n')
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def generated_server(self):
        self.source("internal/platform/httpapi/generated/api.gen.go", "package generated\nfunc HandlerFromMux() {}\n")

    def test_generated_server_import_outside_httpapi_red(self):
        self.generated_server()
        for name in ("cmd/api/main.go", "internal/modules/a/mount.go"):
            with self.subTest(name=name):
                self.source(name, 'package a\nimport "example.test/shop/internal/platform/httpapi/generated"\nvar Mount = generated.HandlerFromMux\n')
                self.assert_red("MODULE_GENERATED_SERVER_IMPORT")
                (self.root / name).unlink()

    def test_generated_server_import_from_httpapi_and_tests_green(self):
        self.generated_server()
        importer = 'import "example.test/shop/internal/platform/httpapi/generated"\nvar Mount = generated.HandlerFromMux\n'
        self.source("internal/platform/httpapi/router.go", "package httpapi\n" + importer)
        self.source("cmd/api/main_test.go", "package main\n" + importer)
        self.source("cmd/api/main.go", "package main\nfunc main() {}\n")
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_operation_status_drift_red(self):
        path = self.root / "api/openapi.yaml"
        path.write_text(path.read_text() + "      x-opensite-status: implemented\n")
        self.assert_red("MODULE_OPERATION_STATUS_MISMATCH")
        path.write_text(path.read_text().replace("implemented", "planned"))
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

if __name__ == "__main__":
    unittest.main(verbosity=2)
