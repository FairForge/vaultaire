#!/bin/bash
# pg-pitr-install.sh — point-in-time recovery of the metadata database (Stage 1a Prompt 40):
# pgBackRest archives every WAL segment of the PostgreSQL 18 cluster to a private,
# encrypted repository and takes a weekly full + daily differential backup, so the cluster
# can be restored to any moment of the retention window — the nightly pg_dump
# (pg-backup.sh, 03:00 UTC) stays as the independent logical copy.
#
# Run as root on slc-vaultaire-01 from a checkout of this repository:
#   sudo bash deploy/scripts/pg-pitr-install.sh <cipher-file>
# <cipher-file> holds the repository passphrase (the owner's Keychain item
# `vaultaire-pgbackrest-cipher`, sent to the box on stdin into a 0600 file); it is read,
# then deleted. The passphrase is never an argument, never logged, never in the repo. A
# repository's passphrase never changes: a cipher that differs from the installed one is
# refused (the backups already in the repository would no longer open).
#
# What it installs (the repo is the source, the box gets a copy — docs/DEPLOY.md
# § Point-in-time recovery):
#   /etc/pgbackrest/pgbackrest.conf          postgres:postgres 0640, secrets inside
#   /usr/local/lib/vaultaire-pitr/           pg-pitr-metrics.sh, pg-pitr-drill.sh (root 0755)
#   /var/lib/vaultaire-pitr/vaultaire_pitr.prom  written each minute by the metrics timer
#                                            (postgres), read by node_exporter through a
#                                            root-owned symlink in its textfile directory
#   vaultaire-pitr-backup@.service + vaultaire-pitr-full.timer / -diff.timer
#   vaultaire-pitr-metrics.service + .timer
#   ALTER SYSTEM archive_mode / archive_command / archive_timeout (postgresql.auto.conf
#                                            backed up first, only when one of them changes)
#   pgbackrest stanza-create                 idempotent
# What it does NOT do (the operator's steps, in order, in docs/DEPLOY.md): the PostgreSQL
# restart that makes archive_mode take effect (at a quiet minute), check, the first full.
#
# The lines between the markers below are read by internal/api/produce_test.go: the
# produce page and the privacy policy state where the archive is, how it is encrypted and
# how long it lasts from THESE values (contract rule 8). Change them and the page together.
set -euo pipefail
umask 077

# --- facts (produce_test.go) ---
PITR_STANZA=vaultaire
PITR_REPO_VENDOR_ENDPOINT=r2.cloudflarestorage.com
PITR_REPO_BUCKET=stored-pg-pitr
PITR_REPO_PATH=/vaultaire
PITR_CIPHER_TYPE=aes-256-cbc
PITR_RETENTION_FULL=4
PITR_FULL_CALENDAR='Sun *-*-* 01:30:00'
PITR_DIFF_CALENDAR='Mon..Sat *-*-* 01:30:00'
PITR_PG_ARCHIVE_TIMEOUT=60s
# --- end facts ---

PITR_PG_PATH=/var/lib/postgresql/18/main
PITR_PG_SOCKET=/var/run/postgresql
PITR_PG_PORT=5432
# Sized for this box (32 threads, a 381 MB database): backups and restores move up to 4
# files at once, archive-push 2 (async, spooled on the NVMe).
PITR_PROCESS_MAX=4
PITR_ARCHIVE_PROCESS_MAX=2

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
PREFIX=${PITR_PREFIX:-}                       # tests: a temporary root; prod: /
ENV_FILE=${PITR_ENV_FILE:-/opt/vaultaire/configs/.env}
PG_USER=${PITR_PG_USER:-postgres}
CONF_DIR=$PREFIX/etc/pgbackrest
CONF=$CONF_DIR/pgbackrest.conf
UNIT_DIR=$PREFIX/etc/systemd/system
LIB_DIR=$PREFIX/usr/local/lib/vaultaire-pitr
METRICS_DIR=$PREFIX/var/lib/vaultaire-pitr
TEXTFILE_DIR=$PREFIX/var/lib/prometheus/node-exporter
SPOOL_DIR=$PREFIX/var/spool/pgbackrest
LOG_DIR=$PREFIX/var/log/pgbackrest

die() { echo "pg-pitr-install: $*" >&2; exit 1; }

# env_value_from FILE KEY — the value of the first KEY= line of an .env file with one level
# of surrounding quotes removed (what systemd's EnvironmentFile does; pg-backup.sh's parser).
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

# conf_value FILE KEY — the value of KEY= in an installed pgbackrest.conf (empty if none).
conf_value() {
  local line
  line=$(grep -m1 "^$2=" "$1" 2>/dev/null) || return 0
  printf '%s' "${line#"$2"=}"
}

