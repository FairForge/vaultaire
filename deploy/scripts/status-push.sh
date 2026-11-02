#!/bin/bash
# status-push.sh — push the service's state to the status page (WP-DOMAIN-1 part 3B).
#
# Run every 60 s as root by vaultaire-status-push.timer (deploy/systemd/), from
# /usr/local/lib/vaultaire-status/. The status page (edge/status, the Worker on
# status.stored.cloud) never asks the origin anything: this push is all it knows, and
# when the pushes stop it shows the red banner. So this script reads, builds one report
# and POSTs it; it never retries — the next minute's run is the retry.
#
# What it reads (each with a 10 s bound; a source that does not answer is reported as
# such, never as healthy):
#   web        STATUS_ORIGIN/health (127.0.0.1:8010 — HAProxy's internal frontend to the
#              active slot): healthy = up, degraded = degraded with its reason, else down
#   s3         STATUS_ORIGIN/metrics: vaultaire_synthetic_check_success 1 and a last
#              success within 10 min = up (the in-process canary signs PUT/HEAD/GET/DELETE
#              against https://s3.stored.cloud, the customer path)
#   legacy     STATUS_LEGACY_URL/health (https://stored.ge — through Cloudflare)
#   dashboard  STATUS_WEB_URL/login (https://stored.cloud — through Cloudflare): 200
#   build      STATUS_ORIGIN/version's version
#   alerts     STATUS_PROMETHEUS/api/v1/alerts: the NAMES of the alerts firing whose
#              severity is in STATUS_ALERT_SEVERITIES (default critical,page) — never a
#              label, an annotation or a value
#
# The token: STATUS_PUSH_TOKEN= in STATUS_PUSH_ENV (/opt/vaultaire/configs/status-push.env,
# root 0600) — parsed, never sourced, and handed to curl on stdin (-H @-), never argv.
#
# Writes STATUS_METRICS_DIR/vaultaire_status_push.prom (atomic rename, 0644; node_exporter
# reads it through a symlink in its textfile directory), every run:
#   vaultaire_status_push_success              1 = this run's push was stored (204)
#   vaultaire_status_push_last_success_seconds unix time of the last stored push (kept
#                                              from the previous file on a failure;
#                                              0 = never) — rule StatusPushStale
#   vaultaire_status_push_timestamp_seconds    this run
# A failed push is one journal line and exit 0 (the rule is the alarm); a missing token
# is a journal line and exit 1, nothing pushed.
set -uo pipefail

