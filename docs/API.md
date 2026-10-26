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
POST /auth/login     {"email","password"}           -> {"token","tenant_id"}   (24 h JWT)
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
  (minutes to hours). Listings and HEAD report the class the object actually
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
  PUT /buckets/{name}/tier {"tier":"auto|performance|standard|archive|resilient"} · PUT /buckets/{name}/residency
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
- `Idempotency-Key` on PUT/POST/DELETE: the 2xx response is cached 24 h and
  replayed with `Idempotency-Replayed: true`; reusing a key on a different
  method/path is 409 `idempotency_key_reuse`
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

## Rate limits

| Surface | Limit |
|---------|-------|
| S3 API | no request-rate limit. **Egress allowance** (WP-R10-9): each UTC month an account may download 0.5 × its Standard quota + 1 × its Vault quota (`GET /api/v1/user/usage` → `egress_allowance`, `egress_used`, `egress_throttled`, `egress_rate_limit_bytes_per_sec`, `egress_resets_at`). Past it, GetObject bodies (header-signed and presigned) are rate-limited — one token bucket per account, shared with `/cdn` — and never billed; uploads, listings, HEAD and errors are not slowed. While rate-limited, more than 16 concurrent downloads are answered 503 `SlowDown` + `Retry-After: 60`. Enforcement is the `egress_throttle` flag (off until launch) |
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
answered before our Worker runs. aws-cli's default 8 MiB parts need nothing,
but aws-cli v2 (2.23+) sends its body `aws-chunked` with a CRC32 trailer,
which the edge hands to our origin unchanged: set
`request_checksum_calculation = when_required` in the profile to have the
edge move the bytes.

What to expect through the edge: one stream 40–55 MB/s, eight parts in flight
(`rclone --transfers 8`) about 150 MB/s per client — a 1 Gbps line is near
its ceiling; a machine that uploads faster than that is faster through
`stored.ge`, which stays a host. Integrity through the edge is the backend's
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
(the origin keeps the failover path). One stream 100–110 MB/s edge →
backend; the egress is metered once, for the bytes the backend served.

Also known to work unmodified: restic, JuiceFS (`docs/guides/juicefs-setup.md`),
Cyberduck, Nextcloud's S3 object store, Immich.

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
