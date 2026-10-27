// WP-EDGE-2 Part B: the Worker 0.3.0's codec pipeline against a FAKE control
// plane (an HTTP server answering index and commit in the shapes of Part A's
// harness, internal/api/edge_codec_harness_test.go) and a FAKE backend (an
// HTTP server storing PUT bodies, answering an MD5 ETag). The pipeline is
// the Worker's own function (runCodec) with the real chunker.wasm and
// e2ee.wasm; only the network is fake.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { loadGo, noWasm } from "./wasm-loader.js";
import { MD5, Semaphore, chunkStream, runCodec, CODEC_IN_FLIGHT_BYTES, ISOLATE_BYTES, isolateBudget, setIsolateBudget } from "../src/codec.js";
import { sealedLength } from "../src/seal.js";
import { codecTamper } from "../src/index.js";

const here = dirname(fileURLToPath(import.meta.url));
const vectors = JSON.parse(readFileSync(join(here, "..", "..", "..", "internal", "crypto", "testdata", "chunker_golden.json"), "utf8"));

function listen(handler) {
  return new Promise((res) => {
    const s = createServer(handler);
    s.listen(0, "127.0.0.1", () => res({ s, url: `http://127.0.0.1:${s.address().port}` }));
  });
}

function readAll(req) {
  return new Promise((res, rej) => {
    const parts = [];
    req.on("data", (d) => parts.push(d));
    req.on("end", () => res(Buffer.concat(parts)));
    req.on("error", rej);
  });
}

/** fakes starts the backend and the control plane; known = hashes the index holds as ready. */
async function fakes({ putDelayMs = 0, putStatus = 200, known = new Set(), commit = null } = {}) {
  const st = { puts: [], indexCalls: [], commits: [], stored: new Map(), inFlightPuts: 0, peakPuts: 0 };
  const backend = await listen(async (req, res) => {
    const body = await readAll(req);
    st.inFlightPuts++;
    st.peakPuts = Math.max(st.peakPuts, st.inFlightPuts);
    if (putDelayMs) await new Promise((r) => setTimeout(r, putDelayMs));
    st.inFlightPuts--;
    const etag = createHash("md5").update(body).digest("hex");
    st.puts.push({ path: req.url, contentLength: Number(req.headers["content-length"]), length: body.length, etag });
    st.stored.set(req.url, { etag, length: body.length, body });
    res.writeHead(putStatus, { ETag: `"${etag}"` });
    res.end();
  });
  const control = await listen(async (req, res) => {
    const body = JSON.parse((await readAll(req)).toString("utf8"));
    if (req.headers["x-vaultaire-edge-token"] !== "tok") {
      res.writeHead(404);
      return res.end();
    }
    if (req.url === "/api/v1/edge/index") {
      st.indexCalls.push(body.chunks.length);
      const hits = [];
      const misses = [];
      for (const c of body.chunks) {
        if (known.has(c.hash)) hits.push(c.hash);
        else misses.push({ hash: c.hash, url: `${backend.url}/vaultaire/t-_global/_global/_chunks/t/${c.hash}.k1`, send_headers: { "content-length": String(sealedLength(c.size)) }, sealed_length: sealedLength(c.size) });
      }
      res.writeHead(200, { "content-type": "application/json" });
      return res.end(JSON.stringify({ hits, misses }));
    }
    st.commits.push(body);
    if (commit) {
      const c = commit(body, st.commits.length);
      res.writeHead(c.status, { "content-type": "application/json" });
      return res.end(JSON.stringify(c.json));
    }
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ status: body.outcome === "ok" ? 200 : 503, headers: { ETag: `"${body.body_md5 || ""}"` }, body: "" }));
  });
  const cp = (path, b) => fetch(`${control.url}${path}`, { method: "POST", headers: { "content-type": "application/json", "x-vaultaire-edge-token": "tok" }, body: JSON.stringify(b) });
  return { st, backend, control, cp, close: () => { backend.s.close(); control.s.close(); } };
}

