# Stage 1a — Prompt 40 Session A: Part 0 (the WP-P39 review's RP56)

*2026-10-24. This is Session A of Prompt 40. Session B runs Part A (Postgres point-in-time recovery) and writes the docs PR for both halves: STATUS.md, the CLAUDE.md rows, the plan, and `docs/reviews/WP-P40.md`, which folds this note in. No migration (next **108**). Residuals here are RP57-1…9; Session B's start at RP57-10.*

Merged:

| PR | Merge | Items |
|---|---|---|
| #953 | `9864016c` | item 2, the DNS cache (RP56-2) |
| #956 | `5e5fc23c` | items 1, 4, 5, 6, and a P2 found doing item 5 |
| this PR | — | item 3 (robots / sitemap / favicon) and this note |

Lane: Engine (the S3 API's own requests). Consumer (the web host's three files). Trust (the operator's proof runs on a real sign-in).

## Step 0

- #952 (the review note) was in the merge queue at the start and merged as `e12ab75d` (docs-only, no deploy). `/version` = `fb48d430`.
- Main's CI on `fb48d430` was red with the DNS flake (RP56-2). The PR runs and Deploy were green.
- **#950 (Session B's `tenant-zero-proof.sh`) merged as `bc74487b` while this session ran**, so item 5 was in play. `docs/reviews/WP-P39.md` had not landed, so there were no RP55-10… residuals to take.
- Tenant zero's flag rows are untouched by this session.

## Item 1 — RP56-1 (P3): a request carrying S3 credentials is the S3 API's, on every host, before any route

| | |
|---|---|
| **Finding** | The web routes (the dashboard's public pages, `/docs`, `/status`, `/changelog`, `/abuse`, `/legal/*`, `/llms.txt`, `/health`, `/version`, `/metrics`) are chi routes ahead of the S3 catch-all on every host, and none looked at credentials. A signed GetObject of key `privacy` in bucket `legal` got 200 with the privacy page as its bytes. ListObjects of `docs` / `status` / `login` / `changelog` / `abuse` got HTML, and every key of bucket `dashboard` a 303. `validateBucketName` reserves nothing, and prod's S3 endpoint is also its web host. |
| **Sev** | P3 |
| **Fix** | **The gate.** `s3CredentialGate` (`internal/api/server.go`) is one `s.router.Use` after request-id / canonical / limits / version / logging, before site statistics and every route. A request with AWS-scheme credentials goes to `s3Handler()` (the `s3Fallback` test hook still works).<br><br>**The predicate.** `signedS3Request` (`translog_read.go`) is narrowed and is now the one predicate. It matches an `Authorization` header of scheme `AWS4-HMAC-SHA256` or SigV2 `AWS` (case-insensitive), or a presigned query: `X-Amz-Algorithm=AWS4-HMAC-SHA256`, `X-Amz-Signature`, `X-Amz-Credential`, or `AWSAccessKeyId` **with** `Signature`. It used to match any `Authorization`.<br><br>**Exceptions.** Any other scheme (a Bearer JWT, Basic) is never S3. OPTIONS is the preflight's (answered before any authentication). `GET /api/v1/whoami` verifies SigV4 itself. A trust host on a public origin is never the S3 API (contract rule 11; `TrustHostRouter` answers it first, and the translog routes' trust branch 404s it behind).<br><br>**Also.** `handleRoot` and site statistics use the predicate (`isS3RootRequest` is gone). **Lab finding A-1:** `handleS3Request`'s first lines answered an empty 200 for `/health` `/ready` `/metrics` `/version`, so a signed ListObjects of bucket `health` would have got an empty 200. That guard now applies to unsigned requests only (`s3.go`, outside this session's file list; the edit is two lines). |
| **Routes that read `Authorization` themselves** | `requireJWT` (`server.go`), behind `/api/v1/{manage,user,sts,webhooks,events,admin}` and `/api/compliance`. `recoveryAuth` (`recovery_tokens.go`), on `POST /api/v1/recovery/tokens`: any `Authorization` goes to `requireJWT`, else the session cookie. `handleWhoami` (`whoami.go`): SigV4 header or presigned, the gate's exception. The predicates themselves: `signedS3Request`, `isAnonymousFetch` / `isBrowserPageView` (`host_canonical.go`: any `Authorization` means no redirect and no page view), and `s3.go`'s web-404 hook (`Authorization == ""`). In the S3 path: `auth.ValidateRequest`, `auth.HeaderAccessKey`, `s3_lock.go`. Outbound only (`integrations`, `global/cdn`) is not a route. The dashboard reads no `Authorization` (session cookies). |
| **The other routers** | The CDN host router serves its own router and never `s.router`, so the gate never runs there: the CDN host is not an S3 endpoint. **The trust host on a public origin has no S3 hand-off at all** (the prompt's "its S3 hand-off" is the translog routes' rule on a development origin, and that is unchanged): a signed request there is the trust router's 404, before and after. `TestS3Gate_TheTrustHostIsNeverTheS3API` covers both the outer router and the main router's own trust branch. |
| **RED on main** | `TestS3Gate_ARealRoundTripOnWebPathBuckets`: GetObject `legal/privacy` returned `<!DOCTYPE html>…` (the privacy page), and ListObjectsV2 `docs` failed `expected element type <ListBucketResult> but have <html>`. `TestS3Gate_ASignedRequestToEveryRouteIsTheS3APIs` (a `chi.Walk` matrix of every route before the catch-all × SigV4 header / SigV2 header / presigned / SigV2 query, on prod-today and target hosts) failed 818 rows on prod-today alone, e.g. `GET stored.ge/abuse (sigv4 header) … answered 200 text/html`. `TestS3Gate_TheReviewsRowsAreTheS3APIs` and `TestSignedS3Request` (Bearer, Basic, `AWSX`, a bare `Signature`) were red too. |
| **Mutation → caught by** | Gate dropped → the matrix, the review rows and the round trip. Gate on any `Authorization` → `TestS3Gate_ABearerRequestKeepsItsRoute` + `TestSignedS3Request`. Presigned branch dropped → the matrix (145 presigned + 145 SigV2-query rows), the round trip's presigned GET, `TestSignedS3Request`. Whoami exception dropped → the matrix's whoami row. Trust exception dropped → `TestS3Gate_TheTrustHostIsNeverTheS3API`. OPTIONS exception dropped → `TestS3Gate_OptionsIsThePreflights`. The empty-200 guard for signed requests (A-1) → the round trip's `/health` `/ready` `/metrics` `/version` rows (`expected: 404 actual: 200`). |

