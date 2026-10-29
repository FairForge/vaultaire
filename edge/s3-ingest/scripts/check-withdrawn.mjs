// The Worker deploy's refusal of a withdrawn build (RP78-10, Stage 1a
// Prompt 50): every module wrangler would publish from src/ is hashed and
// refused when the manifest's `withdrawn` list names its sha256
// (internal/translog/clientbuilds/manifest.json — a row and its log entry
// are never removed; a withdrawal is its own list). `npm run deploy` runs it
// first (predeploy); a withdrawn module fails the deploy before wrangler
// starts.
import { createHash } from "node:crypto";
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";

/** modules lists the files under dir (what find_additional_modules publishes). */
export function modules(dir) {
  const out = [];
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) out.push(...modules(p));
    else out.push(p);
  }
  return out;
}

/** withdrawnModules is every file of dir whose sha256 the manifest withdrew. */
export function withdrawnModules(dir, manifest) {
  const withdrawn = new Map((manifest.withdrawn || []).map((w) => [w.sha256, w]));
  const hits = [];
  for (const p of modules(dir)) {
    const sum = createHash("sha256").update(readFileSync(p)).digest("hex");
    const w = withdrawn.get(sum);
    if (w) hits.push({ file: relative(dir, p), sha256: sum, client: w.client, reason: w.reason, withdrawn_at: w.withdrawn_at });
  }
  return hits;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const here = fileURLToPath(new URL("..", import.meta.url));
  const manifest = JSON.parse(readFileSync(join(here, "../../internal/translog/clientbuilds/manifest.json"), "utf8"));
  const hits = withdrawnModules(join(here, "src"), manifest);
  for (const h of hits) {
    console.error(`refused: src/${h.file} is ${h.client} sha256 ${h.sha256}, withdrawn on ${h.withdrawn_at}: ${h.reason}`);
  }
  if (hits.length > 0) process.exit(1);
  console.log("check-withdrawn: no module of src/ is a withdrawn build");
}
