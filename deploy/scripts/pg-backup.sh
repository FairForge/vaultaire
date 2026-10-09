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
#   vaultaire_backup_run_last_success_timestamp_seconds     the WHOLE run finished: configs
#                                                           tar (the only backup of .env),
#                                                           rules tar + its upload, retention
# Rules: deploy/monitoring/vaultaire-backup.yml (BackupStale / BackupOffboxStale /
# BackupRunIncomplete, > 26 h or absent).
#
# Every failure is ONE log line `BACKUP FAILED - stage=<stage>: <what>` — the explicit
# fail() calls, and (ERR trap, set in main) any command in main that set -e would otherwise
# end the run on silently (an `rclone size` / `rclone lsf` that failed used to exit with
# no line at all). The trap is not inherited by functions (no `set -E`: it would also fire
# inside command substitutions and log twice), so every function checks its own commands.
set -euo pipefail
umask 077

ROOT=${PG_BACKUP_ROOT:-/opt/vaultaire}
BACKUP_DIR=$ROOT/backups
LOG=$ROOT/logs/backup.log
ENV_FILE=$ROOT/configs/.env
OFFBOX_ENV=$ROOT/configs/sync-backup.env   # RCLONE_CONFIG_SYNCBK_* (user1 0600)
PROM_DIR=${PG_BACKUP_PROM_DIR:-/etc/prometheus}
# user1's own directory; node_exporter reads it through root-owned symlinks in its textfile
# directory (deploy/monitoring/README.md "Backup metrics") — the app user never writes there.
METRICS_DIR=${PG_BACKUP_METRICS_DIR:-/opt/vaultaire/metrics}
OFFBOX_REMOTE=syncbk:_ops/backups
OFFBOX_DIR="$OFFBOX_REMOTE/$(date -u +%Y/%m)"
OFFBOX_KEEP_DAYS=30
TIMESTAMP=$(date +%Y%m%d_%H%M%S)

STAGE=start

fail() {
  echo "$(date): BACKUP FAILED - stage=$STAGE: $1" >> "$LOG"
  exit 1
}

