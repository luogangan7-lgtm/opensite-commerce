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
    ids, statuses = {}, {}
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
            ids[tuple(parents)] = identifier
        if (key == "x-opensite-status" and len(parents) == 3 and parents[0] == "paths" and
                parents[1].startswith("/") and parents[2] in methods):
            match_status = re.fullmatch(r"""(['"]?)(planned|implemented)\1(?:\s+#.*)?""", value)
            require(match_status, "MODULE_OPENAPI_INVALID", "x-opensite-status must be planned or implemented")
            statuses[tuple(parents)] = match_status.group(2)
        if re.match(r"[|>][-+0-9]*(?:\s+#.*)?$", value):
            block_indent = indent
        elif not value or value.startswith("#"):
            stack.append((indent, key))
    require(saw_paths and operations, "MODULE_OPENAPI_INVALID", "no inline path operations found")
    require(operation_locations == identified_locations, "MODULE_OPENAPI_INVALID", "every HTTP operation needs an operationId")
    # Presence of x-opensite-status is check_contract's rule; here a declared
    # status is returned so the registry cannot drift from it.
    return {identifier: statuses.get(location) for location, identifier in ids.items()}


def check_operations(registry, path):
    available = openapi_operations(path)
    registered = {}
    for module in registry["modules"]:
        for operation in module["operations"]:
            registered[operation["operation_id"]] = operation.get("status")
    # Status governs handler readiness, never whether a declared ID is a
    # contract. Both planned and implemented operations must exist in the OAS.
    require(registered.keys() <= available.keys(), "MODULE_MISSING_OPERATION",
            ", ".join(sorted(registered.keys() - available.keys())))
    require(available.keys() <= registered.keys(), "MODULE_UNREGISTERED_OPERATION",
            ", ".join(sorted(available.keys() - registered.keys())))
    drift = sorted(identifier for identifier, status in available.items()
                   if status is not None and status != registered[identifier])
    require(not drift, "MODULE_OPERATION_STATUS_MISMATCH", ", ".join(drift))


def json_stream(text):
    decoder = json.JSONDecoder()
    index = 0
    while index < len(text):
        if text[index].isspace():
            index += 1
            continue
        value, index = decoder.raw_decode(text, index)
        yield value


def go_imports(text):
    # A small lexical rule, not a Go type checker: inspect import aliases and
    # selector tokens while keeping comments and literals opaque.
    token_pattern = re.compile(r'''//[^\n]*|/\*.*?\*/|`[^`]*`|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\W\d]\w*|[^\s]''', re.S)
    tokens = [match.group() for match in token_pattern.finditer(text)
              if not match.group().startswith(("//", "/*"))]
    imports = []
    for index, token in enumerate(tokens):
        if token != "import":
            continue
        cursor = index + 1
        grouped = cursor < len(tokens) and tokens[cursor] == "("
        if grouped:
            cursor += 1
        while cursor < len(tokens) and tokens[cursor] != ")":
            alias = None
            if re.fullmatch(r"[^\W\d]\w*|\.", tokens[cursor]):
                alias = tokens[cursor]
                cursor += 1
            if cursor >= len(tokens):
                break
            literal = tokens[cursor]
            cursor += 1
            if literal.startswith('"'):
                # Go permits octal and hex escapes in quoted import paths.
                def unescape(match):
                    value = match.group(1)
                    if value.startswith(("x", "u", "U")):
                        return chr(int(value[1:], 16))
                    if value.isdigit():
                        return chr(int(value, 8))
                    return {"a": "\a", "b": "\b", "f": "\f", "n": "\n", "r": "\r",
                            "t": "\t", "v": "\v", "\\": "\\", '"': '"'}.get(value, value)
                imported = re.sub(r'\\(x[0-9a-fA-F]{2}|u[0-9a-fA-F]{4}|U[0-9a-fA-F]{8}|[0-7]{3}|.)',
                                  unescape, literal[1:-1])
            elif literal.startswith("`"):
                imported = literal[1:-1].replace("\r", "")
            else:
                if not grouped:
                    break
                continue
            imports.append((alias, imported))
            if not grouped:
                break
    return tokens, imports


def check_raw_connection(text, location):
    # All owned source (including tests and ignored tags) is checked before go list.
    tokens, imports = go_imports(text)
    aliases = set()
    for alias, imported in imports:
        require(imported != "github.com/jackc/pgx/v5/pgxpool",
                "MODULE_RAW_POOL_IMPORT", location)
        if imported == "github.com/jackc/pgx/v5":
            # Dot imports hide the origin of Conn and connector functions.
            require(alias != ".", "MODULE_RAW_CONNECTION", location + " (pgx dot import)")
            if alias != "_":
                aliases.add(alias or "pgx")
    for index in range(len(tokens) - 2):
        require(not (tokens[index] in aliases and tokens[index + 1] == "." and
                     tokens[index + 2] in ("Conn", "Connect", "ConnectConfig")),
                "MODULE_RAW_CONNECTION", location)


