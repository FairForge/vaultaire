// stored-s3-edge — the mover (WP-EDGE-1 Part A + Part B, plan 40.10 slice 1).
//
// A Cloudflare Worker on s3.stored.cloud. Every S3 request arrives here;
// a PutObject or UploadPart is described to the control plane (admit),
// and when admitted its body is handed STRAIGHT to the backend on the
// presigned link the control plane minted — never through SLC, and never
// through JavaScript: the subrequest's body is the request's own body
// (bench-results/EDGE-RELAY-2026-10-26.md: any JS in the byte path halves
// a stream to ~25 MB/s; pass-through runs at 42–54 MB/s per stream and
// ~150 MB/s at eight). A GetObject (Part B) is admitted the same way and
// its bytes come STRAIGHT from the backend on a presigned ranged GET: the
// backend's body is the client's response body, under the ORIGIN's
// headers the control plane answered. Everything else, and anything the
// control plane declines, passes through to the origin untouched
// (`passThrough(request)` — `fetch(request, {cache: "no-store"})`: the
// route's origin is SLC, the Host stays, the SigV4 signature still verifies
// there).
//
// What this file is NOT: a privacy feature (it moves Standard plaintext and
// Private ciphertext alike and holds no key — contract lines 1, 4, 8), a
// verifier (the control plane verifies every signature — Architecture
// Decision 8), a hasher (integrity is the backend's own plus the client's
// Content-MD5 signed into the link), a buffer, a cache (a GET's subrequest
// is `cache: "no-store"` — customer bytes never enter the edge cache).
//
// Deployed as the file itself (`no_bundle = true`): its sha256 is a
// client_build row of the transparency log (contract line 7) and the
// deployed script downloads back byte-identical (Step 0.6).
//
// The pure parts are exported for `node --test` (test/*.test.js).
//
// 0.3.0 (WP-EDGE-2 Part B): an admit that answers `codec` (a codec tenant's
// write, the edge's rule — no url) is the codec's: the body is chunked,
// hashed, indexed, sealed and stored by src/codec.js and committed as a
// chunk list. That is the ONE place JavaScript touches the bytes, on purpose
// (the host must read them to chunk them). The modules it needs —
// src/codec.js, src/seal.js, src/wasm/{chunker.wasm, e2ee.wasm,
// wasm_exec.js} — are each published as the file and each is a client_build
// row; the wasm is loaded at the first codec request (dynamic import), so
// every other request costs nothing new.

import { ISOLATE_BYTES, isolateBudget, putTwo, runCodec, s3ErrorXML, setIsolateBudget } from "./codec.js";

const ORIGIN_TIMEOUT_MS = 10_000; // an admit that does not answer → the origin takes the request
const COMMIT_RETRY_MS = 2_000; // one more try of a lost commit answer (idempotent on the admit id)
const SLOWDOWN_RETRY_AFTER = "30";

const HOP_BY_HOP = new Set([
  "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade",
]);

// --- classification -------------------------------------------------------

/** STREAMING_TRAILER is aws-cli ≥ 2.23's default payload: an aws-chunked
 * body with unsigned chunks and a checksum trailer (0.4.0). */
const STREAMING_TRAILER = "STREAMING-UNSIGNED-PAYLOAD-TRAILER";

/**
 * classify says what the request is to the edge: {kind: "put"|"part"|"get"|
 * "pass", reason}. Only a signed (SigV4 header) PUT of an object key with
 * no query (PutObject) or exactly partNumber+uploadId (UploadPart), with a
 * plain body of a known Content-Length or aws-cli's default aws-chunked
 * body (x-amz-content-sha256 STREAMING-UNSIGNED-PAYLOAD-TRAILER with its
 * x-amz-decoded-content-length — 0.4.0: the mover forwards it untouched
 * under the header-signed request the admit mints, the codec decodes it),
 * or a signed GET of an object key with no query (GetObject — Part B; a
 * HEAD never, Decision 2), is the edge's; a copy, a sub-resource, an
 * unsigned or presigned request, a bucket request, signed chunks
 * (STREAMING-AWS4-HMAC-SHA256-PAYLOAD: every chunk signature is the client's
 * key's) and every other method are the origin's. aws-sdk-go-v2 (rclone's
 * library) names the operation in the query (`x-id=PutObject` /
 * `x-id=GetObject`): not a sub-resource.
 */
