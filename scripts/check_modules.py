#!/usr/bin/env python3
"""Static module boundaries and live app-table ownership; stdlib only.

Reads the JSON subset of YAML registry, inline OpenAPI path operations, Go's
actual default/pgx dependency streams including tests, and (with --tables) the
PostgreSQL catalog. Called by check_all/check_pg. Planned data is never a live
table owner; cross-module imports must target declared public contracts.
"""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys
from module_sql import SqlOwnershipError, check_sql_writes


class GovernanceError(Exception):
    """A stable gate diagnostic, safe to report without connection credentials."""


def require(condition, marker, detail):
    if not condition:
        raise GovernanceError(f"{marker}: {detail}")


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "MODULE_REGISTRY_INVALID", f"duplicate JSON key {key}")
        result[key] = value
    return result


def load_registry(path, root):
    try:
        registry = json.loads(path.read_text(), object_pairs_hook=unique_object)
    except (OSError, ValueError) as error:
        raise GovernanceError("MODULE_REGISTRY_INVALID: registry must use JSON-compatible YAML") from error
    require(isinstance(registry, dict) and registry.get("version") == 1,
            "MODULE_REGISTRY_INVALID", "expected version 1 object")
    modules = registry.get("modules")
    require(isinstance(modules, list) and modules, "MODULE_REGISTRY_INVALID", "modules must be a nonempty list")
    names, operations, tables = set(), set(), set()
    for module in modules:
        require(isinstance(module, dict), "MODULE_REGISTRY_INVALID", "module must be an object")
        name = module.get("name")
        require(isinstance(name, str) and re.fullmatch(r"[a-z][a-z0-9]*(?:-[a-z0-9]+)*", name),
                "MODULE_REGISTRY_INVALID", "invalid module name")
        require(name not in names, "MODULE_REGISTRY_INVALID", f"duplicate module {name}")
        names.add(name)
        require(module.get("status") in ("implemented", "planned") and
                module.get("go_status") in ("implemented", "planned"), "MODULE_REGISTRY_INVALID", f"{name}: invalid status")
        require(module.get("go_path") == f"internal/modules/{name}", "MODULE_REGISTRY_INVALID", f"{name}: invalid Go path")
        legacy = module.get("legacy_go_paths", [])
        ports = module.get("approved_public_ports", [])
        for field, paths in (("legacy_go_paths", legacy), ("approved_public_ports", ports)):
            require(isinstance(paths, list) and len(paths) == len(set(paths)) and
                    all(isinstance(value, str) and re.fullmatch(r"internal/[a-z][a-z0-9_/-]*", value) and
                        ".." not in value.split("/") for value in paths),
                    "MODULE_REGISTRY_INVALID", f"{name}: invalid {field}")
        require(all(any(port == path or port.startswith(path + "/") for path in legacy)
                    and "internal" not in port.split("/")[2:] for port in ports),
                "MODULE_REGISTRY_INVALID", f"{name}: public port outside its legacy package")
        require(all((root / path).is_dir() for path in legacy),
                "MODULE_REGISTRY_INVALID", f"{name}: missing legacy Go package")
        for field in ("owns_tables", "dependencies", "tests", "scopes", "operations"):
            values = module.get(field)
            require(isinstance(values, list), "MODULE_REGISTRY_INVALID", f"{name}: {field} must be a list")
            if field != "operations":
                require(all(isinstance(value, str) and value for value in values),
                        "MODULE_REGISTRY_INVALID", f"{name}: {field} must contain strings")
                require(len(values) == len(set(values)), "MODULE_REGISTRY_INVALID", f"{name}: duplicate {field}")
        for table in module["owns_tables"]:
            require(re.fullmatch(r"[a-z][a-z0-9_]*", table), "MODULE_REGISTRY_INVALID", f"{name}: invalid table name")
            require(table not in tables, "MODULE_DUPLICATE_OWNER", table)
            tables.add(table)
        for operation in module["operations"]:
            require(isinstance(operation, dict), "MODULE_REGISTRY_INVALID", f"{name}: invalid operation")
            identifier = operation.get("operation_id")
            require(isinstance(identifier, str) and re.fullmatch(r"[A-Za-z][A-Za-z0-9_]*", identifier),
                    "MODULE_REGISTRY_INVALID", f"{name}: invalid operation ID")
            require(operation.get("status") in ("planned", "implemented"), "MODULE_REGISTRY_INVALID", f"{name}: operation status")
            require(identifier not in operations, "MODULE_REGISTRY_INVALID", f"duplicate operation {identifier}")
            operations.add(identifier)
        for test in module["tests"]:
            target = (root / test).resolve()
            require(target.is_relative_to(root) and not Path(test).is_absolute(),
                    "MODULE_REGISTRY_INVALID", f"{name}: test path escapes repository")
            if module["status"] == "implemented":
                require(target.is_file(), "MODULE_REGISTRY_INVALID", f"{name}: missing test {test}")
    for module in modules:
        require(set(module["dependencies"]) <= names, "MODULE_REGISTRY_INVALID", f"{module['name']}: unknown dependency")
    go_paths = [path for module in modules for path in [module["go_path"], *module.get("legacy_go_paths", [])]]
    require(len(go_paths) == len(set(go_paths)) and
            not any(a != b and b.startswith(a + "/") for a in go_paths for b in go_paths),
            "MODULE_REGISTRY_INVALID", "overlapping module Go paths")
    migration_files = set()
    require(isinstance(registry.get("migrations"), list), "MODULE_REGISTRY_INVALID", "migrations must be a list")
    for migration in registry["migrations"]:
        require(isinstance(migration, dict), "MODULE_REGISTRY_INVALID", "invalid migration")
        file = migration.get("file", "")
        require(isinstance(file, str) and re.fullmatch(r"migrations/[0-9]{3}_[a-z0-9_]+\.sql", file) and
                (root / file).is_file(), "MODULE_REGISTRY_INVALID", "missing or invalid migration path")
        require(file not in migration_files and migration.get("owner") in names,
                "MODULE_REGISTRY_INVALID", f"{file}: duplicate migration or unknown owner")
        migration_files.add(file)
        touches = migration.get("touches_tables")
        require(isinstance(touches, list) and all(isinstance(table, str) and table in tables for table in touches),
                "MODULE_REGISTRY_INVALID", f"{file}: unknown touched table")
    actual_files = {str(path.relative_to(root)) for path in (root / "migrations").glob("[0-9][0-9][0-9]_*.sql")}
    require(migration_files == actual_files, "MODULE_REGISTRY_INVALID", "migration inventory differs from registry")
    check_dependencies(registry)
    return registry


