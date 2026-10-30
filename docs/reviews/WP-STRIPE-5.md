# WP-STRIPE-5 — the account tree: `pool_root_id`, `child_kind`, caps inside the pool, residency (Stage 1a Prompt 53), after Part 0 = RP84's and RP85's findings and RP82-3/4/7

*Plan § WP-STRIPE, the **WP-ACCT-1** row (`docs/ACCOUNTS.md`; D-STRIPE-30). It asks for:*
- *one tree with typed edges (`tenants.child_kind`: `managed` / `app` / `sponsored`);*
- *`tenants.pool_root_id`, denormalised, the pool's one resolver at any depth; depth 2 only under an `app`;*
- *caps inside the pool: a child's storage and egress caps are min(cap, pool);*
- *`tenants.residency`: set at creation, inherited, never widened, enforced at bucket create and on the mirror and parity legs;*
- *suspension down `managed`/`app` edges only; erasure cascading down them and DETACHING a `sponsored` child.*

*Part 0 is the post-merge reviews RP84 (`docs/reviews/WP-STRIPE-4-REVIEW.md`, WP-TEAM-1) and RP85 (`docs/reviews/STAGE1-CLOSEOUT-REVIEW.md`), then RP82-3/4/7 (`docs/reviews/WP-STRIPE-3-REVIEW.md`).*

*Prompt: `docs/reviews/WP-STRIPE-PROMPTS.md` § Stage 1a Prompt 53 (the session ran the Opus edition). Session 2026-10-29/30. **Migration 120** (`120_account_tree.sql`, landed whole with PR 0). Next: **121**. The post-merge review is **RP86**.*

Contract lines cited (`docs/ARCHITECTURE_CONTRACT.md`):
- **4** (keys only through the one hierarchy): keys are still minted through `persistAPIKey`. A key now goes with its **tenant** at erasure, and a member's own erasure hands the keys it made on another tenant to that tenant's owner. No key is ever made on no tenant.
- **5** (convergence within a space only): untouched. A tree is a pool (who pays), never a merged space: every tenant of a tree keeps its own keys, dedup scope and crypto-shred.
- **6** (the chunker is frozen): untouched.

## What was built

