#!/bin/bash
# shellcheck disable=SC2016 # the fakes' bodies are single-quoted on purpose: they expand when the fake runs
# pg-pitr_test.sh — tests for deploy/scripts/pg-pitr-{install,metrics,drill}.sh. Run from
# anywhere:
#   bash deploy/scripts/pg-pitr_test.sh
# CI runs it (ci.yml "Deploy scripts"). Needs shellcheck and jq; the behavioural part needs
# GNU coreutils (`date +%3N`, `stat -c`) and is reported as SKIPPED without them. Every
# external binary the scripts call (pgbackrest, psql, pg_ctl, pg_isready, runuser,
# systemctl, chown, apt-get) is a fake on PATH that logs its arguments.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
INSTALL=$HERE/pg-pitr-install.sh
METRICS=$HERE/pg-pitr-metrics.sh
DRILL=$HERE/pg-pitr-drill.sh
FAILED=0
pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; FAILED=1; }
check() { # check MESSAGE COMMAND... — pass when the command succeeds
  local msg=$1; shift
  if "$@"; then pass "$msg"; else fail "$msg"; fi
}

# --- 1. shellcheck-clean -------------------------------------------------------------
if command -v shellcheck >/dev/null; then
  if shellcheck "$INSTALL" "$METRICS" "$DRILL" "$HERE/pg-pitr_test.sh"; then pass "shellcheck clean"; else fail "shellcheck"; fi
else
  fail "shellcheck is not installed"
fi

# --- 2. the .env parser is pg-backup.sh's ------------------------------------------------
TMP=$(mktemp -d)
trap 'chmod -R u+w "$TMP" 2>/dev/null; rm -rf "$TMP"' EXIT
parity_case() { # name, .env content, key, expected
  local name=$1 content=$2 key=$3 want=$4 a b
  printf '%s\n' "$content" > "$TMP/env"
  a=$(bash -c '. "$1"; env_value_from "$2" "$3"' _ "$INSTALL" "$TMP/env" "$key") || a="<rc $?>"
  b=$(bash -c '. "$1"; env_value_from "$2" "$3"' _ "$HERE/pg-backup.sh" "$TMP/env" "$key") || b="<rc $?>"
  if [ "$a" = "$want" ] && [ "$b" = "$want" ]; then pass "parser parity: $name"; else fail "parser parity: $name: install <$a> pg-backup <$b> want <$want>"; fi
}
parity_case "plain"            $'R2_ACCESS_KEY=abc\nX=1' R2_ACCESS_KEY abc
parity_case "quoted with ="    'R2_SECRET_KEY="a=b=="' R2_SECRET_KEY 'a=b=='
parity_case "longer key"       $'R2_ACCESS_KEY_OLD=x\nR2_ACCESS_KEY=y' R2_ACCESS_KEY y
parity_case "missing"          'X=1' R2_ACCESS_KEY "<rc 1>"

if ! date +%s%3N >/dev/null 2>&1 || [ "$(date +%s%3N | tr -d 0-9)" != "" ] || ! stat -c%a / >/dev/null 2>&1; then
  echo "SKIPPED behaviour tests: need GNU date/stat (run on Linux or in CI)"
  exit $FAILED
fi
command -v jq >/dev/null || { fail "jq is not installed"; exit $FAILED; }

BIN=$TMP/bin; mkdir -p "$BIN"
LOG=$TMP/calls.log

fake() { # fake NAME BODY — a logging fake on PATH
  printf '#!/bin/bash\necho "%s $*" >> "%s"\n%s\n' "$1" "$LOG" "$2" > "$BIN/$1"
  chmod +x "$BIN/$1"
}

# --- 3. pg-pitr-install.sh ------------------------------------------------------------
PREFIX=$TMP/root
ACCT=0123456789abcdef0123456789abcdef
CIPHER_VAL=Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5MGFiY2RlZmdoaWprbG1u
fake chown 'exit 0'
fake systemctl 'exit 0'
fake apt-get 'exit 0'
fake runuser 'while [ "$1" != -- ]; do shift; done; shift; exec "$@"'
fake pgbackrest 'case "$*" in version) echo "pgBackRest 2.58.0" ;; esac'
# psql: the live settings are TMP/settings (mode|command|timeout SECONDS, the raw
# pg_settings.setting values) until an ALTER SYSTEM; pending_restart after one. A FAITHFUL
# fake (RP58-5): current_setting() pretty-prints — archive_timeout 60 is "1min" — so a guard
# comparing current_setting('archive_timeout') to "60s" never matches on a real cluster;
# the installer must ask pg_settings for the raw values.
cat > "$BIN/psql" <<EOF
#!/bin/bash
echo "psql \$*" >> "$LOG"
sql=\${*: -1}
IFS='|' read -r mode cmd secs < "$TMP/settings"
pretty=\$secs; [ "\$secs" = 60 ] && pretty=1min; [ "\$secs" = 0 ] && pretty=0
case "\$sql" in
  *"current_setting('archive_mode')"*) echo "\$mode|\$cmd|\$pretty" ;;
  *"FROM pg_settings WHERE name IN"*) echo "archive_command=\$cmd|archive_mode=\$mode|archive_timeout=\$secs" ;;
  *"ALTER SYSTEM SET archive_mode"*) echo "on|pgbackrest --stanza=vaultaire archive-push %p|60" > "$TMP/settings"; touch "$TMP/pending" ;;
  *pending_restart*) [ -e "$TMP/pending" ] && echo 1 || echo 0 ;;
  *) : ;;
