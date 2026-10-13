#!/usr/bin/env python3
"""Snapshot-consistent custom backups and fresh-database restore verification.

Depends on stdlib + ops_common/container PG18; called by backup.sh/restore.sh and
ops_drill. Reads ALL app/queue tables and both migration histories, restores ACLs.
Never drops/cleans objects, never restores over an existing application database.
"""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import sys

from ops_common import OpsError, ROOT, db_name, load_environment, migrate, pg_command, psql, quote_ident, snapshot, verify_river

TABLES = """SELECT n.nspname AS schema_name, c.relname AS table_name
FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
WHERE c.relkind IN ('r','p') AND
 (n.nspname IN ('app','queue') OR (n.nspname='public' AND c.relname='schema_migrations'))
ORDER BY n.nspname COLLATE "C", c.relname COLLATE "C"
"""

FRESH_DATABASE = """SELECT
 NOT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname !~ '^pg_'
             AND nspname NOT IN ('public','information_schema'))
 AND NOT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
                 WHERE n.nspname='public')
 AND NOT EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
                 WHERE n.nspname='public')
 AND NOT EXISTS (SELECT 1 FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace
                 WHERE n.nspname='public')
 AND NOT EXISTS (SELECT 1 FROM pg_extension WHERE extname <> 'plpgsql')
 AND NOT EXISTS (SELECT 1 FROM pg_stat_activity
                 WHERE datname=current_database() AND pid<>pg_backend_pid());
"""


def cluster_preflight(database):
    from ops_deploy import DeployError, preflight
    try:
        preflight(database, fetch=psql)
    except DeployError as error:
        raise OpsError(str(error)) from None


def restore_provisioning_sql(database):
    from ops_deploy import DeployError, LOGIN_ROLES, provision_sql
    passwords = {kind: os.environ.get(f"OPENSITE_{kind}_DB_PASSWORD", "") for kind in LOGIN_ROLES}
    try:
        return provision_sql(database, passwords)
    except DeployError as error:
        raise OpsError(str(error)) from None


def snapshot_transaction(snapshot_id=None):
    prefix = "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;\n"
    if snapshot_id:
        if not re.fullmatch(r"[0-9A-Fa-f]+-[0-9A-Fa-f]+-[0-9]+", snapshot_id):
            raise OpsError("invalid snapshot identifier")
        prefix += f"SET TRANSACTION SNAPSHOT '{snapshot_id}';\n"
    return prefix


def validate_application_history(history):
    if not isinstance(history, dict) or not history:
        raise OpsError("invalid application migration history")
    for version, checksum in history.items():
        if (not re.fullmatch(r"[0-9]{3}_[a-z][a-z0-9_]*", version)
                or not isinstance(checksum, str) or not re.fullmatch(r"[0-9a-f]{64}", checksum)):
            raise OpsError("invalid application migration history")
    if [int(version[:3]) for version in sorted(history)] != list(range(1, len(history) + 1)):
        raise OpsError("invalid application migration history numbering")
    return dict(sorted(history.items()))


def application_history(database=None, snapshot_id=None):
    sql = snapshot_transaction(snapshot_id) + """
SELECT coalesce(json_object_agg(version, checksum ORDER BY version), '{}'::json)
FROM public.schema_migrations;
COMMIT;
"""
    try:
        history = json.loads(psql(sql, database))
    except (ValueError, TypeError):
        raise OpsError("invalid application migration history response") from None
    return validate_application_history(history)


def source_migrations():
    return validate_application_history({path.stem: digest(path)
        for path in sorted((ROOT / "migrations").glob('[0-9][0-9][0-9]_*.sql'))})


def require_matching_application_history(history):
    if history != source_migrations():
        raise OpsError("application migration history mismatch: restore requires the exact backup source version and checksums")