export function classify(request) {
  if (request.method !== "PUT" && request.method !== "GET") return { kind: "pass", reason: "method" };
  const url = new URL(request.url);
  const path = url.pathname;
  const slash = path.indexOf("/", 1);
  if (path.length < 4 || slash < 0 || slash === path.length - 1) return { kind: "pass", reason: "no_key" };
  const auth = request.headers.get("authorization") || "";
  if (!auth.startsWith("AWS4-HMAC-SHA256 ")) return { kind: "pass", reason: "unsigned" };
  const keys = [...url.searchParams.keys()].filter((k) => k !== "x-id");
  if (request.method === "GET") {
    if (keys.length !== 0) return { kind: "pass", reason: "query" };
    return { kind: "get" };
  }
  if (request.headers.get("x-amz-copy-source")) return { kind: "pass", reason: "copy" };
  const payload = request.headers.get("x-amz-content-sha256") || "";
  const streaming = payload.startsWith("STREAMING-");
  if (streaming && payload !== STREAMING_TRAILER) return { kind: "pass", reason: "aws_chunked" };
  if (!request.headers.get(streaming ? "x-amz-decoded-content-length" : "content-length")) return { kind: "pass", reason: "no_length" };
  if (keys.length === 0) return { kind: "put" };
  const set = new Set(keys);
  if (set.size === 2 && set.has("partNumber") && set.has("uploadId")) return { kind: "part" };
  return { kind: "pass", reason: "query" };
}

// --- the description -------------------------------------------------------

/**
 * describe builds the admit body: every client header lower-cased minus the
 * hop-by-hop ones (the client's Authorization INSIDE this JSON, never on
 * the call itself — the S3 credential gate never sees it), the raw path and
 * query, the client's IP from CF-Connecting-IP, a request id.
 */
export function describe(request, op, requestId) {
  const url = new URL(request.url);
  const headers = {};
  for (const [name, value] of request.headers) {
    const n = name.toLowerCase();
    if (HOP_BY_HOP.has(n)) continue;
    headers[n] = value;
  }
  headers.host = url.host;
  return {
    op,
    method: request.method,
    host: url.host,
    path: url.pathname,
    query: url.search.startsWith("?") ? url.search.slice(1) : url.search,
    headers,
    client_ip: request.headers.get("cf-connecting-ip") || "",
    request_id: requestId,
    // What this Worker reads (0.4.0): the control plane never answers a
    // shape an older Worker would misread (a windowed GET).
    features: ["window"],
  };
}

// --- relaying an S3 answer ---------------------------------------------------

/** relayResponse turns {status, headers, body} into the client's Response. */
export function relayResponse(relay) {
  const headers = new Headers();
  for (const [k, v] of Object.entries(relay.headers || {})) headers.set(k, v);
  const body = relay.body && relay.body.length > 0 ? relay.body : null;
  return new Response(body, { status: relay.status, headers });
}

// s3ErrorXML (src/codec.js) renders the S3 error document the origin's
// writer would; re-exported here for the tests and the relay.
export { s3ErrorXML };

/** backendErrorCode reads <Code> off an S3 error body ("" when none). */
export function backendErrorCode(xml) {
  const m = /<Code>([A-Za-z0-9]+)<\/Code>/.exec(xml || "");
  return m ? m[1] : "";
}

// --- a read (Part B) ----------------------------------------------------------

/**
 * readSucceeded says whether the backend's answer is the one the control
 * plane expected: expect_status (206 — every link names the byte span the
 * backend serves, the whole object's for a whole read) AND, when the
 * backend answered a Content-Length, exactly the admitted span
 * (admit.content_length — RP66-2: the one integrity property this path can
 * check; a truncated blob must fall back to the origin, whose integrity
 * reader decides, never reach the client as a short body). Anything else —
 * Lyve's HEAD 200 / GET 500 of 2026-10-21, a 412 under a replaced blob, a
 * 403 from a dead key — is the origin's to serve (it has the failover
 * path: the breaker, the fan-out, the parity rebuild).
 */
