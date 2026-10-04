// vt-edge-ec: k-of-n erasure reassembly at the edge. Shards live on four
// vendors (presigned URLs in a manifest) and in R2 (binding). Reed-Solomon
// decodes stripe by stripe as the k winning streams arrive (memory ~ (n+k)
// stripes, so any object size fits the 128 MB isolate); RaptorQ needs the
// whole block, so it runs only when k shards fit in memory.
import wasmModule from "./eccodec.wasm";

const STRIPE = 1 << 20; // 1 MiB per shard per stripe

let wasmInstance;
async function wasm() {
  if (!wasmInstance) wasmInstance = await WebAssembly.instantiate(wasmModule, {});
  return wasmInstance.exports;
}

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);
    const t0 = Date.now();
    if (url.pathname === "/ping") return new Response("pong " + t0);
    if (url.pathname === "/echo" && request.method === "POST") {
      // upload-cap probe: count body bytes as they arrive at the edge
      let n = 0; const r = request.body.getReader();
      for (;;) { const { done, value } = await r.read(); if (done) break; n += value.byteLength; }
      return new Response(JSON.stringify({ bytes: n, ms: Date.now() - t0 }), { headers: { "content-type": "application/json" } });
    }
    if (url.pathname === "/relay" && request.method === "POST") {
      // upload-cap probe: relay the body from the edge to a presigned PUT on the
      // origin, so the Cloudflare -> origin leg is timed on its own.
      const to = url.searchParams.get("to");
      const len = request.headers.get("content-length");
      const t1 = Date.now();
      const r = await fetch(to, { method: "PUT", body: request.body, headers: { "content-length": len } });
      return new Response(JSON.stringify({ origin_status: r.status, relay_ms: Date.now() - t1, bytes: Number(len), colo: request.cf && request.cf.colo }), { headers: { "content-type": "application/json" } });
    }
    if (url.pathname === "/relaygen") {
      // upload-cap probe without a client leg: the edge makes the bytes and PUTs them.
      const mb = Number(url.searchParams.get("mb") || "16");
      const to = url.searchParams.get("to");
      const body = new Uint8Array(mb << 20); for (let i = 0; i < body.length; i += 4096) body[i] = i & 255;
      const t1 = Date.now();
      const r = await fetch(to, { method: "PUT", body, headers: { "content-length": String(body.length) } });
      const ms = Date.now() - t1;
      return new Response(JSON.stringify({ origin_status: r.status, put_ms: ms, mb, mbps: Math.round(mb * 1048576 / 1048.576 / ms) / 1000 * 1000, colo: request.cf && request.cf.colo }), { headers: { "content-type": "application/json" } });
    }
    if (url.pathname.startsWith("/up/") && (request.method === "PUT" || request.method === "POST")) {
      // direct-to-R2 through the edge: the body streams into the bound bucket, never touching the origin
      const key = url.pathname.slice(4);
      const t1 = Date.now();
      const o = await env.R2.put(key, request.body, { httpMetadata: { contentType: request.headers.get("content-type") || "application/octet-stream" } });
      return new Response(JSON.stringify({ key, size: o.size, etag: o.httpEtag, ms: Date.now() - t1, colo: request.cf && request.cf.colo }), { headers: { "content-type": "application/json" } });
    }
    if (url.pathname.startsWith("/del/") && request.method === "DELETE" && request.headers.get("x-edge-admin") === env.EDGE_ADMIN_KEY) {
      await env.R2.delete(url.pathname.slice(5)); return new Response("deleted " + url.pathname.slice(5));
    }
    if (url.pathname === "/pget") {
      // Parallel-range streaming through the edge: a slow origin (tape's landing
      // zone serves one stream at ~5 MB/s) is read as `parts` concurrent ranges,
      // re-ordered and streamed; optional edge cache of the whole object.
      const to = url.searchParams.get("to"); const parts = Math.min(16, Number(url.searchParams.get("parts") || 8));
      const useCache = url.searchParams.get("cache") === "1";
      const cacheKey = new Request(url.origin + "/pget-cache/" + encodeURIComponent(to.split("?")[0]), { method: "GET" });
      if (useCache) { const hit = await caches.default.match(cacheKey); if (hit) { const h = new Headers(hit.headers); h.set("x-edge-cache", "HIT"); return new Response(hit.body, { headers: h }); } }
      // size from a 1-byte range (a GET-presigned URL cannot be HEADed: the method is signed)
      const probe = await fetch(to, { headers: { range: "bytes=0-0" } });
      const cr = probe.headers.get("content-range") || "";
      const size = Number((cr.split("/")[1]) || probe.headers.get("content-length") || 0);
      try { await probe.body.cancel(); } catch {}
      const head = probe;
      if (!(probe.status === 206 || probe.status === 200) || !size) return new Response("probe failed " + probe.status + " " + cr, { status: 502 });
      const piece = Math.ceil(size / parts);
      const t1 = Date.now();
      // Workers allow six subrequests waiting on headers at once: keep a
      // sliding window of WINDOW ranges in flight, start the next as one drains.
      const WINDOW = 6;
      const ranges = []; for (let i = 0; i < parts; i++) { const lo = i * piece, hi = Math.min(size, lo + piece) - 1; if (lo > hi) break; ranges.push([lo, hi]); }
      const inflight = new Map(); const start = (i) => { if (i < ranges.length && !inflight.has(i)) inflight.set(i, fetch(to, { headers: { range: `bytes=${ranges[i][0]}-${ranges[i][1]}` } })); };
      for (let i = 0; i < Math.min(WINDOW, ranges.length); i++) start(i);
      let idx = 0;
      const body = new ReadableStream({
        async pull(controller) {
          if (idx >= ranges.length) { controller.close(); return; }
          const r = await inflight.get(idx); inflight.delete(idx);
          if (r.status !== 206 && r.status !== 200) { controller.error(new Error("range " + idx + " " + r.status)); return; }
          start(idx + WINDOW); idx++;
          const reader = r.body.getReader(); for (;;) { const { done, value } = await reader.read(); if (done) break; controller.enqueue(value); }
        },
      });
      const fetches = ranges;
      const headers = { "content-type": head.headers.get("content-type") || "application/octet-stream", "content-length": String(size), "x-edge-parts": String(fetches.length), "x-edge-head-ms": String(t1 - t0) };
      const resp = new Response(body, { headers });
      if (useCache) ctx.waitUntil(caches.default.put(cacheKey, resp.clone()));
      return resp;
    }
    if (url.pathname.startsWith("/r2/")) {
      const o = await env.R2.get(url.pathname.slice(4));
      if (!o) return new Response("not found", { status: 404 });
      return new Response(o.body, { headers: { "x-edge-ms": String(Date.now() - t0) } });
    }
    const m = url.pathname.match(/^\/ec\/([a-z0-9]+)$/);
    if (!m) return new Response("usage: /ec/<manifest>?mode=whole|stripe&cache=1&lose=vendor (stripe assumes an interleaved shard layout; erasure-bench shards are contiguous → use whole)", { status: 404 });

    const useCache = url.searchParams.get("cache") === "1";
    const cache = caches.default;
    const cacheKey = new Request(url.origin + url.pathname + "?v=1", { method: "GET" });
    if (useCache) {
      const hit = await cache.match(cacheKey);
      if (hit) { const h = new Headers(hit.headers); h.set("x-edge-cache", "HIT"); h.set("x-edge-ms", String(Date.now() - t0)); return new Response(hit.body, { headers: h }); }
    }

    const manObj = await env.R2.get("manifests/" + m[1] + ".json");
    if (!manObj) return new Response("no manifest", { status: 404 });
    const man = await manObj.json();
    const lose = url.searchParams.get("lose"); // drop every shard of this vendor
    const drop = new Set((url.searchParams.get("drop") || "").split(",").filter(Boolean).map(Number)); // drop these slots
    // A manifest written with an interleaved layout carries its stripe size;
    // stripe mode is then the right (and bounded-memory) decode for any size.
    const STRIPE_EFF = man.stripe || STRIPE;
    const mode = url.searchParams.get("mode") || (man.stripe ? "stripe" : "whole");
    const n = man.k + man.m;

    // Start every shard fetch; keep the first k whose headers arrive.
    const ctrls = man.shards.map(() => new AbortController());
    const starts = man.shards.map((s, i) => {
      if ((lose && s.vendor === lose) || drop.has(i)) return Promise.reject(new Error("lost " + s.vendor));
      if (s.r2key) return env.R2.get(s.r2key).then(o => { if (!o) throw new Error("r2 miss"); return { i, body: o.body, vendor: s.vendor }; });
      return fetch(s.url, { signal: ctrls[i].signal }).then(r => { if (!r.ok) throw new Error(s.vendor + " " + r.status); return { i, body: r.body, vendor: s.vendor }; });
    });
    const winners = [];
    await new Promise((resolve, reject) => {
      let failed = 0;
      starts.forEach(p => p.then(w => { if (winners.length < man.k) { winners.push(w); if (winners.length === man.k) resolve(); } else { ctrls[w.i].abort(); w.body.cancel().catch(() => {}); } },
        () => { failed++; if (failed > man.m) reject(new Error("too many shards failed")); }));
    }).catch(e => { return e; });
    if (winners.length < man.k) return new Response("not enough shards: " + winners.length, { status: 502 });
    starts.forEach((p, i) => { if (!winners.find(w => w.i === i)) ctrls[i].abort(); });
    const tFirstK = Date.now() - t0;
    const present = winners.reduce((acc, w) => acc | (1 << w.i), 0);
    const names = winners.map(w => w.vendor + ":" + w.i).join(",");
    const ex = await wasm();
    const mem = () => new Uint8Array(ex.memory.buffer);

    const headers = {
      "content-type": "application/octet-stream",
      "content-length": String(man.size),
      "x-edge-codec": man.codec, "x-edge-mode": mode, "x-edge-winners": names,
      "x-edge-first-k-ms": String(tFirstK),
    };

    let decodeMs = 0;
    if (mode === "whole") {
      // Pull the k shards fully into wasm memory, decode once.
      const buf = ex.ec_alloc(n * man.shard_len);
      const out = man.codec === "raptorq" ? ex.ec_alloc(man.size) : ex.ec_alloc(8);
      await Promise.all(winners.map(async w => {
        const r = w.body.getReader(); let off = w.i * man.shard_len;
        for (;;) { const { done, value } = await r.read(); if (done) break; mem().set(value, buf + off); off += value.byteLength; }
      }));
      // Date.now() is frozen during CPU work in Workers, so decode time is
      // measured by the caller (TTFB minus first-k), not here.
      let rc;
      if (man.codec === "raptorq") {
        rc = ex.rq_decode(man.k, man.m, man.shard_len, man.symbol, buf, present, out, man.size);
      } else {
        // RS data layout is contiguous (klauspost Split): rebuild missing data
        // slots in place and stream slots 0..k straight out of wasm memory —
        // no second copy of the payload, so a 64 MiB object fits the isolate.
        rc = ex.rs_reconstruct(man.k, man.m, man.shard_len, buf, present);
      }
      if (rc !== 0) return new Response("decode failed rc=" + rc, { status: 500 });
      headers["x-edge-fetched-ms"] = String(Date.now() - t0);
      const srcPtr = man.codec === "raptorq" ? out : buf;
      let off = 0;
      const CH = 4 << 20;
      const body = new ReadableStream({
        pull(controller) {
          if (off >= man.size) { controller.close(); ex.ec_free(buf, n * man.shard_len); ex.ec_free(out, man.codec === "raptorq" ? man.size : 8); return; }
          const len = Math.min(CH, man.size - off);
          controller.enqueue(mem().slice(srcPtr + off, srcPtr + off + len));
          off += len;
        },
      });
      const resp = new Response(body, { headers });
      if (useCache) ctx.waitUntil(cache.put(cacheKey, resp.clone()));
      return resp;
    }

    // Stripe mode (RS): read the k streams in lockstep, decode STRIPE_EFF bytes at a
    // time. Each reader keeps a queue of chunks and copies bytes ONCE, straight
    // into wasm memory — the first version re-concatenated Uint8Arrays per
    // chunk, which burned the isolate's CPU budget on a 512 MiB object.
    const readers = winners.map(w => ({ i: w.i, r: w.body.getReader(), q: [], qlen: 0, off: 0, done: false }));
    const buf = ex.ec_alloc(n * STRIPE_EFF);
    const out = ex.ec_alloc(man.k * STRIPE_EFF);
    let produced = 0;
    const fill = async (rd, want) => {
      while (rd.qlen < want && !rd.done) {
        const { done, value } = await rd.r.read();
        if (done) { rd.done = true; break; }
        rd.q.push(value); rd.qlen += value.byteLength;
      }
    };
    // take copies `len` bytes from the reader's queue into wasm memory at dst
    const take = (rd, len, dst) => {
      const m8 = mem(); let copied = 0;
      while (copied < len) {
        const head = rd.q[0]; const avail = head.byteLength - rd.off; const nb = Math.min(avail, len - copied);
        m8.set(head.subarray(rd.off, rd.off + nb), dst + copied);
        copied += nb; rd.off += nb; rd.qlen -= nb;
        if (rd.off === head.byteLength) { rd.q.shift(); rd.off = 0; }
      }
    };
    const stream = new ReadableStream({
      async pull(controller) {
        if (produced >= man.size) { controller.close(); ex.ec_free(buf, n * STRIPE_EFF); ex.ec_free(out, man.k * STRIPE_EFF); return; }
        const want = Math.min(STRIPE_EFF, man.shard_len - Math.floor(produced / man.k));
        await Promise.all(readers.map(rd => fill(rd, want)));
        const L = Math.min(want, ...readers.map(rd => rd.qlen));
        if (L === 0) { controller.error(new Error("short shard")); return; }
        for (const rd of readers) take(rd, L, buf + rd.i * STRIPE_EFF);
        const outLen = Math.min(man.k * L, man.size - produced);
        const rc = ex.rs_decode(man.k, man.m, L, buf, present, out, outLen);
        if (rc !== 0) { controller.error(new Error("decode rc=" + rc)); return; }
        controller.enqueue(mem().slice(out, out + outLen));
        produced += outLen;
      },
    });
    const resp = new Response(stream, { headers });
    if (useCache) ctx.waitUntil(cache.put(cacheKey, resp.clone()));
    return resp;
  },
};
