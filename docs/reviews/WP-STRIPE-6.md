# WP-STRIPE-6 — the management key, then per-key and per-bucket budgets (Stage 1a Prompt 54), after Part 0 = RP86's findings

*Plan § WP-STRIPE, the **WP-KEY-1** row (D-STRIPE-29; F-39; `docs/ACCOUNTS.md` § Principal). It asks for:*
- *the management key: an opaque `sk_live_…` / `sk_test_…` bearer of the TENANT, sha256-stored, shown once, scopes `manage:read` / `manage:write` / `platform` / `apps`, an S3 ceiling for what it mints (RP82-6), revocable, last use recorded; `requireCaller` beside the JWT;*
- *budgets: a scoped key's and a bucket's storage cap and egress cap per UTC month, min(key, bucket, tenant, pool), a refusal that names the cap (`x-stored-cap`), STS narrowing only, the edge admit enforcing at presign.*

*Part 0 is the post-merge review RP86 (`docs/reviews/WP-STRIPE-5-REVIEW.md`), plus its three corrections to PR A. PR A was read before merge by **RP88 Part 1** (`docs/reviews/WP-STRIPE-6-REVIEW.md`): it held at P4. RP88-1…3 and D-RP88-1/2 were taken before it merged, as Prompt 54's addendum says.*

*Prompt: `docs/reviews/WP-STRIPE-PROMPTS.md` § Stage 1a Prompt 54 (the Opus edition) and its addendum after RP88's pre-merge pass. Sessions 2026-10-30 (Part 0, PR A, a first PR B/C kept as `stage1a-p54-*-archived` and not merged) and 2026-10-31 (a cold start: RP88's fixes, PR B rebuilt to the owner's F-39 decision, PR C, this note). **Migration 121** (`121_management_keys.sql`, landed whole with PR A). Next: **122**. The post-merge review is **RP88 Part 2**.*

Contract lines cited (`docs/ARCHITECTURE_CONTRACT.md`):
- **4** (keys only through the one hierarchy): S3 keys are still minted through `persistAPIKey`, now possibly by a management key bounded by its ceiling. A management key is no storage key and holds no key material. Caps are numbers on existing rows.
- **5** (convergence within a space only): untouched. An app's key reaches its own subtree's tenants through the platform routes, never another space's data.
- **6** (the chunker is frozen): untouched.

## What was built

