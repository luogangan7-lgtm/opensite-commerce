"""CI notification recovery fixtures and independent persistence witness.

Called by guarded run.py only. Uses production fanout/worker; attempts=9 is an
explicit final-attempt fixture, never a fabricated failure or delivery result.
"""
import json
import time
import urllib.request

SOURCE_FILTER = "source_event_id::text IN ('c3400001-0000-4000-8000-000000000001','c3400001-0000-4000-8000-000000000002')"


def seed(psql):
    psql("""BEGIN;
    INSERT INTO app.tenant_memberships(tenant_id,user_id,role)
      SELECT t.id,u.id,'viewer' FROM app.tenants t CROSS JOIN app.users u
      WHERE t.id IN ('c1900010-0000-4000-8000-000000000001','c1900010-0000-4000-8000-000000000002')
        AND u.email='staff-viewer@example.test';
    COMMIT;""")
    for n in (1, 2):
        tenant = f'c1900010-0000-4000-8000-{n:012d}'
        source = f'c3400001-0000-4000-8000-{n:012d}'
        # Fanout executes as its FORCE-RLS executor, so source fixtures need
        # exactly the same tenant context as real business-event producers.
        psql(f"""BEGIN;
        SELECT set_config('app.tenant_id','{tenant}',true);
        INSERT INTO app.outbox_events(tenant_id,id,event_type,payload,delivery_target,dedup_key)
          VALUES('{tenant}','{source}','order.fulfilled',
            jsonb_build_object('order_id','c1900003-0000-4000-8000-{n:012d}'),
            'orders.events','admin-redrive-{n}');
        UPDATE app.outbox_events e SET attempts=9 FROM app.notification_deliveries d
          WHERE (d.tenant_id,d.dispatch_event_id)=(e.tenant_id,e.id)
            AND d.tenant_id='{tenant}' AND d.source_event_id='{source}';
        COMMIT;""")


def await_failure(psql):
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline:
        rows = json.loads(psql(f"""SELECT coalesce(jsonb_agg(jsonb_build_object(
          'status',d.status,'error',d.last_error,'generation',d.generation,
          'attempts',d.attempt_count,'outbox',e.status,'outbox_attempts',e.attempts)), '[]')
          FROM app.notification_deliveries d JOIN app.outbox_events e
            ON (e.tenant_id,e.id)=(d.tenant_id,d.dispatch_event_id) WHERE d.{SOURCE_FILTER};"""))
        if len(rows) == 2 and all(row == dict(status='dead_letter', error='NOTIFICATION_SEND_FAILED',
                generation=1, attempts=1, outbox='dead_letter', outbox_attempts=10) for row in rows):
            return {'status': 'PASS', 'count': 2, 'precondition': 'final outbox attempt 9 → 10', 'facts': rows}
        time.sleep(.2)  # bounded positive-state readiness, not concurrent ordering
    raise RuntimeError('Real refusing-port notification failure/DLQ witness timed out')


WITNESS_SQL = f"""
SELECT jsonb_agg(jsonb_build_object('project',CASE right(d.source_event_id::text,1) WHEN '1' THEN 'desktop' ELSE 'mobile' END,
 'status',d.status,'generation',d.generation,'attempts',d.attempt_count,'sent',d.sent_at IS NOT NULL,
 'requests',(SELECT count(*) FROM app.notification_redrive_requests r WHERE (r.tenant_id,r.notification_id)=(d.tenant_id,d.id)),
 'old_dlq',(SELECT count(*) FROM app.outbox_events e WHERE e.tenant_id=d.tenant_id AND e.delivery_target='notifications.email' AND e.payload->>'notification_id'=d.id::text AND e.status='dead_letter' AND e.attempts=10),
 'delivered',(SELECT count(*) FROM app.outbox_events e WHERE e.tenant_id=d.tenant_id AND e.delivery_target='notifications.email' AND e.payload->>'notification_id'=d.id::text AND e.status='delivered'),
 'dispatches',(SELECT count(*) FROM app.outbox_events e WHERE e.tenant_id=d.tenant_id AND e.delivery_target='notifications.email' AND e.payload->>'notification_id'=d.id::text),
 'current_delivered',e.status='delivered' AND e.attempts=1 AND e.id=r.dispatch_event_id AND r.generation=d.generation))
FROM app.notification_deliveries d JOIN app.outbox_events e ON (e.tenant_id,e.id)=(d.tenant_id,d.dispatch_event_id)
LEFT JOIN app.notification_redrive_requests r ON (r.tenant_id,r.notification_id)=(d.tenant_id,d.id)
WHERE d.{SOURCE_FILTER};
"""


def verify(rows):
    if (not isinstance(rows, list) or len(rows) != 2 or any(not isinstance(r, dict) for r in rows)
            or {r.get('project') for r in rows} != {'desktop', 'mobile'}):
        raise RuntimeError('Notification recovery project inventory differs')
    expected = dict(status='sent', generation=2, attempts=2, sent=True,
                    requests=1, old_dlq=1, delivered=1, dispatches=2, current_delivered=True)
    if any(any(row.get(key) != value for key, value in expected.items()) for row in rows):
        raise RuntimeError('Notification redrive/DLQ/outbox delivery witness differs')
    return {'status': 'PASS', 'projects': rows}


def mailpit_witness():
    """After worker shutdown, require one actual MIME delivery per frozen reference."""
    def get(path):
        with urllib.request.urlopen('http://127.0.0.1:8025/api/v1/' + path, timeout=5) as response:
            return json.loads(response.read(1024 * 1024))
    counts = {'desktop': 0, 'mobile': 0}
    for item in get('messages?limit=100')['messages']:
        if item['Subject'] != 'Order shipped':
            continue
        # Mailpit IDs are untrusted response data, never an arbitrary URL/path.
        import re
        if not re.fullmatch(r'[A-Za-z0-9-]{1,128}', item['ID']):
            raise RuntimeError('Invalid local Mailpit message identity')
        body = get('message/' + item['ID'])['Text']
        for n, project in enumerate(('desktop', 'mobile'), 1):
            if f'c1900003-0000-4000-8000-{n:012d}' in body:
                counts[project] += 1
    if counts != {'desktop': 1, 'mobile': 1}:
        raise RuntimeError('Real redrive delivered duplicate or missing MIME messages: ' + json.dumps(counts))
    return {'status': 'PASS', 'counts': counts}