function bodyRequest(bytes, piece = 1 << 20) {
  let off = 0;
  const stream = new ReadableStream({
    pull(ctl) {
      if (off >= bytes.length) return ctl.close();
      const n = Math.min(piece, bytes.length - off);
      ctl.enqueue(bytes.slice(off, off + n));
      off += n;
    },
  });
  return { headers: new Headers({ "content-length": String(bytes.length) }), body: stream };
}

async function codecDeps() {
  return { chunker: await loadGo("chunker.wasm", "storedChunker"), e2ee: await loadGo("e2ee.wasm", "storedE2EE") };
}

function admitFor(e2ee) {
  const pub = e2ee.publicKey(new Uint8Array(32).fill(7)).value;
  return { edge: true, admit_id: "admit-1", codec: { version: 1, kem_public_key: Buffer.from(pub).toString("base64"), chunk_threshold: 4 << 20, segment_size: 1 << 20 } };
}

test("md5: the pure-JS MD5 equals node's, fed in uneven pieces", () => {
  for (const n of [0, 1, 55, 56, 63, 64, 65, 1000, 123457]) {
    const b = new Uint8Array(n).map((_, i) => (i * 7 + n) & 255);
    const m = new MD5();
    for (let o = 0; o < n; o += 37) m.update(b.subarray(o, Math.min(n, o + 37)));
    assert.equal(m.hex(), createHash("md5").update(b).digest("hex"), `n=${n}`);
  }
});

test("the golden body's boundaries through the cutter TransformStream are Go's", { skip: noWasm }, async () => {
  const { chunker } = await codecDeps();
  const body = chunker.goldenBody(vectors.seed, vectors.size).value;
  const cutter = chunkStream(chunker);
  const lengths = [];
  const started = Date.now();
  await bodyRequest(body, 3 << 19).body.pipeThrough(cutter.stream).pipeTo(new WritableStream({ write: (c) => { lengths.push(c.length); } }));
  const ms = Date.now() - started;
  assert.deepEqual(lengths, vectors.chunk_lengths, "byte-identical boundaries");
  console.log(`cutter: ${vectors.size >> 20} MiB in ${ms} ms (${Math.round(vectors.size / 1048576 / (ms / 1000))} MB/s in node)`);
});