export function readSucceeded(backendResponse, admit) {
  if (!backendResponse || backendResponse.status !== admit.expect_status) return false;
  const cl = backendResponse.headers.get("content-length");
  if (cl !== null && admit.content_length > 0 && Number(cl) !== admit.content_length) return false;
  return true;
}

/**
 * readResponse is the client's response of an admitted read: the backend's
 * body as it is (no JavaScript touches it) under the ORIGIN's headers the
 * control plane answered — never the backend's — with the CLIENT's status
 * (admit.status: 200 for a whole read the backend served as a 206 of its
 * whole span, 206 for the client's own Range).
 */
export function readResponse(backendResponse, admit) {
  const headers = new Headers();
  for (const [k, v] of Object.entries(admit.response_headers || {})) headers.set(k, v);
  return new Response(backendResponse.body, { status: admit.status || admit.expect_status, headers });
}

/**
 * readFetchInit is the backend subrequest of a read: exactly the signed
 * headers, and `cache: "no-store"` — a Worker subrequest to a third-party
 * URL follows the default cache rules and presigned paths end in cacheable
 * extensions; customer bytes never enter the edge cache.
 */
export function readFetchInit(admit) {
  return { method: "GET", headers: admit.send_headers || {}, cache: "no-store" };
}

/** mirrorReadInit is the read subrequest of the mirror leg's link (0.5.0,
 * WP-EDGE-4): its own signed headers (no If-Match — the copy's ETag may be
 * another backend's), never cached. */
export function mirrorReadInit(admit) {
  return { method: "GET", headers: admit.mirror_send_headers || admit.send_headers || {}, cache: "no-store" };
}

/**
 * readCommit is the commit body of a read, decided AT THE BACKEND'S
 * HEADERS: the Worker cannot see the end of a stream it does not touch, so
 * `served` means the backend answered expect_status and its body is the
 * client's from here; the metered bytes are the backend's Content-Length.
 */
export function readCommit(admit, backendResponse, error, startedAt) {
  const r = { admit_id: admit.admit_id, outcome: "error", backend_status: 0, backend_code: "", backend_content_length: 0, duration_ms: Date.now() - startedAt };
  if (backendResponse) {
    r.backend_status = backendResponse.status;
    r.backend_content_length = Number(backendResponse.headers.get("content-length") || 0) || 0;
    r.outcome = readSucceeded(backendResponse, admit) ? "served" : "fallback";
  } else if (error) {
    r.backend_code = String(error && error.message ? error.message : error).slice(0, 120);
  }
  return r;
}

// --- the codec (0.3.0) --------------------------------------------------------

/**
 * codecTamper reads CODEC_TAMPER, the LAB switch of the tampered-host proofs
 * (WP-EDGE-2 Part B (g): "etag" reports a wrong backend ETag, "key" seals
 * under another public key). It is refused outright when ENVIRONMENT is
 * production — the deployed Worker can never run a tampered build.
 */
export function codecTamper(env) {
  const t = (env && env.CODEC_TAMPER) || "";
  if (!t) return "";
  if (env.ENVIRONMENT === "production") throw new Error("CODEC_TAMPER is set on the production Worker: refused");
  if (t !== "etag" && t !== "key") throw new Error(`CODEC_TAMPER=${t}: only etag or key`);
  return t;
}

let codecModules = null;

/** bootGo runs one Go WebAssembly module and returns the API it registers. */
async function bootGo(module, name) {
  const go = new globalThis.Go();
  const instance = await WebAssembly.instantiate(module, go.importObject);
  go.run(instance); // never resolves: the module serves
  for (let i = 0; i < 100 && !globalThis[name]; i++) await new Promise((r) => setTimeout(r, 1));
  if (!globalThis[name]) throw new Error(`${name} did not register`);
  return globalThis[name];
}

