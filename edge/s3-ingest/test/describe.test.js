// The classifier, the descriptor builder and the error relay — the pure
// parts of the one-file Worker (the body itself never meets JavaScript).
import { test } from "node:test";
import assert from "node:assert/strict";
import { backendErrorCode, classify, describe, relayResponse, s3ErrorXML } from "../src/index.js";

const sig = "AWS4-HMAC-SHA256 Credential=VKX/20261026/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=00";

function put(url, headers = {}, method = "PUT") {
  return new Request(url, { method, headers: { Authorization: sig, "Content-Length": "5", ...headers }, body: method === "PUT" ? "hello" : null });
}

test("classify: a signed PutObject and an UploadPart are the edge's; the rest the origin's", () => {
  assert.equal(classify(put("https://s3.stored.cloud/b/k.bin")).kind, "put");
  assert.equal(classify(put("https://s3.stored.cloud/b/dir/k.bin?partNumber=2&uploadId=upload-1")).kind, "part");
  assert.deepEqual(classify(put("https://s3.stored.cloud/b/k.bin?acl")), { kind: "pass", reason: "query" });
  // aws-sdk-go-v2 (rclone) names the operation in the query: still the edge's.
  assert.equal(classify(put("https://s3.stored.cloud/b/k.bin?x-id=PutObject")).kind, "put");
  assert.equal(classify(put("https://s3.stored.cloud/b/k.bin?partNumber=1&uploadId=u&x-id=UploadPart")).kind, "part");
  assert.deepEqual(classify(put("https://s3.stored.cloud/b")), { kind: "pass", reason: "no_key" });
  assert.deepEqual(classify(put("https://s3.stored.cloud/b/")), { kind: "pass", reason: "no_key" });
  assert.deepEqual(classify(put("https://s3.stored.cloud/b/k", { "x-amz-copy-source": "/b/x" })), { kind: "pass", reason: "copy" });
  assert.deepEqual(classify(new Request("https://s3.stored.cloud/b/k", { method: "GET" })), { kind: "pass", reason: "unsigned" }, "an unsigned GET (Part B: a signed one is the edge's)");
  assert.deepEqual(classify(new Request("https://s3.stored.cloud/b/k", { method: "DELETE", headers: { Authorization: sig } })), { kind: "pass", reason: "method" });
  const presigned = new Request("https://s3.stored.cloud/b/k?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=00", { method: "PUT", body: "x", headers: { "content-length": "1" } });
  assert.deepEqual(classify(presigned), { kind: "pass", reason: "unsigned" });
  const bearer = put("https://s3.stored.cloud/b/k", { Authorization: "Bearer x" });
  assert.deepEqual(classify(bearer), { kind: "pass", reason: "unsigned" });
  // An aws-chunked body (aws-cli v2's CRC32 trailer over TLS) is the origin's:
  // decoding it would put JavaScript in the byte path.
  const chunked = put("https://s3.stored.cloud/b/k", { "x-amz-content-sha256": "STREAMING-UNSIGNED-PAYLOAD-TRAILER", "content-encoding": "aws-chunked" });
  assert.deepEqual(classify(chunked), { kind: "pass", reason: "aws_chunked" });
  const plain = put("https://s3.stored.cloud/b/k", { "x-amz-content-sha256": "UNSIGNED-PAYLOAD" });
  assert.equal(classify(plain).kind, "put");
});

test("describe: every header lower-cased, hop-by-hop dropped, Authorization kept in the JSON, the raw path and query", () => {
  const r = put("https://s3.stored.cloud/my-bucket/dir%2Fa%20b.txt?partNumber=1&uploadId=u-1", {
    "X-Amz-Date": "20261026T120000Z",
    "Content-MD5": "AQEBAQEBAQEBAQEBAQEBAQ==",
    Connection: "keep-alive",
    "Transfer-Encoding": "chunked",
    "CF-Connecting-IP": "203.0.113.9",
  });
  const d = describe(r, "part", "req-1");
  assert.equal(d.op, "part");
  assert.equal(d.method, "PUT");
  assert.equal(d.host, "s3.stored.cloud");
  assert.equal(d.path, "/my-bucket/dir%2Fa%20b.txt");
  assert.equal(d.query, "partNumber=1&uploadId=u-1");
  assert.equal(d.client_ip, "203.0.113.9");
  assert.equal(d.request_id, "req-1");
  assert.equal(d.headers.authorization, sig);
  assert.equal(d.headers["x-amz-date"], "20261026T120000Z");
  assert.equal(d.headers["content-md5"], "AQEBAQEBAQEBAQEBAQEBAQ==");
  assert.equal(d.headers.host, "s3.stored.cloud");
  assert.equal(d.headers.connection, undefined, "hop-by-hop dropped");
  assert.equal(d.headers["transfer-encoding"], undefined);
  for (const k of Object.keys(d.headers)) assert.equal(k, k.toLowerCase(), `${k} lower-cased`);
});

test("relayResponse sends the S3 XML unchanged with its status and headers", async () => {
  const body = s3ErrorXML("QuotaExceeded", "Storage quota exceeded. Upgrade your plan for more storage.", "/b/k", "req-2");
  const resp = relayResponse({ status: 403, headers: { "Content-Type": "application/xml", "x-amz-request-id": "req-2" }, body });
  assert.equal(resp.status, 403);
  assert.equal(resp.headers.get("content-type"), "application/xml");
  assert.equal(await resp.text(), body);
  const ok = relayResponse({ status: 200, headers: { ETag: '"abc"', "x-amz-object-size": "5" } });
  assert.equal(ok.status, 200);
  assert.equal(ok.headers.get("etag"), '"abc"');
  assert.equal(await ok.text(), "");
});

test("backendErrorCode reads the backend's <Code>", () => {
  assert.equal(backendErrorCode('<?xml version="1.0"?><Error><Code>BadDigest</Code><Message>x</Message></Error>'), "BadDigest");
  assert.equal(backendErrorCode(""), "");
  assert.equal(backendErrorCode("<html>413</html>"), "");
});
