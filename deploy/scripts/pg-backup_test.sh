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
  if shellcheck "$SCRIPT" "$HERE/pg-backup_test.sh" "$HERE/db-password.sh" "$HERE/age-install.sh"; then pass "shellcheck clean"; else fail "shellcheck"; fi
else
  fail "shellcheck is not installed"
fi

# --- 2. DB_PASSWORD parsing against fixture .env files ---------------------------------
# shellcheck source=deploy/scripts/pg-backup.sh
. "$SCRIPT"   # defines db_password_from_env; main() does not run when sourced
# The script defines its own fail() (log + exit 1): restore the test's, or the first
# failing check after this line would end the test silently.
fail() { echo "FAIL $1"; FAILED=1; }

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

# --- 2c. write_metric never ends the run silently --------------------------------------
# A printf failing inside write_metric (a full disk) ended the run through set -e with no
# line at all: the ERR trap is not inherited by functions (Prompt 2a.3 H3).
# shellcheck disable=SC2034 # LOG, METRICS_DIR and STAGE are read by the sourced functions
wm_out=$(
  LOG=$TMP/wm.log METRICS_DIR=$TMP STAGE=offbox
  # shellcheck disable=SC2317,SC2329 # shadows the builtin inside the sourced write_metric
  printf() { return 1; }
  set -e
  write_metric vaultaire_test_metric "test"
  echo continued
) || true
check "2c: a failed metric write does not end the run" [ "$wm_out" = continued ]
check "2c: it is logged with its stage" grep -q 'METRICS: cannot write vaultaire_test_metric (non-fatal, stage=offbox)' "$TMP/wm.log"
check "2c: no temporary file is left behind" [ -z "$(find "$TMP" -maxdepth 1 -name '.vaultaire_test_metric.*')" ]

# --- 2b. deploy.yml reads the password with the SAME parser ---------------------------
# deploy.yml sources deploy/scripts/db-password.sh on the box; pg-backup.sh (one installed
# file) carries its own copy. Both must answer every fixture identically, and the workflow
# must not grow its own parser again (`grep DB_PASSWORD | cut -d= -f2` until 2a.2).
WORKFLOW=$HERE/../../.github/workflows/deploy.yml
parity_case() { # name, .env content, expected value
  local name=$1 content=$2 want=$3 a b
  printf '%s\n' "$content" > "$TMP/env"
  a=$(bash -c '. "$1"; db_password_from_env "$2"' _ "$SCRIPT" "$TMP/env") || a="<rc $?>"
  b=$(bash -c '. "$1"; db_password_from_env "$2"' _ "$HERE/db-password.sh" "$TMP/env") || b="<rc $?>"
  if [ "$a" = "$want" ] && [ "$b" = "$want" ]; then pass "parity: $name"; else fail "parity: $name: pg-backup <$a> deploy <$b> want <$want>"; fi
}
parity_case "plain value"                 $'PORT=8000\nDB_PASSWORD=hunter2\nDB_NAME=vaultaire' "hunter2"
parity_case "value containing ="           'DB_PASSWORD=abc=def==' "abc=def=="
parity_case "double-quoted value"          'DB_PASSWORD="quoted=pw"' "quoted=pw"
parity_case "single-quoted value"          "DB_PASSWORD='sq=pw'" "sq=pw"
parity_case "only one quote level removed" 'DB_PASSWORD=""inner""' '"inner"'
parity_case "unbalanced quote is kept"     'DB_PASSWORD="half' '"half'
parity_case "a longer key does not match"  $'DB_PASSWORD_OLD=stale\nDB_PASSWORD=fresh' "fresh"
parity_case "missing line"                 'DB_USER=x' "<rc 1>"
n_own=$(grep -cE 'grep[^|]*DB_PASSWORD|cut -d= -f2( |\))' "$WORKFLOW" || true)
check "deploy.yml has no parser of its own (found $n_own)" [ "$n_own" = 0 ]
n_use=$(grep -c 'db_password_from_env /opt/vaultaire/configs/.env' "$WORKFLOW" || true)
check "deploy.yml reads the password through db_password_from_env in both jobs (found $n_use)" [ "$n_use" = 2 ]
n_src=$(grep -c '\. /tmp/vaultaire-db-password.sh' "$WORKFLOW" || true)
check "deploy.yml sources the shared parser in both jobs (found $n_src)" [ "$n_src" = 2 ]

# --- 3. behaviour: the dump goes off-box before the tars; a failed tar fails the run but
#        not the upload; metrics are written where the textfile collector reads ---------
if ! stat -c%s /dev/null >/dev/null 2>&1 || ! date -d '-1 days' +%Y%m%d >/dev/null 2>&1; then
  echo "SKIPPED behaviour tests: need GNU stat/date (run on Linux or in CI)"
  exit $FAILED
fi

ROOT=$TMP/root; mkdir -p "$ROOT/backups" "$ROOT/logs" "$ROOT/configs"
PROM=$TMP/prom; mkdir -p "$PROM/rules" "$PROM/consoles"; echo "groups: []" > "$PROM/rules/x.yml"
echo "rule_files: []" > "$PROM/prometheus.yml"; echo "receivers: []" > "$PROM/alertmanager.yml"; : > "$PROM/consoles/c.html"
METRICS=$TMP/metrics; mkdir -p "$METRICS"
REMOTE=$TMP/remote; mkdir -p "$REMOTE"
REMOTE2=$TMP/remote2; mkdir -p "$REMOTE2"
BIN=$TMP/bin; mkdir -p "$BIN"
printf 'DB_HOST=localhost\nDB_PASSWORD="p=w=d"\n' > "$ROOT/configs/.env"
printf 'RCLONE_CONFIG_SYNCBK_TYPE=webdav\n' > "$ROOT/configs/sync-backup.env"

