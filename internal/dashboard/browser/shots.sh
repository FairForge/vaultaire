#!/usr/bin/env bash
# Dashboard screenshots: render the real dashboard pages with Go (the
# *_WriteScreenshotFixtures tests, against the local vaultaire_test DB), then
# photograph each one in headless Chrome at 1280 px light and dark and at
# 390 px (phone). Optionally score every page's accessibility with Lighthouse.
#
#   make dash-shots                 -> $DASH_SHOTS_OUT/shots/*.png (default /tmp/vaultaire-dash)
#   make dash-lighthouse            -> the same, plus Lighthouse accessibility per page (needs npx)
#   DASH_SHOTS_OUT=dir CHROME=/path/to/chrome bash internal/dashboard/browser/shots.sh [lighthouse]
#
# Pages come from the fixture tests in internal/dashboard/handlers
# (TestHouse_/TestOverview_/TestPages_/TestAdmin_WriteScreenshotFixtures);
# add a page there and it shows up here. Phone shots go through a 390 px
# <iframe> because headless Chrome refuses windows narrower than 500 px.
set -euo pipefail

cd "$(dirname "$0")/../../.."
MODE="${1:-shots}"
OUT="${DASH_SHOTS_OUT:-/tmp/vaultaire-dash}"
PAGES="$OUT/pages"
SHOTS="$OUT/shots"
HEIGHT="${DASH_SHOTS_HEIGHT:-3200}"
PHONE_HEIGHT="${DASH_SHOTS_PHONE_HEIGHT:-2400}"

CHROME="${CHROME:-}"
if [ -z "$CHROME" ]; then
  for c in google-chrome google-chrome-stable chromium chromium-browser "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"; do
    if command -v "$c" >/dev/null 2>&1 || [ -x "$c" ]; then CHROME="$c"; break; fi
  done
fi
[ -n "$CHROME" ] || { echo "no Chrome found; set CHROME=/path/to/chrome"; exit 2; }

rm -rf "$OUT"
mkdir -p "$PAGES" "$SHOTS"

# 1. Render the pages. The fixtures need the migrated test database.
make -s test-db >"$OUT/test-db.log" 2>&1 || { cat "$OUT/test-db.log"; exit 1; }
DASH_OUT="$PAGES" go test -count=1 ./internal/dashboard/handlers \
  -run 'Test(House|Overview|Pages|Admin)_WriteScreenshotFixtures' >/dev/null
ln -sfn "$PWD/internal/dashboard/static" "$PAGES/static"

shopt -s nullglob
pages=("$PAGES"/*.html)
[ ${#pages[@]} -gt 0 ] || { echo "no pages rendered under $PAGES"; exit 1; }

shot() { # shot <scheme 0|1> <width> <height> <url> <png>
  "$CHROME" --headless=new --disable-gpu --no-sandbox --hide-scrollbars \
    --allow-file-access-from-files --force-device-scale-factor=1 \
    --blink-settings=preferredColorScheme="$1" --virtual-time-budget=4000 \
    --window-size="$2,$3" --screenshot="$5" "$4" >/dev/null 2>&1
}

# 2. Photograph: 1280 px light + dark, and 390 px light through the iframe.
for p in "${pages[@]}"; do
  name="$(basename "$p" .html)"
  shot 1 1280 "$HEIGHT" "file://$p" "$SHOTS/$name-light.png"
  shot 0 1280 "$HEIGHT" "file://$p" "$SHOTS/$name-dark.png"
  cat > "$PAGES/phone-$name.html" <<HTML
<!doctype html><html><head><meta charset="utf-8"><style>html,body{margin:0;background:#888}iframe{border:0;width:390px;height:${PHONE_HEIGHT}px;display:block}</style></head><body><iframe src="$name.html"></iframe></body></html>
HTML
  shot 1 390 "$PHONE_HEIGHT" "file://$PAGES/phone-$name.html" "$SHOTS/$name-phone.png"
  echo "$name: light, dark, phone"
done
echo "screenshots in $SHOTS ($(ls "$SHOTS" | wc -l | tr -d ' ') files)"

[ "$MODE" = "lighthouse" ] || exit 0

# 3. Lighthouse accessibility, the same routine as the site (Lighthouse 12 over a
# local static server). Fails when any page scores below 100.
command -v npx >/dev/null || { echo "npx not found; install Node to run Lighthouse"; exit 2; }
PORT="${DASH_SHOTS_PORT:-8765}"
python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$PAGES" >/dev/null 2>&1 &
SERVER=$!
trap 'kill $SERVER 2>/dev/null || true' EXIT
sleep 1
fail=0
for p in "${pages[@]}"; do
  name="$(basename "$p" .html)"
  report="$OUT/lh-$name.json"
  CHROME_PATH="$CHROME" npx -y lighthouse@12 "http://127.0.0.1:$PORT/$name.html" \
    --only-categories=accessibility --chrome-flags="--headless=new --no-sandbox" \
    --quiet --output=json --output-path="$report" >/dev/null 2>&1 || true
  score="$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(int(round(d["categories"]["accessibility"]["score"]*100)))' "$report" 2>/dev/null || echo "?")"
  echo "accessibility $name: $score"
  [ "$score" = "100" ] || fail=1
done
[ $fail -eq 0 ] || { echo "Lighthouse: a page is below 100; reports in $OUT/lh-*.json"; exit 1; }
echo "Lighthouse: every page at 100"
