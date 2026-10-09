# WebDAV driver (`webdav.go`) — first instance: Sync.com's encrypted bridge (`sync`)

A generic WebDAV (RFC 4918) backend. One driver type serves any WebDAV
server — Sync.com's local bridge, Hetzner Storage Box, Koofr, pCloud,
Nextcloud — through `NewWebDAVDriver(name, baseURL, username, password, root, logger)`.
`cmd/vaultaire/main.go` registers ONE instance today: `sync`.

## Role: target-only, flag-gated

`sync` is never the primary and never a failover destination:

- `engine.targetOnlyBackends` lists it (a STANDARD write that fails on the
  primary never lands on it; `CheckPrimaryEligible` refuses it, so the admin
  "Set as Primary" button cannot pick it);
- `config.DetectStorageMode` never selects it, and `STORAGE_MODE=sync` is a
  Fatal at boot (`refusedPrimary` in `cmd/vaultaire/main.go`);
- the only placement is the internal `SYNC` storage class: a bucket whose
  `tier_preference` is `sync` (operator-set — no customer-facing control
  offers it: the management API and the dashboard reject the value) of a
  tenant with the **`sync_backend` feature flag** (default OFF, per tenant,
  never a global row). Without the flag such a bucket places exactly like an
  `auto` one. Reads follow `object_head_cache.backend_name` like every other
  backend. `SYNC` objects are stored whole (no chunking: chunk blobs live on
  the primary) and are reported to the customer as `STANDARD`.

```sql
-- the operator, for a tenant Sync has agreed to in writing (or our own):
UPDATE buckets SET tier_preference = 'sync' WHERE tenant_id = '<tenant>' AND name = '<bucket>';
```
```bash
curl -X PUT -H "Authorization: Bearer $ADMIN_JWT" -d '{"enabled":true,"tenant_id":"<tenant>"}' \
  https://stored.ge/api/v1/admin/flags/sync_backend
```

A dashboard save of that bucket's settings rewrites `tier_preference` from
the form (which does not offer `sync`) — set it again after one.

Not wired: generated objects (access logs, inventory, exports) delivered into
a `sync` bucket place on the primary (`generatedObjectWriter` passes no flag
gate), and a region-pinned bucket keeps its region driver.

### Sync's terms (read before enabling a tenant)

Sync's Terms of Service forbid reselling or providing the service to third
parties without Sync's **written consent**. Our own data and our own backups
on our own Sync account are fine. **Customer data goes there only with that
written consent** — that is what the per-tenant flag is for. Never set a
global `sync_backend` row.

## Layout and key mapping

`<root>/t-<tenant>/<container>/<artifact…>%o` below the server URL — the
fixed-bucket shape of iDrive/R2/Geyser (`tenantKey`), with the tenant from the
call's context, and every object's **leaf marked `%o`** (below). A call whose
context names no tenant is refused with `ErrNoTenant` (`tenant_ctx.go`); chunk
blobs (`engine.ChunkContext`) sit at `t-_global/_global/_chunks/<hash>%o`,
packs (`internal/packstore`) at `t-_global/_packs/<aa>/<sha256>.pack%o`. `ObjectKey` (engine.KeyAddresser) and
`StoreID` (`dav:<origin><path>/<root>/`, the routing-truth shared-store check)
are implemented.

Each `/`-separated key segment is one WebDAV path segment, `url.PathEscape`d,
after a reversible mapping (`davName` / `keySegment`) that makes every S3 key
one Sync.com's bridge stores. The rules were measured on prod's bridges
(2026-10-07: raw PUT through one bridge, GET through the four others 150 s
later):

| Sync's bridge | names |
|---|---|
| **400** | any of `: ? * " < > \| \`, a control character (tab is fine), a leading space or `~`, a trailing `.` or space, `CON PRN AUX NUL COM1-9 LPT1-9` (any case, also with an extension: `con.txt`, `aux.c`), `desktop.ini`, `Thumbs.db` (any case) |
| **201 and silently dropped** | `.DS_Store` (GET 404 on every bridge) |
| **too long** | a name over 248 characters (249 → 400; 248 × `é` = 496 bytes is accepted: characters, not bytes; per name, not per path) |
| fine | Unicode, `% # ; & = + @ ^ $`, inner `~`, tab, `.hidden`, `thumbs.db.x`, `ehthumbs.db`, `Icon`, `x.tmp`; names are case-sensitive (`case` and `CASE` coexist) |

The mapping (plain names map to themselves):

| key segment | resource name |
|---|---|
| `%` | `%25` (so a real `%` is never an escape) |
| `: ? * " < > \| \`, control chars but tab, DEL | `%XX` wherever they are |
| leading space or `~` | that character `%XX` (`~x` → `%7Ex`) |
| trailing `.` or space | that character `%XX` (`trail.` → `trail%2E`; `.` → `%2E`, `..` → `.%2E`) |
| device name, `desktop.ini`, `Thumbs.db`, `.DS_Store` (any case) | first character `%XX` (`CON` → `%43ON`, `.DS_Store` → `%2EDS_Store`) |
| `""` (trailing `/` — an S3 folder marker, or `a//b`) | `%` |