| PR | What | Tests (red first) |
|---|---|---|
| **#1085** Part 0 + migration 120 | **RP84-1 (P2):** a `ByTenant` `api_keys` rule; a member's own erasure hands the keys it made on other tenants to the tenant's owner (`keyMakerHandover`, a `key.maker_erased` row on the tenant); the schema walk requires a tenant rule on every `tenant_id` table (it caught `account_exports`).<br>**RP84-2/6:** no role on no tenant; `memberRole()` defaults to `""`; the accept route sits outside `requireMember`; both key generators refuse `''` (`ErrNoTenant`); the invitee's cached tenant is gone; a login with no membership mints on no tenant.<br>**RP84-3:** every sender refuses CR/LF in From/To/Subject (`ErrHeaderValue`), and the subject is RFC 2047-encoded; `CleanTenantName` at registration; ≤ 20 pending invitations and ≤ 50 invitation mails a day per tenant (429 `invitation_rate_limited`).<br>**RP84-4:** the removal answers `keys_made`, and `revoke_keys` revokes them and their STS tokens in its transaction; the dashboard lists them with the box ticked.<br>**RP84-5:** one pending invitation per address (a new one replaces it); the accept answers the real role; `lower()`.<br>**RP85-1/2:** the idempotency key is `<user id>:<key>`; a replay sends `Idempotent-Replayed: true` and `secret_redacted: true`.<br>**RP82-3:** `pool_used_bytes` on the root, moved in the member's statement (`usage.moveUsed`) by every ledger writer; `vaultaire_pool_used_drift_bytes{root}` + `PoolUsedDrift`; the operator's reseed.<br>**RP82-4:** flag inheritance in `internal/flags` (`InheritTighten`, `InheritDefault`; 409 `flag_only_tightens`).<br>**RP82-7:** `?kind=` on every admin total and list.<br>**Migration 120:** the WP's every column (below). | `TestErase_TheTeamsErasureTakesEveryKeyOfTheTenant`, `TestErase_AMembersErasureKeepsTheTeamsKeys`, `TestMembers_ATenantlessSignInIsNoOwner`, `TestMembers_KeyGeneratorsRefuseNoTenant`, `TestEmail_HeaderValuesRefuseCRLF`, `TestRegister_TenantNameHasNoControlCharacters`, `TestMembers_InvitationsAreCapped`, `TestMembers_RemovalNamesTheKeysTheMemberMade`, `TestMembers_ANewInvitationReplacesThePendingOne`, `TestIdempotency_KeyedByTheActingMember`, `TestPool_UsedCounterEqualsTheSum`, `TestPoolDrift_TheGaugeNamesTheDriftingRoot`, `TestFlags_ChildInheritsSealingDefault`, `TestFlags_APerformanceFlagInheritsAsADefault`, `TestFlags_TheTreeInheritanceOfEveryFlag`, `TestAdminFlags_AChildCannotTurnOffItsPlatformsSealing`, `TestAdminTenants_KindFilter` |
| **#1086** PR A the tree | `child_kind` (`managed` default, `app`); an app's managed children at depth 2 (`parent_id` = the app; anything deeper is 400 `child_of_child`; `sponsored` is 400 `invalid_kind` here); `pool_root_id` written at creation; `usage` the one resolver; the platform's reach over its tree (`auth.DescendantT1…3`); caps (`storage_cap_bytes` = min(cap, pool) at the reservation and at a guarded settle; `egress_cap_bytes` paces the member on its own bucket); `PATCH /api/v1/platform/tenants/{id}`; the erasure cascade one level deeper. | `TestTree_PoolRootIsTheOneResolver`, `TestTree_PoolAtDepthTwo`, `TestTree_AnAppsChildrenAreErasedWithIt`, `TestTree_ChildCapInsideThePool` (storage, egress), `TestPool_AGuardedSettleHoldsAChildToItsCap` |
| **#1087** PR B residency | `tenants.residency` set at creation (a child inherits its parent's, another is 400 `residency_widens`), changed only by an admin while every bucket of the tree complies (`PUT /api/v1/admin/tenants/{id}/residency`, 409 `buckets_outside_residency`); bucket create inside the jurisdiction (S3 400 `InvalidLocationConstraint`, management `region_outside_residency`, the dashboard's message), the default region the first inside it; the mirror leg and the parity leg only inside it. | `TestResidency_BucketOutsideRefused`, `TestResidency_AdminSetsItOnlyWhenEveryBucketComplies`, `TestResidency_ChildNarrowsNeverWidens`, `TestResidency_MirrorAndParityLegsStayInside` |
| **#1088** PR C the edges | Suspension down `managed`/`app` edges only (the grandparent read for depth 2); the cascade skips a sponsored child and DETACHES it (`account.DetachSponsored`); the platform's reach excludes sponsored children; RB10-1 — a member leaves on its own (`DELETE /api/v1/manage/members/{self}`, `member.left`), and a sign-in with no tenant and no membership schedules its own erasure (`DELETE /api/v1/user/signin`, 409 `still_a_member`), the user rows only. | `TestTree_SponsoredNeverSuspendedOrErasedWithTheSponsor`, `TestMembers_ALeaveRemovesTheMembershipOnly`, `TestAccount_TenantlessSignInErasesTheUserOnly` |
| **PR D** this note | STATUS, the plan row, CLAUDE.md, `docs/API.md` § Platform and § Members, `docs/ACCOUNTS.md`, Prompt 54 in both editions. | — |

