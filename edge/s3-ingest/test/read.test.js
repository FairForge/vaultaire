// Part B: the read half's pure parts — the GET classify matrix, the
// client's response under the origin's headers, the fetch init of the
// backend subrequest (no edge cache), the commit at the backend's headers
// and the fallback decision.
import { test } from "node:test";
import assert from "node:assert/strict";
import { classify, readCommit, readFetchInit, readResponse, readSucceeded } from "../src/index.js";

const sig = "AWS4-HMAC-SHA256 Credential=VKX/20261026/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=00";

function get(url, headers = {}, method = "GET") {
  return new Request(url, { method, headers: { Authorization: sig, ...headers } });
}

test("classify: a signed GetObject is the edge's; HEAD, presigned, sub-resources and versions are the origin's", () => {
  assert.deepEqual(classify(get("https://s3.stored.cloud/b/k.bin")), { kind: "get" });
  assert.deepEqual(classify(get("https://s3.stored.cloud/b/dir/k.bin?x-id=GetObject")), { kind: "get" }, "the SDK's x-id is not a sub-resource");
  assert.deepEqual(classify(get("https://s3.stored.cloud/b/k.bin", { Range: "bytes=0-99" })), { kind: "get" }, "a Range is the control plane's to judge");
  assert.deepEqual(classify(get("https://s3.stored.cloud/b/k.bin", {}, "HEAD")), { kind: "pass", reason: "method" }, "HEAD never (cache-only)");
  assert.deepEqual(classify(get("https://s3.stored.cloud/b/k.bin?versionId=abc")), { kind: "pass", reason: "query" });
  assert.deepEqual(classify(get("https://s3.stored.cloud/b/k.bin?tagging")), { kind: "pass", reason: "query" });
  assert.deepEqual(classify(get("https://s3.stored.cloud/b/k.bin?response-content-disposition=attachment")), { kind: "pass", reason: "query" });
  assert.deepEqual(classify(get("https://s3.stored.cloud/b?list-type=2")), { kind: "pass", reason: "no_key" }, "a bucket listing");
  assert.deepEqual(classify(get("https://s3.stored.cloud/b/")), { kind: "pass", reason: "no_key" });
  assert.deepEqual(classify(get("https://s3.stored.cloud/health")), { kind: "pass", reason: "no_key" });
  const presigned = new Request("https://s3.stored.cloud/b/k?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=00", { method: "GET" });
  assert.deepEqual(classify(presigned), { kind: "pass", reason: "unsigned" }, "a presigned URL stays at the origin");
  assert.deepEqual(classify(new Request("https://s3.stored.cloud/b/k", { method: "GET" })), { kind: "pass", reason: "unsigned" });
  assert.deepEqual(classify(new Request("https://s3.stored.cloud/b/k", { method: "DELETE", headers: { Authorization: sig } })), { kind: "pass", reason: "method" });
  // The write classify is untouched.
  assert.equal(classify(new Request("https://s3.stored.cloud/b/k.bin", { method: "PUT", headers: { Authorization: sig, "Content-Length": "5" }, body: "hello" })).kind, "put");
});

