// WP-EDGE-4 Part A: the mirror at the edge. putTwo tees one body into two
// PUTs with the runtime's own tee() — no JavaScript reads the bytes — the
// primary decides the write, a mirror that fails or falls behind its
// deadline is cut loose (D-EDGE-46).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { createHash } from "node:crypto";
import { putTwo, putTwoInline, mirrorDeadlineMs, setIsolateBudget, isolateBudget, MIRROR_MAX_LAG } from "../src/codec.js";
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
    const r = await putTwoInline(fetch, [pa.url + "/k", pb.url + "/k"], { method: "PUT", headers: { "content-length": String(bytes.length) }, body: stream, duplex: "half" }, bytes.length);
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
    const r = await putTwoInline(fetch, [pa.url, pb.url], { method: "PUT", headers: { "content-length": String(bytes.length) }, body: stream, duplex: "half" }, bytes.length);
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
    const r = await putTwoInline(fetch, [pa.url, pb.url], { method: "PUT", headers: { "content-length": String(bytes.length) }, body: stream, duplex: "half" }, bytes.length, 300);
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

// --- RP76-2: the mirror's lag is bounded; it never holds the primary ------------

// pacedTee is workerd's tee as Step 0.1 measured it: a chunk is pulled from
// the source only when BOTH branches have asked for one, so the faster
// branch waits for the slower (Node's tee buffers for the slower instead).
function pacedTee(src) {
  const reader = src.getReader();
  let waiting = [];
  let done = false;
  const branches = [0, 1].map((i) => {
    let ctl;
    return {
      want: null,
      stream: new ReadableStream({
        start(c) { ctl = c; },
        pull() {
          return new Promise((res) => {
            branches[i].want = res;
            waiting.push(i);
            maybe();
          });
        },
        cancel() { branches[i].cancelled = true; maybe(); },
      }, { highWaterMark: 0 }),
      enqueue: (v) => ctl.enqueue(v),
      close: () => ctl.close(),
    };
  });
  async function maybe() {
    const live = branches.filter((b) => !b.cancelled);
    if (done || live.length === 0 || !live.every((b) => b.want)) return;
    const wants = live.map((b) => { const w = b.want; b.want = null; return w; });
    const { done: d, value } = await reader.read();
    for (const b of live) {
      if (d) b.close(); else b.enqueue(value);
    }
    if (d) done = true;
    wants.forEach((w) => w());
  }
  return [branches[0].stream, branches[1].stream];
}

// throttled reads a request at about rate bytes/s, then stores it.
const throttled = (rate, got) => (req, res) => {
  const parts = [];
  req.on("data", (d) => {
    parts.push(d);
    req.pause();
    setTimeout(() => req.resume(), Math.ceil((d.length / rate) * 1000));
  });
  req.on("end", () => {
    const b = Buffer.concat(parts);
    got.push(b);
    res.writeHead(200, { ETag: `"${createHash("md5").update(b).digest("hex")}"` });
    res.end();
  });
  req.on("error", () => {});
};

// timed stores a request and records when its last byte arrived.
const timed = (got) => async (req, res) => {
  const b = await readAll(req);
  got.push({ b, at: Date.now() });
  res.writeHead(200, { ETag: `"${createHash("md5").update(b).digest("hex")}"` });
  res.end();
};

function chunked(n, piece = 256 << 10) {
  const bytes = new Uint8Array(n).map((_, i) => (i * 7) & 255);
  let off = 0;
  return {
    bytes,
    stream: new ReadableStream({
      pull(c) {
        if (off >= n) { c.close(); return; }
        c.enqueue(bytes.slice(off, off + piece));
        off += piece;
      },
    }, { highWaterMark: 0 }),
  };
}

