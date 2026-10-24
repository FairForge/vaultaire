#!/bin/bash
# pg-pitr-drill.sh — proves point-in-time recovery on the box (Stage 1a Prompt 40), without
# touching the live cluster's data: a restore to a moment T into a SCRATCH cluster.
#
#   sudo /usr/local/lib/vaultaire-pitr/pg-pitr-drill.sh
#
#  1. In the scratch database `pitr_canary` of the live cluster: a row "before" (T₁),
#     the moment T (the server's clock), the live object_head_cache count at T, a row
#     "after" (T₂ > T); then `pgbackrest check` — it switches WAL and waits until the
#     segment holding T₂ is archived, so the restore has a commit past T to stop at.
#  2. `pgbackrest restore --type=time --target=T` into PITR_DRILL_DIR (its own data
#     directory, never the live one; refused if it exists), --archive-mode=off and
#     --target-action=promote.
#  3. Starts it on its own port (5499), on a Unix socket in its own directory and no TCP
#     listener, archive_mode=off on the command line too: what writes by itself between
#     the disaster and the tool (RP42-1)? Here: nothing reaches the repository — a
#     restored cluster that archived would push a new timeline into the live stanza.
#  4. Checks: the "before" row present, the "after" row absent; object_head_cache's count
#     equals the live count read at T, within PITR_DRILL_OHC_SLACK rows (writes between
#     reading T and counting).
#  5. Reports the RTO: restore start → accepting connections → promoted (read-write).
#  6. Stops it and deletes it, pass or fail (PITR_DRILL_KEEP=1 keeps a failed one).
#
# The last line is `PITR DRILL PASS …` or `PITR DRILL FAIL …`; the exit status says the
# same. Tests: pg-pitr_test.sh (every external binary faked).
set -euo pipefail

STANZA=${PITR_STANZA:-vaultaire}
LIVE_DIR=${PITR_LIVE_DIR:-/var/lib/postgresql/18/main}
DRILL_DIR=${PITR_DRILL_DIR:-/var/lib/postgresql/18/pitr-drill}
DRILL_PORT=${PITR_DRILL_PORT:-5499}
PG_BIN=${PITR_PG_BIN:-/usr/lib/postgresql/18/bin}
PG_USER=${PITR_PG_USER:-postgres}
LIVE_DB=${PITR_LIVE_DB:-vaultaire}
GAP=${PITR_DRILL_GAP:-2}                 # seconds between T₁, T and T₂
TIMEOUT=${PITR_DRILL_TIMEOUT:-1800}      # seconds to wait for the scratch cluster
SLACK=${PITR_DRILL_OHC_SLACK:-25}
KEEP=${PITR_DRILL_KEEP:-0}

STARTED=0
RESULT=FAIL

as_pg() { runuser -u "$PG_USER" -- "$@"; }
live_sql() { as_pg psql -X -At -v ON_ERROR_STOP=1 -d "$1" -c "$2"; }
drill_sql() { as_pg psql -X -At -v ON_ERROR_STOP=1 -h "$DRILL_DIR" -p "$DRILL_PORT" -d "$1" -c "$2"; }
now_ms() { date +%s%3N; }
secs() { printf '%d.%03d' $(( $1 / 1000 )) $(( $1 % 1000 )); }
say() { echo "$(date -u +%H:%M:%S) $*"; }
fail() { say "PITR DRILL FAIL: $*"; exit 1; }

cleanup() {
  local rc=$?
  if [ "$STARTED" = 1 ]; then
    as_pg "$PG_BIN/pg_ctl" -D "$DRILL_DIR" -m fast -w stop >/dev/null 2>&1 \
      || as_pg "$PG_BIN/pg_ctl" -D "$DRILL_DIR" -m immediate -w stop >/dev/null 2>&1 || true
    say "scratch cluster stopped"
  fi
  if [ "$RESULT" != PASS ] && [ -s "$DRILL_DIR.log" ]; then
    say "the scratch cluster's log (last lines):"; tail -n 15 "$DRILL_DIR.log" || true
  fi
  if [ "$RESULT" = PASS ] || [ "$KEEP" != 1 ]; then
    if [ -d "$DRILL_DIR" ] && [ "$DRILL_DIR" != "$LIVE_DIR" ]; then
      rm -rf "${DRILL_DIR:?}"
      say "scratch cluster deleted ($DRILL_DIR)"
    fi
    rm -f "$DRILL_DIR.log"
  fi
  exit "$rc"
}

# refuse — the scratch directory must be a NEW directory that is not the live cluster's
# and is not inside it, and the live cluster must be what we think it is.
refuse_unsafe() {
  local d=${DRILL_DIR%/} l=${LIVE_DIR%/}
  if [ -z "$d" ] || [ "${d:0:1}" != / ]; then fail "PITR_DRILL_DIR must be an absolute path"; fi
  case "$d/" in "$l/"*) fail "PITR_DRILL_DIR $d is the live data directory or inside it" ;; esac
  case "$l/" in "$d/"*) fail "PITR_DRILL_DIR $d contains the live data directory" ;; esac
  [ ! -e "$d" ] || fail "$d exists: a drill restores into a new directory only (a failed drill kept with PITR_DRILL_KEEP=1? remove it first)"
}

