#!/usr/bin/env python3
"""In-development migrations: migrations/pending/<stable-id>_<name>.sql.

Responsibility: order pending migrations deterministically and give them the
next contiguous numbers after the last landed (frozen, forward-only) migration.
Called by scripts/migrate.sh (materialize → temp dir), CI (ci-normalize, in the
ephemeral checkout only) and scripts/land_migration.sh (land, once, by the
integrator). Depends on the stdlib only.
Invariants: numbered files are never renamed/edited; numbered history must be
contiguous 001..N; order = sorted stable-id, overridden only by explicit
`-- depends-on: <stable-id>` header lines; unknown deps, cycles, duplicate ids
or names fail closed (exit 2) before anything is written. Every pending file
starts with `-- stable-id: <id>`; land is a byte-identical move, so the header
lets depends-on lines resolve after their target landed and keeps checksums.
`find-stable-id <id> [root]` locates one migration by that header across both
layouts (pending/ or numbered), so callers keep working before and after C-22a
is formally landed; a missing or duplicated id fails closed with exit 2.
"""
import re
import shutil
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
NUMBERED = re.compile(r"([0-9]{3})_([a-z][a-z0-9_]*)\.sql")
PENDING = re.compile(r"([a-z0-9][a-z0-9-]*)_([a-z][a-z0-9_]*)\.sql")
DEPENDS = re.compile(r"--\s*depends-on:\s*(\S+)\s*$")
STABLE_ID = re.compile(r"-- stable-id: ([a-z0-9][a-z0-9-]*)\n")
REFERENCE_FILES = ("modules/registry.yaml", "sqlc.yaml")


class PendingError(Exception):
    pass


def numbered(root):
    files = sorted((root / "migrations").glob("[0-9][0-9][0-9]_*.sql"))
    for expected, path in enumerate(files, 1):
        match = NUMBERED.fullmatch(path.name)
        if not match:
            raise PendingError(f"Invalid migration: {path.name}")
        if int(match[1]) != expected:
            raise PendingError(f"Migration numbering must be contiguous: expected {expected}, found {path.name}")
    return files


def landed_ids(files):
    """Stable-ids carried (from pending/) on the first line of landed files."""
    ids = set()
    for path in files:
        with path.open() as handle:
            match = STABLE_ID.fullmatch(handle.readline())
        if match:
            ids.add(match[1])
    return ids


def find_stable_id(root, stable_id):
    """The file carrying `-- stable-id: <stable_id>`, pending or landed.

    Landing is a byte-identical move from pending/<id>_<name>.sql to
    migrations/<NNN>_<name>.sql, so the stable-id header is the only stable
    handle. Both layouts are scanned; a duplicate fails closed.
    """
    directory = root / "migrations"
    candidates = sorted(directory.glob("[0-9][0-9][0-9]_*.sql"))
    pending = directory / "pending"
    if pending.is_dir():
        candidates += sorted(pending.glob("*.sql"))
    found = []
    for path in candidates:
        with path.open() as handle:
            match = STABLE_ID.fullmatch(handle.readline())
        if match and match[1] == stable_id:
            found.append(path)
    if len(found) > 1:
        raise PendingError(
            f"Duplicate stable-id {stable_id}: {', '.join(path.name for path in found)}")
    return found[0] if found else None


