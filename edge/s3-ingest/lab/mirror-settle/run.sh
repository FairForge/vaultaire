#!/usr/bin/env bash
# The mirror-settle lab (RP78-2): putTwo in workerd against a sink that
# answers 411 to a body with no length; a 2 MB/s mirror (settles after the
# answer, ready) and a 40 s mirror (cut 25 s after the primary, the settle
# says cut). Prints the client's times and the sink's log.
set -euo pipefail
cd "$(dirname "$0")"
rm -rf build && mkdir build && cp worker.js ../../src/*.js build/
node sink.mjs & SINK=$!
npx -y wrangler@4.151.3 dev --local --port 8789 >wrangler.log 2>&1 & WR=$!
# Stop only what this script started (never by pattern): wrangler's tree.
stop() { kill $SINK 2>/dev/null || true; for c in $(pgrep -P $WR 2>/dev/null); do pkill -P $c 2>/dev/null || true; kill $c 2>/dev/null || true; done; kill $WR 2>/dev/null || true; }
trap stop EXIT
for _ in $(seq 60); do curl -fs -o /dev/null http://127.0.0.1:8789/?n=1 2>/dev/null && break; sleep 1; done
echo "slow (2 MB/s) mirror, 8 MiB:"; curl -s "http://127.0.0.1:8789/?n=8388608&mirror=slow"; echo
echo "40 s mirror, 4 MiB:";           curl -s "http://127.0.0.1:8789/?n=4194304&mirror=stall"; echo
sleep 45
curl -s http://127.0.0.1:8788/log
