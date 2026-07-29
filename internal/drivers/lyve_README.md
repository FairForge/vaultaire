# Lyve Cloud 2 Driver — Ops Manual

Status (2026-07-28): **tested and usable, not a launch tier.** Lyve was
"dropped" in July 2026 partly on a perf verdict that turned out to be a
benchmarking artifact (see *The bucket-homing trap* below). Pricing/strategy
still keep it out of the launch lineup, but the driver is fully functional and
benchmarked if we ever want it (e.g. as an extra replication target).

## Platform architecture (Lyve Cloud 2 / RSTOR)

Seagate's current platform (endpoints `s3.<region>.global.lyve.seagate.com`)
is the RSTOR-derived "Lyve Cloud 2". API reference PDF: *Lyve Cloud Object
Storage API User Guide* (extracted highlights below). Key facts:

- **Path-style requests only**, SigV4 (or V2) signing.
- **Every bucket is homed in the region(s) of its replication policy.** The
  policy is set at bucket creation (`PUT /bucket?replication-policy=US-WEST-1`,
  comma-separated list); with no policy the bucket is homed in the region
  whose endpoint created it (the PDF claims "replicated to every region" by
  default — account behavior says otherwise; trust the probe, not the PDF).
- **Any regional endpoint accepts requests for any bucket and transparently
  proxies to the bucket's home region.** No error, no redirect — just
  ~500 ms/op added latency for cross-US proxying.
- Check an object's home: `GET` it with header `x-rstor-replication-status:
  true`; the response header lists the region(s) holding it.
- Our account: all 7 regions available, `ReplicationEnforcement: false`
  (query `POST /?Action=RSAvailableRegions&Version=2010-05-08` on
  `iam.global.lyve.seagate.com`, service `iam`, root S3 creds).
- Multipart uploads must be **completed in the DC where they were initiated**
  (fine for this driver — it pins one regional endpoint).
- SSE is on at rest (`X-Rstor-Sse-Encryption-Status: true` on responses).
- Known wart: malformed-auth PUTs hang ~9 s then return 502 (healthy paths
  return fast 403s). Reads/writes with valid auth are unaffected.

### Quick probes (curl ≥7.75)

```bash
# Where does this object live?
curl -sS -o /dev/null --aws-sigv4 "aws:amz:us-west-1:s3" --user "$AK:$SK" \
  "https://s3.us-west-1.global.lyve.seagate.com/BUCKET/KEY" \
  -H "x-rstor-replication-status: true" -D - | grep -i rstor

# Create a bucket homed in a specific region (do this from that region's endpoint)
curl -sS --aws-sigv4 "aws:amz:us-west-1:s3" --user "$AK:$SK" -X PUT \
  -H "Content-Length: 0" "https://s3.us-west-1.global.lyve.seagate.com/BUCKET"
```

## Admin (RS) API — tested 2025-11-28, still valid

Account identity (`RSGetUserInfo`): we are customer **v01** under reseller
**global** (root user isaacv17@gmail.com) — customer-level RS access, not
reseller-level. Three service hosts, each with its own signing service:
`iam.global.lyve.seagate.com` (service `iam`),
**`sts.global.lyve.seagate.com` (service `sts`)** — this host exists and
works (geo-DNS to the nearest DC; the 2025 note claiming "STS = IAM host"
was wrong — STS actions 400 on the IAM host), and
`s3.<region>.global.lyve.seagate.com` (service `s3`).

**Working with our root S3 creds:**

```bash
# Per-bucket size/objects/REPLICATION POLICY for the whole account — THE
# homing audit tool (S3 endpoint, service s3):
curl -s --aws-sigv4 "aws:amz:us-west-1:s3" --user "$AK:$SK" \
  "https://s3.us-west-1.global.lyve.seagate.com/?rs-bucket-stats"

# Account info / regions / billing (IAM endpoint, service iam, POST form):
curl -s -X POST --aws-sigv4 "aws:amz:us-east-1:iam" --user "$AK:$SK" \
  -d "Action=RSGetUserInfo&Version=2010-05-08" https://iam.global.lyve.seagate.com/