esac
EOF
chmod +x "$BIN/psql"
echo "off|(disabled)|0" > "$TMP/settings"
AUTO=$PREFIX/var/lib/postgresql/18/main/postgresql.auto.conf
mkdir -p "$(dirname "$AUTO")"; echo "shared_buffers = '2GB'" > "$AUTO"

install_run() { # install_run CIPHER — writes the cipher file and runs the installer
  printf '%s\n' "$1" > "$TMP/cipher"; chmod 0600 "$TMP/cipher"
  PATH="$BIN:$PATH" PITR_PREFIX=$PREFIX PITR_ENV_FILE=$TMP/prod.env bash "$INSTALL" "$TMP/cipher"
}
printf 'DB_PASSWORD=x\nR2_ACCOUNT_ID=%s\nR2_ACCESS_KEY="ak-123"\nR2_SECRET_KEY=sk=456\nR2_BUCKET=vaultaire-public\n' "$ACCT" > "$TMP/prod.env"

: > "$LOG"
if out=$(install_run "$CIPHER_VAL" 2>&1); then pass "3a: install runs"; else fail "3a: install failed: $out"; fi
CONF=$PREFIX/etc/pgbackrest/pgbackrest.conf
check "3a: the cipher file is deleted once read" [ ! -e "$TMP/cipher" ]
check "3a: pgbackrest.conf is 0640" [ "$(stat -c%a "$CONF" 2>/dev/null)" = 640 ]
check "3a: chowned to postgres" grep -q "^chown postgres:postgres $PREFIX/etc/pgbackrest/.pgbackrest.conf" "$LOG"
for line in "repo1-type=s3" "repo1-s3-endpoint=$ACCT.r2.cloudflarestorage.com" "repo1-s3-bucket=stored-pg-pitr" \
  "repo1-s3-key=ak-123" "repo1-s3-key-secret=sk=456" "repo1-cipher-type=aes-256-cbc" "repo1-cipher-pass=$CIPHER_VAL" \
  "repo1-retention-full-type=count" "repo1-retention-full=4" "archive-async=y" "spool-path=/var/spool/pgbackrest" \
  "[vaultaire]" "pg1-path=/var/lib/postgresql/18/main"; do
  check "3a: conf has $line" grep -qxF "$line" "$CONF"
done
check "3a: never the public bucket" [ -z "$(grep -F vaultaire-public "$CONF")" ]
check "3a: no secret in any command's arguments" [ -z "$(grep -e "$CIPHER_VAL" -e 'sk=456' -e 'ak-123' "$LOG")" ]
UNITS=$PREFIX/etc/systemd/system
check "3a: full timer weekly, Sunday 01:30" grep -qx 'OnCalendar=Sun \*-\*-\* 01:30:00' "$UNITS/vaultaire-pitr-full.timer"
check "3a: diff timer Monday..Saturday 01:30" grep -qx 'OnCalendar=Mon..Sat \*-\*-\* 01:30:00' "$UNITS/vaultaire-pitr-diff.timer"
check "3a: the backup service runs pgbackrest as postgres with the timer's type" grep -qx 'ExecStart=/usr/bin/pgbackrest --stanza=vaultaire --type=%i backup' "$UNITS/vaultaire-pitr-backup@.service"
check "3a: ... as postgres" grep -qx 'User=postgres' "$UNITS/vaultaire-pitr-backup@.service"
check "3a: the metrics service runs the installed copy" grep -qx 'ExecStart=/usr/local/lib/vaultaire-pitr/pg-pitr-metrics.sh' "$UNITS/vaultaire-pitr-metrics.service"
# The monthly drill (Prompt 41 item 4, RP57-13): root, idle priority, a quiet minute clear
# of 01:30 and 03:00, its metrics directory named, its own textfile symlinked.
check "3a: the drill service runs the installed drill as root" grep -qx 'ExecStart=/usr/local/lib/vaultaire-pitr/pg-pitr-drill.sh' "$UNITS/vaultaire-pitr-drill.service"
check "3a: ... as root" grep -qx 'User=root' "$UNITS/vaultaire-pitr-drill.service"
check "3a: ... at idle priority" bash -c "grep -qx 'Nice=10' '$UNITS/vaultaire-pitr-drill.service' && grep -qx 'IOSchedulingClass=idle' '$UNITS/vaultaire-pitr-drill.service'"
check "3a: ... writing /var/lib/vaultaire-pitr" grep -qx 'Environment=PITR_METRICS_DIR=/var/lib/vaultaire-pitr' "$UNITS/vaultaire-pitr-drill.service"
check "3a: the drill timer is monthly, the 2nd at 02:10 (not 01:30, not 03:00)" grep -qx 'OnCalendar=\*-\*-02 02:10:00' "$UNITS/vaultaire-pitr-drill.timer"
check "3a: ... persistent, naming the service" bash -c "grep -qx 'Persistent=true' '$UNITS/vaultaire-pitr-drill.timer' && grep -qx 'Unit=vaultaire-pitr-drill.service' '$UNITS/vaultaire-pitr-drill.timer'"
check "3a: the drill's textfile has its own symlink" [ "$(readlink "$PREFIX/var/lib/prometheus/node-exporter/vaultaire_pitr_drill.prom")" = /var/lib/vaultaire-pitr/vaultaire_pitr_drill.prom ]
check "3a: jq is ensured (pg-pitr-metrics.sh needs it)" grep -q 'command -v jq >/dev/null || DEBIAN_FRONTEND=noninteractive apt-get install -y -q jq' "$INSTALL"
for s in pg-pitr-metrics.sh pg-pitr-drill.sh; do
  check "3a: $s installed 0755, the repo's bytes" cmp -s "$HERE/$s" "$PREFIX/usr/local/lib/vaultaire-pitr/$s"
