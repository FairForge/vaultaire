// WP-EDGE-2 Part A: the WASM chunker (sdk/chunkerwasm → wasm/chunker.wasm,
// built by `make edge-chunker-wasm`; `make test-edge` builds it first) cuts
// the SAME boundaries as Go's frozen chunker — asserted against the golden
// vectors internal/crypto/testdata/chunker_golden.json, which
// TestChunker_GoldenBoundaries pins on the Go side. The 100 MiB body is
// generated INSIDE the module (goldenBody — the same Go code), fed back
// through the public push API in uneven slices, so the js↔wasm byte path is
// on the line too. Skipped when the wasm has not been built (bare
// `node --test` without the Makefile).
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, existsSync } from "node:fs";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const wasmPath = join(here, "..", "wasm", "chunker.wasm");
const execPath = join(here, "..", "wasm", "wasm_exec.js");
const vectorsPath = join(here, "..", "..", "..", "internal", "crypto", "testdata", "chunker_golden.json");

async function loadChunker() {
  const require = createRequire(import.meta.url);
  require(execPath); // registers globalThis.Go
  const go = new globalThis.Go();
  const { instance } = await WebAssembly.instantiate(readFileSync(wasmPath), go.importObject);
  go.run(instance); // resolves never (the module serves); do not await
  await new Promise((res) => setTimeout(res, 10));
  assert.ok(globalThis.storedChunker, "the module registers storedChunker");
  return globalThis.storedChunker;
}

function val(r, what) {
  assert.equal(r.error, undefined, `${what}: ${r.error}`);
  return r.value;
}

test("the WASM chunker cuts the golden body at exactly Go's boundaries", { skip: !existsSync(wasmPath) && "run `make edge-chunker-wasm` first" }, async () => {
  const vectors = JSON.parse(readFileSync(vectorsPath, "utf8"));
  const chunker = await loadChunker();
  const body = val(chunker.goldenBody(vectors.seed, vectors.size), "goldenBody");
  assert.equal(body.length, vectors.size);

  const id = val(chunker.create(), "create");
  const lengths = [];
  // Uneven slices (64 KiB … ~8 MiB, a fixed pattern): boundaries must not
  // depend on how the stream arrives.
  const slices = [65536, 1048576, 262144, 8388608, 524288, 4194304];
  const started = Date.now();
  let off = 0;
  for (let i = 0; off < body.length; i++) {
    const n = Math.min(slices[i % slices.length], body.length - off);
    lengths.push(...val(chunker.push(id, body.subarray(off, off + n)), "push"));
    off += n;
  }
  lengths.push(...val(chunker.finish(id), "finish"));
  val(chunker.free(id), "free");
  const ms = Date.now() - started;

  assert.deepEqual(lengths, vectors.chunk_lengths, "the WASM boundaries are Go's, byte-identical");
  assert.equal(lengths.reduce((a, b) => a + b, 0), vectors.size);
  // Step 0.1's measurement, printed for the note (node, not workerd).
  console.log(`chunker.wasm: ${vectors.size >> 20} MiB in ${ms} ms (${Math.round(vectors.size / 1048576 / (ms / 1000))} MB/s in node)`);
});

test("boundaries reset per instance: each multipart part is chunked alone", { skip: !existsSync(wasmPath) && "run `make edge-chunker-wasm` first" }, async () => {
  const chunker = await loadChunker();
  const vectors = JSON.parse(readFileSync(vectorsPath, "utf8"));
  const body = val(chunker.goldenBody(vectors.seed, 8 << 20), "goldenBody");
  const one = val(chunker.create(), "create");
  const a = [...val(chunker.push(one, body), "push"), ...val(chunker.finish(one), "finish")];
  val(chunker.free(one), "free");
  const two = val(chunker.create(), "create");
  const b = [...val(chunker.push(two, body), "push"), ...val(chunker.finish(two), "finish")];
  val(chunker.free(two), "free");
  assert.deepEqual(a, b, "a fresh instance cuts the same bytes the same way");
  assert.equal(a.reduce((x, y) => x + y, 0), 8 << 20);
});
