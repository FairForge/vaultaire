#!/bin/bash
# shellcheck disable=SC2016 # the fakes' bodies are single-quoted on purpose: they expand when the fake runs
# status-push_test.sh — tests for deploy/scripts/status-push.sh. Run from anywhere:
#   bash deploy/scripts/status-push_test.sh
# CI runs it (ci.yml "Deploy scripts"). Needs shellcheck and jq. curl is a fake on PATH
# that answers each URL from a fixture and records what was pushed.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
PUSH=$HERE/status-push.sh
FAILED=0
pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; FAILED=1; }
check() { local msg=$1; shift; if "$@"; then pass "$msg"; else fail "$msg"; fi; }

# --- 1. shellcheck-clean -------------------------------------------------------------
if command -v shellcheck >/dev/null; then
  if shellcheck "$PUSH" "$HERE/status-push_test.sh"; then pass "shellcheck clean"; else fail "shellcheck"; fi
else
  fail "shellcheck is not installed"
fi
command -v jq >/dev/null || { fail "jq is not installed"; exit 1; }

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
BIN=$TMP/bin FIX=$TMP/fix OUT=$TMP/out
mkdir -p "$BIN" "$FIX" "$OUT"

# The fake curl: the URL is the last argument; a fixture file per URL holds
# "<http code>\n<body>"; a missing fixture is no answer (curl's exit 7). For the
# push it records the headers read from stdin (-H @-), the body (--data-binary @f)
# and the argv, and answers $FIX/push.code.
cat > "$BIN/curl" <<'EOF'
#!/bin/bash
url=${*: -1}
argv="$*"
body="" hdr_stdin=0 wfmt=""
while [ $# -gt 0 ]; do
  case $1 in
    --data-binary) body=${2#@}; shift ;;
    -H) [ "$2" = "@-" ] && hdr_stdin=1; shift ;;
    -w) wfmt=$2; shift ;;
    -o|-m|-X) shift ;;
  esac
  shift
done
key=$(printf '%s' "$url" | tr -c 'A-Za-z0-9' '_')
if [ "$key" = "$PUSH_KEY" ]; then
  [ "$hdr_stdin" = 1 ] && cat > "$FIX/push.headers"
  cp "$body" "$FIX/push.body"
  printf '%s\n' "$argv" > "$FIX/push.argv"
  code=$(cat "$FIX/push.code" 2>/dev/null || echo 000)
  [ "$code" = 000 ] && exit 7
  [ -n "$wfmt" ] && printf '%s' "$code"
  exit 0
fi
[ -f "$FIX/$key" ] || exit 7
code=$(head -1 "$FIX/$key")
tail -n +2 "$FIX/$key"
[ -n "$wfmt" ] && printf '\n%s' "$code"
exit 0
EOF
chmod +x "$BIN/curl"

key() { printf '%s' "$1" | tr -c 'A-Za-z0-9' '_'; }
fixture() { printf '%s\n%s\n' "$2" "$3" > "$FIX/$(key "$1")"; }

ORIGIN=http://origin.test PROM=http://prom.test WEB=https://web.test LEGACY=https://legacy.test PUSH_URL=https://status.test/push
export PUSH_KEY FIX
PUSH_KEY=$(key "$PUSH_URL")
TOKEN=$(printf 'a%.0s' $(seq 1 64))
printf 'STATUS_PUSH_TOKEN=%s\n' "$TOKEN" > "$TMP/status-push.env"