# write_conf — pgbackrest.conf on stdout. Every secret comes from a variable read from a
# file, never from the command line.
write_conf() {
  cat <<EOF
# Installed by deploy/scripts/pg-pitr-install.sh — do not edit here; change the script.
# Secrets inside (S3 key, cipher passphrase): postgres:postgres 0640.
[global]
repo1-type=s3
repo1-s3-endpoint=$R2_ACCOUNT_ID.$PITR_REPO_VENDOR_ENDPOINT
repo1-s3-region=auto
repo1-s3-uri-style=path
repo1-s3-bucket=$PITR_REPO_BUCKET
repo1-s3-key=$R2_ACCESS_KEY
repo1-s3-key-secret=$R2_SECRET_KEY
repo1-path=$PITR_REPO_PATH
repo1-cipher-type=$PITR_CIPHER_TYPE
repo1-cipher-pass=$CIPHER
repo1-retention-full-type=count
repo1-retention-full=$PITR_RETENTION_FULL
repo1-bundle=y
compress-type=zst
compress-level=3
process-max=$PITR_PROCESS_MAX
start-fast=y
archive-async=y
spool-path=/var/spool/pgbackrest
log-path=/var/log/pgbackrest
log-level-console=info
log-level-file=detail

[global:archive-push]
process-max=$PITR_ARCHIVE_PROCESS_MAX

[global:archive-get]
process-max=$PITR_ARCHIVE_PROCESS_MAX

[$PITR_STANZA]
pg1-path=$PITR_PG_PATH
pg1-port=$PITR_PG_PORT
pg1-socket-path=$PITR_PG_SOCKET
EOF
}

# pg_sql SQL — one statement on the live cluster as the database superuser, over its socket.
pg_sql() {
  runuser -u "$PG_USER" -- psql -X -At -v ON_ERROR_STOP=1 -d postgres -c "$1"
}

# unit NAME — writes UNIT_DIR/NAME from stdin (0644).
unit() {
  cat > "$UNIT_DIR/$1"
  chmod 0644 "$UNIT_DIR/$1"
}

