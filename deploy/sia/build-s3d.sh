#!/usr/bin/env bash
# build-s3d.sh — build Stored's s3d: upstream s3d v0.2.1 with the siastorage SDK
# and indexd client patched for budget-limited reads (bench-results/
# SIA-S3D-2026-10.md, SIA-BENCH-3..5). Reproducible from pinned upstream
# versions; nothing here is sent upstream.
#
#   APP_ID=<seed string>  the app ID the gateway registers under (default: the
#                         upstream "s3d"). Gateway #2 runs "stored-s3d-app2".
#                         An instance must always run the build of ITS app ID:
#                         another ID derives another app key and its objects
#                         are invisible.
#   OUT=<path>            output binary (default $HOME/gopath/bin/s3d-stored)
#
# Needs Go (CGO, sqlite) and network access to the Go module proxy.
set -euo pipefail

S3D_VERSION=v0.2.1
SDK_VERSION=v0.2.2-0.20260831091740-f9961f7e5391
INDEXD_VERSION=v0.5.0
APP_ID=${APP_ID:-s3d}
OUT=${OUT:-$HOME/gopath/bin/s3d-stored}
HERE=$(cd "$(dirname "$0")" && pwd)

WORK=$(mktemp -d)
trap 'chmod -R u+w "$WORK" 2>/dev/null; rm -rf "$WORK"' EXIT

fetch() { # module@version -> a writable copy under $WORK/$2
  local dir
  dir=$(cd "$WORK" && GOFLAGS=-mod=mod go mod download -json "$1" | sed -n 's/^\s*"Dir": "\(.*\)",$/\1/p')
  [ -n "$dir" ] || { echo "cannot download $1" >&2; exit 1; }
  cp -r "$dir" "$WORK/$2"
  chmod -R u+w "$WORK/$2"
}

fetch "github.com/SiaFoundation/s3d@$S3D_VERSION" s3d
fetch "go.sia.tech/siastorage@$SDK_VERSION" siastorage
fetch "go.sia.tech/indexd@$INDEXD_VERSION" indexd

# --ignore-whitespace: blank context lines lose their leading space in git
(cd "$WORK/siastorage" && patch -p1 --ignore-whitespace --quiet < "$HERE/patches/siastorage-f9961f7.patch")
(cd "$WORK/indexd" && patch -p1 --ignore-whitespace --quiet < "$HERE/patches/indexd-v0.5.0.patch")
(cd "$WORK/s3d" && patch -p1 --ignore-whitespace --quiet < "$HERE/patches/s3d-v0.2.1.patch")

if [ "$APP_ID" != "s3d" ]; then
  sed -i.bak "s|types.HashBytes(\[\]byte(\"s3d\"))|types.HashBytes([]byte(\"$APP_ID\"))|" "$WORK/s3d/cmd/s3d/login.go"
  grep -q "HashBytes(\[\]byte(\"$APP_ID\"))" "$WORK/s3d/cmd/s3d/login.go" || { echo "app ID patch failed" >&2; exit 1; }
fi

cd "$WORK/s3d"
go mod edit -replace "go.sia.tech/siastorage=$WORK/siastorage" -replace "go.sia.tech/indexd=$WORK/indexd"
GOFLAGS=-mod=mod CGO_ENABLED=1 go build -trimpath -o "$OUT" ./cmd/s3d
echo "built $OUT (s3d $S3D_VERSION, siastorage $SDK_VERSION, indexd $INDEXD_VERSION, app ID \"$APP_ID\")"
sha256sum "$OUT" 2>/dev/null || shasum -a 256 "$OUT"