/** loadCodec loads the codec's WebAssembly once per isolate. */
function loadCodec() {
  if (!codecModules) {
    codecModules = (async () => {
      await import("./wasm/wasm_exec.js");
      const [c, e] = await Promise.all([import("./wasm/chunker.wasm"), import("./wasm/e2ee.wasm")]);
      return { chunker: await bootGo(c.default, "storedChunker"), e2ee: await bootGo(e.default, "storedE2EE") };
    })();
    codecModules.catch(() => {
      codecModules = null;
    });
  }
  return codecModules;
}

/**
 * serveCodec runs the codec over an admitted write. Before the body is read
 * (the modules would not load), the admit is committed edge_error and the
 * request passes to the origin; once it is read, any failure is a 503
 * SlowDown — the body is gone, the request can never be passed on.
 */
async function serveCodec(request, env, ctx, admit, requestId, url, tamper) {
  const log = (o) => console.log(JSON.stringify(o));
  const cp = (path, body) => controlPlane(env, path, body, ORIGIN_TIMEOUT_MS);
  let mods;
  try {
    mods = await loadCodec();
  } catch (e) {
    log({ edge: "codec_unavailable", error: String(e && e.message ? e.message : e), request_id: requestId });
    try {
      const r = await cp("/api/v1/edge/commit", { admit_id: admit.admit_id, outcome: "edge_error" });
      await r.body?.cancel();
    } catch {
      // the reaper settles the admit at its expiry
    }
    return passThrough(request);
  }
  let commit;
  try {
    commit = await runCodec(request, admit, env, ctx, { ...mods, controlPlane: cp, log, requestId, tamper });
  } catch (e) {
    log({ edge: "codec_failed", error: String(e && e.message ? e.message : e), request_id: requestId });
    return slowDown(url.pathname, requestId);
  }
  if (!commit) return slowDown(url.pathname, requestId);
  return relayResponse(commit);
}

// --- the Worker ------------------------------------------------------------

/**
 * passThrough hands the client's request to the origin untouched, NEVER
 * through Cloudflare's cache layer (RP70-5): the first pass-through after a
 * content deploy reached the origin without the client's signed `range` and
 * `if-none-match` — the cache layer answers a conditional or ranged GET
 * itself and strips them from its origin fetch — and the origin answered
 * 403 SignatureDoesNotMatch (RB-9, captured at the 0.3.0 deploy). Every
 * request the Worker passes on goes through here.
 */
export function passThrough(request) {
  return fetch(request, { cache: "no-store" });
}

function slowDown(path, requestId) {
  return new Response(s3ErrorXML("SlowDown", "Please reduce your request rate: the edge could not report your upload to the control plane. Retry.", path, requestId), {
    status: 503,
    headers: { "content-type": "application/xml", "retry-after": SLOWDOWN_RETRY_AFTER, "x-amz-request-id": requestId },
  });
}

async function controlPlane(env, path, body, timeoutMs) {
  return fetch(`${env.VAULTAIRE_ORIGIN}${path}`, {
    method: "POST",
    headers: { "content-type": "application/json", "x-vaultaire-edge-token": env.VAULTAIRE_EDGE_TOKEN },
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(timeoutMs),
  });
}

/**
 * moveInit is the backend subrequest of an admitted write: the client's
 * body as it is, under the presigned link's signed headers — or, for an
 * aws-chunked body (0.4.0), under the HEADER-signed request the admit
 * minted (its headers, the host being the URL's), the framing untouched and
 * sent chunked when the client sent no Content-Length.
 */
export function moveInit(request, admit) {
  if (!admit.headers) return { method: admit.method || "PUT", headers: admit.send_headers, body: request.body };
  const headers = {};
  for (const [k, v] of Object.entries(admit.headers)) if (k !== "host") headers[k] = v;
  if (!request.headers.get("content-length")) headers["transfer-encoding"] = "chunked";
  return { method: admit.method || "PUT", headers, body: request.body, duplex: "half" };
}

/**
 * moveBody hands the client's body to the backend as it is — no stream
 * touches it in JavaScript — and reports what the backend answered.
 */