def ordered_pending(root):
    """Pending files in apply order: Kahn's algorithm, ties broken by stable-id."""
    directory = root / "migrations" / "pending"
    entries = sorted(directory.iterdir()) if directory.is_dir() else []
    landed_files = numbered(root)
    landed_names = {NUMBERED.fullmatch(p.name)[2] for p in landed_files}
    landed = landed_ids(landed_files)
    by_id, deps, names = {}, {}, set()
    for path in entries:
        match = PENDING.fullmatch(path.name)
        if not match or not path.is_file():
            raise PendingError(f"Invalid pending migration: {path.name} (want <stable-id>_<name>.sql)")
        stable_id, name = match[1], match[2]
        with path.open() as handle:
            header = STABLE_ID.fullmatch(handle.readline())
        if not header or header[1] != stable_id:
            raise PendingError(f"{path.name}: first line must be '-- stable-id: {stable_id}'")
        if stable_id in by_id or stable_id in landed:
            raise PendingError(f"Duplicate pending stable-id: {stable_id}")
        if name in names or name in landed_names:
            raise PendingError(f"Duplicate migration name: {name}")
        by_id[stable_id], names = path, names | {name}
        deps[stable_id] = {m[1] for line in path.read_text().splitlines() if (m := DEPENDS.match(line.strip()))}
    for stable_id, required in deps.items():
        unknown = sorted(required - by_id.keys() - landed)
        if unknown:
            raise PendingError(f"{stable_id}: unknown depends-on {', '.join(unknown)}")
        deps[stable_id] = required - landed  # already landed = satisfied
    order, done = [], set()
    while len(order) < len(by_id):
        ready = sorted(i for i in by_id if i not in done and deps[i] <= done)
        if not ready:
            raise PendingError(f"depends-on cycle among: {', '.join(sorted(by_id.keys() - done))}")
        order.append(ready[0])
        done.add(ready[0])
    return [by_id[i] for i in order]


def plan(root):
    """[(source, final numbered file name)] for every pending migration."""
    start = len(numbered(root)) + 1
    return [(path, f"{start + i:03d}_{PENDING.fullmatch(path.name)[2]}.sql")
            for i, path in enumerate(ordered_pending(root))]


def materialize(root, dest):
    """Temp dir for migrate.sh: landed files as-is, then pending renumbered."""
    steps = plan(root)
    dest.mkdir(parents=True, exist_ok=True)
    for path in numbered(root):
        shutil.copy2(path, dest / path.name)
    for source, target in steps:
        shutil.copy2(source, dest / target)
        print(f"PENDING {source.name} -> {target}", file=sys.stderr)


def land(root, stable_id):
    """Move the next pending migration to its final number and rewrite references."""
    steps = plan(root)
    if not steps or not steps[0][0].name.startswith(stable_id + "_"):
        known = [s.name for s, _ in steps]
        raise PendingError(f"{stable_id} is not the next pending migration to land (order: {known})")
    source, target = steps[0]
    destination = root / "migrations" / target
    if destination.exists():
        raise PendingError(f"{destination} already exists")
    source.rename(destination)
    old, new = f"migrations/pending/{source.name}", f"migrations/{target}"
    for relative in REFERENCE_FILES:
        reference = root / relative
        if reference.is_file() and old in (text := reference.read_text()):
            reference.write_text(text.replace(old, new))
    print(f"LANDED {old} -> {new}")


def main(argv):
    try:
        if argv[:1] == ["order"] and len(argv) == 1:
            for source, target in plan(ROOT):
                print(f"{source.name} -> {target}")
        elif argv[:1] == ["materialize"] and len(argv) == 2:
            materialize(ROOT, Path(argv[1]))
        elif argv[:1] == ["land"] and len(argv) == 2:
            land(ROOT, argv[1])
        elif argv[:1] == ["find-stable-id"] and len(argv) in (2, 3):
            root = Path(argv[2]).resolve() if len(argv) == 3 else ROOT
            path = find_stable_id(root, argv[1])
            if path is None:
                print(f"PENDING_MIGRATION_INVALID: no migration carries stable-id {argv[1]}",
                      file=sys.stderr)
                return 2
            print(path.resolve())
        elif argv[:1] == ["ci-normalize"] and len(argv) == 1:
            # Ephemeral CI checkout only: land every pending file in order so all
            # gates (which glob migrations/NNN_*.sql) see the final candidate.
            for source, _ in plan(ROOT):
                land(ROOT, PENDING.fullmatch(source.name)[1])
        else:
            print("usage: pending_migrations.py order | materialize DIR | land STABLE_ID | "
                  "find-stable-id STABLE_ID [ROOT] | ci-normalize", file=sys.stderr)
            return 64
    except PendingError as error:
        print(f"PENDING_MIGRATION_INVALID: {error}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