test("readResponse: the backend's body under the ORIGIN's headers, never the backend's, with the expected status", async () => {
  const backend = new Response("0123456789", { status: 206, headers: { "content-type": "binary/octet-stream", etag: '"backend"', "x-amz-id-2": "vendor", "content-range": "bytes 0-9/10", "content-length": "10", "cf-cache-status": "HIT" } });
  const admit = { expect_status: 206, status: 206, response_headers: { "Content-Type": "text/plain", Etag: '"row"', "Content-Range": "bytes 0-9/4096", "Content-Length": "10", "X-Amz-Storage-Class": "STANDARD", "X-Amz-Meta-Origin": "edge-test", "Accept-Ranges": "bytes", "Last-Modified": "Sun, 26 Oct 2026 12:00:00 GMT" } };
  const resp = readResponse(backend, admit);
  assert.equal(resp.status, 206);
  assert.equal(resp.headers.get("content-type"), "text/plain");
  assert.equal(resp.headers.get("etag"), '"row"');
  assert.equal(resp.headers.get("content-range"), "bytes 0-9/4096");
  assert.equal(resp.headers.get("x-amz-storage-class"), "STANDARD");
  assert.equal(resp.headers.get("x-amz-meta-origin"), "edge-test");
  assert.equal(resp.headers.get("x-amz-id-2"), null, "nothing of the backend's");
  assert.equal(resp.headers.get("cf-cache-status"), null);
  assert.equal(await resp.text(), "0123456789", "the body as it is");
  // A whole read: the backend's 206 of the whole span is the client's 200.
  const whole = readResponse(new Response("abc", { status: 206, headers: { "content-range": "bytes 0-2/3" } }), { expect_status: 206, status: 200, response_headers: { "Content-Length": "3", Etag: '"x"' } });
  assert.equal(whole.status, 200);
  assert.equal(whole.headers.get("content-range"), null);
  assert.equal(whole.headers.get("content-length"), "3");
});

test("readFetchInit: exactly the signed headers, no edge cache", () => {
  const init = readFetchInit({ send_headers: { range: "bytes=100-299", "if-match": '"abc"' } });
  assert.equal(init.method, "GET");
  assert.deepEqual(init.headers, { range: "bytes=100-299", "if-match": '"abc"' });
  assert.equal(init.cache, "no-store");
  assert.deepEqual(readFetchInit({}).headers, {});
});

test("readSucceeded / readCommit: served at expect_status, fallback on anything else, error without an answer", () => {
  const admit = { admit_id: "a1", expect_status: 206, status: 200 };
  const ok = new Response("x", { status: 206, headers: { "content-length": "4096" } });
  assert.equal(readSucceeded(ok, admit), true);
  const c = readCommit(admit, ok, null, Date.now() - 5);
  assert.equal(c.admit_id, "a1");
  assert.equal(c.outcome, "served");
  assert.equal(c.backend_status, 206);
  assert.equal(c.backend_content_length, 4096, "the backend's Content-Length, read at its headers");
  assert.ok(c.duration_ms >= 5);

  // Lyve's HEAD 200 / GET 500 (2026-10-21): the origin's.
  const five = new Response("<Error><Code>InternalError</Code></Error>", { status: 500, headers: { "content-length": "42" } });
  assert.equal(readSucceeded(five, admit), false);
  const f = readCommit(admit, five, null, Date.now());
  assert.equal(f.outcome, "fallback");
  assert.equal(f.backend_status, 500);
  assert.equal(f.backend_content_length, 42, "whatever the backend sent; the control plane meters nothing on a fallback");

  // A 200 where a 206 was expected (a backend that ignored the Range): the origin's.
  const whole = new Response("x", { status: 200 });
  assert.equal(readSucceeded(whole, { expect_status: 206 }), false);
  assert.equal(readSucceeded(whole, admit), false, "even for a whole read: every link names a Range");
  // A 412 (the blob replaced under the link — impossible by address, the belt): the origin's.
  assert.equal(readSucceeded(new Response("", { status: 412 }), admit), false);
  // No answer at all.
  const e = readCommit(admit, null, new Error("connection reset by peer"), Date.now());
  assert.equal(e.outcome, "error");
  assert.equal(e.backend_status, 0);
  assert.equal(e.backend_code, "connection reset by peer");
  assert.equal(readSucceeded(null, admit), false);
  // A missing Content-Length is 0: the control plane meters the admitted size.
  const noLen = readCommit(admit, new Response(null, { status: 200 }), null, Date.now());
  assert.equal(noLen.backend_content_length, 0);
});

