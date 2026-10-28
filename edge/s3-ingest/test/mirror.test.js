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
      const r = await putTwo(fetch, [pa.url, pb.url], { method: "PUT", headers: { "content-length": String(bytes.length) }, body: stream, duplex: "half" }, bytes.length, undefined, { tee });
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
      const r = await putTwo(fetch, [pa.url, pb.url], { method: "PUT", headers: { "content-length": String(bytes.length) }, body: stream, duplex: "half" }, bytes.length, undefined, { tee });
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

test("putTwo: a mirror that keeps up is not cut (both legs, the lag never past 16 MiB)", async () => {
  const a = [];
  const b = [];
  const pa = await listen(store(a));
  const pb = await listen(store(b));
  try {
    const { bytes, stream } = chunked(40 << 20);
    const r = await putTwo(fetch, [pa.url, pb.url], { method: "PUT", headers: { "content-length": String(bytes.length) }, body: stream, duplex: "half" }, bytes.length, undefined, { tee: pacedTee });
    assert.equal(r.mirrorCut, false);
    assert.equal(r.mirrorETag, r.resp.headers.get("etag"));
    assert.deepEqual(b[0], Buffer.from(bytes));
  } finally {
    pa.s.close();
    pb.s.close();
  }
});
