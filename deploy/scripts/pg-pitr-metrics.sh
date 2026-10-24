#!/bin/bash
# pg-pitr-metrics.sh — the point-in-time-recovery textfile metrics (Stage 1a Prompt 40).
# Run every minute as postgres by vaultaire-pitr-metrics.timer (pg-pitr-install.sh), from
# /usr/local/lib/vaultaire-pitr/. Writes ONE file, PITR_METRICS_DIR/vaultaire_pitr.prom
# (atomic rename, 0644), read by node_exporter through a root-owned symlink in its textfile
# directory. Rules: deploy/monitoring/vaultaire-pitr.yml.
#
# Every series is written on every run, at 0 when there is nothing to report (the counter
# lesson: a series born at its first event never pages), and the two *_success gauges say
# whether its source answered — a source that does not answer reports 0s, which the
# staleness rules read as "not archived / not backed up", never as fresh:
#   from pg_stat_archiver (psql over the local socket):
#     vaultaire_pg_archive_stats_success                    1 = the view was read
#     vaultaire_pg_archive_archived_total / _failed_total   its counters
#     vaultaire_pg_archive_last_archived_timestamp_seconds  last WAL segment archived (0 = never)
#     vaultaire_pg_archive_last_failed_timestamp_seconds    last failed attempt (0 = never)
#   from `pgbackrest info --output=json`:
#     vaultaire_pg_pitr_info_success                        1 = the repository answered
#     vaultaire_pg_pitr_last_full_timestamp_seconds         newest full backup's end
#     vaultaire_pg_pitr_last_diff_timestamp_seconds         newest differential OR full
#                                                           (a full is a superset: Sunday has
#                                                           no diff by design)
#     vaultaire_pg_pitr_oldest_restorable_timestamp_seconds the end of the oldest backup kept:
#                                                           the earliest moment a restore can
#                                                           reach (PGPITRRetentionTooLong)
#     vaultaire_pg_pitr_backups                             backups in the repository
#     vaultaire_pg_pitr_backup_repo_bytes                   their size in the repository
#   vaultaire_pg_pitr_metrics_timestamp_seconds             this run (PGPITRMetricsStale)
set -uo pipefail

OUT_DIR=${PITR_METRICS_DIR:-/var/lib/vaultaire-pitr}
STANZA=${PITR_STANZA:-vaultaire}
OUT=$OUT_DIR/vaultaire_pitr.prom

int_or_zero() { [[ "$1" =~ ^[0-9]+$ ]] && printf '%s' "$1" || printf 0; }

archive_ok=0 archived=0 failed=0 last_archived=0 last_failed=0
if row=$(psql -X -At -F'|' -d postgres -c "SELECT archived_count, failed_count, COALESCE(EXTRACT(EPOCH FROM last_archived_time)::bigint, 0), COALESCE(EXTRACT(EPOCH FROM last_failed_time)::bigint, 0) FROM pg_stat_archiver" 2>/dev/null) \
  && IFS='|' read -r a f la lf <<< "$row" \
  && [[ "$a|$f|$la|$lf" =~ ^[0-9]+\|[0-9]+\|[0-9]+\|[0-9]+$ ]]; then
  archive_ok=1 archived=$a failed=$f last_archived=$la last_failed=$lf
fi

info_ok=0 last_full=0 last_diff=0 oldest=0 backups=0 repo_bytes=0
# status.code: 0 ok, 2 "no valid backups" (a stanza before its first full) — both are a
# repository that answered. Anything else (missing stanza, unreadable repo) is not.
if json=$(pgbackrest --stanza="$STANZA" --output=json info 2>/dev/null) \
  && vals=$(jq -r '.[0] as $s | [$s.backup[]?] as $b
      | if ($s.status.code == 0 or $s.status.code == 2) then
          [ ([$b[] | select(.type == "full") | .timestamp.stop] | max // 0),
            ([$b[] | .timestamp.stop] | max // 0),
            ([$b[] | .timestamp.stop] | min // 0),
            ($b | length),
            ([$b[] | .info.repository.delta // 0] | add // 0) ] | map(tostring) | join(" ")
        else empty end' <<< "$json" 2>/dev/null) \
  && [ -n "$vals" ]; then
  read -r lfu ldi old n rb <<< "$vals"
  info_ok=1
  last_full=$(int_or_zero "$lfu") last_diff=$(int_or_zero "$ldi") oldest=$(int_or_zero "$old")
  backups=$(int_or_zero "$n") repo_bytes=$(int_or_zero "$rb")
fi

metric() { # metric NAME TYPE HELP VALUE
  printf '# HELP %s %s\n# TYPE %s %s\n%s %s\n' "$1" "$3" "$1" "$2" "$1" "$4"
}

tmp=$(mktemp "$OUT_DIR/.vaultaire_pitr.XXXXXX") || exit 1
{
  metric vaultaire_pg_archive_stats_success gauge "1 when pg_stat_archiver was read (pg-pitr-metrics.sh)" "$archive_ok"
  metric vaultaire_pg_archive_archived_total counter "WAL segments archived since the statistics reset (pg_stat_archiver.archived_count)" "$archived"
  metric vaultaire_pg_archive_failed_total counter "failed WAL archive attempts since the statistics reset (pg_stat_archiver.failed_count)" "$failed"
  metric vaultaire_pg_archive_last_archived_timestamp_seconds gauge "unix time the last WAL segment was archived (0 = never or unknown)" "$last_archived"
  metric vaultaire_pg_archive_last_failed_timestamp_seconds gauge "unix time of the last failed WAL archive attempt (0 = never or unknown)" "$last_failed"
  metric vaultaire_pg_pitr_info_success gauge "1 when pgbackrest info answered for the stanza (pg-pitr-metrics.sh)" "$info_ok"
  metric vaultaire_pg_pitr_last_full_timestamp_seconds gauge "unix time the newest full pgBackRest backup ended (0 = none or unknown)" "$last_full"
  metric vaultaire_pg_pitr_last_diff_timestamp_seconds gauge "unix time the newest differential or full pgBackRest backup ended (0 = none or unknown)" "$last_diff"
  metric vaultaire_pg_pitr_oldest_restorable_timestamp_seconds gauge "unix time the oldest backup kept ended: the earliest point a restore reaches (0 = none or unknown)" "$oldest"
  metric vaultaire_pg_pitr_backups gauge "backups in the pgBackRest repository" "$backups"
  metric vaultaire_pg_pitr_backup_repo_bytes gauge "bytes the backups occupy in the repository (sum of their repository deltas, WAL excluded)" "$repo_bytes"
  metric vaultaire_pg_pitr_metrics_timestamp_seconds gauge "unix time pg-pitr-metrics.sh last wrote this file" "$(date +%s)"
} > "$tmp" || { rm -f "$tmp"; exit 1; }
if ! { chmod 0644 "$tmp" && mv -f "$tmp" "$OUT"; }; then rm -f "$tmp"; exit 1; fi
