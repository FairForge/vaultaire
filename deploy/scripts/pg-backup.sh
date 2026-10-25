#!/bin/bash
# pg-backup.sh — cron 03:00 UTC (user1): PostgreSQL dump into /opt/vaultaire/backups
# (7 days) and an OFF-BOX copy of it to Sync.com (our own E2E-encrypted account, through
# the local sync-webdav bridge on :4918; 30 days kept there), THEN the config and
# Prometheus-rule tarballs (configs stay on the box, the rules go off-box too). WP-R9-7.
# A SECOND off-box copy goes to Blomp (OpenStack Swift, 2026-10-21) through an rclone
# crypt remote: Blomp's storage URL is plain HTTP, so the files are encrypted (names
# too) before they leave the box. That leg is optional and never fails the run: Sync
# stays the copy the run stands or falls by; Blomp has its own timestamp and alert.
# Every file is private (umask 077).
#
# The dump is ENCRYPTED BEFORE IT EXISTS ON DISK (WP-P42, plan 43.7): `pg_dump | gzip |
# age -R $BACKUP_AGE_RECIPIENTS` → vaultaire_<ts>.sql.gz.age, its size and DDL asserts
# counted on the stream — no plaintext byte of it is written. The recipients file holds the
# owner's OFFLINE public keys only (one `age1…` per line, comments allowed); the identities
# are never on the box, so a restore happens wherever one is (docs/DEPLOY.md § Backups).
# No file = the plaintext .sql.gz of before, a WARNING line and the gauge at 0 (alert
# BackupDumpPlaintext); a file that cannot be used = BACKUP FAILED stage=encrypt, never a
# silent plaintext.
#
# Order matters: the dump is the prod database's only off-box copy, so it is uploaded
# and size-verified right after its own asserts. The tars come after — from 2026-10-05
# to -07 a root-only file in configs/ made `tar` fail and that aborted the upload of a
# dump that was fine.
#
# Success is reported where Prometheus reads it (node_exporter's textfile collector):
#   vaultaire_backup_last_success_timestamp_seconds         the dump passed its asserts
#   vaultaire_backup_offbox_last_success_timestamp_seconds  the dump is on Sync, size verified
#   vaultaire_backup_offbox2_last_success_timestamp_seconds the dump is on Blomp (encrypted),
#                                                           size verified
#   vaultaire_backup_run_last_success_timestamp_seconds     the WHOLE run finished: configs
#                                                           tar (the only backup of .env),
#                                                           rules tar + its upload, retention
#   vaultaire_backup_offbox{,2}_retention_undeleted_files   old files the Sync / Blomp
#                                                           retention pass could not delete
#                                                           (a gauge, written by every pass
#                                                           that listed the leg; RP54-6)
#   vaultaire_backup_dump_encrypted                         1 = the last dump that passed its
#                                                           asserts was written with age, 0 =
#                                                           in plaintext (no recipients file)
#   vaultaire_backup_offbox{,2}_retention_last_run_timestamp_seconds
#                                                           the pass that wrote that gauge: a
#                                                           listing that fails writes neither, so
#                                                           an old count is never read as fresh
#                                                           (RP55-1)
# Rules: deploy/monitoring/vaultaire-backup.yml (BackupStale / BackupOffboxStale /
# BackupRunIncomplete, > 26 h or absent; BackupSecondOffboxStale for the Blomp leg;
# BackupOffboxRetentionStuck when either leg keeps a file past its 30 days;
# BackupOffboxRetentionNotRunning when either leg's retention has not listed it in 26 h;
# BackupDumpPlaintext while the dumps are written without age).
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
# The second off-box leg (Blomp): an rclone crypt remote `blompcrypt` over the swift remote
# `blompbk`. The crypt passwords live in OFFBOX2_ENV (user1 0600, rclone-obscured; the plain
# ones are in the owner's Keychain — a restore needs them); the Blomp login is read from
# .env (BLOMP_USER / BLOMP_PASSWORD / BLOMP_CONTAINER, the driver's own), so a password
# rotation is one edit. No OFFBOX2_ENV = the leg is not set up on this box: skipped.
OFFBOX2_ENV=$ROOT/configs/blomp-backup.env
OFFBOX2_REMOTE=blompcrypt:
OFFBOX2_DIR="${OFFBOX2_REMOTE}$(date -u +%Y/%m)"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
# The owner's offline age recipients (public keys only, root 0644; WP-P42). Absent = plaintext.
BACKUP_AGE_RECIPIENTS=$ROOT/configs/backup-age-recipients.txt
AGE=${PG_BACKUP_AGE_BIN:-age}

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
  env_value_from "$1" DB_PASSWORD
}

