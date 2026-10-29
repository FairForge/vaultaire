# RP82 — the post-merge review of WP-STRIPE-3 (platform tenants, the pool, child keys, the cascade, RP80's fixes)

*Reviewed 2026-10-29 (session RP82). Read adversarially:*
- *the PRs: #1055 (Part 0: RP80-1…7 + RB8-2), #1056 (PR A: the platform and its children, migration 117), #1057 (PR B: the pool), #1058 (PR C: child keys, STS, the cascade), #1059 (RB9-6, the Worker test flake) and the closeout #1060 (`docs/reviews/WP-STRIPE-3.md`, `docs/ACCOUNTS.md`);*
- *against the note's decisions D-STRIPE-18…31, residuals RB9-1…7, the account model, and the prod proof.*

*Prod state read 2026-10-29 ~19:30 UTC (build `4fda00aa`):*
- *one platform, tenant zero, and one child (`tenant-9f168de3e4ef8be3`, "Prompt 51 proof"), holding 3,000,000 bytes;*
- *the child's one key is revoked;*
- *tenant zero's quota row is 1 TiB with 106.5 GB used, tier `starter`;*
- ***neither tenant has `tenant_floor_quotas` rows** (no house bought), so RP82-1 below is latent on prod today.*

## Verdict

**HOLDS at P2.** Part 0 closed every RP80 finding: a sandbox is metered (5 GiB a month) and paced, never billed; test presigns are capped at an hour; the parent's suspension reaches the sandbox; the event cursor has its settle horizon; long polls are bounded.

The platform is the right primitive:
- a child is a tenant, so every per-tenant guard holds;
- the gate answers 404, never 403;
- creation is one transaction;
- the pool's two-statement walk (D-STRIPE-22) is the correct answer to READ COMMITTED's snapshot, and its comment says why;
- a child key is never wider by omission (D-STRIPE-23);
- the cascade erases each child through the ordinary path.

The prod proof showed isolation (tenant zero's bucket is 404 to the child's key) and the pool's +3,000,000.

The account model (`docs/ACCOUNTS.md`) is the right cut: the pool pays, the tenant isolates, an agent is a principal and never an account.

Two P2s, both latent today and both opened by the account model's next WPs:
- **RP82-1:** the pool checks the total only, so a platform's children skip the Standard / Vault floor split.
- **RP82-2:** a child's downloads have no allowance (RB9-2, D-STRIPE-31: already Prompt 52's Part 0).

Then two P3s and three P4s.

## What was checked and holds

| Area | Checked | Holds |
|---|---|---|
| RP80-1 | `metered` and the bandwidth record no longer skip a sandbox; `usage.SandboxEgressAllowance` (5 GiB); the presign cap by resolved tenant (STS included, D-STRIPE-27); `modes.ErrSandboxNeverPublic` and the `x-amz-acl` refusal | Yes |
| RP80-2…7, RB8-2 | the parent join in `isTenantSuspended`; the 2 s horizon and listen dedup; the 4-wait semaphore (per process, D-STRIPE-28); `?mode=test`; the settle host check; the first-boot rule | Yes |
| The platform gate | `PUT …/platform` is admin-only and audited, never for a child or sandbox, no revoke while children remain; `/api/v1/platform/*` answers 404 to non-platforms; every route logged (`TestPlatformRoutes_EveryRouteLoggedAndGated`) | Yes |
| Children | `CreateChildTenant` locks the parent `FOR SHARE`, writes tenants + the limit-0 quota row with the parent's tier + an audit row; the partial unique `external_id`; no child of a child; suspended with its platform | Yes |
| The pool | `lockPoolOwner` locks the owner's row first, then `poolUsed` sums after the lock; guarded `ApplyLedgerTx` locks the owner before its own row; every reservation path goes through `usage.ReserveTx` | Yes for the total. I walked the callers (`multipart_reservation.go`, `edge_codec_multipart.go`, `quota_accounting.go`): each transaction makes one ledger change, last, so no path takes a child's row before its owner's. The floors are RP82-1. |
| Child keys | `CapScope` (permissions, bucket scope, IP list, expiry, bypass-governance); defaults are the caller's; 50 per child; kept out of the user's in-memory list and the plan's key cap; STS needs `parent_key_id` | Yes (RP82-6 is a WP-KEY-1 note) |
| The cascade | stage b5: `ScheduleChildrenNow`, each child through `eraseTenant` with its own `account.erased`; a deferring child defers the platform; `EraseRows` refuses a platform with a child row (`ErrChildrenRemain`) | Yes |

## Findings

