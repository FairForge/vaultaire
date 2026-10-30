#!/usr/bin/env bash
# promtool unit tests of every rule file (RP86-5): each
# deploy/monitoring/tests/<name>_test.yml is run beside its rule file in a
# scratch directory (promtool resolves rule_files from the test's
# directory), with a pinned promtool — the box's version, 2.53.5 — fetched
# and checksum-verified unless PROMTOOL names one already installed.
#
#   bash deploy/monitoring/promtool-test.sh            # CI (linux-amd64)
#   PROMTOOL=/path/to/promtool bash deploy/monitoring/promtool-test.sh
set -euo pipefail

PROM_VERSION=2.53.5
# The release's sha256sums.txt lines (bash 3 on a Mac: no associative array).
pinned_sha256() {
	case "$1" in
	linux-amd64) echo 456eb31530484d181e860e329fa0c6628e79eb388fac670cdeb4fa5d57c74ed9 ;;
	darwin-arm64) echo 971553b2aadc7323fbf7fd5ec49159cee6fe67c16d6088113dce45a2c2108489 ;;
	esac
}

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

promtool="${PROMTOOL:-}"
if [[ -z "$promtool" ]]; then
	os="$(uname -s | tr '[:upper:]' '[:lower:]')"
	arch="$(uname -m)"
	case "$arch" in x86_64) arch=amd64 ;; aarch64 | arm64) arch=arm64 ;; esac
	platform="$os-$arch"
	want="$(pinned_sha256 "$platform")"
	if [[ -z "$want" ]]; then
		echo "promtool-test: no pinned checksum for $platform; set PROMTOOL" >&2
		exit 1
	fi
	tarball="prometheus-$PROM_VERSION.$platform.tar.gz"
	curl -sSfL --retry 3 -o "$work/$tarball" \
		"https://github.com/prometheus/prometheus/releases/download/v$PROM_VERSION/$tarball"
	got="$( (sha256sum "$work/$tarball" 2>/dev/null || shasum -a 256 "$work/$tarball") | cut -d' ' -f1)"
	if [[ "$got" != "$want" ]]; then
		echo "promtool-test: $tarball checksum $got, want $want" >&2
		exit 1
	fi
	tar -xzf "$work/$tarball" -C "$work" "prometheus-$PROM_VERSION.$platform/promtool"
	promtool="$work/prometheus-$PROM_VERSION.$platform/promtool"
fi
"$promtool" --version | head -1

failed=0
count=0
for t in "$here"/tests/*_test.yml; do
	name="$(basename "$t" _test.yml)"
	dir="$work/run-$name"
	mkdir -p "$dir"
	cp "$t" "$dir/"
	cp "$here/$name.yml" "$dir/"
	count=$((count + 1))
	if ! (cd "$dir" && "$promtool" test rules "$(basename "$t")"); then
		echo "promtool-test: FAILED $name" >&2
		failed=$((failed + 1))
	fi
done
if [[ "$count" -eq 0 ]]; then
	echo "promtool-test: no tests found under $here/tests" >&2
	exit 1
fi
echo "promtool-test: $count rule tests, $failed failed"
[[ "$failed" -eq 0 ]]
