#!/usr/bin/env bash
# Reproducibility gate (RP37-9, WP-P30): builds ./cmd/vaultaire twice with
# scripts/build-release.sh, each from its own fresh copy of the commit's
# tree with its own GOCACHE and GOPATH, a few seconds apart, and fails
# unless the two binaries are byte-identical. With an artifact path it also
# fails unless that artifact (the one deploy.yml ships) is the same bytes.
# Run by deploy.yml's build job on every push to main, before the upload.
#
# Usage: [GOOS=… GOARCH=…] scripts/repro-check.sh [artifact]
#        BUILD_CMD overrides the build (the red run of the old recipe).
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
export BUILD_SHA=${BUILD_SHA:-$(git rev-parse HEAD | cut -c1-12)}
export BUILD_DATE=${BUILD_DATE:-$(TZ=UTC git show -s --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ HEAD)}
work=$(mktemp -d)
trap 'chmod -R u+w "$work" 2>/dev/null; rm -rf "$work"' EXIT

sha() { if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

build() { # $1 = name
	local d="$work/$1"
	mkdir -p "$d/src" "$d/gopath" "$d/gocache"
	git archive HEAD | tar -x -C "$d/src"
	(cd "$d/src" && GOPATH="$d/gopath" GOCACHE="$d/gocache" GOMODCACHE="$d/gopath/pkg/mod" \
		${BUILD_CMD:-scripts/build-release.sh} "$d/vaultaire")
	sha "$d/vaultaire"
}

a=$(build a)
sleep 3
b=$(build b)
echo "build a: $a"
echo "build b: $b"
if [ "$a" != "$b" ]; then
	echo "repro-check: the two builds differ — the server build is not reproducible" >&2
	exit 1
fi
if [ -n "${1:-}" ]; then
	c=$(sha "$1")
	echo "artifact: $c"
	if [ "$c" != "$a" ]; then
		echo "repro-check: the artifact differs from the reproducible build" >&2
		exit 1
	fi
fi
echo "repro-check: reproducible ($a)"
