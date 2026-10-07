#!/bin/bash
# pg-backup.sh — cron 03:00 UTC (user1): PostgreSQL dump + config and Prometheus-rule
# tarballs into /opt/vaultaire/backups (7 days), then an OFF-BOX copy of the dump and the
# rules to Sync.com (our own E2E-encrypted account, through the local sync-webdav bridge
# on :4918; 30 days kept there). WP-R9-7. Every file is private (umask 077).
set -euo pipefail
umask 077

BACKUP_DIR=/opt/vaultaire/backups
LOG=/opt/vaultaire/logs/backup.log
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
OFFBOX_ENV=/opt/vaultaire/configs/sync-backup.env   # RCLONE_CONFIG_SYNCBK_* (user1 0600)
OFFBOX_DIR="syncbk:_ops/backups/$(date +%Y/%m)"
OFFBOX_KEEP_DAYS=30

fail() {
  echo "$(date): BACKUP FAILED - $1" >> "$LOG"
  exit 1
}

# Read DB password from .env (root:user1 640 — group-readable by design;
# if this ever regresses to root:root 600 we must fail loudly, not write
# empty dumps while logging success like before 2026-07-09).
PGPASSWORD=$(grep '^DB_PASSWORD' /opt/vaultaire/configs/.env | cut -d= -f2) || fail "cannot read DB_PASSWORD from .env"
[ -n "$PGPASSWORD" ] || fail "DB_PASSWORD empty"
export PGPASSWORD

# Database backup — -w: never prompt; fail instead.
pg_dump -w -h 127.0.0.1 -U vaultaire -d vaultaire | gzip > "$BACKUP_DIR/vaultaire_$TIMESTAMP.sql.gz" || fail "pg_dump failed"
unset PGPASSWORD

# Size assert: a failed dump gzips to ~20 bytes; a real one is >1KB.
SIZE=$(stat -c%s "$BACKUP_DIR/vaultaire_$TIMESTAMP.sql.gz")
[ "$SIZE" -gt 1024 ] || fail "dump too small ($SIZE bytes): vaultaire_$TIMESTAMP.sql.gz"

# Content assert: the dump must contain real DDL.
DDL_COUNT=$(gzip -dc "$BACKUP_DIR/vaultaire_$TIMESTAMP.sql.gz" | grep -c 'CREATE TABLE') || true
[ "${DDL_COUNT:-0}" -gt 0 ] || fail "dump contains no CREATE TABLE statements"

# Config backup (.env included — do NOT silence errors here). Stays on the box.
tar czf "$BACKUP_DIR/configs_$TIMESTAMP.tar.gz" -C /opt/vaultaire configs/ || fail "config tar failed"

# Prometheus rules backup
tar czf "$BACKUP_DIR/prometheus_$TIMESTAMP.tar.gz" -C /etc/prometheus . || fail "prometheus tar failed"

# Keep only last 7 days on the box
find "$BACKUP_DIR" -name "vaultaire_*.sql.gz" -mtime +7 -delete
find "$BACKUP_DIR" -name "configs_*.tar.gz" -mtime +7 -delete
find "$BACKUP_DIR" -name "prometheus_*.tar.gz" -mtime +7 -delete

echo "$(date): Backup completed - vaultaire_$TIMESTAMP (${SIZE} bytes, ${DDL_COUNT} tables)" >> "$LOG"

# Off-box copy (Sync). A failure here is a failed backup run: the local copy exists,
# but a disk loss would take it with the database.
[ -r "$OFFBOX_ENV" ] || fail "OFFBOX: $OFFBOX_ENV missing"
# shellcheck source=/dev/null
set -a; . "$OFFBOX_ENV"; set +a
for f in "vaultaire_$TIMESTAMP.sql.gz" "prometheus_$TIMESTAMP.tar.gz"; do
  rclone copyto --retries 3 --low-level-retries 5 --timeout 10m "$BACKUP_DIR/$f" "$OFFBOX_DIR/$f" 2>>"$LOG" || fail "OFFBOX: upload of $f failed"
  REMOTE=$(rclone size --json "$OFFBOX_DIR/$f" 2>>"$LOG" | sed -n 's/.*"bytes":\([0-9]*\).*/\1/p')
  [ "$REMOTE" = "$(stat -c%s "$BACKUP_DIR/$f")" ] || fail "OFFBOX: $f size mismatch on Sync (local $(stat -c%s "$BACKUP_DIR/$f"), remote ${REMOTE:-none})"
done
# Retention by the DATE IN THE FILE NAME — never by the backend's modtime: the Sync bridge
# reports every file as modified in January 1970 (a `--min-age` sweep deleted fresh backups,
# 2026-10-07). Files are named *_YYYYMMDD_HHMMSS.*; older than OFFBOX_KEEP_DAYS → deleted.
CUTOFF=$(date -u -d "-$OFFBOX_KEEP_DAYS days" +%Y%m%d)
rclone lsf -R --files-only syncbk:_ops/backups 2>>"$LOG" | while read -r rel; do
  day=$(basename "$rel" | sed -nE 's/^[a-z]+_([0-9]{8})_[0-9]{6}\..*/\1/p')
  if [ -n "$day" ] && [ "$day" -lt "$CUTOFF" ]; then
    rclone deletefile "syncbk:_ops/backups/$rel" 2>>"$LOG" || echo "$(date): OFFBOX: could not delete old $rel (non-fatal)" >> "$LOG"
  fi
done
echo "$(date): Off-box copy completed - vaultaire_$TIMESTAMP + rules → Sync ($OFFBOX_DIR)" >> "$LOG"