NOW=$(date +%s)
healthy_all() {
  rm -f "$FIX"/*
  fixture "$ORIGIN/health" 200 '{"status":"healthy","backends_healthy":21}'
  fixture "$ORIGIN/version" 200 '{"version":"6c03823f3964","build":"x"}'
  fixture "$ORIGIN/metrics" 200 "$(printf '# HELP x\nvaultaire_synthetic_check_success 1\nvaultaire_synthetic_check_last_success_timestamp_seconds %s\n' "$((NOW - 60))")"
  fixture "$LEGACY/health" 200 '{"status":"healthy"}'
  fixture "$WEB/login" 200 '<html>sign in</html>'
  fixture "$PROM/api/v1/alerts" 200 '{"status":"success","data":{"alerts":[
    {"labels":{"alertname":"BackupDumpPlaintext","severity":"warning"},"state":"firing","annotations":{"description":"secret detail"}},
    {"labels":{"alertname":"PrimaryBackendDown","severity":"critical"},"state":"firing"},
    {"labels":{"alertname":"PrimaryBackendDown","severity":"critical","backend":"x"},"state":"firing"},
    {"labels":{"alertname":"SomethingPending","severity":"critical"},"state":"pending"},
    {"labels":{"alertname":"TranslogFrozen","severity":"page"},"state":"firing"}]}}'
  echo 204 > "$FIX/push.code"
}

run() {
  PATH="$BIN:$PATH" STATUS_PUSH_ENV="$TMP/status-push.env" STATUS_METRICS_DIR="$OUT" STATUS_PUSH_URL="$PUSH_URL" \
    STATUS_ORIGIN="$ORIGIN" STATUS_PROMETHEUS="$PROM" STATUS_WEB_URL="$WEB" STATUS_LEGACY_URL="$LEGACY" \
    bash "$PUSH" 2> "$TMP/stderr"
}
prom() { awk -v n="$1" '$1 == n {print $2}' "$OUT/vaultaire_status_push.prom"; }
state_of() { jq -r --arg id "$1" '.components[] | select(.id == $id) | .state' "$FIX/push.body"; }
detail_of() { jq -r --arg id "$1" '.components[] | select(.id == $id) | .detail' "$FIX/push.body"; }

# --- 2. everything healthy -----------------------------------------------------------
healthy_all
check "healthy: exit 0" run
check "the report is v1 with this minute's time" bash -c 'jq -e ".v == 1 and (.at - $0 | fabs) < 5" "$1" >/dev/null' "$NOW" "$FIX/push.body"
check "the build is the origin's version" test "$(jq -r .build "$FIX/push.body")" = 6c03823f3964
check "four components, in order" test "$(jq -c '[.components[].id]' "$FIX/push.body")" = '["web","s3","legacy","dashboard"]'
for id in web s3 legacy dashboard; do check "healthy: $id up" test "$(state_of $id)" = up; done
check "alerts: firing only, critical and page only, names only, unique" \
  test "$(jq -c .alerts "$FIX/push.body")" = '["PrimaryBackendDown","TranslogFrozen"]'
check "no annotation leaves the box" bash -c '! grep -q "secret detail" "$0"' "$FIX/push.body"
check "the token goes on stdin, never on the command line" bash -c 'grep -qx "Authorization: Bearer $0" "$1" && ! grep -q "$0" "$2"' "$TOKEN" "$FIX/push.headers" "$FIX/push.argv"
check "metrics: success 1" test "$(prom vaultaire_status_push_success)" = 1
check "metrics: last success is now" bash -c '[ $(( $1 - $0 )) -lt 5 ]' "$NOW" "$(prom vaultaire_status_push_last_success_seconds)"
check "metrics file 0644" test "$(stat -c%a "$OUT/vaultaire_status_push.prom" 2>/dev/null || stat -f%Lp "$OUT/vaultaire_status_push.prom")" = 644

# --- 3. STATUS_ALERT_SEVERITIES widens the list ----------------------------------------
healthy_all
STATUS_ALERT_SEVERITIES=critical,page,warning run
check "severities: warning added" test "$(jq -c .alerts "$FIX/push.body")" = '["BackupDumpPlaintext","PrimaryBackendDown","TranslogFrozen"]'

# --- 4. each component's failure ------------------------------------------------------
healthy_all
fixture "$ORIGIN/health" 200 '{"status":"degraded","reason":"primary backend circuit breaker open"}'
run
check "origin degraded: web degraded with its reason" test "$(state_of web)/$(detail_of web)" = "degraded/primary backend circuit breaker open"

healthy_all
rm -f "$FIX/$(key "$ORIGIN/health")" "$FIX/$(key "$ORIGIN/version")" "$FIX/$(key "$ORIGIN/metrics")"
run
check "origin silent: web down" test "$(state_of web)/$(detail_of web)" = "down/no answer"
check "origin silent: s3 down, no data" test "$(state_of s3)/$(detail_of s3)" = "down/no data"
check "origin silent: build empty" test "$(jq -r .build "$FIX/push.body")" = ""
check "origin silent: still pushed" test "$(prom vaultaire_status_push_success)" = 1

healthy_all
fixture "$ORIGIN/metrics" 200 "$(printf 'vaultaire_synthetic_check_success 0\nvaultaire_synthetic_check_last_success_timestamp_seconds %s\n' "$((NOW - 60))")"
run
check "synthetic failing: s3 down" test "$(state_of s3)/$(detail_of s3)" = "down/failing"

healthy_all
fixture "$ORIGIN/metrics" 200 "$(printf 'vaultaire_synthetic_check_success 1\nvaultaire_synthetic_check_last_success_timestamp_seconds %s\n' "$((NOW - 1200))")"
run
check "synthetic last success 20 min ago: s3 down" test "$(state_of s3)/$(detail_of s3)" = "down/no pass for 20 min"

healthy_all
fixture "$LEGACY/health" 502 'bad gateway'
fixture "$WEB/login" 503 'nope'
run
check "legacy 502: down" test "$(state_of legacy)/$(detail_of legacy)" = "down/HTTP 502"
check "dashboard 503: down" test "$(state_of dashboard)/$(detail_of dashboard)" = "down/HTTP 503"

healthy_all
rm -f "$FIX/$(key "$PROM/api/v1/alerts")"
run
check "prometheus silent: alerts empty, still pushed" test "$(jq -c .alerts "$FIX/push.body")" = '[]'
check "prometheus silent: a journal line" grep -q "prometheus" "$TMP/stderr"

# --- 5. a failed push: a journal line, success 0, the last success kept ----------------
healthy_all
run
LAST_OK=$(prom vaultaire_status_push_last_success_seconds)
echo 401 > "$FIX/push.code"
sleep 1
check "push 401: exit 0 (the rule is the alarm)" run
check "push 401: a journal line" grep -q "push failed: HTTP 401" "$TMP/stderr"
check "push 401: success 0" test "$(prom vaultaire_status_push_success)" = 0
check "push 401: last success kept" test "$(prom vaultaire_status_push_last_success_seconds)" = "$LAST_OK"
echo 000 > "$FIX/push.code"
run
check "push no answer: a journal line" grep -q "push failed" "$TMP/stderr"
check "push no answer: last success kept" test "$(prom vaultaire_status_push_last_success_seconds)" = "$LAST_OK"

# --- 6. no token: refuses, writes success 0, never pushes ------------------------------
healthy_all
rm -f "$FIX/push.body"
printf 'OTHER=1\n' > "$TMP/status-push.env"
if run; then fail "no token: exits non-zero"; else pass "no token: exits non-zero"; fi
check "no token: nothing pushed" test ! -e "$FIX/push.body"
check "no token: success 0" test "$(prom vaultaire_status_push_success)" = 0
printf 'STATUS_PUSH_TOKEN="%s"\n' "$TOKEN" > "$TMP/status-push.env"
healthy_all
run
check "a quoted token is unquoted" grep -qx "Authorization: Bearer $TOKEN" "$FIX/push.headers"

exit $FAILED