# env_value_from FILE KEY — db_password_from_env for any KEY (the same parsing rules).
env_value_from() {
  local line value
  line=$(grep -m1 "^$2=" "$1") || return 1
  value=${line#"$2"=}
  case "$value" in
    \"*\") value=${value#\"}; value=${value%\"} ;;
    \'*\') value=${value#\'}; value=${value%\'} ;;
  esac
  printf '%s' "$value"
}

# write_metric NAME HELP [VALUE] — writes `NAME <VALUE>` (default: now, unix seconds) to
# METRICS_DIR/NAME.prom (atomic rename, world-readable: node_exporter runs as its own user). Non-fatal: a backup that landed
# is a success whatever the metrics directory says; the absence then raises the alert.
# Every step is checked here: the ERR trap is not inherited by functions, so a printf
# or chmod failing under set -e (a full disk) used to end the run with no line at all.
write_metric() {
  local name=$1 help=$2 value=${3:-} tmp
  [ -n "$value" ] || value=$(date +%s)
  tmp=$(mktemp "$METRICS_DIR/.$name.XXXXXX" 2>>"$LOG") || { metric_failed "cannot write to $METRICS_DIR"; return 0; }
  printf '# HELP %s %s\n# TYPE %s gauge\n%s %s\n' "$name" "$help" "$name" "$name" "$value" > "$tmp" || { rm -f "$tmp"; metric_failed "cannot write $name"; return 0; }
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

# offbox2_setup — exports the Blomp login from .env for the `blompbk` remote and loads
# the crypt remote from OFFBOX2_ENV. Returns 1 (logged) when the leg cannot run; never
# fatal. The password is handed to rclone through its environment, never an argument.
offbox2_setup() {
  local user pass container
  [ -r "$OFFBOX2_ENV" ] || { echo "$(date): OFFBOX2: $OFFBOX2_ENV not present, Blomp copy skipped" >> "$LOG"; return 1; }
  user=$(env_value_from "$ENV_FILE" BLOMP_USER) || user=
  [ -n "$user" ] || { offbox2_failed "BLOMP_USER not set in $ENV_FILE"; return 1; }
  pass=$(env_value_from "$ENV_FILE" BLOMP_PASSWORD) || pass=
  [ -n "$pass" ] || { offbox2_failed "BLOMP_PASSWORD not set in $ENV_FILE"; return 1; }
  container=$(env_value_from "$ENV_FILE" BLOMP_CONTAINER) || container=
  set -a
  # shellcheck source=/dev/null
  . "$OFFBOX2_ENV" || { set +a; offbox2_failed "cannot read $OFFBOX2_ENV"; return 1; }
  set +a
  export RCLONE_CONFIG_BLOMPBK_USER=$user RCLONE_CONFIG_BLOMPBK_KEY=$pass
  export RCLONE_CONFIG_BLOMPCRYPT_REMOTE="blompbk:${container:-$user}/_ops/backups"
}

# offbox2_failed WHAT — the one log line of a failed Blomp step; never fatal.
offbox2_failed() {
  echo "$(date): OFFBOX2 FAILED - stage=$STAGE: $1 (non-fatal: Sync holds the off-box copy)" >> "$LOG" || true
}

# offbox2_copy FILE — offbox_copy for the Blomp leg: upload through the crypt remote and
# verify the (decrypted) size it reports. Returns 1 (logged) on any failure, never exits:
# it runs as an `if` condition, where set -e is off, so every command is checked here.
offbox2_copy() {
  local f=$1 remote local_size
  rclone copyto --retries 3 --low-level-retries 5 --timeout 10m "$BACKUP_DIR/$f" "$OFFBOX2_DIR/$f" 2>>"$LOG" || { offbox2_failed "upload of $f failed"; return 1; }
  remote=$(rclone size --json "$OFFBOX2_DIR/$f" 2>>"$LOG" | sed -n 's/.*"bytes":\([0-9]*\).*/\1/p') || { offbox2_failed "rclone size of $f failed"; return 1; }
  local_size=$(stat -c%s "$BACKUP_DIR/$f") || { offbox2_failed "cannot stat $BACKUP_DIR/$f"; return 1; }
  [ "$remote" = "$local_size" ] || { offbox2_failed "$f size mismatch on Blomp (local $local_size, remote ${remote:-none})"; return 1; }
}

# offbox2_retention — the Sync retention rule (by the date in the name) on the Blomp leg.
# Blomp's listing lags writes and deletes by minutes: a file it does not list yet is
# deleted on a later run. Failures are logged, never fatal.
offbox2_retention() {
  local listing rel day undeleted=0
  listing=$(rclone lsf -R --files-only "$OFFBOX2_REMOTE" 2>>"$LOG") || { offbox2_failed "listing $OFFBOX2_REMOTE failed"; return 1; }
  while read -r rel; do
    [ -n "$rel" ] || continue
    day=$(basename "$rel" | sed -nE 's/^[a-z]+_([0-9]{8})_[0-9]{6}\..*/\1/p')
    if [ -n "$day" ] && [ "$day" -lt "$CUTOFF" ]; then
      rclone deletefile "$OFFBOX2_REMOTE$rel" 2>>"$LOG" || undeleted=$((undeleted + 1))
    fi
  done <<< "$listing"
  write_metric vaultaire_backup_offbox2_retention_undeleted_files "old backup files (past the 30 days, by the date in the name) the last Blomp retention pass could not delete (pg-backup.sh)" "$undeleted"
  write_metric vaultaire_backup_offbox2_retention_last_run_timestamp_seconds "unix time the last Blomp retention pass listed the leg and tried every old file (pg-backup.sh; the undeleted-files gauge is as old as this, RP55-1)"
  [ "$undeleted" = 0 ] || { offbox2_failed "$undeleted old file(s) could not be deleted"; return 1; }
}

# age_recipients_usable — 0 when BACKUP_AGE_RECIPIENTS can seal a dump: readable, at least
# one recipient, every other line a comment or blank, every recipient an `age1…` public key
# (an AGE-SECRET-KEY line means an identity is on the box: refused, never echoed), the age
# binary present, and age itself accepts the file (a dry run over nothing). Sets
# AGE_RECIPIENT_COUNT, or AGE_REFUSAL (why not) and returns 1.
age_recipients_usable() {
  local line n=0
  AGE_REFUSAL=
  [ -r "$BACKUP_AGE_RECIPIENTS" ] || { AGE_REFUSAL="$BACKUP_AGE_RECIPIENTS is not readable"; return 1; }
  while IFS= read -r line || [ -n "$line" ]; do
    line=${line%$'\r'}
    case "$line" in
      ''|'#'*) continue ;;
      AGE-SECRET-KEY-*) AGE_REFUSAL="$BACKUP_AGE_RECIPIENTS holds an IDENTITY (a secret key): identities never live on the box — remove it and rotate it"; return 1 ;;
      AGE-PLUGIN-*) AGE_REFUSAL="$BACKUP_AGE_RECIPIENTS holds a plugin identity line: this is an identities file, not a recipients file (put the age1… recipient there)"; return 1 ;;
    esac
    [[ "$line" =~ ^age1[0-9a-z]+$ ]] || { AGE_REFUSAL="$BACKUP_AGE_RECIPIENTS has a line that is not an age1 public key"; return 1; }
    n=$((n + 1))
  done < "$BACKUP_AGE_RECIPIENTS"
  [ "$n" -gt 0 ] || { AGE_REFUSAL="$BACKUP_AGE_RECIPIENTS names no recipient"; return 1; }
  command -v "$AGE" >/dev/null 2>&1 || { AGE_REFUSAL="the age binary ($AGE) is not installed (deploy/scripts/age-install.sh)"; return 1; }
  # age itself judges the file: a recipient kind it does not know (age1pq1… under age 1.2)
  # or post-quantum and classic recipients mixed (age ≥ 1.3 refuses them) fail here.
  "$AGE" -R "$BACKUP_AGE_RECIPIENTS" -o /dev/null < /dev/null 2>>"$LOG" || { AGE_REFUSAL="age refuses $BACKUP_AGE_RECIPIENTS"; return 1; }
  AGE_RECIPIENT_COUNT=$n
  AGE_VERSION=$("$AGE" --version 2>/dev/null) || AGE_VERSION=unknown
}