| PR | What | Tests (red first) |
|---|---|---|
| **#1091** Part 0 | RP86-1: the GDPR export inside the residency, and the system bucket never blocks the set. RP86-2: the bucket label is its region's. RP86-3: the boot backfill takes the backend's region. RP86-4: a sign-in that rejoined is not erased. RP86-5: promtool in CI. Reviewed by RP88 Part 1: holds. | `TestResidency_TheExportStaysInside`, `TestResidency_TheSystemBucketNeverBlocksTheSet`, `TestBucketResidencyLabelIsTheRegions`, `TestAccount_ASignInThatRejoinedIsNotErased` |
| **#1093** PR A — the management key + migration 121 | `management_keys`; `requireCaller` (scopes as the role per surface); `requirePerson` (403 `person_required`); the S3 ceiling through `CapScope`; `audit_logs.principal`; `created_by` with no foreign key (ByTenant + ByUser); `keys_made` / `revoke_keys` cover management keys. **RP88, before merge:** `"test": true` mints on the sandbox (`auth.ensureSandboxTx`, now the one sandbox maker), `expires_at`, the live-key count under `FOR UPDATE`; and 121 took PR B's columns (D-RP88-2's `written_by_key` ×3 and the two storage counters; F-39 (a)'s egress month counters and warning guard). | `TestManagementKey_ShownOnceStoredAsHash`, `_RevokedIsRefusedAtOnce`, `_ExpiredIsRefused`, `_LiveCountHoldsUnderConcurrentMints` (red under `FOR SHARE` through a widened window), `_TestMintIsTheSandboxs`, `_ScopesAreItsAuthority`, `_ReachesThePlatformRoutes`, `_AnAppsKeyReachesOnlyItsSubtree`, `_RevokedIsRefusedAtOnceThroughTheAPI`, `_CeilingIsItsOwnField`, `_AuditNamesThePrincipal`, `_NeverManagesMembers`, `_TestKeyActsOnTheSandbox`, `TestErase_AMembersErasureKeepsThePlatformsManagementKey` |
| **#1095** PR B — budgets | D-RP88-1: `KeyScope.KeyID`, `Primary`, the key's egress month from the one lookup. Attribution: `written_by_key` on every row writer. The ledger: `usage.Holders` beside the tenant's, `ReserveTxFor` = min(key, bucket, tenant, pool), `x-stored-cap` on every refusal. The egress caps refuse when spent, with `key.cap_warning` at 80 / 95 %. STS acts as its parent. The edge admit runs the same reservation, and a spent read passes through. `PUT …/keys/{id}/caps`, `PUT …/buckets/{name}/caps`. The reconciles and the sandbox purge re-assert the counters; the drift is a gauge. | `TestKeyScope_NamesItsKey`, `TestKeyCap_RowsNameTheirWriter`, `_PutRefusedPastTheKeysStorageCap`, `_403NamesTheCap`, `_DeleteByAnotherKeyReleasesTheWriters`, `_BucketCapAboveTheTenantsIsANote`, `_EgressCapRefusesAtTheKey`, `_STSNarrowsNeverWidens`, `_EdgeAdmitEnforcesIt` (five of the eight fail under a mutation that disables the key-cap check and the egress refusal) |
| **#1096** PR C — the dashboard | Management keys on the API keys page (scopes, test mode, expiry, shown once, revoked at once); a scoped key's caps (GiB) in a Caps column with its form; the primary "never capped". | `TestDashboard_ManagementKeysShownOnceAndRevoked`, `TestDashboard_ManagementKeyTestModeAndExpiry`, `TestDashboard_KeyCapsSetAndShown` |
| **PR D** this note | STATUS, the plan row, CLAUDE.md, `docs/API.md` § Management keys + § Keys, Prompt 55 in both editions. | — |

**Migration 121:**
- `management_keys` (`id`, `tenant_id` → `tenants` ON DELETE CASCADE, `prefix`, `key_hash` unique sha256 hex, `hint`, `name`, `scopes text[]` CHECKed, `s3_ceiling jsonb`, `created_by uuid` with no foreign key, `created_at`, `last_used_at`, `revoked_at`, `expires_at`);
- `api_keys` / `buckets` `.storage_limit_bytes`, `.egress_limit_bytes` (NULL = inherit);
- `object_head_cache` / `object_versions` / `multipart_uploads` `.written_by_key UUID`;
- `api_keys.storage_used_bytes BIGINT NOT NULL DEFAULT 0`, `buckets.storage_used_bytes BIGINT` (NULL = not counted);
- `api_keys` / `buckets` `.egress_used_bytes`, `.egress_month`, `.egress_warned_pct`;
- `audit_logs.principal`, `audit_logs_archive.principal`.

## Step 0 answers

- **Were RP88's sections on main?** Yes: #1094 merged 2026-10-31 12:23 UTC. It also carried RP88-0, the `internal/payments` month-end rule that made #1093's CI red.
- **What did the archived session leave?** PR A on origin (three commits, rebased cleanly). Locally, a PR B built to the prompt's old shape (a bucket's egress paced, then refused past 2 ×; no storage cap) and a PR C (the management keys page). PR B was rebuilt from PR A's tip to the owner's decision; the old one is kept as `stage1a-p54-budgets-archived` and was never pushed. PR C's commit was cherry-picked and extended.
- **Does `object_head_cache.written_by_key` exist?** It does now: D-RP88-2 put it into 121 before PR A merged, so step 6 is no STOP.

## Decisions (D-STRIPE-59…)

