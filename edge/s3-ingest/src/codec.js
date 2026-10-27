// The codec pipeline (WP-EDGE-2 Part B, plan 40.10 slice 2): the Worker
// 0.3.0 as the edge codec's data-plane host — what Part A's Go harness
// (internal/api/edge_codec_harness_test.go) does, in the Worker. An admit
// that answers `codec` (and no url) hands this Worker the body to CHUNK
// (chunker.wasm — the frozen Go chunker, byte-identical boundaries), HASH
// (sha256 per chunk, WebCrypto), ASK the index (POST /api/v1/edge/index, ≤ 64
// chunks a call), SEAL every miss as one 0x06 blob (seal.js) and PUT it on
// its presigned link, then COMMIT the ordered chunk list with the body's MD5
// (MD5 below — WebCrypto has none). The control plane keys every hash in memory and stores keyed
// hashes only (D-EDGE-16); this file holds no key but the admit's public
// one and the per-chunk keys it makes.
//
// Back-pressure: at most CODEC_IN_FLIGHT_BYTES (48 MiB) of chunk PLAINTEXT is
// held between a chunk's cut and the moment its last segment is sealed (a
// byte semaphore the reader awaits before it takes the next chunk — the body
// is read no faster than the chunks leave; RP70-1: the bound was 4 CHUNKS,
// and four 16 MiB chunks plus their whole sealed copies were the isolate).
// A chunk is held as its 1 MiB SEGMENTS, each its own buffer: the PUT body
// is a stream that seals a segment when the backend reads it, drops its
// plaintext and releases its bytes, so no sealed copy of a chunk exists
// whole and the plaintext goes as it is sent. An index batch goes out at 64
// chunks, at the end of the body, or when the reader is about to wait on the
// bound (D-EDGE-22: a batch can never hold more than the bound lets exist —
// waiting for 64 would deadlock).
//
// The 503 rule: once the body has been read from, the request can never be
// passed to the origin (its body is gone). Any failure is a commit
// {outcome: "edge_error"} (the control plane gives back the reservation and
// drops the claims) and a 503 SlowDown + Retry-After 30 to the client.

import { SEGMENT_SIZE, sealChunkStream } from "./seal.js";

/** s3ErrorXML renders the S3 error document the origin's writer would. */
export function s3ErrorXML(code, message, path, requestId) {
  const esc = (s) => String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  return `<?xml version="1.0" encoding="UTF-8"?>\n<Error><Code>${esc(code)}</Code><Message>${esc(message)}</Message><Resource>${esc(path)}</Resource><RequestId>${esc(requestId)}</RequestId></Error>`;
}

// --- MD5 (RFC 1321) ---------------------------------------------------------

const S = [7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22, 5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20,
  4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23, 6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21];
const K = new Int32Array(64);
for (let i = 0; i < 64; i++) K[i] = Math.floor(Math.abs(Math.sin(i + 1)) * 2 ** 32) | 0;

/** MD5 is an incremental MD5 (RFC 1321): the codec commit's body_md5 — the
 * PUT's ETag. WebCrypto has no MD5; never a security property. */
export class MD5 {
  constructor() {
    this.h = new Int32Array([0x67452301, 0xefcdab89 | 0, 0x98badcfe | 0, 0x10325476]);
    this.buf = new Uint8Array(64);
    this.bufLen = 0;
    this.total = 0;
    this.w = new Int32Array(16);
  }

  /** update feeds bytes (a Uint8Array). */
  update(data) {
    let off = 0;
    this.total += data.length;
    if (this.bufLen > 0) {
      const take = Math.min(64 - this.bufLen, data.length);
      this.buf.set(data.subarray(0, take), this.bufLen);
      this.bufLen += take;
      off = take;
      if (this.bufLen < 64) return this;
      this.block(this.buf, 0);
      this.bufLen = 0;
    }
    for (; off + 64 <= data.length; off += 64) this.block(data, off);
    if (off < data.length) {
      this.buf.set(data.subarray(off), 0);
      this.bufLen = data.length - off;
    }
    return this;
  }

