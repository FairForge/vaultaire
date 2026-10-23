#!/usr/bin/env bash
# The ONE build recipe of the server binary (RP37-9, WP-P30): deploy.yml,
# `make build`, the CI smoke test and a third party recomputing a
# server_build entry of the transparency log (docs/TRANSPARENCY.md
# § Recompute a server_build entry) all run this, so the bytes depend only
# on the commit and GOOS/GOARCH:
#
#   - GOTOOLCHAIN = go.mod's `toolchain` line (go downloads it if the local
#     go differs — a different toolchain is a different binary);
#   - CGO_ENABLED=0 (no host C toolchain in the output), -trimpath (no build
#     directory or GOPATH in it), -buildvcs=false (the commit is stamped
#     below; VCS stamping would also record a dirty work tree);
#   - BuildSHA = the first 12 hex of the commit, BuildDate = the COMMIT time
#     in UTC (`git show -s --format=%cd`), never the wall clock: /version's
#     `build` field is when the commit was made, not when it was built.
#
# Usage: [GOOS=linux GOARCH=amd64] scripts/build-release.sh <output>
# BUILD_SHA / BUILD_DATE override the git reads (the reproducibility check
# builds from an exported tree with no .git).
set -euo pipefail

out=${1:?usage: build-release.sh <output>}
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

toolchain=$(awk '$1 == "toolchain" { print $2 }' go.mod)
if [ -z "$toolchain" ]; then
	echo "build-release: go.mod has no toolchain line" >&2
	exit 1
fi
if [ -z "${BUILD_SHA:-}" ]; then
	BUILD_SHA=$(git rev-parse HEAD | cut -c1-12)
fi
if [ -z "${BUILD_DATE:-}" ]; then
	BUILD_DATE=$(TZ=UTC git show -s --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ HEAD)
fi

pkg=github.com/FairForge/vaultaire/internal/api
GOTOOLCHAIN=$toolchain CGO_ENABLED=0 GOFLAGS= GOAMD64=${GOAMD64:-v1} \
	go build -trimpath -buildvcs=false \
	-ldflags "-X $pkg.BuildSHA=$BUILD_SHA -X $pkg.BuildDate=$BUILD_DATE" \
	-o "$out" ./cmd/vaultaire
