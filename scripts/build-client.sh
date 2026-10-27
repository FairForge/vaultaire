#!/usr/bin/env bash
# The ONE build recipe of a client artefact (K4c-3b, Stage 1a Prompt 36;
# docs/TRANSPARENCY.md § Client builds). The client_build entries of the
# transparency log record each release's sha256, source commit, toolchain and
# this script, so anyone rebuilds the artefact and compares — exactly
# build-release.sh's deal (RP37-9) for the clients:
#
#   - GOTOOLCHAIN = go.mod's `toolchain` line;
#   - CGO_ENABLED=0, -trimpath, -buildvcs=false, -ldflags=-s (the Makefile's
#     sdk-wasm recipe): the bytes depend only on the commit;
#   - sdk-js is published as-is: its "build" is a byte copy of the source.
#
# Usage: scripts/build-client.sh <client> <output>
#   sdk-wasm      sdk/go/e2ee compiled to WebAssembly (sdk/wasm)
#   sdk-js        the JS binding (sdk/js/e2ee/index.mjs)
#   edge-ingest   the mover Worker (edge/s3-ingest/src/index.js), published as
#                 the file itself (wrangler no_bundle; WP-EDGE-1 Part A)
#   edge-chunker  the frozen chunker compiled to WebAssembly for the edge
#                 codec's data-plane host (sdk/chunkerwasm → chunker.wasm;
#                 WP-EDGE-2 Part A)
#   edge-codec    the Worker's codec pipeline (edge/s3-ingest/src/codec.js),
#                 published as the file (WP-EDGE-2 Part B)
#   edge-seal     the Worker's 0x06 sealer (edge/s3-ingest/src/seal.js),
#                 published as the file (WP-EDGE-2 Part B)
#   edge-awschunked  the Worker's aws-chunked decoder and CRC tables
#                 (edge/s3-ingest/src/awschunked.js), published as the
#                 file (WP-EDGE-2 Part C)
#   edge-wasm-exec  the toolchain's wasm_exec.js, the Go runtime's JS half
#                 the Worker loads beside chunker.wasm and e2ee.wasm (go.mod's
#                 toolchain's GOROOT/lib/wasm; WP-EDGE-2 Part B)
set -euo pipefail

client=${1:?usage: build-client.sh <client> <output>}
out=${2:?usage: build-client.sh <client> <output>}
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

case "$client" in
sdk-wasm)
	toolchain=$(awk '$1 == "toolchain" { print $2 }' go.mod)
	if [ -z "$toolchain" ]; then
		echo "build-client: go.mod has no toolchain line" >&2
		exit 1
	fi
	GOTOOLCHAIN=$toolchain CGO_ENABLED=0 GOFLAGS= GOOS=js GOARCH=wasm \
		go build -trimpath -buildvcs=false -ldflags=-s -o "$out" ./sdk/wasm
	;;
sdk-js)
	cp sdk/js/e2ee/index.mjs "$out"
	;;
edge-ingest)
	cp edge/s3-ingest/src/index.js "$out"
	;;
edge-chunker)
	toolchain=$(awk '$1 == "toolchain" { print $2 }' go.mod)
	if [ -z "$toolchain" ]; then
		echo "build-client: go.mod has no toolchain line" >&2
		exit 1
	fi
	GOTOOLCHAIN=$toolchain CGO_ENABLED=0 GOFLAGS= GOOS=js GOARCH=wasm \
		go build -trimpath -buildvcs=false -ldflags=-s -o "$out" ./sdk/chunkerwasm
	;;
edge-codec)
	cp edge/s3-ingest/src/codec.js "$out"
	;;
edge-seal)
	cp edge/s3-ingest/src/seal.js "$out"
	;;
edge-awschunked)
	cp edge/s3-ingest/src/awschunked.js "$out"
	;;
edge-wasm-exec)
	toolchain=$(awk '$1 == "toolchain" { print $2 }' go.mod)
	if [ -z "$toolchain" ]; then
		echo "build-client: go.mod has no toolchain line" >&2
		exit 1
	fi
	goroot=$(GOTOOLCHAIN=$toolchain GOFLAGS= go env GOROOT)
	install -m 0644 "$goroot/lib/wasm/wasm_exec.js" "$out"
	;;
*)
	echo "build-client: unknown client $client (sdk-wasm, sdk-js, edge-ingest, edge-chunker, edge-codec, edge-seal, edge-wasm-exec, edge-awschunked)" >&2
	exit 1
	;;
esac
