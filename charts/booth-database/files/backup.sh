#!/bin/sh
# Nightly backup of every workspace database on the bundled server (ADR 0054's v0 baseline,
# applied to this module's own server): server-wide roles/grants plus one pg_dump custom-format
# archive per workspace database, into a timestamped directory, pruned after RETENTION_DAYS.
# Restore runbook: README.md "Backup and restore".
#
# Env: PGHOST PGUSER PGPASSWORD (the bundled admin role), BACKUP_DIR, RETENTION_DAYS.
set -eu

: "${BACKUP_DIR:?}" "${RETENTION_DAYS:?}"
stamp=$(date -u +%Y%m%dT%H%M%SZ)
work="$BACKUP_DIR/.partial-$stamp"
mkdir -p "$work"

# Roles and database-level grants live outside any one database. --no-role-passwords: the only
# passwords on this server are the admin's (in its own Secret) and short-lived lease verifiers,
# neither of which belongs in a backup.
pg_dumpall --globals-only --no-role-passwords -l postgres > "$work/globals.sql"

# Workspace databases only (internal/naming.DatabasePrefix). `_` is a LIKE wildcard, hence \_.
count=0
for db in $(psql -d postgres -AtX -c "SELECT datname FROM pg_database WHERE datname LIKE 'bdb\_ws\_%' ORDER BY 1"); do
  pg_dump -d "$db" -Fc -f "$work/$db.dump"
  count=$((count + 1))
done

# Only a complete run becomes a visible backup directory.
mv "$work" "$BACKUP_DIR/$stamp"
echo "backup $stamp: $count workspace database(s)"

# Retention: complete backups older than RETENTION_DAYS, and any partial run older than a day.
find "$BACKUP_DIR" -mindepth 1 -maxdepth 1 -type d -name '2*' -mtime +"$RETENTION_DAYS" -exec rm -rf {} +
find "$BACKUP_DIR" -mindepth 1 -maxdepth 1 -type d -name '.partial-*' -mtime +1 -exec rm -rf {} +
