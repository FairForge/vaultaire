# API

The customer-facing surface of Stored as served today. The maintained
short form is `GET https://stored.ge/llms.txt` (`internal/api/llms_txt.go`);
the machine-readable route list is `GET https://stored.ge/openapi.json`
(`internal/docs/openapi.go`, guarded by `openapi_drift_test.go` so a route
cannot ship undocumented), browsable at `https://stored.ge/docs/api`. Prices
quoted here come from `internal/api/landing/prices.json`; if the two disagree,
prices.json wins.

## Endpoint and addressing

| | |
|---|---|
| S3 endpoint | `https://stored.ge`; **`https://s3.stored.cloud`** (the same API behind the edge mover, WP-EDGE-1: an admitted upload of a tenant with the `edge_ingest` flag is streamed from Cloudflare's edge straight to its backend — the response is the same; keep every single PUT and every part under 100 MB there, the zone's request limit answers an HTML 413 above it) |
| Addressing | **path-style**: `https://stored.ge/<bucket>/<key>` (virtual-hosted `<bucket>.stored.ge` is not served) |
| Region | `us-east-1` in the SigV4 credential scope, whatever region the bucket was created in |
| CDN (public-read buckets only) | `https://cdn.stored.ge/<slug>/<bucket>/<key>` |
| Dashboard / JSON API | `https://stored.ge/dashboard`, `https://stored.ge/api/v1/…` |

## Authentication

### S3: AWS Signature Version 4

Every S3 request is signed with SigV4 — header auth (`Authorization:
AWS4-HMAC-SHA256 …`) or query auth (presigned URLs). Signature verification
is full (canonical request, signed headers, payload hash or
`UNSIGNED-PAYLOAD`, aws-chunked bodies, ±15 minute clock skew). Any S3 SDK
works unmodified. With curl ≥ 7.75:

```bash
curl --aws-sigv4 "aws:amz:us-east-1:s3" --user "KEY:SECRET" https://stored.ge/my-bucket/hello.txt
```

Three classes of credential are accepted, looked up in this order:

| Access key looks like | What it is | Where it comes from |
|-----------------------|------------|---------------------|
| `VK` + 16 hex (secret `SK` + 32 hex) | the account's **primary key**: full access to every bucket of the tenant | shown once at registration (`POST /auth/register` or the sign-up page); reset from the dashboard, which revokes every session |
| `VLT_…` | a **scoped key**: allowed operations, bucket list, IP allowlist, expiry; rotatable and revocable without touching the primary | dashboard → API Keys, or `POST /api/v1/user/apikeys` / `/api/v1/manage/keys`. The free tier allows one scoped key |
| `ASIA…` | **STS temporary credentials**: a time-boxed key whose scope is the intersection of the request and the parent (never wider) | `POST /api/v1/sts/token` with a JWT |

A revoked or expired key answers 403 (`AccessDenied` / `ExpiredToken`); a
request from outside a key's IP allowlist answers 403 with the peer as HAProxy
reported it (`internal/clientip`). A suspended account answers 403
`AccountSuspended` on every S3 request and its public buckets stop serving on
the CDN.

### Presigned URLs

GET and PUT presigned URLs (SigV4 query auth) verify against the same three
credential classes. Rules: `X-Amz-Expires` between 1 and 604 800 seconds
(7 days), `X-Amz-Date` not more than 15 minutes in the future and its day
equal to the credential-scope date, canonical query encoded per RFC 3986 (a
`response-content-disposition` with spaces verifies). Expired → 403
`ExpiredToken`; missing parameters → 400 `AuthorizationQueryParametersError`.
Account owners can also mint them without an SDK: `GET /api/v1/user/presigned`
(JWT).

### JSON API: Bearer JWT

```
POST /auth/register  {"email","password","company"} -> {"token","tenant_id","access_key","secret_key"}
POST /auth/login     {"email","password","code"?,"tenant_id"?} -> {"token","tenant_id","role","tenants"}   (24 h JWT)
POST /auth/password-reset {"email"};  POST /auth/password-reset/complete {"token","new_password"}
Authorization: Bearer <token>
```

`/auth/login` and the reset routes are limited to 5 requests/min per IP,
`/auth/register` to 10/min; a completed reset revokes every dashboard
session. Accounts with MFA enabled complete it in the dashboard; the JSON API
has no MFA endpoints.

## S3 operations

What `handleS3Request` implements (base `https://stored.ge`). `HEAD` is
answered from `object_head_cache` in ~1 ms and never touches a backend (the one
exception: `x-amz-restore` on GLACIER-class objects).

```
GET    /                              ListBuckets
PUT    /<bucket>                      CreateBucket   (optional <LocationConstraint> = an enabled region;
                                                      also X-Stored-Region / x-amz-bucket-region headers)
DELETE /<bucket>                      DeleteBucket   (empty only: no objects, versions or open multiparts)
HEAD   /<bucket>                      HeadBucket     (x-amz-bucket-region)
GET    /<bucket>?location             GetBucketLocation
GET    /<bucket>?list-type=2          ListObjectsV2  (prefix, delimiter, continuation-token, max-keys; v1 markers too)
GET    /<bucket>?versions             ListObjectVersions (no delimiter yet)
PUT    /<bucket>/<key>                PutObject      (streamed; Content-MD5 verified; x-amz-meta-*, x-amz-tagging,
                                                      Cache-Control/Content-Disposition/-Encoding/-Language stored;
                                                      x-amz-storage-class STANDARD | GLACIER | DEEP_ARCHIVE;
                                                      x-amz-server-side-encryption: AES256 and SSE-C headers)
GET    /<bucket>/<key>                GetObject      (Range; If-Match / If-None-Match / If-Modified-Since /
                                                      If-Unmodified-Since; response-content-disposition)
HEAD   /<bucket>/<key>                HeadObject     (from cache; all conditionals; Range → 206 / 416;
                                                      response-content-disposition)
DELETE /<bucket>/<key>                DeleteObject   (?versionId; delete marker on versioned buckets)
POST   /<bucket>?delete               DeleteObjects  (≤ 1 000 keys, quiet mode)
PUT    /<bucket>/<key> + x-amz-copy-source            CopyObject (metadata/tagging directives; whole SSE sources 501)
POST   /<bucket>/<key>?uploads        CreateMultipartUpload
PUT    /<bucket>/<key>?partNumber=&uploadId=          UploadPart (≥ 5 MB except the last)
PUT    /<bucket>/<key>?partNumber=&uploadId= + x-amz-copy-source
                                      UploadPartCopy (x-amz-copy-source-range bytes=first-last, ≤ 5 GiB;
                                                      ?versionId; x-amz-copy-source-if-*; SSE-C sources 501)
POST   /<bucket>/<key>?uploadId=      CompleteMultipartUpload
DELETE /<bucket>/<key>?uploadId=      AbortMultipartUpload
POST   /<bucket>/<key>?restore        RestoreObject  (GLACIER-class objects; 202 / 409 RestoreAlreadyInProgress)
GET|PUT|DELETE /<bucket>/<key>?tagging              object tags (≤ 10)
GET|PUT|DELETE /<bucket>/<key>?retention ?legal-hold  Object Lock
GET|PUT        /<bucket>?versioning ?object-lock ?cors ?notification ?logging ?acl ?tagging ?inventory
```

