// The Private-mode key core for JavaScript (keystone K2): a thin binding over
// the Go module sdk/go/e2ee compiled to WebAssembly (sdk/wasm). There is no
// cryptography in this file — one implementation, the Go one.
//
//   <script src="wasm_exec.js"></script>            // Go's, shipped beside e2ee.wasm
//   import { load } from './index.mjs';
//   const e2ee = await load(await (await fetch('e2ee.wasm')).arrayBuffer());
//   const sealed = e2ee.seal(key, bytes, purpose);
//
// Constructions (see sdk/go/e2ee): seal/open — XChaCha20-Poly1305 box;
// chunkKey/sealChunk/openChunk — convergent chunks within a space;
// generateKeyPair/sealTo/openSealed — HPKE with the post-quantum hybrid
// X-Wing KEM (ML-KEM-768 + X25519). Every value is a Uint8Array.

export async function load(wasm) {
  if (typeof globalThis.Go !== 'function') {
    throw new Error("load Go's wasm_exec.js (shipped beside e2ee.wasm) before the key core");
  }
  const go = new globalThis.Go();
  const { instance } = await WebAssembly.instantiate(wasm, go.importObject);
  go.run(instance); // resolves only when the module exits; it never does
  const raw = globalThis.storedE2EE;
  if (!raw) throw new Error('the key core did not start');
  const unwrap = (r) => {
    if (r.error) throw new Error(r.error);
    return r;
  };
  const call = (name) => (...args) => unwrap(raw[name](...args)).value;
  return Object.freeze({
    version: raw.version,
    randomKey: call('randomKey'),
    seal: call('seal'),
    open: call('open'),
    chunkHash: call('chunkHash'),
    chunkKey: call('chunkKey'),
    sealChunk: call('sealChunk'),
    openChunk: call('openChunk'),
    generateKeyPair: () => {
      const r = unwrap(raw.generateKeyPair());
      return { privateKey: r.privateKey, publicKey: r.publicKey };
    },
    publicKey: call('publicKey'),
    sealTo: call('sealTo'),
    openSealed: call('openSealed'),
  });
}
