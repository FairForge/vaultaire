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

## Open

2FA on both logins and rotation of everything pasted on 2026-10-04; the `/admin` Access gate; R2 event notifications → Queue → `direct-uploads/complete` so direct uploads register themselves; Workflows around Vault restores; a load balancer once a second origin exists; Workers for Platforms when a customer Worker must run; the stale NS records.
