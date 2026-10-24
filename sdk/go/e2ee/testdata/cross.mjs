// Driven by TestE2EE_WASMMatchesNative: loads the WASM key core through the
// real binding (sdk/js/e2ee/index.mjs) and answers the native test.
// Usage: node cross.mjs <wasm_exec.js> <index.mjs> <e2ee.wasm> <in.json> <vectors.json>
//
// It reports three things: `goldens` — every vector of vectors.json checked
// through the binding ("ok" or the failure); `opened` — what native Go sealed
// (in.json), opened in WASM; `sealed` — fresh values of every construction
// made in WASM, for native Go to open.
import { readFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';

const [wasmExec, binding, wasmPath, inPath, vectorsPath] = process.argv.slice(2);
await import(pathToFileURL(wasmExec).href);
const { load } = await import(pathToFileURL(binding).href);
const e2ee = await load(await readFile(wasmPath));
const inp = JSON.parse(await readFile(inPath, 'utf8'));
const v = JSON.parse(await readFile(vectorsPath, 'utf8'));
const h = (s) => Uint8Array.from(Buffer.from(s, 'hex'));
const x = (b) => Buffer.from(b).toString('hex');
const enc = new TextEncoder();
const eq = (a, b) => x(a) === x(b);

const goldens = {};
const check = (name, f) => {
  try {
    goldens[name] = f() === true ? 'ok' : 'mismatch';
  } catch (err) {
    goldens[name] = 'threw: ' + err.message;
  }
};
const idOf = (iv) => e2ee.identityFromKeys(h(iv.enc_private), h(iv.sig_private));

// --- goldens ---
check('box', () => eq(e2ee.open(h(v.box.key), h(v.box.msg), h(v.box.ad)), h(v.box.plaintext)));
check('sealed', () => eq(e2ee.openSealed(h(v.sealed.seed), h(v.sealed.msg), h(v.sealed.info), h(v.sealed.ad)), h(v.sealed.plaintext)));
check('sealed.pub', () => eq(e2ee.publicKey(h(v.sealed.seed)), h(v.sealed.pub)));
check('password_wrap', () => eq(e2ee.openMasterKeyWithPassword(h(v.password_wrap.wrapped), h(v.password_wrap.password)), h(v.password_wrap.master_key)));
check('password_wrap.params', () => {
  const p = e2ee.passwordWrapParams(h(v.password_wrap.wrapped));
  return p.memoryKiB === v.password_wrap.memory_kib && p.iterations === v.password_wrap.iterations && p.parallelism === v.password_wrap.parallelism;
});
const openers = {
  master_under_recovery: e2ee.openMasterKeyWithRecovery,
  recovery_under_master: e2ee.openRecoveryKey,
  recovery_under_vault: e2ee.openRecoveryKeyFromVault,
};
for (const [name, w] of Object.entries(v.key_wraps)) {
  check('key_wraps.' + name, () => eq(openers[name](h(w.wrapping_key), h(w.wrapped)), h(w.key)));
}
v.bip39.forEach((b, i) => {
  check(`bip39[${i}]`, () => e2ee.recoveryMnemonic(h(b.entropy)) === b.mnemonic && eq(e2ee.parseRecoveryMnemonic(b.mnemonic), h(b.entropy)));
});
for (const [name, iv] of Object.entries(v.identities)) {
  check(`identities.${name}`, () => {
    const id = idOf(iv);
    const fp = e2ee.fingerprint(id);
    const opened = e2ee.openIdentity(h(iv.master_key), h(iv.wrapped_enc), h(iv.wrapped_sig));
    return eq(id.encPublic, h(iv.enc_public)) && eq(id.sigPublic, h(iv.sig_public)) && eq(fp, h(iv.fingerprint)) &&
      e2ee.fingerprintString(fp) === iv.fingerprint_string &&
      eq(e2ee.sign(id.sigPrivate, iv.sign_context, h(iv.sign_message)), h(iv.signature)) &&
      e2ee.verify(id.sigPublic, iv.sign_context, h(iv.sign_message), h(iv.signature)) === true &&
      eq(opened.sigPrivate, id.sigPrivate) && eq(opened.encPrivate, id.encPrivate);
  });
}
check('chunk_id', () => eq(e2ee.chunkId(h(v.chunk_id.space_key), e2ee.deviceOnlyChunkHash(h(v.chunk_id.chunk))), h(v.chunk_id.chunk_id)));
check('bound_wraps.space_key_owner', () => {
  const w = v.bound_wraps.space_key_owner;
  return eq(e2ee.openSpaceKey(h(w.wrapping_key), h(w.space_id), h(w.wrapped)), h(w.key));
});
check('bound_wraps.object_key', () => {
  const w = v.bound_wraps.object_key;
  return eq(e2ee.openObjectKey(h(w.wrapping_key), h(w.space_id), h(w.object_id), h(w.wrapped)), h(w.key));
});
{
  const o = v.object;
  let seen = null;
  for (const mv of o.versions) {
    check(`object.v${mv.version}`, () => {
      const m = e2ee.openManifest(h(o.object_key), h(mv.manifest), h(o.space_id), h(o.object_id), mv.version, seen);
      let ok = m.version === mv.version && eq(m.previous, h(mv.previous)) && eq(e2ee.manifestHash(h(mv.manifest)), h(mv.hash));
      mv.chunks.forEach((c, i) => {
        ok = ok && eq(e2ee.openChunkAt(m, i, h(mv.sealed_chunks[i])), h(c));
        ok = ok && eq(e2ee.prepareChunk(h(o.space_key), h(c)).sealed, h(mv.sealed_chunks[i]));
      });
      return ok;
    });
    seen = { version: mv.version, hash: h(mv.hash) };
  }
  check('object.rollback refused', () => {
    try {
      e2ee.openManifest(h(o.object_key), h(o.versions[0].manifest), h(o.space_id), h(o.object_id), 1, seen);
      return false;
    } catch (err) {
      return err.message.includes('older');
    }
  });
}
check('share', () => {
  const alice = idOf(v.identities[v.share.sender]);
  const bob = idOf(v.identities[v.share.recipient]);
  return eq(e2ee.openShare(bob, h(v.share.share), alice, h(v.share.space_id)), h(v.share.space_key));
});
check('share from another signer refused', () => {
  const mallory = idOf(v.identities.mallory);
  const bob = idOf(v.identities[v.share.recipient]);
  try {
    e2ee.openShare(bob, h(v.share.share), mallory, h(v.share.space_id));
    return false;
  } catch (err) {
    return err.message.includes('trusted sender');
  }
});

// --- native → WASM ---
const opened = {};
opened.box = x(e2ee.open(h(inp.key), h(inp.box), enc.encode(inp.ad)));
opened.chunkHash = x(e2ee.deviceOnlyChunkHash(h(inp.chunk)));
opened.chunkId = x(e2ee.chunkId(h(inp.spaceKey), h(opened.chunkHash)));
opened.chunkKey = x(e2ee.chunkKey(h(inp.spaceKey), h(opened.chunkHash)));
opened.chunkSealed = x(e2ee.sealChunk(h(opened.chunkKey), h(inp.chunk)));
opened.chunk = x(e2ee.openChunk(h(opened.chunkKey), h(inp.nativeChunk), h(opened.chunkHash)));
opened.sealed = x(e2ee.openSealed(h(inp.priv), h(inp.sealed), enc.encode(inp.info), enc.encode(inp.ad)));
opened.passwordWrap = x(e2ee.openMasterKeyWithPassword(h(inp.passwordWrap), enc.encode(inp.password)));
opened.recoveryKey = x(e2ee.parseRecoveryMnemonic(inp.mnemonic));
opened.share = x(e2ee.openShare(idOf(v.identities.bob), h(inp.share), idOf(v.identities.alice), h(inp.spaceId)));
{
  const m = e2ee.openManifest(h(inp.objectKey), h(inp.manifest), h(inp.spaceId), h(inp.objectId), 1, null);
  opened.manifestChunks = m.chunks.map((_, i) => x(e2ee.openChunkAt(m, i, h(inp.sealedChunks[i]))));
}

// --- WASM → native ---
const sealed = {};
sealed.box = x(e2ee.seal(h(inp.key), enc.encode('from wasm'), enc.encode(inp.ad)));
sealed.sealedToNative = x(e2ee.sealTo(h(inp.pub), enc.encode('from wasm to native'), enc.encode(inp.info), enc.encode(inp.ad)));
const kp = e2ee.generateKeyPair();
sealed.wasmPriv = x(kp.privateKey);
sealed.wasmPub = x(kp.publicKey);
sealed.wasmPubAgain = x(e2ee.publicKey(kp.privateKey));
sealed.passwordWrap = x(e2ee.wrapMasterKeyWithPassword(h(inp.masterKey), enc.encode(inp.password), 'interactive'));
sealed.mnemonic = e2ee.recoveryMnemonic(h(inp.recoveryKey));
sealed.recoveryWrap = x(e2ee.wrapMasterKeyWithRecovery(h(inp.recoveryKey), h(inp.masterKey)));
sealed.recoveryKeyWrap = x(e2ee.wrapRecoveryKey(h(inp.masterKey), h(inp.recoveryKey)));
const vault = e2ee.newVaultKey();
sealed.vaultKey = x(vault);
sealed.vaultWrap = x(e2ee.wrapRecoveryKeyForVault(vault, h(inp.recoveryKey)));
const id = e2ee.generateIdentity();
const wrapped = e2ee.wrapIdentity(h(inp.masterKey), id);
sealed.identityEnc = x(wrapped.wrappedEnc);
sealed.identitySig = x(wrapped.wrappedSig);
sealed.identityEncPublic = x(id.encPublic);
sealed.identitySigPublic = x(id.sigPublic);
sealed.fingerprint = x(e2ee.fingerprint(id));
sealed.signature = x(e2ee.sign(id.sigPrivate, 'cross', enc.encode('signed in wasm')));
sealed.spaceKeyWrap = x(e2ee.wrapSpaceKey(h(inp.masterKey), h(inp.spaceId), h(inp.spaceKey)));
sealed.share = x(e2ee.newShare(idOf(v.identities.bob), idOf(v.identities.alice), h(inp.spaceId), h(inp.spaceKey)));
const objectKey = e2ee.newObjectKey();
sealed.objectKey = x(objectKey);
sealed.objectKeyWrap = x(e2ee.wrapObjectKey(h(inp.spaceKey), h(inp.spaceId), h(inp.objectId), objectKey));
{
  const parts = ['first chunk sealed in wasm', 'second one'].map((s) => e2ee.prepareChunk(h(inp.spaceKey), enc.encode(s)));
  const m = {
    spaceId: h(inp.spaceId), objectId: h(inp.objectId), version: 1, previous: new Uint8Array(32), compressor: 0,
    totalSize: parts.reduce((n, p) => n + p.ref.size, 0), chunks: parts.map((p) => p.ref),
  };
  sealed.manifest = x(e2ee.sealManifest(objectKey, m));
  sealed.manifestChunks = parts.map((p) => x(p.sealed));
}

// Errors arrive as thrown Go messages.
const errors = {};
try {
  e2ee.open(h(inp.key), h(inp.box), enc.encode('another purpose'));
  errors.wrongPurpose = 'opened';
} catch (err) {
  errors.wrongPurpose = String(err.message);
}
try {
  e2ee.seal(Uint8Array.of(1, 2, 3), enc.encode('x'));
  errors.shortKey = 'sealed';
} catch (err) {
  errors.shortKey = String(err.message);
}

// Exit once the pipe has the bytes (an early exit cuts an async pipe write;
// the Go module never exits on its own).
process.stdout.write(JSON.stringify({ version: e2ee.version, goldens, opened, sealed, errors }), () => process.exit(0));
