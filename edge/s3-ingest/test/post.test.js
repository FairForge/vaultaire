// WP-POST-1 PR B (Worker 0.7.0): a browser's POST policy form at the edge.
// The Worker reads the fields, describes the form with the file part's
// exact size, and moves the file part — and only it — to the backend PUT
// the control plane signed; a form it cannot read, or one the control plane
// declines, reaches the origin byte for byte.
import { test, afterEach } from "node:test";
import assert from "node:assert/strict";
import worker, { classify, boundaryOf, parseFormPrefix, fileStream, formTail, describe } from "../src/index.js";

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

// --- RP94-2 (0.7.1): the tail proven before the backend has its last byte ---

/**
 * FixedLengthStreamDouble behaves as workerd's FixedLengthStream does
 * (identity-transform-stream.c++, tryReadInternal): its readable reports
 * EOF the moment `limit` bytes have passed — whatever the writable side
 * does after — and errors only when the writable is aborted or closed
 * short. The plain TransformStream the node tests fall back to propagates
 * the source's error after the last byte and hid RP94-2.
 */
class FixedLengthStreamDouble {
  constructor(limit) {
    let left = limit;
    let ctrl;
    let ended = false;
    const end = (fn) => {
      if (ended) return;
      ended = true;
      fn();
    };
    this.readable = new ReadableStream({
      start(c) {
        ctrl = c;
        if (limit === 0) end(() => c.close());
      },
    });
    this.writable = new WritableStream({
      write(chunk) {
        if (ended) return;
        if (chunk.length > left) {
          end(() => ctrl.error(new Error("FixedLengthStream: too many bytes")));
          throw new Error("too many bytes");
        }
        left -= chunk.length;
        ctrl.enqueue(chunk);
        if (left === 0) end(() => ctrl.close()); // EOF at the limit
      },
      close() {
        if (left > 0) end(() => ctrl.error(new Error("FixedLengthStream: closed short")));
      },
      abort(r) {
        end(() => ctrl.error(r || new Error("aborted")));
      },
    });
  }
}

function withFLS(t) {
  const prior = globalThis.FixedLengthStream;
  globalThis.FixedLengthStream = FixedLengthStreamDouble;
  t.after(() => {
    if (prior === undefined) delete globalThis.FixedLengthStream;
    else globalThis.FixedLengthStream = prior;
  });
}

/** throughFLS pipes a fileStream into the double, as servePost does, and reads it. */
async function throughFLS(stream, size) {
  const { readable, writable } = new FixedLengthStreamDouble(size);
  stream.pipeTo(writable).catch(() => {});
  return bytesOf(readable);
}

/** trailingForm is a form whose file is followed by another part (a named submit button). */
function trailingForm(content) {
  const { body } = form(fields, "cat.png", content);
  const tail = formTail(B);
  const extra = enc.encode(`\r\n--${B}\r\nContent-Disposition: form-data; name="submit"\r\n\r\nUpload`);
  const fileEnd = body.length - tail.length;
  const out = new Uint8Array(body.length + extra.length);
  out.set(body.subarray(0, fileEnd));
  out.set(extra, fileEnd);
  out.set(tail, fileEnd + extra.length);
  return out;
}

function edgeStub(seen, contentLength) {
  return async (input, init = {}) => {
    const url = typeof input === "string" ? input : input.url;
    if (url.endsWith("/api/v1/edge/admit")) {
      seen.admit = JSON.parse(init.body);
      return Response.json({ edge: true, admit_id: "a1", url: "https://backend.test/put", method: "PUT", send_headers: { "content-length": String(contentLength) } });
    }
    if (url === "https://backend.test/put") {
      seen.put = await bytesOf(init.body); // rejects when the body errors
      return new Response(null, { status: 200, headers: { etag: '"e"' } });
    }
    if (url.endsWith("/api/v1/edge/commit")) {
      seen.commit = JSON.parse(init.body);
      return Response.json(seen.commit.outcome === "ok"
        ? { status: 204, headers: {}, body: "" }
        : { status: 503, headers: { "content-type": "application/xml" }, body: "<Error><Code>SlowDown</Code></Error>" });
    }
    throw new Error("unexpected fetch " + url);
  };
}

test("a part after the file errors the stream before its last bytes", async (t) => {
  withFLS(t);
  const content = enc.encode("z".repeat(3000));
  const body = trailingForm(content);
  const { fileStart } = parseFormPrefix(body, B);
  const tail = formTail(B);
  const size = body.length - fileStart - tail.length; // what the Worker computes: the trailing part inside
  // Every split, the delimiter of the trailing part across reads too.
  for (const cut of [1, 7, 3001, 3005, size - 1]) {
    const rest = body.subarray(fileStart);
    await assert.rejects(throughFLS(fileStream(rest.subarray(0, cut), readerOf([rest.subarray(cut)]), size, tail), size), /a part follows the file/, `cut ${cut}`);
  }
  // End to end: nothing committed, the form's own 400 to the client.
  const seen = {};
  globalThis.fetch = edgeStub(seen, size);
  const resp = await worker.fetch(postRequest(body), env, { waitUntil() {} });
  assert.equal(seen.put, undefined, "the backend never received a whole body");
  assert.notEqual(seen.commit.outcome, "ok");
  assert.equal(resp.status, 400);
  const text = await resp.text();
  assert.match(text, /<Code>MalformedPOSTRequest<\/Code>/);
  assert.match(text, /a part follows the file/);
});

test("a wrong tail errors before the last bytes under a FixedLengthStream double", async (t) => {
  withFLS(t);
  const content = enc.encode("0123456789".repeat(100));
  const tail = formTail(B);
  const all = new Uint8Array(content.length + tail.length);
  all.set(content);
  all.set(tail, content.length);
  // The good body: exactly the file's bytes, the readable ends cleanly.
  assert.deepEqual(await throughFLS(fileStream(all.subarray(0, 9), readerOf([all.subarray(9)]), content.length, tail), content.length), content);
  const bad = new Uint8Array(all);
  bad[bad.length - 4] = 0x41;
  await assert.rejects(throughFLS(fileStream(new Uint8Array(0), readerOf([bad]), content.length, tail), content.length), /closing delimiter/);
  const extra = new Uint8Array(all.length + 3);
  extra.set(all);
  await assert.rejects(throughFLS(fileStream(new Uint8Array(0), readerOf([extra.subarray(0, 600), extra.subarray(600)]), content.length, tail), content.length), /after the closing delimiter/);
  // End to end with a wrong tail: nothing committed, a 400.
  const { body } = form(fields, "cat.png", enc.encode("w".repeat(4000)));
  body[body.length - 3] = 0x41;
  const seen = {};
  globalThis.fetch = edgeStub(seen, 4000);
  const resp = await worker.fetch(postRequest(body), env, { waitUntil() {} });
  assert.equal(seen.put, undefined);
  assert.notEqual(seen.commit.outcome, "ok");
  assert.equal(resp.status, 400);
  assert.match(await resp.text(), /MalformedPOSTRequest/);
});

test("the host names post-tail", () => {
  const { body } = form(fields, "a.png", enc.encode("x"));
  const d = describe(postRequest(body), "post", "rid");
  assert.ok(d.features.includes("post-tail"), "the control plane's dataplane.FeaturePostTail");
  assert.ok(d.features.includes("window"));
});
