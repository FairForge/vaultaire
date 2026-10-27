// Loads a Go WebAssembly module of the Worker (src/wasm/, built by
// `make edge-wasm` — the client_build recipe) in node, the way the Worker
// does: one Go runtime per module, its API on globalThis.
import { readFileSync, existsSync } from "node:fs";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
export const wasmDir = join(here, "..", "src", "wasm");
export const haveWasm = existsSync(join(wasmDir, "chunker.wasm")) && existsSync(join(wasmDir, "e2ee.wasm"));
export const noWasm = !haveWasm && "run `make edge-wasm` first";

const loaded = {};
export async function loadGo(file, globalName) {
  if (loaded[globalName]) return loaded[globalName];
  const require = createRequire(import.meta.url);
  require(join(wasmDir, "wasm_exec.js")); // registers globalThis.Go
  const go = new globalThis.Go();
  const { instance } = await WebAssembly.instantiate(readFileSync(join(wasmDir, file)), go.importObject);
  go.run(instance); // never resolves: the module serves
  for (let i = 0; i < 100 && !globalThis[globalName]; i++) await new Promise((r) => setTimeout(r, 5));
  if (!globalThis[globalName]) throw new Error(`${file} did not register ${globalName}`);
  loaded[globalName] = globalThis[globalName];
  return loaded[globalName];
}
