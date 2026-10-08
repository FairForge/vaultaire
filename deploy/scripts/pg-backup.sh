#!/bin/bash
# pg-backup.sh — cron 03:00 UTC (user1): PostgreSQL dump into /opt/vaultaire/backups
# (7 days) and an OFF-BOX copy of it to Sync.com (our own E2E-encrypted account, through
# the local sync-webdav bridge on :4918; 30 days kept there), THEN the config and
# Prometheus-rule tarballs (configs stay on the box, the rules go off-box too). WP-R9-7.
# Every file is private (umask 077).
#
# Order matters: the dump is the prod database's only off-box copy, so it is uploaded
# and size-verified right after its own asserts. The tars come after — from 2026-10-05
# to -07 a root-only file in configs/ made `tar` fail and that aborted the upload of a
# dump that was fine.
#
# Success is reported where Prometheus reads it (node_exporter's textfile collector):
#   vaultaire_backup_last_success_timestamp_seconds         the dump passed its asserts
#   vaultaire_backup_offbox_last_success_timestamp_seconds  the dump is on Sync, size verified
# Rules: deploy/monitoring/vaultaire-backup.yml (BackupStale / BackupOffboxStale, > 26 h).
set -euo pipefail
umask 077

ROOT=${PG_BACKUP_ROOT:-/opt/vaultaire}
BACKUP_DIR=$ROOT/backups
LOG=$ROOT/logs/backup.log
ENV_FILE=$ROOT/configs/.env
OFFBOX_ENV=$ROOT/configs/sync-backup.env   # RCLONE_CONFIG_SYNCBK_* (user1 0600)
PROM_DIR=${PG_BACKUP_PROM_DIR:-/etc/prometheus}
METRICS_DIR=${PG_BACKUP_METRICS_DIR:-/var/lib/prometheus/node-exporter}   # textfile collector
OFFBOX_REMOTE=syncbk:_ops/backups
OFFBOX_DIR="$OFFBOX_REMOTE/$(date -u +%Y/%m)"
OFFBOX_KEEP_DAYS=30
TIMESTAMP=$(date +%Y%m%d_%H%M%S)

fail() {
  echo "$(date): BACKUP FAILED - $1" >> "$LOG"
  exit 1
}