def check_dependencies(registry):
    modules = {module["name"]: module for module in registry["modules"]}
    visited, active = set(), set()

    def visit(name):
        require(name not in active, "MODULE_DEPENDENCY_CYCLE", name)
        if name in visited:
            return
        active.add(name)
        for dependency in modules[name]["dependencies"]:
            visit(dependency)
        active.remove(name)
        visited.add(name)

    for name in modules:
        visit(name)


def openapi_operations(path):
    """Read operationId only directly within paths/path/HTTP-method mappings.

    Deliberately supports the repository's block YAML form, not aliases, flow
    maps, or referenced Path Items. Unsupported path structures fail closed;
    components/examples and scalar descriptions cannot witness an operation.
    """
    methods = {"get", "put", "post", "delete", "options", "head", "patch", "trace"}
    stack, operations = [], set()
    mapping_keys, operation_locations, identified_locations = set(), set(), set()
    block_indent = None
    saw_paths = False
    for line in path.read_text().splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        require("\t" not in line, "MODULE_OPENAPI_INVALID", "tabs are unsupported")
        indent = len(line) - len(line.lstrip(" "))
        if block_indent is not None:
            if indent > block_indent:
                continue
            block_indent = None
        while stack and indent <= stack[-1][0]:
            stack.pop()
        # YAML's mapping separator is a colon followed by whitespace or EOL;
        # a colon inside an unquoted action-style path is part of the key.
        match = re.match(r'''("(?:\\.|[^"\\])*"|'(?:''|[^'])*'|.+?):(?:\s+(.*))?$''', stripped)
        if not match:
            continue  # sequence values do not change mapping ancestry
        key, value = match.group(1), (match.group(2) or "").strip()
        if key.startswith('"'):
            key = json.loads(key)
        elif key.startswith("'"):
            key = key[1:-1].replace("''", "'")
        parents = [entry[1] for entry in stack]
        if indent == 0 and key == "paths":
            require(not saw_paths, "MODULE_OPENAPI_INVALID", "duplicate paths mapping")
            saw_paths = True
            require(not value or value.startswith("#"), "MODULE_OPENAPI_INVALID", "paths must be a block mapping")
        if parents and parents[0] == "paths" and (len(parents) <= 2 or
                                                 (len(parents) == 3 and parents[2] in methods)):
            location = (*parents, key)
            require(location not in mapping_keys, "MODULE_OPENAPI_INVALID", "duplicate path/operation key")
            mapping_keys.add(location)
        if parents == ["paths"]:
            require(key.startswith("/") or key.startswith("x-"), "MODULE_OPENAPI_INVALID", "invalid path item")
            require(not value or value.startswith("#"), "MODULE_OPENAPI_INVALID", "Path Items must be inline block mappings")
        if len(parents) == 2 and parents[0] == "paths" and parents[1].startswith("/") and key in methods:
            require(not value or value.startswith("#"), "MODULE_OPENAPI_INVALID", "operations must be block mappings")
            operation_locations.add((*parents, key))
        if parents and parents[0] == "paths" and len(parents) <= 3 and key in ("$ref", "<<"):
            raise GovernanceError("MODULE_OPENAPI_INVALID: referenced or merged Path Items are unsupported")
        if (key == "operationId" and len(parents) == 3 and parents[0] == "paths" and
                parents[1].startswith("/") and parents[2] in methods):
            match_id = re.fullmatch(r"(?:([A-Za-z][A-Za-z0-9_]*)|'([A-Za-z][A-Za-z0-9_]*)'|\"([A-Za-z][A-Za-z0-9_]*)\")(?:\s+#.*)?", value)
            require(match_id, "MODULE_OPENAPI_INVALID", "operationId must be a literal identifier")
            identifier = next(part for part in match_id.groups() if part is not None)
            require(identifier not in operations, "MODULE_OPENAPI_INVALID", f"duplicate operationId {identifier}")
            operations.add(identifier)
            identified_locations.add(tuple(parents))
        if re.match(r"[|>][-+0-9]*(?:\s+#.*)?$", value):
            block_indent = indent
        elif not value or value.startswith("#"):
            stack.append((indent, key))
    require(saw_paths and operations, "MODULE_OPENAPI_INVALID", "no inline path operations found")
    require(operation_locations == identified_locations, "MODULE_OPENAPI_INVALID", "every HTTP operation needs an operationId")
    return operations