# count_ddl FILE / count_bytes FILE — pass the stream through unchanged and write, when it
# ends, the number of CREATE TABLE lines / of bytes it carried to FILE: the dump's asserts
# read the stream, so the encrypted dump never has to be decrypted (or exist in plaintext)
# to be checked.
count_ddl() { { tee /dev/fd/3 | { grep -c 'CREATE TABLE' || true; } > "$1"; } 3>&1; }
count_bytes() { { tee /dev/fd/3 | wc -c > "$1"; } 3>&1; }

# seal — the pipeline's last stage: age to the recipients, or a plain copy without them.
seal() {
  if [ "$ENCRYPTED" = 1 ]; then "$AGE" -R "$BACKUP_AGE_RECIPIENTS"; else cat; fi
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

  # One run at a time (RP62-5): a second run's sweep of stale .dump-* directories would
  # delete the first one's work directory mid-dump.
  STAGE=lock
  command -v flock >/dev/null 2>&1 || fail "flock is not installed"
  exec 9>"$BACKUP_DIR/.lock" || fail "cannot open $BACKUP_DIR/.lock"
  flock -n 9 || fail "another pg-backup.sh run holds $BACKUP_DIR/.lock"

  # Encryption first: the recipients decide the dump's shape before any byte is read.
  # A dangling symlink is a file that cannot be used, never "absent" (RP62-4).
  STAGE=encrypt
  ENCRYPTED=0
  if [ -e "$BACKUP_AGE_RECIPIENTS" ] || [ -L "$BACKUP_AGE_RECIPIENTS" ]; then
    age_recipients_usable || fail "$AGE_REFUSAL: no dump written (a plaintext fallback would hide it)"
    ENCRYPTED=1
  else
    echo "$(date): WARNING - stage=encrypt: $BACKUP_AGE_RECIPIENTS not present: the dump is written in PLAINTEXT (plan 43.7; alert BackupDumpPlaintext)" >> "$LOG"
  fi

  # Database backup — -w: never prompt; fail instead. pipefail: any stage that fails fails
  # the run; the file is written under a dot-name and renamed only after the asserts.
  STAGE=dump
  DUMP="vaultaire_$TIMESTAMP.sql.gz"
  [ "$ENCRYPTED" = 0 ] || DUMP="$DUMP.age"
  rm -rf "$BACKUP_DIR"/.dump-*   # a run killed mid-dump leaves its partial and counters
  WORK=$(mktemp -d "$BACKUP_DIR/.dump-XXXXXX") || fail "cannot create a work directory in $BACKUP_DIR"
  pg_dump -w -h 127.0.0.1 -U vaultaire -d vaultaire | count_ddl "$WORK/ddl" | gzip | count_bytes "$WORK/size" | seal > "$WORK/partial" || {
    st=("${PIPESTATUS[@]}"); rm -rf "$WORK"
    fail "pg_dump failed (pipeline exit statuses pg_dump|count|gzip|count|seal = ${st[*]})"
  }
  unset PGPASSWORD

  # Size assert (on the gzip stream, before age): a failed dump gzips to ~20 bytes; a real
  # one is >1KB.
  SIZE=$(tr -d ' ' < "$WORK/size")
  [ "${SIZE:-0}" -gt 1024 ] || { rm -rf "$WORK"; fail "dump too small ($SIZE bytes): $DUMP"; }

  # Content assert (on the SQL stream): the dump must contain real DDL.
  DDL_COUNT=$(cat "$WORK/ddl")
  [ "${DDL_COUNT:-0}" -gt 0 ] || { rm -rf "$WORK"; fail "dump contains no CREATE TABLE statements"; }
  mv -f "$WORK/partial" "$BACKUP_DIR/$DUMP" || { rm -rf "$WORK"; fail "cannot rename the dump to $DUMP"; }
  rm -rf "$WORK"
  SIZE=$(stat -c%s "$BACKUP_DIR/$DUMP")   # what goes off-box (the .age file's own size)

  write_metric vaultaire_backup_last_success_timestamp_seconds "unix time the last pg_dump passed its size and DDL asserts (pg-backup.sh)"
  write_metric vaultaire_backup_dump_encrypted "1 = the last dump that passed its asserts was encrypted with age before it reached disk, 0 = written in plaintext (no recipients file; pg-backup.sh, plan 43.7)" "$ENCRYPTED"
  if [ "$ENCRYPTED" = 1 ]; then SHAPE="encrypted with age to $AGE_RECIPIENT_COUNT recipient(s), age $AGE_VERSION"; else SHAPE="PLAINTEXT"; fi
  echo "$(date): Backup completed - $DUMP (${SIZE} bytes, ${DDL_COUNT} tables, $SHAPE)" >> "$LOG"

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

  # The second off-box copy (Blomp, encrypted). Never fails the run.
  STAGE=offbox2
  OFFBOX2_OK=false
  if offbox2_setup; then
    OFFBOX2_OK=true
    if offbox2_copy "$DUMP"; then
      write_metric vaultaire_backup_offbox2_last_success_timestamp_seconds "unix time the last pg_dump was uploaded to Blomp (rclone crypt) and its size verified (pg-backup.sh)"
      echo "$(date): Off-box copy 2 completed - $DUMP → Blomp ($OFFBOX2_DIR, encrypted, $SIZE bytes verified)" >> "$LOG"
    fi
  fi

  # Config backup (.env included — do NOT silence errors here). Stays on the box.
  STAGE=configs
  tar czf "$BACKUP_DIR/configs_$TIMESTAMP.tar.gz" -C "$ROOT" configs/ || fail "config tar failed"

  # Prometheus backup, on the box and off it: prometheus.yml and the alert rules — what
  # this repo cannot rebuild by itself. Not the rest of /etc/prometheus (alertmanager.yml
  # carries the receivers' URLs; consoles and templates are the package's).
  STAGE=rules
  tar czf "$BACKUP_DIR/prometheus_$TIMESTAMP.tar.gz" -C "$PROM_DIR" prometheus.yml rules || fail "prometheus tar failed"
  offbox_copy "prometheus_$TIMESTAMP.tar.gz"
  if [ "$OFFBOX2_OK" = true ]; then STAGE=offbox2; offbox2_copy "prometheus_$TIMESTAMP.tar.gz" || true; STAGE=rules; fi

  # Keep only last 7 days on the box — both dump shapes: the plaintext .sql.gz of before
  # WP-P42 (and of a box without recipients) and the age-encrypted .sql.gz.age.
  STAGE=retention
  find "$BACKUP_DIR" \( -name "vaultaire_*.sql.gz" -o -name "vaultaire_*.sql.gz.age" \) -mtime +7 -delete
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
  write_metric vaultaire_backup_offbox_retention_undeleted_files "old backup files (past the 30 days, by the date in the name) the last Sync retention pass could not delete (pg-backup.sh)" "$undeleted"
  write_metric vaultaire_backup_offbox_retention_last_run_timestamp_seconds "unix time the last Sync retention pass listed the leg and tried every old file (pg-backup.sh; the undeleted-files gauge is as old as this, RP55-1)"
  [ "$undeleted" = 0 ] || fail "OFFBOX: $undeleted old file(s) could not be deleted"
  if [ "$OFFBOX2_OK" = true ]; then STAGE=offbox2; offbox2_retention || true; STAGE=retention; fi

  STAGE=finish
  echo "$(date): Run completed - configs + rules archived, rules → Sync, retention applied" >> "$LOG"
  # LAST: everything above succeeded.
  write_metric vaultaire_backup_run_last_success_timestamp_seconds "unix time pg-backup.sh last finished a whole run: dump, off-box copy, configs + rules tars, rules upload, retention"
}

# Sourced by pg-backup_test.sh for its functions; run as a script it backs up.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