main() {
  [ $# -eq 1 ] || die "usage: $0 <cipher-file>"
  local cipher_file=$1 installed
  [ -r "$cipher_file" ] || die "cannot read the cipher file $cipher_file"
  IFS= read -r CIPHER < "$cipher_file" || true
  rm -f "$cipher_file"
  # base64/hex of at least 32 characters: the passphrase sits on a conf line, so nothing that
  # could end it or start a comment.
  [[ "$CIPHER" =~ ^[A-Za-z0-9+/=_-]{32,}$ ]] || die "the cipher passphrase must be 32+ characters of [A-Za-z0-9+/=_-]"

  R2_ACCOUNT_ID=$(env_value_from "$ENV_FILE" R2_ACCOUNT_ID) || R2_ACCOUNT_ID=
  R2_ACCESS_KEY=$(env_value_from "$ENV_FILE" R2_ACCESS_KEY) || R2_ACCESS_KEY=
  R2_SECRET_KEY=$(env_value_from "$ENV_FILE" R2_SECRET_KEY) || R2_SECRET_KEY=
  [[ "$R2_ACCOUNT_ID" =~ ^[0-9a-f]{32}$ ]] || die "R2_ACCOUNT_ID missing or malformed in $ENV_FILE"
  [ -n "$R2_ACCESS_KEY" ] || die "R2_ACCESS_KEY missing in $ENV_FILE"
  [ -n "$R2_SECRET_KEY" ] || die "R2_SECRET_KEY missing in $ENV_FILE"

  if [ -e "$CONF" ]; then
    installed=$(conf_value "$CONF" repo1-cipher-pass)
    [ -z "$installed" ] || [ "$installed" = "$CIPHER" ] || die "the cipher differs from the one in $CONF: a repository's passphrase never changes (its backups would no longer open)"
  fi

  command -v pgbackrest >/dev/null || DEBIAN_FRONTEND=noninteractive apt-get install -y -q pgbackrest
  echo "pg-pitr-install: $(pgbackrest version)"

  mkdir -p "$CONF_DIR" "$UNIT_DIR" "$LIB_DIR" "$METRICS_DIR" "$TEXTFILE_DIR" "$SPOOL_DIR" "$LOG_DIR"
  local tmp
  tmp=$(mktemp "$CONF_DIR/.pgbackrest.conf.XXXXXX")
  write_conf > "$tmp"
  chmod 0640 "$tmp"
  chown "$PG_USER:$PG_USER" "$tmp"
  if [ -e "$CONF" ] && ! cmp -s "$tmp" "$CONF"; then
    cp -p "$CONF" "$CONF.bak-$(date -u +%Y%m%dT%H%M%S)"
  fi
  mv -f "$tmp" "$CONF"
  chmod 0750 "$CONF_DIR"; chown "root:$PG_USER" "$CONF_DIR"
  chmod 0750 "$SPOOL_DIR" "$LOG_DIR"; chown "$PG_USER:$PG_USER" "$SPOOL_DIR" "$LOG_DIR"

  # The helper scripts: root-owned, so the database user runs what root installed.
  for s in pg-pitr-metrics.sh pg-pitr-drill.sh; do
    cp "$HERE/$s" "$LIB_DIR/$s"
    chmod 0755 "$LIB_DIR/$s"
  done
  chmod 0755 "$LIB_DIR"

  # Metrics: postgres writes its own directory; node_exporter reads the file through a
  # root-owned symlink in its one textfile directory (the backup-metrics layout).
  chmod 0755 "$METRICS_DIR"; chown "$PG_USER:$PG_USER" "$METRICS_DIR"
  ln -sfn /var/lib/vaultaire-pitr/vaultaire_pitr.prom "$TEXTFILE_DIR/vaultaire_pitr.prom"

  unit vaultaire-pitr-backup@.service <<EOF
[Unit]
Description=pgBackRest %i backup of the vaultaire stanza (pg-pitr-install.sh)
After=postgresql.service

[Service]
Type=oneshot
User=$PG_USER
Group=$PG_USER
ExecStart=/usr/bin/pgbackrest --stanza=$PITR_STANZA --type=%i backup
Nice=10
IOSchedulingClass=idle
EOF
  unit vaultaire-pitr-full.timer <<EOF
[Unit]
Description=Weekly full pgBackRest backup (expire keeps $PITR_RETENTION_FULL fulls)

[Timer]
OnCalendar=$PITR_FULL_CALENDAR
Persistent=true
Unit=vaultaire-pitr-backup@full.service

[Install]
WantedBy=timers.target
EOF
  unit vaultaire-pitr-diff.timer <<EOF
[Unit]
Description=Daily differential pgBackRest backup

[Timer]
OnCalendar=$PITR_DIFF_CALENDAR
Persistent=true
Unit=vaultaire-pitr-backup@diff.service

[Install]
WantedBy=timers.target
EOF
  unit vaultaire-pitr-metrics.service <<EOF
[Unit]
Description=Write the PITR textfile metrics (pg_stat_archiver + pgbackrest info)

[Service]
Type=oneshot
User=$PG_USER
Group=$PG_USER
ExecStart=/usr/local/lib/vaultaire-pitr/pg-pitr-metrics.sh
EOF
  unit vaultaire-pitr-metrics.timer <<EOF
[Unit]
Description=PITR textfile metrics, every minute

[Timer]
OnBootSec=1min
OnUnitActiveSec=1min
AccuracySec=5s

[Install]
WantedBy=timers.target
EOF

  systemctl daemon-reload
  systemctl enable --now vaultaire-pitr-metrics.timer vaultaire-pitr-full.timer vaultaire-pitr-diff.timer

  # The stanza (idempotent), then the archive settings. archive_timeout makes a segment
  # switch every minute the cluster writes: the RPO the produce page states.
  runuser -u "$PG_USER" -- pgbackrest --stanza="$PITR_STANZA" stanza-create
  local want current auto
  want="on|pgbackrest --stanza=$PITR_STANZA archive-push %p|$PITR_PG_ARCHIVE_TIMEOUT"
  current=$(pg_sql "SELECT current_setting('archive_mode') || '|' || current_setting('archive_command') || '|' || current_setting('archive_timeout')")
  if [ "$current" != "$want" ]; then
    auto=$PREFIX$PITR_PG_PATH/postgresql.auto.conf
    [ ! -e "$auto" ] || cp -p "$auto" "$auto.bak-pitr-$(date -u +%Y%m%dT%H%M%S)"
    pg_sql "ALTER SYSTEM SET archive_mode = 'on'" >/dev/null
    pg_sql "ALTER SYSTEM SET archive_command = 'pgbackrest --stanza=$PITR_STANZA archive-push %p'" >/dev/null
    pg_sql "ALTER SYSTEM SET archive_timeout = '$PITR_PG_ARCHIVE_TIMEOUT'" >/dev/null
    pg_sql "SELECT pg_reload_conf()" >/dev/null
  fi
  if [ "$(pg_sql "SELECT count(*) FROM pg_settings WHERE pending_restart")" != 0 ]; then
    echo "pg-pitr-install: PostgreSQL must be restarted for archive_mode (pending_restart) — at a quiet minute, docs/DEPLOY.md § Point-in-time recovery"
  fi
  echo "pg-pitr-install: installed $CONF, units, helpers, the stanza and the archive settings; next: docs/DEPLOY.md § Point-in-time recovery (restart, check, first full)"
}

# Sourced by pg-pitr_test.sh for its functions; run as a script it installs.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