- **D-STRIPE-59: a test management key is minted on the sandbox by the one sandbox maker.** `auth.ensureSandboxTx` makes the sandbox and its quota row on first use, for the test S3 key and the test management key alike. The tenant lists and revokes its sandbox's keys. A sandbox is no platform and no app, so `platform` / `apps` on a test key are refused (`invalid_scope`).
- **D-STRIPE-60: a key never outlives the key that mints it.** A platform key's mint of an app's key has its `expires_at` clamped to the platform key's own.
- **D-STRIPE-61: F-39 as the owner decided it, 2026-10-31.** (a) A key's or a bucket's egress cap refuses when spent: 403 `QuotaExceeded`, `x-stored-cap`, `Retry-After` at the next UTC month, warnings at 80 / 95 %. There is no pacing at the key or the bucket, no 2× hybrid; pacing stays the tenant's (D-25). (b) The minimum always applies; a larger number is a note. (c) The primary is never capped.
- **D-STRIPE-62: the egress month is a durable counter on the row, not the in-memory meter.** `egress_used_bytes` / `egress_month` move with each 200/206 response of a capped key or bucket. Both slots and a restart then see one number, which a refusal needs: the per-process meter (RB11-4) would refuse on one slot and serve on the other. A key's counter rides `LookupCredential`'s SELECT, so a GET costs no extra query for it. A bucket's costs one.
- **D-STRIPE-63: a response admitted under the cap is served whole.** The refusal is decided before the body, so one response may pass the cap by its own size. Byte-exact refusal would cut a body in the middle (RB12-2).
- **D-STRIPE-64: the primary pair's writes name no key, and its scope has a KeyID.** D-RP88-2 records the primary as NULL, so the most-used credential takes no extra row lock per write. The owner's (c) keeps the KeyID on its scope. A token of the primary is the primary's.
- **D-STRIPE-65: a CompleteMultipartUpload's object is the upload's key's,** whoever completes it: the parts were held on that key.
- **D-STRIPE-66: the holders' lock order.** A holder row (`api_keys` by id, then `buckets` by name) is locked only inside a ledger statement, after the tenant's own quota row, which serialises the tenant's ledger. A reservation locks the holders before its floor rows, so a refusal charges nothing anywhere. A settle locks them after. A change that moves only holders (a write as large as its reservation, over another key's row) locks no tenant row. No transaction applies a holders-only change and then a tenant change (RB12-8).
- **D-STRIPE-67: a net reservation charges the holders net too.** R10-20's rule: an overwrite at the edge is reserved less what its commit releases. The commit is held to every cap (`Guard`) when the credit was another key's bytes.
- **D-STRIPE-68: a bucket is counted only while it has a storage cap.** `SetBucketCaps` seeds the counter from the rows under the bucket's lock, and the reservation locks the same row. Clearing the cap clears the counter. An uncapped bucket takes no per-write row lock. A non-primary key's counter always moves, so a cap set later is true at once.
- **D-STRIPE-69: there is no daily rewrite of the counters.** The prompt says "the daily reconcile", but there is no daily reconcile job: the tenant's reconcile is an operator call, "run only while writes are quiesced", and a rewrite under live writes erases reservations in flight. The counters are re-asserted where the tenant's are: the admin reconcile, whole or per tenant, and the sandbox purge. The drift is a gauge, the pool counter's rule (RP82-3).
- **D-STRIPE-70: a key's egress counter counts only while the key is capped,** from 0 at its first cap. The request log is flag-gated and kept 30 days, so it is no seed.
- **D-STRIPE-71: an STS token has no caps of its own.** It acts as its parent: its writes are the parent's, the parent's caps refuse it, and it cannot widen them. A token-level lower cap would need counters of its own for a credential that lives ≤ 12 h.
- **D-STRIPE-72: an edge GET of a spent cap is a pass-through (`capped`).** The origin refuses it, never the admit (Part B's rule). A read's bytes, committed or expired, count to the caps the admit found (`attrs.egress_key`, `egress_bucket`).
- **D-STRIPE-73: caps have their own routes** (`PUT …/caps`), beside `/tier` and `/residency`. Both members are always sent; null = none.

## Residuals (RB12-n)

- **RB12-1: the deploy overlap.** The previous build writes no `written_by_key` and moves no holder counter. A capped key's write served by the old slot during the switch is uncounted (its row names no key), and so is a capped GET. The drift gauge shows it; the operator's reconcile re-asserts the counters.
- **RB12-2:** one response may pass an egress cap by its own size (D-STRIPE-63).
- **RB12-3:** a GET reads its bucket's egress cap with one query, capped or not.
- **RB12-4:** a codec part refused at its commit (`failWithRefs`) answers 403 `QuotaExceeded` without `x-stored-cap`. The admit's refusal names the cap.
- **RB12-5:** a direct upload (`/direct-uploads/complete`) and a generated object count to their bucket only, never to a key: neither carries an S3 credential.
- **RB12-6:** the holder drift is a gauge (over the capped keys and the counted buckets) with no alert rule yet.
- **RB12-7:** the prod proof is owed: it needs the owner's TOTP (below).
- **RB12-9:** `written_by_key` has no index. The drift gauge reads the capped keys only, but `KeyHeldBytes` and the reconcile's `ReassertHolders` scan the head rows per key. A partial index (`WHERE written_by_key IS NOT NULL`) on the three tables is a candidate for migration 122.
- **RB12-8:** a transaction that applied a holders-only change and then a tenant change could deadlock against a reservation. None does today (D-STRIPE-66). The structure test pins the tenant half's order, not this.

## Open questions

1. **`standard_sealed_default` per tenant** (WP-STRIPE-5's open question 1): still the owner's. Nothing here changes it.

## The prod proof

**Deployed:** #1093 as `3e4bd888` (migration 121 on prod: `management_keys` and every new column checked), #1095 as `ee823db2`, #1096 as `93679a1b` (2026-10-31; `/health` 21/21 backends). No management key and no cap exist yet, so nothing changes for any tenant until a person sets one.

**Owed: it needs the owner's TOTP.** Minting a management key needs a person (`requirePerson`); `POST /auth/login` asks for the TOTP code since #956, and so does the dashboard.
1. The owner signs in (`POST /auth/login {email, password, code}`) and mints a key: `POST /api/v1/manage/management-keys {"name": "proof", "scopes": ["manage:write", "platform"], "expires_at": "<+1 d>"}`.
2. From a shell with no person's token: `POST /api/v1/platform/tenants {"label": "kp-proof"}`, then `POST /api/v1/platform/tenants/<id>/keys`. A child and its key, with no person in the loop.
3. With the same key: `POST /api/v1/manage/keys {"name": "capped"}`, then `PUT /api/v1/manage/keys/<id>/caps {"storage_limit_bytes": 104857600}`.
4. With the capped key: 100 MiB in one PUT through stored.ge is 200. A further 1 MiB is 403 `QuotaExceeded`, `x-stored-cap: key`, through stored.ge and through `s3.stored.cloud` (the Worker's admit). Then delete the objects and revoke both keys.

## Post-merge review

**RP88 Part 2** (`docs/reviews/WP-STRIPE-6-REVIEW.md` § Part 2, 2026-10-31): **HOLDS at P2**, every finding latent on prod (no cap, no management key exists). Fixed in the review's PR: RP88-4 (the two egress-cap counters were never registered — the RB2-6 shape again), RP88-5 (a test management key resolved no actor: `POST /keys` was 401 `missing_user`; it now mints the account's test keys). To Prompt 55's Part 0: RP88-6 (P2: the egress cap admits a response whatever its size — refuse by the remaining bytes at `WriteHeader`, at `/cdn` and at the edge admit's `length`), RP88-8 (a management key may set or lower a cap, never raise or clear one), RP88-7 / RP88-9 / RP88-11 (P4s), RB12-6 and RB12-9 with their shapes; RB12-10 added (the seed and a write in flight). The prod proof (RB12-7) stays owed.

- **RB12-10:** a bucket counter seeded while a write of that bucket is in flight (its reservation ran before the bucket was counted, its commit after the seed) is low by that write until an operator's reconcile; the drift gauge shows it (RP88-10).
