#!/usr/bin/env python3
"""Offline orchestration gates for backup/restore; real SQL is gated by ops_drill.

Depends on stdlib + scripts.ops_backup/common; called by check_all. Never opens PG.
Fault probes require corruption and occupied targets to fail before restore writes.
"""
import hashlib
from contextlib import nullcontext
import io
import importlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
import secrets
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "scripts"))
backup = importlib.import_module("ops_backup")
common = importlib.import_module("ops_common")


class BackupTests(unittest.TestCase):
    def migration_history(self):
        return {path.stem: hashlib.sha256(path.read_bytes()).hexdigest()
                for path in sorted((backup.ROOT / "migrations").glob('[0-9][0-9][0-9]_*.sql'))}

    def manifest(self, archive):
        history = self.migration_history()
        return {"format": 2, "archive": archive.name,
                "application_migrations": history,
                "archive_sha256": hashlib.sha256(archive.read_bytes()).hexdigest(),
                "tables": {"app.tenants": {"rows": 2, "sha256": "a" * 64},
                           "public.schema_migrations": {"rows": len(history), "sha256": "b" * 64},
                           "queue.river_migration": {"rows": 7, "sha256": "c" * 64},
                           "queue.river_job": {"rows": 2, "sha256": "d" * 64}}}

    def fixture(self, root):
        archive = root / "fixture.dump"
        archive.write_bytes(b"PGDMP fake offline archive")
        manifest = root / "fixture.dump.manifest.json"
        manifest.write_text(json.dumps(self.manifest(archive)))
        return archive, manifest

    def test_older_application_history_refuses_before_database_access(self):
        with tempfile.TemporaryDirectory() as root:
            archive, manifest = self.fixture(Path(root))
            content = self.manifest(archive)
            del content["application_migrations"][max(content["application_migrations"])]
            content["tables"]["public.schema_migrations"]["rows"] -= 1
            manifest.write_text(json.dumps(content))
            with patch.object(backup, "psql") as psql, patch.object(backup, "pg_command") as pg:
                with self.assertRaisesRegex(common.OpsError, "application migration history mismatch"):
                    backup.restore(archive, manifest)
                psql.assert_not_called()
                pg.assert_not_called()

    def test_drifted_application_checksum_refuses_before_database_access(self):
        with tempfile.TemporaryDirectory() as root:
            archive, manifest = self.fixture(Path(root))
            content = self.manifest(archive)
            content["application_migrations"][max(content["application_migrations"])] = "f" * 64
            manifest.write_text(json.dumps(content))
            with patch.object(backup, "psql") as psql:
                with self.assertRaisesRegex(common.OpsError, "application migration history mismatch"):
                    backup.restore(archive, manifest)
                psql.assert_not_called()

    def test_application_history_read_uses_exported_snapshot(self):
        history = {"001_identity": "a" * 64}
        with patch.object(backup, "psql", return_value=json.dumps(history)) as psql:
            self.assertEqual(backup.application_history("opensite", "00000003-00000007-1"), history)
            self.assertIn("SET TRANSACTION SNAPSHOT '00000003-00000007-1'", psql.call_args.args[0])
            self.assertIn("public.schema_migrations", psql.call_args.args[0])

    def test_changed_source_history_refuses_before_forward_runner(self):
        with tempfile.TemporaryDirectory() as root:
            archive, manifest = self.fixture(Path(root))
            expected = self.manifest(archive)["application_migrations"]
            changed = {**expected, "999_drift": "f" * 64}
            with patch.object(backup, "cluster_preflight"), \
                    patch.object(backup, "restore_provisioning_sql", return_value="PROVISION"), \
                    patch.object(backup, "psql", side_effect=["t", "t", "", ""]), \
                    patch.object(backup, "pg_command"), \
                    patch.object(backup, "verify_river"), \
                    patch.object(backup, "table_manifest", return_value=self.manifest(archive)["tables"]), \
                    patch.object(backup, "source_migrations", create=True, side_effect=[expected, changed]), \
                    patch.object(backup, "migrate") as migrate:
                with self.assertRaisesRegex(common.OpsError, "application migration history mismatch"):
                    backup.restore(archive, manifest)
                migrate.assert_not_called()

    def test_new_backup_can_record_older_history_but_restore_rejects_it(self):
        with tempfile.TemporaryDirectory() as root:
            source_history = {path.stem: hashlib.sha256(path.read_bytes()).hexdigest()
                for path in sorted((backup.ROOT / "migrations").glob('[0-9][0-9][0-9]_*.sql'))}
            source_history.pop(max(source_history))
            tables = {"app.tenants": {"rows": 2, "sha256": "a" * 64},
                      "public.schema_migrations": {"rows": len(source_history), "sha256": "b" * 64},
                      "queue.river_migration": {"rows": 7, "sha256": "c" * 64},
                      "queue.river_job": {"rows": 2, "sha256": "d" * 64}}
            def dump(*args, **kwargs):
                kwargs["stdout"].write(b"PGDMP offline archive")
            with patch.object(backup, "cluster_preflight"), \
                    patch.object(backup, "snapshot", return_value=nullcontext("00000003-00000007-1")), \
                    patch.object(backup, "pg_command", side_effect=dump), \
                    patch.object(backup, "table_manifest", return_value=tables), \
                    patch.object(backup, "application_history", return_value=source_history) as history:
                archive, manifest = backup.backup(root, "opensite")
                history.assert_called_once_with("opensite", "00000003-00000007-1")
            content = backup.read_manifest(archive, manifest)
            self.assertEqual(content["format"], 2)
            self.assertEqual(content["application_migrations"], source_history)
            with patch.object(backup, "psql") as psql:
                with self.assertRaisesRegex(common.OpsError, "application migration history mismatch"):
                    backup.restore(archive, manifest)
                psql.assert_not_called()

    def test_legacy_manifest_is_rejected_before_database_access(self):
        with tempfile.TemporaryDirectory() as root:
            archive, manifest = self.fixture(Path(root))
            content = self.manifest(archive)
            content["format"] = 1
            del content["application_migrations"]
            manifest.write_text(json.dumps(content))
            with patch.object(backup, "psql") as psql:
                with self.assertRaisesRegex(common.OpsError, "format 2"):
                    backup.restore(archive, manifest)
                psql.assert_not_called()

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
                    patch.object(backup, "verify_river", create=True, side_effect=lambda _: events.append("river-verify")) as river, \
                    patch.object(backup, "table_manifest", side_effect=lambda _: (events.append("manifest") or self.manifest(archive)["tables"])):
                backup.restore(archive, manifest)
                self.assertEqual(events[0], "preflight")
                self.assertTrue(any('CREATE DATABASE "opensite" TEMPLATE template0' in x for x in events))
                self.assertIn("--single-transaction", pg.call_args.args[0])
                self.assertNotIn("--no-acl", pg.call_args.args[0])
                self.assertNotIn("--no-owner", pg.call_args.args[0])
                migrate.assert_called_once_with("opensite")
                river.assert_called_once_with("opensite")
                self.assertLess(events.index("migrate"), events.index("river-verify"))
                self.assertEqual(events[events.index("river-verify") + 1], "manifest")
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

    def test_missing_returns_secret_refuses_restore_before_target_ddl(self):
        from ops_deploy import LOGIN_ROLES
        with tempfile.TemporaryDirectory() as root:
            archive, manifest = self.fixture(Path(root))
            env = {f'OPENSITE_{kind}_DB_PASSWORD': secrets.token_urlsafe(32) for kind in LOGIN_ROLES}
            env.update(POSTGRES_PASSWORD=secrets.token_urlsafe(32), OPENSITE_RETURNS_DB_PASSWORD='')
            with patch.dict(os.environ, env, clear=True), patch.object(backup, 'cluster_preflight'), \
                    patch.object(backup, 'psql') as psql, patch.object(backup, 'pg_command') as pg:
                with self.assertRaisesRegex(common.OpsError, 'INVALID_ENV_SECRET', msg='RETURNS_RESTORE_RED'):
                    backup.restore(archive, manifest)
                psql.assert_not_called()
                pg.assert_not_called()

    def test_missing_customer_auth_secret_refuses_restore_before_target_ddl(self):
        from ops_deploy import LOGIN_ROLES
        with tempfile.TemporaryDirectory() as root:
            archive, manifest = self.fixture(Path(root))
            env = {f'OPENSITE_{kind}_DB_PASSWORD': secrets.token_urlsafe(32) for kind in LOGIN_ROLES}
            env['POSTGRES_PASSWORD'] = secrets.token_urlsafe(32)
            env['OPENSITE_CUSTOMER_AUTH_DB_PASSWORD'] = str()
            with patch.dict(os.environ, env, clear=True), patch.object(backup, 'cluster_preflight'), \
                    patch.object(backup, 'psql') as psql, patch.object(backup, 'pg_command') as pg:
                with self.assertRaisesRegex(common.OpsError, 'INVALID_ENV_SECRET',
                                            msg='CUSTOMER_AUTH_RESTORE_RED: missing customer capability credential accepted'):
                    backup.restore(archive, manifest)
                psql.assert_not_called()
                pg.assert_not_called()

    def test_tainted_login_refuses_before_restore_target_ddl(self):
        from ops_deploy import DeployError, LOGIN_ROLES
        with tempfile.TemporaryDirectory() as root:
            archive, manifest = self.fixture(Path(root))
            env = {f"OPENSITE_{kind}_DB_PASSWORD": f"nonsecret_fixture_{kind}_123456789" for kind in LOGIN_ROLES}
            with patch.dict(os.environ, env), patch.object(backup, "cluster_preflight"), \
                    patch("ops_deploy.audit_login_roles", create=True, side_effect=DeployError("TAINTED_LOGIN_ROLE")) as audit, \
                    patch.object(backup, "psql") as psql, patch.object(backup, "pg_command") as pg:
                with self.assertRaisesRegex(common.OpsError, "TAINTED_LOGIN_ROLE"):
                    backup.restore(archive, manifest)
                audit.assert_called_once_with("opensite", fetch=psql)
                psql.assert_not_called()
                pg.assert_not_called()

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
                    "public.schema_migrations": {"rows": len(self.migration_history()), "sha256": "b" * 64}}
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

    def test_manifest_requires_queue_history_and_job_witness(self):
        with tempfile.TemporaryDirectory() as root:
            archive, manifest = self.fixture(Path(root))
            for omitted in ("queue.river_migration", "queue.river_job"):
                content = self.manifest(archive)
                del content["tables"][omitted]
                manifest.write_text(json.dumps(content))
                with patch.object(backup, "psql") as psql:
                    with self.assertRaisesRegex(common.OpsError, "queue"):
                        backup.restore(archive, manifest)
                    psql.assert_not_called()

    def test_queue_job_and_history_changes_fail_manifest_comparison(self):
        expected = {
            "queue.river_job": {"rows": 2, "sha256": "d" * 64},
            "queue.river_migration": {"rows": 7, "sha256": "c" * 64}}
        for key in expected:
            actual = {**expected, key: {"rows": expected[key]["rows"], "sha256": "e" * 64}}
            with self.assertRaisesRegex(common.OpsError, "manifest mismatch"):
                backup.compare_tables(expected, actual)

    def test_all_table_inventory_contains_queue(self):
        self.assertIn("'queue'", backup.TABLES)

    def test_live_manifest_refuses_omitted_queue_witness(self):
        tables = {"app.tenants": {"rows": 2, "sha256": "a" * 64},
                  "public.schema_migrations": {"rows": len(self.migration_history()), "sha256": "b" * 64}}
        output = "\n".join(json.dumps({"table": table, **receipt}) for table, receipt in tables.items())
        with patch.object(backup, "psql", return_value=output):
            with self.assertRaisesRegex(common.OpsError, "queue"):
                backup.table_manifest("opensite")

    def test_river_verify_prefers_compiled_container_binary(self):
        ready = subprocess.CompletedProcess([], 0, b"available", b"")
        with patch.dict(os.environ, {"POSTGRES_PASSWORD": "stdin-secret"}), \
                patch.object(common, "pg_command", return_value=ready) as pg:
            common.verify_river("opensite")
            self.assertEqual(pg.call_count, 2)
            verify = pg.call_args.args[0]
            self.assertTrue(any('/usr/local/bin/river-migrate --verify' in arg for arg in verify))
            self.assertNotIn("stdin-secret", str(pg.call_args))

    def test_river_verify_fallback_targets_configured_migrator_without_host_go(self):
        missing = subprocess.CompletedProcess([], 0, b"missing", b"")
        success = subprocess.CompletedProcess([], 0, b"PASS", b"")
        env = {"DATABASE_URL": "must-not-connect-to-this", "OPENSITE_ENV_FILE": "/tmp/configured.env",
               "COMPOSE_PROJECT_NAME": "c23atest"}
        with patch.dict(os.environ, env), patch.object(common, "pg_command", return_value=missing), \
                patch.object(common.subprocess, "run", return_value=success) as run:
            common.verify_river("opensite")
            command = run.call_args.args[0]
            self.assertEqual(command[-2:], ["migrate", "verify"])
            self.assertIn("--no-deps", command)
            self.assertIn("--rm", command)
            self.assertIn("OPENSITE_DB=opensite", command)
            self.assertIn("--env-file", command)
            self.assertIn("/tmp/configured.env", command)
            self.assertNotIn("must-not-connect-to-this", str(command))
            self.assertGreater(run.call_args.kwargs["timeout"], 0)

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

    def test_restore_archive_uses_file_stdin_without_read_bytes(self):
        with tempfile.TemporaryDirectory() as root:
            archive, manifest = self.fixture(Path(root))
            tables = self.manifest(archive)["tables"]
            streams = []
            def recover(args, **kwargs):
                self.assertNotIn("input", kwargs)
                stream = kwargs["stdin"]
                streams.append(stream)
                self.assertEqual(stream.read(1024), b"PGDMP fake offline archive")
            with patch.object(backup, "cluster_preflight"), \
                    patch.object(backup, "restore_provisioning_sql", return_value="PROVISION"), \
                    patch.object(backup, "psql", side_effect=["t", "t", "", ""]), \
                    patch.object(backup, "table_manifest", return_value=tables), \
                    patch.object(backup, "migrate"), patch.object(backup, "verify_river"), \
                    patch.object(backup, "pg_command", side_effect=recover), \
                    patch.object(Path, "read_bytes", side_effect=AssertionError("whole-file reads forbidden")):
                backup.restore(archive, manifest)
            self.assertEqual(len(streams), 1)
            self.assertTrue(streams[0].closed)

    def test_file_transport_is_private_bounded_and_password_stdin_only(self):
        class BoundedSource(io.BytesIO):
            def read(inner, size=-1):
                self.assertGreater(size, 0)
                self.assertLessEqual(size, 1024 * 1024)
                return super().read(size)
        archive = b"PGDMP" + b"fixture" * 300000
        source = BoundedSource(archive)
        streams = []
        result = subprocess.CompletedProcess([], 0, b"ok", b"")
        def run(args, **kwargs):
            self.assertNotIn("input", kwargs)
            stream = kwargs["stdin"]
            streams.append(stream)
            self.assertEqual(os.fstat(stream.fileno()).st_mode & 0o777, 0o600)
            self.assertEqual(stream.read(), b"stdin-secret\n" + archive)
            self.assertNotIn("stdin-secret", str(args))
            self.assertGreater(kwargs["timeout"], 0)
            return result
        with tempfile.TemporaryDirectory() as root, patch.object(common, "ROOT", Path(root)), \
                patch.dict(os.environ, {"POSTGRES_PASSWORD": "stdin-secret"}), \
                patch.object(common.subprocess, "run", side_effect=run):
            self.assertIs(common.pg_command(["pg_restore"], stdin=source), result)
            self.assertEqual(list((Path(root) / "work/C23a").iterdir()), [])
        self.assertTrue(streams[0].closed)
        self.assertFalse(source.closed)

    def test_transport_rejects_both_input_and_file_stdin(self):
        for command in (common.compose, common.pg_command):
            with patch.object(common.subprocess, "run") as run:
                with self.assertRaisesRegex(common.OpsError, "input and stdin"):
                    command(["ps"], input=b"payload", stdin=io.BytesIO(b"archive"))
                run.assert_not_called()

    def test_file_copy_failure_is_opaque_and_closes_private_spool(self):
        class FailingSource(io.BytesIO):
            def read(inner, size=-1):
                raise OSError("stdin-secret private archive details")
        opened = []
        original = tempfile.TemporaryFile
        def spool(*args, **kwargs):
            stream = original(*args, **kwargs)
            opened.append(stream)
            return stream
        with tempfile.TemporaryDirectory() as root, patch.object(common, "ROOT", Path(root)), \
                patch.dict(os.environ, {"POSTGRES_PASSWORD": "stdin-secret"}), \
                patch.object(tempfile, "TemporaryFile", side_effect=spool), \
                patch.object(common.subprocess, "run") as run:
            with self.assertRaises(common.OpsError) as failure:
                common.pg_command(["pg_restore"], stdin=FailingSource())
            self.assertNotIn("stdin-secret", str(failure.exception))
            self.assertNotIn("private archive", str(failure.exception))
            run.assert_not_called()
        self.assertEqual(len(opened), 1)
        self.assertTrue(opened[0].closed)

    def test_file_transport_failures_close_spool_and_redact_client_details(self):
        for outcome in ("timeout", "exit"):
            streams = []
            def fail(args, **kwargs):
                streams.append(kwargs["stdin"])
                if outcome == "timeout":
                    raise subprocess.TimeoutExpired(args, kwargs["timeout"], stderr=b"stdin-secret private archive")
                return subprocess.CompletedProcess(args, 1, b"", b"stdin-secret private archive")
            with tempfile.TemporaryDirectory() as root, patch.object(common, "ROOT", Path(root)), \
                    patch.dict(os.environ, {"POSTGRES_PASSWORD": "stdin-secret"}), \
                    patch.object(common.subprocess, "run", side_effect=fail):
                with self.assertRaises(common.OpsError) as failure:
                    common.pg_command(["pg_restore"], stdin=io.BytesIO(b"PGDMP"))
                self.assertNotIn("stdin-secret", str(failure.exception))
                self.assertNotIn("private archive", str(failure.exception))
            self.assertTrue(streams[0].closed)

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

    def test_dotenv_accepts_explicit_worker_keys_without_evaluation(self):
        with tempfile.TemporaryDirectory() as root:
            Path(root, ".env").write_text("JOB_SIGNING_KEY=abc123\nJOB_TENANTS='tenant-a,tenant-b'\nOUTBOX_ENABLED=false\n")
            with patch.object(common, "ROOT", Path(root)), patch.dict(os.environ, {}, clear=True):
                common.load_environment()
                self.assertEqual(os.environ["JOB_SIGNING_KEY"], "abc123")
                self.assertEqual(os.environ["JOB_TENANTS"], "tenant-a,tenant-b")
                self.assertEqual(os.environ["OUTBOX_ENABLED"], "false")

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
