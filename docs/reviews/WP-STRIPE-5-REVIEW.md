# RP86 — the post-merge review of WP-STRIPE-5 (the account tree, residency, the edges; Part 0 = RP84/RP85/RP82-3/4/7)

*Reviewed 2026-10-30 (session RP86). Read adversarially:*
- *the PRs: #1085 (Part 0 + migration 120), #1086 (PR A: the tree, caps), #1087 (PR B: residency), #1088 (PR C: the edges, RB10-1) and the closeout #1089 (`docs/reviews/WP-STRIPE-5.md`);*
- *against D-STRIPE-43…58 and RB11-1…8;*
- *with throwaway tests on a private migrated database (`vaultaire_test_rp84`, migrated through 120), never committed.*

*Prod state read 2026-10-30 ~12:00 UTC (build `9e2a5095`, read-only):*
- *5 tenants, every `pool_root_id` set: tenant zero's proof child is `managed` under its platform; the others are their own roots;*
- *pool drift 0 on every root (`pool_used_bytes` = the sum of its pool);*
- *no tenant has a residency; no `_exports` bucket exists (no export has ever been made);*
- *3 owner members, 0 invitations.*

## Verdict

**HOLDS at P3.**

Part 0 closed every RP84 and RP85 finding the way the review asked:
- **RP84-1:** a ByTenant `api_keys` rule; the handover to an heir before the `users` cascade, with a `key.maker_erased` row; the schema walk now demands a tenant rule on every `tenant_id` table (and it caught `account_exports`).
- **RP84-2/6:** no role on no tenant; the accept route outside `requireMember`; both key generators refuse `''`; a login with no membership mints on no tenant.
- **RP84-3:** one CR/LF guard in every sender, the subject Q-encoded, the name cleaned at registration, both caps under one advisory lock. D-STRIPE-45 (count the day's sends from the never-deleted `member.invited` audit rows, not from invitations a revoke deletes) is better than what the review asked for.
- **RP84-4:** `keys_made` and `revoke_keys` (the STS tokens with them).
- **RP84-5:** one pending invitation per address.
- **RP85-1/2:** the `<user id>:` prefix (the column is TEXT); `Idempotent-Replayed` and `secret_redacted`.

The tree is right:
- `pool_root_id` is the one resolver inside `usage`, and a structure test keeps it so;
- `moveUsed` moves the member and the root in one statement, by what was *applied* after the clamp, so a clamp never drifts the root;
- the reconcile moves a root only by its members' corrections, never a silent rewrite;
- `DescendantT*` scopes every platform route (`SetChildCaps` included) and excludes sponsored children;
- a region-pinned bucket writes through `PutOn(regionDriver)` whatever its class or tier, so neither GLACIER nor `resilient` takes an EU bucket's bytes to the US.

All 29 tests the note claims exist by name.

One P3, four P4s. The P3 is the residency promise's one unbuilt path.

## What was checked and holds

| Area | Checked | Holds |
|---|---|---|
| RP84-1 | `keyMakerHandover`: heir = the key tenant's owner member, else the pool root's owner, else its registering user, never the erased user; the moved keys' audit rows have no `user_id`, so the user's own audit DELETE keeps them; the old ByUser DELETE runs after, for keys with no heir | Yes |
| RP84-2/6 | `requireJWT`: no role for a JWT on `''` (owner only with no database, the unit tests); `memberRole()` defaults to `""`; `DELETE /api/v1/user/signin` refuses a member or an owner (the e-mail rule included, `lower()`) | Yes (RP86-4 is a P4 on the grace) |
| RP84-3 | `checkHeaderValues(from, to, subject)` in the SMTP, Resend and log senders; `mime.QEncoding` | Yes |
| RP82-3 | every writer of `storage_used_bytes` is in `internal/usage` (`floor.go`, `pool.go`, `ledger_tx.go`, the reconciles in `quota_manager.go`); `moveUsed` locks root then member; the erasure's `poolRootRelease` locks the root first | Yes. Prod drift is 0. RB11-1 (the previous build moves no counter during the first overlap) is accepted: it applies to the #1085 overlap only |
| RP82-4 | `InheritTighten` (a child row cannot turn off a platform's sealing flag, 409) and `InheritDefault`; `TestFlags_TheTreeInheritanceOfEveryFlag` pins the classification | Yes; the open question on `standard_sealed_default` is the owner's |
| The tree | depth 2 only under an `app`; caps checked as min(cap, pool) at the reservation and at a guarded settle; `PATCH /platform/tenants/{id}` scoped by `DescendantT2` | Yes |
| Residency | bucket create on all three entry points (S3, management, dashboard) through `createBucket`; a create naming no region gets the first enabled region inside; a pinned bucket's every write goes through `PutOn(regionDriver)` (`object_write_shared.go`), so class, tier, PUBLIC and the chunk path (pinning disables chunking) cannot leave the region; the mirror and parity legs only inside; no write path changes a bucket's region after create | Yes, except the system bucket (RP86-1) and two label/backfill paths (RP86-2/3) |
| The edges | suspension down `managed`/`app` only (the grandparent for depth 2); the cascade detaches a sponsored child (D-STRIPE-55) before `EraseRows` would refuse it | Yes |

## Findings

| ID | Sev | Where | What | Fix |
|---|---|---|---|---|
| **RP86-1** | **P3** (latent: no residency set, no export ever made) | `internal/api/account_export.go` `ensureSystemBucket` (`region := drivers.IDriveDefaultRegion(...)`); `residency.go` `treeBucketRegions` (no system-bucket filter) | **The GDPR export ignores residency, in both directions.** Proven by a throwaway test:<br>**(a)** an EU tenant's `_exports` bucket is created in `us-central-1`. The export is the account's whole metadata (object names, keys list, audit trail, the members), and it sits at rest in the US for its 7 days. That is the one byte path the residency promise ("where the tenant's bytes may be stored at rest") does not cover.<br>**(b)** once a tenant has exported, an admin can never give it a residency. The hidden bucket row outlives its object (retention deletes the object, the row stays), and `setTenantResidency` answers 409 `buckets_outside_residency` "move or delete it first" for a bucket the customer can neither see nor delete. | Tests first:<br>• `TestResidency_TheExportStaysInside`: an EU tenant's export lands in an EU region, and `firstRegionInside` none = the export fails with a note (`no_region_inside`), never written outside;<br>• `TestResidency_TheSystemBucketNeverBlocksTheSet`: a tenant whose `_exports` holds no object takes a residency, and its system bucket row moves to the first region inside in the same transaction; one holding a live export is 409 until it expires.<br>Then `ensureSystemBucket` reads the tenant's residency, and `treeBucketRegions` treats `_`-prefixed system buckets by those rules. |
| RP86-2 | P4 | `management_routes.go` `handleMgmtSetBucketResidency` (`PUT /api/v1/manage/buckets/{name}/residency`) | The legacy route still writes the bucket's `data_residency` LABEL to any of `us` / `eu` (no `uk`, although RP78-4 made London `uk`), whatever the bucket's region. The dashboard and the export's buckets section show that label. An EU-region bucket can be labelled `us` and a US bucket `eu`, a self-made lie beside the real `tenants.residency`. | The label is derived from the region only (`drivers.DataResidency`, as create does). The route answers 409 `residency_is_the_region` when the asked label is not the region's, and the docs mark it deprecated. `TestBucketResidencyLabelIsTheRegions`. |
| RP86-3 | P4 | `auth.BackfillBuckets` (boot) | A head row with no bucket row (a DeleteBucket racing a PUT's commit) gets its bucket row back at the next boot with **no region**, i.e. the US default, under any residency. The next write to it lands outside. | Backfill with the region of the head row's recorded backend (`idrive-<region>` → its region; the primary → the default), or skip a tenant with a residency and log it. |
| RP86-4 | P4 | `account.ScheduleSignIn` / the runner | The "member of nothing" check runs only when the erasure is scheduled. A sign-in that accepts an invitation during its 30-day grace is a member on the due date, and the runner's `EraseRows(user, "")` deletes it with the membership (the ByUser rule) and no word to the team. | On the due date the runner re-checks a tenant-less erasure under the user's lock. A user who is a member again gets the schedule cleared with `account.deletion_cancelled` (`reason: became_member`), never an erasure. `TestAccount_ASignInThatRejoinedIsNotErased`. |
| RP86-5 | P4 | `deploy/monitoring/tests/vaultaire-jobs_test.yml` | The repo's own promtool test of `vaultaire-jobs.yml` failed: `PeriodicJobStale`'s expected description had been stale since #981 (WP-EDGE-1 A added `edge_admit_reaper` and `blob_copies` to the rule's text, not to the test). CI never runs promtool, so nothing noticed. | **Fixed in this PR** (the expected text; SUCCESS on the box). Worth one CI step that runs the pinned promtool over every `tests/*_test.yml` (a download, seconds). |

## Ops done by this review

- **`vaultaire-quota.yml` installed (RB11-8) and `vaultaire-jobs.yml` re-installed** on SLC around 12:10 UTC, after promtool tests on the box (rules beside their tests in one directory). Hashes `582c81dd…` and `bd98b8ae…` = repo; the old jobs file is at `/root/vaultaire-jobs.yml.bak-rp86`. Prometheus reloaded (the slots untouched); 129 rules load.
- Live from then: `PoolUsedDrift`, and WP-VAULT-2's `VaultParityScrubStale`, `VaultParityScrubBacklog`, `VaultTapeAuditFailed`, `VaultTapeAuditStale`, `VaultParityRestoreStuck`. None fires.
- Recorded in `deploy/monitoring/README.md`.

## Seen on the box, not this WP's (for the owner)

- **`BackendVendorQuota80` / `90` firing for `blomp`:** the vendor reports 62.4 GiB used against a 40 GiB quota. Blomp refuses writes past its quota, which is the likely reason **`BackupSecondOffboxStale`** fires (no second off-box dump copy in over 26 h since 05:00 UTC). The first off-box leg (Sync) is not alerting. The likely fill is the 2026-10-21 bench uploads in the bench bucket. Clear them, or move the backup leg.
- `RoutingTruthMissing{backend="idrive"}` (since the 01:28 reboot's first run): the 501 head rows of the 2026-10-04 iDrive account that CLAUDE.md says stay 503 until deleted.
- `BackupDumpPlaintext` (expected until F-36) and `BackendPlanUnknown` ×3 (plan rows not entered).

## The decisions and residuals, re-checked

- **D-STRIPE-43 (migration 120 whole with Part 0) and D-STRIPE-44:** agreed. The `BEFORE INSERT` trigger is the right answer for a NOT NULL column the previous build does not write.
- **D-STRIPE-45:** agreed (better than the review's text).
- **D-STRIPE-46…58:** agreed.
  - D-STRIPE-52 (a leg's jurisdiction comes from the region it stores in; an unknown leg is outside every residency) is the conservative reading.
  - D-STRIPE-55 (detach = a pool of one, the free tier's limit) keeps the sponsored edge reversible.
- **RB11-1…7:** accepted. **RB11-8:** done by this review.
- **Open question 1 (`standard_sealed_default` per tenant):** the owner's. The review's view: keep it global until F-14 (the master key). Turning it on is refused without a master key (`flags.Refusal`), so a per-tenant row buys nothing before the ceremony. The inheritance is already built for the day it matters.

## For Prompt 54 (WP-KEY-1), Part 0 — in this order

1. **RP86-1 (P3):** the export inside the residency; the system bucket never blocks the admin's set.
2. **RP86-2 / RP86-3 / RP86-4 (P4):** the label is the region's; the boot backfill keeps the region; a rejoined sign-in is not erased.
3. **RP86-5's CI step (P4):** promtool over `deploy/monitoring/tests/` in CI.

**And three corrections to PR A's design** (found while reading the draft against this code):
- **The audit actor.** `audit_logs.user_id` and `performed_by` are UUID columns, and `audit.Record` writes a non-UUID actor as NULL. A management key's principal `mk:<id>` would therefore audit as "the system did it" (the reading R10-3d's `actor_erased` exists to avoid). Migration 121 adds `audit_logs.principal TEXT` (and the archive's): the key's `mk:<id>`, a member's user id. The audit page and its export show it.
- **The key's maker** (RP84-1 / RP84-4's lesson, applied before the table exists):
  - `management_keys.created_by` must NOT `REFERENCES users ON DELETE CASCADE` (a member's erasure would revoke the platform's server key);
  - the erasure takes the table ByTenant and hands `created_by` over (or nulls it) ByUser;
  - the member removal's `keys_made` lists management keys too, with the same `revoke_keys`.
- **No owner act by key.** The draft already keeps `/api/v1/user`, the deletion, the export and billing JWT-only. Add the member routes to that list: a management key must never invite, remove, or change a role. A leaked server key must not be able to make its holder a member.