  block(b, o) {
    const w = this.w;
    for (let i = 0; i < 16; i++) w[i] = b[o + 4 * i] | (b[o + 4 * i + 1] << 8) | (b[o + 4 * i + 2] << 16) | (b[o + 4 * i + 3] << 24);
    let a = this.h[0], bb = this.h[1], c = this.h[2], d = this.h[3];
    for (let i = 0; i < 64; i++) {
      let f, g;
      if (i < 16) { f = (bb & c) | (~bb & d); g = i; }
      else if (i < 32) { f = (d & bb) | (~d & c); g = (5 * i + 1) & 15; }
      else if (i < 48) { f = bb ^ c ^ d; g = (3 * i + 5) & 15; }
      else { f = c ^ (bb | ~d); g = (7 * i) & 15; }
      const t = d;
      d = c;
      c = bb;
      const x = (a + f + K[i] + w[g]) | 0;
      bb = (bb + ((x << S[i]) | (x >>> (32 - S[i])))) | 0;
      a = t;
    }
    this.h[0] = (this.h[0] + a) | 0;
    this.h[1] = (this.h[1] + bb) | 0;
    this.h[2] = (this.h[2] + c) | 0;
    this.h[3] = (this.h[3] + d) | 0;
  }

  /** hex finishes and returns the digest as lower-case hex. */
  hex() {
    const bits = this.total * 8;
    const pad = new Uint8Array(((this.bufLen < 56 ? 56 : 120) - this.bufLen) + 8);
    pad[0] = 0x80;
    const lo = bits >>> 0;
    const hi = Math.floor(bits / 2 ** 32) >>> 0;
    const n = pad.length;
    pad[n - 8] = lo & 255; pad[n - 7] = (lo >>> 8) & 255; pad[n - 6] = (lo >>> 16) & 255; pad[n - 5] = lo >>> 24;
    pad[n - 4] = hi & 255; pad[n - 3] = (hi >>> 8) & 255; pad[n - 2] = (hi >>> 16) & 255; pad[n - 1] = hi >>> 24;
    const total = this.total;
    this.update(pad);
    this.total = total;
    let out = "";
    for (let i = 0; i < 4; i++) for (let j = 0; j < 4; j++) out += ((this.h[i] >>> (8 * j)) & 255).toString(16).padStart(2, "0");
    return out;
  }
}

// --- the pipeline -----------------------------------------------------------

export const CODEC_IN_FLIGHT_BYTES = 48 << 20;
export const INDEX_BATCH = 64;

/**
 * Semaphore is a byte semaphore (RP70-1): acquire(n) waits until n bytes fit
 * under the bound, FIFO — a small waiter never overtakes a large one; a
 * waiter larger than the bound proceeds alone when nothing is held (a chunk
 * is never refused: no deadlock). It records its peak in bytes (the tests
 * and the codec log line read it).
 */
export class Semaphore {
  constructor(bound) {
    this.bound = bound;
    this.used = 0;
    this.waiters = [];
    this.peak = 0;
  }
  get held() {
    return this.used;
  }
  /** wouldAdmit says whether acquire(n) would not wait. */
  wouldAdmit(n) {
    return this.waiters.length === 0 && this.fits(n);
  }
  fits(n) {
    return this.used === 0 || this.used + n <= this.bound;
  }
  take(n) {
    this.used += n;
    this.peak = Math.max(this.peak, this.used);
  }
  acquire(n) {
    if (this.wouldAdmit(n)) {
      this.take(n);
      return Promise.resolve();
    }
    return new Promise((res) => this.waiters.push({ n, res }));
  }
  release(n) {
    this.used -= n;
    if (this.used < 0) throw new Error(`edge codec: released ${-this.used} bytes more than were held`);
    while (this.waiters.length > 0 && this.fits(this.waiters[0].n)) {
      const w = this.waiters.shift();
      this.take(w.n);
      w.res();
    }
  }
}

function hex(buf) {
  const b = new Uint8Array(buf);
  let s = "";
  for (let i = 0; i < b.length; i++) s += b[i].toString(16).padStart(2, "0");
  return s;
}