async function moveBody(request, admit) {
  const started = Date.now();
  const result = { outcome: "edge_error", backend_status: 0, backend_code: "", backend_etag: "" };
  let mirror = null; // a mirror that runs on after the primary's answer (RP78-2)
  try {
    let resp;
    const mirrored = Array.isArray(admit.urls) && admit.urls.length === 2 && !admit.headers;
    if (mirrored) {
      // 0.5.0 (WP-EDGE-4): the body teed to the placement's link and the
      // mirror leg's (the same signed headers). 0.6.0 (RP78-2): the commit
      // is the primary's answer — a mirror still running is settled after
      // the client's response (settleMirror, ctx.waitUntil); one the
      // isolate's budget could not take is mirror_unbudgeted (RP78-1). Its
      // failure only leaves the copy to the blob_copies job.
      const n = Number(admit.send_headers?.["content-length"] || request.headers.get("content-length") || 0);
      const two = await putTwo(fetch, admit.urls, moveInit(request, admit), n);
      resp = two.resp;
      result.backend_etags = [resp.headers.get("etag") || ""];
      if (two.mirrorUnbudgeted) result.mirror_unbudgeted = true;
      else if (resp.ok) {
        result.mirror_settle = true;
        mirror = two;
      } else two.cut();
    } else {
      resp = await fetch(admit.url, moveInit(request, admit));
    }
    result.backend_status = resp.status;
    result.backend_etag = resp.headers.get("etag") || "";
    if (resp.ok) {
      result.outcome = "ok";
      await resp.body?.cancel();
    } else {
      result.outcome = "backend_error";
      result.backend_code = backendErrorCode(await resp.text().catch(() => ""));
    }
  } catch (e) {
    // The subrequest failed before an answer: the client's body broke off,
    // or the backend's connection did. The control plane answers 503 and
    // queues the address for the collector; the client retries.
    result.outcome = "edge_error";
    result.backend_code = String(e && e.message ? e.message : e).slice(0, 120);
  }
  result.duration_ms = Date.now() - started;
  return { result, mirror };
}

/**
 * settleMirror reports a mirror that outlived the primary's answer once it
 * has ended (RP78-2): POST /api/v1/edge/mirror with its ETag, or cut. Under
 * ctx.waitUntil — the client has its answer; a settle that is lost leaves
 * the copy pending, the blob_copies job's.
 */
export async function settleMirror(env, admitId, mirror, requestId, cp = controlPlane) {
  const { etag, cut } = await mirror.settle;
  try {
    const r = await cp(env, "/api/v1/edge/mirror", cut || !etag ? { admit_id: admitId, cut: !!cut } : { admit_id: admitId, etag }, ORIGIN_TIMEOUT_MS);
    console.log(JSON.stringify({ edge: "mirror_settled", request_id: requestId, status: r.status, cut: !!cut }));
    await r.body?.cancel();
  } catch (e) {
    console.log(JSON.stringify({ edge: "mirror_settle_failed", error: String(e && e.message ? e.message : e), request_id: requestId }));
  }
}

/**
 * commitRead reports a read's outcome; a lost answer is retried once. The
 * client is not waiting on it (ctx.waitUntil), so no answer is relayed.
 */
async function commitRead(env, body, requestId) {
  for (let attempt = 0; attempt < 2; attempt++) {
    if (attempt > 0) await new Promise((res) => setTimeout(res, COMMIT_RETRY_MS));
    try {
      const r = await controlPlane(env, "/api/v1/edge/commit", body, ORIGIN_TIMEOUT_MS);
      if (r.status === 200 || r.status === 404 || r.status === 410) {
        await r.body?.cancel();
        return;
      }
      await r.body?.cancel();
    } catch (e) {
      console.log(JSON.stringify({ edge: "commit_failed", op: "get", attempt, error: String(e && e.message ? e.message : e), request_id: requestId }));
    }
  }
}

/**
 * serveRead streams an admitted read: the backend's body straight to the
 * client under the origin's headers, the commit at the backend's headers
 * (served), or the origin's own serve when the backend did not answer as
 * expected (fallback / error): a GET has no request body, so
 * passThrough(request) is always possible.
 */