test("runCodec: a 20 MiB body — index calls of ≤ 64 chunks, one PUT per miss at its sealed length, the commit names the backend's ETags", { skip: noWasm }, async () => {
  const deps = await codecDeps();
  const f = await fakes();
  try {
    const body = deps.chunker.goldenBody(4620, 20 << 20).value;
    const relay = await runCodec(bodyRequest(body), admitFor(deps.e2ee), {}, {}, { ...deps, controlPlane: f.cp });
    assert.equal(relay.status, 200);
    assert.ok(f.st.indexCalls.length >= 1 && f.st.indexCalls.every((n) => n >= 1 && n <= 64), `index batches ${f.st.indexCalls}`);
    assert.equal(f.st.commits.length, 1);
    const commit = f.st.commits[0];
    assert.equal(commit.outcome, "ok");
    assert.equal(commit.admit_id, "admit-1");
    assert.equal(commit.body_md5, createHash("md5").update(body).digest("hex"));
    assert.equal(commit.chunks.reduce((a, c) => a + c.size, 0), body.length, "the chunk list covers the body");
    const stored = commit.chunks.filter((c) => c.status === "stored");
    assert.equal(f.st.puts.length, stored.length, "one PUT per miss");
    for (const p of f.st.puts) assert.equal(p.contentLength, p.length, "content-length is the body's");
    for (const c of stored) {
      const blob = [...f.st.stored.values()].find((s) => s.etag === c.backend_etag.replace(/"/g, ""));
      assert.ok(blob, `the commit's ETag ${c.backend_etag} is one the backend answered`);
      assert.equal(c.sealed_size, sealedLength(c.size));
      assert.equal(blob.length, c.sealed_size);
      assert.equal(blob.body[0], 0x06, "a 0x06 blob");
    }
    // Same boundaries as the chunker over the whole body.
    const { chunker } = deps;
    const id = chunker.create().value;
    const want = [...chunker.push(id, body).value, ...chunker.finish(id).value];
    chunker.free(id);
    assert.deepEqual(commit.chunks.map((c) => c.size), want);
    for (const c of commit.chunks) assert.match(c.hash, /^[0-9a-f]{64}$/);
  } finally {
    f.close();
  }
});

test("runCodec: chunks the index holds are hits — no PUT, the commit says hit", { skip: noWasm }, async () => {
  const deps = await codecDeps();
  const body = deps.chunker.goldenBody(4621, 6 << 20).value;
  const first = await fakes();
  let hashes;
  try {
    await runCodec(bodyRequest(body), admitFor(deps.e2ee), {}, {}, { ...deps, controlPlane: first.cp });
    hashes = new Set(first.st.commits[0].chunks.map((c) => c.hash));
  } finally {
    first.close();
  }
  const again = await fakes({ known: hashes });
  try {
    const relay = await runCodec(bodyRequest(body), admitFor(deps.e2ee), {}, {}, { ...deps, controlPlane: again.cp });
    assert.equal(relay.status, 200);
    assert.equal(again.st.puts.length, 0, "0 PUTs");
    assert.ok(again.st.commits[0].chunks.every((c) => c.status === "hit"));
  } finally {
    again.close();
  }
});

test("runCodec: a repeated chunk is asked once and committed as a hit after its first occurrence", { skip: noWasm }, async () => {
  const deps = await codecDeps();
  const block = deps.chunker.goldenBody(4622, 12 << 20).value;
  // The same 12 MiB block twice: the chunker cuts the same chunks again
  // after the first boundary inside the second copy.
  const body = new Uint8Array(block.length * 2);
  body.set(block, 0);
  body.set(block, block.length);
  const f = await fakes();
  try {
    await runCodec(bodyRequest(body), admitFor(deps.e2ee), {}, {}, { ...deps, controlPlane: f.cp });
    const chunks = f.st.commits[0].chunks;
    const asked = f.st.indexCalls.reduce((a, b) => a + b, 0);
    assert.equal(asked, new Set(chunks.map((c) => c.hash)).size, "one ask per distinct hash");
    const firstSeen = new Map();
    chunks.forEach((c, i) => { if (!firstSeen.has(c.hash)) firstSeen.set(c.hash, i); });
    chunks.forEach((c, i) => { if (firstSeen.get(c.hash) !== i) assert.equal(c.status, "hit", `chunk ${i} repeats chunk ${firstSeen.get(c.hash)}`); });
    assert.ok(firstSeen.size < chunks.length, "the second copy repeats chunks of the first (precondition)");
    assert.equal(f.st.puts.length, firstSeen.size);
  } finally {
    f.close();
  }
});

test("Semaphore counts bytes: FIFO waiters, the peak in bytes, a waiter larger than the bound proceeds alone", async () => {
  const sem = new Semaphore(10);
  await sem.acquire(6);
  assert.equal(sem.held, 6);
  assert.equal(sem.wouldAdmit(4), true);
  assert.equal(sem.wouldAdmit(5), false);
  const order = [];
  const a = sem.acquire(5).then(() => order.push("a5"));
  const b = sem.acquire(1).then(() => order.push("b1")); // FIFO: never ahead of a
  await new Promise((r) => setTimeout(r, 5));
  assert.deepEqual(order, [], "b fits but waits behind a");
  sem.release(6);
  await Promise.all([a, b]);
  assert.deepEqual(order, ["a5", "b1"]);
  assert.equal(sem.held, 6);
  sem.release(6);
  // A chunk larger than the bound is admitted when nothing is held: never a deadlock.
  await sem.acquire(25);
  assert.equal(sem.held, 25);
  const c = sem.acquire(1);
  let cDone = false;
  c.then(() => { cDone = true; });
  await new Promise((r) => setTimeout(r, 5));
  assert.equal(cDone, false, "nothing beside an oversized holder");
  sem.release(25);
  await c;
  assert.equal(sem.peak, 25);
});

/** maxChunkBody is n bytes of 16 MiB regions of distinct constant non-zero bytes:
 * the chunker finds no boundary in any (an all-zero run cuts at the 1 MiB
 * minimum and dedups), so it cuts 16 MiB chunks — the RP70-1 shape. */
function maxChunkBody(n) {
  const body = new Uint8Array(n);
  for (let k = 0; k * (16 << 20) < n; k++) body.fill(0x11 + (k % 200), k * (16 << 20), Math.min(n, (k + 1) * (16 << 20)));
  return body;
}

test("back-pressure in BYTES: 200 MiB of 16 MiB chunks, PUTs +50 ms — at most 48 MiB of plaintext held, every PUT a streamed body of its sealed length", { skip: noWasm }, async () => {
  const deps = await codecDeps();
  const f = await fakes({ putDelayMs: 50 });
  try {
    const body = maxChunkBody(200 << 20);
    const sem = new Semaphore(CODEC_IN_FLIGHT_BYTES);
    const started = Date.now();
    const relay = await runCodec(bodyRequest(body), admitFor(deps.e2ee), {}, {}, { ...deps, controlPlane: f.cp, sem });
    assert.equal(relay.status, 200);
    const sizes = f.st.commits[0].chunks.map((c) => c.size);
    assert.ok(sizes.slice(0, -1).every((n) => n === 16 << 20), `the chunker cuts max-size chunks: ${sizes.map((n) => n >> 20)}`);
    assert.equal(CODEC_IN_FLIGHT_BYTES, 48 << 20);
    assert.ok(sem.peak <= CODEC_IN_FLIGHT_BYTES, `peak ${sem.peak} bytes`);
    assert.ok(sem.peak >= 32 << 20, `the bound is reached: peak ${sem.peak}`);
    assert.equal(sem.held, 0, "everything released");
    assert.equal(f.st.puts.length, sizes.length);
    for (const p of f.st.puts) {
      assert.equal(p.length, p.contentLength, "the body's length is its content-length");
      assert.ok(sizes.some((n) => sealedLength(n) === p.length), `a sealed length: ${p.length}`);
    }
    console.log(`byte bound: 200 MiB of 16 MiB chunks, PUTs +50 ms: ${Date.now() - started} ms, peak ${sem.peak >> 20} MiB, index calls ${f.st.indexCalls.length}, rss ${Math.round(process.memoryUsage().rss / 1048576)} MiB`);
  } finally {
    f.close();
  }
});

test("RP70-6: a hit's bytes are released at once — before the batch's PUTs settle", { skip: noWasm }, async () => {
  const deps = await codecDeps();
  // Learn the hashes of the first half; the second run sees them as hits
  // beside slow misses.
  const half = deps.chunker.goldenBody(4629, 6 << 20).value;
  const first = await fakes();
  let known;
  try {
    await runCodec(bodyRequest(half), admitFor(deps.e2ee), {}, {}, { ...deps, controlPlane: first.cp });
    known = new Set(first.st.commits[0].chunks.map((c) => c.hash));
  } finally {
    first.close();
  }
  const body = new Uint8Array((6 << 20) + (6 << 20));
  body.set(half, 0);
  body.set(deps.chunker.goldenBody(4630, 6 << 20).value, 6 << 20);
  const f = await fakes({ putDelayMs: 400, known });
  try {
    const sem = new Semaphore(CODEC_IN_FLIGHT_BYTES);
    const heldAtPutEnd = [];
    const origRelease = sem.release.bind(sem);
    let firstPutDone = null;
    const backendDone = f.st;
    const relay = await runCodec(bodyRequest(body), admitFor(deps.e2ee), {}, {}, {
      ...deps, controlPlane: f.cp, sem,
      fetch: async (url, init) => {
        const r = await fetch(url, init);
        if (firstPutDone === null) { firstPutDone = Date.now(); heldAtPutEnd.push(sem.held); }
        return r;
      },
    });
    void origRelease; void backendDone;
    assert.equal(relay.status, 200);
    const hits = f.st.commits[0].chunks.filter((c) => c.status === "hit");
    const hitBytes = hits.reduce((a, c) => a + c.size, 0);
    assert.ok(hitBytes > 0, "the precondition: hits beside misses");
    // Every hit's bytes were gone before the first slow PUT answered: what
    // is still held then is at most the misses' plaintext.
    const missBytes = f.st.commits[0].chunks.filter((c) => c.status === "stored").reduce((a, c) => a + c.size, 0);
    assert.ok(heldAtPutEnd[0] <= missBytes, `held ${heldAtPutEnd[0]} at the first PUT's answer, misses ${missBytes}`);
  } finally {
    f.close();
  }
});

test("a failure commits edge_error and throws (the caller answers 503) — a backend PUT 500", { skip: noWasm }, async () => {
  const deps = await codecDeps();
  const f = await fakes({ putStatus: 500 });
  try {
    const body = deps.chunker.goldenBody(4624, 3 << 20).value;
    await assert.rejects(runCodec(bodyRequest(body), admitFor(deps.e2ee), {}, {}, { ...deps, controlPlane: f.cp }), /chunk PUT answered 500/);
    assert.equal(f.st.commits.length, 1);
    assert.deepEqual(f.st.commits[0], { admit_id: "admit-1", outcome: "edge_error" });
  } finally {
    f.close();
  }
});

test("a body shorter than its content-length is an edge_error, never a commit", { skip: noWasm }, async () => {
  const deps = await codecDeps();
  const f = await fakes();
  try {
    const body = deps.chunker.goldenBody(4625, 2 << 20).value;
    const req = bodyRequest(body);
    req.headers.set("content-length", String(body.length + 5));
    await assert.rejects(runCodec(req, admitFor(deps.e2ee), {}, {}, { ...deps, controlPlane: f.cp }), /content-length/);
    assert.equal(f.st.commits[0].outcome, "edge_error");
  } finally {
    f.close();
  }
});

test("CODEC_TAMPER is a lab switch: refused when ENVIRONMENT is production", () => {
  assert.equal(codecTamper({}), "");
  assert.equal(codecTamper({ CODEC_TAMPER: "etag" }), "etag");
  assert.equal(codecTamper({ CODEC_TAMPER: "key", ENVIRONMENT: "lab" }), "key");
  assert.throws(() => codecTamper({ CODEC_TAMPER: "etag", ENVIRONMENT: "production" }), /production/);
  assert.throws(() => codecTamper({ CODEC_TAMPER: "other" }), /CODEC_TAMPER/);
});

// RP70-7: what the commit answers reaches the client as the S3 answer it is.
test("a commit relayed as 400 reaches the client as 400 with its XML", { skip: noWasm }, async () => {
  const deps = await codecDeps();
  const xml = '<?xml version="1.0" encoding="UTF-8"?>\n<Error><Code>InvalidRequest</Code><Message>a stored chunk this admit never claimed</Message></Error>';
  const f = await fakes({ commit: (b) => (b.outcome === "ok" ? { status: 200, json: { status: 400, headers: { "Content-Type": "application/xml" }, body: xml } } : { status: 200, json: { status: 503 } }) });
  try {
    const body = deps.chunker.goldenBody(4626, 2 << 20).value;
    const relay = await runCodec(bodyRequest(body), admitFor(deps.e2ee), {}, {}, { ...deps, controlPlane: f.cp });
    assert.equal(relay.status, 400);
    assert.equal(relay.body, xml);
    assert.equal(f.st.commits.filter((c) => c.outcome === "ok").length, 1, "a relayed answer is never retried");
  } finally {
    f.close();
  }
});

test("a control plane answering 503 twice is the Worker's own 503 (null)", { skip: noWasm }, async () => {
  const deps = await codecDeps();
  const f = await fakes({ commit: () => ({ status: 503, json: { error: "no_database" } }) });
  try {
    const body = deps.chunker.goldenBody(4627, 1 << 20).value;
    const relay = await runCodec(bodyRequest(body), admitFor(deps.e2ee), {}, {}, { ...deps, controlPlane: f.cp });
    assert.equal(relay, null);
    assert.equal(f.st.commits.length, 2, "one retry of a lost answer");
  } finally {
    f.close();
  }
});

// RP72-3: a 401/403 from the control plane is the SERVICE channel's (the
// token, the peer rule) — transient to the client, never "retrying unchanged
// will fail the same way": the Worker's 503 SlowDown (runCodec answers null).
for (const status of [401, 403]) {
  test(`a control plane's ${status} at commit is the Worker's 503, not the client's 400`, { skip: noWasm }, async () => {
    const deps = await codecDeps();
    const f = await fakes({ commit: () => ({ status, json: { error: "forbidden" } }) });
    try {
      const body = deps.chunker.goldenBody(4629, 1 << 20).value;
      const req = bodyRequest(body);
      req.url = "https://s3.stored.cloud/b/k.bin";
      const relay = await runCodec(req, admitFor(deps.e2ee), {}, {}, { ...deps, controlPlane: f.cp, requestId: "req-8" });
      assert.equal(relay, null, "no client answer: the caller's 503 SlowDown");
    } finally {
      f.close();
    }
  });
}

test("a control plane's own 4xx is the client's 400 InvalidRequest with its error, never retried", { skip: noWasm }, async () => {
  const deps = await codecDeps();
  const f = await fakes({ commit: () => ({ status: 400, json: { error: "invalid_json" } }) });
  try {
    const body = deps.chunker.goldenBody(4628, 1 << 20).value;
    const req = bodyRequest(body);
    req.url = "https://s3.stored.cloud/b/k.bin";
    const relay = await runCodec(req, admitFor(deps.e2ee), {}, {}, { ...deps, controlPlane: f.cp, requestId: "req-7" });
    assert.ok(relay, "an answer, not the Worker's 503");
    assert.equal(relay.status, 400);
    assert.match(relay.body, /<Code>InvalidRequest<\/Code>/);
    assert.match(relay.body, /invalid_json/);
    assert.match(relay.body, /<Resource>\/b\/k.bin<\/Resource>/);
    assert.equal(f.st.commits.length, 1, "not retried");
  } finally {
    f.close();
  }
});

// RP72-6: ONE isolate-wide byte budget (96 MiB) for the codec's chunk
// plaintext and the windowed reader's ranges: a codec write's bytes are
// counted against the window's (and the reverse), so a max-size codec write
// and a full window can never meet in one isolate at 144 MiB. A child
// semaphore (the write's own 48 MiB) takes from its parent too.
test("a child semaphore takes from its parent: one budget for the isolate", async () => {
  const parent = new Semaphore(10);
  const child = new Semaphore(8, parent);
  await child.acquire(6);
  assert.equal(parent.held, 6, "the child's bytes are the parent's");
  assert.equal(child.wouldAdmit(5), false, "the parent is the limit (4 left)");
  let got = false;
  const p = child.acquire(4).then(() => (got = true));
  await new Promise((r) => setTimeout(r, 5));
  assert.equal(got, false, "waits on its own bound (6 + 4 > 8)");
  child.release(6);
  await p;
  assert.equal(parent.held, 4);
  child.release(4);
  assert.equal(parent.held, 0);
  assert.equal(ISOLATE_BYTES, 96 << 20, "the window's full budget, shared");
});

test("a codec write waits on the isolate budget a window holds", { skip: noWasm }, async () => {
  const restore = setIsolateBudget(4 << 20);
  const deps = await codecDeps();
  const f = await fakes();
  try {
    const iso = isolateBudget();
    await iso.acquire(4 << 20); // a windowed read holds the isolate's budget
    const body = deps.chunker.goldenBody(4630, 3 << 20).value;
    let done = false;
    const run = runCodec(bodyRequest(body), admitFor(deps.e2ee), {}, {}, { ...deps, controlPlane: f.cp, requestId: "req-9" }).then((r) => {
      done = true;
      return r;
    });
    await new Promise((r) => setTimeout(r, 200));
    assert.equal(done, false, "the codec waits: the isolate's bytes are held");
    assert.equal(f.st.puts.length, 0, "no chunk stored while the budget is held");
    iso.release(4 << 20);
    const relay = await run;
    assert.equal(relay.status, 200);
    assert.equal(iso.held, 0, "everything given back");
  } finally {
    f.close();
    restore();
  }
});
