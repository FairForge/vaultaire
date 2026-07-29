# Geyser (Spectra Logic Vail) — Ops Manual

Geyser is our LTO-9 tape archive backend (`la1.geyserdata.com`, LA; London
exists but its bucket was deleted 2026-04-20). It is a **Spectra Logic Vail**
deployment — responses carry `HostId: SpectraLogicVail-lavail/1.1` — which
means Spectra's published API surface applies even where Geyser's own docs are
silent. Vail spec: `docs/references/vail-api-guide.pdf`.

Driver: `geyser.go`. Console client: `geyser_admin.go` (programmatic login
flow as of WP-Geyser-Admin 2026-07-29, see Auth below).

## Storage semantics — it is true Glacier, not slow S3

Measured 2026-07-29 against the prod bucket:

- Every object lands unclassed and readable, then flips to **`StorageClass:
  GLACIER`** and is evicted from the Vail staging disk (>24h — canary
  `bench-probe/fresh.txt` planted 2026-07-29 was still readable after 24h;
  keep checking until it goes 403).
- A direct `GET` on an evicted object returns **403 `InvalidObjectState`**.
  `HEAD` always works (metadata/ETag intact).
- `RestoreObject` recalls it: **<3 min** measured (idle library), bulk-friendly
  (24 objects submitted at 17.6 req/s, all recalled together). Re-restoring
  **extends** the expiry.
- **Ingest (corrected 2026-07-29): the infamous "27.7 MB/s account-wide
  ceiling" was OUR bug, not Geyser's.** The Vail gateway negotiates HTTP/2
  and Go multiplexes all concurrent uploads onto one TCP connection. Over
  HTTP/1.1 (one connection per request): **235–276 MB/s** burst (256 MB
  multipart, 16 parts), **227 MB/s sustained** (2.5 GB, 5 rounds, aws-cli
  control confirmed 251 MB/s independently — no QoS kick-in). ≈ **19+ TB/day
  to staging**. `geyser.go` now forces h1 via `WithHTTP1Only()`. Single
  connection is still ~25–30 MB/s (TCP-window bound) — throughput comes from
  concurrent objects/parts, so drain many objects in parallel.
- **Reads are genuinely slow per-stream, but scale with fan-out**
  (h1, staging-disk objects, 2026-07-29): ~2.5–3 MB/s per stream; 8 streams
  → 25 MB/s, 32 streams → 76 MB/s aggregate (≈6.4 TB/day; more streams
  untested — keep widening). The old "rehydration ≈ 2 TB/day" figure was
  h2 + low-concurrency; V18.2's orchestrator should restore with 32-64
  parallel streams. Small-file burst 118 ops/s.
- **Bench caveat**: `bench-compare`'s default client forces h2, so its
  `sustained_upload`/`concurrent_ingest` numbers under-report Geyser ~9×.
  Trust the `*_h1_*` workloads (or aws-cli) for this backend.
- Safe on archived objects: `DELETE` (so restic prune works), `PUT` overwrite
  (immediately readable), range-GET on *restored* copies (so restic pack reads
  work post-restore).
- Server-side lifecycle auto-aborts multipart uploads older than 7 days (set by
  Geyser during provisioning — we cannot modify lifecycle rules via S3).

**Engine gap:** nothing in Vaultaire handles `InvalidObjectState` today, so any
GET on data past the staging window fails. That is plan item **V18.2**, the
first post-launch WP.

## Three API layers

| Layer | Endpoint | Auth | What our S3 keys can do |
|---|---|---|---|
| S3 data plane | `la1.geyserdata.com` | SigV4 with `GEYSER_ACCESS_KEY/SECRET` | everything above — full read/write/restore |
| Geyser console API | `console.geyserdata.com/api/*` | browser session (**not** S3 keys) | nothing without a session |
| Vail management API | `la1.geyserdata.com/sl/api/*` | `POST /sl/api/tokens` → JWT (console user/password + MFA) | nothing without a token (401) |

