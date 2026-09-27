# R7 Storage drivers — 2026-09-27

**Repo state reviewed:** `main` @ `53dbfdb` (#495). Branch: `review/R7-drivers`. **PR:** #496.
**Depends on:** R6 (contract table in `docs/reviews/R6-engine.md` § *Driver contract as the engine
assumes it*; R6-01/02/04/06 fixed there), R0 (drivers files deleted; `geyser_admin.go` kept for the
`cmd/geyser-*` probes), R1 (R1-12 probes; HAProxy `timeout server 50000`), R2 (R2-04 `resolvePath`,
R2-06 short body, R2-09 client-body errors), R9 (R9-12 backups).

## Scope reviewed

| File | Lines | Depth |
|------|------:|-------|
| `internal/drivers/local.go` | 1,210 | fully (all path sites, `AtomicWrite`, List, Exists, HealthCheck, the 40 non-interface helpers) |
| `internal/drivers/s3.go`, `s3compat.go`, `s3upload.go` | 212 / 181 / 144 | fully |
| `internal/drivers/lyve.go`, `lyve_console.go` | 259 / 151 | fully |
| `internal/drivers/idrive.go`, `idrive_regions.go` | 428 / 85 | fully |
| `internal/drivers/geyser.go` | 363 | fully |
| `internal/drivers/quotaless.go` | 281 | fully |
| `internal/drivers/r2.go` | 255 | fully |
| `internal/drivers/onedrive.go` | 1,100 | fully |
| `internal/drivers/transport.go`, `sparse_unix.go`, `xattr_unix.go`, `locking_unix.go`, `sparse_fallback.go`, `xattr_other.go` | 105 / 106 / 104 / 54 / 52 / 20 | fully |
| `internal/drivers/plugins/transform/main.go` | — | `//go:build wasm` stub for the dead `wasm.go` (D-2); read only |
| `internal/drivers/geyser_admin.go` | 1,282 | **not reviewed** — tool library for `cmd/geyser-{admin-test,cloudsync-probe,console-probe,smoke}` (confirmed the four importers); 49/49 funcs unreachable from `cmd/vaultaire` |
| `internal/drivers/{capabilities,egress_tracker,cost_advisor,wasm,watch}.go` | 43 / 82 / 233 / 57 / 26 | signatures + reachability only (class-B, D-2/D-9) |
| `internal/engine/interface.go`, `failover.go` (`isBackendFailure`, `isSDKNotFound`), `errors.go`; `internal/api/not_found.go`; `internal/common/context.go` | — | the contract and the two classifiers every error shape below was checked against |
| `cmd/vaultaire/main.go:166-373` | — | how each driver is constructed and named |
| `internal/api/backend_probes.go`, `server.go:465-585` (probe loop), `health_handlers.go`, `deploy/monitoring/vaultaire-backends.yml` | — | fully |
| `internal/api/s3_engine_adapter.go:940-990`, `:2066-2085` (`bucketRegionDriver`), `s3_buckets.go:183-194`, `dashboard/handlers/{buckets,bucket_settings,compliance}.go` (region calls) | — | region-pinned routing only |
| READMEs: `lyve_README.md` (1,081 — contract, homing, WORM, probe, 09-27 re-check sections), `onedrive_README.md` (placement/rules/transport sections), `quotaless_README.md`, `geyser_README.md`, `idrive_README.md`, `idrive_integration.md`, `pixeldrain_README.md`; `docs/DRIVERS.md`; `internal/drivers/CLAUDE.md`; `.private/IDRIVE_RESELLER_API.md` § 4 | — | read for the claims checked below |
| SDK sources | — | `aws-sdk-go-v2@v1.42.1` (`aws/retry/standard.go`), `config@v1.32.30` (`resolve.go:194,209` checksum defaults), `service/s3@v1.105.2`, `service/internal/checksum@v1.9.23/middleware_compute_input_checksum.go:140-180`, `feature/s3/manager@v1.22.34/upload.go:444-530,736-760` |

**Tests run:** `go build ./...`, `go vet ./internal/drivers/`, `go test -race -short ./internal/drivers/...`
→ `ok (39.8 s)` on `main`; the new fake-endpoint tests (below) on the branch. `deadcode ./cmd/vaultaire`
re-run for the package (list under *Dead code noted*).

**Live checks (credentials never printed; nothing written to a customer bucket):**

| Check | Backend / creds | Result |
|-------|-----------------|--------|
| Signed HeadBucket, same key, real regional host vs the driver's table host | iDrive **us-west-2** key (LA), not the prod pair | `s3.us-west-2.idrivee2.com` → `NotFound 404` (no `vaultaire` bucket in that region); `e2-us-west-2.idrive.com` (table host) → `NotFound 404` — the legacy host does answer signed S3 calls |
| Prod's primary pair against another region's real endpoint | iDrive primary (us-central-1) key vs `s3.us-west-2.idrivee2.com` | **`Forbidden 403`** — per-region keys confirmed; every `idrive-<region>` driver in prod runs on this pair |
| Object-level shapes on iDrive | — | **not run**: no `vaultaire` bucket exists outside the prod region and creating one is out of scope; the prod region is the prod endpoint + key (forbidden) |
| Get / GetRange / Exists / Delete / List of a missing key | R2 (`vaultaire-public`, reads only) | `Get`, `GetRange` → typed `NoSuchKey` + HTTP 404 (`*fmt.wrapError` chain); `Exists` → `(false, nil)`; `Delete` → nil (idempotent); `List` empty container → `[] nil`; the token sees exactly one bucket (no scratch bucket → no writes) |
| Object Lock / versioning of the Lyve failover buckets | prod `LYVE_*` from the box, `get-object-lock-configuration` (read-only) | `stored-us-east-1`, `stored-us-west-1`: **`ObjectLockConfigurationNotFoundError`**, versioning unset. HEAD of a missing key with the root key → `404 Not Found` (no body) |
| Prod routing rows (`object_head_cache.backend_name`, read-only) | prod DB from the box | `onedrive` 2,613 rows / 880 MB, `idrive` 2,038 / 2.0 GB, `local` 452 / 567 MB, `NULL` 62 / 12 GB (non-chunked), `r2` 26, `permafrost` 25, `lyve` 7 / 576 MB, `geyser` 2 / 48 MB. `DATA_PATH` (`/opt/vaultaire/data`) holds **10 files, 120 KB, 3 containers** (all `letshow`). `buckets.region`: `us-west-1` × 25, `eu-west-1` × 1 (test tenant, 0 objects). `/tmp` is the root ext4 (3.4 TB free), `PrivateTmp=no` |
| `deploy` / `cmd/backend-matrix` | — | the matrix tool needs the same env as prod; with no creds in this environment or in SLC's `.env.bench` (which now holds load-test logins only) it would skip every backend, so it was not run; the `live`-tagged tests above replace it |

The live tests are kept as `internal/drivers/r7_live_test.go` (`//go:build live`; `R7_IDRIVE_REGION_PREFIX`
selects a non-prod region key; it refuses the prod endpoint).

**Skipped and why:** `geyser_admin.go` internals (tool library, R0 row confirmed); `cost_advisor.go` /
`egress_tracker.go` / `wasm.go` bodies (class-B, decisions D-2/D-9 pending — not acted on); the S3 API
handlers around the drivers (R2/R3/R4); the chunk pipeline that calls `Put` per chunk (R8); the
smart-demotion/promotion jobs that call drivers directly (R13 — only their `Put` option usage was
checked, `smart_demotion.go:336`, `smart_promotion.go:266`).

## Findings

Severity: P0 data loss / security / cross-tenant / money · P1 wrong behaviour a customer hits · P2
correctness risk or unsafe pattern not yet triggered · P3 quality / dead code / docs drift.

| ID | Sev | file:line | What | Why it matters | Proposed fix |
|----|-----|-----------|------|----------------|--------------|
| R7-01 | **P1** | `internal/drivers/idrive_regions.go:4-24`; `cmd/vaultaire/main.go:289-326`; `internal/api/s3_buckets.go:189-194`; `s3_engine_adapter.go:2066-2085`, `:949-963`; `dashboard/handlers/buckets.go:138-164` | The region table behind bucket-level data residency does not describe the account. It lists eight AWS-style ids (`us-west-1`, `us-west-2`, `us-central-1`, `us-east-1`, `eu-west-1`, `eu-central-2`, `eu-west-2`, `eu-south-1`) on `https://e2-<region>.idrive.com`. The reseller account (`.private/idrive-keys-2026-09-20.env`, hostnames) has **13** regions on `https://s3.<region>.idrivee2.com`: `us-central-1` (prod's primary), `us-west-2`, `us-west-4`, `us-southwest-1`, `us-southeast-1`, `us-midwest-1`, `us-east-1`, `eu-west-1`, `eu-west-3`, `eu-west-4`, `eu-central-1`, `eu-south-1`, `ap-northeast-1`. `us-west-1`, `eu-central-2`, `eu-west-2` do not exist; the display names are wrong (`us-west-2` is Los Angeles, not Dallas; `us-central-1` is the primary, not "Chicago"); `main.go:293` defaults `IDRIVE_ENDPOINT` to a host that is not in the account. Prod sets no `IDRIVE_<REGION>_*` pair, so all eight `idrive-<region>` drivers sign with the primary pair — **live: the primary pair answers `403 Forbidden` in another region**, and no `vaultaire` bucket exists outside the primary region (`404` with that region's own key). `bucketRegionDriver` treats `us-west-1` as "the default" (`:2074`) while prod's real region is `us-central-1`, so a bucket honestly labelled `us-central-1` would leave the primary for a driver that cannot work. Region-pinned PUTs bypass the engine and the breaker (`:949-963`) and answer 5xx via `bodyReadErrorCode`. `CreateBucket` and the dashboard accept any table region; prod holds one `eu-west-1` bucket (test tenant, empty) and 25 labelled `us-west-1`. None of the eight drivers is probed (R1-12). | The data-residency feature (5.14.7, sold in the dashboard) cannot store a single object, fails loudly only at the first PUT, and mislabels every default bucket. An EU customer's bucket would 5xx forever with no alert. | **WP-R7-1** (not fixed here — region ids, existing `buckets.region` rows, dashboard copy and per-region bucket provisioning are one product change): rebuild the table from the reseller `GET /regions` (`region_key`, `region_name`, `storage_dn` per user), map "default" to the primary's region, refuse to register an `idrive-<region>` driver whose region has no dedicated key pair *and* log it at Error, provision `IDRIVE_BUCKET` per region (or one bucket name per region), migrate the 25 `us-west-1` rows, and probe every registered region driver (the probe hook is added in this PR — R7-19). |
| R7-02 | **P1** [YOU] | prod `/opt/vaultaire/configs/.env` (`LYVE_ACCESS_KEY` = account root, `LYVE_PROBE_*` = the same root); `lyve_README.md:1031-1034` | The Lyve data plane runs on the **account root key** with TFA off and `PwdMustChange: true`. The scoped IAM user `vaultaire-prod` (policy `vaultaire-prod-stored-buckets`) has existed since 2026-09-23 and is unused. A leaked data-plane key today = whole-account delete (root cannot escape COMPLIANCE, but our buckets have no lock — R7-16). | Security: the broadest credential in the fleet sits in the hottest code path. | [YOU]: set `LYVE_ACCESS_KEY/SECRET` to the `vaultaire-prod` user, keep `LYVE_PROBE_*` = root (RSCustomerDetails is root-only) **or** drop the console probe: after this PR, with `LYVE_PROBE_*` unset the Lyve probe is the driver's signed `HeadBucket` (works with the scoped user) instead of the data-plane key against the console (R7-19). Enable TFA on root. Do not create users from this session (rule). |
| R7-03 | P2 | `internal/drivers/geyser.go:223-252`, `:258-297` | `Put` ignores every `PutOption` — including `ContentLength`, which the interface comment says Geyser needs (`interface.go:140-142`) — and always `materialize`s: ≤64 MiB bodies are copied into a `bytes.Buffer` (64 MiB RAM per concurrent archive PUT, unbounded across requests), larger bodies are written to `os.CreateTemp("", …)` (the OS temp dir, not `DATA_PATH`) and re-read, so every Vault PUT >64 MB (the V18.2 whole-object rule) costs 2× disk I/O and the full write time before the first byte reaches Vail. `ContentType`/metadata are dropped. | Memory profile: N archive PUTs = N × 64 MiB + N temp files; the "stream, never buffer" rule is violated on the one tier where objects are largest. Prod `/tmp` is the 3.4 TB root ext4, so capacity is not the risk — RSS and latency are. | **WP-R7-2**: when `ContentLength > 0` send the reader straight to `PutObject` with `ContentLength` set (the SDK already sends `aws-chunked` + CRC32 trailer for seekable bodies today, so the wire format does not change); keep `materialize` only for unknown lengths; verify against live Vail before merging (no Geyser creds outside prod). |
| R7-04 | P2 | `internal/drivers/quotaless.go:83-91`, `:95-125`, `:135-151`, `:161-177`, `:192-207`, `:230-246`; `s3.go:67-72`; `quotaless_README.md:79-100`, `:328-333` | `Put` `io.ReadAll`s the whole body into RAM (unbounded) and then `S3Driver.Put` `materialize`s it again; every operation retries 3× with `time.Sleep(1 s, 4 s)` that ignores `ctx`, including on a miss (a 404 costs 5 s and three round trips before the caller sees it — R6 contract (d)); `useMultipart`/`chunkSize`/`uploadCutoff` are stored and never read. The driver's own ops manual says the AWS SDK client must not be used against the Minio gateway (checksum middleware "corrupts data on download", raw HTTP + `UNSIGNED-PAYLOAD` required) — the wired driver uses the SDK with default `WhenSupported` checksums. | Not in prod (no creds; the account's key is 401-dead) and slated for removal by M12 (D-decisions). If it were wired: OOM per large PUT, HAProxy-cut retries, and, per its README, corrupted reads. | **WP-R7-3**: delete `quotaless.go` + `S3Driver` (or rewrite on the shared `s3upload` path with ctx-aware, miss-exempt backoff and no in-request sleeps). Not touched here. |
| R7-05 | P2 | `s3compat.go:153`, `s3.go:139`, `lyve.go:242`, `idrive.go:368`, `geyser.go:344`, `r2.go:240`; `onedrive.go:244-246` | `Exists` classifies a miss with `strings.Contains(err.Error(), "NotFound") \|\| strings.Contains(err.Error(), "404")` on six drivers; permafrost's `odIsNotFound` is `strings.Contains(err.Error(), "404")` over a message that embeds up to 512 bytes of the Graph error body. A request id, host or message containing `404` reads as "absent"; a `NoSuchBucket` 404 reads as "absent" (it is a misconfigured backend); a 403 for a missing key (vendors without `ListBucket` on the key) is an error, not a miss. | Wrong `Exists` answers for the two tools that call it (`cmd/backend-matrix`, `cmd/erasure-bench`) and a contract deviation from R6's typed taxonomy. | **Fixed here** for the six S3-class drivers: `s3IsNotFound` (typed — `smithy.APIError` `NoSuchKey`/`NotFound`, or `*smithyhttp.ResponseError` 404; `NoSuchBucket` excluded) with a table-driven test against a real `s3.Client` and `httptest` (404 empty body, 404 `NoSuchKey` XML, `NoSuchBucket`, 403, 500, connection reset, request id containing `404`). Permafrost → WP-R7-4 (typed Graph error). |
| R7-06 | P2 | `s3compat.go:119-142`; `s3.go:112-130`; `geyser.go:313-332`; `local.go:131-154`; `onedrive.go:522-572` | `List` contract (R6 table): keys relative to the container, filtered by `prefix`, **all pages**. s3compat lists the container but strips `len(prefix)` bytes (the artifact prefix, not the `personal-files/vaultaire/<container>/` key prefix) and never applies `prefix`; s3compat, s3 (→ quotaless) and geyser read **one** `ListObjectsV2` page (1,000 keys). local ignores `prefix` entirely, returns in-flight `.tmp-*` temp files as objects, and walks the whole container (lstat per file). permafrost lists **direct children** of the container folder only: keys containing `/` are stored as nested folders (`odEscapePath` preserves `/`), so nested keys never appear and folder names appear instead. | The no-DB listing fallback (`s3_list.go` → `engine.List` → primary) returns wrong names on s3compat, truncated sets on s3/geyser, temp files on local, and a flat top level on permafrost. | **Fixed here**: s3compat/s3/geyser paginate with `NewListObjectsV2Paginator`, s3compat strips the key prefix and honours `prefix`; local applies `prefix`, skips `.tmp-*`, resolves the container through `resolvePath`. Tests: two-page fake endpoints per driver, local prefix/temp-file test. Permafrost → WP-R7-4 (recursive listing via `/drives/{id}/root:/…:/delta` or a walk). |
| R7-07 | P2 | `onedrive.go:407-464` (`downloadRanges`), `:306-331`, `:968` | `Get` of any object ≥10 MiB opens 2–8 parallel range requests and **`io.ReadAll`s every range** before the pipe writer emits them — the whole object sits in RAM per GET (a 5 GB object = 5 GB). `Put` without `ContentLength` `materialize`s (64 MiB RAM or a temp file) and then `io.ReadAll`s the result again into `chunkedUpload(data []byte)` — the whole object in RAM a second time. The streaming path allocates a 60 MiB chunk buffer per PUT (`make([]byte, odChunkSize)`), not pooled. | Permafrost is registered in prod but targeted by nothing (R6 Q6), so this is dormant; `cmd/onedrive-bench` and any future parity job would hit it. | **WP-R7-4**: stream ranges through a bounded window (one buffered range ahead), pool the 60 MiB chunk buffer (or drop to 10 MiB per the README's 5–10 MB guidance), remove the `materialize`+`ReadAll` fallback in favour of the streaming path with a spooled length. |
| R7-08 | P2 | `onedrive.go:652-727` (`graphDo`), `:916-940` (`putChunk`) | In-request retries with `time.Sleep`: `graphDo` makes up to 5 attempts with decorrelated jitter capped at 30 s **and honours `Retry-After` unbounded** (a 429 with `Retry-After: 120` sleeps 120 s inside the request); `putChunk` sleeps 1/2/4 s. None of the sleeps select on `ctx`. R6 contract (d): the breaker and the client are the retry policy. | HAProxy cuts the client at 50 s; the request keeps sleeping and retrying against Graph after the client is gone, then reports `context.Canceled` (not a backend failure) — the outage is invisible to the breaker. | **WP-R7-4**: `select { case <-ctx.Done(): case <-time.After(wait) }`, cap `Retry-After` at the remaining ctx budget, at most 2 attempts on the request path. |
| R7-09 | P2 | `onedrive.go:123-161`, `:214-222`; `onedrive_README.md:453-458` | The "never reorder or remove `TENANT_N_*`" rule is documented but neither enforced nor logged: the fleet is built from `TENANT_1..15` skipping gaps, so removing `TENANT_2` silently renumbers tenant 3 to index 1 and remaps every object's home (reads survive only through the fleet-walk fallback, deletes sweep so they survive too). Nothing records the fleet order anywhere. | A config edit becomes a silent fleet-wide remap with a permanent fallback-walk penalty. | **WP-R7-4**: persist a fleet fingerprint (ordered tenant ids) in `feature_flags`/a `permafrost_fleet` row at first boot; on mismatch log at Error and export `vaultaire_permafrost_fleet_mismatch 1`; document the rebalance procedure. |
| R7-10 | P2 | `onedrive.go:191`; prod `object_head_cache` (2,612 rows `backend_name='onedrive'`, one more under a versioned test bucket) | `Name()` returns `"onedrive"` while `main.go:352` registers the driver as `"permafrost"`. The engine records the **registration key** (`engine.go:372,434`), and the tools register the same driver as `"onedrive"` (`cmd/dedup-migrate/main.go:459`, `cmd/backend-matrix/main.go:80`, `cmd/erasure-bench/main.go:616`) — the 2,613 prod rows carrying `onedrive` were written that way in May 2026 (dedup-migrate against the bench tenant); no driver in the product binary answers to it, `HintBackend("onedrive")` cannot resolve, and those objects are readable only through R6-07's fan-out — which WP-R6-1 intends to remove. All rows belong to the bench tenant `vbench-user1-vaultaire`. | Renaming a driver's registration key orphans routing truth silently; WP-R6-1 would make those objects unreadable. | **Fixed here**: `Name()` = `"permafrost"` (so the two names cannot drift again) + test. **WP-R7-5**: a boot-time check that every distinct `backend_name` in `object_head_cache` has a registered driver (Warn + metric), and a one-off reconciliation of the bench rows (`onedrive` → `permafrost`). |
| R7-11 | P2 | prod `object_head_cache`: 452 rows `backend_name='local'` (447 from 2026-05-12/13, bench tenant; 3 `letshow`; 2 bench 07-29) vs `DATA_PATH` = 10 files; 62 rows `backend_name IS NULL`, 12 GB, non-chunked | Head rows point at bytes that are not there: 447 `local` rows exist for a `DATA_PATH` holding ten files — those objects (bench data) are gone and no alert or listing says so; 62 rows have no recorded backend at all (pre-`backend_name` era) and are served only by fan-out. `DATA_PATH` itself is **not backed up** (R9-12: the nightly dump covers the DB + configs only), so a hub-disk loss loses every `local`/`REDUCED_REDUNDANCY` object outright while the head rows survive and keep answering HEAD 200. | Routing truth without an integrity check; `local` is a single-copy tier with no backup. | **WP-R7-5**: reconciliation job (head row ↔ driver `Exists`, sampled) + a `vaultaire_head_rows_unresolvable` metric; decide `local`'s role (hub cache only, never a durable class — today `REDUCED_REDUNDANCY` maps to it, `storage_class.go`) and either back `DATA_PATH` up or refuse durable writes to it. |
| R7-12 | P2 | `s3upload.go:63-73`; `manager@v1.22.34/upload.go:444-530`, `:736-760` | For bodies larger than one part (>16 MiB) the SDK uploader ignores `PutObjectInput.ContentLength` on a non-seekable body (`initSize` sets `totalSize=-1` unless `io.Seeker`): it reads parts until the reader returns `io.EOF` and commits whatever arrived. Today this is safe **only** because Go's server body returns `io.ErrUnexpectedEOF` (not `EOF`) on a short `Content-Length` body and the `aws-chunked` decoder errors on truncated framing — `shouldContinue` treats any non-EOF error as fatal and aborts. A reader that ends cleanly short of the declared size (a future wrapper, a proxy that strips length) would commit a truncated object under the declared size (R2-06 residue for the >16 MiB path). The ≤16 MiB path is guarded explicitly (`putSinglePartIfSmall`, R2-06). | Correctness depends on a transport property, not on the driver's own check. | **WP-R2-1 / small**: wrap the body in a counting reader and, when `size > 0` and the uploader returns without error but the count is short, return an error — after WP-R2-1 (write-new-key-then-swap) the truncated blob is a fresh key and can be deleted; before it, deleting would destroy the overwritten object (R2-05 class), so only the error is safe now. Test added here proves the `ErrUnexpectedEOF` path aborts after >1 part. |
| R7-13 | P2 | `transport.go:82-98`; `geyser.go:82`; `onedrive.go:1016-1076`; SDK `aws/retry/standard.go:29-32` | No driver sets a per-operation deadline. `TunedHTTPClient` sets dial 10 s, TLS 10 s, `ResponseHeaderTimeout` **0** (unbounded) except Geyser (5 min); permafrost's three transports set none. The SDK retryer defaults to 3 attempts / 20 s max backoff, so one stalled backend call can occupy a request for minutes; HAProxy cuts the client at 50 s and the driver then sees `context.Canceled` — which R6-06 correctly excludes from the breaker. A stalled backend therefore never trips its breaker (WP-R6-3). | Outage invisible to routing; slow first byte = HAProxy 504 with no backend signal. | **WP-R6-3 values** (see *Transport and limits*): `ResponseHeaderTimeout` 20 s (S3-class hot), 60 s (Geyser — Vail answers `InvalidObjectState` immediately, so cold objects do not need the 5 min; restores are async), 20 s (Graph API); per-op `context.WithTimeout` inside the driver: Get/GetRange first byte 25 s, HEAD/Delete/List 15 s, Put total = 60 s + 1 s/MiB of `ContentLength` (unknown length: 15 min), all below HAProxy only for first-byte; SDK `RetryMaxAttempts` 2 on the request path. Surface as `context.DeadlineExceeded` (a breaker failure). |
| R7-14 | P2 | `idrive.go:187-288` | `PutWithSize`/`putMultipart` (no callers): `abortMultipartUpload(ctx, container, artifact, …)` at `:219` aborts with the **S3-layer container/artifact instead of `d.bucket`/`key`** (the two later calls use the right pair), so a read error mid-upload leaves the multipart orphaned and billed; parts are `make([]byte, 5 MiB)` per iteration. `egressTracker` (`:125-135`) keys on a package-private `TenantIDKey` = `"tenant_id"` that nothing sets (the real key is `common.TenantIDKey` = `"tenant-id"`), so it could never attribute a byte. `ValidateAuth`, `LoadIDriveConfig`, `NewIDriveDriverFromConfig` unreachable. | Dead code with a latent bug that would bill orphaned parts if revived. | **WP-R7-6** (with WP-R0 D-9): delete `PutWithSize`/`putMultipart`/`abortMultipartUpload`, the egress tracker wiring and `TenantIDKey`, `ValidateAuth`, the two config loaders; `idrive_integration.md` describes eight deleted features (BandwidthQuota, SmartCache, RegionalFailover…) — delete or rewrite. |
| R7-15 | P2 | `r2.go:132-141`; `common/context.go:19-24`; `r2.go:54`, `:47-53` | Tenant-prefix escape: the key is `t-<tenant>/<container>/<artifact>` and S3 keys are opaque — the SDK percent-encodes each segment and does **not** normalise dot segments (new `httptest` test captures the raw request path for `a/../b` and asserts it is sent literally), and R2 answered `NoSuchKey` for such keys in the live read check; `/`-leading or empty artifacts produce `…//` keys, still inside the prefix. The only escape is the **empty tenant** → `t-default/` (R6-22): any caller without a tenant in `ctx` shares one prefix. `us` jurisdiction is accepted by `R2Endpoint` although its endpoint fails TLS (2026-09-24). The driver has **no** guard against non-PUBLIC use — by design the guard is `api.resolvePutStorageClass` (PUBLIC class only when the bucket is public-read) + `engine.targetOnlyBackends` (R6-04); documented here. | Isolation holds at the key level; the `default` fallback is the residual hazard (engine WP-R6-1). | Document (this file, `r2.go` header); WP-R6-1 refuses missing-tenant writes; `R2Endpoint` should reject `us` until Cloudflare fixes it (one line, WP-R7-6). |
| R7-16 | P2 | `lyve.go:133-181`; `engine/engine.go` write candidates (R6-04); live `get-object-lock-configuration` | **WORM vs failover writes (WP-R6-8 question) — answered:** Lyve is the only general-purpose failover leg; a failover PUT lands in `stored-<region>` under `t-<tenant>/…`. Both prod buckets (`stored-us-east-1`, `stored-us-west-1`) have **no Object Lock configuration and versioning off** (verified read-only from the box), so a failover write cannot create a retained version today. If a lock with a default retention were ever enabled on that bucket, every failover write would become undeletable until the retention lapsed (the SDK returns success on the PUT and `AccessDenied` on the later `DeleteObject`, which `isBackendFailure` charges as a backend failure) — an erasure request could not be honoured. The COMPLIANCE lock live to 2053 in the account (memory) is on another bucket. | Decision recorded; the hazard is one console click away. | **WP-R7-7**: `NewLyveDriver` calls `GetObjectLockConfiguration` once at boot; if enabled, log at Error and register the driver as target-only (the engine's `targetOnlyBackends` needs a driver-declared flag rather than a name list). [YOU]: never enable a default retention on `stored-*`. |
| R7-17 | P3 | `geyser.go:82`, `:113-128`, `:168-218`; `engine/failover.go` (`ErrArchived` excluded) | Cold-read path conforms: Vail's `InvalidObjectState` → `engine.ErrArchived` (wrapped `%w`) on Get/GetRange/Restore/RestoreStatus; `isBackendFailure` excludes it; `RestoreAlreadyInProgress` → typed sentinel; `RestoreStatus` passes `x-amz-restore` through. The ~13-day eviction window lives only in comments (`:115-117`) and the README; nothing in code depends on it. `ResponseHeaderTimeout` 5 min is for restores that never go through this client (restores are async `RestoreObject` + later GET). | Nothing to fix on the recall path; the 5-min header timeout is the R7-13 item. | Document; fold the timeout into WP-R6-3. |
| R7-18 | P3 | `lyve.go:253-259`; `local.go:157-163`; `s3compat.go:162-176`; `quotaless.go:252-264` | `HealthCheck` kinds: idrive/geyser/r2/**lyve** = signed `HeadBucket` (lyve returns the SDK error **unwrapped**, no context); s3compat = signed `ListObjectsV2 MaxKeys=1` (authenticated, but a list); quotaless = signed `ListObjectsV2` with **no** `MaxKeys` (lists up to 1,000 keys per probe); permafrost = authenticated Graph `GET /drives/{id}` on **one rotating tenant** per call (a dead tenant is observed 1/N of the time); local = `os.Stat(basePath)` (a full or read-only disk passes). All authenticated where a credential exists → rule 1 holds; no bare GET anywhere. | Probe quality, not correctness. | Wrap lyve's error; `MaxKeys: 1` on quotaless (or delete with WP-R7-3); permafrost probe all tenants and report the worst (WP-R7-4); local: `statfs` free-space check (WP-R7-6). |
| R7-19 | P2 | `internal/api/backend_probes.go:69-96`; `deploy/monitoring/vaultaire-backends.yml` | (R1-12 / WP-R1-6) Only `idrive`, `geyser`, `r2` and `lyve` are probed. The eight `idrive-<region>` drivers and `permafrost` never are; with `LYVE_PROBE_*` unset the Lyve probe signs the **console** action with the data-plane key (root today), which will fail the moment R7-02 moves that key to a scoped user. | Dead regional key or dead permafrost token = no alert; R7-02 cannot be executed without a false alert. | **Fixed here**: every registered `idrive-<region>` driver **with its own key pair configured** gets a staggered signed `HeadBucket` (regions on the primary pair are known-403 — R7-01 — and are not probed so the fix does not page eight times forever); `permafrost` gets the driver's authenticated probe when `TENANT_1_ID` is set; Lyve uses the console action only when `LYVE_PROBE_*` is set, else the driver's signed `HeadBucket`, else TCP. Rules: `BackendProbeFailing` excludes `permafrost` (warning rule added, like Lyve) — **install on SLC = ops step**. |
| R7-20 | P3 | `local.go:79`, `:104-110`, `:688-727`, `:947-1010`, `:1013-1023`; `sparse_unix.go:22`, `:96` | Local driver details: `Get` = `os.Open` — **symlinks inside `DATA_PATH` are followed** (decision: acceptable; only an operator can create one; documented), and a key naming a directory opens successfully and fails on first `Read` (`EISDIR`, a backend failure). `AtomicWrite`: temp `.tmp-*` in the **same directory**, `fsync(file)`, close, `rename` — but **no `fsync` of the parent directory**, so after a crash the rename itself may not be durable (POSIX); temp removed on every error path (partial writes never surface); files 0600 (`CreateTemp`), directories 0750, except `UploadPart`/`CompleteMultipartUpload`/`CreateSparse` which use 0755 and `os.Create` (truncating) — none are engine-reachable. `Delete` leaves empty parent directories forever. `Exists` skipped `resolvePath` (fixed here). xattr/sparse: only in the non-engine helpers; xattr failure surfaces as `ENOTSUP` from `Setxattr` — never called by product code. `GOOS=windows` does not build (`syscall.Stat_t`, R0-19). Restore from backup: **nothing restores `DATA_PATH`** (R9-12) — the DB backup restores head rows, quota and versions for `local` objects whose bytes are gone (R7-11). | Crash-durability gap is theoretical on ext4 with `data=ordered` but real on other filesystems; the rest is hygiene. | `fsync` the parent dir after rename (WP-R7-6); prune empty dirs on Delete; move the 40 unused helpers under D-9. |
| R7-21 | P3 | `s3.go:18-63`, `:67`; `drivers/CLAUDE.md` table | `S3Driver` does **not** implement `engine.Driver` (`Put` has no options, no `Name`, no `HealthCheck`); it exists only as `QuotalessDriver`'s embedded base and as the raw client `cmd/backend-matrix` / `cmd/erasure-bench` wrap. `NewS3Driver` uses `context.TODO()` for `LoadDefaultConfig`. `UsePathStyle` comment says "Lyve Cloud uses virtual hosted-style" (irrelevant). | Docs list it as a driver. | Goes with WP-R7-3. |
| R7-22 | P3 | `docs/DRIVERS.md:11-25`, `:117-119`, `:224-233`; `drivers/CLAUDE.md` (S3Compat "maps to Quotaless endpoint", `idrive_integration.md`) ; `idrive_README.md` (pricing $0.004/GB) | `DRIVERS.md` is fiction (R6-19): `Put` without options, `List(ctx, container)`, `GetMetrics()`, `engine.ErrNotFound`/`ErrAccessDenied` sentinels, `engine.RegisterDriver`. `idrive_integration.md` documents `BandwidthQuota`, `EgressPredictor`, `SmartCache`, `RegionalFailover` — all deleted by R0. `idrive_README.md` prices are stale. | R14 needs the truth. | The *Contract conformance* + *Transport and limits* sections below are the replacement text (WP-R6-9). |
| R7-23 | P3 | `s3upload.go:30-33`, `:80-92`; `api/s3_engine_adapter.go` chunk path (`CHUNK_PUT_CONCURRENCY` = 8) | Double parallelism — **not present**: the chunk path hands drivers ≤16 MiB bodies, which take the single-`PutObject` branch (`putSinglePartIfSmall`), so the manager's 8-way concurrency is only used by whole-object PUTs >16 MiB (resilient/public/multipart-complete). Worst case per PUT: 8 in-flight parts + 1 pooled = **9 × 16 MiB = 144 MiB** (+ the API's own buffers, R2 table); per chunked PUT: 8 workers × 16 MiB single-part buffers = 128 MiB (already inside R2's 576 MiB figure). `uploaderCache` is keyed by client pointer (fixed set of clients — cannot grow). | Confirms the R2 memory table. | Document. |
| R7-24 | P3 | all S3-class constructors; `r2.go:105-106`; `config@v1.32.30/resolve.go:194,209` | Checksums: `LoadDefaultConfig` (idrive, lyve, geyser, s3/quotaless) and `s3.New(s3.Options{})` (s3compat) resolve to `RequestChecksumCalculationWhenSupported` / `ResponseChecksumValidationWhenSupported`, so every non-empty PUT over HTTPS is sent as `Content-Encoding: aws-chunked` + `x-amz-content-sha256: STREAMING-UNSIGNED-PAYLOAD-TRAILER` + `x-amz-checksum-crc32` trailer (`checksum@v1.9.23:165-176`) — iDrive, Lyve and Geyser accept it (prod PUTs work; Lyve README `:503`, Geyser README `:121`); **R2 rejects it (501)** and opts out (`WhenRequired`); Quotaless's Minio gateway is documented as incompatible (R7-04). Response validation only triggers when the server returns a checksum header; the Lyve `CopyObject` CRC bug (README `:1055-1059`) needs `ChecksumMode: ENABLED`, which no driver sets → drivers immune. | Vendor matrix for R14; one known-bad default per vendor. | Document; make the checksum mode an explicit per-driver setting rather than the SDK default (WP-R7-6). |
| R7-25 | P3 | `lyve.go:35-67`, `:88-90`; prod `LYVE_REGION=us-east-1`; `lyve_README.md:242-249`, `:839-870` | Bucket homing: the driver derives both endpoint and bucket from one region (`s3.<r>.global.lyve.seagate.com` ↔ `stored-<r>`), so it can only ever address a bucket homed where it talks — the cross-region proxy penalty (~500 ms/op) is designed out as long as `stored-<region>` was created through that region's endpoint (verified for east/west, README `:864`). Prod runs `us-east-1` (`stored-us-east-1`, 576 MB, 7 objects), not the `us-west-1` the code/README call "closest to SLC" — consistent, just not the documented default; the probe address follows `LYVE_REGION` (`server.go:553`) so the "prod dials us-east-1 vs driver default" nit from memory is a non-issue. | Config/doc drift only. | Note in README; [YOU] decide east vs west homing before customer volume. |
| R7-26 | P3 | `idrive.go:172-185`, `lyve.go:155-170`, `r2.go:157-164`, `local.go:89-101` | Put options honoured: `ContentLength` — idrive ✓ lyve ✓ r2 ✓ s3compat ✓ onedrive ✓ geyser ✗ quotaless ✗ local n/a; `ContentType` — idrive ✓ lyve ✓ r2 ✓ s3compat ✓, geyser/onedrive/quotaless/local ✗ (dropped); `CacheControl`/`ContentEncoding`/`ContentLanguage`/`UserMetadata` — **lyve only**; `StorageClass` — no driver (correct: it is an engine routing hint, `storage_class.go`). Only `WithContentLength` and `WithStorageClass` are ever passed by product code (`s3_engine_adapter.go:941-943`, `:1481`, `s3_multipart.go:481-483`, `smart_*`), so the metadata gaps are latent; the head cache is the metadata source of record. | Contract table entry. | Document; drop the unused options or make every driver honour `ContentType` (WP-R7-6). |

## Contract conformance

Columns follow R6's *Driver contract as the engine assumes it*. "conforms" = as `main` @ `53dbfdb`;
"→ fixed" = changed in this PR.

### local (`local.go`) — registered `local`; hub disk; `REDUCED_REDUNDANCY` class; target-only

| Method | On missing | List semantics | Exists cost / shape | HealthCheck | Streaming | ctx | Put options | Conforms? |
|---|---|---|---|---|---|---|---|---|
| `Name` | `"local"` = key | | | | | | | ✓ |
| `Get` | `*fs.PathError` wrapping `ErrNotExist` ✓; escape → `engine.NotFoundError` (R2-04) | | | | `os.Open` → file handle; directories open and fail on read | ctx unused (fs) | | ✓ (dir case P3) |
| `GetRange` | not implemented → engine discards `offset` (R6-23) | | | | streamed | | | n/a |
| `Put` | n/a | | | | `io.Copy` into same-dir temp + `fsync` + `rename`; temp removed on error | unused | all dropped (metadata lives in head cache) | ✓ (no dir fsync, R7-20) |
| `Delete` | `*fs.PathError` ✓ | | | | | | | ✓; empty dirs remain |
| `List` | empty slice for a missing container ✓ (errors swallowed) | **ignored `prefix`**, returned `.tmp-*`, container not via `resolvePath`, full walk | | | | | | ✗ → **fixed** |
| `Exists` | `(false, nil)` ✓ | | `os.Stat`; **no `resolvePath`** | | | | | ✗ → **fixed** |
| `HealthCheck` | | | | `os.Stat(basePath)` — passes on a full/read-only disk | | | | weak (P3) |

### s3compat (`s3compat.go`) — registered `s3` when `S3_ACCESS_KEY` set (not in prod); hard-wired `us.s3compat.cloud:8000`, bucket `data`, prefix `personal-files/vaultaire`

| Method | On missing | List semantics | Exists | HealthCheck | Streaming | ctx | Put options | Conforms? |
|---|---|---|---|---|---|---|---|---|
| `Name` | `"s3compat"` ≠ key `"s3"` | | | | | | | ✗ (P3, R7-10 class) |
| `Get` | SDK `%w` ✓ (typed) | | | | body returned unread ✓ | ✓ | | ✓ |
| `GetRange` | — | | | | not implemented | | | n/a |
| `Put` | | | | | `s3ParallelUpload`: ≤16 MiB one exact buffer, else 16 MiB × 8 | ✓ | `ContentLength`, `ContentType` ✓; metadata dropped | ✓ |
| `Delete` | SDK `%w` ✓ | | | | | ✓ | | ✓ |
| `List` | empty ✓ | **stripped `len(prefix)`, ignored `prefix`, one page** | | | | ✓ | | ✗ → **fixed** |
| `Exists` | | | HeadObject; **`"404"` substring** | | | ✓ | | ✗ → **fixed** |
| `HealthCheck` | | | | signed `ListObjectsV2 MaxKeys=1` | | ✓ | | ✓ (authenticated) |

### s3 (`s3.go`) — **not an `engine.Driver`** (base of quotaless; raw client in tools)

| Method | On missing | List | Exists | HealthCheck | Streaming | ctx | Put options | Conforms? |
|---|---|---|---|---|---|---|---|---|
| `Put(ctx, c, a, r)` | | | | | **`materialize`**: ≤64 MiB RAM else temp file | ✓ (constructor `context.TODO()`) | none accepted | ✗ (buffers) |
| `Get`/`Delete` | SDK `%w` ✓ | | | | ✓ | ✓ | | ✓ |
| `List` | | **one page**, keys as-is | | | | ✓ | | ✗ → **fixed** (paginate) |
| `Exists` | | | **`"404"` substring** | | | ✓ | | ✗ → **fixed** |
| `Name`/`HealthCheck` | absent | | | | | | | n/a |

### quotaless (`quotaless.go`) — registered `quotaless` when creds set (not in prod; key dead)

| Method | On missing | List | Exists | HealthCheck | Streaming | ctx | Put options | Conforms? |
|---|---|---|---|---|---|---|---|---|
| `Get` | last SDK error `%w` ✓ — **after 3 attempts + 5 s of sleeps** | | | | ✓ | sleeps ignore ctx | | ✗ (contract d) |
| `Put` | | | | | **`io.ReadAll` whole body**, then `materialize` again | ✗ | all dropped | ✗ |
| `Delete` | as Get | | | | | ✗ | | ✗ |
| `List` | | strips `personal-files/<c>/` ✓, honours `prefix` ✓, one page (via s3) → **paginated now** | | | | ✗ | | partial |
| `Exists` | | | via s3 (substring) → fixed; retries on error | | | ✗ | | partial |
| `HealthCheck` | | | | signed list, 10 s timeout, **no `MaxKeys`** | | ✓ | | ✓ (authenticated) |

### lyve (`lyve.go`) — registered `lyve`; `RESILIENT` class + general failover leg; bucket `stored-<region>`, keys `t-<tenant>/<c>/<a>`

| Method | On missing | List | Exists | HealthCheck | Streaming | ctx | Put options | Conforms? |
|---|---|---|---|---|---|---|---|---|
| `Name` | `"lyve"` = key ✓ | | | | | | | ✓ |
| `Get` | SDK `%w` ✓ | | | | ✓ | ✓ | | ✓ |
| `GetRange` | SDK `%w` ✓ | | | | `Range: bytes=o-(o+l-1)` ✓ | ✓ | | ✓ |
| `Put` | | | | | `s3ParallelUploadInput` (as s3compat) | ✓ | **all** options honoured ✓ | ✓ |
| `Delete` | SDK `%w` ✓ (Lyve 204 on missing) | | | | | ✓ | | ✓ |
| `List` | empty ✓ | paginated ✓, strips `t-<t>/<c>/` ✓, prefix ✓ | | | | ✓ | | ✓ |
| `Exists` | | | HeadObject; **`"404"` substring** | | | ✓ | | ✗ → **fixed** |
| `HealthCheck` | | | | signed `HeadBucket` ✓ (error **unwrapped**) | | ✓ | | ✓ |
| tenant | `ctx` → `d.tenantID` (`""` in prod) → `"default"` + Warn | | | | | | | R6-22 |

### idrive (`idrive.go`) — registered `idrive` (primary) + `idrive-<region>` × 8 (R7-01); fixed bucket `IDRIVE_BUCKET` (default `vaultaire`), keys `t-<tenant>/<c>/<a>`

| Method | On missing | List | Exists | HealthCheck | Streaming | ctx | Put options | Conforms? |
|---|---|---|---|---|---|---|---|---|
| `Name` | `"idrive"` for **all nine** registrations (region drivers are keyed `idrive-<region>`) | | | | | | | ✗ (P3; engine uses keys) |
| `Get` | SDK `%w` ✓ | | | | ✓ (egress wrapper never active) | ✓ | | ✓ |
| `GetRange` | SDK `%w` ✓ | | | | ✓ | ✓ | | ✓ |
| `Put` | | | | | `s3ParallelUpload` ✓ | ✓ | `ContentLength`, `ContentType` ✓ | ✓ |
| `Delete` | SDK `%w` ✓ | | | | | ✓ | | ✓ |
| `List` | empty ✓ | paginated ✓, strips base prefix ✓, prefix ✓ | | | | ✓ | | ✓ |
| `Exists` | | | HeadObject; **`"404"` substring** | | | ✓ | | ✗ → **fixed** |
| `HealthCheck` | | | | signed `HeadBucket` ✓ | | ✓ | | ✓ |
| regions | keys: `IDRIVE_<REGION>_ACCESS_KEY/SECRET_KEY` (both) else primary pair — **live: primary pair = 403 elsewhere**; endpoint `IDRIVE_<REGION>_ENDPOINT` else table (wrong hosts) | | | | | | | ✗ R7-01 |

### geyser (`geyser.go`) — registered `geyser`; `GLACIER`/`DEEP_ARCHIVE`; target-only; fixed bucket, keys `t-<tenant>/<c>/<a>`, tenant default `"vaultaire"`

| Method | On missing | List | Exists | HealthCheck | Streaming | ctx | Put options | Conforms? |
|---|---|---|---|---|---|---|---|---|
| `Name` | `"geyser"` ✓ | | | | | | | ✓ |
| `Get` | SDK `%w` ✓; `InvalidObjectState` → `engine.ErrArchived` ✓ | | | | ✓ | ✓ | | ✓ |
| `GetRange` | as Get ✓ | | | | ✓ | ✓ | | ✓ |
| `Put` | | | | | **`materialize` always** (64 MiB RAM / temp file) | ✓ | **all dropped incl. `ContentLength`** | ✗ R7-03 |
| `Delete` | SDK `%w` ✓ | | | | | ✓ | | ✓ |
| `List` | empty ✓ | strips base prefix ✓, prefix ✓, **one page** | | | | ✓ | | ✗ → **fixed** |
| `Exists` | | | HeadObject; **`"404"` substring** | | | ✓ | | ✗ → **fixed** |
| `HealthCheck` | | | | signed `HeadBucket` ✓ | | ✓ | | ✓ |
| `RestoreObject`/`RestoreStatus` | typed sentinels ✓ | | | | | ✓ | | ✓ |

### r2 (`r2.go`) — registered `r2`; `PUBLIC` class only; target-only; fixed bucket `R2_BUCKET`, keys `t-<tenant>/<c>/<a>`

| Method | On missing | List | Exists | HealthCheck | Streaming | ctx | Put options | Conforms? |
|---|---|---|---|---|---|---|---|---|
| `Name` | `"r2"` ✓ | | | | | | | ✓ |
| `Get` | SDK `%w` — **live: `NoSuchKey` 404** ✓ | | | | ✓ | ✓ | | ✓ |
| `GetRange` | live ✓ | | | | ✓ | ✓ | | ✓ |
| `Put` | | | | | `s3ParallelUpload` ✓; checksums `WhenRequired` | ✓ | `ContentLength`, `ContentType` ✓ | ✓ |
| `Delete` | live: nil on missing ✓ | | | | | ✓ | | ✓ |
| `List` | live: empty ✓ | paginated ✓, strip ✓, prefix ✓ | | | | ✓ | | ✓ |
| `Exists` | live `(false, nil)` | | HeadObject; **`"404"` substring** | | | ✓ | | ✗ → **fixed** |
| `HealthCheck` | | | | signed `HeadBucket` ✓ (live ✓) | | ✓ | | ✓ |
| tenant | `common.GetTenantID` → `"default"` | | | | | | | R6-22 |

### permafrost (`onedrive.go`) — registered `permafrost`; no storage class; target-only; path `vaultaire/t-<tenant>/<c>/<a>` on the FNV home tenant

| Method | On missing | List | Exists | HealthCheck | Streaming | ctx | Put options | Conforms? |
|---|---|---|---|---|---|---|---|---|
| `Name` | **`"onedrive"` ≠ key** | | | | | | | ✗ → **fixed** (`"permafrost"`) |
| `Get` | string `onedrive [t] HTTP 404: {…itemNotFound…}` — matched by the classifiers' `itemNotFound`/`NotFound` **string fallbacks only**; a Graph body without that code = backend failure | | | | 1 stream <10 MiB ✓; **2–8 ranges fully buffered** ≥10 MiB | ✓ HTTP; **sleeps ignore ctx** | | ✗ R7-07/08, typed error WP-R7-4 |
| `GetRange` | — | | | | not implemented | | | n/a |
| `Put` | | | | | `ContentLength` known: ≤4 MiB `ReadAll` (bounded) else 60 MiB chunk buffer ✓; unknown: `materialize` + `ReadAll` (**whole object**) | ✓ / sleeps ✗ | `ContentLength` ✓, rest dropped | partial |
| `Delete` | sweeps all tenants; returns 404-string only if nothing deleted ✓ | | | | | ✓ | | ✓ (string error) |
| `List` | empty ✓ | union of all tenants ✓, paginated ✓, prefix ✓ — **direct children only** | | | | ✓ | | ✗ R7-06 |
| `Exists` | `(false, nil)`; a down tenant → error ✓ | | | Graph GET metadata per tenant; **`"404"` substring** | | ✓ | | partial |
| `HealthCheck` | | | | authenticated Graph `GET /drives/{id}` on **one rotating tenant** | | ✓ | | ✓ (weak) |

**Additional assumptions (R6 (a)–(e)):** (a) every driver keys by `ctx` tenant or container ✓ (all
fall back to `"default"` — R6-22); (b) `%w` everywhere on S3-class drivers ✓, permafrost wraps its
own strings; (c) concurrent use: all S3 clients are safe; permafrost's `graphDo` mutates the shared
`req` across retries (same goroutine — fine), token refresh serialised per tenant ✓ (one refresh
under load, callers wait on `tokenMu`); local `AtomicWrite` safe (per-call temp); (d) in-request
sleeps: **quotaless, permafrost** violate; `lyve_console` sleeps once (10 s) but selects on `ctx` ✓;
(e) partial-body failure never a truncated success on the ≤16 MiB path (R2-06) and on the manager
path via `ErrUnexpectedEOF` (R7-12); geyser/quotaless `materialize` propagates the body error ✓; local
removes the temp ✓.

## Transport and limits

| Driver | `http.Transport` | Idle / per-host | Dial / TLS / ResponseHeader timeouts | HTTP version | Retry policy | Put path (threshold / part / concurrency) | Request checksum | Response validation |
|---|---|---|---|---|---|---|---|---|
| local | — | — | — | — | — | temp+rename | — | — |
| s3compat | `TunedHTTPClient()` (shared factory, one transport per client) | 200 / 200, idle 90 s | 10 s / 10 s / **0** | h2 attempted | SDK standard: 3 attempts, 20 s max backoff | single PUT ≤16 MiB; manager 16 MiB × 8 above | SDK default `WhenSupported` (CRC32 trailer) | `WhenSupported` |
| s3 / quotaless | `TunedHTTPClient()` | 200 / 200 | 10 / 10 / 0 | h2 attempted | SDK 3 + **driver 3 with 1 s, 4 s sleeps** | `materialize` → single `PutObject` | `WhenSupported` (README: incompatible) | `WhenSupported` (README: corrupts) |
| lyve | `TunedHTTPClient(WithHTTP1Only())` | 200 / 200 | 10 / 10 / 0 | **h1 pinned** | SDK 3 | as s3compat | `WhenSupported` — accepted | `WhenSupported` |
| idrive (+regions) | `TunedHTTPClient(WithHTTP1Only())` per driver (9 transports) | 200 / 200 each | 10 / 10 / 0 | h1 pinned | SDK 3 | as s3compat | `WhenSupported` — accepted | `WhenSupported` |
| geyser | `TunedHTTPClient(WithHTTP1Only(), WithResponseHeaderTimeout(5m))` | 200 / 200 | 10 / 10 / **300 s** | h1 pinned (h2 = 9× ingest collapse) | SDK 3 | `materialize` → single `PutObject` (no multipart) | `WhenSupported` — accepted (Vail also computes CRC64NVME) | `WhenSupported` |
| r2 | `TunedHTTPClient(WithHTTP1Only())` | 200 / 200 | 10 / 10 / 0 | h1 pinned | SDK 3 | as s3compat | **`WhenRequired`** (R2 501s on trailers) | `WhenRequired` |
| permafrost | own: `odGraphTransport` (h2 via `http2.ConfigureTransport`, 200/200, 1 MiB bufs), `odCDNTransport` (h1, `MaxConnsPerHost` 128, idle 32, 4 MiB bufs), `odUploadTransport` (h1, 200/200) — per tenant ×3 | see left | 10 / 10 / **0** on all three | h2 API, h1 CDN + upload | **driver: 5 attempts, jitter ≤30 s + unbounded `Retry-After`**; chunk 3 attempts 1/2/4 s | ≤4 MiB single PUT; upload session 60 MiB chunks, sequential | Graph (n/a) | n/a |
| lyve console probe | `TunedHTTPClient(WithHTTP1Only())` | | 10 / 10 / 0 | h1 | one 403 retry after 10 s (ctx-aware) | — | SigV4 `iam` | — |

Shared facts: `TunedHTTPClient` is a **factory** — each call builds a new `http.Transport`, so prod
runs ≥13 S3 transports (9 idrive + lyve + geyser + r2 + probe) plus 9 permafrost transports, each
with `MaxIdleConnsPerHost` 200 and 4 MiB read/write buffers per connection (a warm connection costs
8 MiB of buffers; 200 idle × 4 MiB × 2 = 1.6 GiB theoretical ceiling per transport — a WP-R6-3
value to revisit). `DisableCompression` on; proxy-from-env **not** set (`Proxy: nil`) — the
process ignores `HTTPS_PROXY`, which is fine on SLC. DNS is cached forever per host in a
package-level `sync.Map` (`transport.go:16`, `onedrive.go:63`) — a vendor IP change is invisible
until restart (P3, WP-R7-6: TTL). `VAULTAIRE_TUNED_TRANSPORT=false` returns `http.DefaultClient`
(no timeouts at all).

**Proposed per-operation deadlines (WP-R6-3)** — all produce `context.DeadlineExceeded`, a breaker
failure, before HAProxy's 50 s cut where the operation is a first-byte wait:

| Operation | Budget | Rationale |
|---|---|---|
| `HeadBucket` (probe) | 15 s (exists) | unchanged |
| `Exists` / `Delete` / `List` page | 15 s | metadata round trip |
| `Get` / `GetRange` first byte | 25 s (`ResponseHeaderTimeout`); body: no total deadline, ctx-bound | HAProxy 50 s; Geyser cold = immediate 403 |
| `Put` total | 60 s + 1 s per MiB of `ContentLength`; unknown length 15 min | 1 MiB/s floor; a stalled backend fails within a minute for small objects |
| Graph API call | 20 s first byte; ≤2 attempts on the request path | current 5 attempts × 30 s is a client-side outage |
| SDK `RetryMaxAttempts` | 2 on request paths (3 on jobs) | the breaker + client retry is the policy |

## Invariants confirmed

1. **Stream, never buffer** — bounded buffers: `putSinglePartIfSmall` ≤16 MiB exact (`s3upload.go:114`), manager 8 × 16 MiB parts, permafrost ≤4 MiB `ReadAll` (`onedrive.go:297`), 60 MiB chunk (`:968`), `lyve_console` 64 KiB drain. **Unbounded**: `materialize` 64 MiB RAM + temp file (geyser `:258`, s3 `:68`, permafrost fallback `:307`), quotaless `io.ReadAll` (`:85`), permafrost `downloadRanges` `io.ReadAll` per range (`:444`) and `ReadAll(body)` after materialize (`:320`, `:327`), local `Transaction.Put` `ReadAll` (`local.go:788`, unused). Every `Get` returns the backend body unread ✓.
2. **ctx first and propagated** — every SDK/HTTP call takes `ctx` ✓ (`s3.go:40` and the constructors use `context.TODO()`/`Background()` for `LoadDefaultConfig`, which performs no I/O with static creds). Sleeps in quotaless/permafrost do not select on ctx ✗.
3. **`%w`** — every S3-class error path wraps ✓ (`isSDKNotFound`/`isObjectMissingErr` see the types — R6-01 fix holds); geyser wraps `geyserWireErr` ✓; local wraps `NotFoundError` ✓ and returns `*fs.PathError` unwrapped (fine: `errors.Is(err, os.ErrNotExist)`); permafrost wraps its own string errors (`%w` of a `fmt.Errorf` string) — classifiers rely on the `itemNotFound`/`NotFound` substrings.
4. **Authenticated probes (rule 1)** — no bare GET anywhere: idrive/geyser/r2/lyve `HeadBucket` signed; s3compat/quotaless signed list; permafrost bearer-token Graph GET; local `Stat`; API-level: console action (root) for lyve, TCP dial only where no driver exists. After this PR: `idrive-<region>` (with own keys) and `permafrost` probed; lyve falls back to signed `HeadBucket`, not to the data-plane key on the console.
5. **Tenant isolation** — drivers derive nothing: tenant from `ctx` (idrive/r2/permafrost/geyser/lyve) or container only (local/s3compat/quotaless); shared-bucket drivers prefix `t-<tenant>/`; keys are opaque and dot segments are sent literally (test) — the one hazard is the `"default"` fallback (R6-22).
6. **Local path sites** — engine-reachable `Get`/`Put`/`Delete` go through `resolvePath` (R2-04); `List`/`Exists` now do; `HealthCheck` touches only `basePath`; the 40 helper methods (`GetPooled`, `PutBuffered`, `AtomicDelete`, `CreateSparse`, `SetXAttr`, `LockFile`, `Watch`, `Copy`, …) use `filepath.Join` with at most the old `HasPrefix` check and have **no callers outside tests** (grep + deadcode) — D-9 material, not engine-reachable.
7. **R6-04 target-only** — `local`, `r2`, `geyser`, `permafrost`, `idrive-<region>` are reached only when targeted; `lyve` is the only general failover leg and its buckets carry no lock (R7-16) ✓.
8. **Geyser cold read** — `InvalidObjectState` → `ErrArchived`, never a breaker charge, 403 `InvalidObjectState` at the API ✓; `RestoreStatus` = HEAD passthrough ✓.
9. **Chunk path vs manager** — no double parallelism (R7-23) ✓.
10. **Probe budget** — 15 s per probe; drivers add no retries beyond the SDK's 3 attempts, except lyve console (one 10 s retry inside a 45 s budget) ✓.

**Areas with no test (of the ten):** (1) local — `List` prefix/temp-file behaviour (added), `Exists` traversal (added), dir-fsync/permissions (none); (2) s3/s3compat/s3upload — pagination (added), `Exists` shapes (added), manager >1-part short body (added), clean-EOF short body (none, documented); (3) lyve — homing/WORM (live-only, none automated), console probe ✓ existing; (4) idrive — region table vs account (none — needs the reseller API), per-region key fallback ✓ existing; (5) geyser — restore/archived ✓ existing, `materialize` memory bound (none), pagination (added); (6) r2 — dot-segment raw path (added), jurisdiction (existing), live round trip (existing, env-gated); (7) quotaless — retries ✓ existing (with real sleeps: the suite pays them), no test that a miss is not retried (none); (8) permafrost — placement/sweep/pagination ✓ existing, typed not-found (none), range-download memory (none), fleet-order fingerprint (none), token refresh concurrency (none); (9) transport — settings ✓ existing, no deadline test (none); (10) probes — extended tests (added); alert-rule/label parity (none — deploy is manual).

## Dead code noted

R0 handoff row for R7 confirmed (`deadcode ./cmd/vaultaire`, 2026-09-27): `idrive.go` 2/20
(`LoadIDriveConfig`, `NewIDriveDriverFromConfig`), `lyve_console.go` 3/7 (the three `WithLyveConsole*`
options — test-only), `egress_tracker.go` 1/7 (`NewEgressTracker`; the rest are methods reachable only
through the never-set `egressTracker` field). Also fully dead: `cost_advisor.go` 9/9, `wasm.go` 3/3 +
`plugins/transform/main.go` (`//go:build wasm` — never built; D-2), `geyser_admin.go` 49/49 from
`cmd/vaultaire` (tool library — keep, R0). Reachable-but-unused (RTA keeps interface-shaped methods
alive; **zero callers** by grep): `IDriveDriver.PutWithSize/putMultipart/abortMultipartUpload/
ValidateAuth/GetEgressTracker/SetEgressTracker`, `LocalDriver` helpers `GetPooled`, `ReturnPooledReader`,
`GetPoolStats`, `SupportsSymlinks`, `GetWithOptions`, `GetInfo`, `Set/GetPermissions`, `Set/GetOwnership`,
`GetChecksum`, `VerifyChecksum`, `CreateDirectory`, `RemoveDirectory`, `DirectoryExists`, `ListDirectory`,
`WalkDirectory`, `GetDirectorySize`, `IndexDirectory`, `FindFilesBy*`, `SyncDirectory`,
`CompareDirectories`, `GetDirectoryModTime`, `HasDirectoryChanged`, `AtomicRename`, `AtomicDelete`,
`BeginTransaction`/`Transaction.*`, `PutBuffered`/`BufferedWriter`, `GetWriteBufferStats`,
`CreateMultipartUpload`/`UploadPart`/`CompleteMultipartUpload` (local **and** s3), `Watch`, `Copy`,
`WriteAt`, `LockFile`, `CreateSparse`/`GetHoles`/`GetFileInfo`, `Set/Get/ListXAttr`,
`Capabilities`/`HasCapability`; `QuotalessDriver.GetMetrics`; `LocalDriver.filePool` (a pool whose
`New` returns nil). `S3Driver` is not a driver (R7-21). Recommendation for D-9: delete all of the
above with `quotaless.go`/`s3.go` (WP-R7-3/6) — none is on a product path and several carry the
pre-R2-04 traversal pattern.

## Follow-up work packages

| WP | Title | Files | Size | Depends on |
|----|-------|-------|------|-----------|
| WP-R7-1 | **iDrive region routing rebuilt from the account** (R7-01): table from reseller `GET /regions` (ids `s3.<region>.idrivee2.com`, display names from `region_name`), "default" = primary's region (`us-central-1`), migrate 25 `buckets.region='us-west-1'` rows, register an `idrive-<region>` driver **only** with its own key pair (else Error log + `CreateBucket` rejects the region), per-region `vaultaire` bucket provisioning (reseller API or console), dashboard copy, `main.go:293` default endpoint. Region probes land automatically (R7-19). | `drivers/idrive_regions.go`, `cmd/vaultaire/main.go`, `api/s3_buckets.go`, `api/s3_engine_adapter.go` (`bucketRegionDriver`), dashboard handlers, migration | M | [YOU] `GET /regions` output + bucket provisioning; Isaac: which regions to sell |
| WP-R7-2 | **Geyser streams known lengths** (R7-03): `ContentLength > 0` → direct `PutObject`; `materialize` only for unknown; honour `ContentType`; live Vail verification; memory test. | `drivers/geyser.go`, `geyser_test.go` | S | Geyser creds for the live check |
| WP-R7-3 | **Quotaless + S3Driver: delete** (R7-04/21) or rewrite on `s3upload` with ctx-aware, miss-exempt backoff; drop `README` contradiction. | `drivers/quotaless.go`, `s3.go`, tests, `cmd/backend-matrix`, `cmd/erasure-bench` (raw-client uses), `main.go`, CLAUDE.md | S | D-decisions (M12 exit) |
| WP-R7-4 | **Permafrost conformance**: typed Graph error (`odError{Status, Code}`) replacing the `"404"` substring; ctx-aware, bounded retries (R7-08); streamed range downloads + pooled chunk buffer + no `materialize` fallback (R7-07); recursive `List` (R7-06); fleet fingerprint (R7-09); all-tenant HealthCheck (R7-18). | `drivers/onedrive.go`, tests | M | — |
| WP-R7-5 | **Routing-truth reconciliation** (R7-10/11): boot check that every `object_head_cache.backend_name` has a registered driver; sampled head-row ↔ driver `Exists` job with a metric; one-off fix of the bench rows (`onedrive` → `permafrost`; `local` rows with no bytes → delete or flag); decide `local`'s durable role and `DATA_PATH` backup. | `api/` job, `engine`, migration/script | S–M | R13 (jobs), WP-R9-7 |
| WP-R7-6 | **Driver hygiene**: delete the unused idrive/local helpers (R7-14, D-9), `R2Endpoint` rejects `us`, explicit checksum mode per driver (R7-24), `fsync` parent dir + prune empty dirs in local (R7-20), wrap lyve `HealthCheck` error, `MaxKeys: 1` quotaless probe, DNS cache TTL, every driver honours `ContentType`, `Name()` = registration key for s3compat/idrive regions. | `internal/drivers/*` | S | WP-R7-3 for the deletions |
| WP-R7-7 | **Lyve lock guard** (R7-16): `GetObjectLockConfiguration` at boot; lock enabled → Error + target-only; driver-declared `TargetOnly()` capability replacing the engine's name list. | `drivers/lyve.go`, `engine/engine.go` | S | R6 (`targetOnlyBackends`) |
| WP-R6-3 (values) | Per-operation deadlines and `ResponseHeaderTimeout` per the table above; SDK `RetryMaxAttempts` 2 on request paths; transport buffer sizing review. | `drivers/transport.go`, each constructor, `engine/failover.go` | S–M | — |
| handed on | R7-02 → [YOU] (Lyve scoped user + TFA); alert rule install → ops; `docs/DRIVERS.md`/`ARCHITECTURE.md` rewrite from the conformance tables → R14 (WP-R6-9); `idrive_integration.md` deletion → R14; clean-EOF short body → WP-R2-1; `"default"` tenant fallback → WP-R6-1 | | | |