done
check "3a: textfile symlink to the metrics file" [ "$(readlink "$PREFIX/var/lib/prometheus/node-exporter/vaultaire_pitr.prom")" = /var/lib/vaultaire-pitr/vaultaire_pitr.prom ]
check "3a: daemon-reload" grep -qx 'systemctl daemon-reload' "$LOG"
check "3a: the four timers enabled" grep -qx 'systemctl enable --now vaultaire-pitr-metrics.timer vaultaire-pitr-full.timer vaultaire-pitr-diff.timer vaultaire-pitr-drill.timer' "$LOG"
check "3a: the timers are enabled AFTER stanza-create (RP58-9 c)" [ "$(grep -n -e 'stanza-create' -e 'systemctl enable' "$LOG" | head -1 | grep -c stanza-create)" = 1 ]
check "3a: stanza-create as postgres" grep -qx 'runuser -u postgres -- pgbackrest --stanza=vaultaire stanza-create' "$LOG"
check "3a: archive_mode on" grep -q "^psql .*ALTER SYSTEM SET archive_mode = 'on'" "$LOG"
check "3a: archive_command pushes to the stanza" grep -qF "ALTER SYSTEM SET archive_command = 'pgbackrest --stanza=vaultaire archive-push %p'" "$LOG"
check "3a: archive_timeout 60s" grep -qF "ALTER SYSTEM SET archive_timeout = '60s'" "$LOG"
check "3a: reloaded" grep -q '^psql .*pg_reload_conf' "$LOG"
check "3a: the restart is named, never done" grep -q 'must be restarted for archive_mode' <<< "$out"
check "3a: no restart attempted" [ -z "$(grep -E 'systemctl .*(restart|stop).*postgres' "$LOG")" ]
check "3a: postgresql.auto.conf backed up before the ALTER SYSTEM" [ "$(cat "$AUTO".bak-pitr-* 2>/dev/null)" = "shared_buffers = '2GB'" ]
check "3a: the stanza exists before archiving is switched on" [ "$(grep -n -e 'stanza-create' -e 'ALTER SYSTEM SET archive_mode' "$LOG" | grep -v '^.*runuser' | head -1 | grep -c stanza-create)" = 1 ]

# 3b. a re-run with the same cipher is idempotent: same file, no backup copy, no second
#     ALTER SYSTEM (the settings are already the wanted ones).
: > "$LOG"
if install_run "$CIPHER_VAL" >/dev/null 2>&1; then pass "3b: re-run with the same cipher"; else fail "3b: re-run failed"; fi
check "3b: no .bak when nothing changed" [ -z "$(find "$PREFIX/etc/pgbackrest" -name '*.bak-*')" ]
check "3b: no ALTER SYSTEM when the settings are already set (the faithful fake: current_setting says 1min)" [ -z "$(grep 'ALTER SYSTEM' "$LOG")" ]
check "3b: the guard asked pg_settings for the raw values" grep -q "FROM pg_settings WHERE name IN ('archive_mode', 'archive_command', 'archive_timeout')" "$LOG"
check "3b: one auto.conf backup only" [ "$(find "$(dirname "$AUTO")" -name 'postgresql.auto.conf.bak-pitr-*' | wc -l)" = 1 ]

# 3c. another cipher is refused, the conf untouched.
before=$(sha256sum "$CONF")
if install_run "Q2hhbmdlZFBhc3NwaHJhc2VDaGFuZ2VkUGFzc3BocmFzZQ" >/dev/null 2>&1; then fail "3c: a different cipher must be refused"; else pass "3c: a different cipher is refused"; fi
check "3c: the conf is untouched" [ "$(sha256sum "$CONF")" = "$before" ]
check "3c: the cipher file is deleted anyway" [ ! -e "$TMP/cipher" ]

