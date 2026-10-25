#!/usr/bin/env bash
# The server SBOM against go.sum at HEAD (WP-SUPPLY-1 A.1): every module
# component the SBOM names must be a `<module> <version> h1:…` line of go.sum
# whose h1 (the base64 SHA-256 of the module's dirhash) decodes to the SBOM's
# SHA-256 — the dependency set and the hashes are go.sum's truth, and a
# go.sum that moved without the SBOM (or the reverse) fails. The std module
# is the toolchain of go.mod. Fails on an SBOM with no modules.
#
# Usage: scripts/sbom-check.sh [vaultaire.cdx.json]   (default bin/sbom/vaultaire.cdx.json)
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
sbom=${1:-bin/sbom/vaultaire.cdx.json}
gosum=${SBOM_GO_SUM:-go.sum}     # tests: a fixture
gomodf=${SBOM_GO_MOD:-go.mod}
[ -r "$sbom" ] || { echo "sbom-check: $sbom is not readable (make sbom first)" >&2; exit 1; }
toolchain=$(awk '$1 == "toolchain" { print $2 }' "$gomodf")
n=0 bad=0
while IFS=$'\t' read -r name version sum; do
  n=$((n + 1))
  if [ "$name" = std ]; then
    [ "$version" = "$toolchain" ] || { echo "sbom-check: std is $version, go.mod's toolchain is $toolchain" >&2; bad=$((bad + 1)); }
    continue
  fi
  line=$(grep -F -- "$name $version h1:" "$gosum" | head -1 || true)
  if [ -z "$line" ]; then echo "sbom-check: $name@$version is not in $gosum" >&2; bad=$((bad + 1)); continue; fi
  h1=${line##*h1:}
  hex=$(printf '%s' "$h1" | base64 -d | od -An -tx1 | tr -d ' \n')
  if [ "$hex" != "$sum" ]; then echo "sbom-check: $name@$version: SBOM sha256 $sum, go.sum h1 decodes to $hex" >&2; bad=$((bad + 1)); fi
done < <(jq -r '.components[] | select(.type == "library") | [.name, .version, ((.hashes // []) | map(select(.alg == "SHA-256")) | .[0].content // "")] | @tsv' "$sbom")
[ "$n" -gt 0 ] || { echo "sbom-check: the SBOM names no module" >&2; exit 1; }
if [ "$bad" != 0 ]; then echo "sbom-check: $bad of $n components disagree with go.sum" >&2; exit 1; fi
echo "sbom-check: $n components, every module and hash is go.sum's at HEAD (std = $toolchain)"
