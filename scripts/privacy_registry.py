#!/usr/bin/env python3
"""Explicit privacy policies and persisted canary witnesses, using stdlib/psql.

Called by CI and the data-rights fixture consumer. The migrated PG catalog is
the column authority; module JSON is the policy authority. No name inference,
business erasure or export code lives here. Fixture writes are CI-only and roll
back by default; --emit-fixture produces input for a consumer's own test DB.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys


PII = {"personal", "contact"}
CLASSES = PII | {"financial_non_contact", "operational", "secret_hash"}
IDENTIFIER = re.compile(r"[a-z][a-z0-9_]*\Z")
PLACEHOLDER = re.compile(r"\{\{canary:([a-z0-9_.]+)\}\}")


def require(ok, marker, detail):
    if not ok:
        raise ValueError(f"{marker}: {detail}")


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "PRIVACY_REGISTRY_INVALID", f"duplicate key {key}")
        result[key] = value
    return result


def read_json(path):
    try:
        return json.loads(path.read_text(), object_pairs_hook=unique_object)
    except (OSError, json.JSONDecodeError) as error:
        raise ValueError(f"PRIVACY_REGISTRY_INVALID: unreadable JSON {path.name}") from error


def field_parts(key):
    parts = key.split(".")
    if len(parts) == 2:
        parts.insert(0, "app")
    require(len(parts) == 3 and all(IDENTIFIER.fullmatch(x) for x in parts),
            "PRIVACY_REGISTRY_INVALID", "unsafe field identifier")
    return parts


def field_key(schema, table, column):
    return f"{table}.{column}" if schema == "app" else f"{schema}.{table}.{column}"


def load_registry(root):
    files = sorted((root / "modules/privacy").glob("*.json"))
    require(files, "PRIVACY_REGISTRY_MISSING", "modules/privacy has no declarations")
    result = {}
    ownership = {}
    module_registry = root / "modules/registry.yaml"
    if module_registry.is_file():
        modules = read_json(module_registry)["modules"]
        ownership = {table: module["name"] for module in modules for table in module["owns_tables"]}
    for path in files:
        document = read_json(path)
        require(isinstance(document, dict) and document.get("version") == 1,
                "PRIVACY_REGISTRY_INVALID", path.name)
        module = document.get("module")
        require(isinstance(module, str) and module == path.stem,
                "PRIVACY_REGISTRY_INVALID", f"module/file mismatch {path.name}")
        structures = document.get("structures", {})
        require(isinstance(structures, dict) and all(isinstance(k, str) and k and
                isinstance(v, str) and v.strip() for k, v in structures.items()),
                "PRIVACY_REGISTRY_INVALID", f"invalid structures in {path.name}")
        tables = document.get("tables")
        require(isinstance(tables, dict) and tables, "PRIVACY_REGISTRY_INVALID", f"no tables in {path.name}")
        for table, body in tables.items():
            require(IDENTIFIER.fullmatch(table) and isinstance(body, dict),
                    "PRIVACY_REGISTRY_INVALID", f"invalid table in {path.name}")
            schema = body.get("schema", "app")
            require(schema in {"app", "queue"}, "PRIVACY_REGISTRY_INVALID", f"unsupported schema {schema}")
            if ownership and schema == "app":
                require(ownership.get(table) == module, "PRIVACY_COLUMN_OWNERSHIP", table)
            if schema == "queue":
                require(module == "reliability" and table == "river_job",
                        "PRIVACY_COLUMN_OWNERSHIP", "only the pinned River job envelope is vendor-owned")
            columns = body.get("columns")
            require(isinstance(columns, dict) and columns, "PRIVACY_REGISTRY_INVALID", f"no columns {table}")
            for column, spec in columns.items():
                require(IDENTIFIER.fullmatch(column) and isinstance(spec, dict),
                        "PRIVACY_REGISTRY_INVALID", f"invalid column {table}")
                key = field_key(schema, table, column)
                require(key not in result, "PRIVACY_REGISTRY_INVALID", f"duplicate column ownership {key}")
                field = {**spec, "module": module}
                if "subject" not in field and "subject" in body:
                    field["subject"] = body["subject"]
                require(field.get("classification") in CLASSES, "PRIVACY_CLASSIFICATION_MISSING", key)
                if "structure" in field:
                    require(isinstance(field["structure"], str) and field["structure"] in structures,
                            "PRIVACY_STRUCTURE_MISSING", key)
                if field["classification"] == "secret_hash":
                    require(field.get("export") == "exclude", "PRIVACY_SECRET_EXPORT", key)
                if field["classification"] in PII:
                    subject = field.get("subject")
                    require(isinstance(subject, dict) and "tenant_key" in subject and
                            (subject["tenant_key"] is None or (isinstance(subject["tenant_key"], str) and
                             IDENTIFIER.fullmatch(subject["tenant_key"]))) and
                            any(isinstance(subject.get(k), str) and subject[k].strip()
                                for k in ("identity_path", "null_subject_policy")),
                            "PRIVACY_SUBJECT_MISSING", key)
                    erase = field.get("erase")
                    require(isinstance(erase, dict) and erase.get("strategy") in
                            {"scrub", "null", "placeholder", "retain"}, "PRIVACY_ERASE_MISSING", key)
                    if erase["strategy"] == "retain":
                        require(isinstance(erase.get("reason"), str) and erase["reason"].strip(),
                                "PRIVACY_RETAIN_REASON_MISSING", key)
                    require(field.get("export") in {"include", "redact", "exclude"},
                            "PRIVACY_EXPORT_MISSING", key)
                    if "canary_value" in field:
                        require(isinstance(field["canary_value"], str) and field["canary_value"],
                                "PRIVACY_CANARY_INVALID", key)
                result[key] = field
    return result


def validate_catalog(registry, rows):
    actual = {}
    for row in rows:
        key = field_key(row.get("table_schema", "app"), row["table_name"], row["column_name"])
        field_parts(key)
        require(key not in actual, "PRIVACY_CATALOG_INVALID", f"duplicate {key}")
        actual[key] = row
    unknown = sorted(actual.keys() - registry.keys())
    phantom = sorted(registry.keys() - actual.keys())
    require(not unknown, "PRIVACY_UNDECLARED_COLUMN", ", ".join(unknown))
    require(not phantom, "PRIVACY_PHANTOM_COLUMN", ", ".join(phantom))
    for key, spec in registry.items():
        tenant_key = (spec.get("subject") or {}).get("tenant_key")
        if tenant_key:
            schema, table, _ = field_parts(key)
            require(field_key(schema, table, tenant_key) in actual, "PRIVACY_TENANT_KEY_UNKNOWN",
                    f"{key} -> {tenant_key}")
    for key, row in actual.items():
        if row["udt_name"] in {"json", "jsonb", "bytea", "_json", "_jsonb", "_bytea"}:
            require(registry[key].get("structure"), "PRIVACY_STRUCTURE_MISSING", key)


def personal_fields(registry):
    return {key: spec for key, spec in registry.items() if spec["classification"] in PII}


def sql_literal(value):
    return "'" + value.replace("'", "''") + "'"


def render_fixture(registry, template, prefix):
    require(re.fullmatch(r"[a-z][a-z0-9_]{0,31}", prefix), "PRIVACY_CANARY_INVALID", "unsafe prefix")
    fields = personal_fields(registry)
    mentioned = set(PLACEHOLDER.findall(template))
    require(not mentioned - fields.keys(), "PRIVACY_FIXTURE_UNKNOWN", ", ".join(sorted(mentioned - fields.keys())))
    require(not fields.keys() - mentioned, "PRIVACY_FIXTURE_UNCOVERED", ", ".join(sorted(fields.keys() - mentioned)))
    canaries = {key: spec.get("canary_value", "cpii_" + prefix[:12] + "_" +
               hashlib.sha256((prefix + ":" + key).encode()).hexdigest()[:12]) for key, spec in fields.items()}
    require(len(set(canaries.values())) == len(canaries), "PRIVACY_CANARY_INVALID", "duplicate canary values")
    sql = PLACEHOLDER.sub(lambda match: sql_literal(canaries[match[1]]), template)
    require("{{canary:" not in sql, "PRIVACY_CANARY_INVALID", "malformed placeholder")
    return sql, canaries


def contains(value, needle):
    if isinstance(value, str):
        return needle in value
    if isinstance(value, bytes):
        return needle.encode() in value
    if isinstance(value, dict):
        return any(contains(k, needle) or contains(v, needle) for k, v in value.items())
    if isinstance(value, list):
        return any(contains(v, needle) for v in value)
    return False


def validate_fixture(registry, actual_values, canaries):
    fields = personal_fields(registry)
    require(set(canaries) == set(fields), "PRIVACY_CANARY_MISSING",
            ", ".join(sorted(set(fields) ^ set(canaries))))
    invalid = [key for key, value in canaries.items() if not isinstance(value, str) or not value]
    require(not invalid, "PRIVACY_CANARY_INVALID", ", ".join(sorted(invalid)))
    require(len(set(canaries.values())) == len(canaries), "PRIVACY_CANARY_INVALID", "canaries must be unique")
    absent = [key for key, needle in canaries.items()
              if not isinstance(actual_values.get(key), list) or not contains(actual_values[key], needle)]
    require(not absent, "PRIVACY_FIXTURE_UNCOVERED", ", ".join(sorted(absent)))


CATALOG_SQL = """-- PRIVACY_COLUMN_CATALOG: all app tables plus the explicit vendor job envelope.
SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY table_schema,table_name,ordinal_position),'[]')
FROM (SELECT c.table_schema,c.table_name,c.column_name,c.data_type,c.udt_name,c.ordinal_position
      FROM information_schema.columns c JOIN information_schema.tables t
      USING(table_catalog,table_schema,table_name)
      WHERE t.table_type='BASE TABLE' AND (c.table_schema='app' OR
            (c.table_schema='queue' AND c.table_name='river_job'))) c;
