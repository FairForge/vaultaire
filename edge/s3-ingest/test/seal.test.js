// WP-EDGE-2 Part B: the Worker's 0x06 sealer (src/seal.js) against the Go
// opener. The Go test TestEdgeCodec_ExportVectorForNode writes a lab key
// pair (a FIXED seed) to internal/crypto/testdata/edge_codec_node.json; this
// test seals the golden 3 MiB chunk under its public key and writes the blob
// to test/out/ (gitignored); TestEdgeCodec_NodeSealedChunkOpens opens it with
// crypto.OpenWrappedChunk under the same seed and compares the plaintext
// (`make test-edge` runs it right after this file).
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, writeFileSync, mkdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { loadGo, noWasm } from "./wasm-loader.js";
import { HEADER_LEN, WRAP_LEN, sealChunk, sealedLength } from "../src/seal.js";

const here = dirname(fileURLToPath(import.meta.url));
const vector = JSON.parse(readFileSync(join(here, "..", "..", "..", "internal", "crypto", "testdata", "edge_codec_node.json"), "utf8"));

test("sealedLength is WrappedChunkSealedLength (the Go vector's)", () => {
  assert.equal(HEADER_LEN, 1186);
  assert.equal(WRAP_LEN, 1169);
  for (const [size, want] of Object.entries(vector.sealed_lengths)) assert.equal(sealedLength(Number(size)), want, `size ${size}`);
});

test("sealChunk: a 0x06 blob of the golden 3 MiB chunk under the lab public key (opened by the Go test)", { skip: noWasm }, async () => {
  const e2ee = await loadGo("e2ee.wasm", "storedE2EE");
  const chunker = await loadGo("chunker.wasm", "storedChunker");
  const pub = Buffer.from(vector.public_key, "base64");
  assert.equal(pub.length, 1216, "the X-Wing public key");
  const plain = chunker.goldenBody(vector.chunk_seed, vector.chunk_size).value;
  const started = Date.now();
  const sealed = await sealChunk(e2ee, new Uint8Array(pub), vector.key_version, plain);
  const ms = Date.now() - started;
  assert.equal(sealed.length, sealedLength(plain.length));
  assert.equal(sealed[0], 0x06);
  assert.equal(sealed[1], 0x03);
  assert.deepEqual([...sealed.subarray(2, 6)], [0, 0, 0, vector.key_version]);
  assert.deepEqual([...sealed.subarray(6 + WRAP_LEN, 10 + WRAP_LEN)], [0, 0x10, 0, 0], "segment size 1 MiB, big-endian");
  // Two seals of the same chunk differ (a random chunk key and nonce prefix).
  const again = await sealChunk(e2ee, new Uint8Array(pub), vector.key_version, plain);
  assert.notDeepEqual(Buffer.from(again.subarray(0, 64)), Buffer.from(sealed.subarray(0, 64)));
  mkdirSync(join(here, "out"), { recursive: true });
  writeFileSync(join(here, "out", "node_sealed_chunk.sealed"), sealed);
  console.log(`sealChunk: ${vector.chunk_size >> 20} MiB in ${ms} ms`);
});

test("sealChunk refuses a version below 1", { skip: noWasm }, async () => {
  const e2ee = await loadGo("e2ee.wasm", "storedE2EE");
  await assert.rejects(sealChunk(e2ee, new Uint8Array(Buffer.from(vector.public_key, "base64")), 0, new Uint8Array(4)), /key version/);
});
