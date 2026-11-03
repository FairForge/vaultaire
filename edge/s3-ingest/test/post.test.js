// WP-POST-1 PR B (Worker 0.7.0): a browser's POST policy form at the edge.
// The Worker reads the fields, describes the form with the file part's
// exact size, and moves the file part — and only it — to the backend PUT
// the control plane signed; a form it cannot read, or one the control plane
// declines, reaches the origin byte for byte.
import { test, afterEach } from "node:test";
import assert from "node:assert/strict";
import worker, { classify, boundaryOf, parseFormPrefix, fileStream, formTail } from "../src/index.js";

const env = { VAULTAIRE_ORIGIN: "https://origin.test", VAULTAIRE_EDGE_TOKEN: "tok" };
const realFetch = globalThis.fetch;
afterEach(() => {
  globalThis.fetch = realFetch;
});
const enc = new TextEncoder();
const B = "----formB0undary";

function form(fields, filename, content) {
  let head = "";
  for (const [k, v] of fields) head += `--${B}\r\nContent-Disposition: form-data; name="${k}"\r\n\r\n${v}\r\n`;
  head += `--${B}\r\nContent-Disposition: form-data; name="file"; filename="${filename}"\r\nContent-Type: image/png\r\n\r\n`;
  const h = enc.encode(head);
  const t = enc.encode(`\r\n--${B}--\r\n`);
  const out = new Uint8Array(h.length + content.length + t.length);
  out.set(h);
  out.set(content, h.length);
  out.set(t, h.length + content.length);
  return { body: out, fileStart: h.length };
}

function postRequest(body, extra = {}) {
  return new Request("https://s3.stored.cloud/my-bucket", {
    method: "POST",
    body,
    headers: { "content-type": `multipart/form-data; boundary=${B}`, "content-length": String(body.length), ...extra },
  });
}

async function bytesOf(stream) {
  return new Uint8Array(await new Response(stream).arrayBuffer());
}

const fields = [["key", "in/${filename}"], ["Content-Type", "image/png"], ["policy", "cG9saWN5"], ["x-amz-signature", "abc"]];

test("classify: a form to a bucket's path is the edge's; anything else the origin's", () => {
  const { body } = form(fields, "a.png", enc.encode("x"));
  assert.deepEqual(classify(postRequest(body)), { kind: "post" });
  assert.equal(classify(postRequest(body, { authorization: "AWS4-HMAC-SHA256 x" })).kind, "pass");
  assert.equal(classify(new Request("https://s3.stored.cloud/b/k?uploads", { method: "POST" })).kind, "pass");
  const deep = new Request("https://s3.stored.cloud/b/k", { method: "POST", body, headers: { "content-type": `multipart/form-data; boundary=${B}` } });
  assert.equal(classify(deep).reason, "no_bucket");
  assert.equal(boundaryOf(`multipart/form-data; boundary="${B}"`), B);
  assert.equal(boundaryOf("application/json"), "");
});

test("parseFormPrefix: the fields, the file's name and offset; more bytes asked for until the file part", () => {
  const { body, fileStart } = form(fields, "cat é.png", enc.encode("PNGDATA"));
  const got = parseFormPrefix(body, B);
  assert.equal(got.fileStart, fileStart);
  assert.equal(got.filename, "cat é.png");
  assert.deepEqual(got.fields, { key: "in/${filename}", "content-type": "image/png", policy: "cG9saWN5", "x-amz-signature": "abc" });
  assert.equal(parseFormPrefix(body.subarray(0, fileStart - 3), B), null, "the file part's headers are not complete yet");
  const noFile = enc.encode(`--${B}\r\nContent-Disposition: form-data; name="key"\r\n\r\nk\r\n--${B}--\r\n`);
  assert.throws(() => parseFormPrefix(noFile, B), /exactly one file/);
  const twice = enc.encode(`--${B}\r\nContent-Disposition: form-data; name="key"\r\n\r\nk\r\n--${B}\r\nContent-Disposition: form-data; name="Key"\r\n\r\nj\r\n--${B}\r\n`);
  assert.throws(() => parseFormPrefix(twice, B), /twice/);
});