# 3d. a short or unsafe passphrase is refused before anything is written.
rm -rf "$PREFIX"
if install_run "short" >/dev/null 2>&1; then fail "3d: a short cipher must be refused"; else pass "3d: a short cipher is refused"; fi
if install_run "has space and is long enough to pass the length check" >/dev/null 2>&1; then fail "3d: a cipher with spaces must be refused"; else pass "3d: a cipher with spaces is refused"; fi
check "3d: nothing installed" [ ! -e "$CONF" ]

# 3e. missing R2 credentials are refused.
printf 'R2_ACCOUNT_ID=%s\nR2_ACCESS_KEY=ak\n' "$ACCT" > "$TMP/prod.env"
if out=$(install_run "$CIPHER_VAL" 2>&1); then fail "3e: a missing R2_SECRET_KEY must be refused"; else pass "3e: missing R2_SECRET_KEY refused"; fi
check "3e: the message names it" grep -q 'R2_SECRET_KEY missing' <<< "$out"
check "3e: nothing installed" [ ! -e "$CONF" ]

# 3f. a key changed in .env is a new conf: the old one is kept as .bak.
printf 'R2_ACCOUNT_ID=%s\nR2_ACCESS_KEY=ak-1\nR2_SECRET_KEY=sk-1\n' "$ACCT" > "$TMP/prod.env"
install_run "$CIPHER_VAL" >/dev/null 2>&1
printf 'R2_ACCOUNT_ID=%s\nR2_ACCESS_KEY=ak-2\nR2_SECRET_KEY=sk-2\n' "$ACCT" > "$TMP/prod.env"
sleep 1; install_run "$CIPHER_VAL" >/dev/null 2>&1
check "3f: the new key is installed" grep -qx 'repo1-s3-key=ak-2' "$CONF"
baks=("$PREFIX"/etc/pgbackrest/pgbackrest.conf.bak-*)
check "3f: the old conf is kept beside it" grep -qx 'repo1-s3-key=ak-1' "${baks[0]}"

# 3g. --restore-box (RP58-7, the "box lost too" path): the conf, helpers and units, and
#     NOTHING that needs the repository's cluster — no stanza-create (the fresh cluster's
#     identifier is refused), no timers, no ALTER SYSTEM; exit 0 with the next step named.
rm -rf "$PREFIX"; : > "$LOG"
printf '%s\n' "$CIPHER_VAL" > "$TMP/cipher"; chmod 0600 "$TMP/cipher"
if out=$(PATH="$BIN:$PATH" PITR_PREFIX=$PREFIX PITR_ENV_FILE=$TMP/prod.env bash "$INSTALL" --restore-box "$TMP/cipher" 2>&1); then pass "3g: --restore-box runs"; else fail "3g: --restore-box failed: $out"; fi
check "3g: the conf is installed" grep -qx 'repo1-s3-key=ak-2' "$CONF"
check "3g: the units are installed" [ -e "$UNITS/vaultaire-pitr-backup@.service" ]
check "3g: no stanza-create" [ -z "$(grep 'stanza-create' "$LOG")" ]
check "3g: no timer enabled" [ -z "$(grep 'systemctl enable' "$LOG")" ]
check "3g: no ALTER SYSTEM, no psql at all" [ -z "$(grep -e 'ALTER SYSTEM' -e '^psql' "$LOG")" ]
check "3g: the next step is named" grep -q 'continue with docs/DEPLOY.md § Restoring the database step 3' <<< "$out"

# --- 4. pg-pitr-metrics.sh -------------------------------------------------------------
MDIR=$TMP/metrics; mkdir -p "$MDIR"
OUT=$MDIR/vaultaire_pitr.prom
SERIES="vaultaire_pg_archive_stats_success vaultaire_pg_archive_archived_total vaultaire_pg_archive_failed_total vaultaire_pg_archive_last_archived_timestamp_seconds vaultaire_pg_archive_last_failed_timestamp_seconds vaultaire_pg_pitr_info_success vaultaire_pg_pitr_last_full_timestamp_seconds vaultaire_pg_pitr_last_diff_timestamp_seconds vaultaire_pg_pitr_oldest_restorable_timestamp_seconds vaultaire_pg_pitr_backups vaultaire_pg_pitr_backup_repo_bytes vaultaire_pg_pitr_metrics_timestamp_seconds"
value() { sed -n "s/^$1 //p" "$OUT"; }
metrics_run() { PATH="$BIN:$PATH" PITR_METRICS_DIR=$MDIR bash "$METRICS"; }
# shellcheck disable=SC2317,SC2329 # called through check
all_series() { # every series, one sample, a TYPE line
  local m
  for m in $SERIES; do
    if [ "$(grep -c "^$m " "$OUT")" != 1 ] || ! grep -q "^# TYPE $m " "$OUT"; then echo "missing $m"; return 1; fi
  done
}
fake psql 'cat "'"$TMP"'/psql-out" 2>/dev/null; exit "$(cat "'"$TMP"'/psql-rc" 2>/dev/null || echo 0)"'
fake pgbackrest 'cat "'"$TMP"'/info-out" 2>/dev/null; exit "$(cat "'"$TMP"'/info-rc" 2>/dev/null || echo 0)"'
INFO_TWO='[{"name":"vaultaire","status":{"code":0,"message":"ok"},"backup":[
 {"type":"full","timestamp":{"start":1761000000,"stop":1761000100},"info":{"repository":{"delta":1000,"size":1000}}},
 {"type":"diff","timestamp":{"start":1761086400,"stop":1761086450},"info":{"repository":{"delta":200,"size":200}}},
 {"type":"full","timestamp":{"start":1761600000,"stop":1761600090},"info":{"repository":{"delta":1100,"size":1100}}},
 {"type":"diff","timestamp":{"start":1761686400,"stop":1761686420},"info":{"repository":{"delta":50,"size":50}}}]}]'

