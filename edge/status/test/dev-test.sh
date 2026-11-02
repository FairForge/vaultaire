#!/usr/bin/env bash
# dev-test.sh — the status Worker in the real runtime (wrangler dev --local,
# a real Durable Object): a bad token is 401, a good push renders, a push
# left to go stale shows the red banner. Needs node + network for npx.
#   bash edge/status/test/dev-test.sh
# CI runs the node tests (make test-edge); this one is the deploy's own check.
set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
PORT=${STATUS_DEV_PORT:-8799}
TOKEN=$(openssl rand -hex 32)
WORK=$(mktemp -d)
FAILED=0
pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; FAILED=1; }

cd "$HERE"
npx -y wrangler@4.151.3 dev --local --port "$PORT" --persist-to "$WORK/state" \
  --var "STATUS_PUSH_TOKEN:$TOKEN" --var "STALE_AFTER_SECONDS:10" > "$WORK/dev.log" 2>&1 &
DEV_PID=$!
cleanup() { kill "$DEV_PID" 2>/dev/null || true; wait "$DEV_PID" 2>/dev/null || true; rm -rf "$WORK"; }
trap cleanup EXIT

for _ in $(seq 1 60); do
  curl -s -o /dev/null "http://127.0.0.1:$PORT/" && break
  sleep 1
done

URL=http://127.0.0.1:$PORT
report() {
  printf '{"v":1,"at":%s,"build":"6c03823f3964","components":[{"id":"web","name":"stored.cloud","state":"up","detail":"healthy"}],"alerts":[]}' "$(date +%s)"
}

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Authorization: Bearer wrong-token-wrong-token-wrong-token' --data-binary "$(report)" "$URL/push")
[ "$code" = 401 ] && pass "bad token: 401" || fail "bad token: $code"

code=$(printf 'Authorization: Bearer %s\n' "$TOKEN" | curl -s -o /dev/null -w '%{http_code}' -X POST -H @- --data-binary "$(report)" "$URL/push")
[ "$code" = 204 ] && pass "good token: 204" || fail "good token: $code"

page=$(curl -s "$URL/")
grep -q 'All systems operational' <<< "$page" && pass "the page renders the push" || fail "the page: $(head -c 300 <<< "$page")"
[ "$(curl -s -o /dev/null -w '%{header_json}' "$URL/" | grep -c set-cookie)" = 0 ] && pass "no cookie" || fail "a cookie"

sleep 12
page=$(curl -s "$URL/")
grep -q 'class="banner"' <<< "$page" && grep -q 'No recent report' <<< "$page" && pass "stale: the red banner" || fail "stale: $(head -c 300 <<< "$page")"

code=$(curl -s -o /dev/null -w '%{http_code}' "$URL/anything")
[ "$code" = 404 ] && pass "other paths: 404" || fail "other paths: $code"

[ "$FAILED" = 0 ] || { echo "--- wrangler dev log"; tail -40 "$WORK/dev.log"; }
exit $FAILED