| ID | Sev | Where | What | Fix |
|---|---|---|---|---|
| **RP82-1** | **P2** | `internal/usage/floor.go` `ReserveTx` (and `ApplyLedgerTx`'s floor statements) | **A platform's children skip its floor split.** `lockPool` checks the pool's total against the owner's limit, but the floor check reads the CHILD's `tenant_floor_quotas`, and a child has none (RB9-1). So every child write is unconstrained by floor. A platform that bought 1 TB Standard + 10 TB Vault lets its children put 11 TB on the Standard floor: hot storage at the Vault price ($2 against $4.49, and Standard's COGS is above $2 on iDrive at list). The parent's own floor rows also never see its children's bytes. Latent today (tenant zero has no floors, no outside platform exists). It opens with the first platform that holds a house, and broadly with WP-APP-1's self-serve apps (F-43 recommends any paying account). | Test first: `TestPool_ChildFloorsDrawFromTheOwnersFloors` (owner floors 10 KiB standard + 100 KiB vault; a child's 12 KiB Standard PUT is 403 while a 12 KiB Vault PUT succeeds; the owner's own Standard PUT counts against the same 10 KiB). Then floors are pooled exactly like the total: a child gets counting floor rows (limit 0) whenever its owner has floors (created with the child, and by `SetHouse` for existing children); the check sums the pool's rows for that floor against the OWNER's floor limit; lock order owner total → child total → owner floor → child floor (`TestLedger_LockOrder_*` extended). With WP-ACCT-1's `pool_root_id` the same function resolves the root. |
| **RP82-2** | **P2** | `usage.EgressAllowance` (a child's limit-0 row answers 0 = "never over") | **A child's downloads have no allowance** (RB9-2). Confirmed: same class as RP80-1. A child is never paced, so a platform's children are free unmetered hosting. Latent (one proof child, no outside platform). Decided by D-STRIPE-31 and already Prompt 52's Part 0. | As D-STRIPE-31: the pool root's allowance, the month's egress summed over the pool, pacing on the pool's position, through one function that resolves the root. Also make "0" mean zero for a child, never "unlimited" (`TestPool_ChildEgressSpendsThePoolsAllowance`, `TestEgress_ZeroAllowanceIsNotUnlimitedForAChild`). |
| RP82-3 | P3 | `internal/usage/pool.go` `poolUsed` | The pool's sum runs over every child's quota row on every write, under the owner's row lock, so writes are serialised behind a sum that grows with the number of children. That is fine at 1 child and poor at 10,000 (WP-APP-1's end users at depth 2 multiply it). | With WP-ACCT-1's `pool_root_id`: a denormalised `pool_used_bytes` on the root's row, moved in the same statement as the member's counter. The reconcile job checks it equals the sum, and a mismatch is a metric, never a silent fix. |
| RP82-4 | P3 | `internal/flags` (per-tenant rows); children | **Children inherit none of the parent's per-tenant flags.** That includes `standard_sealed_default`: a platform that turned on sealing by default stores its customers' Standard objects unsealed, and `edge_ingest` / `edge_mirror` / `parallel_get` don't follow either. The mode a customer chose for their own data should not silently weaken one level down. | WP-ACCT-1 decides inheritance once, in the flags service: privacy and safety flags (`standard_sealed_default`, any future sealing default) inherit down the tree and may only be tightened; performance flags inherit as defaults. `TestFlags_ChildInheritsSealingDefault`. |
| RP82-5 | P4 | `internal/auth/platform_keys.go` `GenerateChildKey` | It reads `deletion_scheduled_at` and never uses it. The grace blocks nothing (D-16), so minting during it is consistent. | Drop the read (or name the rule in a comment). |
| RP82-6 | P4 | `auth.CapScope` | A scoped calling credential's `BucketScope` names ITS OWN tenant's buckets. Capping a child key's bucket scope by it compares names across two namespaces. Harmless with JWT callers (no scope); wrong once management keys mint child keys (D-STRIPE-29). | WP-KEY-1: a management key's S3 ceiling for what it mints is its own field, distinct from the bucket scope of its own S3 use. |
| RP82-7 | P4 | admin views (RB9-4) | Admin totals and lists count sandboxes and children as tenants. | WP-ACCT-1's typed edges give every admin total a kind filter (live / sandbox / child). |

## The residuals and decisions, re-checked

- **D-STRIPE-18…28:** agreed. D-STRIPE-22's reasoning is the right reading of Postgres, and D-STRIPE-26 (the child's id on every row, the pool as a read-time sum) keeps the request log and disclosure per tenant.
- **D-STRIPE-29 (management keys), 30 (tenant residency), 31 (pool egress):** agreed, and the order (31 first, as a correctness gap) is right. D-STRIPE-30 also closes RP76-1 and must cover the parity leg as it says.
- **RB9-1:** is RP82-1. **RB9-2:** is RP82-2. **RB9-3 (the Children card), RB9-5, RB9-7:** accepted. **RB9-4:** RP82-7. **RB9-6:** fixed (#1059).
- **The prod proof by operator SQL:** acceptable as written. The owner authorised it, it ran in the code's own transactions with audit rows saying so, and the key was revoked at the end. The admin route and the platform API with a JWT still need the owner's TOTP to be proven end to end; carry that to the next session with the owner present.

## For Prompt 52, Part 0 — in this order

1. **RP82-2 / D-STRIPE-31:** the pool's egress (already Part 0 in the closeout's draft); "0" is never unlimited for a child.
2. **RP82-1:** floors pooled like the total.
3. **RP82-5:** the P4 cleanup.

RP82-3 (the pool counter), RP82-4 (flag inheritance) and RP82-7 (admin kinds) are WP-ACCT-1's (Prompt 53), and RP82-6 is WP-KEY-1's (Prompt 54).
