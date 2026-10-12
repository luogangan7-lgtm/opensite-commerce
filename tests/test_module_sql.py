#!/usr/bin/env python3
"""A1 static SQL ownership regressions; stdlib only, called by check_all.

Temporary query trees test the exported gate, including injected illegal writes.
The real PostgreSQL catalog and sqlc gates remain separate acceptance layers.
"""
import importlib.util
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("module_sql", ROOT / "scripts/module_sql.py")
checker = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(checker)


class SQLWritesTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.registry = {"modules": [
            dict(name="catalog", status="implemented", go_status="planned", owns_tables=["products"]),
            dict(name="site", status="implemented", go_status="planned", owns_tables=["sites"]),
            dict(name="future", status="planned", go_status="planned", owns_tables=["future_table"]),
        ]}

    def query(self, text, path="queries/catalog.sql"):
        target = self.root / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(text)

    def green(self):
        checker.check_sql_writes(self.registry, self.root)

    def red(self, marker):
        with self.assertRaisesRegex(checker.SqlOwnershipError, marker):
            self.green()

    def test_own_qualified_writes_and_cross_module_reads(self):
        self.query('INSERT INTO "app"."products" VALUES (1); UPDATE ONLY app.products SET x=2; '
                   'DELETE FROM ONLY app.products WHERE x=3; SELECT * FROM app.sites;')
        self.green()

    def test_cross_module_insert(self):
        self.query("INSERT INTO app.sites VALUES (1);")
        self.red("MODULE_SQL_CROSS_MODULE_WRITE")

    def test_cross_module_update_in_cte(self):
        self.query("WITH changed AS (UPDATE app.sites SET x=1 RETURNING *) SELECT * FROM changed;")
        self.red("MODULE_SQL_CROSS_MODULE_WRITE")

    def test_cross_module_delete(self):
        self.query("DELETE FROM app.sites;")
        self.red("MODULE_SQL_CROSS_MODULE_WRITE")

    def test_cross_module_merge(self):
        self.query("MERGE INTO app.sites USING app.products ON true WHEN MATCHED THEN DELETE;")
        self.red("MODULE_SQL_CROSS_MODULE_WRITE")

    def test_own_merge_actions_and_upsert(self):
        self.query("MERGE INTO app.products USING app.sites ON true WHEN MATCHED THEN UPDATE SET x=1 "
                   "WHEN NOT MATCHED THEN INSERT (x) VALUES (1); INSERT INTO app.products VALUES (1) "
                   "ON CONFLICT (x) DO UPDATE SET x=2; SELECT * FROM app.products FOR NO KEY UPDATE;")
        self.green()

    def test_comments_literals_and_dollar_quotes_are_not_write_witnesses(self):
        self.query("/* UPDATE app.sites /* INSERT INTO app.sites */ */\n"
                   "-- DELETE FROM app.sites\nSELECT 'UPDATE app.sites', 'it''s INSERT INTO app.sites', "
                   "E'escaped\\\' UPDATE app.sites', $$ DELETE FROM app.sites $$, "
                   "$body$ MERGE INTO app.sites $body$, $标签$ UPDATE app.sites $标签$, "
                   "\"update\" FROM app.products;")
        self.green()

    def test_statement_after_standard_backslash_string_is_checked(self):
        self.query("SELECT 'slash\\'; UPDATE app.sites SET x=1;")
        self.red("MODULE_SQL_CROSS_MODULE_WRITE")

    def test_unregistered_and_planned_tables_are_not_owned(self):
        for table in ("ghost", "future_table"):
            with self.subTest(table=table):
                self.query(f"INSERT INTO app.{table} VALUES (1);")
                self.red("MODULE_SQL_UNDECLARED_WRITE")

    def test_unqualified_and_other_schema_fail_closed(self):
        for target, marker in (("products", "MODULE_SQL_UNQUALIFIED_TARGET"),
                               ("public.products", "MODULE_SQL_UNDECLARED_WRITE")):
            with self.subTest(target=target):
                self.query(f"UPDATE {target} SET x=1;")
                self.red(marker)

    def test_quoted_names_preserve_case(self):
        self.query('UPDATE app."Products" SET x=1;')
        self.red("MODULE_SQL_UNDECLARED_WRITE")

    def test_both_query_layouts_map_to_exact_module(self):
        self.query("UPDATE app.products SET x=1;", "queries/catalog/update.sql")
        self.green()
        self.query("UPDATE app.products SET x=1;", "queries/site/update.sql")
        self.red("MODULE_SQL_CROSS_MODULE_WRITE")

    def test_unknown_or_nested_query_paths_rejected(self):
        for path in ("queries/catalog_extra.sql", "queries/catalog/nested/update.sql"):
            with self.subTest(path=path):
                self.query("SELECT 1;", path)
                self.red("MODULE_SQL_QUERY_PATH")
                (self.root / path).unlink()

    def test_dynamic_and_ddl_are_rejected(self):
        for sql in ("DO $$ BEGIN EXECUTE 'UPDATE app.sites'; END $$;", "CALL app.mutate();",
                    "EXECUTE prepared_write;", "PREPARE x AS UPDATE app.products SET x=1;",
                    "TRUNCATE app.products;", "CREATE TABLE app.new (x int);",
                    "SELECT * INTO app.new FROM app.products;",
                    "SET search_path = app; UPDATE products SET x=1;"):
            with self.subTest(sql=sql):
                self.query(sql)
                self.red("MODULE_SQL_UNSUPPORTED")

    def test_unterminated_lexical_constructs_fail_closed(self):
        for sql in ("SELECT 'abc", 'SELECT "abc', "SELECT $tag$abc", "/* never closed"):
            with self.subTest(sql=sql):
                self.query(sql)
                self.red("MODULE_SQL_LEXICAL")

    def test_missing_write_target_fails_closed(self):
        self.query("INSERT INTO ($1);")
        self.red("MODULE_SQL_UNSUPPORTED")

    def test_duplicate_live_owner_fails_closed(self):
        self.registry["modules"][1]["owns_tables"].append("products")
        self.query("UPDATE app.products SET x=1;")
        self.red("MODULE_SQL_DUPLICATE_OWNER")


if __name__ == "__main__":
    unittest.main()