async function serveRead(request, env, ctx, admit, requestId) {
  const started = Date.now();
  let backend = null;
  let error = null;
  try {
    backend = await fetch(admit.url, readFetchInit(admit));
  } catch (e) {
    error = e;
  }
  let commit = readCommit(admit, backend, error, started);
  if (commit.outcome !== "served" && Array.isArray(admit.urls) && admit.urls.length === 2) {
    // 0.5.0 (WP-EDGE-4): the primary did not answer as admitted — the
    // mirror leg's copy before the origin.
    if (backend) await backend.body?.cancel();
    let second = null;
    let err2 = null;
    try {
      second = await fetch(admit.urls[1], mirrorReadInit(admit));
    } catch (e) {
      err2 = e;
    }
    const c2 = readCommit(admit, second, err2, started);
    if (c2.outcome === "served") {
      c2.served_url = 1;
      commit = c2;
      backend = second;
    } else if (second) {
      await second.body?.cancel();
      backend = null;
    }
  }
  ctx.waitUntil(commitRead(env, commit, requestId));
  console.log(JSON.stringify({ edge: "get", request_id: requestId, backend_status: commit.backend_status, outcome: commit.outcome, bytes: commit.backend_content_length, ms: commit.duration_ms }));
  if (commit.outcome !== "served") {
    if (backend) await backend.body?.cancel();
    return passThrough(request);
  }
  return readResponse(backend, admit);
}

// --- the windowed reader (0.4.0, D-EDGE-32) -----------------------------------

/** fixedLength is a FixedLengthStream of n bytes where the runtime has one
 * (workerd: the client's Content-Length is the stream's, a short body an
 * error), an identity TransformStream elsewhere (node: the tests). */
function fixedLength(n) {
  return typeof globalThis.FixedLengthStream === "function" ? new globalThis.FixedLengthStream(n) : new TransformStream();
}

/**
 * WINDOW_ISOLATE_BYTES bounds the range buffers ALL windowed reads of one
 * isolate hold at once: one full window (6 × 16 MiB). An isolate serves
 * concurrent requests, and 6 × 16 MiB per read would be 192 MiB for two —
 * past the isolate's 128 MB; concurrent reads share the budget (FIFO, a
 * range admitted alone when nothing is held — never a deadlock). Since
 * RP72-6 it is the isolate's ONE budget (src/codec.js isolateBudget): the
 * codec's chunk plaintext is taken from it too.
 */
export const WINDOW_ISOLATE_BYTES = ISOLATE_BYTES;

/** setWindowBudget replaces the isolate's budget (the tests); returns the restore. */
export function setWindowBudget(bytes) {
  return setIsolateBudget(bytes);
}

/** windowRange is range i of a windowed read: [first byte, last byte]. */
export function windowRange(admit, i) {
  const w = admit.window;
  const a = w.start + i * w.range_bytes;
  return [a, Math.min(w.start + admit.content_length, a + w.range_bytes) - 1];
}

/**
 * serveWindow streams an admitted read of ≥ 64 MiB as the windowed reader
 * (the SPEED bench § 4: 16 MiB ranges, ≤ 6 in flight, doubled both
 * backends): each range is its own GET on the admit's link (which signs no
 * range — the Worker names each inside the admitted span — and the
 * if-match where provable), read whole (`arrayBuffer()`: ~1 s of CPU per
 * GiB) and written IN ORDER to a FixedLengthStream of the span. The FIRST
 * range decides, as Part B's single GET: anything but a 206 of the expected
 * length is the origin's (fallback — no byte has left the edge). After it,
 * a range that fails — another status, another length, another ETag than
 * the first's — CUTS the client's body (an aborted stream, never a clean
 * short end). The commit is made when the stream ends: served with the
 * bytes sent, or error with the bytes sent before the cut (the control
 * plane meters what the client received). A client that disconnects
 * (workerd fires request.signal) stops the reader at once (RP72-5): no
 * range is launched after it, the ranges in flight are cancelled, the body
 * is aborted, and the commit is `served` with the bytes sent and
 * backend_code client_disconnected — a client leaving is never the
 * backend's failure (no breaker charge).
 */
