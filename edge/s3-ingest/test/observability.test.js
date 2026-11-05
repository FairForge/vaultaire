// WP-LOG-2 (D-LOG-1): Workers Logs stays enabled, invocation logs are OFF.
// Cloudflare's invocation event of a request carries its URL (the bucket and
// the key in clear), the client's IP, postal code, coordinates and user agent,
// and is kept 7 days; a data-plane host keeps no record of a request after it
// ends — the control plane's rows are the record. The committed wrangler.toml
// is what every deploy publishes, so the setting is held here.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

const read = (rel) => readFileSync(new URL(rel, import.meta.url), "utf8");

/** section returns the lines of a TOML table up to the next table header. */
export function section(toml, name) {
  const lines = toml.split("\n");
  const start = lines.findIndex((l) => l.trim() === `[${name}]`);
  if (start < 0) return null;
  const out = [];
  for (const l of lines.slice(start + 1)) {
    if (/^\s*\[/.test(l)) break;
    const t = l.replace(/#.*$/, "").trim();
    if (t) out.push(t);
  }
  return out;
}

test("wrangler.toml keeps observability enabled and invocation logs off (D-LOG-1)", () => {
  const toml = read("../wrangler.toml");
  const obs = section(toml, "observability");
  assert.ok(obs, "an [observability] table");
  assert.ok(obs.includes("enabled = true"), "[observability] enabled = true");
  const logs = section(toml, "observability.logs");
  assert.ok(logs, "an [observability.logs] table");
  assert.ok(logs.includes("invocation_logs = false"), "[observability.logs] invocation_logs = false");
});