# Even where a pool may be held (pgxadapter), it must stay unexported: an
# exported type, alias, field, embedding, variable or function signature naming
# pgxpool.Pool/Conn or pgx.Conn hands callers a way around tenanttx.Run.
POOL_TYPES = {"github.com/jackc/pgx/v5/pgxpool": ("pgxpool", {"Pool", "Conn"}),
              "github.com/jackc/pgx/v5": ("pgx", {"Conn"})}


def check_pool_export(text, location):
    tokens, imports = go_imports(text)
    names = {}
    for alias, imported in imports:
        if imported in POOL_TYPES and alias not in ("_", "."):
            default, types = POOL_TYPES[imported]
            names[alias or default] = types

    def exported(token):
        return bool(re.fullmatch(r"[^\W\d_]\w*", token)) and token[0].isupper()

    for index in range(len(tokens) - 2):
        if not (tokens[index] in names and tokens[index + 1] == "." and
                tokens[index + 2] in names[tokens[index]] and
                (index == 0 or tokens[index - 1] != ".")):
            continue
        cursor = index - 1
        while cursor >= 0 and tokens[cursor] in ("*", "=", "[", "]"):
            cursor -= 1
        previous = tokens[cursor] if cursor >= 0 else ""
        leaked = exported(previous) and (cursor == 0 or tokens[cursor - 1] != ".")
        leaked = leaked or (previous == "{" and cursor > 0 and tokens[cursor - 1] == "struct")
        if previous in ("(", ")", ","):
            # Signature position: the nearest func keyword names the function
            # (a method name follows its receiver); literals have no name.
            func = max((i for i in range(cursor) if tokens[i] == "func"), default=None)
            if func is not None and "{" not in tokens[func:index] and "}" not in tokens[func:index]:
                name_at = func + 1
                if name_at < len(tokens) and tokens[name_at] == "(":
                    depth = 0
                    for name_at in range(func + 1, index):
                        depth += {"(": 1, ")": -1}.get(tokens[name_at], 0)
                        if depth == 0:
                            break
                    name_at += 1
                leaked = leaked or exported(tokens[name_at])
        require(not leaked, "MODULE_RAW_POOL_EXPORT",
                f"{location}: exported identifier exposes {tokens[index]}.{tokens[index + 2]}")


# Default deny: only the tenant-transaction adapter may hold a pgxpool or open
# a raw pgx connection; the generated db package's integration tests open a
# maintenance connection. Everything else gets connections via tenanttx.
RAW_CONNECTION_ALLOWED = {Path("internal/tenanttx/pgxadapter"): "", Path("internal/db"): "_test.go"}
# Only the httpapi chain may mount the oapi-codegen server; tests anywhere may
# exercise it.
GENERATED_SERVER = "internal/platform/httpapi/generated"
GENERATED_SERVER_IMPORTERS = {Path("internal/platform/httpapi"), Path("internal/platform/httpapi/carthttp"),
                              Path("internal/platform/httpapi/notificationhttp"),
                              Path(GENERATED_SERVER)}


def raw_connection_allowed(relative):
    suffix = RAW_CONNECTION_ALLOWED.get(relative.parent)
    return suffix is not None and relative.name.endswith(suffix)