async function serveWindow(request, env, ctx, admit, requestId) {
  const started = Date.now();
  const w = admit.window;
  const n = Math.ceil(admit.content_length / w.range_bytes);
  const lengthOf = (i) => {
    const [a, b] = windowRange(admit, i);
    return b - a + 1;
  };
  const ranges = new AbortController(); // cancels the ranges in flight when the read ends early
  const get = (i) => {
    const [a, b] = windowRange(admit, i);
    return fetch(admit.url, { method: "GET", headers: { ...(admit.send_headers || {}), range: `bytes=${a}-${b}` }, cache: "no-store", signal: ranges.signal });
  };
  const budget = isolateBudget();
  let first = null;
  let error = null;
  await budget.acquire(lengthOf(0));
  try {
    first = await get(0);
  } catch (e) {
    error = e;
  }
  const firstLength = first ? first.headers.get("content-length") : null;
  if (!first || first.status !== 206 || (firstLength !== null && Number(firstLength) !== lengthOf(0))) {
    budget.release(lengthOf(0));
    const commit = readCommit(admit, first, error, started);
    if (commit.outcome === "served") commit.outcome = "fallback"; // a 206 of another length
    ctx.waitUntil(commitRead(env, commit, requestId));
    console.log(JSON.stringify({ edge: "get", window: true, request_id: requestId, backend_status: commit.backend_status, outcome: commit.outcome, ms: commit.duration_ms }));
    if (first) await first.body?.cancel();
    return passThrough(request);
  }
  const etag = first.headers.get("etag");
  const { readable, writable } = fixedLength(admit.content_length);
  const writer = writable.getWriter();

  const pump = async () => {
    let sent = 0;
    let failure = null;
    const pending = new Map();
    const gone = Object.assign(new Error("the client disconnected"), { clientGone: true, code: "client_disconnected" });
    const onAbort = () => {
      if (!failure) failure = gone;
      ranges.abort(gone);
      writer.abort(gone).catch(() => {}); // a write waiting on a body nobody drains rejects
    };
    if (request.signal) {
      if (request.signal.aborted) onAbort();
      else request.signal.addEventListener("abort", onAbort, { once: true });
    }
    const held = new Set(); // ranges whose bytes this read holds of the isolate's budget
    const fetchRange = async (i) => {
      if (i > 0) {
        await budget.acquire(lengthOf(i));
        if (failure) {
          budget.release(lengthOf(i));
          throw failure;
        }
      }
      held.add(i);
      const r = i === 0 ? first : await get(i);
      const len = r.headers.get("content-length");
      if (r.status !== 206 || (len !== null && Number(len) !== lengthOf(i)) || (etag && r.headers.get("etag") !== etag)) {
        const code = backendErrorCode(await r.text().catch(() => ""));
        throw Object.assign(new Error(`range ${i}: ${r.status}`), { status: r.status, code: code || (r.status === 206 ? "range_mismatch" : "") });
      }
      return new Uint8Array(await r.arrayBuffer());
    };
    let next = 0;
    const launch = () => {
      while (next < n && pending.size < w.in_flight && !failure) {
        const p = fetchRange(next);
        p.catch(() => {}); // awaited in order below; a rejection ahead of its turn is not unhandled
        pending.set(next, p);
        next++;
      }
    };
    try {
      launch();
      for (let i = 0; i < n; i++) {
        if (failure) throw failure;
        const buf = await pending.get(i);
        pending.delete(i);
        launch();
        await writer.write(buf);
        sent += buf.length;
        held.delete(i);
        budget.release(lengthOf(i));
      }
      await writer.close();
    } catch (e) {
      failure = failure && failure.clientGone ? failure : e;
      // Give this read's ranges back FIRST: a pending range may be waiting
      // on exactly that budget (it then sees the failure, gives back what
      // it took and ends).
      for (const i of held) budget.release(lengthOf(i));
      held.clear();
      ranges.abort(failure);
      await writer.abort(failure).catch(() => {});
      await Promise.allSettled(pending.values());
    }
    if (request.signal) request.signal.removeEventListener("abort", onAbort);
    const clientGone = !!(failure && failure.clientGone);
    const commit = { admit_id: admit.admit_id, outcome: failure && !clientGone ? "error" : "served", backend_status: failure && !clientGone ? failure.status || 0 : 206,
      backend_code: failure ? String(failure.code || failure.message || failure).slice(0, 120) : "", backend_content_length: sent, bytes_sent: sent,
      duration_ms: Date.now() - started };
    console.log(JSON.stringify({ edge: "get", window: true, request_id: requestId, ranges: n, outcome: commit.outcome, backend_status: commit.backend_status, bytes: sent, ms: commit.duration_ms }));
    await commitRead(env, commit, requestId);
  };
  ctx.waitUntil(pump());
  return new Response(readable, { status: admit.status || admit.expect_status, headers: admit.response_headers || {} });
}