# 4a. both sources answer.
echo '1234|2|1761690000|1761680000' > "$TMP/psql-out"; echo 0 > "$TMP/psql-rc"
printf '%s' "$INFO_TWO" > "$TMP/info-out"; echo 0 > "$TMP/info-rc"
if metrics_run; then pass "4a: metrics run"; else fail "4a: metrics run failed"; fi
check "4a: every series written once with its TYPE" all_series
check "4a: archiver read" [ "$(value vaultaire_pg_archive_stats_success)/$(value vaultaire_pg_archive_archived_total)/$(value vaultaire_pg_archive_failed_total)" = "1/1234/2" ]
check "4a: last archived / failed" [ "$(value vaultaire_pg_archive_last_archived_timestamp_seconds)/$(value vaultaire_pg_archive_last_failed_timestamp_seconds)" = "1761690000/1761680000" ]
check "4a: info read" [ "$(value vaultaire_pg_pitr_info_success)" = 1 ]
check "4a: last full = the newest full's end" [ "$(value vaultaire_pg_pitr_last_full_timestamp_seconds)" = 1761600090 ]
check "4a: last diff = the newest backup's end" [ "$(value vaultaire_pg_pitr_last_diff_timestamp_seconds)" = 1761686420 ]
check "4a: oldest restorable = the oldest backup's end" [ "$(value vaultaire_pg_pitr_oldest_restorable_timestamp_seconds)" = 1761000100 ]
check "4a: 4 backups, 2350 bytes" [ "$(value vaultaire_pg_pitr_backups)/$(value vaultaire_pg_pitr_backup_repo_bytes)" = "4/2350" ]
check "4a: counters typed as counters" grep -qx '# TYPE vaultaire_pg_archive_failed_total counter' "$OUT"
check "4a: the file is 0644" [ "$(stat -c%a "$OUT")" = 644 ]
check "4a: no temp file left" [ -z "$(find "$MDIR" -name '.*' -type f)" ]
check "4a: metrics timestamp is now" [ $(( $(date +%s) - $(value vaultaire_pg_pitr_metrics_timestamp_seconds) )) -lt 5 ]

# 4b. a stanza before its first full (status 2, no backups): answered, zeros.
printf '%s' '[{"name":"vaultaire","status":{"code":2,"message":"no valid backups"},"backup":[]}]' > "$TMP/info-out"
metrics_run
check "4b: every series written" all_series
check "4b: info answered" [ "$(value vaultaire_pg_pitr_info_success)" = 1 ]
check "4b: no backup = 0s" [ "$(value vaultaire_pg_pitr_last_full_timestamp_seconds)/$(value vaultaire_pg_pitr_oldest_restorable_timestamp_seconds)/$(value vaultaire_pg_pitr_backups)" = "0/0/0" ]

# 4c. the repository does not answer (pgbackrest fails): info 0, its series 0 — never the
#     old values; the archiver's series still read.
echo 1 > "$TMP/info-rc"; printf '' > "$TMP/info-out"
metrics_run
check "4c: every series written" all_series
check "4c: info_success 0" [ "$(value vaultaire_pg_pitr_info_success)" = 0 ]
check "4c: last full 0, not the previous value" [ "$(value vaultaire_pg_pitr_last_full_timestamp_seconds)" = 0 ]
check "4c: the archiver still read" [ "$(value vaultaire_pg_archive_stats_success)" = 1 ]

# 4d. a stanza that is missing (status 1) is not an answer either.
echo 0 > "$TMP/info-rc"; printf '%s' '[{"name":"vaultaire","status":{"code":1,"message":"missing stanza path"},"backup":[]}]' > "$TMP/info-out"
metrics_run
check "4d: missing stanza = info_success 0" [ "$(value vaultaire_pg_pitr_info_success)" = 0 ]