test("a backend Content-Length that is not the admitted span is a fallback (RP66-2, Worker 0.2.1)", () => {
  const admit = { admit_id: "a2", expect_status: 206, status: 200, content_length: 4096 };
  const exact = new Response("x", { status: 206, headers: { "content-length": "4096" } });
  assert.equal(readSucceeded(exact, admit), true);
  const short = new Response("x", { status: 206, headers: { "content-length": "100" } });
  assert.equal(readSucceeded(short, admit), false, "a truncated blob falls back: the origin's integrity path decides");
  const c = readCommit(admit, short, null, Date.now());
  assert.equal(c.outcome, "fallback");
  assert.equal(c.backend_content_length, 100);
  // No Content-Length at the headers = no check (metered as admitted by the
  // control plane, RP66-7); an admit without the field (an older control
  // plane) = no check either.
  assert.equal(readSucceeded(new Response(null, { status: 206 }), admit), true);
  assert.equal(readSucceeded(short, { expect_status: 206 }), true);
});

// --- 0.4.0: the windowed reader (D-EDGE-32) ---

import worker, { setWindowBudget, WINDOW_ISOLATE_BYTES } from "../src/index.js";
import { createHash } from "node:crypto";

const env = { VAULTAIRE_ORIGIN: "https://origin.test", VAULTAIRE_EDGE_TOKEN: "tok" };

/** windowFakes installs a fake fetch: the admit answers `admit`, the backend
 * serves ranges of `object` (failing range index `failAt` with a 500, after
 * `delayMs`), commits are recorded, a pass-through is recorded. */
function windowFakes({ object, admit, failAt = -1, delayMs = 5, firstStatus = 206 }) {
  const st = { inFlight: 0, peak: 0, ranges: [], commits: [], passes: 0 };
  const real = globalThis.fetch;
  globalThis.fetch = async (input, init) => {
    if (input instanceof Request) {
      st.passes++;
      return new Response("origin", { status: 200 });
    }
    const url = String(input);
    if (url.endsWith("/api/v1/edge/admit")) return new Response(JSON.stringify(admit), { status: 200 });
    if (url.endsWith("/api/v1/edge/commit")) {
      st.commits.push(JSON.parse(init.body));
      return new Response("{}", { status: 200 });
    }
    st.inFlight++;
    st.peak = Math.max(st.peak, st.inFlight);
    try {
      const m = /^bytes=(\d+)-(\d+)$/.exec(new Headers(init.headers).get("range") || "");
      assert.ok(m, "every range names its span");
      assert.equal(init.cache, "no-store");
      assert.equal(new Headers(init.headers).get("if-match"), admit.send_headers["if-match"], "the signed if-match on every range");
      const a = Number(m[1]);
      const b = Number(m[2]);
      const idx = st.ranges.length;
      st.ranges.push([a, b]);
      await new Promise((r) => setTimeout(r, delayMs));
      if (idx === 0 && firstStatus !== 206) return new Response("x", { status: firstStatus });
      if (a === failAt * admit.window.range_bytes + admit.window.start) return new Response("<Error><Code>InternalError</Code></Error>", { status: 500 });
      const body = object.subarray(a, b + 1);
      return new Response(body, { status: 206, headers: { "content-length": String(body.length), "content-range": `bytes ${a}-${b}/${object.length}`, etag: '"obj-3"' } });
    } finally {
      st.inFlight--;
    }
  };
  return { st, restore: () => { globalThis.fetch = real; } };
}

function windowAdmit(size, start = 0) {
  return {
    edge: true, admit_id: "w-1", url: "https://backend.test/stored/t-x/obj", method: "GET",
    send_headers: { "if-match": '"obj-3"' }, expect_status: 206, status: 200, content_length: size,
    response_headers: { "Content-Length": String(size), ETag: '"obj-3"' },
    window: { range_bytes: 1 << 20, in_flight: 6, start },
  };
}

function getReq() {
  return new Request("https://s3.stored.cloud/b/obj", { method: "GET", headers: { authorization: sig } });
}

async function runWindow(f) {
  const waits = [];
  const resp = await worker.fetch(getReq(), env, { waitUntil: (p) => waits.push(p) });
  return { resp, settle: () => Promise.all(waits) };
}

