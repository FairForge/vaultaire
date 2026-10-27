#!/usr/bin/env bash
# The client manifest's gate (K4c-3b, Stage 1a Prompt 36): every row of
# internal/translog/clientbuilds/manifest.json is rebuilt from its OWN source
# commit with its recipe, in a fresh work tree with its own GOCACHE and
# GOPATH, and the deploy FAILS when any sha256 differs — a manifest row the
# log will publish must be the bytes the commit builds. Run by deploy.yml's
# build job and by TestClientBuildManifest_ReproCheckFailsOnADifferentHash.
#
# Then the CHECKOUT's own clients are built and each is looked up in the
# manifest (RP51-3, Stage 1a Prompt 38): between an sdk merge and its row,
# main ships client bytes no row lists, and translog-verify -client refuses
# them. No CI check can demand the row — it names the MERGE commit, which no
# sdk PR knows — so this only WARNS (a ::warning:: line naming the client and
# the hash); it never fails the deploy, not even when the build itself fails.
# REPRO_CHECKOUT_CLIENTS narrows the list (the tests build sdk-js only);
# REPRO_CHECKOUT_BUILD replaces the checkout's build script (a test seam).
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

# The checkout's clients (RP51-3). The default list is translog's closed list
# (TestReproCheckClients_DefaultsToTheClosedClientList).
checkout_clients=${REPRO_CHECKOUT_CLIENTS:-sdk-wasm sdk-js edge-ingest edge-chunker}
checkout_build=${REPRO_CHECKOUT_BUILD:-$root/scripts/build-client.sh}
for client in $checkout_clients; do
	start=$SECONDS
	out="$work/checkout-$client"
	if ! bash "$checkout_build" "$client" "$out"; then
		echo "::warning::repro-check-clients: this checkout's $client did not build — whether main ships a listed build is unknown"
		continue
	fi
	# A build that exits 0 with no output is a warning as well, never the
	# set -e end of the step (RP54-7).
	if [ ! -s "$out" ] || ! got=$(shasum -a 256 "$out" | cut -d' ' -f1) || [ -z "$got" ]; then
		echo "::warning::repro-check-clients: this checkout's $client built nothing — whether main ships a listed build is unknown"
		continue
	fi
	if jq -e --arg c "$client" --arg h "$got" 'any(.clients[]; .client == $c and .sha256 == $h)' "$manifest" >/dev/null; then
		echo "repro-check-clients: this checkout's $client $got is listed ($((SECONDS - start)) s)"
	else
		echo "::warning::repro-check-clients: this checkout builds $client $got, which no row of the client manifest lists — translog-verify -client refuses it until a row names it ($((SECONDS - start)) s)"
	fi
done

exit $fail