"""


def psql(root, sql):
    # Never print a DSN, subprocess arguments, real cell values or raw DB errors.
    require(os.environ.get("DATABASE_URL"), "PRIVACY_DATABASE_UNAVAILABLE", "DATABASE_URL is required")
    try:
        result = subprocess.run(["psql", os.environ["DATABASE_URL"], "-X", "-qAt", "-v", "ON_ERROR_STOP=1",
                                 "-v", "VERBOSITY=verbose"],
                                input=sql, cwd=root, text=True, capture_output=True, timeout=90)
    except (OSError, subprocess.TimeoutExpired) as error:
        raise ValueError("PRIVACY_DATABASE_UNAVAILABLE: psql failed or timed out") from error
    if result.returncode != 0:
        # Only fixed-format SQLSTATE and fixture line numbers survive redaction;
        # constraint detail, SQL source, DSN and stored values never do.
        state = re.search(r"ERROR:\s+([0-9A-Z]{5}):", result.stderr)
        lines = re.findall(r"PL/pgSQL function inline_code_block line ([0-9]+) at", result.stderr)
        detail = "query/fixture failed"
        if state:
            detail += " SQLSTATE=" + state[1]
        if lines:
            detail += " fixture_line=" + lines[-1]
        raise ValueError("PRIVACY_DATABASE_ERROR: " + detail + " (credentials and values redacted)")
    return result.stdout


def fixture_values_sql(registry, catalog_rows=()):
    # Identifiers come ONLY from validated declarations, never policy paths.
    pairs = []
    types = {field_key(row.get("table_schema", "app"), row["table_name"], row["column_name"]): row["udt_name"]
             for row in catalog_rows}
    for key in sorted(personal_fields(registry)):
        schema, table, column = field_parts(key)
        value = f'"{column}"::text'
        if types.get(key) == "bytea":
            value = f'encode("{column}",\'escape\')'
        elif types.get(key) == "_bytea":
            value = f'(SELECT jsonb_agg(encode(b,\'escape\')) FROM unnest("{column}") b)'
        pairs.append(f"SELECT {sql_literal(key)} k, coalesce(jsonb_agg({value}),'[]') v "
                     f'FROM "{schema}"."{table}"')
    require(pairs, "PRIVACY_REGISTRY_INVALID", "no personal/contact fields")
    return "SELECT 'PRIVACY_CELL_VALUES=' || jsonb_object_agg(k,v)::text FROM (" + " UNION ALL ".join(pairs) + ") fields;\n"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--database", action="store_true", help="reconcile real migrated PG columns")
    parser.add_argument("--fixture", action="store_true", help="write/read canaries in a rolled-back CI transaction")
    parser.add_argument("--emit-fixture", type=Path, help="write SQL for a consumer test, without executing it")
    parser.add_argument("--manifest", type=Path, help="save field-to-canary map for an erasure-test consumer")
    parser.add_argument("--prefix", default="run" + secrets.token_hex(4))
    args = parser.parse_args()
    try:
        require(not args.manifest or args.fixture or args.emit_fixture, "PRIVACY_MANIFEST_USAGE",
                "--manifest requires --fixture or --emit-fixture")
        root = args.root.resolve()
        registry = load_registry(root)
        require((root / "modules/registry.yaml").is_file(), "PRIVACY_REGISTRY_INVALID", "missing module ownership registry")
        catalog_rows = []
        if args.database or args.fixture:
            require(os.environ.get("GITHUB_ACTIONS") == "true", "PRIVACY_CI_ONLY", "database gates run only in GitHub CI")
            catalog_rows = json.loads(psql(root, CATALOG_SQL))
            validate_catalog(registry, catalog_rows)
            print(f"PASS: privacy catalog {len(registry)} actual columns, bidirectional match")
        if args.fixture or args.emit_fixture:
            template = (root / "tests/fixtures/privacy_canaries.sql").read_text()
            sql, canaries = render_fixture(registry, template, args.prefix)
            if args.emit_fixture:
                args.emit_fixture.parent.mkdir(parents=True, exist_ok=True)
                args.emit_fixture.write_text(sql)
            if args.manifest:
                args.manifest.parent.mkdir(parents=True, exist_ok=True)
                args.manifest.write_text(json.dumps(canaries, indent=2) + "\n")
            if args.fixture:
                output = psql(root, "BEGIN;\n" + sql + "\n" + fixture_values_sql(registry, catalog_rows) + "ROLLBACK;\n")
                rows = [line.removeprefix("PRIVACY_CELL_VALUES=") for line in output.splitlines()
                        if line.startswith("PRIVACY_CELL_VALUES=")]
                require(len(rows) == 1, "PRIVACY_DATABASE_ERROR", "missing persisted cell witness")
                validate_fixture(registry, json.loads(rows[0]), canaries)
                print(f"PASS: privacy fixture {len(canaries)}/{len(canaries)} persisted personal/contact columns; ROLLBACK")
        print(f"PASS: explicit privacy policies {len(registry)} columns; {len(personal_fields(registry))} personal/contact")
    except (OSError, ValueError, KeyError, TypeError) as error:
        print(str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
