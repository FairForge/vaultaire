#!/usr/bin/env bash
# Landing page browser checks: render both template variants with Go, then
# drive the house builder in headless Chrome (harness.html) and fail on any
# FAIL line. Locally: make landing-browser. CI: the landing-browser job.
set -euo pipefail

cd "$(dirname "$0")/../../../.."
OUT="$(mktemp -d)"
trap 'rm -rf "$OUT"' EXIT

LANDING_OUT="$OUT" go test -count=1 ./internal/api/ -run TestLanding_WriteVariants >/dev/null
cp internal/api/landing/browser/harness.html "$OUT/"

CHROME="${CHROME:-}"
if [ -z "$CHROME" ]; then
  for c in google-chrome google-chrome-stable chromium chromium-browser "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"; do
    if command -v "$c" >/dev/null 2>&1 || [ -x "$c" ]; then CHROME="$c"; break; fi
  done
fi
[ -n "$CHROME" ] || { echo "no Chrome found; set CHROME=/path/to/chrome"; exit 2; }

"$CHROME" --headless=new --disable-gpu --no-sandbox --allow-file-access-from-files \
  --blink-settings=preferredColorScheme=1 --virtual-time-budget=12000 \
  --dump-dom "file://$OUT/harness.html" 2>/dev/null \
  | sed -n '/<pre id="out">/,/<\/pre>/p' | sed 's/<pre id="out">//; s/<\/pre>//; s/&gt;/>/g; s/&lt;/</g; s/&amp;/\&/g' > "$OUT/result.txt"

cat "$OUT/result.txt"
grep -q '^DONE$' "$OUT/result.txt" || { echo "harness did not finish"; exit 1; }
if grep -q '^FAIL' "$OUT/result.txt"; then echo "browser checks FAILED"; exit 1; fi
echo "browser checks passed ($(grep -c '^PASS' "$OUT/result.txt") checks)"

# Real mouse input through the DevTools protocol (pointer.mjs): the one thing
# synthetic events cannot see is a lost pointer capture. Needs Node 22+ (global
# WebSocket); CI installs it, locally it is skipped with a note when missing.
if command -v node >/dev/null 2>&1 && node -e 'process.exit(typeof WebSocket === "function" ? 0 : 1)' 2>/dev/null; then
  node internal/api/landing/browser/pointer.mjs "$OUT/closed.html" "$CHROME" | tee "$OUT/pointer.txt"
  if grep -q '^FAIL' "$OUT/pointer.txt"; then echo "pointer checks FAILED"; exit 1; fi
  echo "pointer checks passed ($(grep -c '^PASS' "$OUT/pointer.txt") checks)"
else
  echo "pointer checks skipped: node 22+ not found"
fi