for (const [name, tee] of [["runtime tee", undefined], ["workerd-paced tee", pacedTee]]) {
  test(`putTwo (${name}): a mirror that answers nothing is cut within 5 s of the primary's answer`, async () => {
    const a = [];
    const pa = await listen(timed(a));
    const pb = await listen((req) => { req.pause(); }); // never reads, never answers
    try {
      const { bytes, stream } = chunked(32 << 20);
      const r = await putTwoInline(fetch, [pa.url, pb.url], { method: "PUT", headers: { "content-length": String(bytes.length) }, body: stream, duplex: "half" }, bytes.length, undefined, { tee });
      const back = Date.now();
      assert.equal(r.resp.status, 200);
      assert.equal(r.mirrorETag, "");
      assert.equal(r.mirrorCut, true);
      assert.deepEqual(a[0].b, Buffer.from(bytes));
      assert.ok(back - a[0].at < 5_500, `putTwo returned ${back - a[0].at} ms after the primary's last byte`);
    } finally {
      pa.s.close();
      pb.s.closeAllConnections?.();
      pb.s.close();
    }
  });

  test(`putTwo (${name}): a mirror at a tenth of the primary's pace is cut at 16 MiB of lag; the primary keeps its pace`, async () => {
    const a = [];
    const b = [];
    const pa = await listen(timed(a));
    const pb = await listen(throttled(4 << 20, b)); // 4 MiB/s: 16 s for 64 MiB
    try {
      const { bytes, stream } = chunked(64 << 20);
      const started = Date.now();
      const r = await putTwoInline(fetch, [pa.url, pb.url], { method: "PUT", headers: { "content-length": String(bytes.length) }, body: stream, duplex: "half" }, bytes.length, undefined, { tee });
      assert.equal(r.resp.status, 200);
      assert.deepEqual(a[0].b, Buffer.from(bytes), "every byte to the primary");
      assert.ok(a[0].at - started < 8_000, `the primary took ${a[0].at - started} ms (the mirror's pace would be 16 s)`);
      assert.equal(r.mirrorCut, true);
      assert.equal(r.mirrorETag, "");
      assert.equal(b.length, 0, "the cut mirror stored nothing");
    } finally {
      pa.s.close();
      pb.s.closeAllConnections?.();
      pb.s.close();
    }
  });
}

// A mirror whose lag cannot pass the bound (a body under 16 MiB) is never
// cut. (A 40 MiB body between two local sinks was flaky on a CI runner: a
// mirror only slightly slower than the primary may trail by 16 MiB on a
// large body, and is cut by design.)
test("putTwo: a mirror whose lag stays under 16 MiB is not cut (both legs store, the ETag returned)", async () => {
  const a = [];
  const b = [];
  const pa = await listen(store(a));
  const pb = await listen(store(b));
  try {
    const { bytes, stream } = chunked(12 << 20);
    const r = await putTwoInline(fetch, [pa.url, pb.url], { method: "PUT", headers: { "content-length": String(bytes.length) }, body: stream, duplex: "half" }, bytes.length, undefined, { tee: pacedTee });
    assert.equal(r.mirrorCut, false);
    assert.equal(r.mirrorETag, r.resp.headers.get("etag"));
    assert.deepEqual(b[0], Buffer.from(bytes));
  } finally {
    pa.s.close();
    pb.s.close();
  }
});

// 0.5.1's regression (found on prod 2026-10-28 23:58 UTC, rolled back at
// 23:59): putTwo's counting streams have no known length, so workerd sent
// both PUTs chunked with no Content-Length and iDrive answered 411. Where
// the runtime has FixedLengthStream, each body goes through one of the
// admitted length (Node's fetch sends the header anyway: this stand-in
// records that putTwo asked).
test("putTwo: each body has the admitted length where the runtime has FixedLengthStream", async () => {
  const made = [];
  class FakeFixedLengthStream extends TransformStream {
    constructor(n) { super(); made.push(n); }
  }
  globalThis.FixedLengthStream = FakeFixedLengthStream;
  const a = [];
  const b = [];
  const pa = await listen(store(a));
  const pb = await listen(store(b));
  try {
    const { bytes, stream } = body(2 << 20);
    const r = await putTwoInline(fetch, [pa.url, pb.url], { method: "PUT", headers: { "content-length": String(bytes.length) }, body: stream, duplex: "half" }, bytes.length);
    assert.equal(r.resp.status, 200);
    assert.deepEqual(made, [bytes.length, bytes.length], "the primary's body and the mirror's, each of the admitted length");
    assert.deepEqual(b[0], Buffer.from(bytes));
  } finally {
    delete globalThis.FixedLengthStream;
    pa.s.close();
    pb.s.close();
  }
});

// --- RP78-1: the mirror's buffer is the isolate's bytes ---------------------------