# on_err — the ERR trap: a command failed outside an explicit check. Logged with the stage
# (and the command's text, never its expanded values), then the run ends like fail().
on_err() {
  local rc=$1 line=$2 cmd=$3
  echo "$(date): BACKUP FAILED - stage=$STAGE: command failed (exit $rc, line $line): $cmd" >> "$LOG"
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
# Every step is checked here: the ERR trap is not inherited by functions, so a printf
# or chmod failing under set -e (a full disk) used to end the run with no line at all.
write_metric() {
  local name=$1 help=$2 tmp
  tmp=$(mktemp "$METRICS_DIR/.$name.XXXXXX" 2>>"$LOG") || { metric_failed "cannot write to $METRICS_DIR"; return 0; }
  printf '# HELP %s %s\n# TYPE %s gauge\n%s %s\n' "$name" "$help" "$name" "$name" "$(date +%s)" > "$tmp" || { rm -f "$tmp"; metric_failed "cannot write $name"; return 0; }
  chmod 0644 "$tmp" || { rm -f "$tmp"; metric_failed "cannot chmod $name"; return 0; }
  mv -f "$tmp" "$METRICS_DIR/$name.prom" || { rm -f "$tmp"; metric_failed "cannot publish $name"; return 0; }
}

# metric_failed WHAT — the metrics line of a failed write_metric, with the stage; never
# fatal itself (on a full disk the log line may not land either).
metric_failed() {
  echo "$(date): METRICS: $1 (non-fatal, stage=$STAGE)" >> "$LOG" || true
}

# offbox_copy FILE — uploads BACKUP_DIR/FILE to OFFBOX_DIR/FILE and verifies the size
# Sync reports (never its modtime: the bridge reports 1970 for every file).
offbox_copy() {
  local f=$1 remote local_size
  rclone copyto --retries 3 --low-level-retries 5 --timeout 10m "$BACKUP_DIR/$f" "$OFFBOX_DIR/$f" 2>>"$LOG" || fail "OFFBOX: upload of $f failed"
  remote=$(rclone size --json "$OFFBOX_DIR/$f" 2>>"$LOG" | sed -n 's/.*"bytes":\([0-9]*\).*/\1/p') || fail "OFFBOX: rclone size of $f failed"
  local_size=$(stat -c%s "$BACKUP_DIR/$f") || fail "cannot stat $BACKUP_DIR/$f"
  [ "$remote" = "$local_size" ] || fail "OFFBOX: $f size mismatch on Sync (local $local_size, remote ${remote:-none})"
}

main() {
  trap 'on_err $? $LINENO "$BASH_COMMAND"' ERR

  # Read DB password from .env (root:user1 640 — group-readable by design; if this ever
  # regresses to root:root 600 we must fail loudly, not write empty dumps while logging
  # success like before 2026-07-09).
  STAGE=password
  PGPASSWORD=$(db_password_from_env "$ENV_FILE") || fail "cannot read DB_PASSWORD from $ENV_FILE"
  [ -n "$PGPASSWORD" ] || fail "DB_PASSWORD empty"
  export PGPASSWORD

  # Database backup — -w: never prompt; fail instead.
  STAGE=dump
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
  STAGE=offbox
  [ -r "$OFFBOX_ENV" ] || fail "OFFBOX: $OFFBOX_ENV missing"
  set -a
  # shellcheck source=/dev/null
  . "$OFFBOX_ENV"
  set +a
  offbox_copy "$DUMP"
  write_metric vaultaire_backup_offbox_last_success_timestamp_seconds "unix time the last pg_dump was uploaded to Sync and its size verified (pg-backup.sh)"
  echo "$(date): Off-box copy completed - $DUMP → Sync ($OFFBOX_DIR, $SIZE bytes verified)" >> "$LOG"

  # Config backup (.env included — do NOT silence errors here). Stays on the box.
  STAGE=configs
  tar czf "$BACKUP_DIR/configs_$TIMESTAMP.tar.gz" -C "$ROOT" configs/ || fail "config tar failed"

  # Prometheus backup, on the box and off it: prometheus.yml and the alert rules — what
  # this repo cannot rebuild by itself. Not the rest of /etc/prometheus (alertmanager.yml
  # carries the receivers' URLs; consoles and templates are the package's).
  STAGE=rules
  tar czf "$BACKUP_DIR/prometheus_$TIMESTAMP.tar.gz" -C "$PROM_DIR" prometheus.yml rules || fail "prometheus tar failed"
  offbox_copy "prometheus_$TIMESTAMP.tar.gz"

  # Keep only last 7 days on the box
  STAGE=retention
  find "$BACKUP_DIR" -name "vaultaire_*.sql.gz" -mtime +7 -delete
  find "$BACKUP_DIR" -name "configs_*.tar.gz" -mtime +7 -delete
  find "$BACKUP_DIR" -name "prometheus_*.tar.gz" -mtime +7 -delete

  # Off-box retention by the DATE IN THE FILE NAME — never by the backend's modtime: the
  # Sync bridge reports every file as modified in January 1970 (a `--min-age` sweep deleted
  # fresh backups, 2026-10-07). Files are named *_YYYYMMDD_HHMMSS.*; older than
  # OFFBOX_KEEP_DAYS → deleted.
  # A listing that fails, or an old file that cannot be deleted, fails the run (no run
  # timestamp): the remote would fill up silently otherwise. Every deletion is tried first.
  CUTOFF=$(date -u -d "-$OFFBOX_KEEP_DAYS days" +%Y%m%d)
  local listing rel day undeleted=0
  listing=$(rclone lsf -R --files-only "$OFFBOX_REMOTE" 2>>"$LOG") || fail "OFFBOX: listing $OFFBOX_REMOTE failed"
  while read -r rel; do
    [ -n "$rel" ] || continue
    day=$(basename "$rel" | sed -nE 's/^[a-z]+_([0-9]{8})_[0-9]{6}\..*/\1/p')
    if [ -n "$day" ] && [ "$day" -lt "$CUTOFF" ]; then
      if ! rclone deletefile "$OFFBOX_REMOTE/$rel" 2>>"$LOG"; then
        echo "$(date): OFFBOX: could not delete old $rel" >> "$LOG"
        undeleted=$((undeleted + 1))
      fi
    fi
  done <<< "$listing"
  [ "$undeleted" = 0 ] || fail "OFFBOX: $undeleted old file(s) could not be deleted"

  STAGE=finish
  echo "$(date): Run completed - configs + rules archived, rules → Sync, retention applied" >> "$LOG"
  # LAST: everything above succeeded.
  write_metric vaultaire_backup_run_last_success_timestamp_seconds "unix time pg-backup.sh last finished a whole run: dump, off-box copy, configs + rules tars, rules upload, retention"
}

# Sourced by pg-backup_test.sh for its functions; run as a script it backs up.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
