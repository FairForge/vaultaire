// Driven by TestE2EE_WASMMatchesNative: loads the WASM key core through the
// real binding (sdk/js/e2ee/index.mjs) and answers the native test's
// questions. Usage: node cross.mjs <wasm_exec.js> <index.mjs> <e2ee.wasm> <in.json>
import { readFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';

const [wasmExec, binding, wasmPath, inPath] = process.argv.slice(2);
await import(pathToFileURL(wasmExec).href);
const { load } = await import(pathToFileURL(binding).href);
const e2ee = await load(await readFile(wasmPath));
const inp = JSON.parse(await readFile(inPath, 'utf8'));
const h = (s) => Uint8Array.from(Buffer.from(s, 'hex'));
const x = (b) => Buffer.from(b).toString('hex');
const enc = new TextEncoder();

const out = { version: e2ee.version };
// Native → WASM.
out.boxOpened = x(e2ee.open(h(inp.key), h(inp.box), enc.encode(inp.ad)));
out.chunkHash = x(e2ee.deviceOnlyChunkHash(h(inp.chunk)));
out.chunkId = x(e2ee.chunkId(h(inp.spaceKey), h(out.chunkHash)));
out.chunkKey = x(e2ee.chunkKey(h(inp.spaceKey), h(out.chunkHash)));
out.chunkSealed = x(e2ee.sealChunk(h(out.chunkKey), h(inp.chunk)));
out.chunkOpened = x(e2ee.openChunk(h(out.chunkKey), h(inp.nativeChunk), h(out.chunkHash)));
out.sealedOpened = x(e2ee.openSealed(h(inp.priv), h(inp.sealed), enc.encode(inp.info), enc.encode(inp.ad)));
// WASM → native.
out.box = x(e2ee.seal(h(inp.key), enc.encode('from wasm'), enc.encode(inp.ad)));
out.sealedToNative = x(e2ee.sealTo(h(inp.pub), enc.encode('from wasm to native'), enc.encode(inp.info), enc.encode(inp.ad)));
const kp = e2ee.generateKeyPair();
out.wasmPriv = x(kp.privateKey);
out.wasmPub = x(kp.publicKey);
out.wasmPubAgain = x(e2ee.publicKey(kp.privateKey));
// Errors are thrown, with the Go message.
try {
  e2ee.open(h(inp.key), h(inp.box), enc.encode('another purpose'));
  out.wrongPurpose = 'opened';
} catch (err) {
  out.wrongPurpose = String(err.message);
}
try {
  e2ee.seal(Uint8Array.of(1, 2, 3), enc.encode('x'));
  out.shortKey = 'sealed';
} catch (err) {
  out.shortKey = String(err.message);
}
// Exit once the pipe has the bytes (an early exit cuts an async pipe write;
// the Go module never exits on its own).
process.stdout.write(JSON.stringify(out), () => process.exit(0));
