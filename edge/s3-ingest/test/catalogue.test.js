// WP-EDGE-4 PR D: the public catalogue's filter in the Worker is the Go
// package (internal/catalogue) compiled into e2ee.wasm (sdk/wasm): it must
// answer the golden vectors byte for byte (internal/catalogue/testdata).
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { loadGo, noWasm } from "./wasm-loader.js";

const here = dirname(fileURLToPath(import.meta.url));
const golden = JSON.parse(readFileSync(join(here, "..", "..", "..", "internal", "catalogue", "testdata", "bloom_golden.json"), "utf8"));

test("catalogue: the WASM filter answers the Go goldens", { skip: noWasm }, async () => {
  const e2ee = await loadGo("e2ee.wasm", "storedE2EE");
  const bytes = Uint8Array.from(Buffer.from(golden.filter_hex, "hex"));
  const loaded = e2ee.catalogueLoad(bytes);
  assert.equal(loaded.error, undefined, loaded.error);
  assert.equal(loaded.value.version, golden.version);
  const h = loaded.value.handle;
  for (const x of golden.inserted) assert.equal(e2ee.catalogueContains(h, x).value, true, "never a false negative");
  golden.probes.forEach((x, i) => assert.equal(e2ee.catalogueContains(h, x).value, golden.probe_results[i], x));
  assert.ok(e2ee.catalogueContains(h, "zz").error, "not a hash");
  assert.ok(e2ee.catalogueLoad(new Uint8Array([2, 0, 0])).error, "not a filter");
  e2ee.catalogueFree(h);
  assert.ok(e2ee.catalogueContains(h, golden.inserted[0]).error, "freed");
});

import { catalogueFilter, resetCatalogue, runCodec } from "../src/codec.js";

function fakeFetch(files) {
  const calls = [];
  const f = async (url) => {
    calls.push(url);
    const b = files[url];
    if (b === undefined) return new Response("no", { status: 404 });
    return new Response(b, { status: 200 });
  };
  f.calls = calls;
  return f;
}

test("catalogueFilter: latest, then its filter, once per isolate; refreshed past an hour", { skip: noWasm }, async () => {
  const e2ee = await loadGo("e2ee.wasm", "storedE2EE");
  resetCatalogue();
  const filter = Buffer.from(golden.filter_hex, "hex");
  const doFetch = fakeFetch({
    "https://cat.test/catalogue/latest": String(golden.version),
    [`https://cat.test/catalogue/${golden.version}.bloom`]: filter,
  });
  const env = { CATALOGUE_URL: "https://cat.test/catalogue/" };
  assert.equal(await catalogueFilter({}, e2ee, doFetch), null, "no CATALOGUE_URL: no filter");
  const c = await catalogueFilter(env, e2ee, doFetch, 1000);
  assert.equal(c.version, golden.version);
  assert.equal(e2ee.catalogueContains(c.handle, golden.inserted[0]).value, true);
  await catalogueFilter(env, e2ee, doFetch, 2000);
  assert.equal(doFetch.calls.length, 2, "cached in the isolate");
  await catalogueFilter(env, e2ee, doFetch, 1000 + 3_600_001);
  assert.equal(doFetch.calls.length, 3, "past an hour: latest asked again, the same version not fetched twice");
  resetCatalogue();
  const broken = fakeFetch({});
  assert.equal(await catalogueFilter(env, e2ee, broken, 0), null, "an unreachable catalogue is no filter, never a failed write");
});

test("runCodec: a chunk the filter holds is sent public: true; others are not", { skip: noWasm }, async () => {
  const e2ee = await loadGo("e2ee.wasm", "storedE2EE");
  const chunker = await loadGo("chunker.wasm", "storedChunker");
  const body = chunker.goldenBody(4630, 6 << 20).value;
  const id = chunker.create().value;
  const hashes = [];
  // the first chunk's hash: what the catalogue holds
  const sizes = [...chunker.push(id, body).value, ...chunker.finish(id).value];
  chunker.free(id);
  const { createHash } = await import("node:crypto");
  let off = 0;
  for (const n of sizes) {
    hashes.push(createHash("sha256").update(body.subarray(off, off + n)).digest("hex"));
    off += n;
  }
  const indexBodies = [];
  const cp = async (path, b) => {
    if (path === "/api/v1/edge/index") {
      indexBodies.push(b);
      return new Response(JSON.stringify({ hits: b.chunks.map((c) => c.hash), misses: [] }), { status: 200 });
    }
    return new Response(JSON.stringify({ status: 200, headers: {}, body: "" }), { status: 200 });
  };
  const pub = e2ee.publicKey(new Uint8Array(32).fill(7)).value;
  const admit = { edge: true, admit_id: "a", codec: { version: 1, kem_public_key: Buffer.from(pub).toString("base64"), chunk_threshold: 4 << 20, segment_size: 1 << 20 } };
  const stream = new ReadableStream({ start(c) { c.enqueue(body); c.close(); } });
  const catalogue = { contains: (h) => h === hashes[0] };
  const relay = await runCodec({ headers: new Headers({ "content-length": String(body.length) }), body: stream }, admit, {}, {}, { chunker, e2ee, controlPlane: cp, catalogue });
  assert.equal(relay.status, 200);
  const sent = indexBodies.flatMap((b) => b.chunks);
  assert.equal(sent.find((c) => c.hash === hashes[0]).public, true);
  for (const c of sent.filter((x) => x.hash !== hashes[0])) assert.equal(c.public, undefined, "only what the filter holds");
});