test("the windowed reader: ranges of range_bytes, at most in_flight at once, written in order — the client gets the exact bytes, the commit meters the sum", async () => {
  const object = new Uint8Array(20 * (1 << 20) + 12345).map((_, i) => (i * 13 + 5) & 255);
  const f = windowFakes({ object, admit: windowAdmit(object.length) });
  try {
    const { resp, settle } = await runWindow(f);
    assert.equal(resp.status, 200);
    assert.equal(resp.headers.get("etag"), '"obj-3"', "the origin's headers");
    const got = new Uint8Array(await resp.arrayBuffer());
    await settle();
    assert.equal(got.length, object.length);
    assert.equal(createHash("md5").update(got).digest("hex"), createHash("md5").update(object).digest("hex"));
    assert.equal(f.st.ranges.length, 21, "20 full ranges and the tail");
    assert.ok(f.st.peak <= 6, `peak ${f.st.peak} ranges in flight`);
    assert.ok(f.st.peak >= 4, `the window is used: peak ${f.st.peak}`);
    assert.equal(f.st.commits.length, 1);
    assert.equal(f.st.commits[0].outcome, "served");
    assert.equal(f.st.commits[0].bytes_sent, object.length);
    assert.equal(f.st.commits[0].backend_content_length, object.length);
    assert.equal(f.st.passes, 0);
  } finally {
    f.restore();
  }
});

test("the windowed reader from a client Range's start: every range inside the admitted span", async () => {
  const object = new Uint8Array(12 * (1 << 20)).map((_, i) => i & 255);
  const start = 3 * (1 << 20) + 7;
  const span = object.length - start;
  const admit = windowAdmit(span, start);
  admit.status = 206;
  const f = windowFakes({ object, admit });
  try {
    const { resp, settle } = await runWindow(f);
    assert.equal(resp.status, 206);
    const got = new Uint8Array(await resp.arrayBuffer());
    await settle();
    assert.deepEqual(Buffer.from(got), Buffer.from(object.subarray(start)));
    assert.ok(f.st.ranges.every(([a, b]) => a >= start && b < object.length));
  } finally {
    f.restore();
  }
});

test("a range that fails after bytes went out cuts the body (never a clean short end); the commit reports error with the bytes sent", async () => {
  const object = new Uint8Array(20 * (1 << 20)).map((_, i) => (i * 7) & 255);
  const f = windowFakes({ object, admit: windowAdmit(object.length), failAt: 9 });
  try {
    const { resp, settle } = await runWindow(f);
    assert.equal(resp.status, 200);
    await assert.rejects(resp.arrayBuffer(), "the client's body errors");
    await settle();
    assert.equal(f.st.commits.length, 1);
    const c = f.st.commits[0];
    assert.equal(c.outcome, "error");
    assert.equal(c.backend_status, 500);
    assert.equal(c.bytes_sent, 9 * (1 << 20), "ranges 0–8 reached the client");
    assert.equal(f.st.passes, 0, "the body had started: never the origin");
  } finally {
    f.restore();
  }
});

test("a first range the backend does not serve is the origin's (fallback), nothing sent from the edge", async () => {
  const object = new Uint8Array(8 * (1 << 20));
  const f = windowFakes({ object, admit: windowAdmit(object.length), firstStatus: 412 });
  try {
    const { resp, settle } = await runWindow(f);
    assert.equal(await resp.text(), "origin");
    await settle();
    assert.equal(f.st.passes, 1);
    assert.equal(f.st.commits[0].outcome, "fallback");
    assert.equal(f.st.commits[0].backend_status, 412);
    assert.ok(!f.st.commits[0].bytes_sent);
  } finally {
    f.restore();
  }
});

