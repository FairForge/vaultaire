#!/usr/bin/env bash
# The SBOMs (WP-SUPPLY-1, Stage 1a Prompt 41 Part A.1; docs/RELEASES.md): one
# CycloneDX 1.6 JSON document per build the transparency log names —
#
#   vaultaire.cdx.json  the server: cyclonedx-gomod `bin` over the binary the
#                       release recipe builds (scripts/build-release.sh,
#                       linux/amd64) — the module list and go.sum hashes as
#                       LINKED (debug/buildinfo), the binary's own sha256 in
#                       metadata.component.hashes: the SBOM names the bytes;
#   sdk-wasm.cdx.json   the WebAssembly client: cyclonedx-gomod `app` for
#                       GOOS=js GOARCH=wasm over ./sdk/wasm (debug/buildinfo
#                       cannot read a wasm binary) — run in a throwaway git
#                       repository made of `git archive HEAD` (the tool reads
#                       the main module's version from git, and go-git reads
#                       neither a worktree nor a shallow checkout the same
#                       way), the version then set to HEAD's commit;
#   sdk-js.cdx.json     the JS binding: one file component, its sha256, its
#                       version from the client manifest.
#
# cyclonedx-gomod is PINNED and built for the HOST into a temporary GOBIN
# (the Makefile's golangci-lint / lint-wasm pattern — never a PATH binary;
# `go run` under GOOS=linux would build the tool for linux).
#
# REPRODUCIBLE: two runs on the same commit are byte-identical
# (scripts/repro-check-sbom.sh proves it, deploy.yml runs it): no serial
# number, no timestamp, the main component's version = HEAD's commit, and
# the tool's own binary hashes dropped from metadata.tools (they differ per
# host). scripts/sbom-check.sh proves every module the server SBOM names is
# go.sum's at HEAD with go.sum's hash.
#
# Usage: scripts/sbom.sh [out-dir]     (default bin/sbom)
#   SBOM_SERVER_BINARY=<path>  use this binary instead of building one
#   SBOM_TARGETS=server,sdk-wasm,sdk-js   which documents to write (default all;
#                              a released client's SBOM is regenerated from its
#                              pinned source commit with SBOM_TARGETS=sdk-wasm,sdk-js)
#   SBOM_MANIFEST=<path>       the client manifest to read sdk-js's version from
#                              (default this checkout's; a regeneration at an old
#                              commit names the current one)
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
out=${1:-bin/sbom}
mkdir -p "$out"
out=$(cd "$out" && pwd)
targets=,${SBOM_TARGETS:-server,sdk-wasm,sdk-js},
want() { case "$targets" in *,"$1",*) return 0 ;; *) return 1 ;; esac; }
command -v jq >/dev/null || { echo "sbom: jq is required" >&2; exit 1; }

commit=$(git rev-parse HEAD)
CYCLONEDX_GOMOD_VERSION=${CYCLONEDX_GOMOD_VERSION:-v1.12.0}
work=$(mktemp -d "${TMPDIR:-/tmp}/sbom.XXXXXX")
trap 'chmod -R u+w "$work" 2>/dev/null || true; rm -rf "$work"' EXIT
GOBIN=$work/tool GOOS='' GOARCH='' GOFLAGS='' go install "github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@$CYCLONEDX_GOMOD_VERSION"
gomod() { GOFLAGS='' "$work/tool/cyclonedx-gomod" "$@"; }
common=(-json -noserial -notimestamp -output-version 1.6 -std -licenses=false)

# normalize FILE — the main component's version is HEAD's commit: every string
# equal to its purl or bom-ref (the component, the dependency graph's refs)
# is rewritten with the commit in place of the tool's version; the tool's
# host-dependent hashes go. Key order kept.
normalize() {
  jq --arg v "$commit" '
    def fix: sub("@[^?#]*"; "@" + $v);
    (.metadata.component.purl // "") as $p
    | (.metadata.component["bom-ref"] // "") as $r
    | walk(if type == "string" and $p != "" and . == $p then fix
           elif type == "string" and ($r | startswith("pkg:")) and . == $r then fix
           else . end)
    | .metadata.component.version = $v
    | .metadata.tools |= (if type == "array" then map(del(.hashes)) else . end)
  ' "$1" > "$1.n" && mv "$1.n" "$1"
}

# 1. the server, from the release binary.
if want server; then
  bin=${SBOM_SERVER_BINARY:-}
  if [ -z "$bin" ]; then
    bin=$work/vaultaire-linux
    GOOS=linux GOARCH=amd64 scripts/build-release.sh "$bin" >/dev/null
  fi
  GOOS=linux GOARCH=amd64 gomod bin "${common[@]}" -version "$commit" -output "$out/vaultaire.cdx.json" "$bin"
  normalize "$out/vaultaire.cdx.json"
fi

# 2. sdk-wasm, from the module graph for js/wasm, in a throwaway repository of HEAD.
if want sdk-wasm; then
  src=$work/src
  mkdir -p "$src"
  git archive HEAD | tar -x -C "$src"
  ( cd "$src" && git init -q && git add -A && git -c user.name=sbom -c user.email=sbom@invalid -c commit.gpgsign=false commit -q -m sbom \
    && GOOS=js GOARCH=wasm CGO_ENABLED=0 gomod app "${common[@]}" -main ./sdk/wasm -output "$out/sdk-wasm.cdx.json" . )
  normalize "$out/sdk-wasm.cdx.json"
fi

# 3. sdk-js: no modules — one file, its sha256, its version from the manifest.
want sdk-js || { ls "$out"/*.cdx.json; exit 0; }
js=sdk/js/e2ee/index.mjs
sum=$(if command -v sha256sum >/dev/null; then sha256sum "$js"; else shasum -a 256 "$js"; fi | cut -d' ' -f1)
version=$(jq -r '[.clients[] | select(.client == "sdk-js") | .version] | last' "${SBOM_MANIFEST:-internal/translog/clientbuilds/manifest.json}")
jq -n --arg sum "$sum" --arg version "$version" --arg commit "$commit" '{
  bomFormat: "CycloneDX", specVersion: "1.6", version: 1,
  metadata: {
    tools: { components: [{ type: "application", name: "scripts/sbom.sh", version: "1" }] },
    component: { type: "file", name: "sdk-js", version: $version,
      description: "The Stored E2EE JS binding sdk/js/e2ee/index.mjs, published as-is (scripts/build-client.sh sdk-js)",
      hashes: [{ alg: "SHA-256", content: $sum }],
      properties: [{ name: "stored:source_commit", value: $commit }, { name: "stored:recipe", value: "scripts/build-client.sh" }] }
  },
  components: [], dependencies: []
}' > "$out/sdk-js.cdx.json"

for f in "$out"/*.cdx.json; do
  echo "sbom: $f — $(jq -r '.metadata.component.version' "$f") — $(jq '.components | length' "$f") components"
done