Behaviour worth knowing before writing code against it:

- **Storage classes are the tiers.** No header or `STANDARD` = the Standard
  floor (hot). `GLACIER` / `DEEP_ARCHIVE` = the Vault floor (tape): a GET of an
  archived object answers 403 `InvalidObjectState` until `?restore` completes
  (usually a few minutes; allow up to 12 hours). Listings and HEAD report the class the object actually
  has. Any other class in `x-amz-storage-class` — `REDUCED_REDUNDANCY`,
  `STANDARD_IA`, `ONEZONE_IA`, `INTELLIGENT_TIERING` — is accepted and stored
  as the bucket's own tier (`STANDARD` on a plain bucket): we do not sell a
  single-copy or an infrequent-access class, and nothing is ever placed on the
  server's own disk.
- **Quota, not overage.** A PUT past the tenant's quota (per floor when the
  tenant bought a house, else the total) answers 403 `QuotaExceeded`; the free
  tier is 5 GB, 1 bucket, 1 scoped key. Nothing is ever billed per request or
  per byte transferred.
- **Object Lock** (`GOVERNANCE`, `COMPLIANCE`, legal hold) is enforced on
  every delete flavour, on overwrite, on multipart complete and on copy;
  refusals are 403 `AccessDenied` with "Object is protected by Object Lock."
  appended. `COMPLIANCE` retention can only be extended, and nothing bypasses
  it or a legal hold. `GOVERNANCE` retention is bypassed (delete, overwrite,
  shorten) only by a request that sends a **signed**
  `x-amz-bypass-governance-retention: true` with a key that may: a full-access
  key (`*`, e.g. the account's primary key) or a scoped key that lists the
  `BypassGovernanceRetention` permission. An STS token bypasses only when its
  request named that permission and its parent could; a presigned URL carries
  its signer's scope, and the flag counts only where the signature covers it.
  MFA Delete (`x-amz-mfa` = `<serial> <TOTP code>` of the account's second
  factor) is enabled and disabled through `PutBucketVersioning`, as on AWS:
  `MfaDelete=Enabled` needs versioning `Enabled` (in the same request's
  `Status` or already on the bucket; 409 `InvalidBucketState` otherwise)
  and the header; `MfaDelete=Disabled` needs the header too. On an MFA
  Delete bucket every delete needs the code — a single `DELETE` of any
  flavour, a `DeleteObjects` batch (one code for the whole batch; without
  it the batch is 403 `AccessDenied` and nothing is deleted) and a change
  of the versioning state. AWS gates only versioned deletes; ours is the
  stricter rule. A code is accepted once: the same `x-amz-mfa` on a second
  request inside the validator's window is 403. When the bucket's MFA
  Delete setting cannot be read (a database outage), a delete and a
  versioning change answer 503 `ServiceUnavailable` with `Retry-After` —
  never "MFA Delete off".
- **The bucket state machine** (as AWS's API): Object Lock goes on through
  `PutObjectLockConfiguration` only on a bucket whose versioning is
  `Enabled` (409 `InvalidBucketState` "Versioning must be 'Enabled' on the
  bucket to apply a Object Lock configuration" otherwise — turn versioning
  on first, as the AWS console does for you) and never off again; on a
  bucket with Object Lock, `PutBucketVersioning Suspended` is 409
  `InvalidBucketState` ("An Object Lock configuration is present on this
  bucket, so the versioning state cannot be changed."). Object-level
  retention and legal holds are enforced whatever the bucket's flags say.
- **Object Lock at creation.** `CreateBucket` with
  `x-amz-bucket-object-lock-enabled: true` (aws-cli
  `--object-lock-enabled-for-bucket`) creates the bucket with Object Lock on
  and versioning `Enabled`; a value other than `true`/`false` is 400
  `InvalidArgument`. Re-creating a bucket you own that has no Object Lock,
  with the header, is 409 `InvalidBucketState` (enable it with
  `PutObjectLockConfiguration`).
- **Versioning keeps every version's bytes** (since 2026-10-12, WP-R2-1):
  `GET` / `HEAD ?versionId=<old>` serve that version from its own row and
  blob, `DELETE ?versionId` removes it (deleting the current version makes
  the newest remaining one current). Retained versions count toward storage —
  `tenant_quotas`, the floor they were written on, the quota check of every
  write and the usage numbers. A non-current version written before
  2026-10-12 kept no bytes: `GET`/`HEAD ?versionId` of one answers **501**
  (the newest version behind a delete marker is the exception: its bytes
  were never overwritten). An object written before versioning was enabled
  is the key's `null` version: the first overwrite or delete marker keeps it
  as one (since 2026-10-13), and `?versionId=null` reads or deletes it.
  `DeleteObjects` on a versioned bucket writes a marker per key (no
  `VersionId`) or deletes the named version (`VersionId`), with
  `DeleteMarker` / `DeleteMarkerVersionId` / `VersionId` in its response.
  A chunked object (> 64 MiB) is a version like any other (since
  2026-10-14): one written before versioning was enabled is kept as the
  `null` version, and `?versionId` reads, copies and deletes chunked
  versions. A version keeps its own metadata and headers. `DELETE` with no
  `versionId` on a Suspended bucket replaces the `null` version with a
  `null` delete marker (its bytes go), and `HEAD` answers `x-amz-version-id`.
  CopyObject honours `x-amz-copy-source-if-match`, `-if-none-match`,
  `-if-modified-since` and `-if-unmodified-since`.
- **Regions**: a bucket's region is fixed at creation and must be one the
  deployment has a key pair for; otherwise 400 `InvalidLocationConstraint`.
  A region that is enabled but whose storage is not answering is refused
  with 503 `ServiceUnavailable` and `Retry-After: 60` until it answers again
  (the dashboard shows it as temporarily unavailable). The default region is
  always available.
  Vault objects go to the tape backend and public-read objects to the CDN
  origin regardless of the bucket's region.
- **Public buckets** (`?acl` public-read) are served unauthenticated from
  `cdn.stored.ge` with Range, conditionals, per-bucket CORS and a
  `Cache-Control` from the bucket's cache TTL; HTML/SVG/XML are always
  attachments. Chunked objects (> 64 MiB) are served through the content
  index, Range included (since 2026-10-13); SSE-S3 objects are opened; an
  SSE-C object answers 403 (its key cannot travel on a public link).
- **CORS on the S3 API** (2026-10-06): `PUT|GET|DELETE /<bucket>?cors` take and
  return the AWS `CORSConfiguration` document (≤ 100 rules; origins
  case-sensitive with at most one `*`; methods GET/PUT/HEAD/POST/DELETE;
  `ExposeHeader` without wildcards). A bucket has **no** configuration until you
  put one, and a bucket without a configuration never gets CORS headers on its
  responses: a page cannot read them. Bucket names are per account, and a
  browser's `OPTIONS` preflight is answered **before** SigV4 (a browser never
  signs it, and a path-style URL names no account), so it is answered from the
  rules of **any** bucket of that name — 200 with the matching rule's headers,
  403 `AccessForbidden` otherwise (the same answer for "no such bucket" and "no
  rule allows it"). The preflight only lets the browser *send* the real
  request. What it may *read* is decided by **your** bucket: the signed
  response carries the headers of the first rule of your own bucket that allows
  the origin and method — `Access-Control-Allow-Origin`,
  `Access-Control-Allow-Methods`, `Access-Control-Expose-Headers` (list `ETag`
  to read multipart part ETags from a page) and
  `Access-Control-Allow-Credentials` (when the rule names the origin rather
  than `*`) — on success and on every error the operation returns. A response
  written before the key is known (a bad signature, an unknown, expired or
  IP-refused key) carries `Access-Control-Allow-Origin` alone, when a rule of
  some bucket of that name allows the origin, so the page can read the error.
  Every S3 response to a request with an `Origin` carries `Vary: Origin`. Use
  `s3.stored.ge` from a page for uploads above 100 MB (the `stored.ge` hostname
  is proxied and capped).
  The `/cdn` path's CORS (`cors_origins`, default `*`) is separate and unchanged.
- **`GET /api/v1/whoami`** (2026-10-06): signed with an S3 key pair (SigV4,
  header or presigned), not a JWT — answers the tenant the key belongs to, the
  key id, whether it is the account's **primary** pair, a **scoped** key or a
  temporary **sts** token, its permissions, bucket scope, IP allowlist and
  expiry, never a secret. For an edge gateway or an app that holds only a
  customer's key and needs to know the tenant. `key_id` is the key the
  signature was verified with. Refusals use the JSON error envelope of the
  management API (400 `invalid_request` when a request carries both an
  `Authorization` header and presigned `X-Amz-*` query parameters — one
  mechanism per request, as on AWS; 401 `invalid_credentials` / `key_revoked` /
  `key_expired` — an expired STS token included, on both paths; 403
  `ip_denied` / `account_suspended`). Rate-limited per tenant like the JSON API.
- **Encryption**: SSE-C works per request at any size, sealed as it streams
  and a `Range` read by segment (since 2026-10-13, WP-R8-5 Part B; it was
  capped at 256 MiB). **SSE-C multipart uploads** (since 2026-10-20,
  WP-SSEC-1): `CreateMultipartUpload` and every `UploadPart` /
  `UploadPartCopy` carry the key headers (a part with another key, or
  without one, is 400 `InvalidArgument` and nothing is staged);
  `CompleteMultipartUpload` carries none, as aws-cli and the SDKs send it —
  each part is sealed under your key the moment it arrives and Complete
  concatenates the sealed parts, so the server never needs the key again.
  The object reads whole and by `Range` with the key (a wrong key 403, none
  400), `HEAD`/`GET` answer `x-amz-server-side-encryption-customer-algorithm`
  and `-key-MD5`, the ETag is the usual `<md5-of-md5s>-N`. **Copies with
  keys**: an SSE-C source is opened with
  `x-amz-copy-source-server-side-encryption-customer-*` (none 400, a wrong
  key 403), an SSE-C destination sealed under the request's
  `x-amz-server-side-encryption-customer-*` — `CopyObject` and
  `UploadPartCopy`, on either side; copying an object onto its own key with a
  new key (`x-amz-metadata-directive: REPLACE`) is the key rotation. SSE-C
  bodies are never chunked and never deduplicated. **A bucket can require a key** (the
  `ssec_required` setting: `PATCH /api/v1/manage/buckets/{name}` with
  `{"ssec_required": true}`, or the dashboard's bucket settings): an unkeyed
  `PutObject`, `CreateMultipartUpload` or copy into it is 400 `InvalidRequest`
  ("Requests specifying Server Side Encryption with Customer provided keys
  must provide an appropriate secret key.", the sentence AWS's bucket-policy
  condition answers), and the bucket cannot receive server-generated objects
  (access logs, inventory reports — refused when the target is set, 409).
  Objects written before the flag stay as they were; a public bucket cannot
  require a key. We never store your key — it passes through our servers on
  every request; names, sizes, timestamps and access logs stay plaintext. SSE-S3 requires the deployment's master
  key, which production does not set today — without it a write into an
  `sse_enabled` bucket, or one that sends `x-amz-server-side-encryption:
  AES256`, is refused 503 + `Retry-After` (CreateMultipartUpload and
  CompleteMultipartUpload too) rather than stored plaintext. With the key,
  objects over 64 MiB are encrypted chunk by chunk where they can be chunked,
  and every other object is sealed whole **at any size** — sealed as it
  streams, 1 MiB at a time (since 2026-10-13, WP-R8-5; over 256 MiB it was a
  413 before) — **a multipart upload exactly like a PUT of the same bytes**,
  the header sent on CreateMultipartUpload included. A `Range` of a sealed
  object reads only the segments it spans; a sealed object can be copied
  (it was 501).
- **HEAD, GET and a ranged GET carry the same object headers** (since
  2026-10-19, WP-R2-4): `ETag`, `Last-Modified`, `Content-Type`,
  `Accept-Ranges: bytes`, `x-amz-storage-class`, `x-amz-version-id` (on a
  versioned bucket; a bucket never versioned names none), `x-amz-meta-*`,
  `Content-Disposition` / `-Encoding` / `-Language`, `Cache-Control` (the
  stored one, else `private, no-cache`), `Expires`,
  `x-amz-website-redirect-location`, `x-amz-tagging-count` and the
  encryption header. `Content-Length` is the object's on HEAD and 200 and the
  range's on 206, which adds `Content-Range`. `HEAD` with a `Range` answers
  as AWS does: 206 with the range's length, 416 + `Content-Range: bytes
  */<size>` when nothing satisfies it (any range of an empty object, on GET
  too). A 304 carries `ETag`, `Last-Modified`, `Cache-Control` and `Expires`.
  `GET` / `HEAD ?versionId=` of a **delete marker** answers 405
  `MethodNotAllowed` + `x-amz-delete-marker: true`. An SSE-C object read
  without its key answers **400 `InvalidRequest`** ("The object was stored
  using a form of Server Side Encryption…") with no object header; a GET with
  a wrong key answers 403.
- **Deviations from AWS** (each deliberate):

  | Request | AWS | Stored | Why |
  |---|---|---|---|
  | `HEAD` of an SSE-C object with a **wrong** key, object written before 2026-10-19 | 403 | 200 | HEAD answers from the metadata index and never reads the object (~1 ms), and such rows keep no key digest. Objects written since 2026-10-19 keep the key's MD5 (as AWS does) and HEAD and GET with a wrong key both answer 403 from the row, before any read. A GET with a wrong key answers 403 either way |
  | `GET` of a restored archive object | `x-amz-restore` | not sent (HEAD sends it) | the restore state lives on the tape backend; HEAD asks it, a GET does not ask twice |
  | a `Range`, or a `-N` object written before 2026-10-19 | — | the body's size is checked, not its MD5 | only a whole body hashes to a digest the row holds: a single-part object's ETag, or the whole-body MD5 recorded since 2026-10-19 for a multipart object completed whole — a whole GET of either is checked as it streams and cut on a mismatch |

- **Large uploads** (> 64 MiB, a single PUT or a multipart upload): stored as
  content-defined chunks, deduplicated and compressed, unless the bucket is
  versioned, pinned to a region, or of the archive / resilient / public /
  Sync tier. A multipart object keeps the `<md5-of-part-md5s>-<N>` ETag
  aws-cli computes. A copy of a chunked object into a bucket that cannot hold
  chunks is stored whole there (fresh MD5 ETag); a copy into one that can
  shares the chunks and keeps the source's ETag.

## JSON API

Base `https://stored.ge/api/v1`, `Authorization: Bearer <jwt>`, JSON in and
out. One token-bucket limiter of **100 requests/min per tenant** (burst 10)
covers every group; responses carry `X-RateLimit-Limit`, `-Remaining`,
`-Reset` and a 429 carries `Retry-After`.

```
Management  /api/v1/manage
  GET/POST /buckets · GET/PATCH/DELETE /buckets/{name} · GET /buckets/{name}/objects
  PUT /buckets/{name}/tier {"tier":"auto|performance|standard|archive|resilient"} · PUT /buckets/{name}/residency (deprecated: re-asserts the region's label us|eu|uk; another is 409 residency_is_the_region — RP86-2)
  GET/POST /keys · DELETE /keys/{id} · GET /usage
  POST /account/export (202 + id; the export is rendered by a job into the account's private `_exports` system bucket, kept 7 days) · GET /account/export/{id} (status; when completed a 1 h presigned download URL; 410 once expired) · DELETE /account (30-day grace) · POST /account/cancel-deletion
Account     /api/v1/user
  GET / · GET /quota · GET /quota/history · GET /usage · GET /usage/alerts · GET /presigned
  GET/POST /apikeys · DELETE /apikeys/{id} · POST /apikeys/{id}/rotate · POST /apikeys/{id}/expire · GET /apikeys/audit
Webhooks    GET/POST /api/v1/webhooks · PATCH/DELETE /webhooks/{id} · GET /webhooks/{id}/deliveries · POST /webhooks/{id}/test · POST /webhooks/{id}/rotate-secret
Events      GET /api/v1/events   (object.created/deleted/downloaded, bucket.*, key.*, sts.token_created, webhook.test)
STS         POST /api/v1/sts/token {"duration_seconds","permissions","bucket_scope","parent_key_id"}
Admin       /api/v1/admin/*  (admin role: audit, flags, quota-reconcile; jobs = the background jobs' state,
            POST jobs/{job}/run or dedup-gc | smart-demotion | retention | account-deletion → 202, the run continues;
            GET routing-truth = head rows vs. the backends that hold their bytes, POST routing-truth/resolve-null)
```

Conventions (Stripe-style):

- single object: `{"object":"bucket", …, "request_id":"…"}`
- list: `{"object":"list","data":[…],"has_more":bool,"next_cursor":"…"}` with `?limit=&starting_after=`
- error: `{"error":{"type":"invalid_request_error|authentication_error|permission_error|not_found_error|conflict_error|rate_limit_error|gone_error|unavailable_error|api_error","code":"…","message":"…","param":"…","request_id":"…"}}`
- `Idempotency-Key` on PUT/POST/DELETE: the 2xx response is cached 24 h, per
  member of the account (one member's key never replays another's answer),
  and replayed with `Idempotent-Replayed: true` (and the older
  `Idempotency-Replayed: true`); reusing a key on a different
  method/path is 409 `idempotency_key_reuse`. **A replay never carries a
  secret:** a retried key create or rotate, STS mint or webhook secret is
  answered with the secret blanked and `"secret_redacted": true` — revoke that
  key and create another
- webhooks are signed `X-Webhook-Signature: t=<unix>,v1=<hex>`
  with the `whsec_` secret shown once at creation (and by
  `POST /webhooks/{id}/rotate-secret`, which replaces it at once — retries of
  earlier events are signed with the new one): `v1` is HMAC-SHA256 over
  `<t>.<raw body>`; verify it and refuse a `t` more than **5 minutes** from
  your clock (a replayed delivery). Split the header on `,` and read the
  elements by name rather than comparing it whole. The old body-only
  `sha256=<hex>` element (sent for one release while receivers moved to
  `v1`) is no longer sent since 2026-10-20
- a delivery that does not answer 2xx (an error status, a redirect, a
  timeout after 10 s, a connection failure) is retried — up to **5 attempts**,
  1 s, 5 s, 25 s and 125 s apart (±20 %, picked up within 30 s); then the
  event is given up for that endpoint. An attempt can arrive twice:
  deduplicate on `X-Event-ID`. `GET /webhooks/{id}/deliveries` lists every
  attempt with its `retry_count` (0 = first) and `next_retry_at`; targets may
  not be private or loopback addresses
- every response carries `X-Request-Id` and `X-Vaultaire-Version` (a date)

The management bucket create applies the same rules as S3 CreateBucket
(idempotent re-create, free-tier cap, region validation, the reserved names
below).

## Reserved bucket names

A deviation from AWS, which reserves nothing: a bucket may not be NAMED
after a path of this service's web host. CreateBucket of such a name is
400 `InvalidBucketName` with a message naming the reservation (the
management API: 400 `invalid_bucket_name`; the dashboard form says so).
The list is derived from the server's own routes — every first path
segment registered ahead of the S3 catch-all — so it follows the site:
`api`, `docs`, `legal`, `status`, `login`, `register`, `changelog`, `abuse`,
`dashboard`, `auth`, `cdn`, `translog`, `health`, `ready`, `version`,
`metrics`, `llms.txt`, `robots.txt`, `sitemap.xml`, `favicon.ico`,
`og.png`, `openapi.json`, `static`, `pricing`, `signup`, `compliance`,
`verify`, `logout`, `forgot-password`, `reset-password` as of 2026-10-25.
It applies at CREATE only: a bucket that already exists under such a name
keeps every operation. Why: a request carrying S3 credentials is the S3
API's on every host (the credential gate), so a signed GetObject of bucket
`legal` can no longer get a page — what remained was the reverse, a bucket
whose name is a page (an edge cache keyed by extension once served
`/favicon.ico` to a signed GET of bucket `favicon.ico`). Requests on a
`cdn.*` host serve only `/{slug}/{bucket}/*` public bytes and never a page,
so the names are not reserved there — nothing to shadow. The one aliasing
left is `GET /api/v1/whoami`, which verifies its own SigV4 signature.

## Errors

S3 errors are XML with the AWS shape; `RequestId` is the id to quote to
support:

```xml
<Error>
  <Code>NoSuchKey</Code>
  <Message>The specified key does not exist</Message>
  <Resource>/my-bucket/missing.txt</Resource>
  <RequestId>…</RequestId>
</Error>
```

Names are checked before anything else: a bucket name that is not a valid
S3 name (3–63 characters, lower-case letters, digits, `.` and `-`, no `..`)
is 400 `InvalidBucketName` and a key over 1,024 bytes 400 `KeyTooLongError`,
on every operation. A write takes keys up to 857 bytes (the rest is the room
the service's own addressing needs); a `DeleteObjects` key over 1,024 bytes
is that key's error, the other keys are deleted.

Some messages carry a suggestion after the standard text (closest bucket or
key name within the tenant, the auth header that was malformed, the lock that
refused a delete). The full code list from `internal/api/s3_errors.go`:

| HTTP | Codes |
|------|-------|
| 400 | `InvalidBucketName`, `KeyTooLongError`, `InvalidObjectName`, `InvalidRequest`, `IncompleteBody`, `BadDigest`, `InvalidDigest`, `MalformedXML`, `XAmzContentSHA256Mismatch`, `InvalidArgument`, `InvalidPart`, `InvalidPartOrder`, `EntityTooSmall`, `InvalidPartNumber`, `AccessControlListNotSupported`, `InvalidRetentionPeriod`, `AuthorizationQueryParametersError`, `AuthorizationQueryParametersError_Expires`, `InvalidLocationConstraint`, `InvalidTag` |
| 403 | `AccessDenied`, `SignatureDoesNotMatch`, `RequestTimeTooSkewed`, `AccountSuspended`, `ExpiredToken`, `QuotaExceeded`, `InvalidObjectState` |
| 404 | `NoSuchBucket`, `NoSuchKey`, `NoSuchUpload`, `NoSuchVersion` |
| 405 | `MethodNotAllowed` |
| 408 | `RequestTimeout` |
| 409 | `BucketAlreadyExists`, `BucketNotEmpty`, `InvalidBucketState`, `RestoreAlreadyInProgress` |
| 411 | `MissingContentLength` |
| 412 | `PreconditionFailed` |
| 413 | `EntityTooLarge` |
| 500 | `InternalError` |
| 501 | `NotImplemented` |
| 503 | `ServiceUnavailable` (every backend unreachable, or CreateBucket in a region whose storage is down; carries `Retry-After`), `SlowDown` (+ `Retry-After`: the egress stream guard, see Rate limits) |

An object miss on any backend is 404 `NoSuchKey`; an unreachable backend is
never reported as a miss — it is 503, so sync clients do not treat the object
as deleted. Lock refusals use `AccessDenied` (the AWS wire code), not an
invented code.

## Request log

Every S3 request of your account from the last 7 days, newest first. It is
on the dashboard's **Developers** page and at
`GET /api/v1/manage/requests` (Bearer JWT). Each row has:

- the time and the access key id that signed the request (never the secret)
- the operation, bucket and key
- the HTTP status and the S3 error code, with what to do about that code
  (`suggestion`)
- the **request id the client was answered**: the `RequestId` of an error
  body, the id a support ticket quotes
- the body bytes in and out
- what served the request: `origin`, or the edge host

It never holds a body, a header, an IP address, a user agent, an SSE-C key
or any secret. Unlike the S3 access log (per bucket, opt-in, 30 days), it
covers every bucket and is always on.

| Query parameter | |
|---|---|
| `key` | only requests signed with this access key id |
| `since` | only requests at or after this time (RFC 3339) |
| `status` | an exact status (`403`), a class (`4xx`, `5xx`), or `error` (400 and above) |
| `request_id` | the one request answered with this id |
| `cursor`, `limit` | keyset paging: pass `next_cursor` back as `cursor`; `limit` 1–200, default 50 |

```bash
curl -H "Authorization: Bearer $TOKEN" "https://stored.cloud/api/v1/manage/requests?status=error&limit=20"
```

## Test mode

A **test key** works like any key but writes to the account's **sandbox**, never its live data.

**Making one.** `POST /api/v1/manage/keys` with `"test": true` makes a test key. Its access key id starts with `VLT_TEST_`, and the answer says `"livemode": false`. The sandbox is made with the first test key. An account may hold 10 live test keys, and they don't count against the plan's key limit.

**What a test key does:**
- It has the same S3 API on every host.
- Every object it writes is stored on a local disk (never a paid backend), whole and as received: never chunked, sealed, mirrored or tiered.
- Its requests never go through the edge: the origin serves them.
- It is never billed. The sandbox's own counter is its cap, the free tier's 5 GiB.
- Its downloads are metered under the sandbox, against a fixed 5 GiB a month; past it they are slowed to the throttle's floor, as any account's past its allowance. Nothing reaches the account's own counters.
- Its presigned links live at most one hour (`X-Amz-Expires` ≤ 3600, else 400 `AuthorizationQueryParametersError`), and a test bucket is never public (a create with `x-amz-acl: public-read` is 400 `InvalidRequest`).
- It stops working while the account is suspended (403 `AccountSuspended`), and no test key is made for a suspended account (403 `account_suspended`).
- Every S3 response carries `x-stored-mode: test`.

**Clearing it.** Test data is deleted automatically once its last write is 7 days old. `POST /api/v1/manage/sandbox/reset` empties the sandbox now (objects, versions, uploads and buckets); the test keys stay. An account's sandbox is erased with the account.

**Webhooks.** An event caused by a test key goes to the account's own webhooks with `"livemode": false` in its body. `stored listen --forward-to localhost:<port>/<path>` replays the account's events to a local endpoint, each once. The event log's forward cursor (`GET /api/v1/events?after=<id>&wait=<s>`) answers an event once it is 2 seconds old, so a slower commit is never skipped; at most 4 waits of one account at once (the fifth is 429 `too_many_listeners`).

**The request log.** `GET /api/v1/manage/requests?mode=test` (and the Developers page's **Test** toggle) lists the test keys' requests; the default is the live ones.

**Elsewhere.** The dashboard's overview shows a **Test data** card once a sandbox exists. To run Stored entirely on your own machine, see `docs/guides/local.md`.

## Platform

A **platform** account creates and runs accounts of its own — **child tenants** — by API, for the users of an app it builds on Stored (the Connect primitive). An account becomes a platform only when an operator grants it; to every other account the routes below do not exist (404).

**A child** is an account with no user, no e-mail and no primary key pair. It has a `label` and, optionally, an `external_id` the app owns (unique per platform). Its buckets default to the same region as the platform's.

**The tree.** A child hangs off its platform by a `kind`:
- `managed` (the default): the platform's customer;
- `app`: an app built on the platform, which may have `managed` children of its own. The platform makes them with `parent_id` set to the app. This is depth 2, the only depth beyond 1.

A child of a managed child, an app under an app or anything deeper is 400 `child_of_child`, and `kind: "sponsored"` is 400 `invalid_kind` (a family's edge is made by an invitation). The platform reaches its whole tree: every route below takes a grandchild's id as well as a child's.

| Route | What |
|---|---|
| `POST /api/v1/platform/tenants` `{label, external_id?, kind?, parent_id?, storage_cap_bytes?, egress_cap_bytes?, residency?}` | Make a child (201 `{object: "tenant", id, label, external_id, kind, parent_id, caps, residency, created_at, usage}`); 409 `external_id_taken`; 404 `parent_not_found` |
| `GET /api/v1/platform/tenants` | The platform's children, by id (`limit`, `starting_after`), each with `usage.storage_used_bytes` |
| `GET /api/v1/platform/tenants/{id}` | One child |
| `PATCH /api/v1/platform/tenants/{id}` `{storage_cap_bytes?, egress_cap_bytes?}` | Set its caps inside the pool (0 = the pool decides); 400 `invalid_cap` |
| `DELETE /api/v1/platform/tenants/{id}` | Schedule the child's erasure: its objects, keys and rows go after the usual 30 days |
| `POST /api/v1/platform/tenants/{id}/keys` | Mint a scoped key of the child; the secret is shown once |
| `GET /api/v1/platform/tenants/{id}/keys` | The child's keys (never a secret) |
| `DELETE /api/v1/platform/tenants/{id}/keys/{key}` | Revoke one (its temporary credentials die with it) |
| `POST /api/v1/platform/tenants/{id}/sts` `{parent_key_id, permissions?, bucket_scope?, ip_restrict?, ttl?}` | A temporary credential of the child, bounded by one of its keys |

Every call is a Bearer JWT of the platform, in its request log, rate limited and audited (`platform.*`).

**A child's keys never exceed the credential that mints them.** What a request leaves unset is the caller's (permissions, buckets, IP allowlist, expiry); asking more is 400 `scope_exceeds_parent`. The JWT is the account's own authority. A child holds at most 50 live keys.

**The pool.** A child's bytes count against its platform's quota: the platform's limit, against the bytes the whole tree stores (the platform's own and every child's, at any depth). When the pool is spent, a write by any of them — a child's or the platform's own, through any host — is 403 `QuotaExceeded`. `usage.storage_used_bytes` is each child's own share. A platform's test-mode sandbox is never in the pool.

**Caps inside the pool.** A child may carry its own caps (`caps`):
- `storage_cap_bytes` > 0: it may store at most min(cap, what the pool has left). One byte past its cap is 403 `QuotaExceeded`, whatever room the pool has.
- `egress_cap_bytes` > 0: past that many bytes downloaded this UTC month, its downloads are rate-limited while the others' are not. Past the pool's allowance, every member is.
- 0 = the pool decides.

**Residency.** `residency` (`eu`, `uk` or `us`) keeps the child's bytes at rest in that jurisdiction:
- **Buckets:** they can be created only in a region of it (S3 400 `InvalidLocationConstraint`; management 400 `region_outside_residency`), and a create that names no region gets the first one inside it. London (`eu-west-3`) is the UK.
- **Second copies:** the mirror copy and the Vault parity copy stay inside it. Where there is no second leg inside it, the object has one copy.
- **Inheritance:** a child keeps its parent's residency. Naming another is 400 `residency_widens`. A parent with none lets the child choose.
- **Once set,** it changes only by an operator, and only while every bucket complies.

Bytes in transit through an edge location are not storage: residency covers where they are kept.

**Suspension and erasure.** A suspended platform's children (and its apps' children) answer 403 `AccountSuspended`. When a platform's account is erased, its children are erased first (objects, keys and rows; an app's children before the app), then the platform. A platform cannot be turned off while it has children.

**Operator:**
- `PUT /api/v1/admin/tenants/{id}/platform` `{"platform": true|false}` (admin JWT; audited `admin.platform_granted` / `admin.platform_revoked`).
- `PUT /api/v1/admin/tenants/{id}/residency` `{"residency": "eu"|"uk"|"us"|null}`: for the tenant and its tree, only while every bucket of the tree complies (409 `buckets_outside_residency`, `children_outside_residency`; audited `admin.residency_set`). The hidden `_exports` system bucket never blocks it while it holds no export: it moves to the first region inside in the same transaction (a live export is 409 until it expires, 7 days). Under a residency the GDPR export is written inside it; with no enabled region inside, the export fails (`no_region_inside`) rather than be written outside (RP86-1).

## Members

An account can have several **members**: people who sign in with their own e-mail and password (and their own MFA) and act on the account's buckets and keys, each with a **role**. A team is one shared account with members. Nothing about the account's data changes: S3 keys stay the account's, and the bytes and the bill are the same whoever made them.

| Role | May |
|---|---|
| `viewer` | read: buckets, objects' listings, keys (never a secret), usage, the request log, the members |
| `developer` | also make and revoke keys and temporary credentials, manage webhooks, make, change and delete buckets, reset the test-mode sandbox, and — on a platform — mint and revoke a child's keys and temporary credentials |
| `admin` | also invite, change the role of and remove members (never the owner), and make or erase a platform's children |
| `owner` | everything: also delete the account, export it, and change billing. One per account: the person who registered it. The owner cannot be removed or given another role |

A write the role does not allow is 403 `role_forbidden`. A JWT whose user is no longer a member of its account is 403 `not_a_member` on every route of `/api/v1/manage`, `/api/v1/platform`, `/api/v1/user`, `/api/v1/webhooks`, `/api/v1/events` and `/api/v1/sts`: the role is read on every request, never from the token alone.

| Route | Role | What |
|---|---|---|
| `GET /api/v1/manage/members` | any | The members (`user_id`, `email`, `role`, `invited_by`, `accepted_at`) and the pending `invitations` |
| `POST /api/v1/manage/members` `{email, role}` | admin | Invite an address (`admin`, `developer` or `viewer`): it receives a link that works **once** and expires after **7 days**. Inviting an address again **replaces** its pending invitation (the first link stops working): that is the resend. An account holds at most **20 pending invitations** and sends at most **50 invitation e-mails a day** (429 `invitation_rate_limited`). 409 `already_a_member`; 400 `invalid_role` |
| `PATCH /api/v1/manage/members/{user_id}` `{role}` | admin | Change a member's role. 409 `owner_is_permanent` for the owner |
| `DELETE /api/v1/manage/members/{user_id}` `{revoke_keys?}` | admin, or any member for itself (leaving) | Remove a member: its membership only — never its sign-in, never a bucket. The answer names the S3 keys the member made (`keys_made: [{id, name, last_used}]`): they are the account's and **stay live** unless the body says `"revoke_keys": true`, which revokes them and their temporary credentials in the same step (`keys_revoked: true`). 409 `owner_is_permanent` |
| `DELETE /api/v1/manage/invitations/{id}` | admin | Revoke a pending invitation |
| `POST /api/v1/manage/invitations/{token}/accept` | any signed-in user | Accept an invitation sent to the token's address (403 `invitation_other_address` for another one); the answer's `role` is the one the person has. 410 `invitation_expired` / `invitation_used` |

**The link.** An invitation is e-mailed as `https://<host>/auth/invitations/<token>`. Only the token's SHA-256 is stored. An address without an account chooses a password on that page and becomes a member — a sign-in with no account of its own; an address that has an account signs in and accepts from the dashboard. Accepting is open while public sign-ups are closed: it adds a person to an account that exists.

**A sign-in on no account.** A person who is a member of no account (an invitee who left, or was removed) signs in to a JWT on no account: it has no role, and every route above but the accept is 403 `not_a_member`. It may ask for its own erasure: `DELETE /api/v1/user/signin` `{reason?}` schedules it (the usual 30 days), then its sign-in rows go and no account is touched (keys it once made stay the account's). Anyone who owns an account or is a member of one is 409 `still_a_member`: leave first (`DELETE /api/v1/manage/members/<your user_id>`; the owner cannot leave its own account).

**Several accounts.** A person may be a member of several accounts. `POST /auth/login` answers `tenants: [{id, role, name}]` and takes an optional `tenant_id` (default: the account the person owns, else the first joined; 403 `not_a_member` for one they are not in). The JWT is for that one account — its `tenant_id` and `role` claims — and every management route acts on it alone; signing in again with another `tenant_id` switches. The dashboard's switcher (Members page) does the same for the session.

Every change is in the audit trail (`member.invited`, `member.joined`, `member.role_changed`, `member.removed`, `member.invitation_revoked`), and every audit row of a tenant surface (the management, platform, user, webhook and STS APIs) and the dashboard names the member who acted and the account acted on.

## Rate limits

| Surface | Limit |
|---------|-------|
| S3 API | no request-rate limit. **Egress allowance** (WP-R10-9): each UTC month an account may download 2 × its Standard quota + 1 × its Vault quota (`GET /api/v1/user/usage` → `egress_allowance`, `egress_used`, `egress_throttled`, `egress_rate_limit_bytes_per_sec`, `egress_resets_at`). Past it, GetObject bodies (header-signed and presigned) are rate-limited — one token bucket per account, shared with `/cdn` — and never billed; uploads, listings, HEAD and errors are not slowed. While rate-limited, more than 16 concurrent downloads are answered 503 `SlowDown` + `Retry-After: 60`. Enforcement is the `egress_throttle` flag (off until launch) |
| CDN | Egress counts on the bucket owner's allowance and shares its rate limit; while the owner is rate-limited, more than 16 concurrent `/cdn` downloads answer 429 + `Retry-After: 60`. Request rate: 100 requests/s, burst 200, **per bucket** (keyed `cdn:<slug>:<bucket>`, not per client); the refusal is currently a 404 — WP-R14-1 moves it to per-IP + 429 |
| JSON API (`/api/v1/*`) | 100 requests/min per tenant, burst 10, `X-RateLimit-*` + `Retry-After` |
| `/auth/login`, password reset | 5/min per IP; `/auth/register` 10/min per IP; `/api/waitlist` 10/hour per IP |
| Email | 10 messages/min per recipient |

## SDK snippets

All examples use the primary key; a `VLT_` scoped key or `ASIA` STS pair
drops in the same way.

**aws-cli**

```bash
aws configure set aws_access_key_id VKxxxxxxxxxxxxxxxx
aws configure set aws_secret_access_key SKxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
aws configure set region us-east-1
aws --endpoint-url https://stored.ge s3 mb s3://my-bucket
aws --endpoint-url https://stored.ge s3 cp ./photo.jpg s3://my-bucket/photo.jpg
aws --endpoint-url https://stored.ge s3api put-object --bucket my-bucket --key archive.tar --body archive.tar --storage-class GLACIER
```

**boto3**

```python
import boto3
s3 = boto3.client(
    "s3",
    endpoint_url="https://stored.ge",
    aws_access_key_id="VKxxxxxxxxxxxxxxxx",
    aws_secret_access_key="SKxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
    region_name="us-east-1",
    config=boto3.session.Config(s3={"addressing_style": "path"}),
)
s3.upload_file("photo.jpg", "my-bucket", "photo.jpg")
url = s3.generate_presigned_url("get_object", Params={"Bucket": "my-bucket", "Key": "photo.jpg"}, ExpiresIn=3600)
```

**aws-sdk-go-v2**

```go
cfg, _ := config.LoadDefaultConfig(ctx,
    config.WithRegion("us-east-1"),
    config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
        "VKxxxxxxxxxxxxxxxx", "SKxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", "")),
)
client := s3.NewFromConfig(cfg, func(o *s3.Options) {
    o.BaseEndpoint = aws.String("https://stored.ge")
    o.UsePathStyle = true
})
_, err := client.PutObject(ctx, &s3.PutObjectInput{
    Bucket: aws.String("my-bucket"), Key: aws.String("photo.jpg"), Body: f,
})
```

**rclone** (`~/.config/rclone/rclone.conf`; the full guide is
`https://stored.ge/docs/rclone`, source `docs/guides/rclone-setup.md`)

```ini
[storedge]
type = s3
provider = Other
access_key_id = VKxxxxxxxxxxxxxxxx
secret_access_key = SKxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
region = us-east-1
endpoint = https://stored.ge
acl = private
```

Through `s3.stored.cloud` keep parts at 64 MiB: `rclone copy … --s3-upload-cutoff
64M --s3-chunk-size 64M` (or `upload_cutoff = 64M` / `chunk_size = 64M` in the
remote). rclone's default cutoff is 200 MiB, so a 150 MB file would go as one
PUT and get the zone's HTML 413 — the 100 MB request limit is Cloudflare's,
answered before our Worker runs. aws-cli's defaults need nothing: aws-cli v2
(2.23+) sends its body `aws-chunked` with a checksum trailer (CRC64NVME in
2.28 and 2.35, or CRC32), and the edge moves it as it is. iDrive verifies the
trailer; a multipart part goes through the edge on every backend, and a
single `PutObject` sent `aws-chunked` to an iDrive bucket is moved by our
origin. On Lyve the trailer checksum is not verified by the backend; send
Content-MD5 or use iDrive for a verified write until the codec path carries
it. Nor does Lyve enforce `x-amz-decoded-content-length`: the edge checks
the stored size against it when the write is committed (a `PutObject`) or
completed (a multipart upload), and a mismatch is `400 IncompleteBody`.
Through the edge every part of a multipart upload is a real part on the
backend, so S3's minimum applies: every part but the last must be at least
5 MiB, or `CompleteMultipartUpload` answers `400 EntityTooSmall` (the upload
stays open: upload the part again and complete). aws-cli, rclone and the
SDKs never send a smaller part.

What to expect through the edge: one stream 35–50 MB/s; the client's number of
parts in flight sets the rest (see "Fast uploads and downloads" below).
Integrity through the edge is the backend's
own plus your `Content-MD5` when you send one (signed into the link the
backend enforces); a hex `x-amz-content-sha256` is verified on the
`stored.ge` path only.

Reads go the same way (WP-EDGE-1 Part B): a `GetObject` — whole, or one
`Range` — of an object of at least 4 MiB that is a whole unsealed object on
iDrive or Lyve is streamed to you from the backend by the edge, under the
same response headers `stored.ge` sends (ETag, Content-Type, Content-Length,
Content-Range, `x-amz-storage-class`, `x-amz-meta-*`, the version id, the
Object Lock headers); `aws s3 cp`'s `If-Match` + `Range: bytes=0-` download
shape is served that way too. Everything else — HEAD, a presigned URL,
`?versionId`, a public bucket, a sealed (SSE-S3 / SSE-C) or chunked object,
an object on tape, a multi-range or unsatisfiable `Range`, `If-None-Match`
and the `If-*-Since` headers, a read while your account is past its egress
allowance with the throttle on — is answered by the origin as on
`stored.ge`, and so is any read the backend does not answer as expected
(the origin keeps the failover path). A read of 64 MiB or more is read
by the edge as 16 MiB ranges, six at a time, and written to you in order
(about twice one stream in the 2026-10-27 bench); a range that fails after
bytes have reached you cuts the response, which your client retries. The
egress is metered once, for the bytes you received.

Also known to work unmodified: restic, JuiceFS (`docs/guides/juicefs-setup.md`),
Cyberduck, Nextcloud's S3 object store, Immich.

## Fast uploads and downloads

Measured 2026-10-27 (`bench-results/SPEED-2026-10-27.md`), from a 10 Gbps
host in Salt Lake City through `s3.stored.cloud`. A home or office line is
its own limit long before these numbers are.

| | One stream | One client, many parts in flight |
|---|---|---|
| Upload through the edge | 35–50 MB/s | ~370 MB/s at 8 parts, ~640 at 16, 840–980 at 32 |
| Download through the edge | 50–110 MB/s; a GET of 64 MiB or more ~2× that (the edge reads it as 16 MiB ranges, 6 in flight) | run several ranged GETs for more |

Speed comes from parts in flight, not from the part size or the
protocol. HTTP/3 was slower than HTTP/1.1 and HTTP/2 in the same test, so
keep your client's default. Parts must stay under 100 MB each: the
Cloudflare request limit answers a larger single request with an HTML
413 before it reaches us.

**aws-cli** (`~/.aws/config`, the profile you use with Stored):

```ini
[profile stored]
s3 =
  multipart_threshold = 64MB
  multipart_chunksize = 64MB
  max_concurrent_requests = 16
```

**rclone**: `--s3-upload-cutoff 64M --s3-chunk-size 64M
--s3-upload-concurrency 16`, plus `--transfers 4` to `8` when you copy many
files. Never set an upload cutoff above 95M (see the 413 above).

**Large downloads**: `aws s3 cp` already reads big objects as parallel
ranged GETs (`max_concurrent_requests` applies); rclone splits a large
download to a local disk into `--multi-thread-streams` ranges (default 4),
and 8 helps for single large files.

## Durability

Every object is stored with one storage provider, whose own redundancy
keeps it. For an account with the edge mirror (a second copy at the
time of the write), the same bytes also go to a second fast provider at
the same address, and a background job writes the second copy for
everything written before. The second copy never slows a write: a second
provider more than 16 MiB behind the first, or that has not answered five
seconds after it, is dropped from that write and the background job
writes the copy later.

The second copy satisfies the bucket:

- **An EU-pinned bucket is a single copy until an EU leg exists.** Its
  objects are never copied outside the EU, and today the second provider
  has no EU location, so nothing in an EU bucket is mirrored.
- A bucket pinned to a US region is mirrored only within the US; a bucket
  in the default region (US Central) is mirrored only within the US.
- A bucket on a dedicated tier (Sync, Blomp, Sia) is never mirrored: its
  objects stay on the tier the bucket names.

## Other public routes

```
GET /health          JSON service health with per-backend probe state
GET /health/live     process liveness        GET /health/ready   readiness
GET /status          HTML status page        GET /version        build version
GET /docs            guides hub (getting started, rclone, FAQ)
GET /docs/api        Swagger UI              GET /openapi.json   OpenAPI 3.0
GET /changelog       dated changes           GET /llms.txt       the plain-text summary of this file
GET /.well-known/security.txt
```

### Direct uploads to R2 (public buckets)

For a **public-read** bucket the bytes can go straight from the client to R2
on a Vaultaire-issued presigned PUT and never cross the origin (2026-10-04
bench: 25–28 MB/s from a server, 2–3× a Worker relay). Two calls:

```
POST /api/v1/manage/buckets/{name}/direct-uploads
{"key": "img/a.png", "content_type": "image/png"}
→ 201 {"object":"direct_upload","method":"PUT","url":"https://…r2.cloudflarestorage.com/…","expires_at":…,"complete":"/api/v1/manage/buckets/{name}/direct-uploads/complete"}

PUT <url>   (the client uploads the bytes)

POST /api/v1/manage/buckets/{name}/direct-uploads/complete
{"key": "img/a.png"}
→ 200 {"object":"object","size":…,"etag":…,"backend":"r2"}
```

`complete` HEADs the object on R2, reserves quota and writes the head row, so
the object lists, HEADs and serves through `/cdn` like any other. A private
bucket answers `bucket_not_public`; a deployment without an `r2` driver
answers `direct_upload_unavailable`; completing before the upload answers
`object_not_uploaded`; a quota overrun removes the uploaded object and
answers `quota_exceeded`. Presigned URLs live 15 minutes.