# db_password_from_env FILE — prints the value of the first DB_PASSWORD= line of an
# .env file, with ONE level of surrounding quotes removed (what systemd's EnvironmentFile
# does). Everything after the first `=` is the value: a password may contain `=`.
# Returns 1 when the file has no such line; prints nothing for an empty value.
db_password_from_env() {
  local line value
  line=$(grep -m1 '^DB_PASSWORD=' "$1") || return 1
  value=${line#DB_PASSWORD=}
  case "$value" in
    \"*\") value=${value#\"}; value=${value%\"} ;;
    \'*\') value=${value#\'}; value=${value%\'} ;;
  esac
  printf '%s' "$value"
}

# write_metric NAME HELP — writes `NAME <now>` to METRICS_DIR/NAME.prom (atomic rename,
# world-readable: node_exporter runs as its own user). Non-fatal: a backup that landed
# is a success whatever the metrics directory says; the absence then raises the alert.
write_metric() {
  local name=$1 help=$2 tmp
  tmp=$(mktemp "$METRICS_DIR/.$name.XXXXXX" 2>>"$LOG") || { echo "$(date): METRICS: cannot write to $METRICS_DIR (non-fatal)" >> "$LOG"; return 0; }
  printf '# HELP %s %s\n# TYPE %s gauge\n%s %s\n' "$name" "$help" "$name" "$name" "$(date +%s)" > "$tmp"
  chmod 0644 "$tmp"
  mv -f "$tmp" "$METRICS_DIR/$name.prom" || echo "$(date): METRICS: cannot publish $name (non-fatal)" >> "$LOG"
}

# offbox_copy FILE — uploads BACKUP_DIR/FILE to OFFBOX_DIR/FILE and verifies the size
# Sync reports (never its modtime: the bridge reports 1970 for every file).
offbox_copy() {
  local f=$1 remote local_size
  rclone copyto --retries 3 --low-level-retries 5 --timeout 10m "$BACKUP_DIR/$f" "$OFFBOX_DIR/$f" 2>>"$LOG" || fail "OFFBOX: upload of $f failed"
  remote=$(rclone size --json "$OFFBOX_DIR/$f" 2>>"$LOG" | sed -n 's/.*"bytes":\([0-9]*\).*/\1/p')
  local_size=$(stat -c%s "$BACKUP_DIR/$f")
  [ "$remote" = "$local_size" ] || fail "OFFBOX: $f size mismatch on Sync (local $local_size, remote ${remote:-none})"
}

main() {
  # Read DB password from .env (root:user1 640 — group-readable by design; if this ever
  # regresses to root:root 600 we must fail loudly, not write empty dumps while logging
  # success like before 2026-07-09).
  PGPASSWORD=$(db_password_from_env "$ENV_FILE") || fail "cannot read DB_PASSWORD from $ENV_FILE"
  [ -n "$PGPASSWORD" ] || fail "DB_PASSWORD empty"
  export PGPASSWORD

  # Database backup — -w: never prompt; fail instead.
  DUMP="vaultaire_$TIMESTAMP.sql.gz"
  pg_dump -w -h 127.0.0.1 -U vaultaire -d vaultaire | gzip > "$BACKUP_DIR/$DUMP" || fail "pg_dump failed"
  unset PGPASSWORD

  # Size assert: a failed dump gzips to ~20 bytes; a real one is >1KB.
  SIZE=$(stat -c%s "$BACKUP_DIR/$DUMP")
  [ "$SIZE" -gt 1024 ] || fail "dump too small ($SIZE bytes): $DUMP"

  # Content assert: the dump must contain real DDL.
  DDL_COUNT=$(gzip -dc "$BACKUP_DIR/$DUMP" | grep -c 'CREATE TABLE') || true
  [ "${DDL_COUNT:-0}" -gt 0 ] || fail "dump contains no CREATE TABLE statements"

  write_metric vaultaire_backup_last_success_timestamp_seconds "unix time the last pg_dump passed its size and DDL asserts (pg-backup.sh)"
  echo "$(date): Backup completed - vaultaire_$TIMESTAMP (${SIZE} bytes, ${DDL_COUNT} tables)" >> "$LOG"

  # Off-box copy of the dump FIRST (Sync). A failure here is a failed backup run: the
  # local copy exists, but a disk loss would take it with the database.
  [ -r "$OFFBOX_ENV" ] || fail "OFFBOX: $OFFBOX_ENV missing"
  set -a
  # shellcheck source=/dev/null
  . "$OFFBOX_ENV"
  set +a
  offbox_copy "$DUMP"
  write_metric vaultaire_backup_offbox_last_success_timestamp_seconds "unix time the last pg_dump was uploaded to Sync and its size verified (pg-backup.sh)"
  echo "$(date): Off-box copy completed - $DUMP → Sync ($OFFBOX_DIR, $SIZE bytes verified)" >> "$LOG"

  # Config backup (.env included — do NOT silence errors here). Stays on the box.
  tar czf "$BACKUP_DIR/configs_$TIMESTAMP.tar.gz" -C "$ROOT" configs/ || fail "config tar failed"

  # Prometheus rules backup, on the box and off it.
  tar czf "$BACKUP_DIR/prometheus_$TIMESTAMP.tar.gz" -C "$PROM_DIR" . || fail "prometheus tar failed"
  offbox_copy "prometheus_$TIMESTAMP.tar.gz"

  # Keep only last 7 days on the box
  find "$BACKUP_DIR" -name "vaultaire_*.sql.gz" -mtime +7 -delete
  find "$BACKUP_DIR" -name "configs_*.tar.gz" -mtime +7 -delete
  find "$BACKUP_DIR" -name "prometheus_*.tar.gz" -mtime +7 -delete

  # Off-box retention by the DATE IN THE FILE NAME — never by the backend's modtime: the
  # Sync bridge reports every file as modified in January 1970 (a `--min-age` sweep deleted
  # fresh backups, 2026-10-07). Files are named *_YYYYMMDD_HHMMSS.*; older than
  # OFFBOX_KEEP_DAYS → deleted.
  CUTOFF=$(date -u -d "-$OFFBOX_KEEP_DAYS days" +%Y%m%d)
  rclone lsf -R --files-only "$OFFBOX_REMOTE" 2>>"$LOG" | while read -r rel; do
    day=$(basename "$rel" | sed -nE 's/^[a-z]+_([0-9]{8})_[0-9]{6}\..*/\1/p')
    if [ -n "$day" ] && [ "$day" -lt "$CUTOFF" ]; then
      rclone deletefile "$OFFBOX_REMOTE/$rel" 2>>"$LOG" || echo "$(date): OFFBOX: could not delete old $rel (non-fatal)" >> "$LOG"
    fi
  done
  echo "$(date): Run completed - configs + rules archived, rules → Sync, retention applied" >> "$LOG"
}

# Sourced by pg-backup_test.sh for its functions; run as a script it backs up.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
