#!/usr/bin/env python3
"""Offline admission gate; SQL semantics require the separate real PG gate."""
from pathlib import Path
root = Path(__file__).resolve().parents[1]
files = list((root/'migrations').glob('*_notifications.sql'))
assert len(files) == 1, 'NOTIFICATIONS_SCHEMA_ADMISSION: exactly one notification migration'
sql = files[0].read_text()
for marker in ('notification_deliveries','notification_redrive_requests','FORCE ROW LEVEL SECURITY',
               'claim_notification_outbox','notification_begin','notification_finish','notification_redrive',
               "delivery_target='notifications.email'", "nullif(current_setting('app.tenant_id',true),'')::uuid"):
    assert marker in sql, f'NOTIFICATIONS_SCHEMA_ADMISSION: missing {marker}'
assert "payload->>'recipient'" not in sql, 'recipient must come from authoritative records'
assert "to_regclass('app.quotes')" in sql, 'unmerged quote dependency must fail closed'
assert 'tests/db_notifications.sql' in (root/'scripts/check_pg.sh').read_text()

assert "delivery_target<>'quotes'" in sql, 'NOTIFICATIONS_QUOTE_SOURCE_AUTH: runtime must not forge quote source'
assert "q.status=CASE $3" in sql and "q.version::text=$4" in sql, 'NOTIFICATIONS_QUOTE_SOURCE_AUTH: authoritative lifecycle and optional version'

print('PASS: notifications offline schema/source admission (NOT SQL execution)')
