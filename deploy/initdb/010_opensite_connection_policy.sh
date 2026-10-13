#!/bin/sh
# Fresh Compose cluster connection policy; depends on initdb's PGDATA/HBA file.
# Docker entrypoint executes this file once, before application LOGIN provisioning.
# Restrict seven LOGINs to the exact application database; preserve admin rules.
set -eu
LC_ALL=C
export LC_ALL

opensite_database=${POSTGRES_DB-}
case "$opensite_database" in
    ''|[!a-z]*|*[!a-z0-9_]*)
        printf '%s\n' 'INVALID_DATABASE_NAME' >&2
        exit 1
        ;;
esac
if [ "${#opensite_database}" -gt 63 ]; then
    printf '%s\n' 'INVALID_DATABASE_NAME' >&2
    exit 1
fi

opensite_hba_file=${PGDATA-}/pg_hba.conf
if [ -z "${PGDATA-}" ] || [ ! -f "$opensite_hba_file" ] || [ -L "$opensite_hba_file" ]; then
    printf '%s\n' 'MISSING_HBA_FILE' >&2
    exit 1
fi
opensite_logins=opensite_runtime_login,opensite_auth_login,opensite_domain_login,opensite_media_login,opensite_queue_login,opensite_outbox_login,opensite_customer_auth_login
opensite_hba_tmp=$(mktemp "${PGDATA}/pg_hba.conf.opensite.XXXXXX")
trap 'rm -f "$opensite_hba_tmp"' 0
trap 'exit 1' HUP INT TERM

{
    printf '%s\n' '# OpenSite application LOGINs: exact database only; administrator rules follow.'
    # Quotes prevent names such as "all" or "replication" becoming HBA keywords.
    printf 'local "%s" %s scram-sha-256\n' "$opensite_database" "$opensite_logins"
    printf 'host "%s" %s 0.0.0.0/0 scram-sha-256\n' "$opensite_database" "$opensite_logins"
    printf 'host "%s" %s ::/0 scram-sha-256\n' "$opensite_database" "$opensite_logins"
    printf 'local all %s reject\n' "$opensite_logins"
    printf 'host all %s 0.0.0.0/0 reject\n' "$opensite_logins"
    printf 'host all %s ::/0 reject\n' "$opensite_logins"
    # Physical replication has no database and does not match the "all" records.
    printf 'local replication %s reject\n' "$opensite_logins"
    printf 'host replication %s 0.0.0.0/0 reject\n' "$opensite_logins"
    printf 'host replication %s ::/0 reject\n' "$opensite_logins"
    cat "$opensite_hba_file"
} > "$opensite_hba_tmp"
chmod 600 "$opensite_hba_tmp"
mv "$opensite_hba_tmp" "$opensite_hba_file"
