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
CLASSES = PII | {"financial_non_contact", "financial_obligation", "operational", "secret_hash"}
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
                if field["classification"] == "financial_obligation":
                    require(field.get("retention") == "financial-obligation",
                            "PRIVACY_FINANCIAL_RETENTION_MISSING", key)
                    require(isinstance(field.get("reason"), str) and field["reason"].strip(),
                            "PRIVACY_RETAIN_REASON_MISSING", key)
                if "retention" in field:
                    require(field["classification"] == "financial_obligation",
                            "PRIVACY_FINANCIAL_CLASSIFICATION_MISSING", key)
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
        policy = registry[key].get("merchant_deletion")
        if isinstance(policy, dict) and policy.get("action") == "retain" and policy.get("value_type") != "global":
            types = {"uuid": {"uuid"}, "integer": {"int2", "int4", "int8"}, "currency": {"bpchar"},
                     "timestamp": {"timestamptz"}, "boolean": {"bool"}, "digest": {"bytea"},
                     "enum": {"text"}, "opaque_ref": {"text"}}
            require(row["udt_name"] in types.get(policy.get("value_type"), set()), "MERCHANT_RETAIN_CATALOG_TYPE", key)


# This is a declarative contract, never a SQL/path interpreter or erasure engine.
# Bootstrap/owner handlers consume the version+digest in later MDEL cards.
MERCHANT_STRATEGIES = {"delete_row", "unchanged", "constant", "row_id_placeholder",
                       "database_time", "fixed_enum", "generated_from", "actor_placeholder",
                       "financial_source_id", "financial_event", "financial_account_code", "unlink_tenant_namespace"}
ARTIFACT_MARKER = re.compile(r"^// privacy-artifact: ([a-z][a-z0-9_]*\.[a-z][a-z0-9_]*)$", re.M)
FILE_WRITER = re.compile(r"(?:\bos\.(?:Create|CreateTemp|WriteFile|OpenFile)|\.OpenFile)\s*\(")


