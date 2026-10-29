// The mirror-settle lab's Worker (RP78-2): putTwo — the Worker's own module,
// src/codec.js, copied unchanged into build/ by run.sh — in workerd, a generated body of ?n bytes sent
// to the primary sink and to ?mirror (slow | stall), the client answered at
// the primary's answer, the mirror settled under ctx.waitUntil exactly as
// src/index.js does (settleMirror, the control plane being the sink).
import { putTwo } from "./codec.js";
import { settleMirror } from "./index.js";

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);
    const n = Number(url.searchParams.get("n") || 64 << 20);
    const mirror = url.searchParams.get("mirror") || "slow";
    const piece = new Uint8Array(1 << 20).fill(7);
    let sent = 0;
    const body = new ReadableStream({
      pull(c) {
        if (sent >= n) { c.close(); return; }
        const k = Math.min(piece.length, n - sent);
        c.enqueue(piece.slice(0, k));
        sent += k;
      },
    }).pipeThrough(new FixedLengthStream(n)); // a client body keeps its length, as request.body does
    const t0 = Date.now();
    const sink = env.SINK;
    const two = await putTwo(fetch, [`${sink}/primary/k`, `${sink}/${mirror}/k`],
      { method: "PUT", headers: { "content-length": String(n) }, body }, n);
    const answeredMs = Date.now() - t0;
    if (!two.mirrorUnbudgeted) ctx.waitUntil(settleMirror({ VAULTAIRE_ORIGIN: sink, VAULTAIRE_EDGE_TOKEN: "lab" }, "lab-admit", two, "lab"));
    return Response.json({ status: two.resp.status, answered_ms: answeredMs, unbudgeted: two.mirrorUnbudgeted });
  },
};