def table_manifest(database=None, snapshot_id=None):
    prefix = snapshot_transaction(snapshot_id)
    # Hash canonical jsonb row representations; sorting hashes preserves duplicates
    # while excluding physical row order. No row content leaves the container.
    # ponytail: aggregation uses O(rows) server memory; stream sorted hashes if
    # measured table size exceeds PostgreSQL aggregate memory/string limits.
    sql = prefix + """SET LOCAL TIME ZONE 'UTC';
SET LOCAL extra_float_digits=3;
SET LOCAL bytea_output='hex';
SET LOCAL DateStyle='ISO, YMD';
SET LOCAL IntervalStyle='postgres';
""" + """
SELECT format($query$
 SELECT json_build_object('table', %L, 'rows', count(*), 'sha256',
  encode(sha256(convert_to(COALESCE(string_agg(row_hash, '' ORDER BY row_hash COLLATE "C"), ''), 'UTF8')), 'hex'))
 FROM (SELECT encode(sha256(convert_to(to_jsonb(t)::text, 'UTF8')), 'hex') AS row_hash FROM %I.%I t) rows
 $query$, schema_name || '.' || table_name, schema_name, table_name)
FROM (""" + TABLES + ") tables\n\\gexec\nCOMMIT;\n"
    try:
        result = {}
        for line in psql(sql, database).splitlines():
            row = json.loads(line)
            if row["table"] in result:
                raise OpsError("duplicate table in manifest")
            result[row["table"]] = {"rows": row["rows"], "sha256": row["sha256"]}
    except (ValueError, KeyError, TypeError):
        raise OpsError("invalid table manifest response") from None
    if "public.schema_migrations" not in result or not any(key.startswith("app.") for key in result):
        raise OpsError("manifest requires app tables and migration history")
    if not {"queue.river_migration", "queue.river_job"}.issubset(result):
        raise OpsError("manifest requires queue history and job witness")
    return dict(sorted(result.items()))


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def backup(directory=None, database=None):
    name = db_name(database)
    cluster_preflight(name)
    root = Path(directory or os.environ.get("OPENSITE_BACKUP_DIR", ROOT / "work/backups"))
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
    archive = root / f"{name}-{stamp}.dump"
    manifest_path = archive.with_suffix(".dump.manifest.json")
    archive_created = manifest_created = False
    # Exclusive create + private permissions; no global umask mutation.
    try:
        with snapshot(name) as snap:
            fd = os.open(archive, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            archive_created = True
            with os.fdopen(fd, "wb") as stream:
                pg_command(["pg_dump", "-h", "127.0.0.1", "-U", "postgres", "-d", name,
                            "-Fc", "--snapshot=" + snap], stdout=stream)
            tables = table_manifest(name, snap)
            history = application_history(name, snap)
        if not archive.stat().st_size:
            raise OpsError("empty backup archive")
        manifest = {"format": 2, "created_at": datetime.now(timezone.utc).isoformat(),
                    "database": name, "archive": archive.name,
                    "archive_sha256": digest(archive), "tables": tables,
                    "application_migrations": history}
        fd = os.open(manifest_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        manifest_created = True
        with os.fdopen(fd, "w") as stream:
            json.dump(manifest, stream, sort_keys=True, indent=2)
            stream.write("\n")
    except BaseException:
        # Only incomplete files created by this invocation may be removed.
        if archive_created:
            archive.unlink(missing_ok=True)
        if manifest_created:
            manifest_path.unlink(missing_ok=True)
        raise
    return archive, manifest_path


def read_manifest(archive, manifest_path):
    try:
        manifest = json.loads(Path(manifest_path).read_text())
        if manifest["format"] != 2 or manifest["archive"] != Path(archive).name:
            raise OpsError("unsupported or mismatched backup manifest: format 2 application history required")
        history = validate_application_history(manifest["application_migrations"])
        tables = manifest["tables"]
        if not isinstance(tables, dict) or "public.schema_migrations" not in tables:
            raise OpsError("manifest requires migration history")
        if not any(key.startswith("app.") for key in tables):
            raise OpsError("manifest requires app tables")
        if not {"queue.river_migration", "queue.river_job"}.issubset(tables):
            raise OpsError("manifest requires queue history and job witness")
        for table, receipt in tables.items():
            if not (table.startswith(("app.", "queue.")) or table == "public.schema_migrations"):
                raise OpsError("unexpected manifest table")
            if (not isinstance(receipt, dict) or type(receipt.get("rows")) is not int
                    or receipt["rows"] < 0
                    or not re.fullmatch(r"[0-9a-f]{64}", receipt.get("sha256", ""))):
                raise OpsError("invalid table receipt")
        if tables["public.schema_migrations"]["rows"] != len(history):
            raise OpsError("application migration history count mismatch")
        if digest(Path(archive)) != manifest["archive_sha256"]:
            raise OpsError("archive digest mismatch")
    except (OSError, ValueError, KeyError, TypeError):
        raise OpsError("cannot read valid backup manifest/archive") from None
    return manifest


def compare_tables(expected, actual):
    if expected != actual:
        changed = sorted(key for key in set(expected) | set(actual) if expected.get(key) != actual.get(key))
        raise OpsError("all-table manifest mismatch: " + ", ".join(changed))


def restore(archive, manifest_path=None, database=None):
    archive = Path(archive)
    manifest_path = Path(manifest_path or archive.with_suffix(".dump.manifest.json"))
    manifest = read_manifest(archive, manifest_path)
    # Reject upgrade/downgrade/checksum drift before even inspecting the target.
    # A restore never doubles as a forward upgrade of an older valid backup.
    require_matching_application_history(manifest["application_migrations"])
    name = db_name(database)
    if name != db_name():
        raise OpsError("restore target must match assigned OPENSITE_DB")
    cluster_preflight(name)
    provisioning = restore_provisioning_sql(name)  # validate every secret BEFORE any restore DDL
    exists = psql(f"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname='{name}');", "postgres").strip()
    if exists not in {"t", "true", "f", "false"}:
        raise OpsError("cannot inspect restore target")
    if exists in {"f", "false"}:
        # The preflight passed before this first write. A pristine template0
        # target is safe to create; failures never trigger DROP or --clean.
        psql(f"CREATE DATABASE {quote_ident(name)} TEMPLATE template0;", "postgres")
    if psql(FRESH_DATABASE, name).strip() not in {"t", "true"}:
        raise OpsError("restore requires a fresh empty database with no other sessions")
    psql(f"REVOKE CONNECT ON DATABASE {quote_ident(name)} FROM PUBLIC;", name)
    # Preserve owners, grants, RLS and constraints. Roles must already be provisioned
    # by migrations in the new cluster; errors roll back the whole restore.
    pg_command(["pg_restore", "-h", "127.0.0.1", "-U", "postgres", "-d", name,
                "--exit-on-error", "--single-transaction"], input=archive.read_bytes())
    compare_tables(manifest["tables"], table_manifest(name))
    require_matching_application_history(manifest["application_migrations"])
    migrate(name)
    verify_river(name)
    compare_tables(manifest["tables"], table_manifest(name))
    # pg_restore into a pre-created DB does not recover database-level CONNECT
    # privileges. Apply the same env-scoped login policy as normal provisioning.
    psql(provisioning, name)
    return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    create = commands.add_parser("backup")
    create.add_argument("--directory")
    create.add_argument("--database")
    recover = commands.add_parser("restore")
    recover.add_argument("archive", type=Path)
    recover.add_argument("--manifest", type=Path)
    recover.add_argument("--database")
    args = parser.parse_args()
    try:
        load_environment()
        if args.command == "backup":
            archive, manifest_path = backup(args.directory, args.database)
            print(json.dumps({"archive": str(archive), "manifest": str(manifest_path)}))
        else:
            manifest = restore(args.archive, args.manifest, args.database)
            print(json.dumps({"restored": True, "tables_verified": len(manifest["tables"])}))
    except (OpsError, OSError) as error:
        # OS diagnostics may carry input paths; only controlled OpsError is public.
        print(str(error) if isinstance(error, OpsError) else "backup/restore file operation failed", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