### Lab

The lab ran `bin/vaultaire` of the branch on a private DB `vaultaire_lab_p40a`, `STORAGE_MODE=local`, with one lab tenant, `curl --resolve` per name, and two host configurations:

- **Prod today:** `VAULTAIRE_BASE_URL` = `VAULTAIRE_ENDPOINT` = `https://stored.ge`.
- **Target:** `VAULTAIRE_PUBLIC_HOST=https://stored.cloud`, `VAULTAIRE_ENDPOINT=https://s3.stored.cloud`, `VAULTAIRE_ALIAS_HOSTS=stored.ge,www.stored.ge,www.stored.cloud`.

`main` (`fb48d430`) and the branch were run against the same DB and tenant.

aws-cli (`--endpoint-url`), with buckets `docs`, `legal`, `status`, `dashboard`, one object each:

| Command | main | branch |
|---|---|---|
| `aws s3 cp s3://legal/privacy -` | **the privacy page's HTML, exit 0** | `bucket legal key privacy: the customer bytes` |
| `aws s3api list-objects-v2 --bucket docs` | `An error occurred (500) … Internal Server Error` (the SDK could not parse the HTML) | `index.html` |
| `aws s3api get-object --bucket dashboard --key x` | `An error occurred (303) … See Other` | the object |
| `aws s3api head-bucket --bucket status` | rc 0 (the status page answered) | rc 0 (the bucket) |
| `head-object` / `get-object` on all four | — | 45 B, ETag `61c579db…`, the bytes |
| presigned GET (`aws s3 presign`) of `legal/privacy`, `dashboard/x` | — | 200 `application/octet-stream`, the bytes |