function readerOf(chunks) {
  let i = 0;
  return new ReadableStream({ pull(c) { if (i < chunks.length) c.enqueue(chunks[i++]); else c.close(); } }).getReader();
}

test("fileStream: exactly the file's bytes, the closing delimiter proven and withheld", async () => {
  const content = enc.encode("0123456789".repeat(100));
  const tail = formTail(B);
  const all = new Uint8Array(content.length + tail.length);
  all.set(content);
  all.set(tail, content.length);
  // Split anywhere, the tail across chunks.
  const out = await bytesOf(fileStream(all.subarray(0, 7), readerOf([all.subarray(7, 990), all.subarray(990, content.length + 3), all.subarray(content.length + 3)]), content.length, tail));
  assert.deepEqual(out, content);
  // A tail that is not the delimiter, and bytes after it: errors.
  const bad = new Uint8Array(all);
  bad[bad.length - 4] = 0x41;
  await assert.rejects(bytesOf(fileStream(new Uint8Array(0), readerOf([bad]), content.length, tail)), /closing delimiter/);
  const extra = new Uint8Array(all.length + 3);
  extra.set(all);
  await assert.rejects(bytesOf(fileStream(new Uint8Array(0), readerOf([extra]), content.length, tail)), /after the closing delimiter/);
  await assert.rejects(bytesOf(fileStream(new Uint8Array(0), readerOf([all.subarray(0, 500)]), content.length, tail)), /ended inside/);
});

test("an admitted form: the file part to the signed PUT, the commit's POST answer to the client", async () => {
  const content = enc.encode("x".repeat(5000));
  const { body } = form(fields, "cat.png", content);
  const seen = {};
  globalThis.fetch = async (input, init = {}) => {
    const url = typeof input === "string" ? input : input.url;
    if (url.endsWith("/api/v1/edge/admit")) {
      seen.admit = JSON.parse(init.body);
      return Response.json({ edge: true, admit_id: "a1", url: "https://backend.test/put", method: "PUT", send_headers: { "content-length": String(content.length) } });
    }
    if (url === "https://backend.test/put") {
      seen.put = await bytesOf(init.body);
      seen.putLength = init.headers["content-length"];
      return new Response(null, { status: 200, headers: { etag: '"e"' } });
    }
    if (url.endsWith("/api/v1/edge/commit")) {
      seen.commit = JSON.parse(init.body);
      return Response.json({ status: 201, headers: { "content-type": "application/xml", location: "https://s3.stored.cloud/my-bucket/in/cat.png" }, body: "<PostResponse/>" });
    }
    throw new Error("unexpected fetch " + url);
  };
  const resp = await worker.fetch(postRequest(body), env, { waitUntil() {} });
  assert.equal(resp.status, 201);
  assert.equal(await resp.text(), "<PostResponse/>");
  assert.equal(seen.admit.op, "post");
  assert.equal(seen.admit.form.file_size, content.length, "the file part's exact size");
  assert.equal(seen.admit.form.filename, "cat.png");
  assert.equal(seen.admit.form.fields.key, "in/${filename}");
  assert.equal(seen.admit.headers.authorization, undefined);
  assert.deepEqual(seen.put, content, "only the file part reaches the backend");
  assert.equal(seen.commit.outcome, "ok");
  assert.equal(seen.commit.backend_etag, '"e"');
});

test("a declined form reaches the origin byte for byte", async () => {
  const { body } = form(fields, "cat.png", enc.encode("y".repeat(3000)));
  let toOrigin = null;
  globalThis.fetch = async (input, init = {}) => {
    const url = typeof input === "string" ? input : input.url;
    if (url.endsWith("/api/v1/edge/admit")) return Response.json({ edge: false, reason: "post_over_edge_cap" });
    toOrigin = await bytesOf(input.body);
    return new Response(null, { status: 204 });
  };
  const resp = await worker.fetch(postRequest(body), env, { waitUntil() {} });
  assert.equal(resp.status, 204);
  assert.deepEqual(toOrigin, body);
});
