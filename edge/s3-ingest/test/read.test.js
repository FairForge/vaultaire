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
  const admit = { expect_status: 206, response_headers: { "Content-Type": "text/plain", Etag: '"row"', "Content-Range": "bytes 0-9/4096", "Content-Length": "10", "X-Amz-Storage-Class": "STANDARD", "X-Amz-Meta-Origin": "edge-test", "Accept-Ranges": "bytes", "Last-Modified": "Sun, 26 Oct 2026 12:00:00 GMT" } };
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
});

test("readFetchInit: exactly the signed headers, no edge cache", () => {
  const init = readFetchInit({ send_headers: { range: "bytes=100-299", "if-match": '"abc"' } });
  assert.equal(init.method, "GET");
  assert.deepEqual(init.headers, { range: "bytes=100-299", "if-match": '"abc"' });
  assert.equal(init.cache, "no-store");
  assert.deepEqual(readFetchInit({}).headers, {});
});

test("readSucceeded / readCommit: served at expect_status, fallback on anything else, error without an answer", () => {
  const admit = { admit_id: "a1", expect_status: 200 };
  const ok = new Response("x", { status: 200, headers: { "content-length": "4096" } });
  assert.equal(readSucceeded(ok, admit), true);
  const c = readCommit(admit, ok, null, Date.now() - 5);
  assert.equal(c.admit_id, "a1");
  assert.equal(c.outcome, "served");
  assert.equal(c.backend_status, 200);
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