**Object leaves carry a marker** (`WebDAVLeafMarker` = `%o`, 2026-10-07): the
last segment of every object is `davName(seg) + "%o"`, folders stay
`davName(seg)`. S3 allows a key `x` and a key `x/y`; WebDAV cannot hold a file
and a folder of one name, and with several bridges the driver cannot even
check: `x` and `x/y` route to different bridges, the second has not seen `x`
yet (cross-bridge staleness 1.5 s – 5 min), creates the folder, and Sync's
cloud keeps the folder and **loses the file** (prod: PUT `x`, PUT `x/y`, GET
`x` → 503). With the marker `x` is the file `x%o` and `x/y` the folder `x`
holding `y%o` — disjoint on every bridge. Unambiguous because a mapped name
holds `%` only before two hex digits or as the empty-segment name `%`, never
before `o`. `photos/` (an S3 folder marker) is the file `%%o` in the folder
`photos`. A file without the marker (written before 2026-10-07, or by
something else) is not an object: List skips it, Get/Exists miss it; the
erasure walk (`WalkTenant`) still returns it so an erasure removes it.
`TestMultiWebDAV_FileAndFolderOfTheSameNameSurviveCrossBridgeSync` reproduces
the loss (two bridges, separate file systems, a folder-wins cloud sync).

`keySegment` decodes `%XX`. A mapped name over 248 UTF-16 units — **the leaf
marker included, so 246 for an object's last segment** — is refused on PUT as
`engine.ErrInvalidInput` before any request; Get/GetRange answer not-found,
Exists false and Delete succeeds for such a key (it can never have been
stored; the S3 layer used to answer 500). `TestDavName_RoundTripsAndIsAlwaysSyncSafe`
checks 20,000 random segments against an independent statement of the rules;
`TestWebDAVDriver_SyncNameRules` runs every hostile key through the driver
against a server that refuses/drops exactly like the bridge.

**Refused names are the caller's error.** A PUT or MKCOL answered 400 or 414,
and a PUT still answered 404/409 with every folder created (a file where a
folder is needed — only possible against an unmarked file now), wrap
`engine.ErrInvalidInput`: no breaker is charged, the
failover chain stops (the object is never stored on the next candidate), and
the S3 API answers **400 InvalidArgument**. Before (2026-10-07) five bad names
opened the `sync` breaker and every tenant's sync-tier writes went silently to
the primary for 30 s; the client got 503 and retried forever.

