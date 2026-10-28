#!/usr/bin/env python3
"""A1 business-query write ownership gate; Python stdlib only.

Called by check_modules; reads queries and implemented registry table ownership,
never a database. Supports SELECT/WITH and static INSERT INTO, UPDATE [ONLY],
DELETE FROM [ONLY], MERGE INTO, including writable CTEs, upserts and MERGE actions.
Write targets must be explicitly schema-qualified app tables: no search_path
assumptions. DDL, transaction/config statements, DO/CALL/PREPARE/EXECUTE and other
statement roots fail closed. Comments, literals and quoted identifiers cannot
become keyword witnesses. This is a direct-DML gate, not a SQL validity or
stored-function side-effect proof; sqlc and database role gates remain required.
"""
from pathlib import Path
import re


class SqlOwnershipError(Exception):
    """Stable MODULE_SQL_* diagnostics, containing no query literals."""


def _fail(marker, location, detail):
    raise SqlOwnershipError(f"{marker}: {location}: {detail}")


def _tokens(sql, location, literal_values=False):
    """Lex PostgreSQL strings/comments; retain identifier case and token lines."""
    result, i, line = [], 0, 1
    while i < len(sql):
        start, token_line = i, line
        char = sql[i]
        if char.isspace():
            line += char == "\n"
            i += 1
            continue
        if sql.startswith("--", i):
            end = sql.find("\n", i)
            i = len(sql) if end < 0 else end
            continue
        if sql.startswith("/*", i):
            i += 2
            depth = 1
            while i < len(sql) and depth:
                if sql.startswith("/*", i):
                    depth += 1
                    i += 2
                elif sql.startswith("*/", i):
                    depth -= 1
                    i += 2
                else:
                    i += 1
            if depth:
                _fail("MODULE_SQL_LEXICAL", location, f"line {token_line}: unterminated comment")
            line += sql[start:i].count("\n")
            continue
        escaped = char in "eE" and sql[i:i + 2].lower() == "e'"
        if char in "'\"" or escaped:
            quote = "'" if escaped else char
            i += 2 if escaped else 1
            value = []
            while i < len(sql):
                if escaped and sql[i] == "\\":
                    i += 2
                elif sql[i] == quote:
                    if i + 1 < len(sql) and sql[i + 1] == quote:
                        value.append(quote)
                        i += 2
                    else:
                        i += 1
                        break
                else:
                    value.append(sql[i])
                    i += 1
            else:
                _fail("MODULE_SQL_LEXICAL", location, f"line {token_line}: unterminated quote")
            result.append(("ident" if quote == '"' else "literal", "".join(value) if quote == '"' or literal_values and not escaped else "", token_line))
            line += sql[start:i].count("\n")
            continue
        if char == "$":
            match = re.match(r"\$(?:[^\W\d]\w*)?\$", sql[i:])
            if match:
                delimiter = match.group()
                end = sql.find(delimiter, i + len(delimiter))
                if end < 0:
                    _fail("MODULE_SQL_LEXICAL", location, f"line {token_line}: unterminated dollar quote")
                i = end + len(delimiter)
                result.append(("literal", "", token_line))
                line += sql[start:i].count("\n")
                continue
        if char.isalpha() or char == "_":
            i += 1
            while i < len(sql) and (sql[i].isalnum() or sql[i] in "_$"):
                i += 1
            result.append(("word", sql[start:i].lower(), token_line))
        else:
            result.append(("symbol", char, token_line))
            i += 1
    return result


def _word(tokens, index):
    return tokens[index][1] if 0 <= index < len(tokens) and tokens[index][0] == "word" else None


def _target(tokens, index, location):
    if _word(tokens, index) == "only":
        index += 1
        if index < len(tokens) and tokens[index][1] == "(":
            index += 1
    names = []
    while index < len(tokens) and tokens[index][0] in ("word", "ident"):
        names.append(tokens[index][1])
        index += 1
        if index >= len(tokens) or tokens[index][1] != ".":
            break
        index += 1
        if index >= len(tokens) or tokens[index][0] not in ("word", "ident"):
            _fail("MODULE_SQL_UNSUPPORTED", location, "invalid write target")
    if not names or len(names) > 2:
        _fail("MODULE_SQL_UNSUPPORTED", location, "expected static schema.table write target")
    if len(names) == 1:
        _fail("MODULE_SQL_UNQUALIFIED_TARGET", location, "write target requires explicit app schema")
    return tuple(names)


