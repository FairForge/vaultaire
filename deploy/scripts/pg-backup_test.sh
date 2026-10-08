#!/bin/bash
# pg-backup_test.sh — tests for deploy/scripts/pg-backup.sh. Run from anywhere:
#   bash deploy/scripts/pg-backup_test.sh
# CI runs it (ci.yml "Deploy scripts"). Needs shellcheck; the behavioural part needs
# GNU coreutils (the script is Linux-only: `stat -c`, `date -d`) and is reported as
# SKIPPED on a Mac without them.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT=$HERE/pg-backup.sh
FAILED=0
pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; FAILED=1; }
check() { # check MESSAGE COMMAND... — pass when the command succeeds
  local msg=$1; shift
  if "$@"; then pass "$msg"; else fail "$msg"; fi
}

# --- 1. shellcheck-clean -------------------------------------------------------------
if command -v shellcheck >/dev/null; then
  if shellcheck "$SCRIPT" "$HERE/pg-backup_test.sh"; then pass "shellcheck clean"; else fail "shellcheck"; fi
else
  fail "shellcheck is not installed"
fi

# --- 2. DB_PASSWORD parsing against fixture .env files ---------------------------------
# shellcheck source=deploy/scripts/pg-backup.sh
. "$SCRIPT"   # defines db_password_from_env; main() does not run when sourced

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

parse_case() { # name, .env content, expected value
  local name=$1 content=$2 want=$3 got
  printf '%s\n' "$content" > "$TMP/env"
  if ! got=$(db_password_from_env "$TMP/env"); then
    fail "$name: db_password_from_env returned non-zero"
  elif [ "$got" = "$want" ]; then
    pass "$name"
  else
    fail "$name: got <$got> want <$want>"
  fi
}
parse_case "plain value"                 $'PORT=8000\nDB_PASSWORD=hunter2\nDB_NAME=vaultaire' "hunter2"
parse_case "value containing ="           'DB_PASSWORD=abc=def==' "abc=def=="
parse_case "double-quoted value"          'DB_PASSWORD="quoted=pw"' "quoted=pw"
parse_case "single-quoted value"          "DB_PASSWORD='sq=pw'" "sq=pw"
parse_case "only one quote level removed" 'DB_PASSWORD=""inner""' '"inner"'
parse_case "unbalanced quote is kept"     'DB_PASSWORD="half' '"half'
parse_case "a longer key does not match"  $'DB_PASSWORD_OLD=stale\nDB_PASSWORD=fresh' "fresh"
parse_case "empty value prints nothing"   'DB_PASSWORD=' ""
printf 'DB_USER=x\n' > "$TMP/env"
if db_password_from_env "$TMP/env" >/dev/null; then fail "missing line: expected a non-zero return"; else pass "missing line returns 1"; fi

# --- 3. behaviour: the dump goes off-box before the tars; a failed tar fails the run but
#        not the upload; metrics are written where the textfile collector reads ---------
if ! stat -c%s /dev/null >/dev/null 2>&1 || ! date -d '-1 days' +%Y%m%d >/dev/null 2>&1; then
  echo "SKIPPED behaviour tests: need GNU stat/date (run on Linux or in CI)"
  exit $FAILED
fi

ROOT=$TMP/root; mkdir -p "$ROOT/backups" "$ROOT/logs" "$ROOT/configs"
PROM=$TMP/prom; mkdir -p "$PROM/rules"; echo "groups: []" > "$PROM/rules/x.yml"
METRICS=$TMP/metrics; mkdir -p "$METRICS"
REMOTE=$TMP/remote; mkdir -p "$REMOTE"
BIN=$TMP/bin; mkdir -p "$BIN"
printf 'DB_HOST=localhost\nDB_PASSWORD="p=w=d"\n' > "$ROOT/configs/.env"
printf 'RCLONE_CONFIG_SYNCBK_TYPE=webdav\n' > "$ROOT/configs/sync-backup.env"

cat > "$BIN/pg_dump" <<EOF
#!/bin/bash
printf '%s' "\$PGPASSWORD" > "$TMP/seen-password"
echo "CREATE TABLE users (id int);"
head -c 20000 /dev/urandom | base64
EOF
cat > "$BIN/rclone" <<EOF
#!/bin/bash
# Fake rclone: the remote is a directory; "syncbk:" maps to $REMOTE.
echo "\$*" >> "$TMP/rclone.log"
to_path() { printf '%s/%s' "$REMOTE" "\${1#syncbk:}"; }
case "\$1" in
  copyto)  src=\${*: -2:1}; dst=\$(to_path "\${*: -1}"); mkdir -p "\$(dirname "\$dst")"; cp "\$src" "\$dst" ;;
  size)    printf '{"count":1,"bytes":%s}\n' "\$(stat -c%s "\$(to_path "\$3")")" ;;
  lsf)     (cd "\$(to_path "\${*: -1}")" 2>/dev/null && find . -type f | sed 's#^\./##') ;;
  deletefile) rm -f "\$(to_path "\$2")" ;;
  *) echo "fake rclone: unexpected \$*" >&2; exit 99 ;;
