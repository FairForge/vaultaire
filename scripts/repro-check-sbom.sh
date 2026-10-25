#!/usr/bin/env bash
# The SBOMs' reproducibility gate (WP-SUPPLY-1 A.1, the repro-check.sh deal
# for scripts/sbom.sh): generated twice, seconds apart, each into its own
# directory; fails unless every document is byte-identical. With a
# directory argument it also fails unless that directory's documents (the
# ones a release ships) are the same bytes. Run by deploy.yml's build job.
#
# Usage: scripts/repro-check-sbom.sh [shipped-dir]
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
sha() { if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

scripts/sbom.sh "$work/a" >/dev/null
sleep 2
scripts/sbom.sh "$work/b" >/dev/null
rc=0
for f in vaultaire sdk-wasm sdk-js; do
  a=$(sha "$work/a/$f.cdx.json"); b=$(sha "$work/b/$f.cdx.json")
  echo "$f: a $a"; echo "$f: b $b"
  if [ "$a" != "$b" ]; then
    echo "repro-check-sbom: $f.cdx.json differs between two runs — the SBOM is not reproducible" >&2
    diff <(jq . "$work/a/$f.cdx.json") <(jq . "$work/b/$f.cdx.json") | head -20 >&2 || true
    rc=1
  fi
  if [ -n "${1:-}" ]; then
    c=$(sha "$1/$f.cdx.json")
    echo "$f: shipped $c"
    if [ "$c" != "$a" ]; then echo "repro-check-sbom: the shipped $f.cdx.json differs from the reproducible one" >&2; rc=1; fi
  fi
done
[ $rc = 0 ] && echo "repro-check-sbom: reproducible"
exit $rc