function base64Bytes(s) {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

/** Pending is the bytes read and not yet cut into chunks. */
class Pending {
  constructor() {
    this.pieces = [];
    this.len = 0;
  }
  push(p) {
    this.pieces.push(p);
    this.len += p.length;
  }
  /** cut takes the next n bytes as a chunk {length, segments}: its 1 MiB
   * segments, each its own buffer (so each is freed when it is sealed). */
  cut(n) {
    if (n > this.len) throw new Error(`edge codec: the chunker cut ${n} bytes, ${this.len} are held`);
    const segments = [];
    for (let left = n; left > 0; ) {
      const seg = new Uint8Array(Math.min(SEGMENT_SIZE, left));
      let w = 0;
      while (w < seg.length) {
        const p = this.pieces[0];
        const take = Math.min(p.length, seg.length - w);
        seg.set(p.subarray(0, take), w);
        w += take;
        if (take === p.length) this.pieces.shift();
        else this.pieces[0] = p.subarray(take);
      }
      segments.push(seg);
      left -= seg.length;
    }
    this.len -= n;
    return { length: n, segments };
  }
}

/** sha256Hex hashes a chunk's segments: crypto.DigestStream where the
 * runtime has it (workerd — no contiguous copy), else one buffer. */
async function sha256Hex(segments) {
  if (typeof crypto.DigestStream === "function") {
    const ds = new crypto.DigestStream("SHA-256");
    const w = ds.getWriter();
    for (const seg of segments) await w.write(seg);
    await w.close();
    return hex(await ds.digest);
  }
  if (segments.length === 1) return hex(await crypto.subtle.digest("SHA-256", segments[0]));
  let total = 0;
  for (const seg of segments) total += seg.length;
  const all = new Uint8Array(total);
  let o = 0;
  for (const seg of segments) {
    all.set(seg, o);
    o += seg.length;
  }
  return hex(await crypto.subtle.digest("SHA-256", all));
}

/**
 * chunkPutInit is a chunk PUT's fetch init over the streamed seal: in
 * workerd the body goes through a FixedLengthStream of the sealed length
 * (the subrequest's Content-Length is the signed one, and a body of any
 * other length fails the PUT); a runtime without one (node: the tests)
 * sends the signed content-length header with the stream.
 */
export function chunkPutInit(headers, sealed) {
  const h = { ...headers };
  if (typeof globalThis.FixedLengthStream === "function") {
    delete h["content-length"];
    return { method: "PUT", headers: h, body: sealed.stream.pipeThrough(new globalThis.FixedLengthStream(sealed.length)), duplex: "half" };
  }
  h["content-length"] = String(sealed.length);
  return { method: "PUT", headers: h, body: sealed.stream, duplex: "half" };
}

function val(r, what) {
  if (r && r.error !== undefined) throw new Error(`${what}: ${r.error}`);
  return r.value;
}

/**
 * chunkStream is the cutter: a TransformStream whose transform feeds each
 * piece of the body to the chunker (storedChunker.push) and enqueues every
 * chunk the chunker completed, cut from the bytes held (`onPiece` sees each
 * piece first — the body's MD5). free() releases the chunker instance.
 */
export function chunkStream(chunker, onPiece) {
  const pending = new Pending();
  let id = val(chunker.create(), "chunker.create");
  const free = () => {
    if (id) chunker.free(id);
    id = 0;
  };
  const stream = new TransformStream({
    transform(piece, ctl) {
      if (!piece || piece.length === 0) return;
      if (onPiece) onPiece(piece);
      pending.push(piece);
      for (const n of val(chunker.push(id, piece), "chunker.push")) ctl.enqueue(pending.cut(n));
    },
    flush(ctl) {
      for (const n of val(chunker.finish(id), "chunker.finish")) ctl.enqueue(pending.cut(n));
      if (pending.len !== 0) throw new Error(`edge codec: ${pending.len} bytes were never cut`);
      free();
    },
  });
  return { stream, free };
}

/**
 * runCodec runs the pipeline over the request's body and returns the
 * commit's answer {status, headers, body} — or throws, having committed
 * edge_error (the caller answers 503). deps: {chunker, e2ee, fetch,
 * controlPlane(path, body) → Response, sem, log, tamper}.
 */
export async function runCodec(request, admit, env, ctx, deps) {
  const { chunker, e2ee } = deps;
  const doFetch = deps.fetch || fetch;
  const cp = deps.controlPlane;
  const log = deps.log || (() => {});
  const sem = deps.sem || new Semaphore(CODEC_IN_FLIGHT_BYTES);
  const tamper = deps.tamper || "";
  const started = Date.now();
  const declared = Number(request.headers.get("content-length"));
  let pub = base64Bytes(admit.codec.kem_public_key);
  if (tamper === "key") pub = val(e2ee.publicKey(crypto.getRandomValues(new Uint8Array(32))), "publicKey"); // TEST ONLY (seal under another key)
  const version = admit.codec.version;

  const md5 = new MD5();
  const entries = []; // body order
  const seen = new Set(); // hashes already asked of the index by this admit
  const stats = { chunks: 0, misses: 0, hits: 0, plaintext: 0, sealed: 0, index_ms: [] };
  let batch = [];
  const work = [];
  let failure = null;
  const fail = (e) => {
    if (!failure) failure = e;
  };

  const storeBatch = async (items) => {
    const t = Date.now();
    let idx;
    try {
      const r = await cp("/api/v1/edge/index", { admit_id: admit.admit_id, chunks: items.map((it) => ({ hash: it.hash, size: it.size })) });
      if (r.status !== 200) throw new Error(`index answered ${r.status}`);
      idx = await r.json();
    } catch (e) {
      for (const it of items) {
        it.chunk = null;
        sem.release(it.size);
      }
      throw e;
    }
    stats.index_ms.push(Date.now() - t);
    log({ edge: "codec_index", request_id: deps.requestId, chunks: items.length, misses: (idx.misses || []).length, ms: Date.now() - t });
    const misses = new Map((idx.misses || []).map((m) => [m.hash, m]));
    await Promise.all(items.map(async (it) => {
      // The chunk's plaintext bytes this item still holds of the semaphore:
      // a hit gives them back at once (RP70-6), a miss segment by segment
      // as its PUT reads the sealed stream (RP70-1), and whatever is left
      // when the item ends — a failure — in the finally.
      let held = it.size;
      const give = (n) => {
        const k = Math.min(n, held);
        held -= k;
        if (k > 0) sem.release(k);
      };
      try {
        const m = misses.get(it.hash);
        const e = entries[it.idx];
        if (!m) {
          it.chunk = null;
          give(held);
          e.status = "hit";
          stats.hits++;
          return;
        }
        const segments = it.chunk.segments;
        it.chunk = null; // the segments now live in the sealing stream only
        const sealed = await sealChunkStream(e2ee, pub, version, segments, { onSegment: give });
        if (sealed.length !== m.sealed_length) throw new Error(`edge codec: sealed ${sealed.length} bytes, the link signs ${m.sealed_length}`);
        const resp = await doFetch(m.url, chunkPutInit(m.send_headers || {}, sealed));
        const etag = resp.headers.get("etag") || "";
        await resp.body?.cancel();
        if (!resp.ok) throw new Error(`chunk PUT answered ${resp.status}`);
        e.status = "stored";
        e.sealed_size = sealed.length;
        e.backend_etag = tamper === "etag" ? `"${"ab".repeat(16)}"` : etag; // TEST ONLY
        stats.misses++;
        stats.sealed += sealed.length;
      } finally {
        give(held);
      }
    }));
  };

  const flush = () => {
    if (batch.length === 0) return;
    const b = batch;
    batch = [];
    work.push(storeBatch(b).catch((e) => {
      fail(e);
    }));
  };

  const emit = async (chunk) => {
    if (failure) throw failure;
    if (!sem.wouldAdmit(chunk.length)) flush(); // the batch goes out before the reader waits on the bound
    await sem.acquire(chunk.length);
    if (failure) {
      sem.release(chunk.length);
      throw failure;
    }
    const hash = await sha256Hex(chunk.segments);
    const idx = entries.length;
    entries.push({ hash, size: chunk.length });
    stats.chunks++;
    stats.plaintext += chunk.length;
    if (seen.has(hash)) {
      // Asked already: the commit settles it as a hit after the first
      // occurrence (stored or hit) is settled — the commit runs in order.
      entries[idx].status = "hit";
      chunk.segments = null;
      sem.release(chunk.length);
      return;
    }
    seen.add(hash);
    batch.push({ idx, hash, size: chunk.length, chunk });
    if (batch.length >= INDEX_BATCH) flush();
  };

  const cutter = chunkStream(chunker, (piece) => md5.update(piece));
  try {
    // The awaited write is the back-pressure: the cutter is read no faster
    // than emit admits chunks under the bound.
    // An empty body (an empty last part, D-EDGE-29) has no stream: the
    // chunker's finish cuts nothing and the commit names no chunk.
    const source = request.body || new ReadableStream({ start: (c) => c.close() });
    await source.pipeThrough(cutter.stream).pipeTo(new WritableStream({ write: (chunk) => emit(chunk) }));
    flush();
    await Promise.all(work);
    if (failure) throw failure;
    if (Number.isFinite(declared) && stats.plaintext !== declared) throw new Error(`edge codec: the body is ${stats.plaintext} bytes, content-length ${declared}`);
  } catch (e) {
    await Promise.allSettled(work);
    await commitEdgeError(cp, admit, log, deps.requestId);
    throw e;
  } finally {
    cutter.free();
  }

  const body = { admit_id: admit.admit_id, outcome: "ok", chunks: entries, body_md5: md5.hex() };
  const sorted = [...stats.index_ms].sort((a, b) => a - b);
  const pct = (p) => (sorted.length ? sorted[Math.min(sorted.length - 1, Math.floor((p / 100) * sorted.length))] : 0);
  log({ edge: "codec", request_id: deps.requestId, chunks: stats.chunks, misses: stats.misses, hits: stats.hits, bytes: stats.plaintext, sealed: stats.sealed,
    index_calls: sorted.length, index_p50_ms: pct(50), index_p95_ms: pct(95), peak_in_flight_bytes: sem.peak, ms: Date.now() - started });
  return commitOK(cp, body, log, deps.requestId, requestPath(request));
}

function requestPath(request) {
  try {
    return new URL(request.url).pathname;
  } catch {
    return "";
  }
}

async function commitEdgeError(cp, admit, log, requestId) {
  try {
    const r = await cp("/api/v1/edge/commit", { admit_id: admit.admit_id, outcome: "edge_error" });
    await r.body?.cancel();
  } catch (e) {
    log({ edge: "commit_failed", op: "codec", error: String(e && e.message ? e.message : e), request_id: requestId });
  }
}

/**
 * commitOK commits; a lost answer is retried once (the commit is idempotent
 * on the admit id). A 200 is the client's answer, relayed whatever status it
 * carries (a commit the control plane refused with an S3 400 included). A
 * 4xx of the control plane's OWN (its JSON error: the commit itself was
 * malformed) is the client's 400 InvalidRequest naming that error — the same
 * upload would fail the same way, so never a 503 to retry (RP70-7). null =
 * no answer (the caller's 503).
 */
async function commitOK(cp, body, log, requestId, path) {
  for (let attempt = 0; attempt < 2; attempt++) {
    if (attempt > 0) await new Promise((res) => setTimeout(res, 2000));
    try {
      const r = await cp("/api/v1/edge/commit", body);
      if (r.status === 200) return await r.json();
      if (r.status === 404 || r.status === 410) {
        await r.body?.cancel();
        return null;
      }
      if (r.status >= 400 && r.status < 500) {
        const j = await r.json().catch(() => ({}));
        const why = (j && j.error) || `HTTP ${r.status}`;
        log({ edge: "commit_refused", op: "codec", status: r.status, error: why, request_id: requestId });
        return {
          status: 400,
          headers: { "content-type": "application/xml", "x-amz-request-id": requestId || "" },
          body: s3ErrorXML("InvalidRequest", `Your request was invalid. The edge could not commit this upload (${why}); retrying it unchanged will fail the same way.`, path, requestId || ""),
        };
      }
      await r.body?.cancel();
    } catch (e) {
      log({ edge: "commit_failed", op: "codec", attempt, error: String(e && e.message ? e.message : e), request_id: requestId });
    }
  }
  return null;
}
