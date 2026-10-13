#!/usr/bin/env python3
"""Container PostgreSQL transport for local ops; depends only on stdlib.

Called by ops_deploy/ops_backup/ops_drill; owns no business tables. Secrets travel
through stdin, diagnostics never include SQL/client stderr, and waits are bounded.
"""
from contextlib import contextmanager
import os
from pathlib import Path
import re
import select
import subprocess

ROOT = Path(__file__).resolve().parents[1]


class OpsError(RuntimeError):
    """Safe operator-facing failure (no upstream payloads)."""


def load_environment():
    """Read ops-only simple dotenv: KEY=value or matching quotes, env wins.

    Full-line comments/blank lines are supported; shell syntax, interpolation,
    escapes, inline comments and duplicate keys fail closed without partial writes.
    """
    configured = os.environ.get("OPENSITE_ENV_FILE")
    path = Path(configured) if configured else ROOT / ".env"
    if not path.is_absolute():
        path = ROOT / path
    if not path.exists() and not configured:
        return
    try:
        lines = path.read_text().splitlines()
    except (OSError, UnicodeError):
        raise OpsError("cannot read ops environment file") from None
    values = {}
    for number, raw in enumerate(lines, 1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        key, separator, value = line.partition("=")
        if (not separator or not re.fullmatch(r"(?:POSTGRES_PASSWORD|COMPOSE_PROJECT_NAME|OPENSITE_[A-Z0-9_]+)", key)
                or key in values):
            raise OpsError(f"invalid ops environment assignment at line {number}")
        if value[:1] in {"'", '"'}:
            if len(value) < 2 or value[-1] != value[0]:
                raise OpsError(f"invalid quoted ops value at line {number}")
            value = value[1:-1]
        elif any(character.isspace() for character in value):
            raise OpsError(f"unsupported ops value syntax at line {number}")
        if any(character in value for character in "'\"$`\\") or any(ord(character) < 32 for character in value):
            raise OpsError(f"unsupported ops value syntax at line {number}")
        values[key] = value
    for key, value in values.items():
        os.environ.setdefault(key, value)


def timeout():
    try:
        value = int(os.environ.get("OPENSITE_OPS_TIMEOUT", "300"))
    except ValueError:
        raise OpsError("OPENSITE_OPS_TIMEOUT must be an integer") from None
    if not 1 <= value <= 3600:
        raise OpsError("OPENSITE_OPS_TIMEOUT must be 1..3600 seconds")
    return value


def db_name(value=None):
    name = os.environ.get("OPENSITE_DB", "opensite") if value is None else value
    if not re.fullmatch(r"[a-z][a-z0-9_]{0,62}", name) or name in {"postgres", "template0", "template1"}:
        raise OpsError("invalid or system database name")
    return name


def quote_ident(value):
    return '"' + value.replace('"', '""') + '"'


def compose_command(args):
    command = ["docker", "compose", "-f", str(ROOT / "deploy/compose.yaml")]
    env_file = os.environ.get("OPENSITE_ENV_FILE")
    if env_file:
        command.extend(["--env-file", env_file])
    project = os.environ.get("COMPOSE_PROJECT_NAME")
    if project:
        if not re.fullmatch(r"[a-z0-9][a-z0-9_-]{0,62}", project):
            raise OpsError("invalid COMPOSE_PROJECT_NAME")
        command.extend(["--project-name", project])
    return command + list(args)


def compose(args, input=None, capture=True, stdout=None):
    """Run configured Compose with a timeout; return byte CompletedProcess."""
    try:
        result = subprocess.run(compose_command(args), cwd=ROOT, input=input,
                                stdout=stdout if stdout is not None else (subprocess.PIPE if capture else subprocess.DEVNULL),
                                stderr=subprocess.PIPE, timeout=timeout())
    except subprocess.TimeoutExpired:
        raise OpsError("compose operation timed out") from None
    except OSError:
        raise OpsError("cannot start docker compose") from None
    if result.returncode:
        raise OpsError(f"compose operation failed (exit {result.returncode}; client details suppressed)")
    return result


def admin_password():
    password = os.environ.get("POSTGRES_PASSWORD", "")
    if not password or any(c in password for c in "\n\r\0"):
        raise OpsError("POSTGRES_PASSWORD must be set and single-line")
    return password.encode() + b"\n"


def pg_args(args):
    # Read exactly one line before handing remaining stdin (SQL/archive) to PG.
    return ["exec", "-T", "postgres", "sh", "-c",
            'IFS= read -r PGPASSWORD || exit 2; export PGPASSWORD; exec "$@"',
            "ops-pg"] + list(args)


def pg_command(args, input=None, capture=True, stdout=None):
    payload = input.encode() if isinstance(input, str) else (input or b"")
    return compose(pg_args(args), input=admin_password() + payload,
                   capture=capture, stdout=stdout)


def psql(sql, database=None):
    # postgres is the fixed maintenance database, never a backup/restore target.
    name = "postgres" if database == "postgres" else db_name(database)
    args = ["psql", "-X", "-q", "-At", "-h", "127.0.0.1", "-U", "postgres",
            "-d", name, "-v", "ON_ERROR_STOP=1"]
    return pg_command(args, input=sql).stdout.decode()


def migrate(database=None):
    # Use the repository's frozen checksum/advisory-lock runner. Its libpq
    # locator stays inside the container environment, never in client argv.
    name = db_name(database)
    return pg_command(["bash", "-c",
                       'export DATABASE_URL="host=127.0.0.1 user=postgres dbname=$1"; '
                       'exec bash /workspace/scripts/migrate.sh', "ops-migrate", name])


@contextmanager
def snapshot(database=None):
    """Hold an exported REPEATABLE READ snapshot while dump+manifest consume it."""
    args = ["psql", "-X", "-q", "-At", "-h", "127.0.0.1", "-U", "postgres",
            "-d", db_name(database), "-v", "ON_ERROR_STOP=1"]
    process = None
    try:
        process = subprocess.Popen(compose_command(pg_args(args)), cwd=ROOT,
                                   stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                   stderr=subprocess.DEVNULL)
        process.stdin.write(admin_password() + b"BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;\nSELECT pg_export_snapshot();\n")
        process.stdin.flush()
        if not select.select([process.stdout], [], [], timeout())[0]:
            raise OpsError("snapshot export timed out")
        identifier = process.stdout.readline().decode().strip()
        if not re.fullmatch(r"[0-9A-Fa-f]+-[0-9A-Fa-f]+-[0-9]+", identifier):
            raise OpsError("snapshot export failed (client details suppressed)")
        yield identifier
        process.communicate(input=b"COMMIT;\n\\q\n", timeout=timeout())
        if process.returncode:
            raise OpsError("snapshot transaction failed")
    except subprocess.TimeoutExpired:
        raise OpsError("snapshot operation timed out") from None
    except OSError:
        raise OpsError("cannot start snapshot client") from None
    finally:
        if process is not None:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
            for pipe in (process.stdin, process.stdout):
                if pipe is not None:
                    pipe.close()
