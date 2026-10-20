# Stage 1a — Prompt 17: Part 0 (WP-P16 residuals), WP-R4-2 (the bucket state machine), WP-SSEC-1 (SSE-C complete, migration 096)

Date: 2026-10-20. Prod at the start: `4429cd2` (slot 8001). Branches `stage1a-p17-*`.
Prompt: Part 0 = RP17-1, RP17-2, RP17-10, RP18-1, RP18-2 + the prod read; Part A = WP-R4-2 (R4-10); Part B = WP-SSEC-1 (plan 5.14.8a; migration 096, WP-PAY-1 → 097).

## Decisions

| # | Decision | Why |
|---|---|---|
| D-P17-1 | The Object Lock headers come from the key's `object_locks` row LEFT JOINed into the ONE query HEAD, GET and `?versionId` already make — never a second round trip. | Non-negotiable 2: HEAD is one read of the cache. `object_locks` has no version column (it is what `checkObjectLock` enforces on every version), so a version names the key's lock. |
| D-P17-2 | A row with no retention but a hold (or `legal_hold = FALSE` after a hold was lifted) sends `x-amz-object-lock-legal-hold` alone; a row is the only thing that makes a header appear. | AWS sends no lock header for an object that never had a configuration, never an empty value. |
| D-P17-3 | `materialize` is two fixed buffers (a 1 MiB probe, then the one 64 MiB + 1 spool) and the temp file — not "spill at 1 MiB". | The drivers that use it keep the 64 MiB in-memory shape they were measured with (Geyser, the fixed-bucket uploader, permafrost); the cost was the doubling, not the threshold. A body under 1 MiB never takes the spool. |
| D-P17-4 | The fleet re-check runs INSIDE the probe (synchronous, 10 s of the probe's 15 s) after the accounts' probes have answered, never changing the probe's verdict. | Deterministic (a test can assert the verdict after one `HealthCheck`), bounded, one at a time (`fleetChecking`); a goroutine would have needed its own lifecycle. |
| D-P17-5 | A fleet verdict is logged once per CHANGE; the write-failure Warn and the `accepted` Warn stay unconditional. | An hourly `match` line is noise; a failed marker write is an error path every time. |
| D-P17-6 | The webhook envelope's `created_at` is the `events` row's, carried as `recordedEvent` through every delivery job; the retry job reads `COALESCE(e.created_at, o.created_at)`. | One event, one body (RP18-2). The signature's `t` dates the request. |

## What was built

### Part 0 (one PR)

- **RP17-1** `object_headers.go`: `LockMode`, `LockRetainUntil`, `LegalHold` on `objectHeaders`, written on HEAD / 200 / 206 (never on a 304); `objectLockColumns` + `lockHeadersOf`; the three queries (`handleHeadObject`, `headVersion`, `HandleGet`) alias their table and LEFT JOIN `object_locks`.
- **RP17-2** `drivers/geyser.go`: `materialize` = `materializeProbe` (1 MiB) + `materializeSpool` (the one 64 MiB + 1 buffer) + the file; two rows in `data_path_allocations_test.go`.
- **RP17-10** `dispatchWebhooks` deleted; the R11 SSRF guard test calls `deliverToEndpoints` with the endpoint it used to mock.
- **RP18-1** `drivers/onedrive_fleet.go`: `recheckFleet` from `HealthCheck` (after `unknown`: the next probe; else hourly; 10 s; one at a time), `FleetState()`, verdict/accept/time on the driver, log on change (`fleetVerdictChanged`); `cmd/vaultaire` unchanged but for the comment (the accept value boot passes is the one the re-checks use); a bounds row in `data_path_bounds_test.go`.
- **RP18-2** `events.go`: `recordedEvent` (RETURNING `created_at`), `submitEventDelivery(ctx, db, logger, ev)`, `webhookJob(…, ev)`, `deliverToEndpoints(…, ev)`; `object_delete_shared.go` carries it per pending key; `webhooks_routes.go`'s test event returns its `created_at`; the retry SELECT reads `e.created_at`.

## Before → after

| Test | Red (how) | Green |
|---|---|---|
| `TestObjectLockHeaders_AnObjectUnderTheBucketDefaultRetentionNamesItOnHeadGetAnd206` | against the branch base (= `origin/main`): mode `""`, hold `""`, date `""` on HEAD | the three on HEAD, 200 and 206 |
| `TestObjectLockHeaders_ALegalHoldIsON` | base: `""` | `ON`, no mode/date |
| `TestObjectLockHeaders_NoneWithoutARow` | guard (green on base) | — |
| `TestObjectLockHeaders_AVersionCarriesTheKeysLock` | base: `""` on HEAD `?versionId` | COMPLIANCE + date + OFF |
| `TestMaterialize_SpoolsInOneFixedBufferThenSpills` | base: 268,512,856 B allocated for a 400 MiB body (bound 70 MiB) | 64 MiB + 1 MiB + small |
| `TestOneDrivePut_AnUnknownLengthUploadsFromTheSpooledBodyWithoutASecondCopy` (the P16 test, the spool subtraction removed) | base: 339,837,768 B (bound 140 MiB) | under 140 MiB |
| `TestFleetFingerprint_AnUnknownBootVerdictIsRecheckedFromTheProbe` | new symbols: red by mutation (the `recheckFleet` call removed from `HealthCheck`): verdict stays `unknown`, gauge 0, no Error line | mismatch found at the next probe, gauge 1, one Error line, no re-read inside the hour |
| `TestFleetFingerprint_TheHourlyRecheckLogsOnlyAChangeOfVerdict` | same mutation: "the markers were re-read" fails | re-read after an hour, no second line |
| `TestWebhookOutbox_ARetrySendsTheSameBodyAsTheFirstAttempt` | base: bodies differ (the retry crosses a second boundary by a sleep of ≤ 1.1 s) and `created_at` ≠ the row's | byte-equal, `created_at` = the row's |

### Part 0 prod read (2026-10-20 01:39 UTC, slot 8001 `4429cd2`)

- `job_runs`: the last `chunk_orphans` run is **2026-10-19 01:44 UTC** (ok; lyve 38,576 listed = named, idrive 1,049, wasabi 1,318, every backend `complete`, 0 orphans; no cursor key — the result has none to resume). The 2026-10-20 01:30 run had not started at 01:39: the scheduler's hourly catch-up check runs it at its next tick (~02:10 UTC). Re-read below once it has.
- `routing_truth` 2026-10-19 06:26 UTC: idrive 10 of 501 rows sampled, 10 consecutive errors (the dead key), the rest not asked; every other backend 0 rows — as expected while `STORAGE_MODE=lyve` and the idrive rows wait for the account.
- Alerts: 12 firing = `BackendProbeFailing` × 11 (idrive + 10 regions) + `RoutingTruthNoVerdict{idrive}` — unchanged, correct, never silenced.
- 0 `"level":"error"` lines in `vaultaire@*` since 2026-10-20 01:00 UTC.
- Rule files: 12 of 12 `deploy/monitoring/vaultaire-*.yml` match `/etc/prometheus/rules/` (sha256).
- #793 (the WP-P16 post-merge docs PR) was open, green and not armed; armed and queued at 01:36 UTC.
