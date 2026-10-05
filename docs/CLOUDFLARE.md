# Cloudflare — operator page (state as of 2026-10-04)

The measurements behind every line are in `bench-results/VAULTAIRE-PATH-2026-10-04.md`.

## Accounts and zones

| | Account | Holds |
|---|---|---|
| **Production** | `cloudflare@fairforge.io` (account `66866bb1…`) | zones **stored.ge** (`15e9c392…`) and **stored.cloud** (`85615649…`), both Free plan; Workers Paid ($5/mo) since 2026-10-04; R2 (activated 2026-10-04, bucket `vt-edge`); Workers `vt-edge` → `edge.stored.ge`, `vt-rules` → `rules.stored.ge`; Zero Trust team `bold-recipe-3e65.cloudflareaccess.com` with one test Access app |
| **Bench** | `isaacv17@gmail.com` (account `71a9e011…`) | no zones; Workers Paid; R2 buckets `vaultaire-public` (the prod `r2` driver's bucket — **prod public objects live in this account**), `vt-ecbench`, `vt-sippy-*`, `bench-*`; Workers `vt-edge-ec`, `vt-edge-ec-smart`, `cfbench`, … |

Tokens (local file `~/fairforge/.cloudflare-creds.env`, never in the repo): an **account token** for the bench account (R2 + Workers; it cannot see any zone — account tokens are bound to one account), and a **user token** `vaultaire-plan-driver-zone-2026-10-04` scoped to both zones (settings, cache, WAF, DNS, load balancers, Workers routes, Access apps) plus Workers scripts, R2 and rulesets on the production account. Neither login had a second factor on 2026-10-04.

## DNS (stored.ge)

`stored.ge`, `www`, `api` → `38.147.105.54` **proxied**; **`s3.stored.ge` → same IP, DNS-only** (the direct origin: no 100 MB body limit, no edge hop); `edge.stored.ge` and `rules.stored.ge` are Worker custom domains (`AAAA 100::`, proxied); mail at Outlook; stale `NS` records pointing at NS1 remain and should be deleted.

## Zone settings applied (2026-10-04)

| Setting | Value | Why |
|---|---|---|
| Smart Tiered Cache + smart topology | on | fewer origin misses on the public `/cdn` path; free |
| Minimum TLS | 1.2 (was 1.0) | |
| Early Hints | on | |
| **`origin_max_http_version`** | **1** | Cloudflare talks HTTP/1.1 to the origin. With HTTP/2, HAProxy 2.8's per-stream window capped uploads (1.2 MB/s at the 64 KB default; a large window starved multi-stream uploads instead). `docs/DEPLOY.md` has the HAProxy side (`tune.h2.initial-window-size 262144`) |
| Rate-limit rule | 30 req / 10 s per IP on `/auth/login`, `/auth/register`, `/api/waitlist`, `/dashboard/login`, block 10 s | the one rule the Free plan allows |
| HTTP/3, Brotli, HSTS, always-https, SSL strict | on (pre-existing) | |
| Cache Reserve, Argo, Load Balancing, WAF managed rules | not available on Free / unsubscribed | Cache Reserve waits for cacheable private reads (plan 40.1(b)); a load balancer waits for a second origin |

## Torn down 2026-10-04 night (nothing of the bench is exposed)

The Workers `vt-edge`, `vt-rules` (and their custom domains `edge.stored.ge`,
`rules.stored.ge`), the bench-account Workers `vt-edge-ec` / `vt-edge-ec-smart`,
the Access test app, and every test bucket (`vt-edge`, `vt-ecbench`,
`vt-sippy-*`, `vbench-user1-r2`) were deleted after the measurements; the
unauthenticated `/up`, `/pget`, `/relaygen` and `/ingest` endpoints they
carried no longer exist. The sources live in `tools/edge-ec-worker` and
`tools/edge-rules-worker`; redeploying is `wrangler deploy` with the production
account's token, after adding an auth check to any endpoint that writes or
fetches on behalf of a caller. The zone settings above stay. A zone token
with `Workers R2 Storage Write` doubles as R2 S3 credentials (access key =
token id, secret = SHA-256 of the token), which is how the production bucket
was emptied without a separate R2 key.

## What the edge did (as benchmarked; redeploy from `tools/`)

- **Public objects**: `/cdn/<slug>/<bucket>/<key>` through the proxy, `cache-control: public, max-age=14400`, MISS→HIT, first byte ~100 ms on a HIT, $0 egress.
- **Private reads**: `cache-control: private, no-cache` → BYPASS; the edge adds a hop (~80 ms). Use `s3.stored.ge` for bulk.
- **Uploads**: through `stored.ge` at the client's uplink (after the h2 fix); bodies over 100 MB must be multipart or go to `s3.stored.ge`.
- **Sippy**: an R2 bucket with `source.bucketUrl = https://s3.stored.ge/<bucket>` and a tenant key fills from the origin on first read (works on the archive tier too; needs ETag on 206, fixed in #578; objects >199 MiB are pulled in parts; a key that failed once keeps its failed state).
- **`edge.stored.ge`** (`tools/edge-ec-worker`): `/ec/<manifest>` k-of-n reassembly (whole or stripe mode), `/pget?parts=N&to=<presigned>` parallel-range streaming of a slow origin (six-wide window), `/up/<key>` → R2, `/del/<key>` (admin header), `/relaygen`, `/echo`.
- **`rules.stored.ge`** (`tools/edge-rules-worker`): declarative per-bucket rules on ingest (webhook, copy-to, tag, size cap, content-type allow-list). Needs no Workers for Platforms.
- **Access**: `edge.stored.ge/admin-gate-test` is gated to the owner e-mails as the proof; gating `stored.ge/admin` is one API call.

## Rules learned

A Worker cannot deliver a webhook to its own hostname. `Date.now()` is frozen during CPU work. Six subrequests may wait on headers at once. The 128 MB isolate bounds whole-block decodes (~64 MiB RS); stripe-interleaved shards decode at any size. A GET-presigned URL cannot be HEADed (probe with `bytes=0-0`). Account tokens never see zones. Workers for Platforms ($25/mo) adds customer-run Workers on top of Workers Paid; it does not raise your own Workers' limits.

## Findings 2026-10-05 (plan-driver session: three doc researchers + live tests on the prod origin)

Tests ran against `s3.stored.ge` with the bench tenant (bucket `cf-tests-20261005`, kept for the lifecycle check) and an R2 bucket `vt-sippy-range` on the fairforge account (Sippy configured, lifecycle `expire-1d` set ~16:00 UTC; delete both after the expiry check).

- **Sippy** (official for any S3-compatible source since 2026-07-24, free beyond R2 ops; one `source` per bucket, no prefix): a ranged miss on a 20 MiB object serves the range from the origin (1.2 s) and copies the whole object into R2 in the background; a ranged miss on a 300 MiB object serves the range and the copy completes within ~5 min; delete in R2 → re-pull on the next read; **an overwrite on the origin is never seen** (v1 served after v2 was PUT — "never re-retrieved" per the docs); the R2 copy carries a multipart ETag, not the origin's MD5; a key that once failed stays failed (re-upload under a new key). **Every chunked object (>64 MiB) failed** with "upstream ETag changed during read": the chunked handler's 206 had no ETag/Last-Modified — fixed #585 (red-first test, shared identity block), re-proven: 300 MiB pulled byte-exact. R2 allows 1,000,000 buckets per account.
- **Workers Cache** (launched 2026-07-06, any Workers plan, no storage fee, 512 MB per object during rollout, tiered, request-collapsing): proven on a throwaway `wctest.stored.ge` — the cache key includes `ctx.props`, so an auth gateway that passes `{userId}` gets a partition per user (user A HIT, user B MISS, bodies differ); purge by tag works **only from the cached entrypoint** (`ctx.cache.purge` is scoped to the caller; expose an RPC on the cached class); **Range is not sliced when a gateway fronts the cached entrypoint** (the gateway receives the full 200 — slice there). This replaces Cache Reserve for private reads. Worker and custom domain deleted.
- **Admission on Vaultaire:** a presigned PUT whose `X-Amz-SignedHeaders` includes `content-length` is refused (403) for any other body size, accepted at the signed size; an unsigned length accepts any size. R2 does not document the same guarantee → R2-direct uploads stay public-bucket only, with the drain.
- **Cache Reserve:** `GET /zones/{id}/cache/cache_reserve` answers 1135 "not available for your plan type" on the Free zone; the dashboard's "Activate" button is the Smart Shield upsell (Cache Reserve = Smart Shield Advanced, Enterprise). Plan 40.1(a) is struck. **Logpush** could not be checked: the zone token lacks the Logs permission.
- **Catalog deltas vs the plan:** Dynamic Workers is an OPEN beta on Workers Paid (1,000 unique/mo included, then $0.002 per Worker-day) — 40.5 says closed; Containers + Sandbox SDK GA 2026-04-13; Email Service beta (SMTP `smtp.mx.cloudflare.net:465`, 3,000/mo included, $0.35 per 1,000 after) is a candidate for prod's empty `EMAIL_PROVIDER`; Workers VPC + Hyperdrive to a private Postgres, free in beta; Logpush on all plans, usage-priced ($0.03/GB to R2); purge by tag/prefix on Free (5 calls/min); R2 Data Access Logs (GA 2026-09-04) are per-request but "best effort, may be omitted" — dashboards, not invoices; the Workers OAuth Provider v1 (2026-10-01) is OAuth 2.1 only, no OpenID Connect; the request-body limit is the ZONE plan (Free/Pro 100 MB, Business 200 MB, Enterprise to 5 GB self-serve); Workers for Platforms is $25/mo (20 M requests, 60 M CPU-ms, 1,000 scripts, per-script bindings, tags, outbound Worker) — only when customer-deployed Workers are sold.
- **Design verdict** for the "stored.cloud base layer" proposal (snapshelter session 2026-10-05): right shape; corrections — Vaultaire is the quota/ledger authority (admission at PUT + the signed Content-Length), the end user is the tenant and an app is a scoped key or STS token (42.2 already says so), private reads go through a Worker + Workers Cache, Sippy keeps the archive and app-owned buckets, R2-direct uploads drain to Vaultaire, deletes/overwrites reach the R2 copy through the webhook outbox (queue item 4, now on the edge plane's critical path). Plan edits proposed, not yet written: 40.1 strike (a); 40.5 Dynamic Workers open; 42.2 note; new 42.7 admission + ledger.
- **Still to test:** lifecycle expiry → re-pull; Sippy warm-up rate from a fast client and a 2 GiB object; Workers Cache against the real origin with the gateway slicing ranges and an object above 512 MB; a Sippy pull counted once in egress accounting; multipart with 64 MB parts through the proxied host; delete propagation once webhooks exist; a one-month Pro zone trial for the WAF HMAC + cache-rule read plane; R2 Data Access Logs loss rate; Workers VPC + Hyperdrive to the SLC Postgres; the CDN path's 206 identity headers (`range.go` `serveRange` sets none).

## Open

2FA on both logins and rotation of everything pasted on 2026-10-04; the `/admin` Access gate; R2 event notifications → Queue → `direct-uploads/complete` so direct uploads register themselves; Workflows around Vault restores; a load balancer once a second origin exists; Workers for Platforms when a customer Worker must run; the stale NS records.
