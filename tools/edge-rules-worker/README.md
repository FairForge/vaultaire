# edge-rules-worker — programmable buckets without Workers for Platforms (bench, 2026-10-04)

Not part of the product. A Cloudflare Worker on `rules.stored.ge` that lets a
customer attach **declarative rules** to a bucket and have the platform's own
Worker apply them on every object event — a webhook, a copy-to prefix, a tag,
a size cap and a content-type allow-list. Customers never run code, so this
fits the $5 Workers Paid plan; the day a customer needs arbitrary code is the
day to buy Workers for Platforms and route the same event to their dispatch
namespace instead of this evaluator (Phase 31 / 42).

Rules live in R2 under `rules/<bucket>.json`:

```json
{"rules":[
  {"id":"images","match":{"prefix":"img/","content_type":"image/"},
   "actions":[{"webhook":"https://…"},{"copy_to":"derived/"},{"tag":"needs-thumb"},{"reject_over_bytes":5242880}]},
  {"id":"no-exe","match":{"prefix":""},"actions":[{"allow_content_types":["image/","text/"]}]}
]}
```

Routes: `PUT /rules/<bucket>` (admin header), `PUT /ingest/<bucket>/<key>`
(stores into the bound R2 bucket, applies rules; rejections happen before a
byte is stored), `GET /get/<key>`, `GET /list/<prefix>`, `POST /hook` (a demo
webhook sink). Measured: 1 MB ingest with webhook + copy + tag applied in
450–600 ms at the Dallas edge; size and content-type rejections in <100 ms.
A Worker cannot deliver a webhook to its own hostname (Cloudflare refuses the
self-subrequest), so the demo delivers to the other edge Worker.

Set `EDGE_ADMIN_KEY` as a `[vars]` entry (or a secret) before deploying.
