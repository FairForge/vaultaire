// vt-rules: "programmable buckets" without Workers for Platforms. Customers do
// not run code; they attach DECLARATIVE rules to a bucket (JSON, stored in R2
// under rules/<bucket>.json) and the platform's own Worker evaluates them on
// every object event. The customer gets a webhook, a copy-to, a tag, a size
// cap and a content-type allow-list — the bulk of what a "run my Worker on my
// bucket" request actually wants — inside the $5 plan. When a customer needs
// arbitrary code, that is the day to buy Workers for Platforms and route the
// event to their dispatch namespace instead of this evaluator.
//
// Rule shape:
//   {"rules":[{"id":"thumbs","match":{"prefix":"img/","content_type":"image/","max_bytes":10485760},
//              "actions":[{"webhook":"https://…"},{"copy_to":"derived/"},{"tag":"needs-thumb"},{"reject_over_bytes":52428800}]}]}
export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);
    const t0 = Date.now();
    const admin = request.headers.get("x-edge-admin") === env.EDGE_ADMIN_KEY;
    if (url.pathname.startsWith("/rules/") && request.method === "PUT") {
      if (!admin) return new Response("forbidden", { status: 403 });
      const bucket = url.pathname.slice(7);
      const body = await request.text();
      try { JSON.parse(body); } catch { return new Response("rules must be JSON", { status: 400 }); }
      await env.R2.put("rules/" + bucket + ".json", body, { httpMetadata: { contentType: "application/json" } });
      return new Response(JSON.stringify({ bucket, stored: true }), { headers: { "content-type": "application/json" } });
    }
    const m = url.pathname.match(/^\/ingest\/([a-z0-9-]+)\/(.+)$/);
    if (m && (request.method === "PUT" || request.method === "POST")) {
      const [, bucket, key] = m;
      const ctype = request.headers.get("content-type") || "application/octet-stream";
      const len = Number(request.headers.get("content-length") || 0);
      const rulesObj = await env.R2.get("rules/" + bucket + ".json");
      const rules = rulesObj ? (await rulesObj.json()).rules || [] : [];
      // pre-ingest rules: reject before storing a byte
      for (const r of rules) {
        if (!matches(r.match, key, ctype, len)) continue;
        for (const a of r.actions || []) {
          if (a.reject_over_bytes && len > a.reject_over_bytes) return new Response(JSON.stringify({ rejected_by: r.id, reason: "size", bytes: len }), { status: 413, headers: { "content-type": "application/json" } });
          if (a.allow_content_types && !a.allow_content_types.some(p => ctype.startsWith(p))) return new Response(JSON.stringify({ rejected_by: r.id, reason: "content-type", content_type: ctype }), { status: 415, headers: { "content-type": "application/json" } });
        }
      }
      const obj = await env.R2.put(bucket + "/" + key, request.body, { httpMetadata: { contentType: ctype } });
      const applied = [];
      const work = [];
      for (const r of rules) {
        if (!matches(r.match, key, ctype, obj.size)) continue;
        for (const a of r.actions || []) {
          if (a.webhook) { applied.push(r.id + ":webhook"); work.push(fetch(a.webhook, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ event: "object.created", bucket, key, size: obj.size, content_type: ctype, etag: obj.httpEtag, rule: r.id }) }).catch(() => {})); }
          if (a.copy_to) { applied.push(r.id + ":copy_to"); work.push(env.R2.get(bucket + "/" + key).then(o => o && env.R2.put(bucket + "/" + a.copy_to + key.split("/").pop(), o.body, { httpMetadata: o.httpMetadata })).catch(() => {})); }
          if (a.tag) { applied.push(r.id + ":tag"); work.push(env.R2.put(bucket + "/.tags/" + key, a.tag).catch(() => {})); }
        }
      }
      ctx.waitUntil(Promise.all(work));
      return new Response(JSON.stringify({ bucket, key, size: obj.size, applied, ms: Date.now() - t0, colo: request.cf && request.cf.colo }), { headers: { "content-type": "application/json" } });
    }
    if (url.pathname === "/hook" && request.method === "POST") { // a webhook target for the demo
      const b = await request.text(); await env.R2.put("hooks/" + Date.now() + ".json", b); return new Response("ok");
    }
    if (url.pathname.startsWith("/get/")) { const o = await env.R2.get(url.pathname.slice(5)); return o ? new Response(o.body, { headers: { "content-type": o.httpMetadata && o.httpMetadata.contentType || "application/octet-stream" } }) : new Response("not found", { status: 404 }); }
    if (url.pathname.startsWith("/list/")) { const l = await env.R2.list({ prefix: url.pathname.slice(6) }); return new Response(JSON.stringify(l.objects.map(o => [o.key, o.size])), { headers: { "content-type": "application/json" } }); }
    return new Response("vt-rules: PUT /rules/<bucket> (admin), PUT /ingest/<bucket>/<key>, GET /get/<key>, GET /list/<prefix>", { status: 404 });
  },
};
function matches(mt, key, ctype, len) {
  if (!mt) return true;
  if (mt.prefix && !key.startsWith(mt.prefix)) return false;
  if (mt.suffix && !key.endsWith(mt.suffix)) return false;
  if (mt.content_type && !ctype.startsWith(mt.content_type)) return false;
  if (mt.max_bytes && len > mt.max_bytes) return false;
  return true;
}
