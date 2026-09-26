# R6 Engine core — 2026-09-26

**Repo state reviewed:** `main` @ `fd1e8b8` (#477). Branch: `review/R6-engine`. **PR:** #478.
**Depends on:** R0 (`load_balancer.go` kept only for `internal/ha`; `monitor.go`, `migrator.go`,
`migration_progress.go`, `performance_monitor.go`, `context.go` already deleted — they are not
reviewed here), R1 (engine `HealthCheck` and `StartTiering` have no callers; caching is switched OFF
at `main.go:175-183`), R5 (tenant scoping is decided in the API layer; containers are
`<tenantID>_<bucket>`, `tenant/tenant.go:73-77`).

## Scope reviewed

| File | Lines | Depth |
|------|------:|-------|
| `internal/engine/engine.go` | 870 | fully |
| `internal/engine/failover.go` | 255 | fully (breaker + `Execute` + `isBackendFailure`) |
| `internal/engine/interface.go` | 154 | fully |
| `internal/engine/errors.go` | 62 | fully |
| `internal/engine/routing.go` (`LocationStore`) | 108 | fully |
| `internal/engine/storage_class.go` | 83 | fully |
| `internal/engine/tiering.go` | 278 | fully |
| `internal/engine/{health,selector,cost_optimizer,types}.go` | 108/112/163/55 | fully — constructed but never consulted (R6-17) |
| `internal/engine/{replicator,load_balancer,analytics,capacity,sla,disaster_recovery}.go` | 189/186/139/211/192/244 | skimmed — 100 % unreachable (`deadcode`), class-B files awaiting D-1/D-2/D-9; not reviewed for correctness |
| `internal/cache/tiered_cache.go` | 76 | fully |
| `internal/intelligence/{access_tracker,anomaly,ml_pipeline,types}.go` | 242/92/78/81 | fully |
| `internal/common/context.go` | 71 | fully |
| `internal/api/not_found.go` | 45 | fully (mirror of `isBackendFailure`) |
| API call sites into the engine | — | `s3_engine_adapter.go` GET/PUT/DELETE/range/chunk (`:349-375`, `:473`, `:701`, `:843-880`, `:1298`, `:1408-1425`, `:1868-1900`), `s3_batch.go:148`, `s3_copy.go:131`, `cdn.go:118-135`, `access_log.go:150-166`, `s3_inventory.go:380-388`, `s3_multipart.go:486`, `backend_probes.go`, `health_handlers.go:154-175`, `dashboard/handlers/admin_backends.go` |
| Migrations | — | `048_object_locations.sql`, `049_bucket_tier_preference.sql`, `055_access_patterns.sql` |
| Docs | — | `docs/DRIVERS.md`, `docs/ARCHITECTURE.md`, `internal/engine/CLAUDE.md` |
| Driver error shapes (for R6-01) | — | `drivers/{s3compat,s3,idrive,lyve,r2,geyser,local,onedrive,quotaless}.go` Get/Delete/Exists/List only; SDK sources `smithy-go@v1.27.8/errors.go`, `aws-sdk-go-v2@v1.42.1/aws/transport/http/response_error.go`, `service/internal/s3shared@v1.19.31/{response_error,xml_utils}.go`, `service/s3@v1.105.2/{types/errors,deserializers}.go` |

**Tests run:** `make test-db`; `go test -race ./internal/engine/... ./internal/cache/...
./internal/intelligence/... ./internal/common/...` → engine `ok` (2.2 s), the other three packages
have **no test files**; `go test -race ./internal/api/ -run 'NotFound|Failover|Backend|Placement|StorageClass'`
→ `ok`. `deadcode ./cmd/vaultaire` re-run for the four packages (list under *Dead code noted*).

**Skipped and why:** driver internals beyond the error shape a miss produces (R7); the chunked
GET/PUT pipeline and GCI (R2/R8); the smart-demotion/promotion jobs that call `GetDriver`/`HintBackend`
(R13) — only their engine-facing contract was checked; `cmd/dedup-migrate` (tool).

## Findings

Severity: P0 data loss / security / cross-tenant / money · P1 wrong behaviour a customer hits ·
P2 correctness risk or unsafe pattern not yet triggered · P3 quality / dead code / docs drift.

| ID | Sev | file:line | What | Why it matters | Proposed fix |
|----|-----|-----------|------|----------------|--------------|
| R6-01 | **P1** | `internal/engine/failover.go:143-151`; test fixture `failover_test.go:409` | `isBackendFailure` recognises an S3 miss only by the strings `NoSuchKey` and `status code: 404`. aws-sdk-go-v2 formats the status as **`StatusCode: 404`** (`aws/transport/http/response_error.go:23-26`, `s3shared/response_error.go:23-26`); the lowercase form is aws-sdk-go **v1**, which is not in `go.mod`. For a 404 whose body carries no `<Code>` the deserializer derives the code from the status text → `api error NotFound: Not Found` (`s3shared/xml_utils.go:66-74`); `HeadObject` misses are the modelled `*types.NotFound` (`types/errors.go:462-485`). Neither contains `NoSuchKey`, so every such miss counts as a backend failure. The unit test encodes the v1 casing, which is why it passed. | Five unrecognised misses in 60 s open the backend's breaker for 30 s: reads fall through to other backends (→ 404 for objects that exist, R6-02), writes silently land on the next candidate (→ R6-04). A NotFound storm on iDrive is enough. | **Fixed here**: typed classification — `errors.As` to `*smithyhttp.ResponseError` (works through the S3 `ResponseError.As` chain, `s3shared/response_error.go:31`) for status 404, `smithy.APIError` code `NoSuchKey`/`NotFound`; `NoSuchBucket` stays a backend failure (misconfigured backend). Table-driven test with errors produced by a **real `s3.Client`** against `httptest` for GET/HEAD/DELETE 404 with and without body, 500, 503, 403, connection refused. `api/not_found.go` given the same typed checks. Two more shapes the tests surfaced: a `*NotFoundError` **pointer** (the engine's own test stub returns one) matched neither classifier — both now accept value and pointer; and `NoSuchBucket` must be decided before any 404 string fallback or its message's `StatusCode: 404` reads as a miss. |
| R6-02 | **P1** | `internal/engine/failover.go:194-233`; `engine.go:238-266`; `api/not_found.go:31-43`; `api/s3_engine_adapter.go:351-375` | `Execute` returns `all backends failed: <lastErr>` where `lastErr` is the **last** candidate's error. Read candidates are preferred → primary → every other registered driver (`buildCandidateList`, `engine.go:717-736`); `local` is always registered (`main.go:220`). When the backend that holds the object fails (timeout, 5xx, open breaker), the walk ends on some other backend's not-found, `isObjectMissingErr` matches it and the client gets **404 NoSuchKey**. During an iDrive outage every GET of an existing object is answered "does not exist"; once the breaker opens the 404 is immediate. | Sync/backup clients treat 404 as a remote deletion; a 503 + Retry-After is what they retry. Also inverts the fail-loudly rule that Put already has. | **Fixed here**: `Execute` tracks whether the first (recorded/preferred) candidate was unavailable — skipped for an open breaker or returned a backend failure. If so, a client-level error from a fallback is not a verdict: the aggregate is `ErrAllBackendsUnavailable` (→ 503). An authoritative not-found from the first candidate still returns 404 even if an unrelated backend is down. `isObjectMissingErr` returns false for anything wrapping `ErrAllBackendsUnavailable` (precedence rule, tested). |
| R6-03 | **P1** | `internal/engine/engine.go:347-353`, `:756-771`; `internal/intelligence/access_tracker.go:90-139`; `migrations/055_access_patterns.sql:24` | When a PUT carries no storage class (every `tier_preference='auto'` bucket — the default from 049 — without a public flag: `resolvePutStorageClass` returns `""`, `s3_engine_adapter.go:2006-2018`, and `WithStorageClass` is only appended when non-empty, `:843-846`) the engine asks `AccessTracker.GetRecommendation` and **applies** the answer. The heuristic reads `temperature`, which nothing ever writes (default `'cold'`, no writer in `internal/`), and `access_count`: `cold && count < 5 → "lyve"` (`access_tracker.go:125-131`). `applyBackendRecommendation` accepts any registered durable backend (`TestApplyBackendRecommendation_AllowsDurableBackend` asserts this). Prod registers `lyve` (`LYVE_*` set — R1 Q4; the Lyve probe is live). | The **second to fifth PUT** of any key in a default bucket (overwrite, or a PUT after a GET/HEAD-miss created the row) is placed on Lyve instead of iDrive: wrong tier (Lyve is the `resilient` backend), no smart demotion (it only scans `HotBackend`), Lyve COGS after the promo. The 2026-07-31 fix (`intelligence_routing_test.go:21-28`, 463 objects found on `local`) closed the `local` branch only. `GetRecommendation` is also a **synchronous `QueryRow` without ctx** on every GET and every PUT (`access_tracker.go:101`; `engine.go:229`, `:348`) — R6-11. | **Fixed here**: placement is decided by the API layer (`resolvePutStorageClass`) and the storage-class map only; the recommendation override and the informational GET lookup are removed (`applyBackendRecommendation` deleted with its tests). `LogAccess` stays. DB-backed regression test seeds an `access_patterns` row and asserts the overwrite stays on the primary. |
| R6-04 | **P1** | `internal/engine/engine.go:742`, `:777-787` | The write failover list is "every registered backend except `local`". In prod that is `idrive`, 12 × `idrive-<region>`, `lyve`, `geyser`, `r2`, `permafrost`. If the target and the primary fail (or their breakers are open — R6-01), a STANDARD object is written to whichever comes next in map iteration: **`geyser`** (tape — evicted after ~13 days, then GET = 403 InvalidObjectState for a Standard customer), **`r2`** (the public-bucket store; not directly public today — the R2 bucket is read only through our CDN handler — but a private object in the public store), **`idrive-eu-*`** (data-residency breach for a US bucket, or vice-versa), **`permafrost`** (10 MB/s async second-copy role). | Placement promise and residency broken silently; `object_head_cache.backend_name` records it as normal. `TestEnginePut_DurableToDurableFailoverPreserved` only covers `idrive → s3`. | **Fixed here**: a `targetOnlyBackends` rule — `r2`, `geyser`, `permafrost`, and any `idrive-<region>` — join `local` as backends that receive a write only when explicitly targeted or configured primary. General-purpose failover remains between the target, the primary and `lyve`/`s3`/`quotaless`/`idrive`. Test extended. Lyve's WORM status for failover writes is an open question for R7 (`project_lyve_account_inventory`: one COMPLIANCE lock live to 2053). |
| R6-05 | **P1** | `internal/engine/engine.go:467-476`; `internal/api/s3_engine_adapter.go:1868-1889`, `:1298`; `s3_batch.go:148`; `s3_copy.go:481` | `Delete` resolves the backend from the in-memory `objectBackends` map **only** — unlike `Get`, it never consults `LocationStore` (`:220-225` vs `:468-471`) — and none of the four API delete sites call `HintBackend` first (`HandleDelete` reads the head row for `is_chunked` alone, `:1868-1873`). After any restart the map is cold, so the DELETE goes to the primary alone; iDrive answers 404, `isObjectMissingErr` treats it as an idempotent miss (`:1890-1899`) and the head-cache row is removed. | Every DELETE of an object stored off-primary (resilient/Lyve, archive/Geyser, public/R2, region-pinned `idrive-<region>`, smart-demoted) after a deploy **orphans the bytes on the real backend**: we keep paying, the customer stops, and an erasure request did not erase. No alert can see it (the head row is gone). | **Fixed here**: `HandleDelete` reads `backend_name` with `is_chunked` and hints the engine before `Delete`; `engine.Delete` falls back to `LocationStore` on a map miss like `Get` does. Tests: engine (cold map, DB-backed) and adapter. `s3_batch.go`/`s3_copy.go`/stale-blob delete sites are R3/R2's to align (R6-25). |
| R6-06 | **P1** | `internal/engine/failover.go:121-153`, `:194-205` | `context.Canceled` — the request context cancelled because the **client** went away (HAProxy closes the server side after its 50 s `timeout server`, mobile uploads abort) — reaches the driver, the SDK returns it wrapped (`aws.RequestCanceledError`), `isBackendFailure` says true, and the breaker is charged. Five aborted uploads in a minute open the **primary's** breaker for 30 s. | Same blast radius as R6-01: misplaced writes (R6-04) and fake 404s (R6-02) caused by clients, not backends. | **Fixed here**: `context.Canceled` is not a backend failure; `Execute` stops walking candidates once `ctx.Err() != nil` (a cancelled request must not be retried against 17 other backends). `context.DeadlineExceeded` remains a failure — it is the timeout signal; R7 should give every driver call a per-operation deadline so a stalled backend produces it (WP-R6-3). |
| R6-07 | P2 | `internal/engine/engine.go:717-736`, `:238`, `:293` | Read candidates include **every** registered backend. A genuine miss costs one round trip per backend (18 in prod, ~50 % of them cross-region iDrive endpoints) and can be served from any backend that happens to hold a copy of the same `container/artifact` — the Permafrost delete-resurrection class (`drivers/onedrive_README.md`, #408): a stale copy left by a failed source delete (`tiering.go:249-254` logs "duplicated (safe)"), or a hot copy inside the smart-demotion grace window if the cold read fails. | 404 storms fan out ×18; reads are not location-authoritative. | WP-R6-1: when the location is recorded (hint or `object_locations`), candidates = `[recorded]` (+ primary only for legacy unrecorded objects); fan-out only when nothing is recorded. Depends on R6-05 (hints everywhere). |
| R6-08 | P2 | `internal/engine/engine.go:215`, `:242`, `:280`, `:297`, `:344`, `:379`, `:468-474`, `:517`, `:580`, `:606-608`, `:651-655`, `:723`, `:732`, `:757-764`, `:781`; `tiering.go:81`, `:136`, `:217-221`; `dashboard/handlers/admin_backends.go:123` | `drivers` and `primary` are written under `mu` (`AddDriver`, `SetPrimary`) but read **without** it on every request path and in `buildCandidateList`'s map iteration. `SetPrimary` is callable at runtime from the admin dashboard (`POST /admin/backends/{name}/primary`). `TieringEngine` holds the map by reference (`:85`). | A concurrent `SetPrimary` is a data race on a string header (torn read) and a map iteration during a (future) runtime `AddDriver` panics. `-race` is clean today only because no test exercises the admin path. | WP-R6-2: read `primary`/`drivers` through the accessors (`GetPrimary`, `GetDriver`) or snapshot under `RLock` at the top of Get/Put/Delete/List; make `AddDriver` after `Start` an error. |
| R6-09 | P2 | `internal/engine/engine.go:44`, `:223`, `:289`, `:438`, `:802` | `objectBackends` grows by one entry per object ever PUT, GET or hinted in the process lifetime and is never trimmed (only `Delete` removes). | Unbounded memory on the hot path; at 10⁸ objects it is gigabytes. It is redundant: the API layer hints from `object_head_cache.backend_name` before every GET/CDN read. | WP-R6-1: bound it (small LRU) or drop it in favour of the hint + `LocationStore`. |
| R6-10 | P2 | `internal/engine/tiering.go:216-263`, `:135-145`, `:155-166`; `engine.go:813-817` | If `StartTiering` were ever called: `migrateObject` updates `object_locations` and the sync.Map but **not `object_head_cache.backend_name`** (the routing truth), so the next hinted GET goes to the source, which it just deleted (works today only through R6-07 fan-out); `dstDriver.Put` passes no `ContentLength` (iDrive/Geyser require it — `interface.go:140-142`); `object_locations` also holds `_global/_chunks/*` rows (every chunk PUT records one, `engine.go:445-447`), so the default 90-day policy would migrate **dedup chunks to tape** and every chunked object older than 90 days would 403 after Geyser's staging window; policies ignore encryption and versioning. | A one-line `eng.StartTiering(ctx)` in `main.go` would corrupt routing for aged objects. Smart demotion (5.15.8) is the correct successor. | WP-R6-4: delete `TieringEngine`, `StartTiering`, `tiering_policies` usage (keep the table for a later DROP migration), and the `Shutdown` hook. |
| R6-11 | P2 | `internal/intelligence/access_tracker.go:60`, `:101`, `:180`; `engine.go:228-235` | `GetRecommendation` (`QueryRow`), `GetHotData` (`Query`), `flushBatch` (`Begin`/`Exec`) take no `ctx`; the recommendation query ran synchronously on **every GET and PUT** (removed by R6-03's fix). Remaining: `flushBatch` executes one `Exec` per event inside one transaction (up to 1000 round trips per 5 s). | Violates the ctx rule; per-request DB latency for a Debug-level log line. | R6-03 fix removes the hot-path call. WP-R6-5 decides the package's future (see R6-16). |
| R6-12 | P2 | `internal/engine/engine.go:455-457`, `:650-672`, `:129-133` | `replicateToBackup` runs `go` per PUT with the **request** context (cancelled when the handler returns → the copy aborts), unbounded, reads `drivers`/`backup`/`primary` unlocked, re-reads the object from the primary (not from the backend it was actually written to, `usedBackend`), and surfaces failure only as a log line. `SetBackup` has no caller outside tests, so the path is dormant. | Dormant footgun; the moment someone sets a backup it is a silent-failure replication with a cancelled context. | WP-R6-4: delete `backup`/`SetBackup`/`replicateToBackup`; the dead `replicator.go` (D-9 "keep") is the same idea — replication belongs in a job with its own context, bounded workers and a ledger (the smart-demotion pattern). |
| R6-13 | P3 | `internal/cache/tiered_cache.go:11-30`, `:43-49`; `engine.go:92-102`, `:641-648`, `:685-713`; `cmd/vaultaire/main.go:176-179` | `TieredCache` is a `map[string][]byte` behind a `RWMutex`: `Config.MemorySize/SSDSize/SSDPath` are stored and never read, `Set` never evicts, `GetMetrics` reports item count only. `cachingReader` buffers up to 10 MiB per concurrent reader into a `bytes.Buffer` and copies it into the map at EOF (`Set` stores the buffer's backing array). With `EnableCaching: false` (prod) `e.cache` is nil, `wrapReaderForCaching` is never called and nothing allocates — confirmed. `main.go:179` still says "re-enable when `internal/cache/lru.go` is wired" — R0 deleted `lru.go`. | Dead weight on the hot path (`e.cache != nil` checks in Get/Put/Delete/GetMetrics/HealthCheck/Shutdown) and a comment steering at a deleted file. | **Decision (WP-R0-3): delete the code path.** See *Follow-up work packages* WP-R6-6 for the reasoning; `main.go` comment fixed here. |
| R6-14 | P3 | `internal/engine/routing.go`; `migrations/048_object_locations.sql:1-11`; `api/smart_demotion.go:390`; `api/smart_promotion.go:204`, `:309` | `object_locations` is a second source of truth beside `object_head_cache.backend_name`, and they drift: smart demotion/promotion flip the head row and hint the map but never touch `object_locations` (no writer outside `internal/engine`); the `bucket` column actually stores the **container** `<tenant>_<bucket>` (the engine passes `container`), so `tenant_id` is duplicated inside it; chunk PUTs add a row per chunk per tenant. `RecordLocation` is fire-and-forget after Put (`engine.go:445-447`) and lost if the process exits. Readers: `Get`/`GetRange` on a map miss, tiering (never), `overview.go:242`, `admin_costs.go:390`. | Two truths; the one the engine consults on a cold miss can be stale. | WP-R6-1: make `object_head_cache.backend_name` the only truth (the hint is already mandatory), drop `LocationStore` from the read path, keep `object_locations` only if the dashboard cost views need it (or derive them from the head cache). |
| R6-15 | P2 | `internal/engine/routing.go:57-62` | Every `LookupBackend` hit (map miss) spawns a goroutine that `UPDATE`s `last_accessed` with `context.Background()`. | One write per cold read, unbounded goroutines, outlives shutdown (fails on the closed pool — harmless but noisy). Only consumer of `last_accessed` is the never-started tiering scan. | Goes with WP-R6-1/WP-R6-4 (delete with tiering). |
| R6-16 | P3 | `internal/intelligence/access_tracker.go:37`, `:142-173`, `:235-242`; `anomaly.go:26-45`; `ml_pipeline.go:53-78`; `api/patterns.go:14-30` (routes live at `server.go:631`) | `processEvents` never exits (no ctx) and keeps a private batch of up to 1000 events that `Flush` cannot see — `Shutdown` drains only the channel, so up to 5 s of events are dropped and later `Begin` calls hit the closed pool. `AnalyzePatterns` returns **hardcoded** peak hours / directories / velocity (`:236-241`), served to customers via `GET /api/v1/patterns`. `IsAnomaly`/`Report`/`HeuristicModel` are constructed and never called. | Fake analytics on a public endpoint; shutdown loss (analytics only, not billing). | WP-R6-5: decide whether access tracking is a product (then it needs real analysis, ctx, and a stop channel) or is removed with its routes and table. |
| R6-17 | P3 | `internal/engine/engine.go:29-30`, `:79-80`, `:624-639`; `health.go`, `selector.go`, `cost_optimizer.go`; `cmd/vaultaire/main.go:186-209`; `api/health_handlers.go:41` | `BackendSelector`, `CostOptimizer` and `HealthScorer` are constructed; `UpdateScore`/`SelectBackend`/`SelectOptimal` are never called by product code (`deadcode` marks them reachable only through the constructors and `SetCostConfiguration`/`SetEgressCosts`, whose values nothing reads). `main.go` maintains a per-backend price table that influences nothing. The only live routing signal is the breaker. | Reviewers (and the CLAUDE.md table) assume health/cost-based selection exists. | WP-R6-4: delete the three types, the two setters and the price table (`SetCostConfiguration` is also the CLAUDE.md "cost optimizer" claim). |
| R6-18 | P3 | `internal/engine/engine.go:513-549`; `drivers/s3compat.go:119-143`; `drivers/geyser.go:313-333`; `drivers/s3.go:105-124` | `engine.List` delegates to the primary only, unlocked, returns all keys (no pagination/delimiter) — the API uses it only as the no-DB fallback (`s3_list.go:302-305`). Driver contract drift for R7: `S3CompatDriver.List` strips `len(prefix)` (the artifact prefix) instead of the `container/` key prefix (`:130-138`) — wrong names whenever `prefix != container`; `S3Driver.List` and `GeyserDriver.List` read a single `ListObjectsV2` page (1000 keys) while iDrive/Lyve/R2 paginate. | Listing correctness on the fallback path; drivers disagree on List semantics. | R7 verifies each driver against the contract table below; the engine's `List` should be documented as "fallback only". |
| R6-19 | P3 | `docs/DRIVERS.md:11-25`, `:100-115`, `:222-233`; `docs/ARCHITECTURE.md:22-60`, `:78-92`, `:120-128`; `internal/engine/CLAUDE.md` | `DRIVERS.md` shows a `Put` without options, a `List(ctx, container)` without prefix, a `GetMetrics()` method, `engine.ErrNotFound`/`ErrAccessDenied` sentinels and an `engine.RegisterDriver` in `internal/engine/registry.go` — none exist (`ErrNotFound` is a constructor returning `NotFoundError`; there is no registry). `ARCHITECTURE.md` describes a NYC hub with Kansas/Montreal/Mumbai spokes, a 7/30/90-day tiering policy and a five-driver list — none of it is the shipped system. `engine/CLAUDE.md` still says "Delete removes an artifact from all backends" (it stops at the first success, R6-24), lists `cost_optimizer`/`selector`/`health` as routing components (R6-17) and references the deleted `BackendMonitor`. | The two public docs are fiction; R14 merges the narrative below. | Narrative + contract table in this file → R14 rewrites `ARCHITECTURE.md` and `DRIVERS.md`; `engine/CLAUDE.md` updated in this PR for the fixed behaviour. |
| R6-20 | P3 | `internal/engine/engine.go:837-854` | `GetContainerMetadata`/`GetArtifactMetadata` return objects stamped `time.Now()` with empty metadata. No callers outside the interface. | Interface stubs that lie if ever used. | WP-R6-4: remove from the `Engine` interface with `Execute`/`Query`/`Train`/`Predict`. |
| R6-21 | **P1** (→ R13/R4) | `internal/api/access_log.go:164-165`, `:105`; `internal/api/s3_inventory.go:386-388`, `:332` | Access-log delivery and inventory reports call `eng.Put` with container **`tenant/<id>/<bucket>`** while the S3 path namespaces as **`<id>_<bucket>`** (`tenant/tenant.go:76`), and with a `context.Background()`-derived ctx carrying no tenant (`common.GetTenantID` → `"default"`, `context.go:19-24`; iDrive/Geyser/Lyve/R2 key by that tenant). No head-cache row is written either. | A customer who enables server access logging or inventory never sees a single delivered object: the bytes land under a container no read path ever addresses, billed to nobody, orphaned forever. | Not fixed here (R13 owns the runners): use `tenant.NamespaceContainer` via the same helper the S3 path uses, set `common.WithTenantID`, write the head-cache row through the normal PUT path (or call the adapter). Recorded as WP-R6-7 with a test that PUTs a report and GETs it through the S3 handler. |
| R6-22 | P2 | `internal/common/context.go:19-24`; every driver's `getTenantID` | `GetTenantID` silently returns `"default"` when the context carries no tenant. The 2026-07-31 CDN bug (`cdn.go:115-120`) and R6-21 are both this fallback. | Any new background caller that forgets the tenant stores or looks up under a shared `default` prefix — the engine cannot tell. | WP-R6-1: the engine should refuse Put/Get/Delete with no tenant in ctx unless the container is the shared chunk container (`_global`), or at least log at Warn with the container. R15 invariant. |
| R6-23 | P3 | `internal/engine/engine.go:306-318`; `api/s3_engine_adapter.go:473`, `:488` | `GetRange` fallback for drivers without `RangeGetter` (only `local`) discards `offset` bytes and returns the **unbounded remainder**; the only caller bounds it with `io.CopyN(w, rr, rng.length)`. `GetRange` also skips the cache and access logging (inconsistent with `Get`). | Contract relies on the caller; fine today. | Document on the method; wrap in `io.LimitReader` when R2 touches the range path. |
| R6-24 | P2 | `internal/engine/engine.go:462-484`; `failover.go:226-227` | `Delete` uses `Execute`, which returns on the **first success**, with candidates `[recorded, primary]`. An object present on both (smart-demotion grace copy, failed tiering source delete) keeps its second copy; the comment says "removes an artifact from all backends". Smart demotion covers its own case through the ledger (`object_gone`), nothing covers the generic one — and R6-07's fan-out would happily serve it. | Resurrection vector; wrong comment. | WP-R6-1: `Delete` must attempt every candidate and aggregate; fix the comment now. |
| R6-25 | P3 (→ R2/R3) | `internal/api/s3_batch.go:148-152`; `s3_copy.go:131-134`; `cdn.go:130-136` | Batch delete and CopyObject classify misses with their own lowercase `"not found"` / `"no such file"` substrings instead of `isObjectMissingErr` — the SDK's `NotFound`/`NoSuchKey` casing does not match, so a batch delete of a missing key on an S3-class backend is reported as a per-key error; the CDN handler maps **every** engine error, including `ErrAllBackendsUnavailable`, to 404. | Inconsistent 404/500/503 across handlers. | R2/R3: route all three through `isObjectMissingErr` + the `ErrAllBackendsUnavailable` check; CDN should answer 503 on unavailability. |
| R6-26 | P3 | `internal/engine/engine.go:579-596` | `CoreEngine.HealthCheck` iterates drivers unlocked and sequentially; no callers (R1). `/health` uses the API probes instead. | Dead, and a second health model (see *Invariants confirmed* Q7). | WP-R6-4: remove from the interface. |
| R6-27 | P3 | `internal/engine/engine.go:820-835` | `Shutdown` ignores `ctx`, stops a tiering loop that never started, flushes an intelligence channel whose in-flight batch it cannot reach (R6-16), and closes the DB; fire-and-forget goroutines from `RecordLocation`/`LookupBackend` may still run and log `sql: database is closed`. Nothing else in the engine needs the DB after `server.Shutdown` (R1-03 order confirmed). | Noise at shutdown, no data at risk (routing truth is the head cache). | Goes with WP-R6-1/4. |

## How an object gets placed and found

*(one page for `docs/ARCHITECTURE.md`; describes `main` after this session's PR)*

**Naming.** The S3 layer authenticates the request and fixes the tenant from the credential alone
(R5). Every engine call receives `container = "<tenantID>_<bucket>"` and `artifact = key`, plus the
tenant in `ctx` (`common.TenantIDKey`). Drivers that share one physical bucket across tenants
(iDrive, Geyser, R2, Lyve) additionally prefix keys with `t-<tenant>/`. Dedup chunks live in the
shared container `_global` under `_chunks/{hash}` (or `_chunks/{tenant}/{hash}` when encrypted, R8).

**Placement (PUT).** `resolvePutStorageClass` in the API layer is the single placement decision:
explicit `x-amz-storage-class` header → bucket `tier_preference` (`archive` → GLACIER → geyser,
`resilient` → RESILIENT → lyve, `performance`/`standard` → STANDARD) → `PUBLIC` when the bucket is
public-read and an `r2` driver exists → `""` for `auto` buckets. Region-pinned buckets bypass the
engine and write straight to `idrive-<region>` (`bucketRegionDriver`). The engine maps the class to a
driver name with `storageClassToBackend` (`storage_class.go:18-39`); an unregistered target or an
unknown class falls back to the primary (`STORAGE_MODE`, prod `idrive`). The class is a hint, never an
error. The engine never re-derives tenant or placement: the intelligence override that used to do so
was removed in R6 (R6-03).

**Write candidates.** `[target, primary, general-purpose durable backends]`. `local` (hub disk),
`r2` (public store), `geyser` (tape), `permafrost` (OneDrive fleet) and every `idrive-<region>` are
*target-only*: they receive a write only when they are the resolved target or the configured primary
(R6-04). `FailoverManager.Execute` walks the list in order, skipping backends whose circuit breaker is
open. A seekable body (`*bytes.Reader` from the chunk path) is rewound before each retry; a
non-seekable body that has been partially consumed stops the walk with `ErrNoFailover` so no backend
receives a truncated object (#402). If every eligible backend fails with a genuine backend failure,
Put returns `ErrAllBackendsUnavailable` (→ 503 + `Retry-After`), increments `write_failures`
(Prometheus `vaultaire_backend_write_failures_total`) and logs at Error — data is never silently
stranded (#347). Client-level errors keep their identity (403/400).

**Recording the location.** Put returns the backend name; the API layer persists it in
`object_head_cache.backend_name` in the same transaction as size/ETag/content-type. **That column is
the routing truth.** The engine also caches it in `objectBackends` (in-memory) and writes an
`object_locations` row asynchronously (048) — both are caches of the head-cache value and may lag or
drift (smart demotion updates the head row only).

**Reading (GET / range / CDN / chunk fetch).** The API layer reads the head row (HEAD never touches a
backend — the one exception is the live `x-amz-restore` passthrough for GLACIER-class objects) and
calls `HintBackend(container, key, backend_name)` before `engine.Get`. The engine resolves the
preferred backend as hint → `objectBackends` → `object_locations` → primary, then walks
`[preferred, primary, every other backend]` with the same breaker-aware `Execute`. Today a miss on the
preferred backend still fans out to every registered backend (R6-07, WP-R6-1 narrows this to the
recorded location). Range reads use `RangeGetter` when the driver has it. Archived objects on Geyser
return `ErrArchived`, which stops the walk (another backend's 404 must not mask it) and maps to 403
`InvalidObjectState`, or to 503 + auto-restore for smart-demoted objects.

**Not found vs. unavailable.** `isBackendFailure` (engine) and `isObjectMissingErr` (API) are the two
halves of one taxonomy: typed `NotFoundError`, `os.ErrNotExist`, SDK HTTP 404 / `NoSuchKey` /
`NotFound` are *misses* — they never charge a breaker and map to 404 `NoSuchKey`. Timeouts, 5xx,
connection errors, `NoSuchBucket` and 403 (dead key) are *backend failures* — they charge the breaker
(5 in 60 s → open 30 s → half-open probe). `context.Canceled` (client gone) is neither. If the
recorded/preferred backend was unavailable, a fallback's not-found is not a verdict: the read fails
with `ErrAllBackendsUnavailable` → 503 (R6-02).

**Delete.** The API layer hints the recorded backend (R6-05) and the engine deletes there, falling
back to the primary; the head row is removed (`DELETE … RETURNING size_bytes` releases quota) even
when the backend already reports the object missing. Chunked objects decrement GCI refs instead; the
sweep deletes blobs later (R8).

**Health.** Two models: the API probes (`backend_probes.go`, signed HeadBucket / Lyve console /
TCP) feed `/health`, `/metrics` and alerting only; the engine's per-backend circuit breaker, fed by
live request outcomes, is the only thing that alters routing. Neither consults `HealthScorer` /
`BackendSelector` / `CostOptimizer`, which are dead (R6-17).

## Driver contract as the engine assumes it

R7 verifies every driver against each row. "Engine assumes" = what `engine.go` / `failover.go` /
the API classification rely on today.

| Method | On missing object | List semantics | Exists cost | HealthCheck kind | Streaming | ctx |
|--------|-------------------|----------------|-------------|------------------|-----------|-----|
| `Name()` | — | — | — | — | — | — |
| `Get(ctx, container, artifact) (io.ReadCloser, error)` | Must be classifiable as a miss by `isBackendFailure`/`isObjectMissingErr`: `engine.NotFoundError`, an error wrapping `os.ErrNotExist`, or an aws-sdk-go-v2 error chain carrying HTTP 404 / `NoSuchKey` / `NotFound` (typed, after R6-01). **Anything else is a backend failure** and charges the breaker. `ErrArchived` for tape-evicted objects (Geyser). Observed: local → `*fs.PathError` ✓; idrive/lyve/r2/geyser/s3compat → wrapped SDK error ✓ (GET misses carry `NoSuchKey` when the vendor sends a body; empty-body 404s were misclassified before R6-01); onedrive → wrapped Graph error containing `404` (string only — R7 to type it); quotaless → retries 3× with sleeps **before** returning any error, including a miss (`quotaless.go:134-153`). | — | — | — | Must return the response body unread; the engine never buffers (`cachingReader` only when caching is enabled, which it is not). | Must pass `ctx` to the SDK call; `context.Canceled` must propagate unwrapped-by-`%w` so `errors.Is` sees it. |
| `GetRange(ctx, …, offset, length)` (optional `RangeGetter`) | As `Get`. | — | — | — | Body positioned at `offset`, at most `length` bytes (S3 `Range` header). Drivers without it get full `Get` + discard (only `local`). | as `Get` |
| `Put(ctx, container, artifact, r io.Reader, opts…) error` | n/a. Returns **no ETag and no size** — the API layer computes the MD5 ETag on the stream (rule 3) and knows the size. | — | — | — | Must stream `r` once. May read `PutOptions.ContentLength` (iDrive/Geyser need it, else they buffer or fail — `interface.go:140-142`). Must **not** read past a failure (the engine's body-safety logic counts consumed bytes). Must ignore `StorageClass` upstream (no driver sets a vendor class — `storage_class.go:3-7`). | as `Get`; a cancelled ctx mid-body is a client abort, not a backend failure |
| `Delete(ctx, container, artifact) error` | Idempotent preferred (AWS: 204 on missing). If the vendor returns 404 it must be classifiable as a miss (see `Get`), since the API treats a miss as success and still removes the head row. | — | — | — | — | as `Get` |
| `List(ctx, container, prefix) ([]string, error)` | Empty slice (not error) for an empty/missing container. | Keys **relative to the container** (the `t-<tenant>/<container>/` prefix stripped), filtered by `prefix`, **all pages** (the engine does no pagination and the API sorts client-side). Ordering: unspecified (API sorts). Observed: idrive/lyve/r2 paginate ✓; s3compat strips the wrong prefix ✗; s3/geyser read one page ✗ (R6-18). Used only on the no-DB fallback path. | — | — | — | as `Get` |
| `Exists(ctx, container, artifact) (bool, error)` | `(false, nil)`. | — | **Not called by the engine or the API** (only by `cmd/backend-matrix`, `cmd/erasure-bench`). Cost is one `HeadObject`. All S3-class drivers detect the miss with `strings.Contains(err, "404")` — a request id containing `404` is a false miss (R7). | — | — | — |
| `HealthCheck(ctx) error` | — | — | — | Must be **authenticated** (rule 1): signed `HeadBucket` (idrive, geyser, r2), `ListObjectsV2 MaxKeys=1` (s3compat — signed, but a list), Lyve uses the console action at the API layer. Called only through `CheckDriver` from the API probes and the admin dashboard; the engine's own `HealthCheck` has no callers. A failing probe **does not** affect routing (only the breaker does). | — | 15 s budget imposed by the probe (`backend_probes.go`); drivers should not add unbounded retries. |

Additional assumptions the engine makes of every driver: (a) it keys by the tenant found in `ctx`
(`common.GetTenantID`) **or** by the container name, never by anything the engine derives; (b) errors
are wrapped with `%w` so the typed checks above see the SDK types; (c) it is safe for concurrent use;
(d) it does not retry with sleeps inside the request (the breaker + client retry is the retry
policy — quotaless and onedrive violate this); (e) `Put` failure after partial body consumption is
reported as an error (never a truncated success).

## Invariants confirmed

**Q1 Driver contract** — table above; `docs/DRIVERS.md` contradicts it on four points (R6-19).

**Q2 Placement / lookup** — narrative above. A read **can** be served from a backend other than the
recorded one today: on any error from the preferred backend the walk continues through every
registered driver (R6-07); the fix for the 404-masking half is in this PR (R6-02), the fan-out itself
is WP-R6-1. Locations: written by `Put` (async) and tiering (never); read by `Get`/`GetRange` on a
map miss and two dashboard pages; **not** updated by smart demotion (R6-14). A read that fell through
every candidate was **not** distinguishable from a miss before this PR; after it, an unavailable
preferred backend yields `ErrAllBackendsUnavailable` and only an authoritative miss yields 404.

**Q3 Failover / body safety** — proven with the actual reader types: the chunk path hands
`*bytes.Reader` (seekable → rewound to `bodyStart`, `engine.go:367-394`); the plain PUT path hands the
API's hashing/counting reader (non-seekable → after any consumed byte the attempt returns
`ErrNoFailover` and `Execute` breaks, `failover.go:213`); `TestEnginePut_NonSeekableConsumedBodyFailsInsteadOfTruncating`
and `…DoesNotChargeHealthyBreakers` cover both. A retry on a consumed body is impossible: the guard at
`:383-389` is a second, independent check. Breaker: 5 failures inside a 60 s sliding window → open;
after 30 s `Allow` flips to half-open and admits one request; success → closed and failures cleared;
failure → the window re-evaluates (in practice re-opens). "Success" = `fn` returned nil. Client error
when all candidates fail: `all backends failed: <last error>` before this PR (→ 404/500 depending on
the last backend, R6-02); after: `ErrAllBackendsUnavailable` (503) when the preferred backend was
unavailable, the authoritative miss (404) otherwise, `ErrArchived` (403) unchanged.

**Q4 Put write path** — primary write via `Execute`; `sizeTrackingReader` counts bytes only (no
buffering); `cachingReader` is only constructed when `e.cache != nil`, i.e. never in prod — with
caching off `wrapReaderForCaching` is not called and nothing allocates. `replicateToBackup` is
dormant (no `SetBackup` caller) and wrong when awakened (R6-12). `writeFailures` increments exactly
when Put returns `ErrAllBackendsUnavailable` (#347) — confirmed by `TestEnginePut_BackendFailureFailsLoudly`.

**Q5 Caching** — decision: delete (R6-13, WP-R6-6). `main.go:176-179` comment corrected in this PR.

**Q6 Tiering / storage classes** — map at `storage_class.go:18-39`: STANDARD→idrive, GLACIER and
DEEP_ARCHIVE→geyser, REDUCED_REDUNDANCY→local, RESILIENT→lyve, PUBLIC→r2; STANDARD_IA deliberately
absent (falls back to primary at STANDARD); `permafrost` and `idrive-<region>` have **no** class —
they are reached only by `bucketRegionDriver` (regions) or explicit `AddDriver` name (permafrost is
never targeted by anything today). Reverse map for HEAD/list: idrive/lyve/permafrost/s3/r2→STANDARD,
geyser→GLACIER, local→REDUCED_REDUNDANCY. `StartTiering` would run the 90-day default policy against
`object_locations` including chunk rows (R6-10). **Engine-level guard against private objects on
r2:** none existed — `r2` was an ordinary failover candidate (R6-04); after this PR `r2` is
target-only, so the API-layer `resolvePutStorageClass`/`PUBLIC` is the only way onto it.

**Q7 Health** — driver health does **not** feed candidate selection; only the breaker does. An
unhealthy primary on writes: requests still go to it until five fail, then the breaker skips it and
writes silently continue on the next eligible candidate (target-only rule now limits which); "fail
loudly" happens only when every eligible candidate fails. API probes (`backend_probes.go`) are a
separate observability model with no routing effect — two models, by design; documented in the
narrative.

**Q8 Shutdown** — `Shutdown` stops tiering (never started), drains the intelligence channel (not the
in-flight batch, R6-16), flushes the (nil) cache, closes the DB. R1-03 order (server first) holds;
nothing in the engine needs the DB afterwards except fire-and-forget goroutines that log and exit.

**Q9 Dead / unused** — `GetHotData`, `GetAccessPatterns`, `GetRecommendations` are reachable via
`api/patterns.go` (routes registered at `server.go:631`, JWT-gated); `Execute`/`Query`/`Train`/`Predict`
return "not implemented" and have no callers; `HealthCheck`, `StartTiering`, `SetBackup` have no
product callers; `SetCostConfiguration`/`SetEgressCosts` are called from `main.go` and feed a dead
optimizer. `internal/intelligence` is on the request path only through `LogAccess` (non-blocking
channel) — the synchronous `GetRecommendation` calls are removed in this PR.

**Q10 Concurrency** — `go test -race` clean on the existing suite. `drivers`/`primary` are read
unlocked on every request path (R6-08); `AddDriver`/`SetPrimary`/`GetDriver`/`GetDriverNames`/
`CheckDriver` take `mu`, so probes iterating `GetDriverNames()` are safe against each other, but a
runtime `SetPrimary` from the admin dashboard races every in-flight request. Goroutines started per
request: `RecordLocation` (one per PUT, `context.Background()`), `LookupBackend` touch (one per cold
GET), `replicateToBackup` (dormant).

**Tenant isolation (R5 §Tenant isolation invariants)** — the engine derives nothing: tenant comes
from `ctx` (`common.GetTenantID`) and is used only as a cache/location key and passed to drivers;
containers arrive already namespaced. Hazard: the `"default"` fallback (R6-22) and the two background
writers that bypass the namespace (R6-21).

**Areas with no test:** cache (`internal/cache` has no test files), intelligence (none), common
(none); engine `Delete` routing (before this PR); `GetRange`; `List`; `Shutdown`; tiering
`migrateObject` against head-cache routing; runtime `SetPrimary` race; `LocationStore` (only sqlmock
tests exist — `routing_test.go`).

## Dead code noted

R0 handoff row for R6 confirmed (`engine/errors.go` 3/5, `engine/interface.go` 2/5):
`ErrNotFound`, `ErrPermissionDenied`, `WrapError` (`errors.go:14`, `:27`, `:31`) and
`WithContentType`, `WithUserMetadata` (`interface.go:127`, `:134`) are unreachable.

New (all `deadcode ./cmd/vaultaire`, 2026-09-26):

- Class-B files still 100 % dead and awaiting D-1/D-2/D-9: `analytics.go` (6 funcs), `capacity.go`
  (7), `disaster_recovery.go` (9), `load_balancer.go` (16, kept for `internal/ha`), `replicator.go`
  (7), `sla.go` (6). R6 recommendation for D-9: **delete `replicator.go`** as well — the P3
  "async parity replication" it is named for needs a ledger-backed job, not a 5-goroutine `[]byte`
  queue that `io.ReadAll`s every object (`replicator.go:59`).
- Live-but-inert (reachable only via constructors/setters, never consulted): `selector.go`,
  `cost_optimizer.go`, `health.go` (`HealthScorer` is also constructed at `api/health_handlers.go:41`
  and never scored), `engine.SetCostConfiguration`/`SetEgressCosts`, `replicateToBackup`/`SetBackup`,
  `TieringEngine` + `StartTiering`, `CoreEngine.HealthCheck`, `GetContainerMetadata`,
  `GetArtifactMetadata`, `Execute`/`Query`/`Train`/`Predict`, `LocationStore.CountByBackend`/
  `TouchLastAccessed` (no callers), `intelligence.AnomalyDetector`, `MLPipeline`/`HeuristicModel`
  (constructed, never invoked). `applyBackendRecommendation` removed in this PR.
- `Config.CacheSize`/`EnableML` are read only to size a cache that is off / never read at all.

## Follow-up work packages

| WP | Title | Files | Size | Depends on |
|----|-------|-------|------|-----------|
| WP-R6-1 | **Location-authoritative reads and deletes**: candidates = recorded backend (+ primary for unrecorded legacy objects); no fan-out when a location is known; `Delete` attempts every candidate and aggregates (R6-24); drop `object_locations` from the read path and `LookupBackend`'s touch goroutine (R6-14/15); bound or remove `objectBackends` (R6-09); refuse/warn on missing tenant ctx (R6-22). Requires every caller to hint (R6-05 done for single delete; batch/copy/stale-blob sites in R2/R3). | `engine.go`, `routing.go`, `api/s3_batch.go`, `api/s3_copy.go` | M | this PR; R2/R3 for the other delete sites |
| WP-R6-2 | **Engine concurrency**: snapshot `primary`/`drivers` under `RLock` at the top of each operation or route through the accessors; `AddDriver` after start = error; race test with a concurrent `SetPrimary`. | `engine.go`, `tiering.go` (or its deletion), `engine_test.go` | S | — |
| WP-R6-3 | **Per-operation driver deadlines** so a stalled backend surfaces as `context.DeadlineExceeded` (a breaker failure) rather than as a client `Canceled` after HAProxy's 50 s cut; SDK retry/timeout settings per driver; body-error sentinel from the API layer so client-body failures on PUT are never charged. | `drivers/*.go`, `api/s3_engine_adapter.go` (body wrapper), `engine/failover.go` | S–M | R7 |
| WP-R6-4 | **Delete the inert engine**: `TieringEngine`/`StartTiering` (R6-10), `backup`/`SetBackup`/`replicateToBackup` (R6-12), `selector.go`/`cost_optimizer.go`/`health.go` + `SetCostConfiguration`/`SetEgressCosts` + the `main.go` price table (R6-17), `HealthCheck`/metadata/`Execute`/`Query`/`Train`/`Predict` from the `Engine` interface (R6-20/26), dead `errors.go`/`interface.go` helpers; drop `tiering_policies`/`tenant_cost_daily` in a later migration; trim `Config` to `DefaultBackend`. | `internal/engine/*`, `cmd/vaultaire/main.go`, `api/health_handlers.go:41`, `engine/CLAUDE.md` | M | D-1/D-2/D-9 for the class-B files |
| WP-R6-5 | **Access-tracking decision**: either make `internal/intelligence` a real product (ctx on every query, stop channel + flush of the in-flight batch, real `AnalyzePatterns`, batched insert) or remove it with `api/patterns.go` routes, `access_patterns`/`access_anomalies` and `Config.EnableML` (R6-11/16). Recommendation: remove — nothing routes on it any more, the analytics it serves are hardcoded. | `internal/intelligence/*`, `engine.go`, `api/patterns.go`, `server.go:631`, migration | S | R11 (public API surface), R14 (docs) |
| WP-R6-6 | **Read cache = delete** (WP-R0-3 decision): remove `internal/cache`, `cachingReader`, `wrapReaderForCaching`, `maxCacheableObjectSize`, `Config.CacheSize`/`EnableCaching`, the `e.cache` branches and the L1 branch in `Get`. Reasoning: the engine's objects are per-tenant blobs read through a single hub — a RAM cache has a poor hit ratio and competes with chunk buffers for the same RSS (#400 memory cap is already deferred); `object_head_cache` is the metadata cache that matters; public/hot reads go through Cloudflare/R2; if a bytes cache is ever wanted it must be bytes-capped, evicting, per-object-capped and **outside** the request goroutine (write-behind), which is a new design, not this code. Comment at `main.go:176-179` corrected in this PR. | `internal/cache/`, `engine.go`, `cmd/vaultaire/main.go` | S | — |
| WP-R6-7 | **Background writers must use the S3 namespace and tenant ctx** (R6-21): access-log delivery and inventory reports go through the same container naming and head-cache write as a customer PUT; E2E test = enable logging → deliver → `GET` the report through the S3 handler. | `api/access_log.go`, `api/s3_inventory.go` | S | R13 |
| WP-R6-8 | **Driver contract conformance** (for R7): per-driver table against *Driver contract as the engine assumes it*; fix `S3CompatDriver.List` prefix strip, single-page `List` in s3/geyser, `Exists`' `"404"` substring, onedrive typed not-found, quotaless in-request retries with sleeps; decide Lyve WORM vs failover writes. | `internal/drivers/*` | M | this PR (contract table) |
| WP-R6-9 | **Docs**: rewrite `docs/ARCHITECTURE.md` from the narrative above and `docs/DRIVERS.md` from the contract table; remove hub/spoke, 7/30/90 tiering and the fictional registry (R6-19). | `docs/ARCHITECTURE.md`, `docs/DRIVERS.md` | S | R14 |