# Also working: Action=RSAvailableRegions (7 regions, ReplicationEnforcement:false),
# Action=RSListBillingData&From=<ISO>&Till=<ISO> (per-region daily
# upload/download/delete bytes + UsedSpace + ObjectCount — usable for COGS
# reconciliation), and standard IAM CreateUser/ListPolicies (sub-user mgmt).
```

**STS works** (2026-07-29): `AssumeRole` on `sts.global.lyve.seagate.com`
(sign service `sts`, `Version=2011-06-15`, RoleArn/RoleSessionName required
but ignored) returns temp credentials that work against S3 with the session
token. The 2025-11 failure was just the wrong host.

**Not available to us** (reseller-only or unsupported): `RSLiveBilling`,
`RSListCustomer`, `RSCustomerDetails`, `RSWhitelist*`, `RSSAML*`
(Unauthorized / BadRequest / not supported).

## API coverage sweep (2026-07-29) — everything in the LC2 guide checked

Every customer-scoped action in `docs/references/lyve-cloud-2-api-en_US.pdf`
has now been live-verified except the deliberate skips below.

**Verified working** (beyond everything already listed above): full IAM
policy versioning (CreatePolicyVersion/List/Get/SetDefault/Delete — version
ids are non-standard strings like `vH92BC9A0V5YE`, not `v2`),
ListAttachedUserPolicies, ListEntitiesForPolicy; STS AssumeRole end-to-end;
bucket encryption retrieve (AES256 default confirmed at bucket level);
**bucket policy set/delete with real anonymous access** (public buckets
work; `rs-info` reflects `isPublic`); object-tag delete;
`response-content-type`/`-disposition` GET overrides; ListMultipartUploads
+ ListParts; **browser-style POST form uploads** (SigV4 policy via
`generate_presigned_post` → 204); Content-MD5 enforcement (BadDigest on
mismatch); trailing-slash empty-directory keys; delimiter `/` listing.

**Broken / quirks found in the sweep:** bucket *tagging* is accepted but
silently dropped (PutBucketTagging 200 → GetBucketTagging NoSuchTagSet —
object tagging works fine); PutBucketLogging → InvalidRequest (logging
likely unsupported); GetBucketPolicyStatus → NotImplemented;
`GET /speedtest/*` → 404 NoSuchBucket on LC2 (v1 relic in the doc);
`?metadata` on a bucket just returns a listing. Plus the earlier finds:
replication-policy param ignored, `If-None-Match: *` overwrites, SigV2
presigned PUT 403s.

**Deliberately untested:** Object Lock / legal hold / retention (WORM —
confirmed working separately; a lock would make test buckets undeletable);
root-auth mutators (ChangePassword, UpdateLoginProfile, RSSetUserInfo,
RSResetPassword/GetPasswordResetToken, RS*TFA, RSSetUserAuthMethod —
lockout risk on the root account); RSLogin (needs console password);
reseller-only management (RSCreate/ModifyCustomer,
RSSetCustomerServiceAccessLevel).

## Feature matrix (probed live from SLC, 2026-07-28)

Everything below was verified against real us-west-1/us-east-1 buckets, all
probe buckets deleted afterward. Object Lock / WORM deliberately **not
tested** — a lock would make the bucket undeletable.

**Works, full S3 semantics:**

| Feature | Notes |
|---|---|
| Versioning | Enable/suspend, old-version GET, delete markers, marker removal restores. Purge all versions before bucket delete. |
| Server-side copy | 16 MB: same-bucket 0.6 s, cross-bucket 0.7 s, **cross-region 1.9 s** (west→east, zero client bandwidth) |
| Presigned URLs | Standard SigV4 (`aws s3 presign`) AND native `POST /b/k?rs-presign=` + `ExpirationDate` (≤720 h) → JSON `{"link": ...}` |
| Lifecycle | Expiration rules set/get/delete (don't add lock-adjacent rules) |
| Object tagging | `x-amz-tagging` on PUT + Put/GetObjectTagging |
| Bucket CORS | Set/get/delete |
| SSE-S3 | On by default account-wide (AES256 on every response) |
| SSE-C | Correct enforcement — GET without the key is rejected |
| Checksums | `--checksum-algorithm SHA256` honored, ChecksumSHA256 returned |
| Conditional GET | `If-None-Match` → 304 |
| IAM sub-users | Default-deny until policy attached; CreateUser/CreateAccessKey/Delete* all work (delete keys before user or DeleteConflict) |
| `GET /<bucket>?rs-info` | `{bucketSize, replicationPolicy, versionStatus, isPublic}` |

**Broken / unavailable — the gotchas that matter:**

- **Multi-region replication is NOT available to our account.** The
  `replication-policy` bucket-create parameter is silently ignored as a
  query param and as a header (bucket homes in the creation region no
  matter what you pass), and 400s as a form body. `ReplicationEnforcement:
  false` from RSAvailableRegions suggests we may choose — reality says no.
  Geo-redundancy must be client-side: write the home region, then async
  **server-side cross-region CopyObject** to a second-region bucket (works,
  no client egress).
- **No atomic create:** `PUT` with `If-None-Match: *` on an existing key
  returns 200 and overwrites (real S3 would 412). Do not build
  concurrency control on Lyve conditional writes.
- `GET /speedtest/download` returns ~200 bytes, not a stream — dud.
- Stale IAM sub-users exist from earlier experiments (`stored-*@stored.ge`
  ×6, `servertest`) — left in place, audit before deleting. The 2025 test
  users were cleaned up 2026-07-28.

## Extended probes (2026-07-29)

**APAC regions work** (smoke, from SLC, correctly-homed buckets — latency is
RTT-bound, so APAC-local clients would see local latencies):

| Region | warm PUT/GET/HEAD p50 | cold dial p50 (p95) |
|---|---|---|
| ap-southeast-1 (Singapore) | 337 / 216 / 215 ms | 2.1 s (9.0 s) |
| ap-northeast-1 (Tokyo) | 332 / 216 / 214 ms | 1.2 s (4.1 s) |

Lyve is the only backend in our lineup with an APAC footprint.

**IAM capacity / per-tenant credentials:** bulk-created 25 sub-users with no
cap hit (~1.2 s per create — provision async), bucket-scoped managed policies
(CreatePolicy → AttachUserPolicy) enforce correctly, and the full
create→scope→key→delete lifecycle is clean. No documented user/bucket caps
in the API guide. This makes **per-tenant direct S3 credentials** viable:
Vaultaire as control plane, tenants reading/writing Lyve directly.

**Presigned offload:** 256 MB via presigned GET = **130 MB/s** single-stream,
unauthenticated — full egress offload with zero Vaultaire bandwidth.
Presigned PUT works **only with SigV4** URLs (SigV2 presigned PUTs 403;
SigV2 presigned GETs work). Direct client upload is therefore possible.

**Cross-region replication at scale:** 256 MB server-side copy west→east in
5.5 s (**~47 MB/s** — the 16 MB number was overhead-dominated), and
**UploadPartCopy works cross-region** (32 MB parts, completed MPU verified),
so objects of any size can replicate server-side in parallel parts.

**Lifecycle canary:** bucket `lyve-lifecycle-canary` (us-west-1) has a 1-day
expire-all rule and one object planted 2026-07-29 — check whether it
actually expired before relying on lifecycle for retention. Delete the
bucket after the check.

## Product fit

- **Best fit: second-vendor DR/replication target for the Standard tier** —
  real S3 semantics (versioning, copy, presign), strong concurrency
  (778 MB/s ingest), Seagate-grade durability, and server-side cross-region
  copy for a cheap geo story. Wire as a `ReplicationDriver` target.
- **EU data residency option** (eu-west-1, eu-central-1 already in our
  account) without onboarding a new vendor — and the only APAC footprint
  (Singapore, Tokyo) in the stack.
- **Direct-access data plane** (candidate, needs pricing confirmation):
  per-tenant sub-users + scoped policies + SigV4 presigned URLs would let
  bandwidth-heavy tenants hit Lyve directly with Vaultaire as control plane
  only. Verify our contract's egress/API pricing first — Lyve markets no
  egress fees, which would make presigned offload free bandwidth.
- **Not** a primary hot tier (iDrive is cheaper with faster single-stream
  GET) and **not** archive (Geyser is ~4× cheaper per TB). Lyve ≈
  $6.37/TB (cost map in `cmd/vaultaire/main.go`).

## Prior art & where the old tooling lives

- `~/fairforge/vaultaire-benchmark/lyve-*.sh` — Nov 2025 API sweeps:
  `lyve-correct-endpoints.sh` (the canonical RS/IAM test, correct global
  endpoints — results in `lyve-correct-test-20251128-123805.log`),
  `lyve-full-api-test.sh` + `lyve-rs-commands-test.sh` (older, use the
  RETIRED `s3.<region>.lyvecloud.seagate.com` v1 endpoints; SSE-S3/SSE-C
  test sections still useful as recipes). Scripts contain hardcoded root
  creds — do not commit them.
- `.private/lyve-advanced-test.js` (+ report JSON) — Node SDK perf suite
  (versioning, tagging, lifecycle, CORS, presign, multipart tuning
  configs), Oct 2025, v1 endpoints.
- `docs/references/lyve-cloud-2-api-en_US.pdf` — the LC2 API guide,
  committed in-repo (same doc as the Downloads copies).
- Seagate legal/portal docs in `~/Downloads`: LYVE CUSTOMER AGREEMENT,
  LYVE SERVICES TERMS, `lyve-management-portal-en_US.pdf` (console admin).
- `test-lyve-live.sh` (deleted from repo Sep 2025) — trivial pre-auth
  smoke, superseded; recover with `git show d997410:test-lyve-live.sh`.
- SLC: `~/vaultaire-bench/bench-lyve-west.sh` (E2E smoke wrapper),
  `drivers-test-linux` (integration-test binary), full/smoke JSONs in
  `~/vaultaire-bench/bench-results/`.

## The bucket-homing trap (root-caused 2026-07-28)

The July "us-west-1 write degradation" (warm 4 KB PUT p50 ~580 ms, 2 ops/s,
cold dial ~740 ms) was **entirely** the proxy path: the shared bench bucket
was homed in US-EAST-1, so every us-west op crossed the country. Controlled
A/B from SLC (warm connection): west-homed bucket via west endpoint
**50–80 ms**; east-homed via east 60–90 ms; east-homed via west 500–700 ms.
Even the cold-dial asymmetry was this artifact (that workload includes a PUT).

Rules that follow:

1. **Create every bucket through the endpoint of the region it should live
   in** (or pass `replication-policy` explicitly).
2. **Never benchmark or serve a bucket through a non-home endpoint** unless
   you intend to measure the proxy path.
3. E2E-through-Vaultaire numbers cannot assess backend health while
   `engine/failover.go` can land PUTs locally — verify objects actually
   reached Lyve with a direct probe.

## Driver specifics (`lyve.go`)

- Region default **us-west-1** (closest to SLC; `LYVE_REGION` overrides —
  also defaulted in `cmd/vaultaire/main.go` and `scripts/bench-vaultaire.sh`).
- Bucket layout: one `stored-<region>` bucket per region, keys
  `t-<tenant>/<container>/<artifact>`. `stored-us-west-1` and
  `stored-us-east-1` exist and are verified correctly homed (2026-07-28).
- Uploads go through the shared parallel multipart uploader
  (`s3ParallelUploadInput`, 16 MiB parts × 8 concurrent) with full metadata
  passthrough; small objects are a single PutObject.
- `List` paginates (ListObjectsV2 paginator) — no 1000-key truncation.
- `HealthCheck` = HeadBucket, which deliberately also catches a missing
  `stored-<region>` bucket before the engine can fail over writes to disk.
- Not in `STORAGE_MODE` auto-detect; enable explicitly with
  `STORAGE_MODE=lyve` + `LYVE_ACCESS_KEY`/`LYVE_SECRET_KEY`.

## Benchmarks (SLC, 2026-07-28, direct via bench-compare, west-homed bucket)

Full run: `bench-results/lyve-uswest-full-0728.json` on the SLC box.

| Workload | Result |
|---|---|
| cold dial + 1 KB PUT | p50 170 ms |
| warm 4 KB PUT / GET / HEAD | p50 50 / 42 / 38 ms |
| 64 MB PUT / GET single-stream | 112.6 / 95.0 MB/s |
| 256 MB multipart (16 parallel parts) | 213.7 MB/s |
| concurrent ingest / download (20 s) | 778 / 496 MB/s |
| sustained upload 60 s | 442 MB/s, steady (no Geyser-style collapse) |
| burst 500 small files | 194 ops/s |
| worker escalation 8→128 | 602 ops/s at 128 workers, **0 errors** |
| read-after-write / overwrite / list consistency | 20/20, 10/10, 10/10 |

Comparison vs iDrive (launch hot tier, 2026-07 numbers): iDrive wins
single-stream 64 MB GET (225 vs 95 MB/s); **Lyve wins multipart (214 vs
183 MB/s) and concurrency (ingest 778 vs 512, download 496 vs 349 MB/s)**
and has no rate-limit errors up to 128 workers. Integration test
(`TestLyveDriver_Integration`, env-guarded) passes from SLC including a
20 MB multipart round-trip.

Bench gotchas:
- `bench-compare -only lyve` matches **all 7** regional endpoints (~15 min);
  use `-only lyve-us-west`.
- Per-region bench buckets `vbench-lyve-<region>` are pre-provisioned and
  correctly homed; the legacy shared `vbench-user1-lyve` bucket is
  **east-homed** — do not reuse it for west benches.
- Set `AWS_DEFAULT_REGION` when using aws-cli on SLC or every op burns ~2 s
  on IMDS probes.