def artifact_catalog(root):
    """Reconcile source-owned persistent writers, including orphan/temp bytes.

    Explicit annotations declare artifact kinds; names never imply ownership.
    A new unannotated internal filesystem writer fails closed. This is a source
    inventory, not evidence of runtime erasure or an on-disk namespace scan.
    """
    result = {}
    for path in sorted((root / "internal").rglob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        source = path.read_text()
        markers = ARTIFACT_MARKER.findall(source)
        require(not FILE_WRITER.search(source) or markers, "MERCHANT_ARTIFACT_WRITER", str(path.relative_to(root)))
        for name in markers:
            require(name not in result, "MERCHANT_ARTIFACT_DUPLICATE", name)
            result[name] = path.relative_to(root).as_posix()
    return result


def merchant_policy(key, policy, module, registry, handlers, artifact=False):
    require(isinstance(policy, dict), "MERCHANT_POLICY_MISSING", key)
    allowed = {"action", "strategy", "reason", "handler", "ownership", "retained_kind",
               "value_type", "values", "value", "source", "paths", "guards", "prerequisite", "preserve_null"}
    require(not policy.keys() - allowed, "MERCHANT_POLICY_UNKNOWN", key)
    require(policy.get("action") in {"retain", "erase"}, "MERCHANT_ACTION_INVALID", key)
    require(isinstance(policy.get("reason"), str) and policy["reason"].strip(), "MERCHANT_REASON_MISSING", key)
    strategy = policy.get("strategy")
    require(isinstance(strategy, str) and strategy in MERCHANT_STRATEGIES, "MERCHANT_STRATEGY_INVALID", key)
    handler = policy.get("handler")
    require(isinstance(handler, str) and handler in handlers and handler.split(".")[0] == module,
            "MERCHANT_HANDLER_UNKNOWN", key)
    ownership = policy.get("ownership")
    require(isinstance(ownership, dict) and not ownership.keys() - {"kind", "path", "reason"},
            "MERCHANT_OWNERSHIP_MISSING", key)
    kind, path = ownership.get("kind"), ownership.get("path")
    require(isinstance(path, str) and path.strip(), "MERCHANT_OWNERSHIP_MISSING", key)
    if kind == "tenant_column":
        require(path in registry and (path.endswith(".tenant_id") or path == "tenants.id"),
                "MERCHANT_TENANT_PATH", key)
        require(registry[path]["module"] == module, "MERCHANT_TENANT_PATH", key)
        if not artifact:
            require(field_parts(path)[:2] == field_parts(key)[:2], "MERCHANT_TENANT_PATH", key)
    elif kind == "global_exclusion":
        require(not artifact and field_parts(key)[0] == "app" and key != "tenants.id" and
                field_key(*field_parts(key)[:2], "tenant_id") not in registry,
                "MERCHANT_GLOBAL_EXCLUSION", key)
        require(isinstance(ownership.get("reason"), str) and ownership["reason"].strip() and
                policy["action"] == "retain" and policy.get("retained_kind") == "global",
                "MERCHANT_GLOBAL_EXCLUSION", key)
    elif kind == "signed_job":
        require(not artifact and key.startswith("queue.river_job.") and
                path == "verified_signed_job_tenant" and module == "reliability",
                "MERCHANT_QUEUE_AUTHORITY", key)
    else:
        require(False, "MERCHANT_OWNERSHIP_UNKNOWN", key)
    if "preserve_null" in policy:
        require(type(policy["preserve_null"]) is bool and policy["action"] == "erase" and
                strategy in {"constant", "row_id_placeholder"}, "MERCHANT_NULL_POLICY", key)
    if artifact:
        require(policy["action"] == "erase" and strategy == "unlink_tenant_namespace", "MERCHANT_ARTIFACT_POLICY", key)
    if policy["action"] == "retain":
        require(strategy == "unchanged" and policy.get("retained_kind") in {"financial", "witness", "control", "global"},
                "MERCHANT_RETENTION_INVALID", key)
        require(policy.get("value_type") in {"uuid", "integer", "currency", "timestamp", "boolean", "digest", "opaque_ref", "enum", "global"},
                "MERCHANT_RETAIN_TYPE", key)
        if not artifact and registry[key]["classification"] in PII | {"secret_hash"}:
            require(kind == "global_exclusion" or policy["value_type"] == "digest" and
                    policy["retained_kind"] == "financial", "MERCHANT_PERSONAL_RETAIN", key)
        require((kind == "global_exclusion") == (policy["retained_kind"] == "global") and
                (kind == "global_exclusion" or policy["value_type"] != "global"), "MERCHANT_RETAIN_TYPE", key)
        if policy["value_type"] == "enum":
            require(isinstance(policy.get("values"), list) and policy["values"] and
                    all(isinstance(x, str) and x.strip() for x in policy["values"]), "MERCHANT_ENUM_MISSING", key)
    else:
        require(strategy != "unchanged" and "retained_kind" not in policy, "MERCHANT_ERASE_INVALID", key)
        if strategy in {"constant", "fixed_enum", "row_id_placeholder"}:
            require("value" in policy, "MERCHANT_SCRUB_VALUE", key)
        special_fields = {
            "financial_source_id": "ledger_transactions.source_id",
            "financial_event": "ledger_transactions.event",
            "financial_account_code": "ledger_accounts.code",
            "actor_placeholder": "checkout_sessions.actor_id",
        }
        if strategy in special_fields:
            require(key == special_fields[strategy], "MERCHANT_STRATEGY_FIELD", key)
        if strategy == "row_id_placeholder":
            require(isinstance(policy["value"], str) and "{id}" in policy["value"] and
                    field_key(*field_parts(key)[:2], "id") in registry,
                    "MERCHANT_SCRUB_VALUE", key)
        if strategy == "generated_from":
            source = policy.get("value")
            require(isinstance(source, str) and source in registry and
                    field_parts(source)[:2] == field_parts(key)[:2], "MERCHANT_SCRUB_SOURCE", key)
        if strategy == "fixed_enum":
            require(isinstance(policy.get("values"), list) and policy["values"] and
                    all(isinstance(x, str) and x.strip() for x in policy["values"]) and
                    policy["value"] in policy["values"], "MERCHANT_ENUM_MISSING", key)
    if artifact:
        require(policy.get("prerequisite") in {None, "MDEL-21a"} and
                (module != "media" or policy.get("prerequisite") == "MDEL-21a"), "MERCHANT_ARTIFACT_PREREQUISITE", key)
        require(isinstance(policy.get("paths"), list) and policy["paths"] and
                all(isinstance(x, str) and x and not x.startswith("/") and ".." not in x.split("/")
                    for x in policy["paths"]), "MERCHANT_ARTIFACT_PATH", key)
        require(isinstance(policy.get("guards"), list) and
                {"canonical_ids", "anchored_root", "no_symlinks", "fsync_parents", "verify_empty"} <= set(policy["guards"]),
                "MERCHANT_ARTIFACT_GUARDS", key)
    else:
        require(strategy != "unlink_tenant_namespace", "MERCHANT_STRATEGY_INVALID", key)
        require(not {"source", "paths", "guards", "prerequisite"} & policy.keys(), "MERCHANT_COLUMN_UNKNOWN", key)


def load_merchant_contract(root, registry):
    """Mandatory independent per-column policy + module-owned dependency DAG."""
    handlers, artifacts, fields = {}, {}, {}
    for file in sorted((root / "modules/privacy").glob("*.json")):
        document = read_json(file)
        module = document["module"]
        contract = document.get("merchant_deletion")
        require(isinstance(contract, dict) and contract.get("version") == 1 and
                not contract.keys() - {"version", "handlers", "artifacts"}, "MERCHANT_CONTRACT_MISSING", module)
        declared = contract.get("handlers")
        require(isinstance(declared, dict) and declared, "MERCHANT_HANDLER_MISSING", module)
        for name, handler in declared.items():
            require(isinstance(name, str) and re.fullmatch(re.escape(module) + r"\.[a-z][a-z0-9_]*", name) and
                    name not in handlers and isinstance(handler, dict), "MERCHANT_HANDLER_INVALID", str(name))
            require(set(handler) == {"depends_on", "tables", "lock_order", "clock", "transaction"} and
                    handler["clock"] == "database" and handler["transaction"] == "one_item_and_checkpoint" and
                    handler["lock_order"] == "tenant_fence -> artifact_uuid -> module", "MERCHANT_HANDLER_CONTRACT", name)
            for prop in ("depends_on", "tables"):
                require(isinstance(handler[prop], list) and all(isinstance(x, str) for x in handler[prop]) and
                        len(handler[prop]) == len(set(handler[prop])), "MERCHANT_HANDLER_INVALID", name)
            handlers[name] = handler
        declared_artifacts = contract.get("artifacts")
        require(isinstance(declared_artifacts, dict), "MERCHANT_ARTIFACT_MISSING", module)
        for name, policy in declared_artifacts.items():
            require(name not in artifacts and isinstance(name, str) and
                    re.fullmatch(re.escape(module) + r"\.[a-z][a-z0-9_]*", name), "MERCHANT_ARTIFACT_DUPLICATE", str(name))
            artifacts[name] = policy
    for key, field in registry.items():
        policy = field.get("merchant_deletion")
        merchant_policy(key, policy, field["module"], registry, handlers)
        fields[key] = policy
    # A module may first detach an owned FK, then delete its row in a later
    # work item. Each item owns explicit columns, never another module's store.
    tables, deleting = set(), {}
    for key, policy in fields.items():
        item = (key.rsplit(".", 1)[0], policy["handler"])
        tables.add(item)
        deleting.setdefault(item, set()).add(policy["strategy"] == "delete_row")
    require(all(len(modes) == 1 for modes in deleting.values()), "MERCHANT_ROW_POLICY",
            "one item cannot both delete a row and retain/scrub its cells")
    declared_tables = set()
    for name, handler in handlers.items():
        for table in handler["tables"]:
            item = (table, name)
            require(item in tables, "MERCHANT_TABLE_ORDER", table)
            declared_tables.add(item)
        require(set(handler["depends_on"]) <= handlers.keys(), "MERCHANT_DEPENDENCY_UNKNOWN", name)
    require(declared_tables == tables, "MERCHANT_TABLE_ORDER", "every owned table/item needs a local order entry")
    ordered, pending = [], set(handlers)
    while pending:
        ready = sorted(name for name in pending if set(handlers[name]["depends_on"]) <= set(ordered))
        require(ready, "MERCHANT_DEPENDENCY_CYCLE", ", ".join(sorted(pending)))
        ordered.extend(ready)
        pending.difference_update(ready)
    actual = artifact_catalog(root)
    require(not actual.keys() - artifacts.keys(), "MERCHANT_ARTIFACT_UNDECLARED", ", ".join(sorted(actual.keys() - artifacts.keys())))
    require(not artifacts.keys() - actual.keys(), "MERCHANT_ARTIFACT_PHANTOM", ", ".join(sorted(artifacts.keys() - actual.keys())))
    for name, policy in artifacts.items():
        merchant_policy(name, policy, name.split(".")[0], registry, handlers, artifact=True)
        require(policy.get("source") == actual[name], "MERCHANT_ARTIFACT_SOURCE", name)
    content = {"version": 1, "fields": fields, "handlers": handlers, "handler_order": ordered, "artifacts": artifacts}
    content["digest"] = hashlib.sha256(json.dumps(content, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    return content


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
        merchant = load_merchant_contract(root, registry)
        print(f"PASS: merchant contract v{merchant['version']} digest={merchant['digest']} "
              f"{len(merchant['fields'])} columns, {len(merchant['artifacts'])} artifacts, "
              f"{len(merchant['handler_order'])} ordered owner handlers; activation disabled")
        print(f"PASS: explicit privacy policies {len(registry)} columns; {len(personal_fields(registry))} personal/contact")
    except (OSError, ValueError, KeyError, TypeError) as error:
        print(str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
