# Cloudflare — operator page (state as of 2026-10-04)

The measurements behind every line are in `bench-results/VAULTAIRE-PATH-2026-10-04.md`.

## Accounts and zones

| | Account | Holds |
|---|---|---|
| **Production** | `cloudflare@fairforge.io` (account `66866bb1…`) | zones **stored.ge** (`15e9c392…`) and **stored.cloud** (`85615649…`), both Free plan; Workers Paid ($5/mo) since 2026-10-04; R2 (activated 2026-10-04, bucket `vt-edge`); Workers `vt-edge` → `edge.stored.ge`, `vt-rules` → `rules.stored.ge`; Zero Trust team `bold-recipe-3e65.cloudflareaccess.com` with one test Access app |
| **Bench** | `isaacv17@gmail.com` (account `71a9e011…`) | no zones; Workers Paid; R2 buckets `vaultaire-public` (the prod `r2` driver's bucket — **prod public objects live in this account**), `vt-ecbench`, `vt-sippy-*`, `bench-*`; Workers `vt-edge-ec`, `vt-edge-ec-smart`, `cfbench`, … |

Tokens (local file `~/fairforge/.cloudflare-creds.env`, never in the repo): an **account token** for the bench account (R2 + Workers; it cannot see any zone — account tokens are bound to one account), and a **user token** `vaultaire-plan-driver-zone-2026-10-04` scoped to both zones (settings, cache, WAF, DNS, load balancers, Workers routes, Access apps) plus Workers scripts, R2 and rulesets on the production account. Neither login had a second factor on 2026-10-04.

## DNS (stored.ge)

`stored.ge`, `www`, `api` → `38.147.105.54` **proxied**; **`s3.stored.ge` → same IP, DNS-only** (the direct origin: no 100 MB body limit, no edge hop); `edge.stored.ge` and `rules.stored.ge` were Worker custom domains and no longer resolve (NXDOMAIN, checked 2026-10-09 — torn down with their Workers, below); mail at Outlook; stale `NS` records pointing at NS1 remain and should be deleted.

## DNS (stored.cloud) — target state, WP-DOMAIN-1 (not applied as of 2026-10-12)

stored.cloud is the canonical domain (F-17). The zone (`85615649…`, same account) already serves the app on the apex (200 on `/health`). The owner click-list in the WP-DOMAIN-1 PR brings it to the stored.ge shape:

| Name | Target | Proxy | Why |
|---|---|---|---|
| `stored.cloud`, `www` | `38.147.105.54` | **proxied** | the web + dashboard; `www` 301s to the apex in Go (`VAULTAIRE_ALIAS_HOSTS`) |
| **`s3.stored.cloud`** | same IP | **DNS-only** (recommended, F-17b) | as `s3.stored.ge`: no 100 MB body limit, no edge hop, TLS ends at our origin (an SSE-C key is never plaintext inside the edge) |
| `cdn.stored.cloud` | same IP | proxied | public-read objects until the non-brand content domain exists (`CDN_PUBLIC_HOST`) |
| `app.`, `docs.`, `trust.`, `status.` | — | — | part 2 of WP-DOMAIN-1 |

Re-create on this zone the three rules the stored.ge zone carries (below): the sign-in rate limit, **"S3 API is never cached"**, and the HEAD cache rule; the zone settings (origin HTTP/1.1, TLS 1.2 minimum, Smart Tiered Cache) the same. DNSSEC: the DS record at Porkbun (pending since 2026-09-20). The origin certificate needs the `s3.stored.cloud` and `cdn.stored.cloud` SANs (certbot) and `TLS_CERT_PROBE_TARGETS` gains them. stored.ge is served forever: its zone stays as it is, the web 301s in Go, the S3 API answers there with no redirect.

## Zone settings applied (2026-10-04)

| Setting | Value | Why |
|---|---|---|
| Smart Tiered Cache + smart topology | on | fewer origin misses on the public `/cdn` path; free |
| Minimum TLS | 1.2 (was 1.0) | |
| Early Hints | on | |
| **`origin_max_http_version`** | **1** | Cloudflare talks HTTP/1.1 to the origin. With HTTP/2, HAProxy 2.8's per-stream window (the box's HAProxy until the 2026-10-10 upgrade to 3.2.9; not re-measured since) capped uploads (1.2 MB/s at the 64 KB default; a large window starved multi-stream uploads instead). `docs/DEPLOY.md` has the HAProxy side (`tune.h2.initial-window-size 262144`) |
| Rate-limit rule | 30 req / 10 s per IP on `/auth/login`, `/auth/register`, `/api/waitlist`, `/dashboard/login`, block 10 s | the one rule the Free plan allows |
| **Cache rule "S3 API is never cached"** (2026-10-05) | `set_cache_settings cache=false` for every path except `/cdn/`, `/static/`, `/dashboard/static/` | `.bin`, `.jpg`, `.zip`, `.pdf` … are on Cloudflare's default static-extension list, so the FIRST HEAD of a new key with such a name through `stored.ge` was turned into a GET to fill the edge cache; SigV4 covers the method → 403 `SignatureDoesNotMatch` (aws-cli's HEAD after every upload, rclone's existence check — the "HEAD→GET flake" of 2026-07, root-caused). Proven: 5/5 first HEADs of fresh `.jpg` keys arrive as HEAD after the rule; before it one in each series was a GET. Private reads already said `private, no-cache`, so nothing cacheable is lost |
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
- **Sippy**: an R2 bucket with `source.bucketUrl = https://s3.stored.ge/<bucket>` and a tenant key fills from the origin on first read (works on the archive tier too; needs ETag on 206, fixed in #578; objects >199 MiB are pulled in parts; a key that failed once was seen keeping its failed state on 2026-10-05; on 2026-10-09 failed pulls of unchanged objects resumed on the next view).
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
- **Still to test (evening results below):** lifecycle expiry → re-pull; Sippy warm-up rate from a fast client and a 2 GiB object; Workers Cache against the real origin with the gateway slicing ranges and an object above 512 MB; a Sippy pull counted once in egress accounting; multipart with 64 MB parts through the proxied host; delete propagation once webhooks exist; a one-month Pro zone trial for the WAF HMAC + cache-rule read plane; R2 Data Access Logs loss rate; Workers VPC + Hyperdrive to the SLC Postgres; the CDN path's 206 identity headers (`range.go` `serveRange` sets none).

## Findings 2026-10-05, evening (plan-driver session 2: the nine measurements)

Everything ran from the Salt Lake box against `s3.stored.ge` (bench tenant) and the fairforge account; every resource was `vt-*` and is gone (teardown at the end of this section). Build under test: `ec5fe6b` (after #588).

1. **Lifecycle expiry → re-pull: proven.** A Date-condition rule (`expire-now`, objects uploaded before 16:55 UTC) removed three of four objects from the listing 28 minutes later (the fourth was copied 3 s after the date — a Date rule is "uploaded before", not "on"). The next GET of an expired key re-pulled it from the origin (20 MiB: 200, 1.16 s, byte-exact; 3 bytes: 0.16 s) and the copy reappeared with a new `LastModified`. **The re-pull served the origin's current bytes**, so lifecycle expiry is the only way an overwrite reaches a Sippy copy — the window bounds staleness. Gotcha: a Date rule in the past re-expires the re-pulled copy at the next pass (`x-amz-expiration` named it at once) — delete the rule after use; `x-amz-expiration` reports the age rule even when a date rule will fire first. Both test buckets deleted.
2. **Sippy warm-up from the box.** Whole-object miss on 300 MiB: TTFB 0.72 s, 8.6 s total = **37 MB/s**, copy present immediately (its ETag is R2's own multipart ETag, `-2`); warm R2 → box 20–56 MB/s single stream. Ranged miss on 2 GiB: the 1 KiB range answers after 5.9–8.3 s — that is **iDrive's first read of a freshly completed multipart object (6.1 s raw, 0.6 s after)**, not Vaultaire — then R2 starts the background copy as **five parallel 200 MiB part GETs**, each signed with `if-match: "<etag>"` and sent without the header → `SignatureDoesNotMatch` on every part (tcpdump between HAProxy and Vaultaire; a Worker's own `fetch` delivers If-Match intact, so it is Sippy's client; the signed value recomputed with the bench key = the quoted ETag). **Fixed in #588** (the verifier offers the head row's ETag for an absent signed `if-match`; red-first tests; deployed 17:38 UTC). After the fix the parts are 206 — and **R2 abandons each part after ~29 s**: the parts stream at 5–7 MB/s each (≈ 33 MB/s aggregate, the same as a single whole-object stream; a Worker `fetch` of the same ranges gets 16–143 MB/s per stream and 116 MB/s single, so the cap is Sippy's puller, not the origin or HAProxy's h2), 150–200 MB of 200 arrive, R2 disconnects, nothing is retried, and **no copy lands — not after a ranged miss and not after a whole-object miss** (a fresh never-parted 2 GiB key: the viewer received all 2 GiB at 33 MB/s, md5 exact, listing still empty 6 minutes later). Each abandoned pull leaves an **in-progress multipart upload in R2** holding the finished parts (billed until aborted) — a bucket with Sippy needs `abortMultipartUploadsTransition` of 1 day. **Where the pull tops out:** 300 MiB (two parts) completes; the five-part shape (≥ ~1 GiB) never does at our 33 MB/s. A ranged miss above 199 MiB completes the copy only when every part fits in 29 s — never without a whole read of the object by R2. Sippy is for objects up to a few hundred MiB; larger media goes to R2 by a direct upload or a Worker multipart copy, never by a pull.
3. **Workers Cache against the real origin (`vt-gw` on `gw.stored.ge`, deleted):** gateway (uncached) authenticates `Authorization: Bearer <key>:<secret>` by one presigned `GET bytes=0-0` against the origin (**170–375 ms per fresh isolate, memoised 60 s** — never a HEAD: a Worker's HEAD on a presigned URL is sent as GET while the edge cache is cold, 403), calls the cached entrypoint with `props {keyId}`, which presigns and fetches the object whole and tags it `obj:<key>:<bucket>/<key>`. From the box: 20 MiB miss TTFB 0.59 s (auth 299 + inner 439 ms), **hit TTFB 0.20 s, 29 MB/s**, bytes identical; 300 MiB miss 15.3 s (20 MB/s), hit 4.4–5.6 s (**56–72 MB/s**), TTFB 0.16 s; **2 GiB = `BYPASS` every time** (above the 512 MB cap), streamed through the Worker at 50 MB/s with flat memory. From a home link (DFW): hit TTFB 0.41–0.46 s, of which 175–350 ms is the key verification in a fresh isolate every time — the per-isolate memo is worthless at the edge; the verdict must be cached (Workers Cache keyed on key id + sha256(secret), 60 s). **Range on a hit is sliced in the gateway and costs the read-through:** 1 KiB at offset 0 = 0.20 s, at 150 MiB of 300 = 2.2 s, at the tail = 7.1 s → cache fixed blocks (`?block=N`, one origin range each) before any seek-heavy workload. Purge by tag from the gateway through an RPC on the cached entrypoint: works. **Workers VPC + Hyperdrive to the SLC Postgres: refused** — it puts a credential that reads `api_keys` on the edge for a lookup the origin already answers at the same network cost; the cheap version is the cached verdict, and the right one is an introspection call (`GET /api/v1/manage/whoami` signed by the key → tenant, scope) so the partition is per tenant, not per key (plan 42.7).
4. **Egress accounting of a Sippy pull.** Bench tenant's live `egress_used` (`/api/v1/user/usage`, same number the admin page reads): three viewers of a fresh 50 MiB key through R2 — viewers 1 and 2 (3.8 s each, 4 s apart) were both served from the origin, viewer 3 from R2 (1.6 s); **delta = 104,857,600 bytes = 2 × the object**. One pull per *concurrent* miss, free once the copy exists — "counted once per expiry window" (plan 40.1, FAQ) is true after the first copy lands, not before. Note: the first key tried (`big300.bin`, a chunked object whose pull failed before #585) still answers `InternalError: Sippy upstream ETag changed during read` nine hours later while the origin HEADs 200 — a failed key is poisoned for at least a day.
5. **Multipart through the proxied host, 64 MB parts.** aws-cli 2.35 with `aws configure set default.s3.multipart_chunksize 64MB`, `multipart_threshold 64MB`, `max_concurrent_requests 8`: **1 GiB in 7.6 s = 135 MB/s through `stored.ge` from the box**; with `multipart_chunksize 128MB` → `UploadPart` **413 Content Too Large**. rclone 1.60 with `--s3-chunk-size 64M --s3-upload-cutoff 64M --s3-upload-concurrency 8`: 1 GiB in 12.5–12.8 s = **79–81 MB/s**; rclone's defaults (upload cutoff 200M) on a 150 MiB file → Cloudflare's 413 is an HTML page and rclone reports `expected element type <Error> but have <html>`. The docs sentence: parts ≤ 100 MB (64 MB recommended) through `stored.ge`, or use `s3.stored.ge`. Before the cache rule above, the first HEAD after each upload was a 403 (rclone "Attempt 1/3 failed … Forbidden", aws-cli HeadObject Forbidden) — fixed by the rule, re-proven 5/5.
6. **R2 Data Access Logs: not measurable from this session.** The toggle is dashboard-only (Bucket settings → Data Access Logs; no API route exists — `…/r2/buckets/<b>/{data-access-logs,logging,logs}` all 404), the logs land in Workers Observability (`r2` dataset, seven days, Observability pricing from 2026-12-01) and the docs say "asynchronous and best effort, events may be delayed or omitted" with status ≥ 400 excluded. The 1,000-read count needs a dashboard session: [YOU] or the next session with the browser recipe. Dashboards only, never invoices (plan 42.7).
7. **Email Service — LIVE 2026-10-12 20:01 UTC on `stored.cloud`** (owner's choice over stored.ge: both zones carry Outlook MX + SPF and no DMARC; the onboarding writes `_dmarc` with `p=reject`, which Outlook mail passes by SPF alignment, but the site's own domain stays untouched). Done through the dashboard with the `cloudflare@fairforge.io` login (the only route: no API for onboarding a sending domain or minting a user token): Compute → Email Service → Email Sending → Onboard Domain → `stored.cloud` (top level) → Activate — Cloudflare wrote `MX cf-bounce.stored.cloud → route1/2/3.mx.cloudflare.net`, `TXT cf-bounce.stored.cloud "v=spf1 include:_spf.mx.cloudflare.net ~all"`, `TXT cf-bounce._domainkey.stored.cloud "v=DKIM1; …"`, `TXT _dmarc.stored.cloud "v=DMARC1; p=reject;"` (all resolving within a minute); then Profile → API Tokens → Create Custom Token `vaultaire-email-sending-2026-10-12`, permission **Account · Email Sending · Edit**, all accounts, no TTL (`cfut_…`, 53 chars, `/user/tokens/verify` = active; lives only in the prod env as `SMTP_PASSWORD`, nowhere else). Prod env: `EMAIL_PROVIDER=smtp SMTP_HOST=smtp.mx.cloudflare.net SMTP_PORT=465 SMTP_USER=api_token EMAIL_FROM=noreply@stored.cloud` (the From domain must be the onboarded one for DKIM alignment), applied with `vaultaire-switch restart` (build 5632e3c live on :8000). **Proof:** `POST /auth/password-reset` for the owner's account answered 200 in 1.49 s (the SMTP round trip), the Email Sending overview showed Total sent 1 / Delivered 1 / quota 1 of 1,000 today, and `GET /reset-password` from the owner's inbox followed 52 s later. Sending quota is 1,000 a day on the beta; the plan's 3,000 a month included then $0.35 per 1,000. Still open on this login: **no 2FA** (the dashboard shows its two-factor interstitial on every sign-in) and the 2026-10-04 password rotation. The pre-live probe for the record: `smtp.mx.cloudflare.net:465` from the prod box: `220 … Cloudflare Email ESMTP Service ready`, `AUTH PLAIN LOGIN`, `SIZE 26214400`, `535 5.7.8 Authentication credentials invalid` with a dummy token — the transport and Vaultaire's `SMTPSender` (implicit TLS on 465, `SMTP_USER=api_token`, `SMTP_PASSWORD=<token>`, `EMAIL_PROVIDER=smtp`) are ready; the message itself needs two things only the dashboard gives: an API token with **Email Sending: Edit** (the zone token cannot even list sending domains — 10001) and **stored.ge onboarded under Compute → Email Service → Email Sending**, which writes the records itself (listed for the record): `MX cf-bounce.stored.ge → route1/2/3.mx.cloudflare.net`, `TXT cf-bounce.stored.ge "v=spf1 include:_spf.mx.cloudflare.net ~all"`, `TXT cf-bounce._domainkey.stored.ge "v=DKIM1; …"` (key from Cloudflare), `TXT _dmarc.stored.ge "v=DMARC1; p=none"` to start. The root MX stays Outlook. 3,000 messages a month included, $0.35 per 1,000 after.
8. **Plan edits** written in this PR: 40.1(a) struck and Workers Cache named with the numbers; 40.1(b) the Sippy semantics; 40.5 Dynamic Workers = open beta ($0.002 per Worker-day after 1,000); 42.2 the end-user-is-the-tenant note; **new 42.7 "Admission and the ledger"**; the COGS table is `.private/EDGE_COGS_2026-10-05.md`.
9. **Pro zone trial — decision memo:** not yet. Pro ($20/mo) buys the WAF custom rules with `is_timed_hmac_valid_v0` (signed public links checked at the edge, zero Worker on the byte path) and 20 cache rules, and still no Cache Reserve, no change to the 100 MB body limit. Today a signed private link costs $0.30 per million through the gateway Worker and there is no public-link traffic to put on the byte path; trial the month when `/cdn` carries real signed-link volume or when the WAF managed ruleset is wanted for the dashboard. The Dynamic Workers prototype (one Phase 31 rule, no network, CPU cap) was not run — the session's time went to the If-Match defect.

**Torn down 2026-10-05 evening:** Workers `vt-gw` (+ custom domain `gw.stored.ge`) and `vt-ifmatch-probe`; R2 bucket `vt-sippy-range` (three abandoned Sippy multipart uploads aborted first — the bucket refused to delete until then); iDrive bucket `vt-firstread-20261005` (created and deleted inside one test); Vaultaire bucket `cf-tests-20261005` (bench tenant, 20 objects); the R2 credentials file and 5 GB of test files on the box; the packet captures. **Kept:** the zone cache rule above (a fix, documented), and a Worker `ql-proxy` on the fairforge account that this session did not create and did not touch. Nothing unauthenticated that writes or fetches is up.

## Findings 2026-10-09 (Birthday Week 2026 claims, live-tested on the lab account, everything deleted after)

- **Large objects into R2 without Sippy — the DO copier works** (plan 40.1(e)). Against the prod origin: one Durable Object streaming one GET into `R2.put()` landed 300 MiB in 13 s, 1 GiB in 31 s, 2 GiB in 57 s; a coordinator DO fanning parts out to 8 child DOs landed 2 GiB in 17–29 s and **5 GiB in 39 s**; all byte-exact (SHA-256 of the R2 copy computed inside Cloudflare = the hash recorded at upload), zero retries. One outbound fetch inside a DO stayed open 13.8 minutes. Limits that still bind: six open connections per invocation (8 parallel parts in one DO → `Response closed due to connection limit`; ≤ 3 parts per DO), a single `R2.put()` of ≥ 5 GiB is refused only after the whole body streamed (multipart only above ~4.9 GiB), a DO left in "retrying" after a connection-limit failure needs a watchdog alarm. Sippy stays for small objects and as the fallback.
- **Subrequests:** the default is 10,000 per invocation, not a million; `limits.subrequests` raises it to 10,000,000 (120,000 calls in one request measured). A Worker fetching another workers.dev Worker of the same account gets 404 without the `global_fetch_strictly_public` compatibility flag.
- **Long outbound connections:** idle-trickle fetches of 16 and 21 minutes completed from a DO and from a plain Worker.
- **Dynamic Workers:** 10 concurrent per request into a Durable Object (the 11th throws); a plain Worker request is still capped at 4.
- **Headers:** `stored.ge` answers up to a 127 KiB header; Cloudflare refuses at 128 KiB before the request reaches HAProxy.
- **R2 credentials scoped to one bucket** work (every other bucket refused, `vaultaire-public` included); creating a token scoped to one Worker fails (API 1004).
- **Post-quantum TLS:** `stored.ge`, `stored.cloud` and R2 negotiate X25519MLKEM768; the Cloudflare → origin leg was classical (HAProxy 2.8.16 on OpenSSL 3.0.13 has no ML-KEM) — **resolved 2026-10-10**: the box's upgrade to Ubuntu 26.04.1 (HAProxy 3.2.9 / OpenSSL 3.5.5) makes the origin negotiate X25519MLKEM768, Cloudflare → origin included (and OpenSSH 10.2 negotiates `mlkem768x25519-sha256`); Vaultaire's own backend connections are classical everywhere except R2 (iDrive, Wasabi, Geyser, B2: X25519; Lyve: P-521). Workers' WebCrypto does ML-KEM-768/1024 and ML-DSA-44/65/87 behind `webcrypto_modern_algorithms` and interoperates with Go's `crypto/mlkem` both ways.
- **Traces** (open beta) are configured at `/zones/{zone}/observability/tracing/{settings,rules}`; the zone token lacks *Zone Observability Write*, so whether `traceparent` reaches Vaultaire is untested.
- **Containers on `ctx.container`:** cold ≈ 0.26 s in the DO, wake ≈ 0.3–0.5 s, warm ≈ 3 ms; an idle container stops 20–25 s after its last request with SIGTERM then SIGKILL ≈ 5 s later, even when PID 1 ignores SIGTERM.
- **EmDash 1.0** stores media in any S3-compatible bucket with path-style requests — `https://stored.ge` works as its endpoint (browser uploads go to presigned URLs, so the bucket needs CORS rules).

## Findings 2026-10-09, evening: Sippy vs the DO copier on the same objects

Same objects, the prod origin, origin bytes from Stored's own access log on the source bucket. Sippy lands on the first view up to 500 MiB (7 of 7, listed 11–32 s later, byte-exact), 600–650 MiB 2 of 3, 750 MiB–1.5 GiB 0 of 6 (200 MiB parts, up to five in flight, abandoned after ~25–30 s; the viewer still receives the whole object; an in-progress multipart upload is left in R2). A failed key resumes on the next view and fetches only the missing parts — every failed key landed by the second or third view. Warming with small ranged GETs does not work (eight on a 2 GiB object read 7.6 GiB from the origin and never landed). Origin reads per promotion: once below 200 MiB, twice for 200–650 MiB, 3–5× above, plus one per concurrent cold viewer; the DO copier reads once (five simultaneous triggers started one job) and listed 300 MiB in 6.6 s, 1 GiB in 10 s, 5 GiB in 40 s, all byte-exact. Sippy's first byte to the first viewer: 0.8–2.5 s (a direct origin read: 0.65–0.76 s). Split adopted in plan 40.1(e): Sippy below 512 MiB (with a copier push for hot objects), the copier above, Sippy kept as the fallback, a 1-day abort-incomplete-multipart rule on Sippy buckets.

## Findings 2026-10-21: Cloudflare in front of Sia (s3d)

Full numbers in `bench-results/SIA-S3D-2026-10.md` § SIA-BENCH-3. Everything
was torn down the same hour: Worker `vt-sia` + custom domain
`vt-sia.stored.ge`, Workflow `vt-sia-hydrate`, R2 `vt-sia-hydrate` /
`vt-sia-sippy`, and the quick tunnel.

- **A Quick Tunnel carries SigV4 unchanged.** `cloudflared tunnel --url
  http://127.0.0.1:9002` exposed one lab s3d to R2 Sippy and to a Worker with
  no inbound port.
- **The Sippy API takes an S3-compatible source that wrangler can't.**
  - `PUT /accounts/{id}/r2/buckets/{b}/sippy` with
    `source.provider = "s3"` and `source.bucketUrl = https://<host>/<bucket>`.
    wrangler 4.151 offers only AWS and GCS.
  - The destination R2 keys are the token-derived pair (token id /
    SHA-256 of the token).
  - The >750 MiB limit still holds: a 1 GiB miss was served but never
    landed.
  - Disabling Sippy leaves the abandoned multipart upload; abort it before
    deleting the bucket.
- **Workflows are the durable hydrator.**
  - One `step.do` per 64 MiB part into an R2 multipart upload, with 4
    in-step resumes then step retries (2 s exponential).
  - 1 GiB landed in 43–68 s.
  - With the origin stopped for 58 s mid-run, it completed in 96 s; the step
    history shows each `origin 502` attempt.
  - Running `Promise.all` over named steps parallelises parts.
- **The Cache API on a custom domain fronts a slow origin.** A 256 MiB miss took
  15–22 s and hits took 3.6 s, with `FixedLengthStream` guaranteeing no short
  cached body. It is a no-op on `workers.dev`.
- **wrangler errors on `delete` with the zone token.** It reports `User->Memberships->Read` missing. The script was deleted anyway (API 404 afterwards); check with `GET /workers/scripts/<name>/settings`.

## Findings 2026-10-21 (Blomp behind Vaultaire, bench account, everything deleted after)

Blomp (OpenStack Swift, `blomp` driver, #826) has no usable S3 of its own; through
Vaultaire a `blomp`-tier bucket is plain S3, so the S3-source products now work on it.

- **Sippy** with source `https://s3.stored.ge/blomp-bench` (path style): first read of
  4 KiB / 64 MiB / 256 MiB pulled from Vaultaire→Blomp (1.8 s, 16.5 MB/s, 75 MB/s),
  sha256 match, then served by R2. R2 → SLC reads stay slow (15–75 MB/s).
- **Super Slurper** (`POST /accounts/{id}/slurper/jobs`) — two settings, neither a
  Vaultaire bug:
  1. the source `endpoint` must NAME THE BUCKET (`https://s3.stored.ge/blomp-bench/`):
     given `https://s3.stored.ge/` Slurper treats the endpoint as the bucket's URL and
     sends `GET /?list-type=2…`, which Vaultaire answers as ListBuckets → "Preconnectivity
     failed, check source tokens". `pathStyle` is not an accepted field.
  2. the target must NOT carry `"jurisdiction": "default"` — with it the target
     pre-check fails ("check target tokens") even with a key that reads and writes the
     bucket; without it it passes (`PUT …/slurper/{source,target}/connectivity-precheck`
     test each side alone).
  With both: 4 KiB and 64 MiB copied in < 1 min, MD5 match. The 256 MiB object never
  came: Slurper opened an R2 multipart upload for it right after its HEAD (200 from us)
  and asked Vaultaire for no range in 50 min (HAProxy shows none); the job sat `running`
  2/3 until aborted (the multipart had to be aborted before the bucket could go). Not
  diagnosed further — a Slurper-side stall on large objects from this source.
- **A Worker reading Blomp directly** (plain HTTP on :8080 works from Workers):
  cache hits 16–34 ms TTFB, up to 447 MB/s from SLC; misses inherit Blomp's 5–10 s
  first-byte stalls. **Smart Placement** never moved the Worker (`local-SLC`,
  `local-EWR`) and changed nothing.
- **Presigned GET through `stored.ge`**: `cf-cache-status: DYNAMIC` (our
  `private, no-cache`) — never cached, as the zone rules intend.
- **rclone from Hostbrr NYC, 256 MiB**: through Cloudflare 12.6 MB/s up / 49 down,
  direct `s3.stored.ge` 26.3 up / 36 down — uploads belong on the DNS-only S3 host.

## Inventory 2026-10-22 (both logins, dashboard session + API)

Checked signed in to each login (the dashboard's own `/api/v4` with the session) because the API tokens cannot see everything (the zone token gets 10000 on Pages, KV, D1 and Containers of the production account).

| | Production `cloudflare@fairforge.io` (`66866bb1…`) | Bench `isaacv17@gmail.com` (`71a9e011…`) |
|---|---|---|
| Workers / Pages / Queues / Workflows / Containers / Tunnels / Hyperdrive / Vectorize | none | none (`isaacv17.workers.dev` subdomain exists, nothing on it) |
| Worker routes, custom domains | none on either zone | — |
| R2 | `stored-sia-litestream` (private, no r2.dev, no custom domain) | `vaultaire-public` (the prod `r2` driver's bucket; private, no r2.dev, no Sippy) |
| KV / D1 | none | none (`BENCH_KV`, `bench-snapshelter` deleted 2026-10-22) |
| Access | Zero Trust team, no apps | not enabled |
| Members | the login only | the login only |
| API tokens kept | `vaultaire-plan-driver-zone-2026-10-04` (the zone token in the creds file), `vaultaire-email-sending-2026-10-12` (prod `SMTP_PASSWORD`) | `R2 Account Token` (= prod `R2_ACCESS_KEY`, checked by id — **never delete**), `black-bar-c5c7` (the bench account token in the creds file) |
| 2FA | **off** (login and account `enforce_twofactor`) | **off** |

Zone rules: `stored.ge` has the login/register/waitlist rate limit and the cache rule that leaves `/cdn/` alone; `stored.cloud` none; both SSL `strict`, no page rules. DNS: every A record is the SLC origin, proxied, except `s3.stored.ge` (DNS only, by design: S3 bodies bypass the 100 MB proxy limit) — so a deny for the origin has to be in HAProxy, not a WAF rule (the `/metrics` deny is, `docs/DEPLOY.md`).

Cleaned up 2026-10-22: bench user token `Cloudflare Agent (auto-generated)` (last used 2026-03-25); the Ente buckets `ente-meta` / `ente-photos` (~2.4 GB, kept since Ente stopped on 2026-10-11) emptied and deleted; `BENCH_KV`, `bench-snapshelter`. Production tokens `Agent Lee (auto-generated)` (never used) and account token `misty-meadow-ed8a` (last used 2026-07-31) are for deletion once the production login is open again.

## Open

2FA on both logins and rotation of everything pasted on 2026-10-04; an **Email Sending: Edit** token + stored.ge onboarded for Email Sending (item 7 above); the R2 Data Access Logs loss-rate count (dashboard toggle, item 6); the `/admin` Access gate; R2 event notifications → Queue → `direct-uploads/complete` so direct uploads register themselves; Workflows around Vault restores; a load balancer once a second origin exists; Workers for Platforms when a customer Worker must run; the stale NS records.