def _check_statement(tokens, module, owners, location):
    if not tokens:
        return
    first = _word(tokens, 0)
    if first not in ("select", "with", "insert", "update", "delete", "merge"):
        _fail("MODULE_SQL_UNSUPPORTED", location, "only SELECT/WITH/static DML statements supported")
    merge = any(_word(tokens, i) == "merge" and _word(tokens, i + 1) == "into" for i in range(len(tokens)))
    for i, token in enumerate(tokens):
        word = _word(tokens, i)
        following, previous = _word(tokens, i + 1), _word(tokens, i - 1)
        if word == "into" and previous not in ("insert", "merge"):
            _fail("MODULE_SQL_UNSUPPORTED", location, "SELECT INTO and other indirect targets unsupported")
        if word == "insert":
            if merge and previous == "then" and following != "into":
                continue  # MERGE insert action writes the already checked MERGE target.
            if following != "into":
                _fail("MODULE_SQL_UNSUPPORTED", location, "INSERT requires static INTO target")
            target_index = i + 2
        elif word == "update":
            if (previous == "do" or (merge and previous == "then")) and following == "set":
                continue  # Upsert/MERGE action uses the checked INSERT/MERGE target.
            if previous == "for" or [_word(tokens, j) for j in range(i - 3, i)] == ["for", "no", "key"]:
                continue  # SELECT row-lock clause is not a write target.
            target_index = i + 1
        elif word == "delete":
            if merge and previous == "then" and following != "from":
                continue
            if following != "from":
                _fail("MODULE_SQL_UNSUPPORTED", location, "DELETE requires static FROM target")
            target_index = i + 2
        elif word == "merge":
            if following != "into":
                _fail("MODULE_SQL_UNSUPPORTED", location, "MERGE requires static INTO target")
            target_index = i + 2
        else:
            continue
        where = f"{location}:{token[2]}"
        schema, table = _target(tokens, target_index, where)
        owner = owners.get(table) if schema == "app" else None
        if owner is None:
            _fail("MODULE_SQL_UNDECLARED_WRITE", where, f"no implemented owner for {schema}.{table}")
        if owner != module:
            _fail("MODULE_SQL_CROSS_MODULE_WRITE", where, f"{module} writes {schema}.{table} owned by {owner}")


def check_sql_writes(registry, root):
    """Raise SqlOwnershipError on violation; return the number of query files.

    The caller validates registry structure. Only status=implemented (SQL model
    status), never go_status, contributes live ownership. No file or DB writes.
    """
    root = Path(root).resolve()
    modules = {module["name"] for module in registry["modules"]}
    owners = {}
    for module in registry["modules"]:
        if module["status"] != "implemented":
            continue
        for table in module["owns_tables"]:
            if table in owners:
                _fail("MODULE_SQL_DUPLICATE_OWNER", "registry", table)
            owners[table] = module["name"]
    files = sorted((root / "queries").rglob("*.sql"))
    for path in files:
        relative = path.relative_to(root)
        parts = relative.parts
        module = path.stem if len(parts) == 2 else parts[1]
        if len(parts) not in (2, 3) or module not in modules or not path.resolve().is_relative_to(root):
            _fail("MODULE_SQL_QUERY_PATH", relative, "expected queries/<module>.sql or queries/<module>/*.sql")
        try:
            tokens = _tokens(path.read_text(encoding="utf-8"), relative)
        except (OSError, UnicodeError) as error:
            raise SqlOwnershipError(f"MODULE_SQL_READ: {relative}: unreadable query") from error
        statement = []
        for token in tokens:
            if token[:2] == ("symbol", ";"):
                _check_statement(statement, module, owners, relative)
                statement = []
            else:
                statement.append(token)
        _check_statement(statement, module, owners, relative)
    return len(files)