**Request URLs** (CodeQL `go/request-forgery`): the configured URL is parsed
once in the constructor (http/https, a host, no userinfo/query/fragment) and
kept as a `url.URL` holding only scheme + host. Every request URL is a copy of
it with only `Path`/`RawPath` set (`requestURL`); the escaped path must match
what `escapedPath` produces (absolute, `url.PathEscape`d segments — no `?`,
`#`, raw `\`, scheme), may hold no empty, `.` or `..` segment (also between
backslashes, which Windows-backed servers split on), no encoded `/` or NUL,
and must be the root folder or below it (or exactly the server's own folder,
the health check's fallback). The built request's scheme + host are compared
to the configured ones before it is sent. A refusal wraps
`engine.ErrInvalidInput` (no breaker charge). Keys that try to climb
(`..\..\x`, `a\..\b`) are refused; `%2e%2e` is a literal name (`%252e%252e`).

## Operations

| Op | Request | Notes |
|---|---|---|
| `Put` | `PUT` (streamed; `Content-Length` when `PutOptions` carries it, else chunked) | Missing folders: the deepest unknown parent is `MKCOL`ed first (201/405 = exists), on 409 every ancestor top-down; known folders are cached (`sync.Map`, per process). One MKCOL per path at a time in the process (concurrent PUTs into a new folder otherwise race and a server answers the loser **423 Locked** — x/net/webdav does; found by `webdav-bench` parity), and a 423 from another process is retried 5× (100 ms × attempt). A PUT answered 404/409 (a folder removed behind the cache) recreates the folders and is retried once when the body can be rewound or was not read; otherwise the error wraps `engine.ErrNoFailover`. With a known length the stored size is checked by `PROPFIND` Depth 0 `getcontentlength`: a short object is an error ("truncated upload"). Overwrite replaces. |
| `Get` | `GET` | 404 → `engine.NotFoundError` (the engine's miss: no breaker charge), like local/OneDrive |
| `GetRange` | `GET` + `Range: bytes=off-end` | 206 accepted (`Content-Range` start checked); a server that ignores Range and answers **200** has the bytes before the offset discarded and the rest limited — never the wrong bytes; 416 / offset past the end = empty |
| `Delete` | `PROPFIND` Depth 0, then `DELETE` | A WebDAV DELETE of a folder is recursive, so a key that is a folder is left alone (`DeleteObject("a")` never removes `a/b`); 404 = nil |
| `Exists` | `PROPFIND` Depth 0 | Not HEAD (some bridges special-case it); a folder is not an object |
| `List` | `PROPFIND` Depth 1 per folder, recursively | Depth infinity is often disabled. Keys relative to the container, sorted; folders skipped and pruned by the prefix; hrefs may be absolute URLs or paths, percent-encoded any way, with or without the trailing slash; an href outside the folder asked for is an error |
| `WalkTenant` | the same walk of `t-<tenant>/` | `engine.TenantWalker` for the erasure sweep: every file with a `Remove` bound to the listed resource; `""` / `/` ids refused (`checkWalkTenant`); a listing that cannot finish is an error. Empty folders are left behind (names only) |
| `HealthCheck` | authenticated `PROPFIND` Depth 0 of the root (the server URL when the root folder does not exist yet) | A wrong password is a 401 → failure (architecture decision 1: never a bare GET). Probed by `api/backend_probes.go` when `SYNC_WEBDAV_PASSWORD` or `SYNC_WEBDAV_PASSWORDS` is set (several bridges: every bridge, healthy while one is — see above); alert `SyncProbeFailing` (warning, 10 m) in `deploy/monitoring/vaultaire-backends.yml` |

HTTP: `TunedHTTPClient(WithHTTP1Only(), WithResponseHeaderTimeout(5m))`
(`VAULTAIRE_TUNED_TRANSPORT=false` = `http.DefaultClient`); 60 s per
PROPFIND/MKCOL/DELETE attempt. Basic auth on every request; credentials in a URL are
refused (a URL lands in error messages); the password is never logged. 401/403
and what is left after the retries below are returned as errors — the
engine's breaker handles them.

## Stalls, retries, concurrency (`webdav_resilience.go`)

The live bench against the bridge (2026-10-06, SLC) found: every op is a
0.5–1 s round trip and throughput saturates at ~10–25 ops/s; at 32 concurrent
requests the bridge answered some GETs and DELETEs **500** (once at 8, on
DELETE); and with 4 concurrent 256 MiB PUTs it **stopped reading one body after
62 KB and never answered** while serving everything else — the client sat in
`io.Copy` for 45+ minutes. Every request now goes through one path (`send`):

| Bound | Default | Option / env | What it does |
|---|---|---|---|
| Concurrency cap | 8 | `WithWebDAVMaxConcurrency`, `SYNC_WEBDAV_MAX_CONCURRENCY` (1..256) | A per-driver semaphore: a request waits for a slot under the caller's context. PROPFIND/MKCOL/DELETE/PUT hold it for the whole request; a **GET gives it back at the headers** (its body streams to a caller that may be copying it into this same driver — held slots would let 8 such copies deadlock it) |
| Idle timeout | 60 s | `WithWebDAVIdleTimeout`, `SYNC_WEBDAV_IDLE_TIMEOUT` (1s..1h) | PUT: the server consumes no body byte for that long (the timer runs only while the transport holds bytes the server has not taken — never while the caller's source is slow). GET: no answer for that long, or a body that yields no byte for that long **while the caller is inside `Read`** (the caller's own pace is never a stall). The request is cancelled with `ErrWebDAVStalled` (wraps `engine.ErrTimeout`) |
| PUT deadline | 2 min per started 64 MiB, ≤ 6 h | `WithWebDAVPutTimeout` | Known-length PUTs only (the fixed-bucket `putDeadline` scaling, ≈ 0.5 MB/s floor). Bounds the wait after the last body byte (the bridge may answer only after its own upload to Sync). **It counts only the server's time** (`paceDeadline`): paused while the transport waits in the source's `Read` — a slow client through Cloudflare ran it out and the bridge was charged (Prompt 2b A2). A source that fails mid-body (`errWebDAVSource`, which wraps `engine.ErrCallerAborted` — also for a body ≤ 8 MiB read into memory first, and while a stripe piece is staged) is the caller's error: not retried, not a bridge failure, not an engine breaker charge, `engine.ErrNoFailover`; the API answers 400 IncompleteBody. A GET has the idle timeout only |
| Source minimum rate | 32 KiB/s averaged over 60 s of waiting for the body | `SYNC_WEBDAV_SOURCE_MIN_RATE` (`off` disables), `WithWebDAVSourceMinRate` | The PUT deadline pauses while the transport waits for the source (A2) and the API server has no body timeout, so a trickling client held its bridge's request slot for as long as it liked (8 of them blocked a bridge, 3 its large uploads). Below the rate the upload fails with `engine.ErrSourceTooSlow` (an `ErrCallerAborted`: the API answers 408 RequestTimeout, nothing is charged); a read the source never returns from is cut when the window runs out (the request is cancelled, its slot freed). Only time spent waiting for the source counts, never the bridge's own pace; a seekable body (a staged piece) is never guarded (Prompt 2b.2 C1 P2-b) |
| Retries | 3 attempts, jittered backoff from 250 ms (×2) | `WithWebDAVRetries` | On 500/502/503/504, 423, a transport error (reset, EOF, refused), a stall or a timeout — for PROPFIND, MKCOL (423 keeps its own 5× loop), DELETE and a GET **before its body reached the caller**. A PUT only when the body can be sent again: seekable → rewound; non-seekable with a known length ≤ 8 MiB → held in memory for it (the engine hands drivers a non-seekable reader); larger → never buffered, and a failure after its bytes went out wraps `engine.ErrNoFailover`. A 2xx is never retried; a caller whose context is done is never retried |

Metrics (on `drivers.Collectors()`, every series at 0 per backend from the
constructor): `vaultaire_webdav_requests_total{backend,bridge,method,outcome}` — one
per attempt, outcome `ok | http_4xx | locked | http_5xx | transport_error |
stall | timeout | canceled`; `vaultaire_webdav_stalls_total{backend,bridge,direction}`
(`upload | download`); `vaultaire_webdav_retries_total{backend,bridge,method}` (`bridge` = the index into `SYNC_WEBDAV_URLS`, `"0"` for one server).
`WebDAVDriver.Stats()` returns the same stall/retry counts for one driver
(the bench prints them). Tests: `webdav_resilience_test.go` (a handler that
reads 62 KB then blocks, one that sends 100 KB then blocks, one that never
answers headers, flaky 5xx/423, an in-flight peak counter).

## Several bridges (`webdav_multi.go`, `MultiWebDAVDriver`)

One `sync-webdav` process is single-core and CPU-bound. The box runs
several — one per Sync device profile, each with its OWN WebDAV password, all
mounted on the same Sync folder (`http://127.0.0.1:4918` … `:4922`) — and the
backend `sync` spreads over them. `cmd/vaultaire` always builds the
`MultiWebDAVDriver` (one bridge = the old single-server behaviour).

Live on SLC (2026-10-07), against the real bridges:

| Load | PUT | GET |
|---|---|---|
| 256 MiB, 1 bridge | 30 MB/s | 84 MB/s |
| 256 MiB, 2 concurrent per bridge × 5 | 131 MB/s | 353 MB/s |
| 256 MiB, 3 concurrent per bridge × 5 | 156 MB/s | 450 MB/s (0 errors, 0 stalls) |
| 64 KiB, 8 concurrent per bridge × 5 | 21/s | 41/s (DELETE 15/s — metadata writes are limited per Sync account, not per bridge) |

Occasional HTTP 500s (0.1–0.25 %) under load are absorbed by the retries.

**Cross-bridge staleness** is what the design rests on: every bridge caches
metadata. A file written through bridge A is visible through bridge B after
**1.5–13 s**; an overwrite after **~30 s (once 310 s)**; a delete after
**~30 s**. Within ONE bridge every read is consistent (0 stale reads in
hundreds of rounds). `webdav-bench -urls … -run crossbridge` measures it.

| | |
|---|---|
| Routing | Every key lives on ONE bridge: rendezvous (HRW) hashing — FNV-1a + splitmix64 of `bridge-id ‖ 0 ‖ <resource path below the root>` (`t-<tenant>/<container>/<key…>`), highest score wins; the bridge id is its URL (scheme://host + path). 10k keys over 5 bridges land within ±15 %; removing a bridge moves only its keys (~1/N), adding one only takes ~1/(N+1). `Put`, `Get`, `GetRange`, `Exists`, `Delete` go to the key's bridge |
| Changing the bridge set | Restart with the new `SYNC_WEBDAV_URLS`. The ~1/N keys that move are read from their new bridge, which may not yet see the old bridge's last writes (≤ minutes, see above). Changing a bridge's **URL** (port) is a remove + an add |
| Fallback reads | **Immutable names only** (`immutableObject`: a parity shard `<tenant>__parity/<24 hex>/<etag>/p<j>`, a pack `_global/_packs/<aa>/<sha256>.pack`; inside the driver a stripe piece and a generation's `retired%o` marker). A generation's key file is NOT immutable since #653 — its heartbeat is rewritten every 30 min — so the reaper reads it on every bridge and the latest heartbeat wins (`readKeyFile`). **A mutable name — an object's `%o` file, a striped object's `%s` manifest — is answered by its routed bridge only** (`readRouted`): a fallback that still saw the previous version served old bytes under the new head row's ETag, or a deleted object existed (Prompt 2b A1). Routed bridge marked down or answering unavailable = `ErrWebDAVBridgeDown` (wraps `engine.ErrAllBackendsUnavailable` → **503 + Retry-After**), counted `routed_down`. A bridge marked down still gets one real read every 5 s (`bridgeTrialEvery`), and any real answer from a bridge (a success, a miss, a refusal) marks it up at once — not only at the next probe (a bridge restarted at 17:09:57 kept its keys at 503 until the probe at 17:10:35). For an immutable name: `Get`/`GetRange`/`Exists` whose bridge is unhealthy — its last probe failed, or 3 consecutive transport errors / stalls / timeouts / 5xx-after-retries opened its breaker for 30 s, or this very call failed that way — go to the next bridge in the key's HRW order. **A fallback bridge's NOT FOUND (or `Exists` false) is never reported as such**: the object may have been written through the dead bridge seconds ago. It is `ErrWebDAVBridgeStale`, which wraps `engine.ErrAllBackendsUnavailable` → the API answers **503 + Retry-After**. A live key bridge's miss is a miss (authoritative). A 401/403/4xx is never failed over. When no bridge looks healthy the key's own bridge is asked. Counted in `vaultaire_webdav_fallback_reads_total{backend,outcome=served\|stale_miss\|failed\|routed_down}` |
| The engine's breaker | One per backend: `sync` as a whole. A call that failed because ITS bridge is out (refused, stalled, 5xx after the retries, marked down) while another bridge is healthy is **`engine.ErrPartiallyUnavailable`** (`settle`): a 503 + Retry-After for that object, no charge to the engine's `sync` breaker, no failover to another backend, never a miss. Only when no other bridge is healthy is the failure the backend's (charged; 5 in 60 s open it). Prompt 2b.2 C1: before, one bridge down (≈ 20 % of reads of mutable names) opened the breaker within seconds, every `sync` read — Vault parity rebuilds included — answered 503 for 30 s, again and again, and SYNC-tier writes failed over to the primary. A striped object caught mid-change (`errStripePieceGone`, `errStripeAmbiguous`) is the same 503, never a charge. A write of a down bridge's key (≈ 1 in 5 with one of five down; every striped upload, whose pieces span all bridges) is that 503 too |
| Writes | **Never fail over**: a `Put`/`Delete` of a down bridge's key fails (the caller retries — immutable, content-addressed callers like parity shards and packs retry later). Writing elsewhere would leave the key's own bridge stale for minutes. **A `Delete` the routed bridge answers "not there"** (no `%o`, no manifest) asks every other bridge and deletes the object wherever it is seen (`deleteOn`; `vaultaire_webdav_delete_elsewhere_total`); a bridge that cannot answer then fails the delete with `ErrWebDAVBridgeDown` — never "deleted" on one bridge's word (Prompt 2b A4: a write through another bridge, or a bridge-set change, left the bytes) |
| `List` | **The union of every bridge's listing** (each key once, sorted); a bridge that cannot list fails it (Prompt 2b A3). A key deleted a moment ago may still be listed by a lagging bridge. No product path calls it: `engine.List` reads the primary (`sync` can never be one), the erasure sweep walks (`WalkTenant`), the parity reconcile uses `ListDir`. `WalkTenant`'s `Remove` goes to the bridge that listed the object AND its routed bridge |
| Files per folder | Sync refuses writes past **50,000 files per folder** (key `/` segments are folders). A PUT the bridge REFUSED (an HTTP status — never a timeout, stall, broken connection or the caller's body: those say nothing about the folder, and the check used to add a PROPFIND, 60 s on a stalled bridge, to every one) is checked against its folder (one Depth 1 PROPFIND under the idle watchdog, the count cached 1 min, a failed count 30 s): at the limit it is `engine.ErrInvalidInput` (400, no breaker charge, no failover; `vaultaire_webdav_folder_full_total`). Every Depth 1 listing is bounded by the idle watchdog while it streams (10 min overall), not a fixed 60 s. `vaultaire_webdav_folder_files_max{backend,bridge}` = the largest folder among those a bridge last counted (a recount after a cleanup lowers it) (every Depth 1 listing, and a recount every 1,000 PUTs into a folder); rule `SyncFolderNearFileLimit` warns above 40,000. The layout fix (a hash fan-out level) is an open decision (Prompt 2b A5) |
| Folders: `ListDir`, `RemoveEmptyDir` | `ListDir` lists ONE folder's direct members through one bridge (the folder's HRW bridge, read fallback) — one PROPFIND. **`RemoveEmptyDir` never trusts one bridge**: a collection `DELETE` is recursive (RFC 4918 §9.6.1) and every bridge mounts the ONE account, so a bridge that has not yet seen another's write would wipe it. Every bridge must list the folder empty first (the first that does not stops it: `ErrDirNotEmpty`, nothing deleted), then ONE `DELETE` goes through one bridge. **No listing can see a write that lands between the last empty listing and the DELETE** — it would go with the folder. The guarantee is the callers': the vault parity job is the only writer of its shards and the only remover of its folders, under one lock (Prompt 2a.3 H1 took folder removal off the DELETE path); the stripe reaper removes only first-layout key folders, which no upload writes any more. The listing after the DELETE sees only content written after it — which survived: a Warn, not a failure. Folder segments are mapped like object folders (`davName`); the container itself is refused. Callers: the vault parity reconcile (only it, since Prompt 2a.3 H1); the stripe reaper's first-layout key folders (`removeEmptyNames`). A dead stripe generation's folder too, since Prompt 2b A6 (`removeGeneration`, `removeGenerationDir`): its files are deleted by name, then the folder only when every bridge lists it empty — a piece another bridge already saw and the listing bridge did not used to go with a recursive DELETE; and a generation is live when ANY bridge's manifest names it (one that has not seen the commit answered "none") (prompt 2a.2, 2026-10-08: the old `RemoveEmptyDir` let each of the 5 bridges send its own `DELETE` on its own view — 5 DELETEs, a lagging bridge's took a live shard) |
| `WalkTenant` (erasure sweep) | The **union of every bridge's walk** of `t-<tenant>/`, each object once — an object written through any bridge within the staleness window is still found; each object's `Remove` goes to its routed bridge. A bridge that cannot list fails the walk → the sweep defers the tenant (never "erased" while a bridge may still name an object) |
| Large transfers | A body ≥ 16 MiB (or of unknown length) takes one of `SYNC_WEBDAV_LARGE_CONCURRENCY` (default **3**) slots **per bridge and direction** on top of the per-bridge total cap (`SYNC_WEBDAV_MAX_CONCURRENCY`, 8). Upload and download slots are separate, so a copy between two keys of one bridge cannot deadlock; a download slot is taken at the GET's headers (by `Content-Length`) and given back at the body's `Close` |
| Health | `HealthCheck` probes every bridge in parallel: the backend is healthy while **≥ 1** bridge is (`SyncProbeFailing` = all down). Per bridge `vaultaire_webdav_bridge_up{backend,bridge}` (1/0); alert `SyncBridgeDown` (warning, 10 m) in `deploy/monitoring/vaultaire-backends.yml`; transitions are logged |
| Per bridge | Each bridge is a full `WebDAVDriver`: its own password, concurrency cap, idle timeout, retries; the `vaultaire_webdav_*` counters carry `bridge="<index into SYNC_WEBDAV_URLS>"` (`"0"` for a single server) |
| `StoreID` | `dav-multi:` + every bridge's `dav:` id, sorted |

**Never use Sync's modification times for age decisions.** The bridge reports
a WRONG `getlastmodified` for every file (live, 2026-10-07: a file uploaded
now lists as `1970-01-21T17:35:39Z`; an rclone `--min-age` retention deleted
fresh files because of it). The driver never asks for it — `PROPFIND` requests
`resourcetype` + `getcontentlength` only (`propfindBody`, guarded by
`TestWebDAV_NeverAsksForModTimes`) — and no listing, walk, health or fallback
decision rests on a server time. `engine.TenantObject` carries no modtime; if
one is ever added, for `sync` it is unreliable. Any age-based job over `sync`
(retention, GC grace) must take its times from our own tables
(`object_head_cache`, `global_content_index`), never from the bridge.

## Striped large objects (`webdav_stripe.go`)

One object is one PUT to one bridge, and one bridge uploads at ~30 MB/s
(prod 2026-10-07: 2 GiB in 71 s, 5 GiB in 167 s) — while five bridges move
156 MB/s up / 450 down when they work in parallel. So the multi-bridge driver
**stripes a Put of a known length ≥ `SYNC_WEBDAV_STRIPE_MIN`** (default
512 MiB) into pieces of `SYNC_WEBDAV_STRIPE_PIECE` (default 256 MiB), each
routed by HRW on its own name — the pieces spread over every bridge — and
uploaded in parallel. Smaller objects, and bodies of unknown length
(`ContentLength` 0), are one file as before.

| | |
|---|---|
| Layout | `t-<tenant>/<container>/…/<leaf>%s` = the **manifest** (the object); `t-<tenant>/<container>%p/<hh>/<h>-<gen>/p00000%o …` = the pieces and `key%o` (which object they are for) — one folder per upload, nothing shared between uploads of a key, so no per-key folder is left behind (`<hh>` holds ≤ 50,000 entries on Sync; the first layout of #626, `<hh>/<h>/<gen>/`, still reads). `<h>` = 32 hex of sha256(key), `<hh>` its first two; `<gen>` = `g<UTC yyyymmddThhmmssZ>-<16 hex>` — the generation's age without a server modtime. `%s` and `%p` are never produced by the name mapping (a literal `%` is `%25`), so an ordinary object can never be read as a manifest and a container listing never shows a piece |
| Manifest | JSON: format `vaultaire-webdav-stripe/1`, logical size, piece size, generation, the generation's folder, each piece's size + sha256. Validated on read (folder inside the object's container, sizes add up) |
| Write | key file (JSON: container, key, **heartbeat**) → pieces (≤ K staged on disk under `SYNC_WEBDAV_STAGING_DIR/p<pid>` and in flight, K = bridges × `SYNC_WEBDAV_LARGE_CONCURRENCY` = 15 by default, **one upload at most K minus one bridge's slots** so another striped PUT still progresses (Prompt 2b B5); each from a seekable section of its staging file, so a bridge's 5xx is resent; each size-verified; each holds a large-upload slot of ITS bridge) → **the key file's heartbeat rewritten every 30 min while pieces upload; an upload whose last heartbeat is older than half the reaper's grace fails** → a last heartbeat and a **stat of every piece on its bridge** (a piece the reaper took fails the PUT, `ErrNoFailover` — never a manifest naming a missing piece; Prompt 2b B1) → **under the key's commit lock** (the plain path's PUT + manifest removal and a Delete take it too: interleaved, a plain and a striped write deleted each other's file and both answered 200 for a 404 — B3) **manifest last** → the key's previous plain `%o` file deleted → the previous generation **retired** (`retired%o` = the time; the reaper deletes it an hour later, so a GET already streaming it finishes — B4). **A manifest PUT that errors** is read back (B2): it names this generation = committed (success); another or none = this generation is dropped; unreadable = nothing deleted, the error returned (`ErrNoFailover`) — a generation is never deleted without proof that no manifest names it (a 5xx after the bridge stored it used to delete the new pieces under the new manifest, the old manifest already replaced: both versions lost). A failure before the manifest cancels the other pieces, waits for every piece upload to return, then deletes every piece it STARTED (a cancelled PUT may have been stored without a confirmation) and repeats that pass twice, 2 s apart, for a request a bridge stores late (else the reaper does); the Put error wraps `ErrNoFailover` once the body is spent |
| Read | **Manifest cache**: up to 4,096 manifests per process, 10 min TTL, LRU — filled by a striped Put and by every manifest read, dropped by this process's Put/Delete of the key; a cached object is read straight from its pieces after **one stat of its generation's `retired%o` marker** (Prompt 2b B4: a retired generation's pieces stay an hour, so a missing piece no longer tells a cache it is stale — another slot's overwrite or delete is seen through the marker) — no plain-file probe, no manifest GET (each a Sync round trip, ~0.7 s). A retired or vanished generation drops the entry and resolves the key again. A freshly read manifest naming a retired generation is read once more; a second retired one is a 503 (`ErrWebDAVBridgeStale`), never the old bytes. **With the recorded size** (`engine.WithExpectedSize`, the API's GET and `/cdn` pass the head row's size): a plain `%o` file of another size is not the committed version — the manifest of that size is served, neither = 503 (`errStripeAmbiguous`); after an interrupted commit both files exist and their sizes differ whenever the stripe minimum has not moved (plain below it, manifest at or above). Without a size the `%o` file is read first, as before. Uncached: the manifest and a plain object share the key's bridge (routed by the `%o` name); `%o` is read first, the manifest on a miss. The first two pieces of a read are opened **at once** (a range across a boundary waits one round trip, a whole read has the next piece on its way before the first byte), then always one ahead; a range opens only the pieces it spans; a whole piece is sha256-verified at its end. A piece missing at open (overwritten since the manifest was read) → the manifest is read once more; a piece missing mid-stream is a 503 (`ErrAllBackendsUnavailable`), never a mix of versions or a miss. Each piece read goes to the bridge that wrote it (fallback rules as above) |
| The key's commit lock | One per key in a process (plain PUT + manifest removal, the striped commit, a Delete). The plain path holds it for its whole upload, so another call of the same key waits: the wait honours the caller's context and one that runs out is `engine.ErrPartiallyUnavailable` (503, never a backend failure; Prompt 2b.2 C3 — a Delete with a 200 ms deadline waited behind a stalled PUT for as long as the PUT took). Uploading to a temporary name and MOVE-ing it into place would shrink the lock to the commit — the bridge supports MOVE (live 2026-10-09 on 4919: new name 201 in 0.54 s, over an existing one with `Overwrite: T` 204 in 1.05 s, `Overwrite: F` 412, another bridge saw the result at once) — but it costs every plain PUT one more ~0.5–1 s round trip and one more Sync metadata write, the account-wide bottleneck: not done. An overwrite or delete that finds an INVALID manifest still naming its generation retires that generation (gone an hour later); one that names none is left to the reaper's 6 h grace |
| Overwrite / Delete | Striped → striped: new generation, manifest replaced, old generation retired. Striped → plain: the plain file, then the manifest goes and its generation is retired. Plain → striped: the plain file goes after the manifest. Delete: plain file, manifest, generation retired — **a deleted striped object's pieces stay on Sync until the reaper's next pass after the hour (≤ 2 h)**, unreadable through the API (no manifest); an account erasure's sweep walks and deletes them at once. A Put/Delete of a plain object costs one Depth 0 PROPFIND more (does a manifest exist?) |
| List / WalkTenant | `List` reports the logical object (once, even beside a leftover plain file). `WalkTenant` (erasure sweep) reports the manifest under the object's name and every piece / key file as a file of container `<container>%p` — each with its own `Remove`, so the sweep deletes all of it |
| Reaper | `ReapOrphanStripes(ctx, olderThan)` — the **`stripe_gc` job** (1 h, boot +9 m): a retired generation one hour after its marker; any other generation whose latest sign of life — its NAME's time, or the key file's **heartbeat on any bridge** — is older than 6 h and that no manifest **on any bridge** references (Prompt 2b B1/A6); the heartbeat is read again as the last thing before the deletes; (key file missing, or the object's manifest names another generation) are deleted, file by file through each file's bridge, then the folder; empty generation folders past the grace and emptied first-layout key folders are removed (`folders_removed`). A generation folder goes only when every bridge lists it empty (A6). A generation whose key file does not match its folder, or whose manifest cannot be read, is kept (an error in the result). Never a server modtime |
| Crash window | A crash between writing the manifest and deleting an old plain `%o` file leaves both; a read with the recorded size serves the right one (see Read), a read without one serves the plain file until the next write of the key. Commits of one key in two processes overlap only during a deploy's drain (no distributed lock) |
| Staging | `<SYNC_WEBDAV_STAGING_DIR or /tmp/vaultaire-stripes>/p<pid>/`: the two app slots share `/tmp` (the unit has no `PrivateTmp=`), so each process stages in its own folder and the boot sweep removes only folders of processes that no longer exist (and staging files of the old shared-root layout older than 7 h) — never the other slot's in-flight pieces (Prompt 2b B5) |
| Metrics | `vaultaire_webdav_stripe_pieces_total{backend,op=written\|read\|deleted}`, `vaultaire_webdav_stripe_put_bytes_total{backend}`, `vaultaire_webdav_stripe_orphans_total{backend,outcome=left\|retired\|reaped}` (a Warn names every generation a failed write could not delete) |

## Configuration (`sync` instance)

| Env | Default | |
|---|---|---|
| `SYNC_WEBDAV_PASSWORD` | — | **Required to register** the `sync` driver with ONE bridge: the bridge's generated password (`sync-webdav credentials`) |
| `SYNC_WEBDAV_URL` | `http://127.0.0.1:4918` | The bridge (single form) |
| `SYNC_WEBDAV_URLS` | — | **Several bridges**: comma-separated URLs, 1..16, distinct (same rules as `SYNC_WEBDAV_URL`: http/https, a host, no credentials/query/fragment). Set = the single form is ignored (warning) |
| `SYNC_WEBDAV_PASSWORDS` | — | One password per `SYNC_WEBDAV_URLS` entry, same order, comma-separated (a password cannot contain a comma). Secrets: never logged, never in an error (errors name the bridge index). Counts must match; a list that does not validate leaves `sync` unregistered with a boot Error |
| `SYNC_WEBDAV_LARGE_CONCURRENCY` | `3` | Transfers ≥ 16 MiB per bridge and direction (1..256) |
| `SYNC_WEBDAV_USER` | `sync` | |
| `SYNC_WEBDAV_ROOT` | `vaultaire` | Folder under the bridge's root objects live in |
| `SYNC_WEBDAV_MAX_CONCURRENCY` | `8` | Requests in flight to EACH bridge (1..256). The bridge 500s some requests at 32 |
| `SYNC_WEBDAV_IDLE_TIMEOUT` | `60s` | A transfer the bridge makes no progress on for this long is cancelled (1s..1h). A rejected value of either is logged at Warn and the default kept |
| `SYNC_WEBDAV_STRIPE_MIN` | `512MiB` | A known-length Put this large or larger is striped (sizes: bytes or K/M/G/T(iB)); `0`/`off` = never. Below the piece size it becomes the piece size (warning) |
| `SYNC_WEBDAV_STRIPE_PIECE` | `256MiB` | Piece size, 16MiB..4GiB |
| `SYNC_WEBDAV_STAGING_DIR` | `<tmp>/vaultaire-stripes` | Absolute path; holds at most K pieces at once (15 × 256 MiB = 3.75 GiB by default) |

## The Sync.com bridge (`sync-webdav`)

Facts from Sync's guide (help.sync.com, "WebDAV Guide for Linux", 2026-10-02):
the program runs on OUR box, encrypts client-side (end-to-end encryption is
preserved) and serves plain WebDAV at `http://127.0.0.1:4918/` with Basic auth
(user `sync`, a generated password). It accepts connections from localhost only
unless `--allow-from` says otherwise. Uploads are spilled to a temp directory
(`--upload-temp-dir`) while they stream to Sync.

```bash
# as a dedicated system user (sync-webdav), on the box
sudo useradd --system --create-home --home-dir /var/lib/sync-webdav --shell /usr/sbin/nologin sync-webdav
sudo -u sync-webdav -H sh -c 'curl -LsSf https://www10.sync.com/download/webdav/install.sh | sh'
# sign in once (headless: prints a URL, paste the code back); saves the session
sudo -u sync-webdav -H /var/lib/sync-webdav/.sync-webdav/bin/sync-webdav login --headless
# show / create the WebDAV password → SYNC_WEBDAV_PASSWORD in /opt/vaultaire/configs/.env
sudo -u sync-webdav -H /var/lib/sync-webdav/.sync-webdav/bin/sync-webdav credentials
```

Options we use: `--mount <folder>/` scopes the bridge to one folder of the
Sync account (`vault/` = the Vault; default = Files) — use a dedicated folder,
e.g. `--mount stored/`; `--allow-from none` (localhost only, the default — keep
it); `--upload-temp-dir` on a disk with room for the largest object in flight
(a full spill disk is what the size check after each PUT catches);
`--analytics false`; `--log-file`. `--daemon` backgrounds the process — under
systemd run it in the foreground instead.

`/etc/systemd/system/sync-webdav.service`:

```ini
[Unit]
Description=Sync.com WebDAV bridge (encrypted, localhost only) for Vaultaire
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=sync-webdav
Group=sync-webdav
Environment=HOME=/var/lib/sync-webdav
ExecStartPre=/usr/bin/mkdir -p /var/lib/sync-webdav/spool
ExecStart=/var/lib/sync-webdav/.sync-webdav/bin/sync-webdav \
  --port 4918 --mount stored/ --allow-from none \
  --upload-temp-dir /var/lib/sync-webdav/spool \
  --analytics false --log-file /var/lib/sync-webdav/sync-webdav.log
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/sync-webdav

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now sync-webdav
curl -s -u sync:"$SYNC_WEBDAV_PASSWORD" -X PROPFIND -H 'Depth: 0' http://127.0.0.1:4918/ -o /dev/null -w '%{http_code}\n'   # 207
```

Then set `SYNC_WEBDAV_PASSWORD` in `/opt/vaultaire/configs/.env` and restart
Vaultaire: the boot log says "Sync WebDAV driver added", `/health/backends`
shows `sync`. Updating the bridge: re-run the installer, restart the unit
(`sync-webdav update` also exists). `sync-webdav credentials --reset` changes
the password — update the env, or the probe pages 401.

## Other WebDAV services

The same constructor serves them; register a new name in `main.go` (and in
`backend_probes.go`, `engine.targetOnlyBackends` if it must not be a failover
target, `backendToStorageClass`): Hetzner Storage Box
(`https://uXXXXX.your-storagebox.de`), Koofr
(`https://app.koofr.net/dav/Koofr`), pCloud (`https://webdav.pcloud.com`),
Nextcloud (`https://host/remote.php/dav/files/<user>`). A URL path is kept
(the root is below it).

## Tests

`webdav_test.go` — `golang.org/x/net/webdav` (MemFS + MemLS) behind
`httptest` with a Basic-auth check: round trip (multi-MiB streamed body, known
and unknown length), overwrite, nested keys + MKCOL caching, a folder deleted
behind the cache (rewindable retry; a sent stream is `ErrNoFailover`), special
characters (`..`, `.`, empty segments, folder markers, `~`, `%`, `#`, `?`,
unicode), GetRange 206 and a server that ignores Range, not-found mapping,
delete (missing = nil; never a folder), List (prefix, nesting, empty folders,
href shapes, a foreign href), WalkTenant (neighbour tenants, Remove twice, bad
ids, fn error), no tenant → `ErrNoTenant` + chunk context, HealthCheck (ok /
401 / down), a server that truncates a PUT, 5xx returned. `webdav_guard_test.go`
— the request-URL guard (scheme/host fixed, root escapes / `%2e%2e` /
backslash dot segments / query / fragment / encoded `/` refused), hostile keys
end to end (no request off the root or to another host), 32 concurrent PUTs
into new folders (the 423 race), a MKCOL 423 retried. Benchmark:
`cmd/tools/webdav-bench` (cmd/tools/README.md). `webdav_multi_test.go` — HRW spread (10k keys over 5 bridges ±15 %) and stability (removing a bridge moves only its keys), each op on the routed bridge (one x/net/webdav server per bridge, own password, request log), fallback served / stale miss never NotFound / 5xx fallback / 401 not failed over, WalkTenant union + Remove on the routed bridge + a bridge that cannot list fails the walk, List fallback, health aggregation + gauge, large PUT/GET caps, `SYNC_WEBDAV_URLS` parsing (no password in any error), no modtime asked. `webdav_config_test.go`
— env defaults and `StoreID`.