esac
EOF
REAL_TAR=$(command -v tar)
cat > "$BIN/tar" <<EOF
#!/bin/bash
if [ -e "$TMP/tar-configs-fails" ] && printf '%s\n' "\$@" | grep -qx 'configs/'; then
  echo "tar: configs/.env.bak: Cannot open: Permission denied" >&2; exit 2
fi
exec "$REAL_TAR" "\$@"
EOF
chmod +x "$BIN"/*

run_backup() {
  PATH="$BIN:$PATH" PG_BACKUP_ROOT=$ROOT PG_BACKUP_PROM_DIR=$PROM PG_BACKUP_METRICS_DIR=$METRICS bash "$SCRIPT"
}

# 3a. the config tar fails (the 2026-10-05 shape): the dump is still off-box.
touch "$TMP/tar-configs-fails"
if run_backup; then fail "3a: a failed config tar must fail the run"; else pass "3a: failed config tar fails the run"; fi
check "3a: pg_dump got the unquoted password with its = (saw <$(cat "$TMP/seen-password")>)" [ "$(cat "$TMP/seen-password")" = "p=w=d" ]
check "3a: failure logged" grep -q 'BACKUP FAILED - config tar failed' "$ROOT/logs/backup.log"
dumps=("$ROOT"/backups/vaultaire_*.sql.gz)
check "3a: one local dump" [ "${#dumps[@]}" = 1 ]
dump=$(basename "${dumps[0]}")
check "3a: the dump is off-box despite the tar failure (upload used to run after the tars)" [ -f "$REMOTE/_ops/backups/$(date -u +%Y/%m)/$dump" ]
check "3a: off-box copy logged with the size" grep -q "Off-box copy completed - $dump" "$ROOT/logs/backup.log"
check "3a: no rules tar uploaded after the failed config tar" [ -z "$(find "$REMOTE" -name 'prometheus_*')" ]
for m in vaultaire_backup_last_success_timestamp_seconds vaultaire_backup_offbox_last_success_timestamp_seconds; do
  f=$METRICS/$m.prom
  if [ -f "$f" ] && grep -qE "^$m [0-9]{10}$" "$f" && [ "$(stat -c%a "$f")" = "644" ]; then pass "3a: $m written, world-readable"; else fail "3a: $m missing or malformed: $(cat "$f" 2>/dev/null)"; fi
done
check "3a: no temp file left in the metrics dir" [ -z "$(find "$METRICS" -name '.*' -type f)" ]

# 3b. everything works: dump + rules off-box, configs stay, run completes.
rm -f "$TMP/tar-configs-fails"; rm -rf "${REMOTE:?}"/* "${ROOT:?}/backups"/* "${METRICS:?}"/*
sleep 1   # a new TIMESTAMP
if run_backup; then pass "3b: run completes"; else fail "3b: run failed: $(tail -3 "$ROOT/logs/backup.log")"; fi
n_dump=$(find "$REMOTE" -name 'vaultaire_*.sql.gz' | wc -l); n_rules=$(find "$REMOTE" -name 'prometheus_*.tar.gz' | wc -l); n_cfg=$(find "$REMOTE" -name 'configs_*' | wc -l)
check "3b: dump + rules off-box, configs stay on the box (dump=$n_dump rules=$n_rules configs=$n_cfg)" [ "$n_dump/$n_rules/$n_cfg" = "1/1/0" ]
cfgs=("$ROOT"/backups/configs_*.tar.gz)
check "3b: configs tar on the box" [ -f "${cfgs[0]}" ]
check "3b: completion logged" grep -q 'Run completed' "$ROOT/logs/backup.log"
# order of the calls: the dump's copyto precedes the rules tar's copyto
first_copy=$(grep '^copyto' "$TMP/rclone.log" | tail -2 | head -1)
case "$first_copy" in *vaultaire_*.sql.gz*) pass "3b: the dump is the first upload" ;; *) fail "3b: first upload was: $first_copy" ;; esac

# 3c. a metrics directory that cannot be written is logged, never fatal.
chmod 0555 "$METRICS"; rm -rf "${REMOTE:?}"/* "${ROOT:?}/backups"/*; sleep 1
if run_backup; then pass "3c: unwritable metrics dir does not fail the run"; else fail "3c: run failed on the metrics dir"; fi
chmod 0755 "$METRICS"
check "3c: logged" grep -q 'METRICS: cannot write' "$ROOT/logs/backup.log"

# 3d. off-box retention: a file older than 30 days by NAME goes, a fresh one stays.
old=$(date -u -d '-40 days' +%Y%m%d); mkdir -p "$REMOTE/_ops/backups/2026/01"
: > "$REMOTE/_ops/backups/2026/01/vaultaire_${old}_030001.sql.gz"
rm -rf "${ROOT:?}/backups"/*; sleep 1; run_backup >/dev/null
check "3d: 40-day-old remote dump deleted by its name" [ ! -e "$REMOTE/_ops/backups/2026/01/vaultaire_${old}_030001.sql.gz" ]
check "3d: fresh remote dumps kept ($(find "$REMOTE" -name 'vaultaire_*' | wc -l))" [ "$(find "$REMOTE" -name 'vaultaire_*.sql.gz' | wc -l)" = 2 ]

exit $FAILED
