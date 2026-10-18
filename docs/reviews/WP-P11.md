# WP-P11: Stage 1a Prompt 11

2026-10-17 → 18. Three parts:

- **Part 0**: start clean.
- **Part A**: WP-R6-2. The engine's driver map and primary are read under its lock.
- **Part B**: WP-R10-2. The Stripe webhook is applied in order, and each event id is claimed before its handler runs.

PRs:

- **#750**: Part A, deployed `e604bcc` (prod since 2026-10-17 23:06 UTC, slot 8000).
- **#752**: Part B, merged `d5d0087`, deployed with `2d46142` (23:01 UTC). It ships dark: prod has no `STRIPE_*`, so `/webhook/stripe` is not mounted.
- this PR: docs only.

Start sha: `origin/main` `2903d84`. **No migration.** The next number stays **093**.

## Decisions

1. **Part 0.1 fell to nobody here.** At the start, the WP-P9 note was in progress in another session's worktree (`../vaultaire-p9`, uncommitted SYNTHESIS edit, waiting on the 02:30 `dedup_gc` proof). It has since landed as #755, and WP-P10 as #756. Neither was touched here.
2. **When registration closes: an explicit `Seal()`, not the first call.**
   - The first build sealed implicitly, at the first data-path call. 17 `internal/api` tests then failed: they swap a driver in after seeding (a panicking delete driver, a region driver added after a 503, the parity legs).
   - With the copy-on-write map a late registration is race-free (`TestAddDriver_UnsealedDuringTraffic_IsRaceFree`). So the seal is a policy for the product, not a memory-safety need.
   - `main` (and `cmd/tools/dedup-migrate`) call `eng.Seal()` right after `SetPrimary`. Test fixtures never seal.
3. **`AddDriver` returns an error.**
   - `main`'s `addDriver` makes a refusal a boot Fatal: a backend silently missing would 503 every row on it.
   - 287 test call sites ignore the return. They are covered by one targeted `.golangci.yml` errcheck exclusion (`_test.go`, text `AddDriver`).