**Migration 120:**
- `tenants.child_kind` (CHECK `managed|app|sponsored`; today's children backfilled `managed`);
- `tenants.pool_root_id` (backfilled `COALESCE(parent_tenant_id, id)`, NOT NULL, indexed);
- `tenants.residency` (CHECK `eu|uk|us`);
- the `tenants_tree_defaults` BEFORE INSERT trigger;
- `tenant_quotas.pool_used_bytes` (backfilled once, in a DO block, when the column is added);
- `tenant_quotas.egress_cap_bytes`;
- `idx_tenant_invitations_one_pending` (deduplicated first).

## Step 0 answers

- **Were RP84's and RP85's files on main?** Yes: #1082 merged 2026-10-30 03:13 UTC, before the worktree was made.
- **Which reads used the pool's old expression?** `usage.PoolRoot`, `lockPoolOwner`, `poolUsed`, `poolFloorUsed`, `PoolBilledBytes`, `MonthEgressBytes`, `SetHouseTx`'s two child statements, and the bandwidth alerter's candidates (`COALESCE(t.parent_tenant_id, b.tenant_id)`). All of them now read `pool_root_id` inside `internal/usage` (the alerter through `usage.PoolRootsWithEgressSince`). On origin/main `pool.go` held three matches of the old expression, so the structure test fails there.
- **Is `standard_sealed_default` per tenant?** No. It is global only (WP-EDGE-4 PR C: a tenant row is refused). See open question 1.
- **What does a depth-2 erasure need?** Stage b5 ran only for a non-child. An app's erasure would have been refused for ever (`ErrChildrenRemain`), so b5 now runs for any tenant with children.

## Decisions (D-STRIPE-43…)

- **D-STRIPE-43: migration 120 lands whole with Part 0.** The prompt places the tree's columns in PR A and the residency in PR B, but "never change a migration already on main" rules that out once PR 0 merges. Every column is additive and read by nothing until its PR. A `BEFORE INSERT` trigger fills `pool_root_id` and `child_kind` for any writer that leaves them out (the previous build during the overlap, tests, tools), so `NOT NULL` holds from the first deploy.
- **D-STRIPE-44: the counter is kept for every root, and the pooled check reads it for pooled tenants only.** A pool of one keeps its own counter in the same statement (one row), so a tenant later granted the platform flag needs no seeding. A tenant in no tree still checks its own row, as before. The backfill runs once (the DO block). A drift is never rewritten by the code: the reconciles move a counter by its members' corrections, and the operator's reseed (`?pool=reseed`) is the one rewrite, audited with the drift it removed.
- **D-STRIPE-45: the day's invitation count is read from the `member.invited` audit rows, written in the invitation's own transaction.** The prompt says "counted from `tenant_invitations` rows", but revoked and replaced invitations are deleted rows. Counting those would let a revoke-and-invite loop send without bound. The audit rows are never deleted (except at erasure). The pending count does come from `tenant_invitations`. Both are read under one per-tenant advisory lock, so both slots agree.
- **D-STRIPE-46: a JWT on `''` has no role for every user, operators included.** The operator routes never read the role (`requireAdmin` reads `users.role`).
- **D-STRIPE-47: the heir of a member's key** is the key tenant's owner member, else the owner of its pool root (a child's platform), else its registering user. With no heir, the key goes with the user: only a tenant with no owner at all can be in that case.
- **D-STRIPE-48: the flag classification.**
  - `InheritTighten`: `edge_codec` (the edge seals what it chunks) and `standard_sealed_default` (declared; inert while global only).
  - `InheritDefault`: `edge_ingest`, `edge_mirror`, `parallel_get`, `data_plane_slc` and `edge_rs`, the edge's performance flags.
  - Every other flag stays per tenant. `TestFlags_TheTreeInheritanceOfEveryFlag` pins the list.
  - The tree is loaded with the table every 15 s and refreshed when a child is made.
- **D-STRIPE-49: depth 2 is made by the platform.** `POST /api/v1/platform/tenants {parent_id: <app>}`. An app child has no members and no management key yet (WP-KEY-1), so it cannot call the API itself. The platform reaches its whole tree: read, keys, STS, caps, erasure.
- **D-STRIPE-50: a child's storage cap is its own `storage_limit_bytes` (> 0), checked at the reservation and at a guarded settle.** Its egress cap paces it at `cfg.Rate(cap)` through its own token bucket while the pool has room. Past the pool's allowance, the pool's bucket paces it as before.
- **D-STRIPE-51: residency is stored on every tenant, never walked at read.** A child copies its parent's at creation. Each residency is one jurisdiction, so "narrowing" means keeping it, and naming another is `residency_widens`.
- **D-STRIPE-52: a leg's jurisdiction comes from the region it stores in** (`legRegion`: `idrive` = IDRIVE_REGION, `idrive-<region>`, `lyve` = LYVE_REGION). Every other leg (`sync`, `permafrost`, `blomp`, `sia`, `r2`) is outside every residency. With no leg inside, there is no mirror and no parity copy, and never one outside: a note each run, not a refusal of the write.
- **D-STRIPE-53: a create that names no region under a residency** gets the deployment's default region when it is inside, else the first enabled region inside it by id. With none, it is the same 400 as a region outside.
- **D-STRIPE-54: an admin's residency applies to the tenant's whole tree.** It is refused while any bucket of the tree is outside it, or while a tenant under it holds another residency (409 `children_outside_residency`).
- **D-STRIPE-55: a detached sponsored child becomes a pool of one:**
  - its bytes leave the root's counter;
  - a cap of 0 becomes the free tier's limit, and the egress cap is cleared;
  - the pool's counting floor rows (limit 0) go;
  - it gets `account.sponsor_detached`;
  - its data, keys and sign-in are untouched.
- **D-STRIPE-56: the platform's reach (`DescendantT*`) excludes sponsored children.** A sponsor never mints a sponsored child's keys, reads it or erases it.
- **D-STRIPE-57: self-leave uses the removal route** (any member but the owner, `member.left`). The erasure of a sign-in with no tenant is its own route, `DELETE /api/v1/user/signin`, outside `requireMember`. It refuses anyone who owns or is a member of a tenant.
- **D-STRIPE-58: the admin kind filter defaults to `live`; the support search is unfiltered** (a search by id must find a child).

## Residuals (RB11-n)

- **RB11-1:** during a deploy overlap the previous build moves no pool counter. A pooled tenant's writes in that window drift its counter. The gauge shows it; fix with the reseed.
- **RB11-2:** another slot's flag cache learns a new child's parent at its next 15 s refresh. Until then, the child of a sealing platform reads the sealing flag from its own row and the global row only.
- **RB11-3:** a sign-in with no tenant has no self-serve cancel of its scheduled erasure (an operator clears it), and the dashboard has no "leave" or "delete this sign-in" button. Both are API only.
- **RB11-4:** a member's egress-cap counter is per process, loaded from its recorded rows plus each slot's live bytes, like the pool's own counter.
- **RB11-5:** the idempotency key's new prefix is unknown to the previous build. A retry that lands on the other slot during the switch is not recognised as a replay.
- **RB11-6:** a sponsored edge is made only by SQL until WP-FAMILY-1. The detach and the suspension rule are tested on such a row.
- **RB11-7:** `platformChild.residency` now names the child's own residency when it has one, else the default region's label as before.
- **RB11-8:** the Prometheus rule file `vaultaire-quota.yml` is new. It is not on the box until the operator installs it (`deploy/monitoring/README.md`), and its promtool test runs on the box.

## Open questions

1. **RP82-4's example cannot be built as written.** `standard_sealed_default` is global only (WP-EDGE-4 PR C, reviewed RP76: a tenant row is 400). So "a platform that turned on sealing by default stores its customers' Standard objects unsealed" cannot happen today: when it is on, it is on for every tenant. The inheritance mechanism is built and tested on a per-tenant sealing flag (`edge_codec` in production; a test flag in `internal/flags`), and `standard_sealed_default` is registered `InheritTighten` for the day it is not global. **Owner:** should a platform be able to turn sealing on for its own pool before the global flip, which would make `standard_sealed_default` per tenant and reverse WP-EDGE-4's "tenant row 400"?

## The prod proof

- **Migration 120** applies before the switch, and the trigger and backfill leave no NULL `pool_root_id`.
- **Read-only, done 2026-10-30 after #1085 deployed (`6420322b`):**
  - 5 tenants, none with a NULL `pool_root_id`;
  - tenant zero's one child: `child_kind = managed`, `pool_root_id` = its parent, the platform;
  - the platform root's `pool_used_bytes` equals its pool's sum;
  - 0 pending invitations;
  - no `vaultaire_pool_used_drift_bytes` series on the active slot's `/metrics`.

  PRs A–C deployed after it (`cf68f521`, `4b1146ea`, `9e2a5095`).
- **Owed, needs the owner's TOTP** (`POST /auth/login` asks for the code since #956, and there is no management key until WP-KEY-1): an EU child of tenant zero (`POST /api/v1/platform/tenants {label, residency: "eu"}`) refused a us-central-1 bucket. Its key's `CreateBucket` with `LocationConstraint us-central-1` should answer 400 `InvalidLocationConstraint`.
- The WP-TEAM-1 proof, owed since Prompt 52, can run now that Part 0 is deployed.

## Post-merge review

**RP86, 2026-10-30 (`docs/reviews/WP-STRIPE-5-REVIEW.md`): HOLDS at P3**, every finding latent on prod (no residency set, no export ever made):
- RP86-1 (P3): the `_exports` system bucket is made in the default region whatever the residency, and its row then blocks the admin's residency set forever. Proven by test.
- P4s: RP86-2 (the legacy bucket-label route), RP86-3 (the boot bucket backfill has no region), RP86-4 (a sign-in that rejoins during its grace is still erased).
- RP86-5 (P4, the stale jobs rule test): fixed in the review.
- RB11-8 is done: `vaultaire-quota.yml` and the re-installed `vaultaire-jobs.yml` are on the box, hashes = repo.
- Prompt 54's Part 0 is filled, with three corrections to its PR A.