cat > "$BIN/pg_dump" <<EOF
#!/bin/bash
printf '%s' "\$PGPASSWORD" > "$TMP/seen-password"
if [ -e "$TMP/pg_dump-tiny" ]; then echo "CREATE TABLE x();"; exit 0; fi
[ -e "$TMP/pg_dump-no-ddl" ] || echo "CREATE TABLE users (id int);"
head -c 20000 /dev/urandom | base64
[ ! -e "$TMP/pg_dump-fails" ] || { echo "pg_dump: error: connection lost" >&2; exit 1; }
EOF
# Fake age (WP-P42): records its arguments; encrypts as a marker header + base64 of the
# input (so nothing it writes is gzip or SQL), decrypts only with the fake identity. Its
# own check of a recipients file refuses a line `age1refusedbyage`, and post-quantum
# `age1pq1…` recipients mixed with classic ones (what age >= 1.3 refuses, RP62-1). Its
# header is padded to the ~1.7 KB of a real age1pq stanza (RP62-2: an empty gzip seals to
# 1,679 B with one age1pq recipient, so a size check on the sealed file would pass it).
cat > "$BIN/age" <<EOF
#!/bin/bash
echo "\$*" >> "$TMP/age.log"
[ "\$1" != --version ] || { echo v1.3.2-fake; exit 0; }
dec=false; out=/dev/stdout; rfile=; ident=; in=/dev/stdin
while [ \$# -gt 0 ]; do
  case "\$1" in -d) dec=true ;; -R) rfile=\$2; shift ;; -i) ident=\$2; shift ;; -o) out=\$2; shift ;; *) in=\$1 ;; esac
  shift
done
if \$dec; then
  [ "\$(cat "\$ident" 2>/dev/null)" = FAKE-IDENTITY ] || { echo "age: no identity matched any of the recipients" >&2; exit 1; }
  sed 1,3d "\$in" | base64 -d > "\$out"; exit
fi
[ -r "\$rfile" ] || { echo "age: cannot read \$rfile" >&2; exit 1; }
! grep -q age1refusedbyage "\$rfile" || { echo "age: malformed recipient" >&2; exit 1; }
if grep -q '^age1pq1' "\$rfile" && grep '^age1' "\$rfile" | grep -qv '^age1pq1'; then
  echo "age: error: incompatible recipients: can't mix post-quantum and classic recipients" >&2; exit 1
fi
{ printf 'age-encryption.org/v1\n-> fake %s %s\n---\n' "\$(grep -c '^age1' "\$rfile")" "\$(head -c 1300 /dev/zero | base64 | tr -d '\n')"; base64 < "\$in"; } > "\$out"
EOF
cat > "$BIN/rclone" <<EOF
#!/bin/bash
# Fake rclone: a remote is a directory; "syncbk:" maps to $REMOTE, "blompcrypt:" to $REMOTE2.
echo "\$*" >> "$TMP/rclone.log"
[ -e "$TMP/rclone-\$1-fails" ] && { echo "fake rclone: \$1 failed" >&2; exit 1; }
case "\$*" in *blompcrypt:*)
  echo "\$RCLONE_CONFIG_BLOMPBK_USER|\$RCLONE_CONFIG_BLOMPBK_KEY|\$RCLONE_CONFIG_BLOMPCRYPT_REMOTE|\$RCLONE_CONFIG_BLOMPCRYPT_TYPE" > "$TMP/rclone2-env"
  [ -e "$TMP/rclone2-fails" ] && { echo "fake rclone: blomp unreachable" >&2; exit 1; }
  [ "\$1" = lsf ] && [ -e "$TMP/rclone2-lsf-fails" ] && { echo "fake rclone: blomp listing failed" >&2; exit 1; } ;;
esac
to_path() { case "\$1" in blompcrypt:*) printf '%s/%s' "$REMOTE2" "\${1#blompcrypt:}" ;; *) printf '%s/%s' "$REMOTE" "\${1#syncbk:}" ;; esac; }
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
# flock is util-linux (the box, CI); a Mac has none: a shim with the same contract for
# `flock -n FD` — perl locks the inherited descriptor, and the lock stays with the
# caller's open file after perl exits.
if ! command -v flock >/dev/null; then
  cat > "$BIN/flock" <<'SHIM'
