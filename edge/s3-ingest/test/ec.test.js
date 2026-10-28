// WP-EDGE-4 PR B: the Reed-Solomon stripe codec (internal/ec) in
// WebAssembly (sdk/ecwasm → ec.wasm, `make ec-wasm`) answers the Go golden
// vectors byte for byte, rebuilds from any k of k+m, and — Step 0.2 — its
// rate is measured here against the native benchmark.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, existsSync } from "node:fs";
import { createHash } from "node:crypto";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { loadGo, wasmDir } from "./wasm-loader.js";

const here = dirname(fileURLToPath(import.meta.url));
const golden = JSON.parse(readFileSync(join(here, "..", "..", "..", "internal", "ec", "testdata", "ec_golden.json"), "utf8"));
const noEC = !existsSync(join(wasmDir, "ec.wasm")) && "run `make ec-wasm` first";

function shardsOf(ec, k, m, s, body) {
  const stripes = Math.ceil(body.length / (k * s));
  const shards = Array.from({ length: k + m }, () => new Uint8Array(stripes * s));
  for (let st = 0; st < stripes; st++) {
    const data = new Uint8Array(k * s);
    data.set(body.subarray(st * k * s, Math.min(body.length, (st + 1) * k * s)));
    const parity = ec.encodeStripe(k, m, data);
    assert.equal(parity.error, undefined, parity.error);
    for (let i = 0; i < k; i++) shards[i].set(data.subarray(i * s, (i + 1) * s), st * s);
    for (let j = 0; j < m; j++) shards[k + j].set(parity.value.subarray(j * s, (j + 1) * s), st * s);
  }
  return shards;
}

test("ec: the WASM codec's shards are the Go goldens", { skip: noEC }, async () => {
  const ec = await loadGo("ec.wasm", "storedEC");
  const body = ec.goldenBody(golden.Seed, golden.Size).value;
  const shards = shardsOf(ec, golden.K, golden.M, golden.Stripe, body);
  shards.forEach((sh, i) => assert.equal(createHash("sha256").update(sh).digest("hex"), golden.shard_sha256[i], `shard ${i}`));
});

test("ec: any k of k+m rebuild a stripe", { skip: noEC }, async () => {
  const ec = await loadGo("ec.wasm", "storedEC");
  const k = 4, m = 2, s = 4096;
  const data = ec.goldenBody(11, k * s).value;
  const parity = ec.encodeStripe(k, m, data).value;
  const pieces = [];
  for (let i = 0; i < k; i++) pieces.push(data.subarray(i * s, (i + 1) * s));
  for (let j = 0; j < m; j++) pieces.push(parity.subarray(j * s, (j + 1) * s));
  for (let a = 0; a < k + m; a++) {
    for (let b = a + 1; b < k + m; b++) {
      const lost = pieces.map((p, i) => (i === a || i === b ? null : p));
      const r = ec.reconstruct(k, m, lost);
      assert.equal(r.error, undefined, r.error);
      assert.deepEqual(Buffer.from(r.value), Buffer.from(data), `lost ${a},${b}`);
    }
  }
  assert.ok(ec.reconstruct(k, m, pieces.map((p, i) => (i < 3 ? null : p))).error, "k−1 left");
});

test("ec: Step 0.2 — the WASM rate at 4+2 over 64 MiB", { skip: noEC }, async () => {
  const ec = await loadGo("ec.wasm", "storedEC");
  const k = 4, m = 2, s = 2 << 20; // 8 MiB stripes
  const body = ec.goldenBody(64, 64 << 20).value;
  const t0 = performance.now();
  shardsOf(ec, k, m, s, body);
  const ms = performance.now() - t0;
  const rate = (64 / (ms / 1000)).toFixed(1);
  console.log(`ec wasm 4+2: 64 MiB in ${ms.toFixed(0)} ms = ${rate} MiB/s`);
  assert.ok(ms > 0);
});
