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
#  6. Stops it and deletes it, pass or fail (PITR_DRILL_KEEP=1 keeps a failed one) — on
#     EXIT, SIGINT and SIGTERM alike (RP58-9 a: an interrupted drill used to leave the
#     scratch postmaster on 5499 and its directory, and the next drill refused).
#  7. Writes its textfile metrics, PASS or FAIL or cut (Stage 1a Prompt 41 item 4,
#     RP57-13; the monthly vaultaire-pitr-drill.timer runs it):
#     PITR_METRICS_DIR/vaultaire_pitr_drill.prom (atomic, 0644; its own file — the
#     minute writer pg-pitr-metrics.sh never touches it):
#       vaultaire_pg_pitr_drill_last_run_timestamp_seconds      this run's end
#       vaultaire_pg_pitr_drill_success                         1 = this run passed
#       vaultaire_pg_pitr_drill_last_success_timestamp_seconds  the newest PASS (kept
#                                                               from the previous file
#                                                               across a FAIL; 0 = never)
#       vaultaire_pg_pitr_drill_rto_seconds                     the newest PASS's RTO
#     Rules: PGPITRDrillStale (no pass in 35 d or no file), PGPITRDrillFailed.
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
METRICS_DIR=${PITR_METRICS_DIR:-/var/lib/vaultaire-pitr}
METRICS=$METRICS_DIR/vaultaire_pitr_drill.prom

STARTED=0
RESULT=FAIL
RTO_S=0
CLEANED=0

as_pg() { runuser -u "$PG_USER" -- "$@"; }
live_sql() { as_pg psql -X -At -v ON_ERROR_STOP=1 -d "$1" -c "$2"; }
drill_sql() { as_pg psql -X -At -v ON_ERROR_STOP=1 -h "$DRILL_DIR" -p "$DRILL_PORT" -d "$1" -c "$2"; }
now_ms() { date +%s%3N; }
secs() { printf '%d.%03d' $(( $1 / 1000 )) $(( $1 % 1000 )); }
say() { echo "$(date -u +%H:%M:%S) $*"; }
fail() { say "PITR DRILL FAIL: $*"; exit 1; }

# write_metrics — the drill's textfile, on every end. A FAIL keeps the previous PASS's
# timestamp and RTO (read from the file this run replaces), so PGPITRDrillStale measures
# the time since the last PASS and PGPITRDrillFailed says this run failed.
write_metrics() {
  local now success=0 last_success=0 rto=0 prev tmp
  now=$(date +%s)
  if [ -r "$METRICS" ]; then
    prev=$(sed -n 's/^vaultaire_pg_pitr_drill_last_success_timestamp_seconds //p' "$METRICS" | head -1)
    [[ "$prev" =~ ^[0-9]+$ ]] && last_success=$prev
    prev=$(sed -n 's/^vaultaire_pg_pitr_drill_rto_seconds //p' "$METRICS" | head -1)
    [[ "$prev" =~ ^[0-9.]+$ ]] && rto=$prev
  fi
  if [ "$RESULT" = PASS ]; then success=1 last_success=$now rto=$RTO_S; fi
  [ -d "$METRICS_DIR" ] || return 0
  tmp=$(mktemp "$METRICS_DIR/.vaultaire_pitr_drill.XXXXXX") || return 0
  {
    printf '# HELP vaultaire_pg_pitr_drill_last_run_timestamp_seconds unix time the last point-in-time recovery drill ended (pg-pitr-drill.sh)\n# TYPE vaultaire_pg_pitr_drill_last_run_timestamp_seconds gauge\nvaultaire_pg_pitr_drill_last_run_timestamp_seconds %s\n' "$now"
    printf '# HELP vaultaire_pg_pitr_drill_success 1 when the last drill passed, 0 when it failed or was cut\n# TYPE vaultaire_pg_pitr_drill_success gauge\nvaultaire_pg_pitr_drill_success %s\n' "$success"
    printf '# HELP vaultaire_pg_pitr_drill_last_success_timestamp_seconds unix time the newest drill PASSED (0 = never)\n# TYPE vaultaire_pg_pitr_drill_last_success_timestamp_seconds gauge\nvaultaire_pg_pitr_drill_last_success_timestamp_seconds %s\n' "$last_success"
    printf '# HELP vaultaire_pg_pitr_drill_rto_seconds restore start to promoted, of the newest PASS\n# TYPE vaultaire_pg_pitr_drill_rto_seconds gauge\nvaultaire_pg_pitr_drill_rto_seconds %s\n' "$rto"
  } > "$tmp" || { rm -f "$tmp"; return 0; }
  if ! { chmod 0644 "$tmp" && mv -f "$tmp" "$METRICS"; }; then rm -f "$tmp"; fi
}

cleanup() {
  local rc=$?
  cleanup_work
  exit "$rc"
}

# cleanup_work — stop and delete the scratch cluster, write the metrics; once.
cleanup_work() {
  if [ "$CLEANED" = 1 ]; then return 0; fi
  CLEANED=1
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
  write_metrics
}

# on_signal — a cut drill cleans up like a failed one, then dies of the signal.
# shellcheck disable=SC2317 # reached through the trap
on_signal() {
  local sig=$1
  say "PITR DRILL FAIL: cut by SIG$sig"
  RESULT=FAIL
  trap - EXIT
  cleanup_work || true
  trap - "$sig"
  kill -s "$sig" $$
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
  trap 'on_signal INT' INT
  trap 'on_signal TERM' TERM
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
  # live data directory, port and pid file — none of it is used. The restored
  # postgresql.auto.conf is PROD's (the ALTER SYSTEM tuning: shared_buffers 2GB,
  # max_wal_size 8GB …) plus what pgbackrest wrote for the recovery (restore_command,
  # recovery_target_*), and auto.conf is read AFTER postgresql.conf — so the scratch
  # overrides are APPENDED to it, last assignment wins, never truncating it (RP58-6:
  # the scratch cluster started prod-sized). The safety settings ride the pg_ctl -o
  # command line too, which outranks every file.
  local scratch_conf
  scratch_conf="# pg-pitr-drill.sh scratch cluster — deleted when the drill ends
port = $DRILL_PORT
listen_addresses = ''
unix_socket_directories = '$DRILL_DIR'
archive_mode = off
archive_command = ''
ssl = off
shared_buffers = 256MB
max_wal_size = 1GB
max_connections = $mc
max_worker_processes = $mw
max_wal_senders = $ms
max_prepared_transactions = $mp
max_locks_per_transaction = $ml
cluster_name = 'pitr-drill'"
  printf '%s\n' "$scratch_conf" | as_pg tee "$DRILL_DIR/postgresql.conf" >/dev/null
  printf '\n# pg-pitr-drill.sh: the scratch overrides, appended after the restored settings (last wins)\n%s\n' "$scratch_conf" \
    | as_pg tee -a "$DRILL_DIR/postgresql.auto.conf" >/dev/null
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
  RTO_S=$(secs $(( t3 - t0 )))
  say "PITR DRILL PASS target=$t_iso restore_s=$(secs $(( t1 - t0 ))) accept_s=$(secs $(( t2 - t0 ))) rto_s=$RTO_S ohc_restored=$ohc ohc_live_at_t=$ohc_at_t"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