ENV_FILE=${STATUS_PUSH_ENV:-/opt/vaultaire/configs/status-push.env}
OUT_DIR=${STATUS_METRICS_DIR:-/var/lib/vaultaire-status}
PUSH_URL=${STATUS_PUSH_URL:-https://status.stored.cloud/push}
ORIGIN=${STATUS_ORIGIN:-http://127.0.0.1:8010}
PROM=${STATUS_PROMETHEUS:-http://127.0.0.1:9090}
WEB_URL=${STATUS_WEB_URL:-https://stored.cloud}
LEGACY_URL=${STATUS_LEGACY_URL:-https://stored.ge}
SEVERITIES=${STATUS_ALERT_SEVERITIES:-critical,page}
OUT=$OUT_DIR/vaultaire_status_push.prom
NOW=$(date +%s)

log() { echo "status-push: $*" >&2; }

# fetch URL — sets CODE (000 = no answer) and BODY.
fetch() {
  local out
  out=$(curl -s -m 10 -w '\n%{http_code}' "$1" 2>/dev/null) || true
  if [ -z "$out" ]; then CODE=000 BODY=""; return; fi
  CODE=${out##*$'\n'}
  BODY=${out%$'\n'*}
  [[ "$CODE" =~ ^[0-9]{3}$ ]] || CODE=000
}

last_ok=0
if [ -f "$OUT" ]; then
  v=$(awk '$1 == "vaultaire_status_push_last_success_seconds" {print $2}' "$OUT")
  [[ "$v" =~ ^[0-9]+$ ]] && last_ok=$v
fi

write_metrics() { # write_metrics SUCCESS
  local tmp
  mkdir -p "$OUT_DIR"
  tmp=$(mktemp "$OUT_DIR/.vaultaire_status_push.XXXXXX") || return 1
  {
    printf '# HELP vaultaire_status_push_success 1 when the last status-push run was stored by the status page\n# TYPE vaultaire_status_push_success gauge\nvaultaire_status_push_success %s\n' "$1"
    printf '# HELP vaultaire_status_push_last_success_seconds unix time of the last push the status page stored (0 = never)\n# TYPE vaultaire_status_push_last_success_seconds gauge\nvaultaire_status_push_last_success_seconds %s\n' "$last_ok"
    printf '# HELP vaultaire_status_push_timestamp_seconds unix time status-push.sh last ran\n# TYPE vaultaire_status_push_timestamp_seconds gauge\nvaultaire_status_push_timestamp_seconds %s\n' "$NOW"
  } > "$tmp" || { rm -f "$tmp"; return 1; }
  if ! { chmod 0644 "$tmp" && mv -f "$tmp" "$OUT"; }; then rm -f "$tmp"; return 1; fi
}

token=""
if [ -r "$ENV_FILE" ]; then
  token=$(sed -n 's/^STATUS_PUSH_TOKEN=//p' "$ENV_FILE" | tail -1)
  token=${token#\"}; token=${token%\"}; token=${token#\'}; token=${token%\'}
fi
if [ ${#token} -lt 32 ]; then
  log "no STATUS_PUSH_TOKEN (32+ characters) in $ENV_FILE; nothing pushed"
  write_metrics 0
  exit 1
fi

# component ID NAME STATE DETAIL — one JSON object on its own line.
COMPONENTS=""
component() {
  COMPONENTS+=$(jq -cn --arg id "$1" --arg name "$2" --arg state "$3" --arg detail "${4:0:120}" '{id:$id,name:$name,state:$state,detail:$detail}')$'\n'
}

health_component() { # health_component ID NAME URL
  fetch "$3"
  local status reason
  status=$(jq -r '.status // empty' <<< "$BODY" 2>/dev/null)
  if [ "$CODE" = 000 ]; then component "$1" "$2" down "no answer"
  elif [ "$CODE" = 200 ] && [ "$status" = healthy ]; then component "$1" "$2" up healthy
  elif [ "$status" = degraded ]; then
    reason=$(jq -r '.reason // "degraded"' <<< "$BODY" 2>/dev/null)
    component "$1" "$2" degraded "$reason"
  else component "$1" "$2" down "HTTP $CODE"
  fi
}

health_component web "stored.cloud" "$ORIGIN/health"

fetch "$ORIGIN/metrics"
metric_int() { awk -v n="$1" '$1 == n {printf "%d", $2; exit}' <<< "$BODY"; }
syn=$(metric_int vaultaire_synthetic_check_success)
syn_last=$(metric_int vaultaire_synthetic_check_last_success_timestamp_seconds)
if [ "$CODE" != 200 ] || [ -z "$syn" ]; then component s3 "S3 API (synthetic check)" down "no data"
elif [ "$syn" != 1 ]; then component s3 "S3 API (synthetic check)" down failing
elif [ -z "$syn_last" ] || [ $((NOW - syn_last)) -gt 600 ]; then
  component s3 "S3 API (synthetic check)" down "no pass for $(( (NOW - ${syn_last:-0}) / 60 )) min"
else component s3 "S3 API (synthetic check)" up passing
fi

health_component legacy "stored.ge" "$LEGACY_URL/health"

fetch "$WEB_URL/login"
if [ "$CODE" = 200 ]; then component dashboard "Dashboard sign-in" up "HTTP 200"
elif [ "$CODE" = 000 ]; then component dashboard "Dashboard sign-in" down "no answer"
else component dashboard "Dashboard sign-in" down "HTTP $CODE"
fi

fetch "$ORIGIN/version"
build=$(jq -r '.version // empty' <<< "$BODY" 2>/dev/null)
[[ "$build" =~ ^[0-9a-f]{7,40}$ ]] || build=""

fetch "$PROM/api/v1/alerts"
alerts=$(jq -c --arg sev "$SEVERITIES" '($sev | split(",")) as $s
    | [.data.alerts[]? | select(.state == "firing" and ((.labels.severity // "") as $v | $s | index($v)))
       | .labels.alertname | select(test("^[A-Za-z][A-Za-z0-9_:]{0,63}$"))] | unique' <<< "$BODY" 2>/dev/null)
if [ "$CODE" != 200 ] || [ -z "$alerts" ]; then
  log "prometheus did not answer the alerts ($CODE); pushing no alerts"
  alerts='[]'
fi

report=$(mktemp) || exit 1
trap 'rm -f "$report"' EXIT
printf '%s' "$COMPONENTS" | jq -cs --argjson at "$NOW" --arg build "$build" --argjson alerts "$alerts" \
  '{v:1, at:$at, build:$build, components:., alerts:$alerts}' > "$report"

code=$(printf 'Authorization: Bearer %s\n' "$token" | curl -s -m 15 -o /dev/null -w '%{http_code}' -X POST \
  -H @- -H 'Content-Type: application/json' --data-binary @"$report" "$PUSH_URL" 2>/dev/null) || true
[ -n "$code" ] || code=000
if [ "$code" = 204 ]; then
  last_ok=$NOW
  write_metrics 1
else
  log "push failed: HTTP $code from $PUSH_URL"
  write_metrics 0
fi
exit 0