# 4e. psql fails or answers garbage: stats 0, zeros; the file still complete.
echo 2 > "$TMP/psql-rc"; printf '%s' "$INFO_TWO" > "$TMP/info-out"
metrics_run
check "4e: every series written" all_series
check "4e: stats_success 0, last archived 0" [ "$(value vaultaire_pg_archive_stats_success)/$(value vaultaire_pg_archive_last_archived_timestamp_seconds)" = "0/0" ]
echo 0 > "$TMP/psql-rc"; echo 'ERROR: something' > "$TMP/psql-out"
metrics_run
check "4e: garbage = stats_success 0" [ "$(value vaultaire_pg_archive_stats_success)" = 0 ]
# A never-archived cluster: NULL times come back as 0 and are an answer.
echo '0|0|0|0' > "$TMP/psql-out"
metrics_run
check "4e: never archived = answered, 0" [ "$(value vaultaire_pg_archive_stats_success)/$(value vaultaire_pg_archive_last_archived_timestamp_seconds)" = "1/0" ]

# --- 5. pg-pitr-drill.sh ---------------------------------------------------------------
LIVE=$TMP/pg/18/main; DRILLD=$TMP/pg/18/pitr-drill
mkdir -p "$LIVE"; echo live > "$LIVE/PG_VERSION"
fake runuser 'while [ "$1" != -- ]; do shift; done; shift; exec "$@"'
# psql: answers by what it is asked. The drill's own cluster is recognised by its port.
cat > "$BIN/psql" <<EOF
#!/bin/bash
echo "psql \$*" >> "$LOG"
sql=\${*: -1}
case "\$*" in
  *"-p 5499"*)
    case "\$sql" in
      *pg_is_in_recovery*) echo f ;;
      *"FROM canary"*) cat "$TMP/drill-canary" ;;
      *object_head_cache*) cat "$TMP/drill-ohc" ;;
      *) echo "unexpected drill query: \$sql" >&2; exit 3 ;;
    esac ;;
  *)
    case "\$sql" in
      *"FROM pg_database"*) [ -e "$TMP/canary-db" ] && echo 1; true ;;
      *"CREATE DATABASE"*) touch "$TMP/canary-db" ;;
      *"CREATE TABLE"*|*INSERT*) : ;;
      *clock_timestamp*) echo "2026-10-24 22:30:01.123456+00" ;;
      *object_head_cache*) echo 5421 ;;
      *max_connections*) echo "100|8|10|0|64" ;;
      *) echo "unexpected live query: \$sql" >&2; exit 3 ;;
    esac ;;
esac
EOF
chmod +x "$BIN/psql"
cat > "$BIN/pgbackrest" <<EOF
#!/bin/bash
echo "pgbackrest \$*" >> "$LOG"
case "\$*" in
  *restore*)
    [ -e "$TMP/restore-fails" ] && { echo "ERROR: restore failed" >&2; exit 1; }
    d=\$(printf '%s\n' "\$@" | sed -n 's/^--pg1-path=//p')
    [ -d "\$d" ] && [ -z "\$(ls -A "\$d")" ] || { echo "ERROR: not an empty dir: \$d" >&2; exit 1; }
    echo 18 > "\$d/PG_VERSION"
    printf "%s\\n" "archive_mode = 'on'" "shared_buffers = '2GB'" "max_wal_size = '8GB'" "# Recovery settings generated by pgBackRest restore" "restore_command = 'pgbackrest --stanza=vaultaire archive-get %f \"%p\"'" "recovery_target_time = 'T'" > "\$d/postgresql.auto.conf" ;;
  *check*) [ -e "$TMP/check-fails" ] && exit 1; true ;;
esac
EOF
chmod +x "$BIN/pgbackrest"
fake pg_ctl 'for a; do case "$a" in start) touch "'"$DRILLD"'/postmaster.pid"; cp "'"$DRILLD"'/postgresql.auto.conf" "'"$TMP"'/started-auto.conf" ;; stop) rm -f "'"$DRILLD"'/postmaster.pid" ;; esac; done'
fake pg_isready 'exit 0'

DMETRICS=$TMP/drill-metrics; mkdir -p "$DMETRICS"
DOUT=$DMETRICS/vaultaire_pitr_drill.prom
dvalue() { sed -n "s/^$1 //p" "$DOUT"; }
drill_env() { # drill_env [DRILL_DIR] — the drill's environment, for drill_run and the background run
  echo "PATH=$BIN:$PATH PITR_PG_BIN=$BIN PITR_LIVE_DIR=$LIVE PITR_DRILL_DIR=${1:-$DRILLD} PITR_DRILL_GAP=0 PITR_PG_USER=postgres PITR_METRICS_DIR=$DMETRICS"
}
drill_run() {
  # shellcheck disable=SC2046 # the env words carry no spaces
  env $(drill_env "${1:-}") bash "$DRILL"
}
reset_drill() { : > "$LOG"; rm -rf "$DRILLD" "$TMP/restore-fails" "$TMP/check-fails"; echo "1|0" > "$TMP/drill-canary"; echo 5421 > "$TMP/drill-ohc"; }