def check_go(registry, root):
    # go list ./... silently skips nested modules. Reject them before trusting
    # its inventory, including vendor and unused replacements. Package owners
    # come from resolved directories, not a renameable import-path prefix.
    modules = {module["name"]: module for module in registry["modules"]}
    paths = [(root / path, name) for name, module in modules.items()
             for path in [module["go_path"], *module.get("legacy_go_paths", [])]]
    require(all(path.resolve() == path for path, _ in paths),
            "MODULE_REGISTRY_INVALID", "registered Go roots must be canonical repository directories")
    owned_paths = dict(paths)
    target_paths = {root / module["go_path"]: name for name, module in modules.items()}
    target_source = set()

    def reject_vendor_source_alias(path):
        # Vendored package names and linker targets can both be foreign while
        # the compiled directory or individual Go file is our private source.
        require(not path.is_relative_to(root / "vendor") or
                not any(path.resolve().is_relative_to(owned) for owned in owned_paths),
                "MODULE_PRIVATE_SOURCE_ALIAS", "vendor source resolves into a business module")

    env = dict(os.environ, GOPROXY="off", GOSUMDB="off", GOWORK="off")
    metadata = subprocess.run(["go", "mod", "edit", "-json"], cwd=root, env=env,
                              capture_output=True, text=True, timeout=90)
    require(metadata.returncode == 0, "MODULE_GO_LIST_ERROR", "cannot identify repository module")
    manifest = json.loads(metadata.stdout)
    prefix = manifest["Module"]["Path"] + "/"
    go_tokens = re.compile(r'''//[^\n]*|/\*.*?\*/|`[^`]*`|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*' ''', re.S | re.X)
    for directory, dirs, files in os.walk(root):
        # Stable traversal gives the same first rejection on every filesystem.
        dirs.sort()
        files.sort()
        directory = Path(directory)
        if directory == root:
            # Sibling managed checkouts and Git metadata are not this checkout.
            dirs[:] = [name for name in dirs if name not in (".git", ".worktrees")]
        require(directory == root or "go.mod" not in files,
                "MODULE_NESTED_GO_MODULE", str(directory.relative_to(root)))
        # Threat model: this gate stops ACCIDENTAL boundary breaks by honest
        # authors/agents, not a determined adversary. Close whole classes with
        # blunt rules: no workspace files (go.work can splice in unscanned
        # modules) and no non-Go compiled sources (assembly/cgo/syso can reach
        # private symbols with no import or linkname).
        require(not ({"go.work", "go.work.sum"} & set(files)),
                "MODULE_GO_WORK_FORBIDDEN", str(directory.relative_to(root) / "go.work"))
        for name in [*dirs, *files]:
            reject_vendor_source_alias(directory / name)
        for name in dirs:
            path = directory / name
            # os.walk deliberately does not follow directory symlinks. An
            # owned tree cannot hide source in an untraversed alias, including
            # ignored or currently unimported packages. Individual Go files
            # remain scanned through logical and resolved ownership below.
            require(not (path.is_symlink() and any(path.is_relative_to(owned)
                                                  for owned in owned_paths)),
                    "MODULE_SOURCE_DIRECTORY_ALIAS", str(path.relative_to(root)))
        relative_parts = directory.relative_to(root).parts
        # Uncompiled receipts have no source authority; nested modules above
        # are still forbidden. Compiled artifacts are rejected below.
        if relative_parts and relative_parts[0] in ("work", "node_modules"):
            continue
        for file in files:
            require(not file.endswith((".s", ".S", ".sx", ".c", ".cc", ".cpp", ".cxx", ".h", ".hh",
                                       ".hpp", ".m", ".f", ".F", ".for", ".f90", ".syso", ".swig", ".swigcxx")),
                    "MODULE_NON_GO_SOURCE", str((directory / file).relative_to(root)))
            if not file.endswith(".go"):
                continue
            source = directory / file
            for target, name in target_paths.items():
                if source.is_relative_to(target):
                    target_source.add(name)
            text = source.read_text()
            relative = source.relative_to(root)
            resolved = source.resolve()
            # All first-party source, not just business modules: a new bridge
            # package must not be able to open connections on a module's behalf.
            allowed = (raw_connection_allowed(relative) and resolved.is_relative_to(root) and
                       raw_connection_allowed(resolved.relative_to(root)))
            if (not allowed and relative_parts[:1] != ("vendor",)) or any(
                    resolved.is_relative_to(owned) for owned in owned_paths):
                check_raw_connection(text, str(relative))
            if relative_parts[:1] != ("vendor",):
                check_pool_export(text, str(relative))
            require(not re.search(r'^\s*import\s*(?:\(\s*)?"C"', text, re.M),
                    "MODULE_NON_GO_SOURCE", str(source.relative_to(root)) + " (cgo)")
            for token in go_tokens.finditer(text):
                directive = re.match(r"//go:linkname(?:\s|$)", token.group())
                vendored = relative_parts and relative_parts[0] == "vendor"
                # Repository aliases can omit /internal/ in a linker target.
                # Vendor has no legitimate linker port into first-party code;
                # deny the repository prefix, including private package roots.
                require(not directive or (vendored and prefix not in token.group()),
                        "MODULE_LINKNAME_FORBIDDEN", str(source.relative_to(root)))

    def reject_local_replace(replacement):
        if replacement and not replacement.get("Version"):
            path = Path(replacement["Path"])
            require(not (root / path).resolve().is_relative_to(root),
                    "MODULE_LOCAL_REPLACE", "replacement resolves inside repository")

    for replacement in manifest.get("Replace") or []:
        reject_local_replace(replacement["New"])

    def module_path(package):
        directory = Path(package.get("Dir", "/")).resolve()
        for package_path, name in owned_paths.items():
            if directory.is_relative_to(package_path):
                return [name, *directory.relative_to(package_path).parts]
        if directory.is_relative_to(root / "internal/modules"):
            raise GovernanceError("MODULE_UNREGISTERED_PACKAGE: " + directory.relative_to(root / "internal/modules").parts[0])
        return None

    implementation_source = set()
    for tags in ([], ["-tags", "pgx"]):
        result = subprocess.run(["go", "list", "-e", "-deps", "-json", "-test", *tags, "./..."],
                                cwd=root, env=env, capture_output=True, text=True, timeout=90)
        errors = result.returncode != 0
        packages = list(json_stream(result.stdout))
        # Test variants carry suffixes, but their Dir still names real source.
        by_import = {package["ImportPath"]: package for package in packages}

        def imported_package(imported):
            return by_import.get(imported) or by_import.get(imported.split(" [", 1)[0], {})

        def first_party_package(package):
            directory = Path(package.get("Dir", "/")).resolve()
            return directory.is_relative_to(root) and not directory.is_relative_to(root / "vendor")

        def check_reachable_imports(package, source):
            # Keep the importing module's context across same-module packages
            # and unregistered first-party bridges. Stop at a foreign module's
            # public entry: its implementation dependencies belong to it.
            pending = list(package.get("Imports", []))
            seen = set()
            while pending:
                imported = pending.pop()
                if imported in seen:
                    continue
                seen.add(imported)
                target = imported_package(imported)
                destination = module_path(target)
                if destination is not None and destination[0] != source[0]:
                    edge = f"{source[0]} -> {destination[0]}"
                    require("internal" not in destination[1:], "MODULE_PRIVATE_IMPORT", edge)
                    relative = str(Path(target["Dir"]).resolve().relative_to(root))
                    public_port = relative in modules[destination[0]].get("approved_public_ports", [])
                    require((len(destination) > 1 and destination[1] == "contract") or public_port,
                            "MODULE_NONCONTRACT_IMPORT", edge)
                    require(destination[0] in modules[source[0]]["dependencies"],
                            "MODULE_UNDECLARED_DEPENDENCY", edge)
                    continue
                if first_party_package(target):
                    pending.extend(target.get("Imports", []))
        for package in packages:
            # Excluded receipts are harmless only when not compiled/imported.
            # The actual Go stream also exposes local replacements skipped by
            # ./... and lets us reject artifact bridges or a second go.mod.
            lexical_directory = Path(package.get("Dir", "/"))
            reject_vendor_source_alias(lexical_directory)
            for field in ("GoFiles", "CgoFiles", "TestGoFiles", "XTestGoFiles"):
                for name in package.get(field, []):
                    reject_vendor_source_alias(lexical_directory / name)
            directory = lexical_directory.resolve()
            module = package.get("Module") or {}
            reject_local_replace(module.get("Replace"))
            module_directory = Path(module.get("Dir", "/")).resolve()
            first_party = (directory.is_relative_to(root) and
                           not directory.is_relative_to(root / "vendor")) or module_directory.is_relative_to(root)
            if first_party and directory.is_relative_to(root):
                go_mod = module.get("GoMod")
                require(not go_mod or Path(go_mod).resolve() == root / "go.mod",
                        "MODULE_NESTED_GO_MODULE", str(directory.relative_to(root)))
                relative_parts = directory.relative_to(root).parts
                require(not relative_parts or relative_parts[0] not in
                        ("work", ".worktrees", ".git", "vendor", "node_modules"),
                        "MODULE_ARTIFACT_SOURCE", str(directory.relative_to(root)))
            if (first_party and directory.is_relative_to(root) and not package.get("ForTest") and
                    not package["ImportPath"].endswith(".test") and
                    directory.relative_to(root) not in GENERATED_SERVER_IMPORTERS):
                require(prefix + GENERATED_SERVER not in package.get("Imports", []),
                        "MODULE_GENERATED_SERVER_IMPORT", str(directory.relative_to(root)))
            source = module_path(package)
            errors = errors or bool(package.get("Error") or package.get("DepsErrors"))
            if source is None:
                continue
            # Use actual Dir and real files, never an import-name prefix or a
            # synthetic .test main. Legacy roots have separate ownership but
            # cannot promote the reserved target's implementation status.
            target_root = root / modules[source[0]]["go_path"]
            if directory.is_relative_to(target_root):
                for field in ("GoFiles", "CgoFiles", "TestGoFiles", "XTestGoFiles"):
                    for file in package.get(field, []):
                        path = (directory / file).resolve()
                        if path.is_file() and path.is_relative_to(target_root):
                            if field in ("GoFiles", "CgoFiles") and not path.name.endswith("_test.go"):
                                implementation_source.add(source[0])
            for imported in package.get("Imports", []):
                require(imported.split(" [", 1)[0] != "github.com/jackc/pgx/v5/pgxpool",
                        "MODULE_RAW_POOL_IMPORT", source[0])
            check_reachable_imports(package, source)
        require(not errors, "MODULE_GO_LIST_ERROR", "Go package loading failed (default/pgx, including tests)")
    for name, module in modules.items():
        consistent = (name in implementation_source if module["go_status"] == "implemented"
                      else name not in target_source)
        require(consistent, "MODULE_GO_STATUS_MISMATCH", name + ": target go_path differs from go_status")


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