main() {
  refuse_unsafe
  trap cleanup EXIT
  local run t_iso ohc_at_t params mc mw ms mp ml
  run=$(date -u +%Y%m%dT%H%M%S)-$$
  say "drill $run: stanza $STANZA, live $LIVE_DIR, scratch $DRILL_DIR on port $DRILL_PORT"

  # 1. the canary rows and the moment T
  if [ "$(live_sql postgres "SELECT 1 FROM pg_database WHERE datname = 'pitr_canary'")" != 1 ]; then
    live_sql postgres "CREATE DATABASE pitr_canary" >/dev/null
  fi
  live_sql pitr_canary "CREATE TABLE IF NOT EXISTS canary (id bigserial PRIMARY KEY, tag text NOT NULL, at timestamptz NOT NULL DEFAULT clock_timestamp())" >/dev/null
  live_sql pitr_canary "INSERT INTO canary (tag) VALUES ('$run-before')" >/dev/null
  sleep "$GAP"
  t_iso=$(live_sql postgres "SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS.US') || '+00'")
  ohc_at_t=$(live_sql "$LIVE_DB" "SELECT count(*) FROM object_head_cache")
  say "T = $t_iso (object_head_cache at T: $ohc_at_t rows)"
  sleep "$GAP"
  live_sql pitr_canary "INSERT INTO canary (tag) VALUES ('$run-after')" >/dev/null
  as_pg pgbackrest --stanza="$STANZA" check >/dev/null || fail "pgbackrest check failed: the WAL past T is not archived"
  say "WAL past T archived (pgbackrest check)"

  # The scratch cluster must allow at least the live values of the settings a hot standby
  # checks against the WAL, or recovery stops.
  params=$(live_sql postgres "SELECT current_setting('max_connections') || '|' || current_setting('max_worker_processes') || '|' || current_setting('max_wal_senders') || '|' || current_setting('max_prepared_transactions') || '|' || current_setting('max_locks_per_transaction')")
  IFS='|' read -r mc mw ms mp ml <<< "$params"

  # 2. restore
  local t0 t1 t2 t3
  as_pg mkdir -m 0700 "$DRILL_DIR"
  t0=$(now_ms)
  as_pg pgbackrest --stanza="$STANZA" --pg1-path="$DRILL_DIR" --type=time "--target=$t_iso" \
    --target-action=promote --archive-mode=off restore || fail "pgbackrest restore failed"
  t1=$(now_ms)
  say "restored into $DRILL_DIR in $(secs $(( t1 - t0 ))) s"

  # 3. its own configuration: the live one lives in /etc/postgresql (Debian) and names the
  # live data directory, port and pid file — none of it is used.
  as_pg tee "$DRILL_DIR/postgresql.conf" >/dev/null <<EOF
# pg-pitr-drill.sh scratch cluster — deleted when the drill ends
port = $DRILL_PORT
listen_addresses = ''
unix_socket_directories = '$DRILL_DIR'
archive_mode = off
archive_command = ''
ssl = off
shared_buffers = 256MB
max_connections = $mc
max_worker_processes = $mw
max_wal_senders = $ms
max_prepared_transactions = $mp
max_locks_per_transaction = $ml
cluster_name = 'pitr-drill'
EOF
  printf 'local all %s peer\n' "$PG_USER" | as_pg tee "$DRILL_DIR/pg_hba.conf" >/dev/null
  : | as_pg tee "$DRILL_DIR/pg_ident.conf" >/dev/null

  STARTED=1
  as_pg "$PG_BIN/pg_ctl" -D "$DRILL_DIR" -l "$DRILL_DIR.log" -W \
    -o "-c port=$DRILL_PORT -c archive_mode=off -c listen_addresses= -c unix_socket_directories=$DRILL_DIR" start >/dev/null \
    || fail "pg_ctl start failed"
  local deadline=$(( $(date +%s) + TIMEOUT ))
  until as_pg "$PG_BIN/pg_isready" -q -h "$DRILL_DIR" -p "$DRILL_PORT"; do
    [ "$(date +%s)" -lt "$deadline" ] || fail "the scratch cluster did not accept connections in $TIMEOUT s"
    sleep 0.2
  done
  t2=$(now_ms)
  until [ "$(drill_sql postgres "SELECT pg_is_in_recovery()" 2>/dev/null)" = f ]; do
    [ "$(date +%s)" -lt "$deadline" ] || fail "the scratch cluster did not finish recovery in $TIMEOUT s"
    sleep 0.2
  done
  t3=$(now_ms)
  say "accepting connections $(secs $(( t2 - t1 ))) s after the restore, promoted $(secs $(( t3 - t1 ))) s after it"

  # 4. what it holds
  local before after ohc diff
  IFS='|' read -r before after <<< "$(drill_sql pitr_canary "SELECT count(*) FILTER (WHERE tag = '$run-before'), count(*) FILTER (WHERE tag = '$run-after') FROM canary")"
  [ "$before" = 1 ] || fail "the row written before T is missing (found $before)"
  [ "$after" = 0 ] || fail "the row written after T is present (found $after): the restore did not stop at T"
  say "canary: before-T row present, after-T row absent"
  ohc=$(drill_sql "$LIVE_DB" "SELECT count(*) FROM object_head_cache")
  diff=$(( ohc - ohc_at_t )); [ "$diff" -ge 0 ] || diff=$(( -diff ))
  [ "$diff" -le "$SLACK" ] || fail "object_head_cache: $ohc rows restored, $ohc_at_t live at T (|diff| $diff > $SLACK)"
  say "object_head_cache: $ohc rows restored, $ohc_at_t live at T"

  RESULT=PASS
  say "PITR DRILL PASS target=$t_iso restore_s=$(secs $(( t1 - t0 ))) accept_s=$(secs $(( t2 - t0 ))) rto_s=$(secs $(( t3 - t0 ))) ohc_restored=$ohc ohc_live_at_t=$ohc_at_t"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