### Auth reality (2026-07-29, live-verified)

`geyser_admin.go`'s documented flow — copy `accessToken` + `userId` cookies
from DevTools — is **stale and will not work**. A live sweep with
`tools/geyser-grabber` found the only readable `geyserdata.com` cookies are
Wix cookies from the marketing site, and header capture shows the app sends
**no explicit auth header at all** — every call is plain
`credentials: "include"` cookie mode against an httpOnly session cookie that
is not exposed to extensions.

**Therefore: don't scrape a cookie — log in.** `POST /api/login` exists
(with `/api/login/regenerate` and `/api/logout`), so `geyser_admin.go`
authenticates with a real login call, keeps the returned session in a cookie
jar, and continues pinging `/api/keepalive`. That removes the manual DevTools
step entirely.

**IMPLEMENTED (WP-Geyser-Admin, 2026-07-29).** The verified flow, now in
`geyser_admin.go`:

1. `Login(ctx, email, password)` — `POST /api/login` with
   `{emailAddress, password}` + `X-Source: UI` → 201 MFA challenge
   `{hash, id, responseType:"MFA", totpUser}`. The `id` is the challenge id,
   NOT a session token.
2. Geyser emails a one-time code (`totpUser:false`) or expects a TOTP code.
3. `VerifyMFA(ctx, hash, code)` — `PUT /api/login` with `{hash, token}` →
   200 session `{id, user:{id,...}}`. The session id/user id become the
   `accessToken`/`userId` cookies on every call; httpOnly cookies the server
   sets ride along via the client's cookie jar.
4. `StartKeepalive(ctx)` as before (session ~1h idle expiry).

`NewGeyserAdminClientWithLogin` bundles steps 1+3 for TOTP accounts (code
known upfront); email-MFA accounts drive the two steps separately.
`NewGeyserAdminClient(accessToken, userID, ...)` remains as a direct-injection
fallback. The client also wires `RestoreToCache`, `RestoreToCloud`,
cloud-integration CRUD, `cloudSync` create/status, `GetTapeCollections`,
`GetSites`, and `GetEvents`. Response-shape caveat: tape/event field names in
Go structs are inferred — verify against a live capture on first real use.

The Vail token endpoint is live and validating — a deliberately fake login
returns a proper field-level `400 Invalid username or password`, so real
console credentials should mint a JWT.

## S3 feature matrix (all probed live 2026-07-29)

**Works via our S3 keys:**

| Feature | Notes |
|---|---|
| Versioning | Enable/suspend — works, but leaves delete markers on every delete that require versioned-delete to purge. Leave suspended on prod. |
| Bucket tagging | Set/get/delete |
| CORS | Set/get/delete |
| Bucket policy | Read AND write — we can re-PUT the existing policy |
| Object tagging | `x-amz-tagging` on PUT + get/delete |
| Presigned GET | SigV2 works |
| Presigned PUT | SigV4 works (200) |
| Server-side copy | ~480ms for small objects on staging disk |
| Content-MD5 enforcement | BadDigest on mismatch |
| SHA256 checksums | `--checksum-algorithm SHA256` honored, `ChecksumSHA256` + `ChecksumType: FULL_OBJECT` returned; also CRC64NVME auto-computed on every PUT |
| Public-access-block | Readable — currently `BlockPublicAcls: true`, `BlockPublicPolicy: true`, `IgnorePublicAcls: true`, `RestrictPublicBuckets: false` |
| Ownership controls | `BucketOwnerEnforced` |
| ACL | Readable — single canonical-user FULL_CONTROL |

**Not available / broken:**

