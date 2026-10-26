// stored-s3-edge — the mover (WP-EDGE-1 Part A, plan 40.10 slice 1).
//
// A Cloudflare Worker on s3.stored.cloud. Every S3 request arrives here;
// a PutObject or UploadPart is described to the control plane (admit),
// and when admitted its body is handed STRAIGHT to the backend on the
// presigned link the control plane minted — never through SLC, and never
// through JavaScript: the subrequest's body is the request's own body
// (bench-results/EDGE-RELAY-2026-10-26.md: any JS in the byte path halves
// a stream to ~25 MB/s; pass-through runs at 42–54 MB/s per stream and
// ~150 MB/s at eight). Everything else, and anything the control plane
// declines, passes through to the origin untouched (`fetch(request)`: the
// route's origin is SLC, the Host stays, the SigV4 signature still
// verifies there).
//
// What this file is NOT: a privacy feature (it moves Standard plaintext and
// Private ciphertext alike and holds no key — contract lines 1, 4, 8), a
// verifier (the control plane verifies every signature — Architecture
// Decision 8), a hasher (integrity is the backend's own plus the client's
// Content-MD5 signed into the link), a buffer.
//
// Deployed as the file itself (`no_bundle = true`): its sha256 is a
// client_build row of the transparency log (contract line 7) and the
// deployed script downloads back byte-identical (Step 0.6).
//
// The pure parts are exported for `node --test` (test/*.test.js).

const ORIGIN_TIMEOUT_MS = 10_000; // an admit that does not answer → the origin takes the request
const COMMIT_RETRY_MS = 2_000; // one more try of a lost commit answer (idempotent on the admit id)
const SLOWDOWN_RETRY_AFTER = "30";

const HOP_BY_HOP = new Set([
  "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade",
]);

// --- classification -------------------------------------------------------

/**
 * classify says what the request is to the edge: {kind: "put"|"part"|"pass",
 * reason}. Only a signed (SigV4 header) PUT of an object key with no query
 * (PutObject) or exactly partNumber+uploadId (UploadPart), with a plain
 * body, is the edge's; a copy, a sub-resource, an unsigned or presigned
 * request, a bucket PUT, an aws-chunked body (x-amz-content-sha256
 * STREAMING-*: decoding it would put JS in the byte path — the origin takes
 * it; aws-cli v2 users set request_checksum_calculation = when_required)
 * and every other method are the origin's.
 */
export function classify(request) {
  if (request.method !== "PUT") return { kind: "pass", reason: "method" };
  const url = new URL(request.url);
  const path = url.pathname;
  const slash = path.indexOf("/", 1);
  if (path.length < 4 || slash < 0 || slash === path.length - 1) return { kind: "pass", reason: "no_key" };
  const auth = request.headers.get("authorization") || "";
  if (!auth.startsWith("AWS4-HMAC-SHA256 ")) return { kind: "pass", reason: "unsigned" };
  if (request.headers.get("x-amz-copy-source")) return { kind: "pass", reason: "copy" };
  if (!request.headers.get("content-length")) return { kind: "pass", reason: "no_length" };
  if ((request.headers.get("x-amz-content-sha256") || "").startsWith("STREAMING-")) return { kind: "pass", reason: "aws_chunked" };
  // aws-sdk-go-v2 (rclone's library) names the operation in the query
  // (`x-id=PutObject` / `x-id=UploadPart`): not a sub-resource.
  const keys = [...url.searchParams.keys()].filter((k) => k !== "x-id");
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

/** s3ErrorXML renders the S3 error document the origin's writer would. */
export function s3ErrorXML(code, message, path, requestId) {
  const esc = (s) => String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  return `<?xml version="1.0" encoding="UTF-8"?>\n<Error><Code>${esc(code)}</Code><Message>${esc(message)}</Message><Resource>${esc(path)}</Resource><RequestId>${esc(requestId)}</RequestId></Error>`;
}

/** backendErrorCode reads <Code> off an S3 error body ("" when none). */
export function backendErrorCode(xml) {
  const m = /<Code>([A-Za-z0-9]+)<\/Code>/.exec(xml || "");
  return m ? m[1] : "";
}

// --- the Worker ------------------------------------------------------------

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
 * moveBody hands the client's body to the backend as it is — no stream
 * touches it in JavaScript — and reports what the backend answered.
 */
async function moveBody(request, admit) {
  const started = Date.now();
  const result = { outcome: "edge_error", backend_status: 0, backend_code: "", backend_etag: "" };
  try {
    const resp = await fetch(admit.url, { method: admit.method || "PUT", headers: admit.send_headers, body: request.body });
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
  return result;
}

export default {
  async fetch(request, env) {
    const kind = classify(request);
    if (kind.kind === "pass") return fetch(request);
    if (!env.VAULTAIRE_ORIGIN || !env.VAULTAIRE_EDGE_TOKEN) return fetch(request);

    const requestId = crypto.randomUUID();
    const url = new URL(request.url);
    const description = describe(request, kind.kind, requestId);

    let admit;
    try {
      const r = await controlPlane(env, "/api/v1/edge/admit", description, ORIGIN_TIMEOUT_MS);
      if (r.status !== 200) {
        console.log(JSON.stringify({ edge: "admit_unavailable", status: r.status, request_id: requestId }));
        return fetch(request);
      }
      admit = await r.json();
    } catch (e) {
      console.log(JSON.stringify({ edge: "admit_failed", error: String(e && e.message ? e.message : e), request_id: requestId }));
      return fetch(request);
    }
    if (admit.refusal) return relayResponse(admit.refusal);
    if (!admit.edge) return fetch(request);

    const moved = await moveBody(request, admit);
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
    if (!commit) return slowDown(url.pathname, requestId);
    return relayResponse(commit);
  },
};
