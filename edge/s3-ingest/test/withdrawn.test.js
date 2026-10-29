// RP78-10: the Worker deploy refuses a module the manifest withdrew.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createHash } from "node:crypto";
import { withdrawnModules } from "../scripts/check-withdrawn.mjs";

test("check-withdrawn: a withdrawn module is refused; the others pass", () => {
  const dir = mkdtempSync(join(tmpdir(), "withdrawn-"));
  mkdirSync(join(dir, "wasm"));
  writeFileSync(join(dir, "codec.js"), "the withdrawn bytes");
  writeFileSync(join(dir, "index.js"), "fine");
  writeFileSync(join(dir, "wasm", "x.wasm"), "fine too");
  const sum = createHash("sha256").update("the withdrawn bytes").digest("hex");
  const manifest = { clients: [], withdrawn: [{ client: "edge-codec", sha256: sum, reason: "411", withdrawn_at: "2026-10-29" }] };
  const hits = withdrawnModules(dir, manifest);
  assert.equal(hits.length, 1);
  assert.equal(hits[0].file, "codec.js");
  assert.equal(hits[0].client, "edge-codec");
  assert.deepEqual(withdrawnModules(dir, { clients: [] }), [], "no withdrawn list: nothing refused");
});

test("check-withdrawn: the repository's src/ is no withdrawn build", () => {
  const manifest = JSON.parse(readFileSync(new URL("../../../internal/translog/clientbuilds/manifest.json", import.meta.url), "utf8"));
  assert.ok((manifest.withdrawn || []).length >= 1, "0.5.1's edge-codec is withdrawn");
  assert.deepEqual(withdrawnModules(new URL("../src", import.meta.url).pathname, manifest), []);
});