`curl --aws-sigv4` with the lab key, on `stored.ge`, `stored.cloud` and `s3.stored.cloud`, in both host configurations:

| path (signed) | main | branch |
|---|---|---|
| `/legal/privacy` | 200 `text/html` (the page) | 200 `application/octet-stream`, the object |
| `/docs?list-type=2`, `/status` | 200 `text/html` | 200 `application/xml` `<ListBucketResult>` |
| `/dashboard/x` | 303 → `/login` | 200, the object |
| `/llms.txt`, `/health` | 200, the file / the JSON | 404 `application/xml` `NoSuchBucket` |
| `/docs` with the review's fake signature | 200 `text/html` | 403 `application/xml` |
| `/legal/privacy` with a SigV2 header | 200 `text/html` | 403 `application/xml` `SignatureDoesNotMatch` |
| `/api/v1/manage/buckets` with `Bearer junk` | 401 `text/plain` (the JWT check) | 401 `text/plain` (unchanged) |
| `trust.stored.cloud/translog/checkpoint` signed | 404 `text/plain` | 404 `text/plain` (unchanged) |

The review's fake-signature probe (13 paths: `docs`, `docs?list-type=2`, `status`, `login`, `changelog`, `abuse`, `legal/privacy`, `dashboard`, `dashboard/x`, `register`, `llms.txt`, `health`, `version`) on `stored.ge` and `s3.stored.cloud`, both configurations: **52/52 `403 application/xml`**.

