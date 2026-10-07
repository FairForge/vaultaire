# Drivers

Storage backends as they are actually wired and as the engine actually treats
them. Source of truth for the contract is `internal/engine/interface.go`; the
per-driver facts below come from the driver review (`docs/reviews/R7-drivers.md`,
"Contract conformance" and "Transport and limits") plus the iDrive region
rebuild that followed it (WP-R7-1, #502). Placement — which driver a PUT goes
to — is described in `docs/ARCHITECTURE.md`.

## The interface

Copied from `internal/engine/interface.go`:

```go
type Driver interface {
    Name() string
    Get(ctx context.Context, container, artifact string) (io.ReadCloser, error)
    Put(ctx context.Context, container, artifact string, data io.Reader, opts ...PutOption) error
    Delete(ctx context.Context, container, artifact string) error
    List(ctx context.Context, container, prefix string) ([]string, error)
    Exists(ctx context.Context, container, artifact string) (bool, error)
    HealthCheck(ctx context.Context) error
}

// Optional
type RangeGetter interface {
    GetRange(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, error)
}
type Restorer interface {
    RestoreObject(ctx context.Context, container, artifact string, days int32) error
    RestoreStatus(ctx context.Context, container, artifact string) (*RestoreStatus, error)
}
```

`PutOption` is a functional option over `PutOptions{ContentType, CacheControl,
ContentEncoding, ContentLanguage, ContentLength, StorageClass, UserMetadata}`;
constructors are `engine.WithContentType`, `WithUserMetadata`,
`WithContentLength`, `WithStorageClass`. Product code only ever passes
`WithContentLength` and `WithStorageClass` (R7-26); the head cache is the
metadata source of record, so a driver dropping `ContentType` is latent, not a
bug. `StorageClass` is an engine routing hint — no driver sets a vendor class.

Things that do **not** exist, whatever older docs said: a `Put` without
options, a `List` without `prefix`, a `GetMetrics()` on drivers,
`engine.ErrNotFound`/`engine.ErrAccessDenied` sentinels (`engine.ErrNotFound`
is a constructor returning `NotFoundError`), `engine.RegisterDriver`, and
`internal/engine/registry.go`. Drivers are added with `eng.AddDriver(key, drv)`
in `cmd/vaultaire/main.go`; the engine addresses them by that key.

## What the engine assumes of every driver

| Method | On missing object | Other requirements |
|--------|-------------------|--------------------|
| `Get` | Must be classifiable as a **miss** by `engine.isBackendFailure` / `api.isObjectMissingErr`: `engine.NotFoundError`, an error wrapping `os.ErrNotExist`, or an aws-sdk-go-v2 chain carrying HTTP 404 / `NoSuchKey` / `NotFound`. Anything else is a backend failure and charges the breaker. `ErrArchived` for tape-evicted objects. | Return the body unread (the engine never buffers). Pass `ctx` to the SDK; wrap with `%w` so `errors.Is(err, context.Canceled)` works. |
| `GetRange` | as `Get` | Body positioned at `offset`, at most `length` bytes. Only `idrive` (+ regions), `lyve`, `geyser`, `r2` and `sync` implement it; `local`, `s3`, `quotaless` and `permafrost` get `Get` + discard. |
| `Put` | n/a. Returns no ETag and no size — the API computes the MD5 ETag on the stream and knows the size. | Stream `data` once; may read `PutOptions.ContentLength`. Never read past a failure; a partially consumed body must surface as an error, never a truncated success. A cancelled ctx mid-body is a client abort. |
| `Delete` | Idempotent preferred (204 on missing). A vendor 404 must classify as a miss — the API treats it as success and still removes the head row. | |
| `List` | Empty slice, not an error, for an empty or missing container. | Keys **relative to the container** (the `t-<tenant>/<container>/` prefix stripped), filtered by `prefix`, **all pages**. Order unspecified (the API sorts). Used only on the no-DB fallback path. |
| `Exists` | `(false, nil)` | Not called by the engine or the API (only `cmd/backend-matrix`, `cmd/erasure-bench`). One `HeadObject`; misses classified by the typed `s3IsNotFound` (`internal/drivers/s3errors.go`). |
| `HealthCheck` | — | Must be **authenticated** (rule 1). Called only through the API probes and the admin dashboard, under a 15 s budget; a failing probe does not affect routing. |

Also: key by the tenant in `ctx` or by the container, never by anything derived;
safe for concurrent use; no retry-with-sleep inside a request (the breaker plus
the client's retry is the retry policy — quotaless and permafrost violate this).

## Registration

`cmd/vaultaire/main.go` registers drivers in this order. `local` is always
present; everything else is gated on environment variables.

| Key | File | Registered when | Class(es) that resolve here | Target-only | Bucket / key layout | Tenant from |
|-----|------|-----------------|-----------------------------|-------------|---------------------|-------------|
| `local` | `local.go` | always (`DATA_PATH`, default `/tmp/vaultaire-data`) | `REDUCED_REDUNDANCY` | yes | `<DATA_PATH>/<container>/<artifact>` | container |
| `s3` | `s3compat.go` (`S3CompatDriver`, `Name()` = `s3compat`) | `S3_ACCESS_KEY` | — (primary only if auto-detected) | no | hard-wired `us.s3compat.cloud:8000`, bucket `data`, prefix `personal-files/vaultaire/<container>/` | container |
| `lyve` | `lyve.go` | `LYVE_ACCESS_KEY` (`LYVE_REGION`, default `us-west-1`; prod sets `LYVE_REGION` — `us-east-1` per R7-25) | `RESILIENT`; the only general failover leg | no | bucket `stored-<region>`, keys `t-<tenant>/<container>/<artifact>` | ctx |
| `quotaless` | `quotaless.go` | `QUOTALESS_ACCESS_KEY` | — | no | `personal-files/<container>/` | container |
| `geyser` | `geyser.go` | `GEYSER_ACCESS_KEY` (`GEYSER_BUCKET`, `GEYSER_ENDPOINT`) | `GLACIER`, `DEEP_ARCHIVE` | yes | fixed bucket, keys `t-<tenant>/<container>/<artifact>` | ctx (default `vaultaire`) |
| `idrive` | `idrive.go` | `IDRIVE_ACCESS_KEY` (`IDRIVE_ENDPOINT`, `IDRIVE_REGION` default `us-central-1`, `IDRIVE_BUCKET` default `vaultaire`) | `STANDARD`; prod primary | no | fixed bucket, keys `t-<tenant>/<container>/<artifact>` | ctx |
| `wasabi` | `wasabi.go` (`NewWasabiDriver` → `NewFixedBucketS3Driver("wasabi", …)`, the `IDriveDriver` type under its own name) | `WASABI_ACCESS_KEY` (`WASABI_REGION` default `us-west-1`, `WASABI_ENDPOINT` default `https://s3.<region>.wasabisys.com`, `WASABI_BUCKET` default `vaultaire`) | `STANDARD`; **prod primary since 2026-10-03 (interim)** via `STORAGE_MODE=wasabi` | yes (signed HeadBucket) | fixed bucket created at boot if absent, keys `t-<tenant>/<container>/<artifact>`, path-style, HTTP/1.1; Wasabi list $7.99/TB, 90-day minimum per object, no egress/API fees — the partner account is free | tenant from context (`ErrNoTenant` otherwise) |
| `idrive-<region>` | `idrive.go` + `idrive_regions.go` | `IDRIVE_<REGION>_ACCESS_KEY` **and** `_SECRET_KEY` (region id upper-cased, `-`→`_`) — one pair per region, no fallback to the primary pair | none; reached only via `bucketRegionDriver` for region-pinned buckets | yes | same fixed bucket name in that region, created at boot by `EnsureBucket` | ctx |
| `r2` | `r2.go` | `R2_ACCOUNT_ID` (`R2_ACCESS_KEY`, `R2_SECRET_KEY`, `R2_JURISDICTION`, `R2_BUCKET` default `vaultaire-public`) | `PUBLIC` only | yes | fixed bucket, keys `t-<tenant>/<container>/<artifact>` | ctx |
| `permafrost` | `onedrive.go` (`OneDriveDriver`) | `TENANT_1_ID` (fleet of `TENANT_N_*`, N = 1..15) | none | yes | `vaultaire/t-<tenant>/<container>/<artifact>` on the FNV-chosen home tenant | ctx |
| `sync` | `webdav_multi.go` (`MultiWebDAVDriver`: one `WebDAVDriver` per bridge, a key per bridge by HRW — `webdav.go`, generic WebDAV — `webdav_README.md`) | `SYNC_WEBDAV_PASSWORD` or `SYNC_WEBDAV_URLS` + `SYNC_WEBDAV_PASSWORDS` (`SYNC_WEBDAV_URL` default `http://127.0.0.1:4918`, `SYNC_WEBDAV_USER` `sync`, `SYNC_WEBDAV_ROOT` `vaultaire`) | `SYNC` only: a bucket with `tier_preference = 'sync'` of a tenant with the `sync_backend` flag; `STORAGE_MODE=sync` is a boot Fatal | yes | `<root>/t-<tenant>/<container>/<artifact>` on Sync.com's local bridge; key segments `""`/`.`/`..`/`~…` mapped reversibly | ctx |

The 13 iDrive regions are the ones the reseller account has
(`internal/drivers/idrive_regions.go`): `us-central-1 us-west-2 us-west-4
us-southwest-1 us-southeast-1 us-midwest-1 us-east-1 eu-west-1 eu-west-3
eu-west-4 eu-central-1 eu-south-1 ap-northeast-1`, endpoint
`https://s3.<region>.idrivee2.com`. The primary `idrive` serves the default
region; the loop skips it and registers the others (sorted) only with their own
key pair. `SetAvailableIDriveRegions` records the result and `CreateBucket`
(S3 and dashboard) refuses any other region (`InvalidLocationConstraint`).

**Primary:** `STORAGE_MODE`, else auto-detected iDrive > Wasabi > Quotaless >
S3 > Geyser > local (`config.DetectStorageMode`). Prod is `wasabi` since
2026-10-03 (interim: the iDrive key answers 403 on object calls); `idrive`
stays registered for the rows it holds. STANDARD resolves to the primary,
whichever it is.

**Not a driver:** `S3Driver` (`s3.go`) does not implement `engine.Driver`
(`Put` has no options, no `Name`, no `HealthCheck`). It is the embedded base of
`QuotalessDriver` and the raw client used by `cmd/backend-matrix` and
`cmd/erasure-bench`.

**Dormant:** `quotaless` is not registered in prod (credentials unset, key
dead) and `s3` is not either. `permafrost` is an internal parity backend, not
customer-facing, registered only where the OneDrive fleet env is present.

## Conformance summary

"OK" = conforms after the R7 fixes. Details and line references are in
`docs/reviews/R7-drivers.md`.

| Driver | Miss shape (`Get`/`Delete`) | `List` | `Exists` | `HealthCheck` | Streaming / buffering | ctx | Known gaps |
|--------|-----------------------------|--------|----------|---------------|------------------------|-----|------------|
| local | `*fs.PathError` wrapping `ErrNotExist`; a path escaping `DATA_PATH` → `NotFoundError` | OK: honours `prefix`, hides `.tmp-*`, relative slash names | OK via `resolvePath` | `os.Stat(basePath)` — weak (passes on a full disk) | temp + `fsync` + `rename`; no parent-dir fsync (WP-R7-6); no `GetRange` | fs, unused | a key naming a directory opens and fails on first read; empty dirs remain after Delete |
| s3 (`s3compat`) | SDK `%w`, typed | OK: paginated, relative to `<root>/<container>/` | OK typed | signed `ListObjectsV2 MaxKeys=1` | ≤16 MiB single PUT, else 16 MiB × 8 parts | OK | `Name()` ≠ registration key |
| quotaless | last SDK error after **3 attempts + 5 s of sleeps** | paginated, strips `personal-files/<c>/` | via base, retries | signed list, 10 s, no `MaxKeys` | **`io.ReadAll` whole body**, then `materialize` | sleeps ignore ctx | violates the no-in-request-retry rule; all Put options dropped |
| lyve | SDK `%w`; Lyve answers 204 on a missing Delete | OK | OK | signed `HeadBucket` | as s3compat; `GetRange` | OK | only driver honouring every Put option |
| idrive (+ regions) | SDK `%w` | OK | OK | signed `HeadBucket` | as s3compat; `GetRange` | OK | `Name()` is `idrive` for every regional registration (engine uses keys) |
| geyser | SDK `%w`; `InvalidObjectState` → `engine.ErrArchived`; `Restorer` implemented | OK (paginated after R7) | OK | signed `HeadBucket` | **`materialize` always**: ≤64 MiB in RAM, else temp file; no multipart | OK | drops `ContentLength` (R7-03); cold reads answer 403 `InvalidObjectState` at the API |
| r2 | SDK `%w` (`NoSuchKey`); nil on missing Delete | OK | OK | signed `HeadBucket` | as s3compat; `GetRange` | OK | — |
| sync (WebDAV) | `engine.NotFoundError` on 404; nil on missing Delete (a key that is a folder is never deleted) | OK: Depth-1 PROPFIND walk, relative keys, prefix-pruned, sorted | Depth-0 PROPFIND (not HEAD) | authenticated Depth-0 PROPFIND of the root | streamed PUT (Content-Length when known, else chunked); size verified by PROPFIND after a known-length PUT; `GetRange` (a 200 to a Range is skipped + limited) | OK | file `a` and folder `a` cannot coexist (key `a` vs `a/b`); empty folders remain after Delete |
| permafrost | string error containing `itemNotFound`/`404` — matched by the classifiers' string fallbacks only (WP-R7-4) | union of all fleet tenants, paginated, **direct children only** (R7-06) | Graph metadata GET per tenant | authenticated Graph `GET /drives/{id}` on one rotating tenant | <10 MiB one stream; ≥10 MiB 2–8 ranges **fully buffered**; Put ≤4 MiB `ReadAll`, else 60 MiB session chunks; unknown length → whole object in memory | HTTP OK; **sleeps ignore ctx** | driver-level retries (5 attempts, up to 30 s jitter + `Retry-After`) |

Target-only backends (`local`, `r2`, `geyser`, `permafrost`, `idrive-<region>`)
are reached only when a PUT resolves to them or they are the primary; they are
never a silent failover destination (R6-04).

## Transport and limits

All S3-class drivers build their client from `TunedHTTPClient`
(`internal/drivers/transport.go`): a **factory** — each call is a new
`http.Transport` with 200 idle / 200 per-host connections, 90 s idle, 10 s
dial, 10 s TLS, 4 MiB read/write buffers, compression off, no proxy from env,
and DNS cached for the process lifetime (a vendor IP change is invisible until
restart, WP-R7-6). `VAULTAIRE_TUNED_TRANSPORT=false` returns
`http.DefaultClient` with no timeouts at all.

| Driver | HTTP version | Response-header timeout | Retry policy | Put path | Request checksum | Response validation |
|--------|--------------|-------------------------|--------------|----------|------------------|---------------------|
| local | — | — | — | temp + rename | — | — |
| s3 (`s3compat`) | h2 attempted | none | SDK standard: 3 attempts, 20 s max backoff | single PUT ≤16 MiB; manager 16 MiB × 8 above | SDK default `WhenSupported` (CRC32 trailer) | `WhenSupported` |
| quotaless | h2 attempted | none | SDK 3 **+ driver 3 with 1 s / 4 s sleeps** | `materialize` → single `PutObject` | `WhenSupported` (README: incompatible with the Minio gateway) | `WhenSupported` (README: corrupts) |
| lyve | **h1 pinned** | none | SDK 3 | as s3compat | `WhenSupported` — accepted | `WhenSupported` |
| idrive (+ regions, one transport each) | h1 pinned | none | SDK 3 | as s3compat | `WhenSupported` — accepted | `WhenSupported` |
| geyser | h1 pinned (h2 collapses ingest ~9×) | **300 s** | SDK 3 | `materialize` → single `PutObject` | `WhenSupported` — accepted | `WhenSupported` |
| r2 | h1 pinned | none | SDK 3 | as s3compat | **`WhenRequired`** (R2 answers 501 to streaming trailers) | `WhenRequired` |
| sync (WebDAV) | h1 pinned | **300 s** (a bridge may answer a PUT after its own upload) | none (breaker only); one PUT retry after recreating a missing folder when the body rewinds | streamed `PUT`, parents by `MKCOL` (cached) | n/a | stored size via PROPFIND |
| permafrost | h2 for the Graph API, h1 for CDN downloads and upload sessions (three transports per fleet tenant) | none | **driver: 5 attempts, jitter ≤30 s + unbounded `Retry-After`**; chunk 3 attempts 1/2/4 s | ≤4 MiB single PUT; upload session 60 MiB chunks, sequential | Graph (n/a) | n/a |
| lyve console probe | h1 | none | one 403 retry after 10 s, ctx-aware | — | SigV4 `iam` | — |

`s3ParallelUpload` (`internal/drivers/s3upload.go`) is the shared upload path:
objects that fit in one 16 MiB part go as a single `PutObject`; larger ones
use the SDK `manager.Uploader` with 8 concurrent 16 MiB parts, one uploader
cached per client. A body that ends before its declared `ContentLength` is an
error, never a shorter object. The chunk path hands drivers ≤16 MiB bodies, so
the manager's concurrency only applies to whole-object PUTs (resilient,
public, multipart complete). Per-operation deadlines beyond the header
timeouts above are not set today (WP-R6-3 proposes them).

## Rule 1: authenticated probes, TCP dial as fallback

Unauthenticated HTTP checks against S3 backends are unreliable (EOF and 403
vary by vendor) and a dead key looks healthy to a TCP dial. `buildBackendProbes`
(`internal/api/backend_probes.go`) therefore probes `local` (always
registered; a stat of `DATA_PATH`, which is what catches a vanished data
directory), iDrive (primary and every region with its own key pair), Geyser,
R2 and Lyve through the driver's `HealthCheck` — a **signed** `HeadBucket` —
permafrost with a bearer-token Graph GET and `sync` with an authenticated
WebDAV PROPFIND (a wrong bridge password is a 401); Lyve uses the console
`RSCustomerDetails` action instead when `LYVE_PROBE_*` root credentials are
set (never the data-plane key). `quotaless` keeps the plain TCP dial from
`configuredBackends` (its signed-list `HealthCheck` is not wired to a probe)
and `s3` (s3compat) is not probed at all. Probes run on a 15 s budget (the Lyve console probe on its own,
slower cadence) and feed `/health`, `/health/backends`, `/metrics` and the
alert rules; they never alter routing.

## How to add a driver

1. Implement `engine.Driver` in `internal/drivers/<name>.go` — and `RangeGetter`
   if the backend can serve byte ranges. Wrap every error with `%w`; make a
   missing object classifiable as a miss (see the table above); stream the
   body; use `s3ParallelUpload`, `s3ListPaginator` and `s3IsNotFound` for an
   S3-class backend; build the client with `TunedHTTPClient` (pin HTTP/1.1 if
   the vendor's h2 serialises uploads); set the checksum mode explicitly if the
   vendor rejects streaming trailers.
2. Register it in `cmd/vaultaire/main.go` behind its own env vars with
   `eng.AddDriver("<key>", drv)`; make `Name()` return the same key. Add it to
   the auto-detect order only if it can be a primary.
3. If a storage class should resolve to it, add the row to
   `storageClassToBackend` and `backendToStorageClass` in
   `internal/engine/storage_class.go`. If it must never receive failover
   writes, add it to `targetOnlyBackends` in `internal/engine/engine.go`.
4. Add an authenticated probe in `internal/api/backend_probes.go`
   (`buildBackendProbes`) so `/health` and the alert rules see it.
5. Document it: a row in `internal/drivers/CLAUDE.md`, an env-var row in the
   root `CLAUDE.md`, a `<name>_README.md` ops manual next to the driver
   (existing ones: `idrive_README.md`, `lyve_README.md`, `geyser_README.md`,
   `onedrive_README.md`, `quotaless_README.md`, `webdav_README.md`), and this file.
6. Tests: the cross-driver conformance suite (`internal/drivers/conformance_test.go`)
   and, for S3-class drivers, the miss/failure shapes against an `httptest`
   server (see `internal/engine/failover_sdk_errors_test.go`).