# 5a. a good restore: PASS, the restore asked to stop at T with no archiving, the scratch
#     cluster started on its own port without archiving, then stopped and deleted.
reset_drill
if out=$(drill_run 2>&1); then pass "5a: drill passes"; else fail "5a: drill failed: $(tail -3 <<< "$out")"; fi
check "5a: the PASS line" grep -q 'PITR DRILL PASS target=2026-10-24 22:30:01.123456+00 .*rto_s=' <<< "$out"
r=$(grep '^pgbackrest .*restore' "$LOG" || true)
for want in "--pg1-path=$DRILLD" "--type=time" "--target=2026-10-24 22:30:01.123456+00" "--target-action=promote" "--archive-mode=off"; do
  check "5a: restore has $want" grep -qF -- "$want" <<< "$r"
done
check "5a: never the live data directory" [ -z "$(grep -F -- "--pg1-path=$LIVE" "$LOG")" ]
check "5a: the WAL past T archived before the restore (check, then restore)" [ "$(grep -n '^pgbackrest' "$LOG" | head -1 | grep -c check)" = 1 ]
s=$(grep '^pg_ctl .* start' "$LOG" || true)
check "5a: started on port 5499 with archive_mode=off" grep -q -- '-c port=5499 -c archive_mode=off' <<< "$s"
check "5a: no TCP listener" grep -q -- '-c listen_addresses= ' <<< "$s"
check "5a: stopped" grep -q '^pg_ctl .* stop' "$LOG"
check "5a: the scratch directory is deleted" [ ! -e "$DRILLD" ]
check "5a: the live directory is untouched" [ "$(cat "$LIVE/PG_VERSION")" = live ]
check "5a: two canary rows written to the live cluster, before and after T" [ "$(grep -c "^psql .*INSERT INTO canary" "$LOG")" = 2 ]
check "5a: T read between them" [ "$(grep -E '^psql .*(INSERT INTO canary|to_char\(clock_timestamp)' "$LOG" | sed -E 's/.*(before|clock_timestamp|after).*/\1/' | tr '\n' ' ')" = "before clock_timestamp after " ]
# RP58-6: the restored auto.conf (prod's tuning + pgbackrest's recovery lines) is kept and
# the scratch overrides are appended, so the effective value (the LAST assignment) is the
# drill's, and nothing pgbackrest wrote is lost.
check "5a: the restored auto.conf keeps pgbackrest's recovery lines" grep -q '^restore_command = ' "$TMP/started-auto.conf"
check "5a: ... and prod's tuning line is still there (appended to, never truncated)" grep -q "^shared_buffers = '2GB'" "$TMP/started-auto.conf"
check "5a: the effective shared_buffers is the drill's (last assignment)" [ "$(grep '^shared_buffers' "$TMP/started-auto.conf" | tail -1)" = "shared_buffers = 256MB" ]
check "5a: the effective max_wal_size is the drill's" [ "$(grep '^max_wal_size' "$TMP/started-auto.conf" | tail -1)" = "max_wal_size = 1GB" ]
check "5a: archive_mode off wins in auto.conf too" [ "$(grep '^archive_mode' "$TMP/started-auto.conf" | tail -1)" = "archive_mode = off" ]
# Item 4: the drill's own textfile, a PASS.
check "5a: the metrics file is written, 0644" [ "$(stat -c%a "$DOUT" 2>/dev/null)" = 644 ]
check "5a: success 1" [ "$(dvalue vaultaire_pg_pitr_drill_success)" = 1 ]
check "5a: last success is now" [ $(( $(date +%s) - $(dvalue vaultaire_pg_pitr_drill_last_success_timestamp_seconds) )) -lt 5 ]
check "5a: last run is now" [ $(( $(date +%s) - $(dvalue vaultaire_pg_pitr_drill_last_run_timestamp_seconds) )) -lt 5 ]
check "5a: the RTO is recorded (s.mmm)" bash -c '[[ "$1" =~ ^[0-9]+\.[0-9]{3}$ ]]' _ "$(dvalue vaultaire_pg_pitr_drill_rto_seconds)"
check "5a: every drill series has a TYPE line" [ "$(grep -c '^# TYPE vaultaire_pg_pitr_drill_' "$DOUT")" = 4 ]
check "5a: no temp file left in the metrics directory" [ -z "$(find "$DMETRICS" -name '.*' -type f)" ]
PASS_TS=$(dvalue vaultaire_pg_pitr_drill_last_success_timestamp_seconds); PASS_RTO=$(dvalue vaultaire_pg_pitr_drill_rto_seconds)

# 5b. the row written after T is in the restore: FAIL, and the cluster is still stopped
#     and deleted.
reset_drill; echo "1|1" > "$TMP/drill-canary"
if out=$(drill_run 2>&1); then fail "5b: a restore past T must fail the drill"; else pass "5b: a restore past T fails"; fi
check "5b: says why" grep -q 'PITR DRILL FAIL: the row written after T is present' <<< "$out"
check "5b: stopped" grep -q '^pg_ctl .* stop' "$LOG"
check "5b: deleted" [ ! -e "$DRILLD" ]
check "5b: the metrics say FAIL" [ "$(dvalue vaultaire_pg_pitr_drill_success)" = 0 ]
check "5b: ... and keep the previous PASS's time and RTO" [ "$(dvalue vaultaire_pg_pitr_drill_last_success_timestamp_seconds)/$(dvalue vaultaire_pg_pitr_drill_rto_seconds)" = "$PASS_TS/$PASS_RTO" ]
check "5b: last run moved" [ $(( $(date +%s) - $(dvalue vaultaire_pg_pitr_drill_last_run_timestamp_seconds) )) -lt 5 ]

