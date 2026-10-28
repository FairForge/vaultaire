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
