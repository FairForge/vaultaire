#!/bin/bash
# Tests of scripts/sbom-check.sh against fixtures (no network, no build): the
# SBOM's modules and hashes must be go.sum's — a hash that moved, a module
# go.sum does not list, a toolchain that differs and an SBOM with no module
# all fail; the matching set passes. Run by CI ("Deploy scripts").
set -euo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
FAILED=0
pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; FAILED=1; }
command -v jq >/dev/null || { fail "jq is not installed"; exit 1; }
if command -v shellcheck >/dev/null; then
  if shellcheck "$HERE/sbom.sh" "$HERE/sbom-check.sh" "$HERE/repro-check-sbom.sh" "$HERE/sbom_test.sh"; then pass "shellcheck clean"; else fail "shellcheck"; fi
else
  fail "shellcheck is not installed"
fi
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
SBOM=$HERE/testdata/sbom-fixture.cdx.json
# h1 of 14d7f8b7…1db0 is its base64: the fixture's edwards25519 line matches, example.com/dep's hash is ff.
good_h1=$(printf '14d7f8b72c5114d4a0b38d7e94eb1d52a1a4226f9f3e55a6d3a25ac0b96a1db0' | xxd -r -p | base64)
dep_h1=$(printf '00000000000000000000000000000000000000000000000000000000000000ff' | xxd -r -p | base64)
printf 'filippo.io/edwards25519 v1.1.0 h1:%s\nfilippo.io/edwards25519 v1.1.0/go.mod h1:xxx\nexample.com/dep v2.0.0 h1:%s\n' "$good_h1" "$dep_h1" > "$TMP/go.sum"
printf 'module x\n\ngo 1.26.0\n\ntoolchain go1.26.10\n' > "$TMP/go.mod"
run() { SBOM_GO_SUM=$TMP/go.sum SBOM_GO_MOD=$TMP/go.mod bash "$HERE/sbom-check.sh" "$1"; }

if out=$(run "$SBOM" 2>&1); then pass "the matching set passes: $out"; else fail "the matching set must pass: $out"; fi

# a hash that moved in go.sum
sed -i.bak "s|^example.com/dep v2.0.0 h1:.*|example.com/dep v2.0.0 h1:$(printf '00000000000000000000000000000000000000000000000000000000000000fe' | xxd -r -p | base64)|" "$TMP/go.sum"
if out=$(run "$SBOM" 2>&1); then fail "a hash that differs from go.sum must fail"; else pass "a hash that differs fails: $(grep -c 'decodes to' <<< "$out") named"; fi
mv "$TMP/go.sum.bak" "$TMP/go.sum"

# a module go.sum no longer lists (a dependency removed without the SBOM)
grep -v '^example.com/dep ' "$TMP/go.sum" > "$TMP/go.sum.2"; mv "$TMP/go.sum.2" "$TMP/go.sum"
if out=$(run "$SBOM" 2>&1); then fail "a module not in go.sum must fail"; else pass "a module not in go.sum fails"; fi
printf 'example.com/dep v2.0.0 h1:%s\n' "$dep_h1" >> "$TMP/go.sum"

# a version bump in go.sum the SBOM does not know
sed -i.bak 's|^example.com/dep v2.0.0 |example.com/dep v2.1.0 |' "$TMP/go.sum"
if out=$(run "$SBOM" 2>&1); then fail "a dependency bump must fail"; else pass "a dependency bump fails (the SBOM names the old version)"; fi
mv "$TMP/go.sum.bak" "$TMP/go.sum"

# the toolchain moved
sed -i.bak 's|toolchain go1.26.10|toolchain go1.26.11|' "$TMP/go.mod"
if run "$SBOM" >/dev/null 2>&1; then fail "a toolchain that differs must fail"; else pass "a toolchain that differs fails"; fi
mv "$TMP/go.mod.bak" "$TMP/go.mod"

# an SBOM with no module
jq '.components = []' "$SBOM" > "$TMP/empty.json"
if run "$TMP/empty.json" >/dev/null 2>&1; then fail "an SBOM naming no module must fail"; else pass "an SBOM naming no module fails"; fi

exit $FAILED
