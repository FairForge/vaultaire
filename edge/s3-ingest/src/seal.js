// The 0x06 sealer (WP-EDGE-2 Part B): one codec chunk sealed exactly as
// internal/crypto/edge_codec.go's SealWrappedChunk seals it, over WebCrypto
// (AES-256-GCM, HKDF-SHA256) and the sdk's X-Wing wrap (storedE2EE.sealTo,
// e2ee.wasm — the client key core's one HPKE implementation). Nothing here
// is a new construction; the Go test TestEdgeCodec_NodeSealedChunkOpens
// opens what this file seals.
//
//   chunk key  = 32 random bytes (a dedup hit reuses the first writer's
//                blob, openable because its header wraps its own key)
//   wrap       = sealTo(codec public key, chunk key, "stored/edge-codec/chunk-key/v1", null) — 1169 bytes
//   header     = [0x06][0x03] ‖ key version u32 BE ‖ wrap ‖ segment size u32 BE ‖ nonce prefix (7 random)
//   DEK        = HKDF-SHA256(chunk key, no salt, info = "stored/edge-codec/stream-v1" ‖ header) → 32 bytes (hkdf32)
//   segment i  = AES-256-GCM(DEK, nonce = prefix ‖ i u32 BE ‖ (last ? 1 : 0), aad = header) of ≤ 1 MiB
//   blob       = header ‖ ct_0 ‖ … ‖ ct_{n-1}   (stream.go's streamNonce byte order)

export const FORMAT_WRAPPED = 0x06;
export const KIND_WRAPPED = 0x03;
export const SEGMENT_SIZE = 1 << 20;
export const WRAP_LEN = 1169;
export const NONCE_PREFIX = 7;
export const HEADER_LEN = 2 + 4 + WRAP_LEN + 4 + NONCE_PREFIX; // 1186
const TAG = 16;
const enc = new TextEncoder();
const CHUNK_KEY_INFO = enc.encode("stored/edge-codec/chunk-key/v1");
const STREAM_INFO = enc.encode("stored/edge-codec/stream-v1");

/** sealedLength is WrappedChunkSealedLength: a pure function of the size. */
export function sealedLength(size) {
  const segments = size <= 0 ? 1 : Math.ceil(size / SEGMENT_SIZE);
  return HEADER_LEN + size + segments * TAG;
}

function u32(v) {
  return [(v >>> 24) & 255, (v >>> 16) & 255, (v >>> 8) & 255, v & 255];
}

function nonce(prefix, i, last) {
  const n = new Uint8Array(12);
  n.set(prefix, 0);
  n.set(u32(i), NONCE_PREFIX);
  n[11] = last ? 1 : 0;
  return n;
}