4. **`SwapPrimary(name) (previous, error)` is the admin's lever.**
   - It runs `CheckPrimaryEligible`'s rules and the swap under one `Lock`. That matters because `TargetOnly()` (#748) can change at run time.
   - `SetPrimary` stays unchecked, for boot (after main's own checks) and for tests.
   - The audit row and the Info line now name the `previous` primary.
5. **The claim holds a transaction instead of a lease column.**
   - The prompt offered two shapes: DELETE on failure, or a `processed_at` lease (migration 093).
   - DELETE-on-failure loses the event when the process dies between claim and handler: the row stays and the retry reads as a duplicate.
   - A transaction held open across the handler meets every requirement with no migration:
     - a dead process takes its connection, and so its claim, with it;
     - a concurrent duplicate waits on the uncommitted row (Postgres's speculative insert);
     - `processed_at` keeps meaning "committed".
   - Cost: two pooled connections per webhook while it runs, out of a pool of 50. Waits are bounded by `lock_timeout` 5 s.
6. **One customer's events are applied one at a time.**
   - The claim also takes `pg_advisory_xact_lock(hashtextextended('stripe-customer:'||customer, 0))`.
   - Fetching alone leaves a window: two `updated` events in flight fetch S1 then S2, and the one holding S1 applies last. Under the lock, the event applied last fetched last.
7. **`paused` is treated as `unpaid`.**
   - Stripe's `paused` status means a trial ended without a payment method, and no invoice is created while it lasts. So nothing is paid, and `applyHouse` clears the house: data untouched, current-subscription guard kept.
   - It is resumable, so `HasLiveSubscription` counts it and the dashboard refuses a second checkout beside it, as it does for `unpaid`.
   - Our Checkout creates no trial, so `paused` arises only from an operator adding one by hand.
   - The Stripe dashboard's "pause payment collection" (`pause_collection`) is a different thing. The status stays `active` and the house is kept; that is an operator's deliberate choice.
8. **`customer.subscription.deleted` keeps its payload.** `canceled` is terminal at Stripe, so a fetch can tell nothing newer. A stale `updated` delivered after it fetches `canceled` and clears the house again.
9. **A Stripe 404 (`resource_missing`) is nothing to apply.** It gets 200 and a Warn, for checkout and for subscription events. Before, it was a 500 that Stripe would retry for three days and then disable the endpoint.
10. **The prod swap was driven through the real dashboard handler.**
    - The owner asked that the admin work be done directly. The only admin account has MFA in the owner's authenticator.
    - So a 15-minute `dashboard_sessions` row was inserted for that user (user agent `wp-p11-swap-proof`).
    - The session made the two `POST /admin/backends/{name}/primary` calls with the page's CSRF token, and the row was deleted after. Password and MFA were left untouched.

## What was built

### Part A: WP-R6-2 (#750)

- **`view()`**:
  - Every entry point reads `primary` and the map once, under `RLock`. The entry points: `Get`, `GetRange`, `Put`, `Delete`, `List`, `GetOn`/`PutOn`/`DeleteOn`/`ExistsOn`, `CanAsk`, `ParallelRangeSource`, `CheckDriver`, `HealthCheck`, `GetMetrics`, `CheckPrimaryEligible`.
  - The view is threaded through `preferredBackend`, `readCandidates`, `buildCandidateList`, `buildWriteCandidateList` and `deleteOn`.
  - A Put now resolves its target and builds its candidates against the same primary. On `2903d84` those were two separate reads.
- **Copy-on-write map**: `AddDriver` publishes a copy, so a view's map is read after the lock is released.
- **`Seal()` and `ErrEngineServing`**: see Decisions 2 and 3.
- **`SwapPrimary`**: see Decision 4. `HandleSetPrimary` uses it.
- **`engine_locking_test.go`** (`TestEngineRegistry_ReadOnlyUnderTheLock`): an AST walk over the package's non-test files. It fails on:
  - a read of `drivers` or `primary` outside the listed `e.mu` sites (`AddDriver`, `SetPrimary`, `SwapPrimary`, `view`, `GetDriverNames`, `GetPrimary`, `GetDriver`, `SharedStores`);
  - a listed site that takes no lock;
  - a stale list entry;
  - any in-place write into a driver map.
- **`FailoverManager` checked**: its breaker map is under its own `f.mu`, and the three reads from outside (`CanAsk`, `ParallelRangeSource`, `RecordReadOutcome`) take `e.failover.mu.RLock`.
- **The 17 sites the prompt counted**: 15 functions read the registry outside a lock (the red list below), plus `preferredBackend` through `GetPrimary` and #748's `IsDeclaredTargetOnly(e.drivers[name])`. WP-R6-1 had already put `RLock` around six of them (`readCandidates`, `deleteOn`, `on`, `CheckDriver`, `CanAsk`, `ParallelRangeSource`). Each was safe on its own, but each was a separate snapshot.

### Part B: WP-R10-2 (#752)

- **`handleSubscriptionUpdated`** (`created` and `updated`):
  - It resolves the tenant from the payload first, so a subscription that is not ours is a 200 with no Stripe call.
  - It then **fetches** the subscription and applies the fetched status, items and metadata. The payload names only the ids.
  - A fetch failure is a 500.
- **`claimEvent` + `handle`**: `ServeHTTP` claims, runs the handler, then commits; any failure rolls back. See Decisions 5 and 6.
- **`stripeMissing`**: see Decision 9.
- **`paused`**: added to `applyHouse`'s clearing statuses and to `HasLiveSubscription`'s live statuses.
- **Tests**:
  - `webhook_ordering_test.go`: signed fixtures through `webhook.ComputeSignature`, against the real Postgres `stripe_events`.
  - `fakeStripeAPI` serves `GET /v1/subscriptions/{id}` and `/v1/subscriptions?customer=` from an `httptest` server behind stripe-go's API backend, including Stripe's real 404 body. So `stripeSubscriptions.Get` and `HasLiveSubscription` run their real code.
  - The older house tests now say what Stripe holds (`f.stripeHas(sub)`) before each subscription event.
- **Stripe calls of a normal house purchase**:
  - 2 subscription GETs from the webhook (`checkout.session.completed` + `customer.subscription.created`; was 1);
  - +1 per later `customer.subscription.updated` (a resize, a dunning step);
  - the dashboard's own calls are unchanged (live-subscription list, checkout session).
- **Not touched**: the endpoint stays pinned to API version 2023-08-16 (R10-03), and stripe-go was not bumped. R10-38 (`isCurrentSubscription` fails closed) was already fixed at the start sha.

## Before → after

Before = the red tests on `origin/main` `2903d84`. Part A's AddDriver/SwapPrimary tests do not compile there (the error return and `SwapPrimary` do not exist). Part B: 8 of 8 new tests are red. The two claim-release tests are red because `updated` never fetched, so a fetch failure could not surface.

| # | Item | Before (2903d84) | After | Test |
|---|---|---|---|---|
| A.1 | Calls on every entry point while `SetPrimary` flips and `AddDriver` runs | 14 `DATA RACE` reports under `-race`, at engine.go:139/172/281 (the closures), 414/421 (List), 440 (HealthCheck), 461/462 (GetMetrics), 512–522 and 643 (the builders), against writes at :74/:89. Without `-race` the process died in **3 of 5** runs: `fatal error: concurrent map read and map write` / `concurrent map iteration and map write` | race-free; every recorded-backend call answers from its backend; late registrations on a sealed engine refused (50/50) | `TestEngine_ServingCallsDuringPrimarySwapsAndLateRegistrations` |
| A.2 | `AddDriver` after the seal | not expressible (void, in-place map write) | `ErrEngineServing`, an Error line, nothing registered, no breaker | `TestAddDriver_RefusedOnceTheEngineIsSealed` |
| A.2 | `AddDriver` on an unsealed engine during traffic | a map write beside reads | race-free (copy-on-write) | `TestAddDriver_UnsealedDuringTraffic_IsRaceFree` |
| A.2 | eligibility check + swap | two calls, two lock sections | one call under one `Lock`; `previous` returned and audited | `TestSwapPrimary_ChecksAndSwapsUnderOneLock` |
| A.3 | structure | 15 functions read the registry outside a locked site; `AddDriver` wrote the map in place | 0; the list is self-checking | `TestEngineRegistry_ReadOnlyUnderTheLock` |
| A.4 | `FailoverManager` | own mutex | unchanged (checked) | — |
| A.4 | primary swap persistence | process memory | process memory (WP-R12-13) | — |
| B.1a/B.2 | `updated(canceled)`, then a stale `updated(active)` | house **re-granted**, status `active` | house stays cleared, status `canceled` | `TestWebhook_StaleActiveUpdateAfterCancel_DoesNotRegrantTheHouse` |
| B.1a/B.2 | a stale `updated(unpaid)` after the paid resize | paid house **taken away** (0 TB) | kept, 5 TB | `TestWebhook_StaleUnpaidUpdate_DoesNotTakeAPaidHouseAway` |
| B.2 | a subscription Stripe 404s | house granted from the payload | 200 + Warn, nothing applied, recorded | `TestWebhook_SubscriptionStripeNoLongerHas_IsAcknowledged` |
| B.1b/B.3 | two concurrent deliveries of one event | **both** handled (2 fetches) | 1 handled, both 200, one row | `TestWebhook_ConcurrentDeliveriesOfOneEvent_AreHandledOnce` |
| B.3 | a handler failure, then Stripe's retry | (`updated` never fetched: 200 from the payload) | 500, no row; the retry is applied | `TestWebhook_FailedHandlerReleasesTheClaim_TheRetryIsApplied` |
| B.3 | the client hangs up mid-handler | 200 (payload path) | not 200, no row | `TestWebhook_DeliveryCutMidHandler_LeavesNoClaim` |
| B.1c/B.4 | `updated(paused)` | house **kept** (3 TiB limit) | cleared, free-tier limit, status `paused` | `TestWebhook_PausedSubscription_ClearsTheHouse` |
| B.4 | `HasLiveSubscription` with a paused subscription | false (a second checkout allowed) | true | `TestHasLiveSubscription_CountsAPausedSubscription` |
| B.5 | `internal/billing/CLAUDE.md` | "Ordering: `updated` still trusts the payload" | Dedup / Acknowledgement / Ordering / `paused` lines rewritten | — |

## Local live proof

- **Part A**: the race test above, under `-race` and without it, on the Mac. The full suite (`go test -race ./...` on the private DB `vaultaire_test_p11`) was green on the PR tree.
- **Part B**: the signed-fixture tests through the real `ServeHTTP` against Postgres, plus the fake Stripe API for the real `subscription.Get` / `List` paths. The full suite was green on `vaultaire_test_p11b`.
  - No Stripe CLI and no test-mode key on this machine, so there was no `stripe listen` replay.
  - **The first prod exercise of Part B is the owner's Stripe go-live.**

## Prod proof

### Part 0: the prod read, before anything deployed (2026-10-17 22:20 UTC, `2903d84`, slot 8001)

- 0 `"level":"error"` lines in the 12,101 journal lines since 19:57 UTC.
- All 18 jobs `ok`. `dedup_gc`'s note was 793 chunks not swept (the dead iDrive key).
- `vaultaire_engine_fallover_total` and `vaultaire_engine_strict_target_unregistered_total` had no series: never incremented in that process.
- Primary `lyve`. **0 `admin.primary_*` audit rows ever** (31 audit rows in all).

### Part A, after #750 deployed (`e604bcc`, slot 8000, 23:06 UTC)

- **Boot**: all 18 drivers registered, no `driver registration refused`, primary `lyve`, 0 Error lines since 23:00 UTC.
- **The swap under load (2026-10-18)**:
  - Load: the bench tenant ran 90 MB chunked PUTs back to back for 10 minutes through slot 8000 (`p11swap.sh`), with `AWS_MAX_ATTEMPTS=1`, so any retry would have shown as a failure.
  - The swaps went through the real dashboard handler, `POST /admin/backends/{name}/primary`, with a real session and CSRF token (Decision 10). Primary to `wasabi` at 03:50:50 UTC, back to `lyve` at 03:52:50. Both answered 303, and `vaultaire_primary_backend` followed.
  - **409 / 409 PUTs succeeded on the client's first attempt.** In the window: n = 26, median 1.7 s, max 30.1 s. On `lyve`: n = 383, median 1.0 s, max 8.4 s.
    - The 30.1 s upload is the known Wasabi edge stall: `vaultaire_driver_put_retries_total{driver="wasabi"}` = 5, each a fresh-connection retry that succeeded.
  - **409 / 409 GETs byte-equal** after the swap back, including the 26 window objects.
    - Their **1,146 chunks are recorded on `wasabi`** in `global_content_index` and were read from there with `lyve` as primary.
    - The other 16,642 chunks are on `lyve`.
  - `vaultaire_engine_fallover_total`: no series, so **0 fallovers**.
  - Audit: `03:50:50 admin.primary_swapped backend:wasabi previous=lyve` and `03:52:50 admin.primary_swapped backend:lyve previous=wasabi`.
  - Cleanup: every object and the bucket were deleted; 0 proof sessions are left in `dashboard_sessions`. The chunk rows are at ref 0 and go with `dedup_gc`'s 7-day grace, including the 1,146 on Wasabi (90-day minimum: about 100 MB of charge).
  - **Error lines since 03:45: 1.** `flush backend bandwidth` (`backend: wasabi`, `context deadline exceeded`, 03:52:08) is the bandwidth flusher's shared 10 s context running out under the load. It is not the engine (RP11-3).

### Part B

Nothing to prove on prod: `/webhook/stripe` is mounted only when both Stripe env vars are set, and prod sets neither.

## Residuals

| # | Sev | Item | Where it goes |
|---|---|---|---|
| WP-R12-13 | P3 | The primary swap is process memory only. The other slot and any restart read `STORAGE_MODE` again, so the next deploy undoes an admin swap silently | WP-R12-12 (persist it; show "set by admin, not by env" on the backends page) |
| RP8-11 | P3 | A chunk blob cut by its deadline after the backend stored it has no `global_content_index` row: a cost leak that self-heals on re-upload, never a correctness issue (WP-P8) | **WP-R8-2**, the next prompt's candidate |
| RP11-1 | P3 | `invoice.payment_succeeded` / `payment_failed` write `tenants.subscription_status` from the payload with no current-subscription check. Delivered after a `subscription.deleted`, they mark a starter tenant `active` / `past_due`. Only display and the admin revenue counts read it; quotas and checkout do not | WP-R10-2 follow-up: a `stripe_subscription_id` guard on both UPDATEs |
| RP11-2 | P3 | `stripe_events.tenant_id` is never written, so the support page's per-tenant event list (`admin_support.go:168`) is always empty | the same follow-up: return the resolved tenant from the handler and set it on the claim row |
| RP11-3 | P3 | `BandwidthTracker.Flush` runs every upsert under one 10 s context. Under load, the per-backend rows written last time out and that flush's per-backend counts are dropped (1 Error line, 2026-10-18 03:52:08) | a context per statement, or one batched upsert; Warn plus a retry next flush instead of a dropped delta |
| RP11-4 | note | A webhook holds two pooled connections while it runs (the claim and the handler's queries). A burst of more than 25 concurrent deliveries would queue on the pool, bounded by the request context | none until Stripe volume says otherwise |
| RP11-5 | note | `AddDriver` errors are ignored in 287 test call sites (a targeted errcheck exclusion). A fixture refused on a sealed engine fails its own calls with `ErrBackendNotRegistered`, but no test seals | none |

## Gates

- `go test -race -count=1 ./...` on private DBs, every package ok: the Part A tree on `vaultaire_test_p11`, the Part B tree on `vaultaire_test_p11b` (one process per DB).
- golangci-lint v2.14.0: 0 on both trees. gosec with the workflow's flags: 0.
- Structure tests green: the new `TestEngineRegistry_ReadOnlyUnderTheLock`, plus `TestEngineCalls_CarryTheTenant`, `TestChunkBlobCalls_GoThroughTheChunkStore`, `TestDataPathBounds_*`, the ledger tests and `TestSQLLiteralsMatchSchema`.
- `build-and-test`:
  - #750's first run failed in `internal/auth` with `pq: sorry, too many clients already` (the known CI flake, a package this PR does not touch). The re-run was green.
  - #752 was green first time.
  - The merge commits `d5d0087` and `e604bcc` are green; Deploy succeeded for both.