Unsigned and unchanged (Prompt 39's browser rows):

- browser `/docs`, `/legal/privacy`, `/status`, `/changelog`, `/login`, `/register`: 200 `text/html`;
- `/nonsense`: 404 `text/html`;
- `/signup`: 301 `/register`;
- curl `*/*` `/status`, `/docs`: 200;
- in the target configuration every `stored.ge` browser row is the alias 301;
- unsigned GET `/health`, `/version`, `/metrics`, `/health/live`, `/ready`: 200 as before;
- unsigned HEAD `/health`: the old empty 200.

### Prod

`5e5fc23c` was deployed through the merge queue: CI and Deploy were green on the merge commit, `/version` = `5e5fc23c3373` (build 22:59:42Z), slot 8001 active since 23:19:25 UTC. PostgreSQL last started at 22:29:35 UTC (Session B's restart for WAL archiving), an hour before these proofs, so no overlap.

**The review's fake-signature probe** (`Authorization: AWS4-HMAC-SHA256 Credential=AKIDNOPE/…, Signature=00`, `x-amz-date`) covered 13 paths × `s3.stored.cloud` and `stored.ge`: `docs`, `docs?list-type=2`, `status`, `login`, `changelog`, `abuse`, `legal/privacy`, `dashboard`, `dashboard/x`, `register`, `llms.txt`, `health`, `version`.

- Before (`9864016c`, 23:0x UTC): 200 `text/html` (the `dashboard*` and `register` rows 303, `/health` and `/version` 200 JSON) on both hosts, as on 10-24.
- After (23:32:39 UTC): **26/26 `403 application/xml`**.

**The real round trip used the bench tenant's own S3 key** (`tenant-14a623b16b3f7012`, `~/vaultaire-bench/.env.bench` on the box, never printed), with aws-cli 2.35.21 from the box through Cloudflare + HAProxy, against `https://stored.ge` and `https://s3.stored.cloud`. Not tenant zero's temporary key, for two reasons:
- tenant zero is on the free tier and its one bucket (`tz-proof-1792873702`) is Session B's, kept overnight for the 05:30 `routing_truth` sample, so `mb s3://docs` could only have answered `TooManyBuckets`;
- the WP-P27 recipe for the temporary key mints a management JWT from `JWT_SECRET`, the very thing RP56-5 removes.

Each run did `mb` `docs` `legal` `dashboard` `status`, put one 56 B object in each, `list-objects-v2`, `head-bucket`, `get-bucket-location`, `head-object`, `aws s3 cp … -`, a presigned GET, delete, `rb`.

| step | before (`9864016c`, same script) | after (`5e5fc23c`) |
|---|---|---|
| `mb s3://dashboard` | `An error occurred (303) when calling the CreateBucket operation: See Other` | `/dashboard` |
| `list-objects-v2 docs` / `status` | `An error occurred (500) … (reached max retries: 2)` (the SDK could not parse the page) | `index.html` / `x` |
| `get-bucket-location docs` | `mismatched tag: line 70, column 2` | `us-central-1` |
| `aws s3 cp s3://legal/privacy -` | **the privacy page's HTML, exit 0** | `p40 proof: bucket legal key privacy, the customer bytes` |
| presigned GET `legal/privacy` | the privacy page | the bytes, 200 `application/octet-stream` |
| `aws s3 cp s3://dashboard/x -` | `(303) … HeadObject … See Other` | the bytes |
| `head-object` × 4 | `dashboard/x` 303 | 56 B `application/octet-stream` × 4 |
| `rb` × 4 | `dashboard` rc 254 (never created) | rc 0 × 4 |

Both endpoints gave the same answers. Afterwards the bench tenant held 0 buckets named `docs` / `legal` / `status` / `dashboard` and 0 head rows in them.

**Unsigned (unchanged):**
- browser `stored.cloud` and `stored.ge` `/docs`, `/legal/privacy`: 200 `text/html`; `/nonsense`: 404 `text/html`;
- curl `/status`, `/docs` on both: 200;
- browser `/signup` → 301 `/register` → **303 `/#waitlist`** (signups closed: RP56-4 live);
- `/auth/github`: 403 `application/xml`, as before. Prod sets no GitHub OAuth, so the route is not registered, and `/auth/` is never a page view.

**The JSON sign-in:** `POST /auth/login` for an unknown address answers 401 `Invalid credentials` and writes `auth.login_failed`, via `api:password`, reason `unknown_user` (23:33:31 UTC). The MFA path was proven in the lab only: it needs the admin's code (RP57-4).

**Journal** of the active slot since 23:19:25: 1,357 lines, **0** `"level":"error"`, 1 warn (`BLOMP_ALLOW_UNSEALED`, expected — RB-3).

## Found doing item 5 — `POST /auth/login` minted an admin JWT without the second factor (P2, pre-existing)

| | |
|---|---|
| **Finding** | Option (b) of RP56-5 needs "a real admin JWT, obtained by signing in with MFA", and there was no such JSON sign-in. `POST /auth/login` minted a 24 h JWT on the password **alone** for an account with MFA on, the prod admin's (MFA ON) included. The admin API (`requireJWT` + `users.role = 'admin'`) took that JWT. It wrote no audit row (the dashboard sign-in writes `auth.*` rows since R12). |
| **Sev** | **P2.** It needs the password, but it voids the second factor on the operator's account. Not in R11/R12/SYNTHESIS: R12-07 rate-limited the route, and WP-R12-4 (open) asked only for an audit row of the mint. |
| **Fix** | `handleLogin` (`server.go`) takes an optional `"code"`. For an account with MFA on (`IsMFAEnabled`, failing closed on a read error), the code must be a TOTP code, accepted once (`ConsumeTOTPCode`, RFC 6238 § 5.2), or else a backup code (single use, `ValidateBackupCode`): the dashboard's `/login/verify-2fa` sequence. With no code the answer is 401 `MFA code required: send "code" (…)`; with a wrong or replayed code, 401 `Invalid credentials`. Every outcome writes an `auth.*` audit row through the dashboard's `RecordLoginEvent`: `auth.login_failed` (`unknown_user` / `bad_password`, with the user id when the user exists), `auth.mfa_failed` (`code_required` / `replayed_code` / `bad_code`), `auth.mfa_succeeded`, or `auth.login_succeeded`, with via `api:password` / `api:totp` / `api:backup_code`. A non-MFA account is unchanged except for the audit row. The OpenAPI `LoginRequest` (`code`), the route's 401 text and `llms.txt` say so. |
| **RED on main** | `TestAuthLogin_AnMFAAccountNeedsItsCode`: password alone `expected: 401 actual: 200` with a token; a wrong code 200; a replayed code 200; the audit rows `[]string(nil)`. `TestAuthLogin_WithoutMFAThePasswordIsEnoughAndAudited`: no row. |
| **Mutation → caught by** | MFA state ignored (`if false && mfaOn`) → `…AnMFAAccountNeedsItsCode`. Replay guard dropped → `…AnMFAAccountNeedsItsCode` (the replayed row). Success row dropped → both tests. |
| **Lab** | One user: role `admin`, `user_mfa.enabled`. **main:** dashboard `POST /login` gives 303 → `/login/verify-2fa`; `POST /auth/login` (password only) gives 200 + JWT; `GET /api/v1/admin/flags` with that JWT gives **200**; 0 audit rows. **Branch:** password only → 401 `MFA code required…`; password + TOTP → 200; the same code again → 401; a wrong code → 401; rows `auth.mfa_failed api:totp code_required`, `auth.mfa_succeeded api:totp`, `auth.mfa_failed api:totp replayed_code`, `auth.mfa_failed api:totp bad_code`. |
| **Not fixed** | The JSON sign-in has the per-IP limiter (5/min) but not the dashboard's account lockout (10 failures / 15 min): RP57-2. |

## Item 5 — RP56-5: the proof script signs in, it never mints (option b)

| | |
|---|---|
| **Finding** | `tenant-zero-proof.sh`'s `refresh_auth` signed a 10 min HS256 JWT for tenant zero's user (the admin) with the box's `JWT_SECRET`. The audit rows and the transparency log's admin reads named the admin with no sign-in or MFA behind them, and an operator script forged the service's session tokens. |
| **Sev** | note |
| **Choice: (b)** | (a), a `via` claim, only labels honest use: anyone holding `JWT_SECRET` mints a token without the claim, so the mark proves nothing about the tokens that matter. Carrying it into the admin-read entry would add a value to `translog.AdminRead.ActorRole`'s closed list, which changes a public entry format that verifiers read. (b) puts a real sign-in and MFA event behind every action of the proof. With the P2 above fixed, a JSON sign-in with MFA exists. |
| **Fix** | The script never reads `JWT_SECRET` (nor the `.env`: `env_value` and `TZP_ENV_FILE` are gone). It takes `TZP_ADMIN_JWT_FILE` (refused when group or other can read it) or `TZP_ADMIN_JWT`: the token `POST /auth/login` answers to the admin's password **and** code. The header shows the curl. `jwt_check` reads the payload only (the server checks the signature) and requires that the token names tenant zero's user and tenant and has `TZP_JWT_MIN_LEFT` (default 30 min) left at the start, and 1 min before every call, so a run that outlives it stops at a clean refusal, never a 401 mid-step. The token is loaded before any database read, key or flag. |
| **RED on main** | `tenant-zero-proof_test.sh` § 3: `FAIL the script still reads JWT_SECRET`; `jwt_check: command not found`; `main` started without a token (it failed later, on `runuser`). |
| **Mutation → caught by** | Tenant check dropped from `jwt_check` → `FAIL jwt_check accepted: another tenant`. Mode check dropped → `FAIL main with a 0644 token file …`. |
| **Lab** | Against the lab server, tenant `tenant-f78c21c49d55d38b` (its user an admin with MFA): `TZP_ADMIN_JWT_FILE` = the token of a password + TOTP sign-in, `TZP_PSQL="psql -d vaultaire_lab_p40a"`. `preflight` ran end to end: temp key minted, 7 result rows, key deleted (HTTP 200). The audit rows, newest first: `key.revoked`, `key.created`, then `auth.mfa_succeeded api:totp`. The same token with its `exp` set 10 min ahead: `the admin JWT has 600s left; the run needs 1800s — sign in again`, `FATAL admin JWT`, nothing done. |

## Item 2 — RP56-2: the DNS cache re-reads its entry inside the singleflight (#953)

| | |
|---|---|
| **Finding** | `dnsDialCache.entry` read `c.entries[host]`, then called `DoChan`. Another dial's flight could store the answer and finish in between, and a second lookup then started. In the product that is one extra resolver query. In CI, `TestDNSCache_ADialWhoseContextEndsDuringTheLookupReturnsAtItsDeadline` failed with calls 2 ≠ 1 under `-race` (main red on `fb48d430`). |
| **Sev** | flake (benign race) |
| **Fix** | The flight function re-reads the cache first; a fresh entry serves without a lookup. |
| **RED on main** (seam only) | `TestDNSCache_ADialThatMissedTheCacheJustBeforeAnotherFlightLandedDoesNotLookUpAgain`: the seam `beforeFlight` parks dial B between the read and `DoChan` while dial A's flight lands. Result: `expected: 1 actual: 2`. |
| **Mutation → caught by** | Re-check dropped → the same test. |
| **Stress** | Every `TestDNSCache*` at `-race -count=20`. The CI-flaking test at `-race -count=300 -cpu 1,2,8`: green. |
| **Main after** | Main CI on `9bbfb600` (#954, the first run that completed after #953; #953's own push run was cancelled by it) is green, and it carries the fix. #953 was deployed (`/version` `9864016c64db`). |

## Item 3 — RP56-3 (P4): `robots.txt`, `sitemap.xml`, `favicon.ico` on the web host

| | |
|---|---|
| **Finding** | All three answered every crawler with 403 XML: crawlers send `*/*` / `image/*`, so Prompt 39's browser predicate never fired. |
| **Sev** | P4 |
| **Fix** | **Who gets them.** `web_files.go` serves an anonymous GET/HEAD on a web host (`hosts.IsWebHost`). Anonymous is `isAnonymousFetch`, split out of `isBrowserPageView`: no `Authorization`, no `x-amz-*` header or query, no presigned query, any `Accept`. Anything else at those paths is `handleS3Request`; they are valid bucket names, and the S3 API has no anonymous read for this to shadow. A signed request never gets here (item 1), and the cdn. and trust. routers never serve them.<br><br>**`robots.txt`.** `User-agent: *`, `Allow: /`, `Disallow: /dashboard`, `/api/`, `/auth/`, `/cdn/`, and `Sitemap: <canonical origin>/sitemap.xml` (`hosts.URL`).<br><br>**`sitemap.xml`.** Canonical absolute URLs of live pages only: `/`, the guides (`docsPages`, the map the routes register, now a package var) and `/docs/api`, `/status`, `/changelog`, then `dashboard.LiveLegalPaths(flags)`. That is the always-public legal pages (`legalPages`, moved out of `RegisterRoutes`), the law-enforcement pages only while `legal_le_pages` is on, and the drafts only while `legal_draft_pages` is on, each through its page's own gate (`legalLEOn` / `legalDraftOn`). **No `<lastmod>`:** the build's commit time is when the binary changed, not when a page did.<br><br>**`favicon.ico`.** The landing's pixel icon, made by `internal/api/landing/favicon.py` (stdlib; `make landing` runs it after `build.py`) from the `data:` SVG in `landing.src.html`. It is an ICO of three PNGs at 22, 44 and 88 px (whole-pixel scales of the 22-cell grid, so the pixel art stays crisp), 561 B, embedded (`//go:embed`). |
| **Cache-Control** | `robots.txt` `public, max-age=3600`: it changes only with the canonical origin, i.e. a deploy. `sitemap.xml` `public, max-age=300`: it follows the two legal-page flags, which the flag service caches ~15 s; a flip shows within 5 min. `favicon.ico` `public, max-age=86400`: it changes with the landing's icon, i.e. a deploy. All `nosniff`. |
| **RED on main** | `TestWebFiles_ServedToAnAnonymousFetchOnAWebHost`: `expected: 200 actual: 403` on every row (both host sets). `TestWebFiles_NeverOnTheS3PathOrTheOtherHosts` and `TestWebFiles_TheSitemapListsLivePagesOnly` failed too, and `TestWebFiles_TheFaviconIsTheLandingsIcon` had no file. |
| **Tests** | Every crawler `Accept` (none, `*/*`, `image/*…`, `text/plain`) × GET/HEAD × the web hosts of both host sets: 200, type, cache. Signed (all three paths, three hosts) → the S3 API. `x-amz-content-sha256`, Bearer and a presigned-shaped query → not served. `s3.stored.cloud` → 403 XML; `cdn.` / `trust.` → 404. `PUT` → not served. **The sitemap, flag by flag** (both off, each on, both on): every URL is `https://stored.cloud/…` and answers an unsigned browser GET with 200 `text/html` on the same server, a dark page is never listed, and a page whose flag is on is (the `TestLegalDraftPages_NothingLinksThem` family). **The favicon:** the ICO header and directory, each PNG decoded and compared pixel by pixel with a grid re-derived in Go from the SVG (independently of `favicon.py`). |
| **Mutation → caught by** | Host check dropped from `webFetch` → `…NeverOnTheS3PathOrTheOtherHosts` (s3.). Drafts listed whatever the flag → `…TheSitemapListsLivePagesOnly`. Icon colour edited in `landing.src.html` without a re-run → `…TheFaviconIsTheLandingsIcon`. `Authorization` not checked in `isAnonymousFetch` → `…NeverOnTheS3PathOrTheOtherHosts` (Bearer). |
| **Lab** | Prod-today configuration: `stored.ge` `/robots.txt`, `/sitemap.xml`, `/favicon.ico` with `Accept: */*` → 200 `text/plain` / `application/xml` / `image/x-icon` with the three cache headers; `s3.stored.cloud` → 403 `application/xml`; signed on `stored.ge` → 404 `application/xml` (`NoSuchBucket`, the S3 API's). Target configuration: `stored.cloud` the same 200s; `s3.` 403 XML; `cdn.` 404 `text/plain`; `trust.` 404 `text/plain` `no-store`. The target `robots.txt` names `Sitemap: https://stored.cloud/sitemap.xml`. Its sitemap, flags off, has 16 URLs: `/`, five `/docs…`, `/status`, `/changelog`, eight `/legal/…`; no refunds, dmca, law-enforcement or what-we-can-produce. |
| **Prod** | #957 `dec6941d` deployed (CI and Deploy green on the merge commit; `/version` `dec6941d5b3b`, build 23:57:21Z). At 00:25 UTC with `Accept: */*`: `stored.cloud` and `stored.ge` `/robots.txt` 200 `text/plain`, `/sitemap.xml` 200 `application/xml` `public, max-age=300`, `/favicon.ico` 200 `image/x-icon` `public, max-age=86400`. Before: 403 `application/xml` on all six. `s3.stored.cloud`: 403 `application/xml` on all three. `cdn.` / `trust.stored.cloud` do not resolve publicly yet (lab rows only). `stored.ge`'s robots.txt names `Sitemap: https://stored.ge/sitemap.xml`; its sitemap lists the 16 live pages and none of the four dark ones. Fake-signed on `stored.ge`: 403 XML on all three (`cf-cache-status: DYNAMIC`: the zone's "S3 API is never cached" rule). **Fake-signed on `stored.cloud`: `robots.txt` 200 `text/plain` and `favicon.ico` 200 `image/x-icon`, `cf-cache-status: HIT`**, i.e. the edge's cached copy answered the signed request (RP57-5, fixed by the follow-up PR below). Every other path on `stored.cloud`, fake-signed: 403 XML (`/`, the 13 review paths, `/og.png` BYPASS, `/openapi.json`) |

## Item 4 — RP56-4 (note): `/register` while signups are closed

| | |
|---|---|
| **Finding** | `/signup` → `/register` → 303 `/`: a launch reader landed on the hero, not on the waitlist the landing's own CTAs use (`href="#waitlist"`). |
| **Fix** | `/register` (`internal/dashboard/router.go`) redirects to `/#waitlist` while `SignupsEnabled()` is false; the gate is unchanged. |
| **RED on main** | `TestRegister_ClosedSignupsGoToTheWaitlist`: `expected: "/#waitlist" actual: "/"`. Open: 200 "Create Account". |

## Item 6 — RP56-6 (note): our realm-token mint failure is not the realm's `invalid_auth`

| | |
|---|---|
| **Finding** | `HTTPTransport.post` wrapped a failure of its own `Token()` as `ErrInvalidAuth`, so `realmOutcome` would count our mint failure as the realm's `invalid_auth` (a page for a realm that was never asked). |
| **Fix** | New `client.ErrTokenMint`, wrapped **together with** `ErrInvalidAuth`, so the client's own per-realm logic (`checked.send`, `failure`) is unchanged. `realmOutcome` checks it first and returns `""`: ours, counted as nothing, the tenant still deferred with the reason. A realm's own 401 stays `invalid_auth`. |
| **RED on main** | `TestRealmOutcome_OurTokenMintFailureIsNotTheRealms` (a token func that errors, through `HTTPTransport` and `client.Delete` against a real X25519 realm key, so the handshake reaches `post`): `expected: "" actual: "invalid_auth"`, and nothing was sent. |

## Residuals

| # | Sev | What | Owner / where |
|---|---|---|---|
| RP57-1 | P4 | A bucket named `api` with key `v1/whoami` is shadowed for SigV4 GETs (whoami keeps its route). Unchanged from before; `validateBucketName` still reserves nothing. Reserving the web paths' names (`api`, `docs`, `legal`, …) at CreateBucket would make the gate's exceptions moot | Stage 1a |
| RP57-2 | P3 | The JSON sign-in `POST /auth/login` has the per-IP limiter (5/min) but not the dashboard's per-account lockout (10 failures / 15 min). Stuffing one account's password from many IPs is throttled only per IP | Stage 1a |
| RP57-3 | P4 | `/auth/login` still answers `User not found` (404) when the password validated but the user row could not be loaded (unchanged), and writes no row for that case | Stage 1a |
| RP57-4 | note | Tenant zero's next proof run (`tenant-zero-proof.sh`) needs the owner: a `POST /auth/login` with the admin's password **and** MFA code, its token in a 0600 file (the script's header). The WP-P27 recipe for a temporary key (a management JWT minted on the box from `JWT_SECRET`) is the pattern RP56-5 removed and should not be reused. This session's prod round trip used the bench tenant's own S3 key instead | owner |
| RP57-5 | P4 → fixed | **Found on prod after #957.** The `stored.cloud` zone has no "S3 API is never cached" rule yet (the `stored.ge` zone has had it since 10-05; WP-DOMAIN-1's owner click-list, `docs/CLOUDFLARE.md`), so Cloudflare cached `/robots.txt` and `/favicon.ico` by extension (they are `public`). Its cached copy then answered a **signed** `GET stored.cloud/favicon.ico` (ListObjects of a bucket `favicon.ico`) with the icon, `cf HIT`. `stored.ge` was not affected (DYNAMIC). **Fix (follow-up PR):** all three files carry `CDN-Cache-Control: no-store` (RFC 9213, honoured by Cloudflare): browsers still cache them, the edge never stores them, whatever the zone says. Test: `TestWebFiles_ServedToAnAnonymousFetchOnAWebHost` asserts it (mutation: header dropped → 96 rows red). The copies cached before the fix expire on their own (robots ≤ 1 h, favicon ≤ 1 day at the edge, by the origin's max-age); a purge is the owner's. The zone rule itself is still owed | WP-DOMAIN-1 (owner: the `stored.cloud` zone rules) |
| RP57-6 | note | `handleS3Request` still answers an unsigned non-GET of `/health` `/ready` `/metrics` `/version` (e.g. HEAD `/health`) with an empty 200: kept as it was, since an unknown prober may rely on it | — |
