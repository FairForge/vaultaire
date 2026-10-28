// WP-EDGE-4 Part A: the mirror at the edge. putTwo tees one body into two
// PUTs with the runtime's own tee() — no JavaScript reads the bytes — the
// primary decides the write, a mirror that fails or falls behind its
// deadline is cut loose (D-EDGE-46).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { createHash } from "node:crypto";
import { putTwo, mirrorDeadlineMs } from "../src/codec.js";
import { mirrorReadInit, readFetchInit } from "../src/index.js";

function listen(handler) {
  return new Promise((res) => {
    const s = createServer(handler);
    s.listen(0, "127.0.0.1", () => res({ s, url: `http://127.0.0.1:${s.address().port}` }));
  });
}
function readAll(req) {
  return new Promise((res) => {
    const parts = [];
    req.on("data", (d) => parts.push(d));
    req.on("end", () => res(Buffer.concat(parts)));
  });
}
const store = (got) => async (req, res) => {
  const b = await readAll(req);
  got.push(b);
  res.writeHead(200, { ETag: `"${createHash("md5").update(b).digest("hex")}"` });
  res.end();
};
function body(n) {
  const bytes = new Uint8Array(n).map((_, i) => (i * 31) & 255);
  return { bytes, stream: new ReadableStream({ start(c) { c.enqueue(bytes); c.close(); } }) };
}

test("putTwo: both legs receive identical bytes; the mirror's ETag is returned", async () => {
  const a = [];
  const b = [];
  const pa = await listen(store(a));
  const pb = await listen(store(b));
  try {
    const { bytes, stream } = body(3 << 20);
    const r = await putTwo(fetch, [pa.url + "/k", pb.url + "/k"], { method: "PUT", headers: { "content-length": String(bytes.length) }, body: stream, duplex: "half" }, bytes.length);
    assert.equal(r.resp.status, 200);
    assert.deepEqual(a[0], Buffer.from(bytes));
    assert.deepEqual(b[0], Buffer.from(bytes));
    assert.equal(r.mirrorETag, r.resp.headers.get("etag"));
  } finally {
    pa.s.close();
    pb.s.close();
  }
});

test("putTwo: a failing mirror still lets the primary commit", async () => {
  const a = [];
  const pa = await listen(store(a));
  const pb = await listen(async (req, res) => { await readAll(req); res.writeHead(503); res.end(); });
  try {
    const { bytes, stream } = body(1 << 20);
    const r = await putTwo(fetch, [pa.url, pb.url], { method: "PUT", headers: { "content-length": String(bytes.length) }, body: stream, duplex: "half" }, bytes.length);
    assert.equal(r.resp.status, 200);
    assert.equal(r.mirrorETag, "");
    assert.equal(a[0].length, bytes.length);
  } finally {
    pa.s.close();
    pb.s.close();
  }
});

test("putTwo: a mirror past its deadline is aborted and the primary finishes", async () => {
  const a = [];
  const pa = await listen(store(a));
  const pb = await listen((req) => { req.pause(); }); // never reads, never answers
  try {
    const { bytes, stream } = body(256 << 10);
    const started = Date.now();
    const r = await putTwo(fetch, [pa.url, pb.url], { method: "PUT", headers: { "content-length": String(bytes.length) }, body: stream, duplex: "half" }, bytes.length, 300);
    assert.equal(r.resp.status, 200);
    assert.equal(r.mirrorETag, "");
    assert.ok(Date.now() - started < 5000);
  } finally {
    pa.s.close();
    pb.s.closeAllConnections?.();
    pb.s.close();
  }
  assert.equal(mirrorDeadlineMs(0), 30_000);
  assert.equal(mirrorDeadlineMs(64 << 20), 30_000 + 32_000, "30 s plus the body at 2 MiB/s");
});

test("mirrorReadInit: the mirror's own signed headers, never cached", () => {
  const admit = { send_headers: { range: "bytes=0-9", "if-match": '"x"' }, mirror_send_headers: { range: "bytes=0-9" } };
  assert.deepEqual(mirrorReadInit(admit), { method: "GET", headers: { range: "bytes=0-9" }, cache: "no-store" });
  assert.deepEqual(mirrorReadInit({ send_headers: { range: "bytes=0-1" } }), readFetchInit({ send_headers: { range: "bytes=0-1" } }));
});
