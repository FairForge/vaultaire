# WP-R7-5 — Routing truth: the boot check, the sampled job, and the plan for prod's rows

Build note, 2026-10-03. Branch `wp/R7-5-routing-truth`. Closes R7-10, R7-11, R7-27 and the
prod backfill of R3-03; stands in front of WP-R6-1 (location-authoritative reads, no fan-out).
Decisions at the end; the [YOU] section is the plan Isaac runs from the box.

## The bug, seen first

`object_head_cache.backend_name` is the routing truth. GET and DELETE hint the engine with it
(`HintBackend`), HEAD-restore picks the restorer by it, the demotion ledger deletes "the copy
on the backend the key does not route to" by it, the deletion runner deletes each object on
it, the erasure sweep reports what it could not reach by it. Nothing checked it.

Prod, read-only, 2026-10-03 00:50 UTC (`SET default_transaction_read_only = on`):

| `backend_name` | rows | bytes | tenants | what the bytes are doing |
|---|---|---|---|---|
| `onedrive` | 2,613 | 0.92 GB | Bench2 2,612, BenchTest 1 | a registration key no driver has answered to since the fleet was registered as `permafrost` (R7-10); written by dedup-migrate in May 2026. **The fleet still holds about two thirds**: 20 of 30 sampled through the permafrost driver are present, 10 are gone |
| `idrive` | 2,037 | 2.16 GB | Bench2 1,230, FairForge Demo 667, BenchTest 140 | 2,007 are older than the reseller-account change of 2026-09-21; the bucket holds 30 objects, all the demo tenant's `letshow` archive of 09-23/24. **And the server's own key cannot read any of them — see "Found on the way"** |
| `local` | 451 | 0.59 GB | Bench2 447, Bench 2, Demo 2 | `DATA_PATH` holds ten files; **449 rows have no file** (the two present ones are the demo's `letshow/place.json` and `place.png`) |
| NULL | 62 | 13.3 GB | BenchTest 50, Bench2 12 | multipart completes before R3-03; served by the engine's fan-out only |
| `r2` 29, `permafrost` 25, `lyve` 6, `geyser` 2 | | | | `permafrost`: 25 of 25 present. Lyve/Geyser/R2 not probed (the tool had no credentials in its env for them — the server does) |

`object_versions`: 197,583 `idrive`, 1,649 `local`, 3 `onedrive`, 1 NULL. `smart_demotions`: empty.
`object_locations` (not routing truth, WP-R6-1 drops it): 1,362 idrive, 392 lyve, 267 permafrost.
HEAD answered 200 for every row above, and the 451 `local` rows reported `REDUCED_REDUNDANCY`
because the class came from the backend name.

Reproduced locally before any code: a PUT on a `local` primary, the file removed, HEAD 200 /
GET 404, and nothing anywhere counts it.

## What was built

### 1. The boot check (`internal/api/routing_truth.go` `BootCheck`, a goroutine in `Server.Start`)

Every distinct non-NULL `backend_name` in `object_head_cache`, the live rows of
`smart_demotions` (both columns) and the non-marker rows of `object_versions` must be a
registered driver name (`engine.GetDriverNames`). One Error line per (table, name) with the row
count, NULL as `<NULL>`. And **two registered names must never share a store** — the invariant
WP-R13-2 left as a comment: `engine.SharedStores()` groups the registry by driver value and by
`StoreID()` (`engine.StoreIdentifier`, implemented by every driver in
`internal/drivers/store_identity.go`: endpoint + bucket for the S3-class drivers, the directory
for local, the sorted account ids for the fleet). Two iDrive drivers on the same bucket name in
two regions are two stores (the endpoints differ); one driver value under two names, or two
drivers on one endpoint + bucket, is one. The check reads three tables and the registry, logs,
and sets nothing a request depends on: it never blocks or fails boot (a database that cannot be
read is one Error line).

### 2. The `routing_truth` job (daily 05:30 UTC on the one scheduler; `jobSpec`, boot catch-up +6 m, 1 h ceiling)

Per registered backend: count its whole-object rows, take `ROUTING_TRUTH_SAMPLE` (300) from a
random window (`ORDER BY tenant_id, bucket, object_key OFFSET random LIMIT n`), ask **the recorded
backend only** — the driver itself, with the row's tenant in the context — whether the bytes are
there. Never the engine's fan-out (`engine.Get`'s candidate walk would find the bytes wherever
they are and hide the drift). Outcomes per row:

| result | when |
|---|---|
| `present` | the backend has the bytes |
| `missing` | the backend says no AND the row, re-read, is unchanged (same backend, same etag) |
| `changed` | the backend said no and the row is gone or different: an erasure, a delete, an overwrite or a demotion landed mid-run — not drift |
| `error` | the backend could not be asked: a driver error (403, `NoSuchBucket`, a timeout) — never a miss |
| `unknown_backend` | the row names a backend no driver has: counted, nobody is asked |

An **open breaker skips the backend with ONE error** and no HEAD (`Skipped` says so). Ten
consecutive errors give the backend up for the run. The job **observes** the breaker but never
**charges** it: through `engine.ExistsOn` five failing HEADs would open the primary's breaker and
every customer PUT in the next 30 s would fail over to the next durable backend — a diagnostic
must not move customer bytes (and prod's primary fails every HEAD today, below). `permafrost`
(by name or `*drivers.OneDriveDriver`) gets `ROUTING_TRUTH_PERMAFROST_SAMPLE` (20): one `Exists`
there probes every account of the fleet.

Chunked rows: `ROUTING_TRUTH_CHUNKED_SAMPLE` (20) chunked objects, each chunk checked through the
chunk store at the one address (`chunkStore.exists`, WP-R8-7), then at the legacy address
(`legacyCopyExists`) — a legacy hit is `present` AND `legacy` (the chunk move's work); a chunk
whose index row names an unregistered backend is `unknown_backend`; 2,000 chunk HEADs per run at
most, the same error cutoff.

The job never writes. A run fails only when the head table could not be read; everything else is
a note. The result (`RoutingTruthResult`) is `jobReport.Result` → **`job_runs.result` JSONB,
migration 074** (a job's structured last result; a failed run keeps the previous one). The
one-line `Summary` is the job's note:
`local: 2 of 2 rows sampled, 1 present, 1 missing; rows on no registered backend: (NULL) 5, onedrive 2`.

### 3. Metrics and rules

| series | from |
|---|---|
| `vaultaire_routing_truth_checks_total{backend,result}` | the job, every result at 0 per registered backend from `NewServer` |
| `vaultaire_routing_truth_chunk_checks_total{backend,result}` (+ `legacy`) | the job |
| `vaultaire_routing_unknown_backend_rows{table,backend}` (`""` = NULL) | **the tables**, on scrape (5-min cache; a failed read keeps the last values) |
| `vaultaire_routing_truth_last_run_missing_ratio{backend}`, `_last_run_timestamp_seconds` | **`job_runs.result`**, on scrape — right after a restart too (the R13 lesson) |
| `vaultaire_routing_shared_store_backends` | the registry |
| `vaultaire_routing_unknown_backend_reads_total{op,backend}` | `noteRecordedBackend`, called by every reader of `backend_name` before it hints the engine: GET, DELETE, `DeleteObjects`, the CopyObject source, RestoreObject, `/cdn` |

`deploy/monitoring/vaultaire-routing.yml`: `RoutingTruthMissing` (warning, ratio > 0),
`RoutingTruthMissingJump` (critical, the ratio a tenth higher than yesterday's run — `offset 25h`,
so the known backlog is a standing warning, not a daily page), `RoutingUnknownBackendRows` (info,
`for: 1h`), `RoutingSharedStore` (critical), `RoutingUnknownBackendReads` (warning).
`routing_truth` is in `vaultaire-jobs.yml`'s `JobStale`.

### 4. Admin and dashboard

- `GET /api/v1/admin/routing-truth` — the boot report, the unknown-backend rows live from the
  tables, the shared stores, the last run (outcome, times, note, the JSON result).
- `POST /api/v1/admin/routing-truth/resolve-null?dry_run=true&limit=50` — the ONE justified
  fan-out: for each NULL row every registered driver is asked once (tenant in context); the row is
  written only when exactly one holds the bytes, under `backend_name IS NULL AND etag unchanged`;
  `nowhere` (nobody has it), `ambiguous` (more than one does) and `error` rows are reported and
  never touched; audit `admin.routing_truth_resolve_null`. Synchronous, bounded by `limit`.
- `POST /api/v1/admin/jobs/routing_truth/run` (the existing trigger). `GET /api/v1/admin/jobs`
  now carries `result` for the jobs that write one.
- The dashboard System page: a "Routing Truth" card with the last run's summary and the rows on
  no registered backend, read from `job_runs` (`handlers.loadRoutingTruth`).
- OpenAPI: both operations and the `RoutingTruth`, `UnknownBackendRows`, `SharedStore`,
  `RoutingTruthResolveNull` schemas (`TestOpenAPIDriftGuard` passes).

### 5. `local` is no tier (decision below)

`engine/storage_class.go`: `REDUCED_REDUNDANCY` is unmapped (it degrades to the primary at
STANDARD like any unsold class; `clientStorageClass` had already dropped the header, R2-07), and
`local` reports `STANDARD` — on a box where local IS the primary that is the class the object has.
`docs/API.md` says so.

### 6. The plan is a tool, not a job: `cmd/tools/routing-truth`

Read-only (`SET default_transaction_read_only`, one connection). Prints the three tables' names,
head rows per recorded backend × tenant with bytes and dates, and — with the backend's credentials
in the env — a sample of each class asked of the real driver (signed HEAD on iDrive, stat on
`DATA_PATH`, Graph on the fleet; `onedrive` rows are asked of the permafrost driver). `-backend`,
`-tenant`, `-sample`; `-one backend:tenant/bucket/key` HEADs then GETs one object;
`-list backend:tenant/bucket` lists what the backend holds. Then the plan per class, with the
decision stated. It writes nothing anywhere.

## The same bug on the other entry points — the audit

What each reader of `backend_name` does with a name no driver has, before and after:

| reader | before | now |
|---|---|---|
| GET (`HandleGet`) | hints the engine; `buildCandidateList` drops the unknown name; the read falls through primary → every backend (silent fan-out) → 404 if nowhere | the same, plus `vaultaire_routing_unknown_backend_reads_total{op="get"}` and one Warn per (op, backend). The fan-out stays until WP-R6-1 — removing it with these rows in place makes 2,613 objects unreadable, which is why this WP is first |
| HEAD | 200 from the cache | **unchanged, by decision** (below) |
| HEAD / GET `x-amz-restore`, RestoreObject (`objectRestorer`) | `GetDriver` fails → "no restore concept" → 403 `InvalidObjectState` on a vault-floor object | the same, counted (`op="restore"`) |
| DELETE, `DeleteObjects` | hint; `engine.Delete` tries [unknown, primary] → the primary misses → the row is deleted and quota released; bytes elsewhere stay | the same, counted (`op="delete"`, `"delete_objects"`). This is the right cleanup path for a row whose bytes are gone — and the erasure sweep picks up bytes a registered backend still holds |
| CopyObject source | hint + `engine.Get` fan-out | the same, counted (`op="copy_source"`) |
| `/cdn` | the same as GET | counted (`op="cdn"`) |
| demotion ledger (`smart_ledger.go`) | `deleteCopyOn`: "backend not registered" → Warn, nothing deleted; `dropDisplacedBlob` returns early | unchanged — correct |
| deletion runner | `deleteOnBackend`: hint + `engine.Delete` → miss on the primary is fine → row deleted | unchanged; the sweep then lists what registered backends still hold for the tenant and names the unregistered ones in `unswept_backends` |
| erasure sweep | an unregistered backend cannot be swept; recorded | unchanged |
| inventory, listings, dashboard browser | the class from the floor (WP-R13-1) | unchanged; `local` now reports STANDARD |

The boot check names the rows; the job measures them; the read counter shows whether anyone is
reading them. Nothing changed what a request does.

## Adversarial pass — on my own code, mutations seen red

Each mutation applied, the named test run, red confirmed, the file restored (M1–M9 in the session):

| # | mutation | test that went red |
|---|---|---|
| M1 | a miss is not re-read (the row deleted mid-run counts as missing) | `…PresentMissingAndChangedAreToldApart` |
| M2 | a driver error (`NoSuchBucket`) counts as missing | `…ABackendErrorIsNeverAMiss` |
| M3 | the open breaker is not checked | `…AnOpenBreakerSkipsTheBackend` (0 calls asserted) |
| M4 | rows on an unregistered name sampled as if registered | `…RowsOnAnUnregisteredBackendAreUnknownNotMissing` |
| M5 | a chunk found at a legacy address counted missing | `…ChunkedRowsAreCheckedChunkByChunkAtTheOneAddress` |
| M6 | the collector reads nothing from `job_runs` (an in-process value) | `…WritesTheResultAndTheCollectorReadsItAfterARestart` |
| M7 | resolve-null writes an ambiguous row | `…ResolveNullAsksEveryDriverOnceAndWritesOnlyOnAYes` |
| M8 | `SharedStores` ignores `StoreID` (identity only) | `TestSharedStores_SameDriverRegisteredTwice`, `…TwoDriversOnTheSameEndpointAndBucket` |
| M9 | the whole-object check goes through `engine.ExistsOn` (charges the breaker) | `…ABackendErrorIsNeverAMiss` ("the job never charges a breaker") |

What the brief asked me to try, and what happened:

- **A backend that answers 404 for a misconfigured bucket**: `NoSuchBucket` is a `smithy.APIError`
  the drivers' `Exists` return as an error (`s3IsNotFound` decides it before the status check) →
  `error`, never `missing` (M2).
- **A row whose backend is registered under a region name** (`idrive-eu-west-1`): registered names
  are the registry's, not a family — checked there, `present`
  (`…ARegionNamedBackendIsJustARegisteredName`).
- **A chunked row whose chunks are at a legacy address**: `present` + `legacy`, with the note
  "run the chunk move" (M5).
- **A tenant erased mid-run**: the miss is re-read; the row is gone → `changed` (M1). The same for
  a demotion (backend changed) or an overwrite (etag changed).
- **The gauge right after a restart**: `vaultaire_routing_truth_last_run_missing_ratio` and the
  unknown-rows gauge are read from the tables on scrape; proven live below (M6 pins it).
- **Found by the pass, not asked**: the first version asked through `engine.ExistsOn`, which
  charges the breaker on a backend failure; on prod every HEAD at the primary fails (below), so
  the job's first five calls would have opened the primary's breaker for 30 s once a day and
  moved every PUT in that window to Lyve. Now the driver is called directly, the breaker is
  observed, and ten consecutive errors end the backend's sample (M9,
  `…ABackendThatKeepsFailingIsGivenUpForTheRun`). The chunk sample has the same cutoff (it still
  goes through the chunk store's `ExistsOn`, bounded to ten failures).
- **The first prod run would page**: with `> 0.1` absolute, `local` (449 of 451 missing) and
  `idrive` would have paged on the first deploy for rows this note already lists. The jump rule
  is a delta against the run 25 h before; the backlog is the standing warning.
- **Sampling**: `OFFSET random` is a scan of the backend's rows up to the offset; at prod's row
  counts (thousands) it is nothing; at millions the job would want a keyset walk from a random
  key. Noted, not built.
- **The dashboard's job row vs. a live server on the same database**: the System page test used
  to skip when a `routing_truth` row existed; it now sets the row aside and restores it.

## Tests

`internal/api/routing_truth_test.go` (own tenant, own backend names, `scopeTenant`; `rtDriver`
fakes a backend's `Exists`): boot check names every table's unknown names and NULL; two names on
one store; present / missing / changed; a backend error is never a miss and never charges the
breaker; an open breaker skips with one error and no call; unknown-backend rows are never
sampled; the sample is bounded and smaller on permafrost; a region-named backend; the job never
writes; the job row carries the JSON result and a fresh checker reads the ratio from it; the
series at 0 from boot and the gauges from the table; the admin view; resolve-null dry run and
write (resolved / nowhere / ambiguous); the read counter; chunked rows (one address, legacy,
missing, unregistered backend); a backend that keeps failing is given up; `local` is no tier.
`job_rules_test.go`: the routing rule file's five rules, severities, series, and the job in
`JobStale`. `internal/engine/store_identity_test.go`: `SharedStores`. Dashboard:
`TestHandleAdminSystem_ShowsTheRoutingTruthLine`. Updated: the storage-class tests that pinned
`local → REDUCED_REDUNDANCY` (engine ×3, api ×2), the job table test, `jobs_wiring`.

`go test -race ./...` green on `vaultaire_test_r75`; `make lint` 0; `make gosec` 0 issues.

## Live proof (the branch binary, a private migrated database, `local` the only backend)

Before boot: three rows planted for a tenant with no driver — two on `onedrive`, one NULL.

1. Boot → two Error lines: `rows name a backend no driver is registered under …
   table=object_head_cache backend=onedrive rows=2 registered=[local]` and the same for `<NULL>`.
2. An admin registered, two objects PUT through aws-cli, the second's file removed from
   `DATA_PATH`. `head-object` → **200, 10 bytes, STANDARD** (a `local` row no longer says
   REDUCED_REDUNDANCY); `get-object` → NoSuchKey.
3. `POST /api/v1/admin/jobs/routing_truth/run` → 202. `GET /api/v1/admin/routing-truth`:
   `last_outcome: ok`, note `local: 2 of 2 rows sampled, 1 present, 1 missing; rows on no
   registered backend: (NULL) 5, onedrive 2`, `last_run.backends.local = {rows 2, sampled 2,
   present 1, missing 1, changed 0, errors 0, missing_ratio 0.5}`.
4. `/metrics`: `vaultaire_routing_truth_checks_total{backend="local",result="missing"} 1`,
   `…{backend="onedrive",result="unknown_backend"} 2`, `…{backend="",…} 5`,
   `vaultaire_routing_unknown_backend_rows{backend="onedrive",table="object_head_cache"} 2`,
   `vaultaire_routing_truth_last_run_missing_ratio{backend="local"} 0.5`,
   `vaultaire_routing_shared_store_backends 0`.
5. `POST …/resolve-null` (dry run): `rows 5, resolved 0, nowhere 5, ambiguous 0, errors 0,
   remaining 5` — nothing written.
6. **Restart.** 3 s later, before any run in the new process: `…last_run_missing_ratio{backend="local"} 0.5`
   and `…last_run_timestamp_seconds 1.790990286e+09` — from `job_runs`.
7. A GET of a row on `onedrive` as its tenant → NoSuchKey;
   `vaultaire_routing_unknown_backend_reads_total{backend="onedrive",op="get"} 1` and one Warn.
8. `job_runs`: `routing_truth | ok | 2 | <the note> | {"checks": 2, "chunks": {…}, …}`.

## Prod (read-only, 2026-10-03 00:50–01:20 UTC; `ssh vaultaire-slc`, psql read-only, the tool with the service env)

The table at the top, and:

- Tenants: `tenant-357e3a63382e091f` BenchTest (2026-04-07), `tenant-14a623b16b3f7012` Bench
  (05-12), `tenant-f4294518dc0a7a8f` Bench2 (05-12), `tenant-cb05d8eeb2093573` FairForge Demo
  (07-31). No user is `pending_deletion`. `feature_flags` has no rows (`chunking` still ON by
  code default — the [YOU] row of WP-R8-7 stands).
- `local` rows vs `DATA_PATH` (`/opt/vaultaire/data`, 10 files, all the demo's `letshow` /
  `letshow-priv`): present 2 (`letshow/place.json`, `place.png`), missing 449 (Bench2 447, Bench 2).
- `onedrive` (Bench2's `vbench-user1-vaultaire`, keys `bench/burst/…`, `bench/warm-get/…`, 519 B –
  64 MiB): 30 sampled through the permafrost driver → **20 present, 10 missing**. BenchTest's one
  row (`validate-slc-onedrive-versioned/versioned-key.txt`): present.
- `permafrost` (Bench, 25 rows): 25 present.
- NULL (62): BenchTest's `stress-test` 24, `validate-tuned` 12, `range-fix-test` 5, `dl-bench` 4
  and five more benches; Bench2's `vbench-user1-vaultaire` 12. 13.3 GB on paper.
- `idrive`: 1,370 rows of the two bench tenants predate 2026-09-21; the demo tenant has 642
  pre-change and 25 post-change rows. The bucket holds exactly 30 objects, all under
  `t-tenant-cb05d8eeb2093573/…_letshow/` (`archive/2026-09-23T02.png` … `2026-09-24T02.png`,
  `live`, `live.png`, `sig`, `sig.png`, `versions.json`). **Every signed HEAD and GET through the
  server's own driver config answers 403** — see below.
- `job_runs`: every job `ok`; `dedup_gc` 10-02 04:30 (0 rows); `retention` 03:30 (2,132 rows);
  `smart_demotion` 06:50; the interval jobs within the hour.

### Found on the way: prod's iDrive key cannot read (P1, [YOU] 0)

The tool's probe of the demo tenant's 667 `idrive` rows: **0 present, 0 missing, 667 error**, all
`HeadObject … StatusCode: 403 … Forbidden`. A single-object probe of `letshow/live.png` — one of
the 30 objects the same key LISTS in the bucket — HEAD → 403 Forbidden, **GET → 403
`AccessDenied`**. The dead-account rows answer the same 403, so with this key "gone" and "there"
are indistinguishable. Prod's own journal agrees: four `backend failed, trying next … idrive get
tenant-cb05d8eeb2093573_letshow/codes.png … 403 … AccessDenied` lines on 2026-09-30 (the only
reads of the primary in seven days). `vaultaire_backend_health{backend="idrive"} 1` — the probe is
HeadBucket, and HeadBucket and ListObjectsV2 work; PUT worked until 2026-09-24 02:00 (the last
head row; the hourly `letshow` archive stops there — whether the cron stopped or PUTs began to
fail is not visible from the box). The synthetic check (`SYNTHETIC_CHECK_*`, R13-13) that would
have caught this is still unset on prod. This WP's job classifies it honestly — `error`, never
`missing`, ten calls then given up, the breaker untouched — and `RoutingTruthMissing` will NOT
fire for the primary until the key is fixed; the Warn lines "backend could not be asked" and the
`error` count in `GET /api/v1/admin/routing-truth` will. Not this WP's to fix (a key or a bucket
policy in the reseller console); it is [YOU] 0, before every other step of the plan, because the
deletion runner's `DeleteObject` through the same key will very likely 403 too and defer every
erasure of a tenant with `idrive` rows.

## Decisions taken

1. **HEAD stays cache-only** (CLAUDE.md non-negotiable 2). A HEAD that asked the backend would
   turn a ~1 ms answer into a round trip and make HEAD depend on a backend outage. The truth is
   the job, the admin endpoint, the metrics and the rules; a `missing` row is a Warn line with
   tenant, bucket and key. The operator sees it there, the customer sees it on GET (404), as
   before.
2. **The job observes the breaker and never charges it.** A diagnostic must not move customer
   writes (the primary's open breaker fails PUTs over to Lyve for 30 s).
3. **`local` is never a durable class.** `REDUCED_REDUNDANCY` is unmapped; `local` reports
   STANDARD. `DATA_PATH` is not backed up (R9-12) and the hub's disk is a single copy; the only
   bytes on it in prod are the demo's ten files. `local` stays registered: it is the development
   primary and the engine's last-resort registration. No PUT path can place on it on prod (the
   header was already dropped, R2-07; now the class is not in the map at all).
4. **Chunked rows are checked chunk by chunk, not by the manifest's existence.** The manifest is
   rows; the bytes are the chunks.
5. **The result lives in `job_runs.result` (migration 074)** rather than a table of its own or an
   in-process gauge: one upsert, read by three consumers, right after a restart.
6. **The jump pages, the backlog warns.** `offset 25h` against yesterday's run.
7. **The prod backfill is a plan Isaac runs**, not code that runs itself (the brief). The one
   server-side write — NULL rows — is a dry-run-by-default admin call that writes only a row
   exactly one backend holds, under a guard.
8. **Bench tenants: erasure, not row surgery.** The deletion runner deletes each object on its
   recorded backend (a miss is fine), releases quota, aborts multipart, then the sweep lists what
   every registered backend still holds for the tenant and deletes it (the fleet's `onedrive`
   bytes included: permafrost is registered) — exactly what the rows need, with an audit row.
   What it cannot do: delete on the primary while the key 403s (deferred, hourly retry) — [YOU] 0
   first.

## Not done / what I could not prove

- Lyve, Geyser, R2 and the region drivers were not probed from the tool (their credentials were
  not in the tool's env — only `DATA_PATH`, `IDRIVE_*`, `TENANT_N_*` were wired); the job on the
  server asks every registered driver.
- The job on prod's real tables: the branch is not deployed. Expected first run: `idrive 300
  sampled, 10 error, given up` (until [YOU] 0), `local 300 sampled, ~298 missing`, `permafrost 20
  present`, `lyve 6`, `geyser 2`, `r2 29` present (if the keys read), unknown `onedrive 2613`,
  `"" 62`; `RoutingTruthMissing{backend="local"}` fires; `RoutingUnknownBackendRows` fires for
  both; nothing pages.
- Whether PUTs to the primary still work with the current key (the last one is 2026-09-24).
- `OFFSET` sampling at millions of rows.
- WP-R6-1 itself (location-authoritative reads) — this WP makes it safe to start.

## [YOU] — the plan, in order, with expected counts

All reads first; every write is one you type. The tool: `set -a; . /opt/vaultaire/configs/.env;
set +a` as the service user, then `./routing-truth` (cross-compiled: `GOOS=linux GOARCH=amd64 go
build -o routing-truth-linux ./cmd/tools/routing-truth`, scp to `/tmp`). The admin calls need an
admin JWT (`POST /auth/login`).

0. **Fix the primary's key.** In the iDrive reseller console, give the server's key read on the
   `vaultaire` bucket (or issue a new pair and rotate `IDRIVE_ACCESS_KEY` / `IDRIVE_SECRET_KEY`).
   Verify: `./routing-truth -one idrive:tenant-cb05d8eeb2093573/letshow/live.png` → `HEAD …
   exists=true`, `GET … ok, read 1 byte`. Set `SYNTHETIC_CHECK_URL` + the synthetic tenant's keys
   (SYNTHESIS checklist row 4) so the next dead key pages within two minutes. Install the rules:
   `sudo cp deploy/monitoring/vaultaire-*.yml /etc/prometheus/rules/ && sudo promtool check
   rules /etc/prometheus/rules/*.yml && sudo systemctl reload prometheus` (seven files now;
   `routing_truth` must appear in `JobStale`'s regex).
1. **Deploy this PR**, then `POST /api/v1/admin/jobs/routing_truth/run` and read
   `GET /api/v1/admin/routing-truth`. Expected with a working key: `idrive` 300 sampled, ≈ 295
   missing (the 1,370 bench rows + 637 of the demo's point at the dead account; only 30 demo
   objects exist), `local` 300 sampled ≈ 298 missing, `permafrost` 20 present, unknown
   `onedrive 2613`, `"" 62`. Two warnings, one info, no page.
2. **NULL rows (62)**: `POST /api/v1/admin/routing-truth/resolve-null` (dry run) → expected
   `nowhere` for most (bench data from May/June on backends that have since changed), possibly a
   few `resolved` on lyve/geyser. Then `?dry_run=false&limit=100`. `nowhere` rows are the owner's
   to delete (step 4 does it).
3. **`onedrive` rows (2,613)**: do NOT rename them wholesale — a third are gone. Either leave them
   to step 4, or, if Bench2 is to be kept, rename only what the fleet holds:
   `./routing-truth -backend onedrive -sample 2612` lists the present/missing split; a per-row
   rename needs the probe's keys (ask me for a `-rename-present` mode if you want to keep the
   tenant — I did not build a writer the brief did not ask for).
4. **Erase the three bench tenants** (BenchTest `tenant-357e3a63382e091f`, Bench
   `tenant-14a623b16b3f7012`, Bench2 `tenant-f4294518dc0a7a8f`) through the product:
   `DELETE /api/v1/manage/account` as each (or the dashboard) → 30-day grace → the runner deletes
   every object on its recorded backend (misses are fine), releases quota, and the sweep deletes
   what the fleet, Lyve, Geyser and iDrive still hold for them; `account.erased` rows record
   `swept` per backend and `unswept_backends: [onedrive]`. Expected: 1,370 + 2,613 + 449 + 62
   + 25 + 5 + 2 rows gone, 17.3 GB of paper storage released, the three tenants' quota rows gone.
   To skip the grace: `POST /api/v1/admin/account-deletion` after setting the three users'
   `deletion_scheduled_at` to the past — your call (D-15 said 30 days for customers; these are
   yours). This is **the decision to confirm**: erasure instead of row surgery.
5. **The demo tenant's dead `idrive` rows (637)**: after step 0, `./routing-truth -backend idrive
   -tenant tenant-cb05d8eeb2093573 -sample 700` → expected 30 present, 637 missing. Delete the 637
   as the tenant (`aws s3 rm` of the keys the Warn lines name, or a `DeleteObjects` of the
   `letshow/archive/2026-08-*` and `2026-09-0*` prefixes) — DELETE releases quota; never the rows
   alone. The two `local` rows (`place.json`, `place.png`) are real: re-upload them so they land on
   the primary, then delete the old keys, or leave them (two files, the hub's disk).
6. **Confirm**: (a) `local` is no tier (REDUCED_REDUNDANCY → STANDARD on the primary; `local`
   reports STANDARD); (b) the bench tenants' erasure over row surgery; (c) whether the demo's
   `onedrive`/`permafrost` second copies matter to you at all now that the fleet is benched.
7. After steps 2–5 the next run should read: every backend 0 missing, unknown rows 0;
   `RoutingTruthMissing` and `RoutingUnknownBackendRows` resolve on their own. Then WP-R6-1 can
   remove the fan-out.
