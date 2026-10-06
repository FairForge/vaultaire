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
| S3 endpoint | `https://stored.ge` |
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
HEAD   /<bucket>/<key>                HeadObject     (from cache; all conditionals)
DELETE /<bucket>/<key>                DeleteObject   (?versionId; delete marker on versioned buckets)
POST   /<bucket>?delete               DeleteObjects  (≤ 1 000 keys, quiet mode)
PUT    /<bucket>/<key> + x-amz-copy-source            CopyObject (metadata/tagging directives; encrypted sources 501)
POST   /<bucket>/<key>?uploads        CreateMultipartUpload
PUT    /<bucket>/<key>?partNumber=&uploadId=          UploadPart (≥ 5 MB except the last; UploadPartCopy 501)
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
  MFA Delete is available on lock-enabled buckets (`x-amz-mfa` TOTP).
- **Versioning is metadata-only today**: version history and delete markers
  work; `GET ?versionId=<non-current>` answers 501 (WP-R2-1).
- **Regions**: a bucket's region is fixed at creation and must be one the
  deployment has a key pair for; otherwise 400 `InvalidLocationConstraint`.
  Vault objects go to the tape backend and public-read objects to the CDN
  origin regardless of the bucket's region.
- **Public buckets** (`?acl` public-read) are served unauthenticated from
  `cdn.stored.ge` with Range, conditionals, per-bucket CORS and a
  `Cache-Control` from the bucket's cache TTL; HTML/SVG/XML are always
  attachments. Chunked (≥ 64 MiB) and SSE-encrypted objects are not yet served
  by the CDN path (WP-R2-2).
- **CORS on the S3 API** (2026-10-06): `PUT|GET|DELETE /<bucket>?cors` take and
  return the AWS `CORSConfiguration` document (≤ 100 rules; origins
  case-sensitive with at most one `*`; methods GET/PUT/HEAD/POST/DELETE;
  `ExposeHeader` without wildcards). A bucket has **no** configuration until you
  put one: every cross-origin browser request is then refused. The browser's
  `OPTIONS` preflight is answered **before** SigV4 (a browser never signs it) —
  200 with the matching rule's headers, 403 `AccessForbidden` otherwise — and
  the real, signed response carries `Access-Control-Allow-Origin`,
  `Access-Control-Expose-Headers` (list `ETag` to read multipart part ETags from
  a page) and `Access-Control-Allow-Credentials` on every status, the auth error
  included. The preflight looks the bucket up by name. Use `s3.stored.ge` from a
  page for uploads above 100 MB (the `stored.ge` hostname is proxied and capped).
  The `/cdn` path's CORS (`cors_origins`, default `*`) is separate and unchanged.
- **Encryption**: SSE-C works per request. SSE-S3 requires the deployment's
  master key, which production does not set today — without it a bucket's
  `sse_enabled` flag has no effect and objects are stored as sent. With the
  key, an object too large to encrypt whole (> 256 MiB on the non-chunked
  path) is refused 413 rather than stored plaintext.

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
Webhooks    GET/POST /api/v1/webhooks · PATCH/DELETE /webhooks/{id} · GET /webhooks/{id}/deliveries · POST /webhooks/{id}/test
Events      GET /api/v1/events   (object.created/deleted/downloaded, bucket.*, key.*, sts.token_created, webhook.test)
STS         POST /api/v1/sts/token {"duration_seconds","permissions","bucket_scope","parent_key_id"}
Admin       /api/v1/admin/*  (admin role: audit, flags, quota-reconcile; jobs = the background jobs' state,
            POST jobs/{job}/run or dedup-gc | smart-demotion | retention | account-deletion → 202, the run continues;
            GET routing-truth = head rows vs. the backends that hold their bytes, POST routing-truth/resolve-null)
```

Conventions (Stripe-style):

- single object: `{"object":"bucket", …, "request_id":"…"}`
- list: `{"object":"list","data":[…],"has_more":bool,"next_cursor":"…"}` with `?limit=&starting_after=`
- error: `{"error":{"type":"invalid_request_error|authentication_error|permission_error|not_found_error|conflict_error|rate_limit_error|api_error","code":"…","message":"…","param":"…","request_id":"…"}}`
- `Idempotency-Key` on PUT/POST/DELETE: the 2xx response is cached 24 h and
  replayed with `Idempotency-Replayed: true`; reusing a key on a different
  method/path is 409 `idempotency_key_reuse`
- webhooks are signed `X-Webhook-Signature: sha256=<hmac-hex>` with the
  `whsec_` secret shown once at creation; one delivery attempt per event, no
  retries yet (WP-R11-3); targets may not be private or loopback addresses
- every response carries `X-Request-Id` and `X-Vaultaire-Version` (a date)

The management bucket create applies the same rules as S3 CreateBucket
(idempotent re-create, free-tier cap, region validation).

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

Some messages carry a suggestion after the standard text (closest bucket or
key name within the tenant, the auth header that was malformed, the lock that
refused a delete). The full code list from `internal/api/s3_errors.go`:

| HTTP | Codes |
|------|-------|
| 400 | `InvalidBucketName`, `InvalidObjectName`, `InvalidRequest`, `IncompleteBody`, `BadDigest`, `InvalidDigest`, `MalformedXML`, `XAmzContentSHA256Mismatch`, `InvalidArgument`, `InvalidPart`, `InvalidPartOrder`, `EntityTooSmall`, `InvalidPartNumber`, `AccessControlListNotSupported`, `InvalidRetentionPeriod`, `AuthorizationQueryParametersError`, `AuthorizationQueryParametersError_Expires`, `InvalidLocationConstraint`, `InvalidTag` |
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
| 503 | `ServiceUnavailable` (every backend unreachable; carries `Retry-After`), `SlowDown` (+ `Retry-After`: the egress stream guard, see Rate limits) |

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
