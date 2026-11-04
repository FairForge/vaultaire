// RP96-22 (0.7.3): the Worker's version lives in three places — package.json,
// the README's newest heading and the client_build manifest's rows — and a
// test holds them together: the README's newest version IS package.json's,
// and no manifest row names a version past it (the row of a new version
// lands in the follow-up PR, from its merge commit, so it may be one behind).
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

const read = (rel) => readFileSync(new URL(rel, import.meta.url), "utf8");
const cmp = (a, b) => {
  const x = a.split(".").map(Number);
  const y = b.split(".").map(Number);
  for (let i = 0; i < 3; i++) if (x[i] !== y[i]) return x[i] - y[i];
  return 0;
};
const newest = (vs) => vs.slice().sort(cmp).at(-1);

test("package.json's version is the README's newest and no manifest row is past it", () => {
  const version = JSON.parse(read("../package.json")).version;
  assert.match(version || "", /^\d+\.\d+\.\d+$/, "package.json names a version");
  const readme = [...read("../README.md").matchAll(/^## (\d+\.\d+\.\d+) /gm)].map((m) => m[1]);
  assert.equal(newest(readme), version, "the README's newest heading");
  const manifest = JSON.parse(read("../../../internal/translog/clientbuilds/manifest.json"));
  const rows = manifest.clients.filter((c) => c.client === "edge-ingest").map((c) => c.version);
  assert.ok(rows.length > 0);
  assert.ok(cmp(newest(rows), version) <= 0, `a manifest row (${newest(rows)}) past package.json (${version})`);
});