async function hmac(key, data) {
  const k = await crypto.subtle.importKey("raw", key, { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  return new Uint8Array(await crypto.subtle.sign("HMAC", k, data));
}

/**
 * hkdf32 is HKDF-SHA256 (RFC 5869) to 32 bytes with NO salt — what Go's
 * hkdf.New(sha256.New, secret, nil, info) computes (a nil salt is HashLen
 * zero bytes): PRK = HMAC(zeros, secret), OKM = HMAC(PRK, info ‖ 0x01). By
 * hand over HMAC because a 0x06 DEK's info (27 + 1186 bytes) is past the
 * 1024-byte info limit of node's (OpenSSL's) HKDF deriveBits.
 */
export async function hkdf32(secret, info) {
  const prk = await hmac(new Uint8Array(32), secret);
  const t = new Uint8Array(info.length + 1);
  t.set(info, 0);
  t[info.length] = 1;
  const okm = await hmac(prk, t);
  prk.fill(0);
  return okm;
}

/**
 * sealSetup makes what one 0x06 blob is sealed under: a random chunk key
 * wrapped to the codec public key, the header, the DEK (a non-extractable
 * WebCrypto key) and the nonce prefix. The chunk key is zeroed before it
 * returns. Both sealers call it, in the same order of randomness.
 */
async function sealSetup(e2ee, pub, version, random) {
  if (!Number.isInteger(version) || version < 1) throw new Error(`edge codec: key version ${version}`);
  const chunkKey = random(new Uint8Array(32));
  const r = e2ee.sealTo(pub, chunkKey, CHUNK_KEY_INFO, null);
  if (r.error) throw new Error(`edge codec: wrap chunk key: ${r.error}`);
  const wrap = r.value;
  if (wrap.length !== WRAP_LEN) throw new Error(`edge codec: a wrapped chunk key is ${WRAP_LEN} bytes, sealed ${wrap.length}`);
  const header = new Uint8Array(HEADER_LEN);
  header[0] = FORMAT_WRAPPED;
  header[1] = KIND_WRAPPED;
  header.set(u32(version), 2);
  header.set(wrap, 6);
  header.set(u32(SEGMENT_SIZE), 6 + WRAP_LEN);
  const prefix = random(new Uint8Array(NONCE_PREFIX));
  header.set(prefix, 6 + WRAP_LEN + 4);

  const info = new Uint8Array(STREAM_INFO.length + HEADER_LEN);
  info.set(STREAM_INFO, 0);
  info.set(header, STREAM_INFO.length);
  const dekBits = await hkdf32(chunkKey, info);
  chunkKey.fill(0);
  const dek = await crypto.subtle.importKey("raw", dekBits, "AES-GCM", false, ["encrypt"]);
  dekBits.fill(0);
  return { header, dek, prefix };
}

async function sealSegment(setup, pt, i, last) {
  return new Uint8Array(await crypto.subtle.encrypt({ name: "AES-GCM", iv: nonce(setup.prefix, i, last), additionalData: setup.header, tagLength: 128 }, setup.dek, pt));
}

/**
 * sealChunk seals one chunk (a Uint8Array) as a 0x06 blob under the codec
 * public key of key version `version`, whole — the cross-implementation
 * vector's sealer (test/seal.test.js; the Go opener opens it). The pipeline
 * streams (sealChunkStream): no sealed copy of a chunk exists whole there.
 * e2ee is the storedE2EE module (its sealTo); `random` fills a Uint8Array
 * (crypto.getRandomValues).
 */
export async function sealChunk(e2ee, pub, version, chunk, random = (b) => crypto.getRandomValues(b)) {
  const setup = await sealSetup(e2ee, pub, version, random);
  const out = new Uint8Array(sealedLength(chunk.length));
  out.set(setup.header, 0);
  const segments = chunk.length <= 0 ? 1 : Math.ceil(chunk.length / SEGMENT_SIZE);
  let w = HEADER_LEN;
  for (let i = 0; i < segments; i++) {
    const pt = chunk.subarray(i * SEGMENT_SIZE, Math.min(chunk.length, (i + 1) * SEGMENT_SIZE));
    const ct = await sealSegment(setup, pt, i, i === segments - 1);
    out.set(ct, w);
    w += ct.length;
  }
  if (w !== out.length) throw new Error(`edge codec: sealed ${w} bytes, the length function says ${out.length}`);
  return out;
}

/**
 * sealChunkStream seals one chunk as a 0x06 blob STREAMED (RP70-1): the
 * chunk is its plaintext SEGMENTS (each its own buffer of SEGMENT_SIZE
 * bytes, the last ≤), and the returned {stream, length} yields the header,
 * then each segment's ciphertext encrypted when the consumer asks for it
 * (highWaterMark 0: nothing is sealed ahead of the PUT reading it). As each
 * segment is sealed its plaintext buffer is dropped from `segments` and
 * opts.onSegment(bytes) is called — the caller's byte semaphore releases
 * it. The bytes are exactly sealChunk's for the same randomness.
 */
export async function sealChunkStream(e2ee, pub, version, segments, opts = {}) {
  const random = opts.random || ((b) => crypto.getRandomValues(b));
  const onSegment = opts.onSegment || (() => {});
  const n = segments.length;
  if (n < 1) throw new Error("edge codec: a chunk has at least one segment");
  let size = 0;
  for (let k = 0; k < n; k++) {
    if (k < n - 1 && segments[k].length !== SEGMENT_SIZE) throw new Error(`edge codec: segment ${k} is ${segments[k].length} bytes, not ${SEGMENT_SIZE}`);
    size += segments[k].length;
  }
  const setup = await sealSetup(e2ee, pub, version, random);
  let i = -1;
  const stream = new ReadableStream({
    async pull(ctl) {
      if (i < 0) {
        ctl.enqueue(setup.header);
        i = 0;
        return;
      }
      const pt = segments[i];
      segments[i] = null;
      const ct = await sealSegment(setup, pt, i, i === n - 1);
      i++;
      ctl.enqueue(ct);
      onSegment(pt.length);
      if (i === n) ctl.close();
    },
  }, { highWaterMark: 0 });
  return { stream, length: sealedLength(size) };
}