test("putTwo: a mirrored PUT the isolate budget cannot cover is sent unmirrored", async () => {
  // The isolate's budget with 10 MiB of its 20 held by another request:
  // the mirror's 18 MiB buffer cannot be had within the short wait. (Held
  // explicitly: two concurrent PUTs raced on a slow CI runner — the first
  // mirror could be cut for lag, freeing its bytes, inside the second's
  // wait.)
  const restore = setIsolateBudget(20 << 20);
  const a = [];
  let mirrorCalls = 0;
  const pa = await listen(store(a));
  const pb = await listen(async (req, res) => {
    mirrorCalls++;
    await readAll(req);
    res.writeHead(200, { ETag: '"m"' });
    res.end();
  });
  try {
    assert.equal(await isolateBudget().acquireWithin(10 << 20, 0), true);
    const one = chunked(8 << 20);
    const init = (st, n) => ({ method: "PUT", headers: { "content-length": String(n) }, body: st, duplex: "half" });
    const r1 = await putTwo(fetch, [pa.url + "/1", pb.url + "/1"], init(one.stream, one.bytes.length), one.bytes.length);
    assert.equal(r1.mirrorUnbudgeted, true, "no budget for the mirror: unmirrored");
    assert.equal(r1.resp.status, 200, "and stored on the primary");
    await r1.settle;
    assert.equal(mirrorCalls, 0, "one link only");
    assert.equal(isolateBudget().held, 10 << 20, "nothing taken for it");

    isolateBudget().release(10 << 20);
    const two = chunked(8 << 20);
    const r2 = await putTwo(fetch, [pa.url + "/2", pb.url + "/2"], init(two.stream, two.bytes.length), two.bytes.length);
    assert.equal(r2.mirrorUnbudgeted, false, "with the budget free: mirrored");
    await r2.settle;
    assert.equal(mirrorCalls, 1);
    assert.equal(a.length, 2);
    assert.equal(isolateBudget().held, 0, "every byte given back when the mirror ended");
  } finally {
    restore();
    pa.s.close();
    pb.s.close();
  }
});

// --- RP78-2: the client is answered at the primary's pace ------------------------

test("putTwo: the client is answered at the primary's pace; the mirror settles after", async () => {
  const a = [];
  const b = [];
  const pa = await listen(timed(a));
  const pb = await listen(throttled(2 << 20, b)); // 2 MB/s: ~2 s for 4 MiB
  try {
    const { bytes, stream } = chunked(4 << 20);
    const started = Date.now();
    const r = await putTwo(fetch, [pa.url, pb.url], { method: "PUT", headers: { "content-length": String(bytes.length) }, body: stream, duplex: "half" }, bytes.length);
    const answered = Date.now() - started;
    assert.equal(r.resp.status, 200);
    assert.ok(answered < 1_000, `answered after ${answered} ms (the mirror's pace would be ~2 s)`);
    assert.ok(isolateBudget().held > 0, "the mirror's buffer is charged until it ends");
    const settled = await r.settle;
    assert.ok(Date.now() - started > answered + 500, "the mirror settled after the answer");
    assert.equal(settled.cut, false);
    assert.equal(settled.etag, `"${createHash("md5").update(Buffer.from(bytes)).digest("hex")}"`);
    assert.deepEqual(b[0], Buffer.from(bytes));
    assert.equal(isolateBudget().held, 0);
  } finally {
    pa.s.close();
    pb.s.close();
  }
  assert.equal(MIRROR_MAX_LAG, 16 << 20);
});

test("putTwoInline: a codec chunk waits for its mirror (both ETags)", async () => {
  const a = [];
  const b = [];
  const pa = await listen(store(a));
  const pb = await listen(store(b));
  try {
    const { bytes, stream } = body(1 << 20);
    const r = await putTwoInline(fetch, [pa.url, pb.url], { method: "PUT", headers: { "content-length": String(bytes.length) }, body: stream, duplex: "half" }, bytes.length);
    assert.equal(r.mirrorETag, r.resp.headers.get("etag"));
    assert.equal(r.mirrorCut, false);
  } finally {
    pa.s.close();
    pb.s.close();
  }
});

test("settleMirror: posts the mirror's ETag, or cut, to /api/v1/edge/mirror", async () => {
  const { settleMirror } = await import("../src/index.js");
  const calls = [];
  const cp = async (env, path, body) => {
    calls.push({ path, body });
    return new Response('{"state":"ready"}');
  };
  await settleMirror({}, "A1", { settle: Promise.resolve({ etag: '"m"', cut: false }) }, "r1", cp);
  await settleMirror({}, "A2", { settle: Promise.resolve({ etag: "", cut: true }) }, "r2", cp);
  await settleMirror({}, "A3", { settle: Promise.resolve({ etag: "", cut: false }) }, "r3", cp);
  assert.deepEqual(calls, [
    { path: "/api/v1/edge/mirror", body: { admit_id: "A1", etag: '"m"' } },
    { path: "/api/v1/edge/mirror", body: { admit_id: "A2", cut: true } },
    { path: "/api/v1/edge/mirror", body: { admit_id: "A3", cut: false } },
  ]);
});
