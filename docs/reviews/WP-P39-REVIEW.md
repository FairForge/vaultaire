# WP-P39 post-merge review (RP56) — Session A and #949; Part B pending

*2026-10-24 ~21:45 UTC. Reviewed what had merged: #948 `72c0fa8a` (Part 0), #951 `fb48d430` (Part A + `WP-P39-PART0A.md`), #949 `2a00052f` (Session B's modes_copy list fix). **Not reviewed yet:** Session B's #950 (`deploy/scripts/tenant-zero-proof.sh`, open, armed, BEHIND at the time), its next-morning reads (`routing_truth` 05:30 UTC on 10-25) and its docs PR (`docs/reviews/WP-P39.md`). The next review reads those first. No migration (next **108**).*

## Verdict

Session A's Part 0 and Part A **hold**. Every test the note names exists in the merged tree, checked by name (the RP50-1 check): `TestAccountDeletionRunner_SilentRealmIsAskedOncePerRun`, `…AnsweringRealmIsAskedForEveryTenant`, `TestAccountDeletionRealmSeries_ExistBeforeTheFirstDelete`, `TestRecoveryRealm_FullSessionTableStillAnswersADelete`, `TestClientBuildManifest_ReproCheckWarnsWhenTheCheckoutBuildWritesNothing`, the seven `TestWebHost_*` / `TestLegalDraftPages*` tests and `TestIsWebHost`. #949 holds: one renderer of the bucket list, so it is one place that sets `ModesCopy`.

**Prod `fb48d430`** (slot 8001 since 21:17:14 UTC): 1,268 journal lines, **0** `"level":"error"`, 1 warn (the expected `BLOMP_ALLOW_UNSEALED` line, RB-3). All 16 rule files on the box match main by sha256. Browser `Accept: text/html` with no credentials:

| URL | Answer |
|---|---|
| `stored.cloud/pricing` | 301 `/#pricing`, `no-store` |
| `stored.cloud/signup` | 301 `/register` → **303 `/`** (signups closed: RP56-4) |
| `stored.cloud/nonsense`, `stored.ge/nonsense`, `stored.cloud/legal/refunds` | 404 `text/html`, `no-store`, CSP, `X-Frame-Options: DENY`, nosniff |
| `s3.stored.cloud/nonsense` | 403 `application/xml` (unchanged) |
| `stored.cloud/nonsense` from curl (`*/*`) | 403 `application/xml` (unchanged) |

Tenant zero (`tenant-4e1b43062db9d685`) at review time: tenant rows ON for `chunking`, `parallel_get` and `versioned_chunking` (20:28–20:29 UTC), plus `modes_copy`, `translog_admin_reads` and `translog_customer_view`. It holds 4 objects (578,813,952 B) in one bucket `tz-proof-1792873702`, backends `chunked` and `idrive`. **No `'*'` row changed:** the only global row is `vault_parity` (10-07).

## Adversarial pass

| # | Checked | Result |
|---|---|---|
| 1 | Can the new chi routes (`/signup`, `/pricing`, `/legal/refunds`, `/legal/dmca`; GET+HEAD only) swallow a CreateBucket `PUT /signup`, a `DELETE`, a `POST ?delete` or an `OPTIONS` preflight with 405? | **No.** I tested the same router shape on chi v5.3.2 in a throwaway test: every other method falls through to `/*`, and OPTIONS goes to the preflight |
| 2 | `realmOutcome` against the client's real error chain (`client.Delete` = `errors.Join` of `%w`-wrapped `ErrTransient` / `ErrInvalidAuth` around `client.Do`'s `*url.Error`) | Holds. A skipped realm is always a *deferral*, never a dropped erasure. A `Token()` failure (ours) is wrapped `ErrInvalidAuth`, so it would be counted as the realm's `invalid_auth`. HS256 over a configured key cannot fail today, so this is a note (RP56-6) |
| 3 | RP54-2: a lifetime-0 answer to a handshake-riding Register1 / Recover1 | Neither mutates the guess count. The client opens a fresh session for phase 2. Holds |
| 4 | The 404 page's headers | Same security headers as `/legal/privacy`. Holds |
| 5 | **What else answers on the S3 path?** (the RP55-3 thread, followed past `legal`) | **RP56-1, P3, proven on prod.** Below |
| 6 | CI on main | `fb48d430` CI **failed**: `TestDNSCache_ADialWhoseContextEndsDuringTheLookupReturnsAtItsDeadline` (`internal/drivers`, untouched by P39), calls 2 ≠ 1. **RP56-2** below. The PR runs were green and Deploy succeeded |

### RP56-1 (P3, pre-existing) — the web routes answer SIGNED S3 requests on every host, the S3 host included

The dashboard's public routes and the API's own pages are chi routes that sit ahead of the S3 catch-all on **every** host, and none of them looks at the request's credentials. I sent prod a request with a SigV4-shaped `Authorization` header and `x-amz-date`; the S3 handler would answer that 403 XML, so a 200 HTML proves the handler never saw it:

| `https://s3.stored.cloud/<path>` with `Authorization: AWS4-HMAC-SHA256 …` | Answer | What a customer's signed request gets |
|---|---|---|
| `docs`, `docs?list-type=2`, `status`, `login`, `changelog`, `abuse` | **200 text/html** | ListObjects / HeadBucket / GetBucketLocation of a bucket with that name gets a web page. The SDK fails to parse it, and HeadBucket says the bucket exists |
| `legal/privacy` (and every `/legal/<slug>`) | **200 text/html** | **GetObject of key `privacy` in bucket `legal` returns 200 with the privacy page as the object's bytes.** aws-cli / s3transfer do not check the ETag on download, so the file is silently wrong |
| `dashboard`, `dashboard/x`, `register` | **303** | every key of a bucket named `dashboard` |
| `llms.txt`, `/health`, `/version`, `/metrics` (valid bucket names) | the page or the JSON | ListObjects of those buckets |
| `docs/key.txt`, `status/key`, `login/k`, `legal/x`, `api/x`, `auth/x` | 403 XML | reach the S3 handler (fine) |

`validateBucketName` reserves nothing, so a customer can create bucket `docs`: the PUT is not a GET, so it falls through. Today's endpoint is `stored.ge`, which is both the web host and the S3 host, so routing by host alone cannot fix it. The predicate already exists: `signedS3Request` in `internal/api/translog_read.go` (Authorization, or a presigned query), used only by the translog read routes. **Fix → Prompt 40 Part 0 item 1:** one router-level gate ahead of every route. A request carrying S3 credentials goes to `handleS3Request`, except the routes that take their own `Authorization`. The fix must keep their auth schemes working: `Bearer` on `/api/`, `/auth/`, `/webhook/`. It narrows the shared predicate to the AWS schemes (`AWS4-HMAC-SHA256 `, `AWS `) plus the presigned query. A matrix test covers every route registered before the catch-all (walk `chi.Walk`, never a typed list).

### RP56-2 (flake, product race real but benign) — the DNS cache reads its entry outside the singleflight

`dnsDialCache.entry` (`internal/drivers/dns_cache.go`) reads `c.entries[host]` and then calls `c.flight.DoChan`. Suppose another dial's flight stores the answer and finishes after that read but before this `DoChan`. This dial then starts a **second** lookup for an answer that is already cached. On CI under `-race`, the test's `Eventually` dial raced the abandoned refresh's completion this way. The product effect is one extra resolver query. **Fix → Prompt 40 Part 0 item 2:** re-check the cache inside the flight function (the double-checked read), and add a deterministic red test. The hook goes between the read and the `DoChan`, e.g. a `beforeFlight` test seam.

### RP56-3 (P4) — `robots.txt`, `sitemap.xml` and `favicon.ico` answer every crawler with 403 XML

Part A's predicate requires `Accept: text/html`, and crawlers and favicon fetches send `*/*` or `image/*`. Today `stored.cloud/robots.txt` and `/sitemap.xml` answer 403 `application/xml` to every Accept tried, and so does `/favicon.ico` (the landing's icon is a `data:` URI, so browsers do not need it, but link-unfurlers do). → Prompt 40 Part 0 item 3: serve them to an *unsigned* request on a web host (RP56-1's gate sends signed ones to S3; `robots.txt` is a valid bucket name). The sitemap must list only live pages: flag-aware, never a dark page (`TestLegalDraftPages_NothingLinksThem` family).

### RP56-4 (note) — `/signup` lands at the top of the landing while signups are closed

`/signup` → `/register` → 303 `/`, because `/register` redirects to `/` when `SignupsEnabled()` is false, not to the waitlist the landing's CTAs use (`#waitlist`). → Prompt 40 Part 0 item 4 (one line + test).

### RP56-5 (note, Session B's #950, still open) — the proof mints admin JWTs from `JWT_SECRET`

`tenant-zero-proof.sh`'s `refresh_auth` signs a 10-minute HS256 JWT for tenant zero's user, the admin (MFA ON), with the box's `JWT_SECRET`. The flag PUTs are audited and the admin reads are logged in the transparency log, so nothing is hidden. But the actor in `audit_logs` is the admin with no sign-in or MFA event behind it, and an operator script now forges the service's session tokens. → Prompt 40 Part 0 item 5: mark these tokens (a claim the admin middleware copies into the audit metadata, e.g. `via: "operator_script"`), or have the script take a real admin JWT from a sign-in. The worker picks one and states why.

### RP56-6 (note) — a realm-token mint failure would count as the realm's `invalid_auth`

See row 2. Fold it into Part 0 item 1's PR or leave it, the worker's call: map a `Token()` error to its own outcome or to `""`.

## Owed by the next review

Session B's #950 diff (the script's cleanup and the `'*'` guard), the next-morning `routing_truth` / `dedup_gc` / `retired_blobs` reads, the tenant-zero verdict table in `WP-P39.md`, and the docs PR's CLAUDE.md / STATUS rows against Session A's merged diffs (the Step 0 drift check).
