#!/usr/bin/env python3
"""Offline orchestration gates for backup/restore; real SQL is gated by ops_drill.

Depends on stdlib + scripts.ops_backup/common; called by check_all. Never opens PG.
Fault probes require corruption and occupied targets to fail before restore writes.
"""
import hashlib
import io
import importlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "scripts"))
backup = importlib.import_module("ops_backup")
common = importlib.import_module("ops_common")


class BackupTests(unittest.TestCase):
    def manifest(self, archive):
        return {"format": 1, "archive": archive.name,
                "archive_sha256": hashlib.sha256(archive.read_bytes()).hexdigest(),
                "tables": {"app.tenants": {"rows": 2, "sha256": "a" * 64},
                           "public.schema_migrations": {"rows": 23, "sha256": "b" * 64}}}

    def fixture(self, root):
        archive = root / "fixture.dump"
        archive.write_bytes(b"PGDMP fake offline archive")
        manifest = root / "fixture.dump.manifest.json"
        manifest.write_text(json.dumps(self.manifest(archive)))
        return archive, manifest

    def test_corruption_rejected_before_database_access(self):
        with tempfile.TemporaryDirectory() as root:
            archive, manifest = self.fixture(Path(root))
            archive.write_bytes(b"corrupted")
            with patch.object(backup, "psql") as psql:
                with self.assertRaisesRegex(common.OpsError, "digest"):
                    backup.restore(archive, manifest)
                psql.assert_not_called()

    def test_nonempty_target_rejected_before_pg_restore(self):
        with tempfile.TemporaryDirectory() as root:
            archive, manifest = self.fixture(Path(root))
            with patch.object(backup, "cluster_preflight"), \
                    patch.object(backup, "restore_provisioning_sql", return_value="PROVISION"), \
                    patch.object(backup, "psql", side_effect=["t\n", "false\n"]), \
                    patch.object(backup, "pg_command") as pg:
                with self.assertRaisesRegex(common.OpsError, "fresh empty"):
                    backup.restore(archive, manifest)
                pg.assert_not_called()

    def test_absent_target_created_only_after_preflight(self):
        with tempfile.TemporaryDirectory() as root:
            archive, manifest = self.fixture(Path(root))
            events = []
            def query(sql, database):
                events.append(sql)
                if "SELECT EXISTS(SELECT 1 FROM pg_database" in sql:
                    return "f\n"
                if "CREATE DATABASE" in sql:
                    return ""
                return "t\n"
            with patch.object(backup, "cluster_preflight", side_effect=lambda _: events.append("preflight")), \
                    patch.object(backup, "restore_provisioning_sql", return_value="PROVISION"), \
                    patch.object(backup, "psql", side_effect=query), \
                    patch.object(backup, "pg_command", side_effect=lambda *args, **kwargs: events.append("pg_restore")) as pg, \
                    patch.object(backup, "migrate", side_effect=lambda _: events.append("migrate")) as migrate, \
                    patch.object(backup, "table_manifest", side_effect=lambda _: (events.append("manifest") or self.manifest(archive)["tables"])):
                backup.restore(archive, manifest)
                self.assertEqual(events[0], "preflight")
                self.assertTrue(any('CREATE DATABASE "opensite" TEMPLATE template0' in x for x in events))
                self.assertIn("--single-transaction", pg.call_args.args[0])
                self.assertNotIn("--no-acl", pg.call_args.args[0])
                self.assertNotIn("--no-owner", pg.call_args.args[0])
                migrate.assert_called_once_with("opensite")
                revoke_index = next(i for i, event in enumerate(events) if "REVOKE CONNECT" in event)
                self.assertLess(revoke_index, events.index("pg_restore"))
                self.assertEqual(events[-1], "PROVISION")
                self.assertEqual(events[-2], "manifest")

    def test_missing_login_secrets_fail_before_restore_ddl(self):
        with tempfile.TemporaryDirectory() as root:
            archive, manifest = self.fixture(Path(root))
            env = {"OPENSITE_RUNTIME_DB_PASSWORD": "", "OPENSITE_AUTH_DB_PASSWORD": "",
                   "OPENSITE_DOMAIN_DB_PASSWORD": "", "OPENSITE_MEDIA_DB_PASSWORD": ""}
            with patch.dict(os.environ, env), patch.object(backup, "cluster_preflight"), \
                    patch.object(backup, "psql") as psql:
                with self.assertRaisesRegex(common.OpsError, "INVALID_ENV_SECRET"):
                    backup.restore(archive, manifest)
                psql.assert_not_called()

    def test_second_application_database_refuses_target_creation(self):
        from ops_deploy import DeployError
        with tempfile.TemporaryDirectory() as root:
            archive, manifest = self.fixture(Path(root))
            with patch("ops_deploy.preflight", side_effect=DeployError("SECOND_APPLICATION_DATABASE")), \
                    patch.object(backup, "psql") as psql:
                with self.assertRaisesRegex(common.OpsError, "SECOND_APPLICATION_DATABASE"):
                    backup.restore(archive, manifest)
                psql.assert_not_called()

    def test_all_table_mismatch_is_a_hard_failure(self):
        expected = {"app.tenants": {"rows": 2, "sha256": "a" * 64},
                    "public.schema_migrations": {"rows": 23, "sha256": "b" * 64}}
        for actual in ({**expected, "app.tenants": {"rows": 2, "sha256": "c" * 64}},
                       {"app.tenants": expected["app.tenants"]},
                       {**expected, "app.extra": {"rows": 0, "sha256": "d" * 64}}):
            with self.assertRaisesRegex(common.OpsError, "manifest mismatch"):
                backup.compare_tables(expected, actual)

    def test_manifest_requires_migration_history(self):
        with tempfile.TemporaryDirectory() as root:
            archive, manifest = self.fixture(Path(root))
            content = json.loads(manifest.read_text())
            del content["tables"]["public.schema_migrations"]
            manifest.write_text(json.dumps(content))
            with self.assertRaisesRegex(common.OpsError, "migration history"):
                backup.read_manifest(archive, manifest)

    def test_database_identifier_and_system_databases_rejected(self):
        for name in ("postgres", "template0", "template1", "evil; DROP DATABASE foo", ""):
            with self.assertRaises(common.OpsError):
                common.db_name(name)
        self.assertEqual(common.db_name("c23a_restore"), "c23a_restore")

    def test_compose_uses_env_file_timeout_and_redacts_failures(self):
        env = {"POSTGRES_PASSWORD": "never-print-this", "OPENSITE_ENV_FILE": "/tmp/ops.env",
               "COMPOSE_PROJECT_NAME": "c23atest"}
        fail = subprocess.CompletedProcess([], 1, b"", b"never-print-this sql private payload")
        with patch.dict(os.environ, env), patch.object(common.subprocess, "run", return_value=fail) as run:
            with self.assertRaises(common.OpsError) as failure:
                common.compose(["ps"])
            self.assertNotIn("never-print-this", str(failure.exception))
            self.assertNotIn("private payload", str(failure.exception))
            command = run.call_args.args[0]
            self.assertIn("--env-file", command)
            self.assertIn("/tmp/ops.env", command)
            self.assertGreater(run.call_args.kwargs["timeout"], 0)

    def test_password_is_stdin_only(self):
        result = subprocess.CompletedProcess([], 0, b"ok\n", b"")
        with patch.dict(os.environ, {"POSTGRES_PASSWORD": "stdin-secret"}), \
                patch.object(common, "compose", return_value=result) as compose:
            self.assertEqual(common.psql("SELECT 1;"), "ok\n")
            self.assertNotIn("stdin-secret", str(compose.call_args.args))
            self.assertTrue(compose.call_args.kwargs["input"].startswith(b"stdin-secret\n"))

    def test_dotenv_default_loads_secrets_and_environment_wins(self):
        with tempfile.TemporaryDirectory() as root:
            Path(root, ".env").write_text("# local ops\nPOSTGRES_PASSWORD='file-secret'\nOPENSITE_DB=opensite\nCOMPOSE_PROJECT_NAME=from_file\n")
            with patch.object(common, "ROOT", Path(root)), \
                    patch.dict(os.environ, {"COMPOSE_PROJECT_NAME": "explicit"}, clear=True):
                common.load_environment()
                self.assertEqual(common.admin_password(), b"file-secret\n")
                self.assertEqual(os.environ["COMPOSE_PROJECT_NAME"], "explicit")
                self.assertEqual(os.environ["OPENSITE_DB"], "opensite")

    def test_dotenv_configured_file_and_no_execution_or_expansion(self):
        with tempfile.TemporaryDirectory() as root:
            env_file = Path(root, "configured.env")
            for value in ("POSTGRES_PASSWORD=$(touch hidden)", "POSTGRES_PASSWORD=$HOME",
                          "export POSTGRES_PASSWORD=secret", "POSTGRES_PASSWORD=unclosed'",
                          "POSTGRES_PASSWORD=secret # comment", "PATH=/malicious"):
                env_file.write_text("OPENSITE_DB=changed\n" + value + "\n")
                with patch.dict(os.environ, {"OPENSITE_ENV_FILE": str(env_file)}, clear=True):
                    with self.assertRaises(common.OpsError):
                        common.load_environment()
                    self.assertNotIn("OPENSITE_DB", os.environ, "invalid files must not partially mutate environment")
            env_file.write_text('POSTGRES_PASSWORD="configured-secret"\n')
            with patch.dict(os.environ, {"OPENSITE_ENV_FILE": str(env_file)}, clear=True):
                common.load_environment()
                self.assertEqual(common.admin_password(), b"configured-secret\n")

    def test_backup_and_restore_cli_load_dotenv_without_manual_exports(self):
        with tempfile.TemporaryDirectory() as root:
            env_file = Path(root, "configured.env")
            env_file.write_text("POSTGRES_PASSWORD=cli-secret\n")
            def create(*args):
                self.assertEqual(common.admin_password(), b"cli-secret\n")
                return Path(root, "test.dump"), Path(root, "test.dump.manifest.json")
            def recover(*args):
                self.assertEqual(common.admin_password(), b"cli-secret\n")
                return {"tables": {"app.tenants": {}}}
            for argv, name, action in ((["backup"], "backup", create), (["restore", "test.dump"], "restore", recover)):
                with patch.dict(os.environ, {"OPENSITE_ENV_FILE": str(env_file)}, clear=True), \
                        patch.object(sys, "argv", ["ops_backup.py"] + argv), \
                        patch.object(backup, name, side_effect=action), \
                        patch.object(sys, "stdout", io.StringIO()):
                    self.assertEqual(backup.main(), 0)


if __name__ == "__main__":
    unittest.main()