def check_operations(registry, path):
    available = openapi_operations(path)
    registered = set()
    for module in registry["modules"]:
        for operation in module["operations"]:
            registered.add(operation["operation_id"])
            if operation["status"] == "implemented":
                require(operation["operation_id"] in available, "MODULE_MISSING_OPERATION", operation["operation_id"])
    require(available <= registered, "MODULE_UNREGISTERED_OPERATION", ", ".join(sorted(available - registered)))


def json_stream(text):
    decoder = json.JSONDecoder()
    index = 0
    while index < len(text):
        if text[index].isspace():
            index += 1
            continue
        value, index = decoder.raw_decode(text, index)
        yield value


def check_go(registry, root):
    # go list ./... silently skips nested modules. Reject them before trusting
    # its package inventory; caches, vendor and private receipt directories
    # are not repository source. Also deny linker-level private symbol access.
    modules = {module["name"]: module for module in registry["modules"]}
    owned_paths = {path: name for name, module in modules.items()
                   for path in [module["go_path"], *module.get("legacy_go_paths", [])]}
    for directory, dirs, files in os.walk(root):
        directory = Path(directory)
        if directory == root:
            dirs[:] = [name for name in dirs if name not in (".git", ".worktrees", "work", "vendor", "node_modules")]
        require(directory == root or "go.mod" not in files,
                "MODULE_NESTED_GO_MODULE", str(directory.relative_to(root)))
    go_tokens = re.compile(r'''//[^\n]*|/\*.*?\*/|`[^`]*`|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*' ''', re.S | re.X)
    for package_path in owned_paths:
        for source in (root / package_path).rglob("*.go"):
            for token in go_tokens.finditer(source.read_text()):
                require(not re.match(r"//go:linkname(?:\s|$)", token.group()),
                        "MODULE_LINKNAME_FORBIDDEN", str(source.relative_to(root)))
    env = dict(os.environ, GOPROXY="off", GOSUMDB="off", GOWORK="off")
    prefix_result = subprocess.run(["go", "list", "-m"], cwd=root, env=env,
                                   capture_output=True, text=True, timeout=90)
    require(prefix_result.returncode == 0, "MODULE_GO_LIST_ERROR", "cannot identify repository module")
    prefix = prefix_result.stdout.strip() + "/"

    def module_path(path):
        # Go test variants append ' [package.test]' to ImportPath and Imports.
        path = path.split(" [", 1)[0]
        if not path.startswith(prefix):
            return None
        relative = path[len(prefix):]
        if relative.endswith(".test"):
            return None  # synthetic test executable, not a module package
        for package_path, name in owned_paths.items():
            if relative == package_path or relative.startswith(package_path + "/"):
                return [name, *relative[len(package_path):].strip("/").split("/")] if relative != package_path else [name]
        if relative.startswith("internal/modules/"):
            raise GovernanceError("MODULE_UNREGISTERED_PACKAGE: " + relative.split("/")[2])
        return None

    for tags in ([], ["-tags", "pgx"]):
        result = subprocess.run(["go", "list", "-e", "-deps", "-json", "-test", *tags, "./..."],
                                cwd=root, env=env, capture_output=True, text=True, timeout=90)
        errors = result.returncode != 0
        for package in json_stream(result.stdout):
            source = module_path(package.get("ForTest") or package["ImportPath"])
            errors = errors or bool(package.get("Error") or package.get("DepsErrors"))
            if source is None:
                continue
            for imported in package.get("Imports", []):
                require(imported.split(" [", 1)[0] != "github.com/jackc/pgx/v5/pgxpool",
                        "MODULE_RAW_POOL_IMPORT", source[0])
                destination = module_path(imported)
                if destination is None or destination[0] == source[0]:
                    continue
                edge = f"{source[0]} -> {destination[0]}"
                require("internal" not in destination[1:], "MODULE_PRIVATE_IMPORT", edge)
                relative = imported.split(" [", 1)[0].removeprefix(prefix)
                public_port = relative in modules[destination[0]].get("approved_public_ports", [])
                require((len(destination) > 1 and destination[1] == "contract") or public_port,
                        "MODULE_NONCONTRACT_IMPORT", edge)
                require(destination[0] in modules[source[0]]["dependencies"], "MODULE_UNDECLARED_DEPENDENCY", edge)
        require(not errors, "MODULE_GO_LIST_ERROR", "Go package loading failed (default/pgx, including tests)")