export default {
  async fetch(request, env, ctx) {
    const kind = classify(request);
    if (kind.kind === "pass") return passThrough(request);
    if (!env.VAULTAIRE_ORIGIN || !env.VAULTAIRE_EDGE_TOKEN) return passThrough(request);
    let tamper;
    try {
      tamper = codecTamper(env);
    } catch (e) {
      console.log(JSON.stringify({ edge: "refused", error: e.message }));
      return passThrough(request);
    }

    const requestId = crypto.randomUUID();
    const url = new URL(request.url);
    const description = describe(request, kind.kind, requestId);

    let admit;
    try {
      const r = await controlPlane(env, "/api/v1/edge/admit", description, ORIGIN_TIMEOUT_MS);
      if (r.status !== 200) {
        console.log(JSON.stringify({ edge: "admit_unavailable", status: r.status, request_id: requestId }));
        return passThrough(request);
      }
      admit = await r.json();
    } catch (e) {
      console.log(JSON.stringify({ edge: "admit_failed", error: String(e && e.message ? e.message : e), request_id: requestId }));
      return passThrough(request);
    }
    if (admit.refusal) return relayResponse(admit.refusal);
    if (!admit.edge) {
      console.log(JSON.stringify({ edge: "pass", op: kind.kind, reason: admit.reason || "", request_id: requestId }));
      return passThrough(request);
    }

    if (kind.kind === "get") return admit.window ? serveWindow(request, env, ctx, admit, requestId) : serveRead(request, env, ctx, admit, requestId);
    if (admit.codec) return serveCodec(request, env, ctx, admit, requestId, url, tamper);

    const { result: moved, mirror } = await moveBody(request, admit);
    const commitBody = { admit_id: admit.admit_id, ...moved };
    let commit = null;
    for (let attempt = 0; attempt < 2 && !commit; attempt++) {
      if (attempt > 0) await new Promise((res) => setTimeout(res, COMMIT_RETRY_MS));
      try {
        const r = await controlPlane(env, "/api/v1/edge/commit", commitBody, ORIGIN_TIMEOUT_MS);
        if (r.status === 200) commit = await r.json();
        else if (r.status === 404 || r.status === 410) {
          commit = { status: 503, headers: { "content-type": "application/xml", "retry-after": SLOWDOWN_RETRY_AFTER }, body: s3ErrorXML("SlowDown", "The edge admission of this upload expired before it was committed. Retry the upload.", url.pathname, requestId) };
        }
      } catch (e) {
        console.log(JSON.stringify({ edge: "commit_failed", attempt, error: String(e && e.message ? e.message : e), request_id: requestId }));
      }
    }
    console.log(JSON.stringify({ edge: kind.kind, request_id: requestId, backend_status: moved.backend_status, outcome: moved.outcome, ms: moved.duration_ms, committed: !!commit }));
    if (mirror) ctx.waitUntil(settleMirror(env, admit.admit_id, mirror, requestId));
    if (!commit) return slowDown(url.pathname, requestId);
    return relayResponse(commit);
  },
};
