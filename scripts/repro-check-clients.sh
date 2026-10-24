#!/usr/bin/env bash
# The client manifest's gate (K4c-3b, Stage 1a Prompt 36): every row of
# internal/translog/clientbuilds/manifest.json is rebuilt from its OWN source
# commit with its recipe, in a fresh work tree with its own GOCACHE and
# GOPATH, and the deploy FAILS when any sha256 differs — a manifest row the
# log will publish must be the bytes the commit builds. Run by deploy.yml's
# build job and by TestClientBuildManifest_ReproCheckFailsOnADifferentHash.
#
# Usage: scripts/repro-check-clients.sh [manifest.json]
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
manifest=${1:-$root/internal/translog/clientbuilds/manifest.json}
cd "$root"

command -v jq >/dev/null || { echo "repro-check-clients: jq is required" >&2; exit 1; }

rows=$(jq -c '.clients[]' "$manifest")
[ -n "$rows" ] || { echo "repro-check-clients: no rows in $manifest" >&2; exit 1; }

work=$(mktemp -d "${TMPDIR:-/tmp}/client-repro.XXXXXX")
# Go's module cache is read-only; make it writable before the sweep.
trap 'chmod -R u+w "$work" 2>/dev/null || true; rm -rf "$work" 2>/dev/null || true' EXIT

fail=0
i=0
while IFS= read -r row; do
	i=$((i + 1))
	client=$(jq -r .client <<<"$row")
	version=$(jq -r .version <<<"$row")
	commit=$(jq -r .source_commit <<<"$row")
	want=$(jq -r .sha256 <<<"$row")
	recipe=$(jq -r .recipe <<<"$row")

	src="$work/src$i"
	mkdir -p "$src"
	if ! git cat-file -e "$commit^{commit}" 2>/dev/null; then
		git fetch --quiet --depth 1 origin "$commit"
	fi
	git archive "$commit" | tar -x -C "$src"
	out="$work/out$i"
	# The recipe at the source commit; the first release's commit predates
	# the script (it landed with the manifest), so the checkout's reviewed
	# copy is used then — the sources built are still the commit's.
	if [ ! -f "$src/$recipe" ]; then
		echo "repro-check-clients: $client $version: $recipe is not at ${commit:0:12} — using the checkout's copy on the commit's sources"
		install -m 0755 "$root/$recipe" "$src/$recipe"
	fi
	GOCACHE="$work/gocache$i" GOPATH="$work/gopath$i" \
		bash "$src/$recipe" "$client" "$out"
	got=$(shasum -a 256 "$out" | cut -d' ' -f1)
	if [ "$got" = "$want" ]; then
		echo "repro-check-clients: $client $version @ ${commit:0:12}: $got OK"
	else
		echo "repro-check-clients: $client $version @ ${commit:0:12}: built $got, the manifest says $want — REFUSED" >&2
		fail=1
	fi
done <<<"$rows"

exit $fail