#!/bin/bash
fd=${*: -1}
exec perl -e 'use Fcntl qw(:flock); open(my $fh, ">&=", $ARGV[0]) or exit 2; flock($fh, LOCK_EX|LOCK_NB) or exit 1' "$fd"
SHIM
fi
chmod +x "$BIN"/*

run_backup() {
  PATH="$BIN:$PATH" PG_BACKUP_ROOT=$ROOT PG_BACKUP_PROM_DIR=$PROM PG_BACKUP_METRICS_DIR=$METRICS bash "$SCRIPT"
}

RUN_METRIC=vaultaire_backup_run_last_success_timestamp_seconds
reset_run() { rm -f "$TMP"/rclone-*-fails "$TMP/rclone2-fails" "$TMP/rclone2-env" "$TMP/tar-configs-fails" "$TMP"/pg_dump-* "$TMP/seen-password" "$TMP/age.log"; rm -rf "${REMOTE:?}"/* "${REMOTE2:?}"/* "${ROOT:?}/backups"/* "${METRICS:?}"/*; : > "$ROOT/logs/backup.log"; sleep 1; }

# 3a. the config tar fails (the 2026-10-05 shape): the dump is still off-box.
touch "$TMP/tar-configs-fails"
if run_backup; then fail "3a: a failed config tar must fail the run"; else pass "3a: failed config tar fails the run"; fi
check "3a: pg_dump got the unquoted password with its = (saw <$(cat "$TMP/seen-password")>)" [ "$(cat "$TMP/seen-password")" = "p=w=d" ]
check "3a: failure logged with its stage" grep -q 'BACKUP FAILED - stage=configs: config tar failed' "$ROOT/logs/backup.log"
check "3a: no run-success timestamp: the configs tar is the only backup of .env" [ ! -e "$METRICS/$RUN_METRIC.prom" ]
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
f=$METRICS/$RUN_METRIC.prom
if [ -f "$f" ] && grep -qE "^$RUN_METRIC [0-9]{10}$" "$f"; then pass "3b: $RUN_METRIC written"; else fail "3b: $RUN_METRIC missing or malformed: $(cat "$f" 2>/dev/null)"; fi
check "3b: the run timestamp is written last (not older than the off-box one)" [ ! "$f" -ot "$METRICS/vaultaire_backup_offbox_last_success_timestamp_seconds.prom" ]
rules_tar=$(find "$ROOT/backups" -name 'prometheus_*.tar.gz' | head -1)
members=$(tar tzf "$rules_tar" | sed 's#^\./##' | grep -v -e '/$' -e '^$' | sort | tr '\n' ' ')
check "3b: the rules tar holds prometheus.yml + rules/ only (got: $members)" [ "$members" = "prometheus.yml rules/x.yml " ]
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
check "3d: vaultaire_backup_offbox_retention_undeleted_files = 0 after a clean pass" grep -qx "vaultaire_backup_offbox_retention_undeleted_files 0" "$METRICS/vaultaire_backup_offbox_retention_undeleted_files.prom"

# 3e. rclone cannot verify the dump's size (the `rclone size` failure that used to exit
#     through set -e with no log line): logged with its stage, no off-box or run timestamp.
reset_run; touch "$TMP/rclone-size-fails"
if run_backup 2>/dev/null; then fail "3e: a failed rclone size must fail the run"; else pass "3e: failed rclone size fails the run"; fi
check "3e: logged as BACKUP FAILED with stage=offbox ($(grep 'BACKUP FAILED' "$ROOT/logs/backup.log" | tail -1 | cut -c1-120))" grep -q 'BACKUP FAILED - stage=offbox' "$ROOT/logs/backup.log"
check "3e: the dump timestamp is written (the dump itself is fine)" [ -f "$METRICS/vaultaire_backup_last_success_timestamp_seconds.prom" ]
check "3e: no off-box timestamp" [ ! -e "$METRICS/vaultaire_backup_offbox_last_success_timestamp_seconds.prom" ]
check "3e: no run timestamp" [ ! -e "$METRICS/$RUN_METRIC.prom" ]

# 3f. the off-box retention listing fails: logged with its stage, the run is not a success.
reset_run; touch "$TMP/rclone-lsf-fails"
if run_backup 2>/dev/null; then fail "3f: a failed retention listing must fail the run"; else pass "3f: failed retention listing fails the run"; fi
check "3f: logged as BACKUP FAILED with stage=retention ($(grep 'BACKUP FAILED' "$ROOT/logs/backup.log" | tail -1 | cut -c1-120))" grep -q 'BACKUP FAILED - stage=retention' "$ROOT/logs/backup.log"
check "3f: dump + off-box timestamps written" [ -f "$METRICS/vaultaire_backup_offbox_last_success_timestamp_seconds.prom" ]
check "3f: no run timestamp after a retention failure" [ ! -e "$METRICS/$RUN_METRIC.prom" ]

# 3g. one old off-box file that cannot be deleted: the run is not a success either.
reset_run; old=$(date -u -d '-40 days' +%Y%m%d); mkdir -p "$REMOTE/_ops/backups/2026/01"
: > "$REMOTE/_ops/backups/2026/01/vaultaire_${old}_030001.sql.gz"; touch "$TMP/rclone-deletefile-fails"
if run_backup 2>/dev/null; then fail "3g: a failed retention delete must fail the run"; else pass "3g: failed retention delete fails the run"; fi
check "3g: logged with stage=retention" grep -q 'BACKUP FAILED - stage=retention' "$ROOT/logs/backup.log"
check "3g: no run timestamp" [ ! -e "$METRICS/$RUN_METRIC.prom" ]
# RP54-6: the count of old files the leg could not delete is exported (the run fails too).
UNDELETED=vaultaire_backup_offbox_retention_undeleted_files
check "3g: $UNDELETED = 1 ($(grep "^$UNDELETED " "$METRICS/$UNDELETED.prom" 2>/dev/null))" grep -qx "$UNDELETED 1" "$METRICS/$UNDELETED.prom"
check "3g: it is a gauge with its help line" grep -q "^# TYPE $UNDELETED gauge" "$METRICS/$UNDELETED.prom"

# 3h. the alert rule exists for the new timestamp (> 26 h or absent).
RULES=$HERE/../monitoring/vaultaire-backup.yml
check "3h: BackupRunIncomplete rule on $RUN_METRIC" grep -q "alert: BackupRunIncomplete" "$RULES"
check "3h: it covers absence" grep -q "absent($RUN_METRIC)" "$RULES"

# --- 4. the second off-box leg (Blomp through an rclone crypt remote) -----------------
OFFBOX2_METRIC=vaultaire_backup_offbox2_last_success_timestamp_seconds

# 4a. not set up on this box (no blomp-backup.env): skipped, logged, the run completes.
reset_run
if run_backup; then pass "4a: run completes without the Blomp leg"; else fail "4a: run failed: $(tail -3 "$ROOT/logs/backup.log")"; fi
check "4a: logged as skipped" grep -q 'OFFBOX2: .*blomp-backup.env not present, Blomp copy skipped' "$ROOT/logs/backup.log"
check "4a: no Blomp call made" [ ! -e "$TMP/rclone2-env" ]
check "4a: no offbox2 timestamp" [ ! -e "$METRICS/$OFFBOX2_METRIC.prom" ]

# 4b. set up: dump + rules on Blomp, size verified, login from .env, crypt from its file.
printf 'DB_HOST=localhost\nDB_PASSWORD="p=w=d"\nBLOMP_USER=ops@example.com\nBLOMP_PASSWORD="b=l=p"\n' > "$ROOT/configs/.env"
printf 'RCLONE_CONFIG_BLOMPCRYPT_TYPE=crypt\n' > "$ROOT/configs/blomp-backup.env"
reset_run
if run_backup; then pass "4b: run completes with the Blomp leg"; else fail "4b: run failed: $(tail -3 "$ROOT/logs/backup.log")"; fi
n2_dump=$(find "$REMOTE2/$(date -u +%Y/%m)" -name 'vaultaire_*.sql.gz' 2>/dev/null | wc -l | tr -d ' '); n2_rules=$(find "$REMOTE2" -name 'prometheus_*.tar.gz' | wc -l | tr -d ' '); n2_cfg=$(find "$REMOTE2" -name 'configs_*' | wc -l | tr -d ' ')
check "4b: dump + rules on Blomp under YYYY/MM, configs never (dump=$n2_dump rules=$n2_rules configs=$n2_cfg)" [ "$n2_dump/$n2_rules/$n2_cfg" = "1/1/0" ]
check "4b: still on Sync too" [ "$(find "$REMOTE" -name 'vaultaire_*.sql.gz' | wc -l | tr -d ' ')" = 1 ]
check "4b: rclone saw the login from .env (unquoted) and the crypt remote over blompbk:<user>/_ops/backups ($(cut -d'|' -f1,3,4 "$TMP/rclone2-env"))" [ "$(cat "$TMP/rclone2-env")" = "ops@example.com|b=l=p|blompbk:ops@example.com/_ops/backups|crypt" ]
check "4b: the Blomp password never appears in rclone's arguments" [ -z "$(grep -F 'b=l=p' "$TMP/rclone.log")" ]
f=$METRICS/$OFFBOX2_METRIC.prom
if [ -f "$f" ] && grep -qE "^$OFFBOX2_METRIC [0-9]{10}$" "$f"; then pass "4b: $OFFBOX2_METRIC written"; else fail "4b: $OFFBOX2_METRIC missing or malformed: $(cat "$f" 2>/dev/null)"; fi
check "4b: logged" grep -q 'Off-box copy 2 completed' "$ROOT/logs/backup.log"
check "4b: run timestamp written" [ -f "$METRICS/$RUN_METRIC.prom" ]

# 4c. BLOMP_CONTAINER, when set, names the container.
printf 'DB_PASSWORD=x\nBLOMP_USER=ops@example.com\nBLOMP_PASSWORD=pw\nBLOMP_CONTAINER=other\n' > "$ROOT/configs/.env"
reset_run; run_backup >/dev/null || true
check "4c: BLOMP_CONTAINER names the container ($(cut -d'|' -f3 "$TMP/rclone2-env"))" [ "$(cut -d'|' -f3 "$TMP/rclone2-env")" = "blompbk:other/_ops/backups" ]

# 4d. Blomp unreachable: logged, no offbox2 timestamp, and the run still completes.
printf 'DB_PASSWORD=x\nBLOMP_USER=ops@example.com\nBLOMP_PASSWORD=pw\n' > "$ROOT/configs/.env"
reset_run; touch "$TMP/rclone2-fails"
if run_backup 2>/dev/null; then pass "4d: a failing Blomp leg does not fail the run"; else fail "4d: run failed: $(tail -3 "$ROOT/logs/backup.log")"; fi
check "4d: logged as OFFBOX2 FAILED with its stage" grep -q 'OFFBOX2 FAILED - stage=offbox2: upload of vaultaire_' "$ROOT/logs/backup.log"
check "4d: no BACKUP FAILED line" [ -z "$(grep 'BACKUP FAILED' "$ROOT/logs/backup.log")" ]
check "4d: no offbox2 timestamp" [ ! -e "$METRICS/$OFFBOX2_METRIC.prom" ]
check "4d: Sync timestamp written" [ -f "$METRICS/vaultaire_backup_offbox_last_success_timestamp_seconds.prom" ]
check "4d: run timestamp written" [ -f "$METRICS/$RUN_METRIC.prom" ]

# 4e. .env without BLOMP_PASSWORD: logged, skipped, the run completes.
printf 'DB_PASSWORD=x\nBLOMP_USER=ops@example.com\n' > "$ROOT/configs/.env"
reset_run
if run_backup; then pass "4e: run completes"; else fail "4e: run failed"; fi
check "4e: logged" grep -q 'OFFBOX2 FAILED - stage=offbox2: BLOMP_PASSWORD not set' "$ROOT/logs/backup.log"

# 4f. Blomp retention by the date in the name, like Sync's.
printf 'DB_PASSWORD=x\nBLOMP_USER=ops@example.com\nBLOMP_PASSWORD=pw\n' > "$ROOT/configs/.env"
reset_run; old=$(date -u -d '-40 days' +%Y%m%d); mkdir -p "$REMOTE2/2026/01"
: > "$REMOTE2/2026/01/vaultaire_${old}_030001.sql.gz"
run_backup >/dev/null || true
check "4f: 40-day-old Blomp dump deleted by its name" [ ! -e "$REMOTE2/2026/01/vaultaire_${old}_030001.sql.gz" ]
check "4f: the fresh one kept" [ "$(find "$REMOTE2" -name 'vaultaire_*.sql.gz' | wc -l | tr -d ' ')" = 1 ]
UNDELETED2=vaultaire_backup_offbox2_retention_undeleted_files
check "4f: $UNDELETED2 = 0 after a clean pass" grep -qx "$UNDELETED2 0" "$METRICS/$UNDELETED2.prom"

# 4h. RP54-6: an old Blomp file that cannot be deleted never fails the run, but it is
#     counted where Prometheus reads it (the produce page's "up to 30 days" rests on it).
reset_run; old=$(date -u -d '-40 days' +%Y%m%d); mkdir -p "$REMOTE2/2026/01"
: > "$REMOTE2/2026/01/vaultaire_${old}_030001.sql.gz"; : > "$REMOTE2/2026/01/prometheus_${old}_030001.tar.gz"
touch "$TMP/rclone-deletefile-fails"
if run_backup 2>/dev/null; then pass "4h: a failed Blomp retention delete does not fail the run"; else fail "4h: run failed: $(tail -3 "$ROOT/logs/backup.log")"; fi
check "4h: logged" grep -q 'OFFBOX2 FAILED - stage=offbox2: 2 old file(s) could not be deleted' "$ROOT/logs/backup.log"
check "4h: $UNDELETED2 = 2 ($(grep "^$UNDELETED2 " "$METRICS/$UNDELETED2.prom" 2>/dev/null))" grep -qx "$UNDELETED2 2" "$METRICS/$UNDELETED2.prom"
check "4h: the Sync leg's count is 0" grep -qx "vaultaire_backup_offbox_retention_undeleted_files 0" "$METRICS/vaultaire_backup_offbox_retention_undeleted_files.prom"
check "4h: run timestamp written" [ -f "$METRICS/$RUN_METRIC.prom" ]
# 4i. the next run deletes them: the count goes back to 0.
rm -f "$TMP/rclone-deletefile-fails"; sleep 1; run_backup >/dev/null || true
check "4i: the next run deletes them and exports 0" grep -qx "$UNDELETED2 0" "$METRICS/$UNDELETED2.prom"
check "4i: gone" [ -z "$(find "$REMOTE2" -name "*_${old}_*")" ]

# 4j. RP55-1: a retention pass that LISTED its leg says so with a timestamp per leg; a
#     listing that fails writes none, so the undeleted-files gauge left from an older pass
#     is never read as fresh (BackupOffboxRetentionNotRunning).
RAN=vaultaire_backup_offbox_retention_last_run_timestamp_seconds
RAN2=vaultaire_backup_offbox2_retention_last_run_timestamp_seconds
reset_run; run_backup >/dev/null || true
for m in "$RAN" "$RAN2"; do
  f=$METRICS/$m.prom
  if [ -f "$f" ] && grep -qE "^$m [0-9]{10}$" "$f" && grep -q "^# TYPE $m gauge" "$f"; then pass "4j: $m written after a pass that listed its leg"; else fail "4j: $m missing or malformed: $(cat "$f" 2>/dev/null)"; fi
done
ran2_before=$(cat "$METRICS/$RAN2.prom" 2>/dev/null); ran_before=$(cat "$METRICS/$RAN.prom" 2>/dev/null)
sleep 1
# The Blomp listing fails (the upload works): the Blomp timestamp stays, the Sync one moves.
touch "$TMP/rclone2-lsf-fails"
rm -rf "${ROOT:?}/backups"/*; run_backup >/dev/null 2>&1 || true
rm -f "$TMP/rclone2-lsf-fails"
check "4j: a failed Blomp listing is logged" grep -q 'OFFBOX2 FAILED - stage=offbox2: listing blompcrypt: failed' "$ROOT/logs/backup.log"
check "4j: a failed Blomp listing writes no new $RAN2" [ "$(cat "$METRICS/$RAN2.prom" 2>/dev/null)" = "$ran2_before" ]
check "4j: the Sync leg's listing worked: $RAN moved" [ "$(cat "$METRICS/$RAN.prom" 2>/dev/null)" != "$ran_before" ]
# The Sync listing fails: the run fails at retention, no Sync timestamp.
sleep 1; ran_before=$(cat "$METRICS/$RAN.prom" 2>/dev/null)
rm -rf "${ROOT:?}/backups"/*; touch "$TMP/rclone-lsf-fails"; run_backup >/dev/null 2>&1 || true; rm -f "$TMP/rclone-lsf-fails"
check "4j: a failed Sync listing writes no new $RAN" [ "$(cat "$METRICS/$RAN.prom" 2>/dev/null)" = "$ran_before" ]
check "4j: BackupOffboxRetentionNotRunning rule" grep -q "alert: BackupOffboxRetentionNotRunning" "$RULES"
for m in "$RAN" "$RAN2"; do
  check "4j: the rule reads $m and its absence" grep -q "absent($m)" "$RULES"
done

# 4g. its alert rule.
check "4g: BackupSecondOffboxStale rule on $OFFBOX2_METRIC" grep -q "alert: BackupSecondOffboxStale" "$RULES"
check "4g: it covers absence" grep -q "absent($OFFBOX2_METRIC)" "$RULES"
check "4g: BackupOffboxRetentionStuck rule" grep -q "alert: BackupOffboxRetentionStuck" "$RULES"
for m in "$UNDELETED" "$UNDELETED2"; do
  check "4g: a rule reads $m" grep -q "$m > 0" "$RULES"
done
rm -f "$ROOT/configs/blomp-backup.env"

# --- 5. the dump is encrypted with age before it exists on disk (WP-P42, plan 43.7) -----
# BACKUP_AGE_RECIPIENTS = configs/backup-age-recipients.txt: the owner's offline public
# keys. Present = `pg_dump | gzip | age -R` and nothing plaintext under backups/; absent =
# today's plaintext dump, loud and counted (vaultaire_backup_dump_encrypted 0); present but
# unusable = the run fails at stage=encrypt, never a silent plaintext.
RCPT=$ROOT/configs/backup-age-recipients.txt
ENC=vaultaire_backup_dump_encrypted
DUMP_TS=vaultaire_backup_last_success_timestamp_seconds
printf 'RCLONE_CONFIG_BLOMPCRYPT_TYPE=crypt\n' > "$ROOT/configs/blomp-backup.env"
good_recipients() {
  printf '# the owner offline identities (public lines only)\n\nage1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqs3290gq\nage1yubikey1qwt50d05nh5vutpdzmlg5wn80xq5negm4uj9ghv0snvdd3yysf5yw3rhl3t\n' > "$RCPT"
}
plaintext_sql_on_disk() { # prints every file under backups/ that reads as SQL, raw or gunzipped
  local f
  while IFS= read -r f; do
    if grep -q 'CREATE TABLE' "$f" 2>/dev/null || { gzip -dc "$f" 2>/dev/null | grep -q 'CREATE TABLE'; }; then echo "$f"; fi
  done < <(find "$ROOT/backups" -type f)
}

# 5a. NoPlaintextDumpOnDisk: recipients present → the only dump is .sql.gz.age, no file
#     under backups/ reads as SQL, no partial or counter left, the off-box copies are the
#     ciphertext, the gauge says 1 — and the identity gets the dump back.
good_recipients; reset_run
if run_backup; then pass "5a NoPlaintextDumpOnDisk: run completes"; else fail "5a NoPlaintextDumpOnDisk: run failed: $(tail -3 "$ROOT/logs/backup.log")"; fi
n_age=$(find "$ROOT/backups" -name 'vaultaire_*.sql.gz.age' | wc -l | tr -d ' '); n_gz=$(find "$ROOT/backups" -name 'vaultaire_*.sql.gz' | wc -l | tr -d ' ')
check "5a NoPlaintextDumpOnDisk: one .sql.gz.age, no .sql.gz on the box (age=$n_age gz=$n_gz)" [ "$n_age/$n_gz" = "1/0" ]
leaked=$(plaintext_sql_on_disk)
check "5a NoPlaintextDumpOnDisk: no file under backups/ reads as SQL (found: ${leaked:-none})" [ -z "$leaked" ]
check "5a NoPlaintextDumpOnDisk: no partial or counter file left ($(find "$ROOT/backups" -name '.*' ! -name .lock | tr '\n' ' '))" [ -z "$(find "$ROOT/backups" -name '.*' ! -name .lock)" ]
check "5a NoPlaintextDumpOnDisk: age was given the recipients file with -R" grep -q -- "-R $RCPT" "$TMP/age.log"
n_off=$(find "$REMOTE" -name 'vaultaire_*.sql.gz.age' | wc -l | tr -d ' '); n_off2=$(find "$REMOTE2" -name 'vaultaire_*.sql.gz.age' | wc -l | tr -d ' ')
check "5a NoPlaintextDumpOnDisk: Sync and Blomp hold the ciphertext (sync=$n_off blomp=$n_off2)" [ "$n_off/$n_off2" = "1/1" ]
check "5a NoPlaintextDumpOnDisk: no plaintext dump went off-box" [ -z "$(find "$REMOTE" "$REMOTE2" -name 'vaultaire_*.sql.gz')" ]
check "5a NoPlaintextDumpOnDisk: $ENC = 1 ($(grep "^$ENC " "$METRICS/$ENC.prom" 2>/dev/null))" grep -qx "$ENC 1" "$METRICS/$ENC.prom"
check "5a NoPlaintextDumpOnDisk: $DUMP_TS written (the asserts ran on the stream)" [ -f "$METRICS/$DUMP_TS.prom" ]
check "5a NoPlaintextDumpOnDisk: the log names the shape and the age version" grep -q 'encrypted with age to 2 recipient(s), age v1.3.2-fake' "$ROOT/logs/backup.log"
echo FAKE-IDENTITY > "$TMP/identity"; enc_file=$(find "$ROOT/backups" -name 'vaultaire_*.sql.gz.age' | head -1)
restored=$(PATH="$BIN:$PATH" age -d -i "$TMP/identity" "${enc_file:-/dev/null}" 2>/dev/null | gzip -dc 2>/dev/null | grep -c 'CREATE TABLE' || true)
check "5a NoPlaintextDumpOnDisk: the identity restores it (age -d | gunzip has the DDL: $restored)" [ "${restored:-0}" = 1 ]
echo WRONG > "$TMP/identity"
# shellcheck disable=SC2016 # expanded by the inner bash
check "5a NoPlaintextDumpOnDisk: without the identity it does not decrypt" bash -c '! PATH="$1:$PATH" age -d -i "$2" "$3" >/dev/null 2>&1' _ "$BIN" "$TMP/identity" "${enc_file:-/dev/null}"

# 5b. RecipientsAbsentIsLoudAndCounted: no file → today's plaintext dump, a loud line, the
#     gauge 0 (BackupDumpPlaintext), and the run still completes.
rm -f "$RCPT"; reset_run
if run_backup; then pass "5b RecipientsAbsentIsLoudAndCounted: run completes"; else fail "5b RecipientsAbsentIsLoudAndCounted: run failed: $(tail -3 "$ROOT/logs/backup.log")"; fi
check "5b RecipientsAbsentIsLoudAndCounted: the dump is .sql.gz" [ "$(find "$ROOT/backups" -name 'vaultaire_*.sql.gz' | wc -l | tr -d ' ')" = 1 ]
check "5b RecipientsAbsentIsLoudAndCounted: age never called" [ ! -e "$TMP/age.log" ]
check "5b RecipientsAbsentIsLoudAndCounted: logged loudly" grep -q "WARNING - stage=encrypt: $RCPT not present: the dump is written in PLAINTEXT" "$ROOT/logs/backup.log"
check "5b RecipientsAbsentIsLoudAndCounted: $ENC = 0 ($(grep "^$ENC " "$METRICS/$ENC.prom" 2>/dev/null))" grep -qx "$ENC 0" "$METRICS/$ENC.prom"
check "5b RecipientsAbsentIsLoudAndCounted: it is a gauge with its help line" grep -q "^# TYPE $ENC gauge" "$METRICS/$ENC.prom"
check "5b RecipientsAbsentIsLoudAndCounted: run timestamp written" [ -f "$METRICS/$RUN_METRIC.prom" ]

# 5c. BrokenRecipientsFailsTheRun: present but unusable → BACKUP FAILED stage=encrypt
#     before pg_dump runs: no dump of either shape, no dump timestamp, no gauge.
broken_case() { # name, recipients content ('' = keep the file as is), extra env assignment
  local name=$1 content=$2 extra=${3:-}
  [ -z "$content" ] || printf '%b' "$content" > "$RCPT"
  reset_run
  if env ${extra:+"$extra"} PATH="$BIN:$PATH" PG_BACKUP_ROOT="$ROOT" PG_BACKUP_PROM_DIR="$PROM" PG_BACKUP_METRICS_DIR="$METRICS" bash "$SCRIPT" 2>/dev/null; then
    fail "5c BrokenRecipientsFailsTheRun ($name): the run must fail"
  else
    pass "5c BrokenRecipientsFailsTheRun ($name): the run fails"
  fi
  check "5c BrokenRecipientsFailsTheRun ($name): logged at stage=encrypt ($(grep 'BACKUP FAILED' "$ROOT/logs/backup.log" | tail -1 | cut -c1-140))" grep -q 'BACKUP FAILED - stage=encrypt' "$ROOT/logs/backup.log"
  check "5c BrokenRecipientsFailsTheRun ($name): pg_dump never ran" [ ! -e "$TMP/seen-password" ]
  check "5c BrokenRecipientsFailsTheRun ($name): no dump of either shape" [ -z "$(find "$ROOT/backups" -name 'vaultaire_*')" ]
  check "5c BrokenRecipientsFailsTheRun ($name): no dump timestamp" [ ! -e "$METRICS/$DUMP_TS.prom" ]
  check "5c BrokenRecipientsFailsTheRun ($name): no gauge" [ ! -e "$METRICS/$ENC.prom" ]
}
broken_case "a malformed line" 'age1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqs3290gq\nnot-a-recipient\n'
broken_case "an identity on the box" 'AGE-SECRET-KEY-1QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQ\n'
check "5c BrokenRecipientsFailsTheRun (an identity on the box): the secret is never logged" [ -z "$(grep -F 'AGE-SECRET-KEY-1QQ' "$ROOT/logs/backup.log")" ]
broken_case "a plugin identity line" 'AGE-PLUGIN-YUBIKEY-1QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQ\n'
check "5c BrokenRecipientsFailsTheRun (a plugin identity line): says it is an identities file" grep -q 'this is an identities file, not a recipients file' "$ROOT/logs/backup.log"
broken_case "comments only" '# nobody yet\n\n'
broken_case "age refuses the file" 'age1refusedbyage\n'
broken_case "post-quantum and classic mixed" 'age1pq1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqs\nage1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqs3290gq\n'
rm -f "$RCPT"; ln -s "$TMP/no-such-recipients" "$RCPT"
broken_case "a dangling symlink (RP62-4)" ''
rm -f "$RCPT"
good_recipients
broken_case "no age binary" '' "PG_BACKUP_AGE_BIN=$TMP/no-such-age"
chmod 0000 "$RCPT"
if [ -r "$RCPT" ]; then echo "SKIPPED 5c (unreadable): running as root"; else broken_case "unreadable" ''; fi
chmod 0600 "$RCPT"

# 5d. RetentionRetiresBothShapes: on the box by mtime, off it by the date in the name —
#     a 10-day-old .sql.gz AND .sql.gz.age go from the box, 40-day-old names of both from
#     Sync and Blomp; the fresh ones stay.
good_recipients; reset_run
old_box=$(date -u -d '-10 days' +%Y%m%d); old=$(date -u -d '-40 days' +%Y%m%d)
for ext in sql.gz sql.gz.age; do
  : > "$ROOT/backups/vaultaire_${old_box}_030001.$ext"; touch -d '-10 days' "$ROOT/backups/vaultaire_${old_box}_030001.$ext"
  mkdir -p "$REMOTE/_ops/backups/2026/01" "$REMOTE2/2026/01"
  : > "$REMOTE/_ops/backups/2026/01/vaultaire_${old}_030001.$ext"; : > "$REMOTE2/2026/01/vaultaire_${old}_030001.$ext"
done
run_backup >/dev/null 2>&1 || true
check "5d RetentionRetiresBothShapes: old .sql.gz and .sql.gz.age gone from the box ($(find "$ROOT/backups" -name "*_${old_box}_*" | tr '\n' ' '))" [ -z "$(find "$ROOT/backups" -name "*_${old_box}_*")" ]
check "5d RetentionRetiresBothShapes: both old names gone from Sync and Blomp" [ -z "$(find "$REMOTE" "$REMOTE2" -name "*_${old}_*")" ]
check "5d RetentionRetiresBothShapes: today's dump kept on the box" [ "$(find "$ROOT/backups" -name 'vaultaire_*.sql.gz.age' | wc -l | tr -d ' ')" = 1 ]

# 5e. the asserts run on the stream: a pg_dump that fails mid-stream, or a dump with no
#     DDL, fails the run at stage=dump and leaves no file (pipefail; nothing to roll back).
reset_run; touch "$TMP/pg_dump-fails"
if run_backup 2>/dev/null; then fail "5e: a failed pg_dump must fail the run"; else pass "5e: a failed pg_dump fails the run"; fi
check "5e: logged at stage=dump naming pg_dump ($(grep 'BACKUP FAILED' "$ROOT/logs/backup.log" | tail -1 | cut -c1-140))" grep -q 'BACKUP FAILED - stage=dump: pg_dump failed' "$ROOT/logs/backup.log"
check "5e: no dump file and no partial left" [ -z "$(find "$ROOT/backups" -name 'vaultaire_*' -o -name '.dump*')" ]
reset_run; touch "$TMP/pg_dump-no-ddl"
if run_backup 2>/dev/null; then fail "5e: a dump with no DDL must fail the run"; else pass "5e: a dump with no DDL fails the run"; fi
check "5e: logged as no CREATE TABLE" grep -q 'BACKUP FAILED - stage=dump: dump contains no CREATE TABLE statements' "$ROOT/logs/backup.log"
check "5e: no dump file and no partial left (no DDL)" [ -z "$(find "$ROOT/backups" -name 'vaultaire_*' -o -name '.dump*')" ]
check "5e: no dump timestamp" [ ! -e "$METRICS/$DUMP_TS.prom" ]

# 5h. TooSmallDumpFailsWhenSealed (RP62-2): a near-empty dump fails the SIZE assert even
#     though its sealed file is bigger than 1 KB (the size is counted on the gzip stream,
#     before age); no file of either shape is left.
good_recipients; reset_run; touch "$TMP/pg_dump-tiny"
if run_backup 2>/dev/null; then fail "5h TooSmallDumpFailsWhenSealed: a near-empty dump must fail the run"; else pass "5h TooSmallDumpFailsWhenSealed: the run fails"; fi
check "5h TooSmallDumpFailsWhenSealed: logged as too small ($(grep 'BACKUP FAILED' "$ROOT/logs/backup.log" | tail -1 | cut -c1-140))" grep -q 'BACKUP FAILED - stage=dump: dump too small' "$ROOT/logs/backup.log"
check "5h TooSmallDumpFailsWhenSealed: no dump file and no partial left" [ -z "$(find "$ROOT/backups" -name 'vaultaire_*' -o -name '.dump*')" ]
check "5h TooSmallDumpFailsWhenSealed: no dump timestamp" [ ! -e "$METRICS/$DUMP_TS.prom" ]

# 5i. OneRunAtATime (RP62-5): while another run holds the lock, a run fails at stage=lock
#     before it touches anything (its sweep of .dump-* would delete the other's work dir).
good_recipients; reset_run; mkdir -p "$ROOT/backups/.dump-other"
exec 8>"$ROOT/backups/.lock"; PATH="$BIN:$PATH" flock -n 8
if run_backup 2>/dev/null; then fail "5i OneRunAtATime: a run must fail while the lock is held"; else pass "5i OneRunAtATime: the run fails"; fi
exec 8>&-
check "5i OneRunAtATime: logged at stage=lock" grep -q 'BACKUP FAILED - stage=lock: another pg-backup.sh run holds' "$ROOT/logs/backup.log"
check "5i OneRunAtATime: the other run's work directory is untouched" [ -d "$ROOT/backups/.dump-other" ]
check "5i OneRunAtATime: pg_dump never ran" [ ! -e "$TMP/seen-password" ]
rm -rf "$ROOT/backups/.dump-other"
if run_backup; then pass "5i OneRunAtATime: the lock released, the next run completes"; else fail "5i OneRunAtATime: run failed: $(tail -3 "$ROOT/logs/backup.log")"; fi

# 5j. age-install.sh refuses a tarball that is not the pin and installs nothing.
echo "not age" > "$TMP/fake-age.tar.gz"; mkdir -p "$TMP/age-dest"
if AGE_TARBALL="$TMP/fake-age.tar.gz" bash "$HERE/age-install.sh" "$TMP/age-dest" 2>"$TMP/age-install.err"; then fail "5j: age-install.sh must refuse a tarball that is not the pin"; else pass "5j: age-install.sh refuses a wrong tarball"; fi
check "5j: it says sha256 mismatch" grep -q 'sha256 mismatch' "$TMP/age-install.err"
check "5j: nothing installed" [ -z "$(ls -A "$TMP/age-dest")" ]

# 5f. the rule.
check "5f: BackupDumpPlaintext rule" grep -q "alert: BackupDumpPlaintext" "$RULES"
check "5f: it reads $ENC == 0" grep -q "$ENC == 0" "$RULES"

# 5g. with the real age (when installed; CI installs the pin, RP62-3 — there a missing age
#     is a failure, never a skip): a throwaway identity decrypts what the run wrote, an
#     age1pq recipient works, and post-quantum mixed with classic is refused at stage=encrypt.
if REAL_AGE=$(command -v age) && command -v age-keygen >/dev/null; then
  age-keygen -o "$TMP/real-identity" 2>/dev/null; age-keygen -y "$TMP/real-identity" > "$RCPT"
  reset_run
  if env PG_BACKUP_AGE_BIN="$REAL_AGE" PATH="$BIN:$PATH" PG_BACKUP_ROOT="$ROOT" PG_BACKUP_PROM_DIR="$PROM" PG_BACKUP_METRICS_DIR="$METRICS" bash "$SCRIPT"; then pass "5g: run with the real age completes"; else fail "5g: run failed: $(tail -3 "$ROOT/logs/backup.log")"; fi
  enc_file=$(find "$ROOT/backups" -name 'vaultaire_*.sql.gz.age' | head -1)
  # shellcheck disable=SC2016 # expanded by the inner bash
  check "5g: the real age file decrypts with the identity to the dump" bash -c '"$1" -d -i "$2" "$3" | gzip -dc | grep -q "CREATE TABLE"' _ "$REAL_AGE" "$TMP/real-identity" "${enc_file:-/dev/null}"
  check "5g: no file under backups/ reads as SQL" [ -z "$(plaintext_sql_on_disk)" ]
  check "5g: the log names the real age's version ($("$REAL_AGE" --version))" grep -qF "age $("$REAL_AGE" --version))" "$ROOT/logs/backup.log"
  if age-keygen -pq -o "$TMP/real-pq-identity" 2>/dev/null; then
    { age-keygen -y "$TMP/real-pq-identity"; age-keygen -y "$TMP/real-identity"; } > "$RCPT"
    broken_case "real age: post-quantum and classic mixed" '' "PG_BACKUP_AGE_BIN=$REAL_AGE"
    age-keygen -y "$TMP/real-pq-identity" > "$RCPT"; reset_run
    if env PG_BACKUP_AGE_BIN="$REAL_AGE" PATH="$BIN:$PATH" PG_BACKUP_ROOT="$ROOT" PG_BACKUP_PROM_DIR="$PROM" PG_BACKUP_METRICS_DIR="$METRICS" bash "$SCRIPT"; then pass "5g: run with a real age1pq recipient completes"; else fail "5g: age1pq run failed: $(tail -3 "$ROOT/logs/backup.log")"; fi
    enc_file=$(find "$ROOT/backups" -name 'vaultaire_*.sql.gz.age' | head -1)
    # shellcheck disable=SC2016 # expanded by the inner bash
    check "5g: the age1pq file decrypts with its identity" bash -c '"$1" -d -i "$2" "$3" | gzip -dc | grep -q "CREATE TABLE"' _ "$REAL_AGE" "$TMP/real-pq-identity" "${enc_file:-/dev/null}"
  else
    fail "5g: age-keygen -pq failed: the installed age ($("$REAL_AGE" --version)) has no post-quantum keys — install the pin (deploy/scripts/age-install.sh)"
  fi
elif [ "${CI:-}" = true ]; then
  fail "5g: the real age/age-keygen are not installed in CI (the Deploy scripts step installs the pin)"
else
  echo "SKIPPED 5g: the real age/age-keygen are not installed"
fi
rm -f "$RCPT" "$ROOT/configs/blomp-backup.env"

exit $FAILED
