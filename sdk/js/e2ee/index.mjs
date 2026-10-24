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
// chunkKey/sealChunk/openChunk(key, sealed, hash) — convergent chunks within
// a space, opened only against their hash; chunkId — the keyed id the server
// sees (deviceOnlyChunkHash never leaves the device);
// generateKeyPair/sealTo/openSealed — HPKE with the post-quantum hybrid
// X-Wing KEM (ML-KEM-768 + X25519).
//
// The hierarchy (K2b): the account root (wrapMasterKeyWithPassword with
// "sensitive" | "interactive", recovery key as 24 words, the vault key);
// the identity ({encPrivate, encPublic, sigPrivate, sigPublic}, fingerprint,
// sign/verify); spaces and signed shares (newShare/openShare refuse a share
// not signed by the sender you trust); objects and manifests (prepareChunk,
// sealManifest/openManifest with the newest {version, hash} you have seen,
// openChunkAt). Bytes are Uint8Array; a context or a mnemonic is a string; a
// version or a size is a number. Every error is thrown with Go's message.

const FUNCTIONS = [
  'randomKey', 'seal', 'open',
  'deviceOnlyChunkHash', 'chunkId', 'chunkKey', 'sealChunk', 'openChunk',
  'generateKeyPair', 'publicKey', 'sealTo', 'openSealed',
  'wrapMasterKeyWithPassword', 'openMasterKeyWithPassword', 'passwordWrapParams',
  'newRecoveryKey', 'recoveryMnemonic', 'parseRecoveryMnemonic',
  'wrapMasterKeyWithRecovery', 'openMasterKeyWithRecovery', 'wrapRecoveryKey', 'openRecoveryKey',
  'newVaultKey', 'wrapRecoveryKeyForVault', 'openRecoveryKeyFromVault',
  'generateIdentity', 'identityFromKeys', 'fingerprint', 'fingerprintString', 'sign', 'verify',
  'wrapIdentity', 'openIdentity',
  'newSpaceKey', 'wrapSpaceKey', 'openSpaceKey', 'newShare', 'openShare',
  'newObjectKey', 'wrapObjectKey', 'openObjectKey', 'prepareChunk',
  'sealManifest', 'manifestHash', 'openManifest', 'openChunkAt',
];

export async function load(wasm) {
  if (typeof globalThis.Go !== 'function') {
    throw new Error("load Go's wasm_exec.js (shipped beside e2ee.wasm) before the key core");
  }
  const go = new globalThis.Go();
  const { instance } = await WebAssembly.instantiate(wasm, go.importObject);
  go.run(instance); // resolves only when the module exits; it never does
  const raw = globalThis.storedE2EE;
  if (!raw) throw new Error('the key core did not start');
  const api = { version: raw.version };
  for (const name of FUNCTIONS) {
    if (typeof raw[name] !== 'function') throw new Error(`the key core has no ${name}`);
    api[name] = (...args) => {
      const r = raw[name](...args);
      if (r.error) throw new Error(r.error);
      return r.value;
    };
  }
  return Object.freeze(api);
}