# 5c. the row written before T is missing: FAIL.
reset_drill; echo "0|0" > "$TMP/drill-canary"
if drill_run >/dev/null 2>&1; then fail "5c: a missing before-T row must fail"; else pass "5c: a missing before-T row fails"; fi

# 5d. object_head_cache far from the live count at T: FAIL.
reset_drill; echo 5000 > "$TMP/drill-ohc"
if out=$(drill_run 2>&1); then fail "5d: a wrong object_head_cache count must fail"; else pass "5d: a wrong count fails"; fi
check "5d: says so" grep -q 'object_head_cache: 5000 rows restored, 5421 live at T' <<< "$out"
reset_drill; echo 5430 > "$TMP/drill-ohc"
if drill_run >/dev/null 2>&1; then pass "5d: within the slack passes"; else fail "5d: within the slack failed"; fi

# 5e. the restore fails: FAIL, no cluster started, nothing left behind.
reset_drill; touch "$TMP/restore-fails"
if drill_run >/dev/null 2>&1; then fail "5e: a failed restore must fail"; else pass "5e: a failed restore fails"; fi
check "5e: no cluster started" [ -z "$(grep '^pg_ctl' "$LOG")" ]
check "5e: nothing left" [ ! -e "$DRILLD" ]

# 5f. the WAL past T cannot be archived: FAIL before any restore.
reset_drill; touch "$TMP/check-fails"
if drill_run >/dev/null 2>&1; then fail "5f: a failed check must fail"; else pass "5f: a failed check fails"; fi
check "5f: no restore attempted" [ -z "$(grep 'restore' "$LOG")" ]

# 5g. refusals: the live directory, a directory inside it, one containing it, an existing one.
reset_drill
for bad in "$LIVE" "$LIVE/sub" "$TMP/pg" ; do
  if out=$(drill_run "$bad" 2>&1); then fail "5g: $bad must be refused"; else pass "5g: refuses $bad"; fi
  check "5g: nothing run for $bad" [ -z "$(grep -e '^pgbackrest' -e '^psql' "$LOG")" ]
done
mkdir -p "$DRILLD"; echo keep > "$DRILLD/keep"
if out=$(drill_run 2>&1); then fail "5g: an existing directory must be refused"; else pass "5g: refuses an existing directory"; fi
check "5g: ... and leaves it alone" [ "$(cat "$DRILLD/keep" 2>/dev/null)" = keep ]
check "5g: the live directory is untouched" [ "$(cat "$LIVE/PG_VERSION")" = live ]

# 5h. a drill cut by SIGTERM mid-wait (RP58-9 a) stops and deletes the scratch cluster
#     like a failed one, writes FAIL to its metrics and dies of the signal. The fake
#     pg_isready answers "not yet" while TMP/hold exists, so the drill is in its wait loop.
reset_drill; rm -rf "$DRILLD"; touch "$TMP/hold"
fake pg_isready '[ -e "'"$TMP"'/hold" ] && { sleep 0.2; exit 1; }; exit 0'
# The drill itself in the background (env … bash, no subshell in between: the signal
# must reach the drill's own bash).
# shellcheck disable=SC2046
env $(drill_env) bash "$DRILL" > "$TMP/5h.out" 2>&1 &
dpid=$!
for _ in $(seq 1 100); do grep -q '^pg_ctl .* start' "$LOG" 2>/dev/null && break; sleep 0.1; done
check "5h: the scratch cluster was started (the drill is waiting for it)" grep -q '^pg_ctl .* start' "$LOG"
kill -TERM "$dpid" || true
for _ in $(seq 1 100); do kill -0 "$dpid" 2>/dev/null || break; sleep 0.1; done
if kill -0 "$dpid" 2>/dev/null; then fail "5h: the drill did not die of SIGTERM within 10 s"; kill -KILL "$dpid" || true; fi
rc=0; wait "$dpid" || rc=$?   # set -e: a non-zero wait must not end the test
rm -f "$TMP/hold"
check "5h: the drill died of SIGTERM (143)" [ "$rc" = 143 ]
check "5h: says it was cut" grep -q 'PITR DRILL FAIL: cut by SIGTERM' "$TMP/5h.out"
check "5h: the scratch cluster was stopped" grep -q '^pg_ctl .* stop' "$LOG"
check "5h: ... and deleted" [ ! -e "$DRILLD" ]
check "5h: the metrics say FAIL" [ "$(dvalue vaultaire_pg_pitr_drill_success)" = 0 ]
check "5h: the live directory is untouched" [ "$(cat "$LIVE/PG_VERSION")" = live ]
fake pg_isready 'exit 0'

exit $FAILED
