#!/bin/bash
# age-install.sh [DEST] — installs the PINNED age release (age + age-keygen) into DEST
# (default /usr/local/bin) after checking the tarball's sha256. WP-P42, RP62-1: Ubuntu's
# apt candidate on the box is age 1.2.1, which has no post-quantum `age1pq1…` recipients
# (they came in 1.3.0) — and the owner's backup recipients are age1pq (decision F-36,
# docs/DEPLOY.md § Backups). pg-backup.sh's dumps are sealed with what this installs; CI's
# "Deploy scripts" step installs the same pin so pg-backup_test.sh 5g runs the real age.
#
# Idempotent: an `age` in DEST already reporting the pinned version = nothing to do. A
# tarball whose sha256 is not the pin = nothing installed, exit 1. A bump changes
# AGE_VERSION and AGE_SHA256 together (the release's linux-amd64 tarball, hashed twice
# from two downloads).
# AGE_TARBALL=<file> uses a local tarball instead of the download (tests, an offline box).
set -euo pipefail

AGE_VERSION=v1.3.2
AGE_SHA256=cbe24006683f8eb669266162894b9a522a1af52f2665fbc63a4bb032ed26ac10
DEST=${1:-/usr/local/bin}
URL="https://github.com/FiloSottile/age/releases/download/$AGE_VERSION/age-$AGE_VERSION-linux-amd64.tar.gz"

if [ -x "$DEST/age" ] && [ "$("$DEST/age" --version 2>/dev/null)" = "$AGE_VERSION" ]; then
  echo "age-install: $DEST/age is already $AGE_VERSION"
  exit 0
fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
if [ -n "${AGE_TARBALL:-}" ]; then
  cp "$AGE_TARBALL" "$WORK/age.tar.gz"
else
  curl -fsSL --retry 3 -o "$WORK/age.tar.gz" "$URL"
fi
got=$(sha256sum "$WORK/age.tar.gz" | cut -d' ' -f1)
if [ "$got" != "$AGE_SHA256" ]; then
  echo "age-install: sha256 mismatch for age $AGE_VERSION (got $got, pinned $AGE_SHA256): nothing installed" >&2
  exit 1
fi
tar xzf "$WORK/age.tar.gz" -C "$WORK"
install -d "$DEST"
install -m 0755 "$WORK/age/age" "$WORK/age/age-keygen" "$DEST/"
echo "age-install: installed $("$DEST/age" --version) into $DEST"