| Feature | Error |
|---|---|
| SSE-C | `NotSupported` — Vail/Geyser doesn't do customer-provided encryption keys (SSE-S3 is not on either — `ServerSideEncryptionConfigurationNotFoundError`) |
| Notifications | Only SNS topic config allowed, validates the topic ARN is reachable — usable with real SNS, not arbitrary webhooks |
| Lifecycle set | `AccessDenied: PutBucketLifecycle is not supported` — the existing rule (MPU cleanup) was set by Geyser during provisioning; we cannot add or modify lifecycle rules via S3 |
| Object Lock | `ObjectLockConfigurationNotFoundError` — semantics exist in Vail but not provisioned on our bucket (ask Geyser) |

**`spectra-storage` metadata:** Vail stamps a custom `spectra-storage` header on
archived objects showing which storage pool(s) hold copies (e.g. `1.293` =
one pool, `1.293,1.4` = two pools — likely disk staging + tape). Fresh
staging-only objects have no value. Useful as a "storage location" indicator
in V18.8's dashboard.

**London endpoint:** TCP + HTTPS alive (`lon1.geyserdata.com`), S3 listing
returns 404 (our bucket is LA-only). Usable once a London bucket is
provisioned via console.

## Console API map

Extracted from the console's own JS bundles (74 page chunks under
`console.geyserdata.com/assets/entries/`, publicly served), then **live-probed
from inside an authenticated session** on 2026-07-29.

Confirmed **200** for our Admin role: `/api/buckets`, `/api/keys`,
`/api/keepalive`, `/api/invoices`, `/api/tapeCollections`, `/api/sites`,
`/api/events` (full audit log — logins, actions, timestamps), `/api/users`,
`/api/roles`, `/api/customers`.
Confirmed **404** (do not exist): `cloudLibraries`, `userGroups`, `orgs`,
`billing`, `usage`, `notifications`, `me`, `session`.

**Facts read out of the live responses:**

- **Three datacenters exist** — `/api/sites` returns Los Angeles US, London
  UK, and **São Paulo, Brazil** (previously unknown to us; a third geo for
  V18.3 dual/tri-site replication).
- Our tape collection `Stored3Lib` is **5 TB, "Single Copy", Los Angeles**,
  compression disabled. Single Copy means there is no second tape today —
  dual-site redundancy is a provisioning/cost change, not a code change.
- **Both S3 keypairs already exist**: `AKIA0ZGZX7E5NTXN1PKN` (the one in
  `.env.bench`) and `AKIALIVSFQ8EFI4ITBL4`, both active. The API returns key
  IDs only — the second secret must come from the console UI. So the "get a
  second key" item is really "retrieve the existing second secret."
- Buckets carry `corsEnabled`, `color`, `customer`, `createdAt`.

**Restore — the important part.** The console exposes *two* restore modes:

```
POST /api/buckets/{bucketId}/restoreToCache  {path, versionId}
POST /api/buckets/{bucketId}/restore         {path, integrationId, versionId}
```

`restoreToCache` is the S3 `RestoreObject` equivalent (thaw onto Vail's
staging disk). **`restore` targets a configured cloud integration** — i.e.
Geyser can push a recalled object straight into an external S3 destination
without the bytes ever transiting our infrastructure. That is a native
answer to "rehydrate somewhere better than the tape gateway."

```
POST   /api/buckets/{id}/cloudIntegrations     {cloudIntegrationType, ...}
DELETE /api/buckets/{id}/cloudIntegrations/{integrationId}
```

`cloudIntegrationType` ∈ **`AWS` | `WASABI` | `ORACLE` | `GEYSER`**; buckets
carry `s3Enabled` / `wasabiEnabled` flags. **Open question:** whether the
`AWS` type accepts a custom endpoint (which would let us target iDrive or
Lyve) or is pinned to real AWS — this decides whether V18.2 can offload
rehydration entirely to Geyser.

**cloudSync — server-side ingest from another cloud** (this is the published
"Wasabi cold data archiving" integration):

```
POST /api/cloudSync         {source:{type,region,bucket,accessKey,secretKey}, action:"SYNC", bucketId}
POST /api/cloudSync/keys    {accessKey, secretKey, cloudSyncId}
POST /api/cloudSync/confirm {topicArn, bucketId}      # AWS SNS completion topic
GET  /api/cloudSync?query=bucketId=={id}
```