CATALOG_SQL = """-- MODULE_TABLE_OWNERSHIP: real app tables, including partitioned tables
SELECT coalesce(json_agg(c.relname ORDER BY c.relname), '[]'::json)
FROM pg_catalog.pg_class AS c
JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace
WHERE n.nspname = 'app' AND c.relkind IN ('r', 'p');
"""


def catalog_tables(root):
    if os.environ.get("DATABASE_URL"):
        command = ["psql", os.environ["DATABASE_URL"], "-X", "-qAt", "-v", "ON_ERROR_STOP=1"]
    else:
        command = ["docker", "compose", "-f", "deploy/compose.yaml", "exec", "-T", "postgres",
                   "psql", "-X", "-U", "postgres", "-d", "opensite", "-qAt", "-v", "ON_ERROR_STOP=1"]
    try:
        result = subprocess.run(command, cwd=root, input=CATALOG_SQL, capture_output=True,
                                text=True, timeout=90)
        require(result.returncode == 0, "MODULE_DATABASE_ERROR", "catalog query failed")
        tables = json.loads(result.stdout)
        require(isinstance(tables, list) and all(isinstance(table, str) for table in tables),
                "MODULE_DATABASE_ERROR", "catalog response must be an array of table names")
        return tables
    except (OSError, ValueError, subprocess.TimeoutExpired) as error:
        raise GovernanceError("MODULE_DATABASE_ERROR: catalog query unavailable or invalid") from error


def check_tables(registry, tables):
    owners = {}
    for module in registry["modules"]:
        if module["status"] == "planned":
            continue
        for table in module["owns_tables"]:
            require(table not in owners, "MODULE_DUPLICATE_OWNER", table)
            owners[table] = module["name"]
    actual = set(tables)
    require(actual <= owners.keys(), "MODULE_UNOWNED_TABLE", ", ".join(sorted(actual - owners.keys())))
    require(owners.keys() <= actual, "MODULE_GHOST_TABLE", ", ".join(sorted(owners.keys() - actual)))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--registry", type=Path, help="alternate registry for fault probes")
    parser.add_argument("--tables", action="store_true", help="real PostgreSQL table audit instead of Go dependency scan")
    args = parser.parse_args()
    root = args.root.resolve()
    try:
        registry = load_registry(args.registry or root / "modules/registry.yaml", root)
        check_operations(registry, root / "api/openapi.yaml")
        check_sql_writes(registry, root)
        if args.tables:
            tables = catalog_tables(root)
            check_tables(registry, tables)
            print(f"PASS: module ownership of {len(tables)} real app tables")
        else:
            check_go(registry, root)
            print("PASS: module registry, operations, dependency DAG; Go default/pgx including tests")
    except (GovernanceError, SqlOwnershipError, OSError, ValueError, subprocess.TimeoutExpired) as error:
        # Never print subprocess args, raw database stderr, or a connection URL.
        print(str(error) if isinstance(error, (GovernanceError, SqlOwnershipError)) else "MODULE_INPUT_ERROR: unavailable or malformed input", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