test("an isolate's windowed reads share ONE byte budget: two concurrent large GETs never hold more than the budget's ranges", async () => {
  // A Workers isolate serves concurrent requests: 6 × 16 MiB per read would
  // be 192 MiB for two — past the isolate's 128 MB. The budget is one full
  // window (6 ranges); here 6 × 1 MiB, the test's range size.
  assert.equal(WINDOW_ISOLATE_BYTES, 6 * (16 << 20));
  const restoreBudget = setWindowBudget(6 * (1 << 20));
  const object = new Uint8Array(24 * (1 << 20)).map((_, i) => (i * 3 + 1) & 255);
  const f = windowFakes({ object, admit: windowAdmit(object.length), delayMs: 15 });
  try {
    const [a, b] = await Promise.all([runWindow(f), runWindow(f)]);
    const [ga, gb] = await Promise.all([a.resp.arrayBuffer(), b.resp.arrayBuffer()]);
    await Promise.all([a.settle(), b.settle()]);
    assert.equal(createHash("md5").update(new Uint8Array(ga)).digest("hex"), createHash("md5").update(object).digest("hex"));
    assert.equal(createHash("md5").update(new Uint8Array(gb)).digest("hex"), createHash("md5").update(object).digest("hex"));
    assert.ok(f.st.peak <= 6, `both reads together held ${f.st.peak} ranges in flight`);
    assert.equal(f.st.commits.filter((c) => c.outcome === "served").length, 2);
  } finally {
    f.restore();
    restoreBudget();
  }
});

test("a cut under a tight budget never hangs: the read gives its ranges back before it waits for its pending ones", async () => {
  const restoreBudget = setWindowBudget(2 * (1 << 20));
  const object = new Uint8Array(12 * (1 << 20));
  const f = windowFakes({ object, admit: windowAdmit(object.length), failAt: 3, delayMs: 5 });
  try {
    const { resp, settle } = await runWindow(f);
    await assert.rejects(resp.arrayBuffer());
    const done = await Promise.race([settle().then(() => "settled"), new Promise((r) => setTimeout(() => r("hung"), 3000))]);
    assert.equal(done, "settled");
    assert.equal(f.st.commits[0].outcome, "error");
    assert.equal(f.st.commits[0].bytes_sent, 3 * (1 << 20));
    // The budget is whole again: a full read goes through.
    f.restore();
    const g = windowFakes({ object, admit: windowAdmit(object.length) });
    const ok = await runWindow(g);
    assert.equal((await ok.resp.arrayBuffer()).byteLength, object.length);
    await ok.settle();
    g.restore();
  } finally {
    f.restore();
    restoreBudget();
  }
});

// RP72-5: a client that disconnects mid-window (workerd fires request.signal)
// stops the reader — no range is launched after the signal, the ranges in
// flight are cancelled, the read ends (it never waits on a body nobody
// drains), and the commit meters what was sent as a SERVED read: a client
// that went away is never the backend's failure (no breaker charge).
test("a client that disconnects mid-window stops the reader: no range launched after the signal, the bytes sent metered as served", { timeout: 10_000 }, async () => {
  const object = new Uint8Array(40 * (1 << 20)).map((_, i) => (i * 3) & 255);
  const f = windowFakes({ object, admit: windowAdmit(object.length), delayMs: 20 });
  try {
    const ac = new AbortController();
    const req = new Request("https://s3.stored.cloud/b/obj", { method: "GET", headers: { authorization: sig }, signal: ac.signal });
    const waits = [];
    const resp = await worker.fetch(req, env, { waitUntil: (p) => waits.push(p) });
    const reader = resp.body.getReader();
    await reader.read(); // the client takes a little, then goes — and nobody drains the body any more
    const launchedAtAbort = f.st.ranges.length;
    ac.abort();
    await Promise.all(waits);
    assert.ok(f.st.ranges.length <= launchedAtAbort + 1, `no range launched after the signal (${launchedAtAbort} → ${f.st.ranges.length})`);
    assert.ok(f.st.ranges.length < 40, "the read stopped");
    assert.equal(f.st.commits.length, 1);
    const c = f.st.commits[0];
    assert.equal(c.outcome, "served", "a client leaving is not the backend's failure");
    assert.equal(c.backend_code, "client_disconnected");
    assert.ok(c.bytes_sent > 0 && c.bytes_sent < object.length, `what was sent is metered: ${c.bytes_sent}`);
  } finally {
    f.restore();
  }
});