Job status enum: `PENDING | QUEUED | INPROGRESS | COMPLETED`. Geyser pulls
directly from the source bucket — relevant to **V18.4** (Glacier/cloud
migration tooling): a customer migration may not need our bandwidth at all.

**Everything else:**

```
/api/login  /api/login/regenerate  /api/logout  /api/keepalive
/api/password/change  /api/password/forgot
/api/totp/enable  /api/totp/confirm  /api/totp/disable
/api/termsOfService/required  /api/users/accepttermsofservice
/api/keys  /api/keys/{id}  /api/keys/{id}/enable      # S3 keypair CRUD
/api/buckets  /api/buckets/{id}  /api/buckets/{id}/delete
/api/buckets/{id}/airgap  /api/buckets/{id}/mount  /api/buckets/{id}/confirmmount
/api/tapeCollections  /api/invoices  /api/estimates
/api/datacenters  /api/datacenterpricing/{id}
/api/supportedregions  /api/supportedregions/wasabi  /api/supportedregions/oracle
/api/providers/{id}  /api/brokers  /api/resellers/{id}  /api/customers
/api/roles?query=org.id=={id}&page=&size=
```

Query params use **RSQL** (`?query=field==value`) with `page`/`size` paging.

Our account: org **FairForge, LLC** (`990ba8ee-ca04-41df-8c10-6f43ece629b5`,
type `CUSTOMER`), role **Admin** with `MANAGE_CLOUD_LIBRARIES`,
`MANAGE_TAPE_COLLECTIONS`, `MANAGE_USERS/ROLES/USER_GROUPS`,
`MANAGE_CUSTOMERS`, `VIEW_BILLING/EVENTS/SITES`.

## Vail management API (`/sl/api`)

Live but unauthenticated to us. Per the spec, buckets carry `locking`
(object lock), `restore` (**automatic read-through recall** — would obviate
most of V18.2), `versioning`, `encrypt`, and `linkedStorage`; plus lifecycle
CRUD, `PUT /sl/api/buckets/{b}/objects/{o}/{storage}?days=N`
(clone-as-restore with TTL), and clone-verification triggers (integrity
attestation — a "we prove your backup restores" primitive).

## Open items for the account owner

1. Test console credentials against `POST /sl/api/tokens` — unlocks the Vail
   control plane (read-through restore, object lock, clone-as-restore).
2. Copy the **second key's secret** (`AKIALIVSFQ8EFI4ITBL4`) out of the
   console UI — the key already exists and is active; the API never returns
   secrets. Enables key rotation. (The old reason — testing whether the
   27.7 MB/s ceiling is per-key — is moot: that ceiling was our HTTP/2
   client bug, fixed 2026-07-29.)
3. **Object lock: per Isaac (2026-07-29) it is accessible through the
   console** — not a support ask. Console bucket responses carry
   `objectLocking` (see `GeyserBucketStatus`), so it's likely a
   creation-time toggle. On the next authenticated session: create a test
   bucket with locking on (probe an `objectLocking: true` field in the
   create payload / check the UI), and wire the flag into
   `geyser_admin.go`'s `createBucketRequest` once the wire name is
   confirmed.
4. Ask Geyser: whether `cloudIntegrationType: AWS` accepts custom endpoints
   (decides if rehydration can bypass our bandwidth entirely); São Paulo
   availability and dual-copy pricing; recall latency under real contention;
   restore/egress contract pricing; staging-window length; whether internal
   staging→tape flush sustains ~19 TB/day of ingest.

## Bench recipes

`ssh vaultaire-slc '~/vaultaire-bench/bench-geyser.sh'`, or targeted:
`./bench-compare-linux -only geyser-la -only-workload "sustained,concurrent_ingest,burst_small"`
(comma-separated, not pipes). Full numbers live in the
`benchmark-results-2026-07` session memory.
