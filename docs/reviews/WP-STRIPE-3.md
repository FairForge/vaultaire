# WP-STRIPE-3 — platform tenants, the Connect primitive (Stage 1a Prompt 51), after Part 0 = RP80's fixes

*Plan § WP-STRIPE, the **WP-PLAT-1** row. It asks for:*
- *"a **platform credential** (a tenant with `platform = true`, granted by an admin) that creates tenants under itself (`POST /api/v1/platform/tenants` — no user row, no e-mail …)";*
- *"mints and revokes scoped keys and STS tokens for them … the scope capped by the platform key's own";*
- *"pools quota: `tenant_quotas` of a child draws from the parent's pool … one query at the quota check, presign-time where the edge admits";*
- *"Erasure of a child is the ordinary erasure; erasure of a parent cascades after the children."*

*Part 0 is the RP80 review's findings (`docs/reviews/WP-STRIPE-2-REVIEW.md`, #1054).*

*Prompt: `docs/reviews/WP-STRIPE-PROMPTS.md` § Stage 1a Prompt 51 (the session ran the Opus edition). Session 2026-10-29. **Migration 117** (`117_platform.sql`, PR A). Next: 118. The post-merge review is RP82.*

Contract lines cited (`docs/ARCHITECTURE_CONTRACT.md`):
- **4** (keys only through the one hierarchy): a child is a tenant. Its keys go through `persistAPIKey` (wrapped under the child's own tenant key once the server has a master key); its STS tokens are bounded by one of its keys; its erasure deletes its own `tenant_key_versions` (the crypto-shred). A platform never holds a child's primary pair: a child has none.
- **5** (convergence within a space only): untouched. A child is its own space; the pool is a ledger rule, not a dedup scope.
- **6** (the chunker is frozen): untouched.

## What was built

| PR | What | Tests (red first) |
|---|---|---|
| **#1055** Part 0 | **RP80-1:** a sandbox's downloads are metered under its own id against `usage.SandboxEgressAllowance` (5 GiB a month, a const) and paced past it; never billed. A test key's presigned link lives ≤ 3600 s. A sandbox bucket is never Public (`modes.ErrSandboxNeverPublic`; a create with `x-amz-acl: public-read` is 400 `InvalidRequest`).<br>**RP80-2:** the parent's suspension covers its sandbox (`isTenantSuspended` joins the parent; `GenerateTestKey` 403 `account_suspended`).<br>**RP80-3/4:** the event cursor's 2 s settle horizon; `stored listen` remembers its last 1,000 ids; ≤ 4 long polls per tenant (429 `too_many_listeners`).<br>**RB8-2:** `?mode=test` on the request log; the Developers page's Live / Test toggle.<br>**RP80-5…7:** a failed sandbox lookup is a 503 at placement; a settle from another host is 404; the first-boot account only with `STORAGE_MODE=local`, with a one-time password. | `TestSandbox_DownloadsArePacedNeverBilled`, `TestTestKey_PresignCappedAtOneHour`, `TestSandbox_NeverPublic`, `TestSandbox_ParentSuspendedRefusesTestKeys`, `TestEvents_AfterNeverSkipsALateCommit`, `TestEvents_LongPollsBoundedPerTenant`, `TestListen_NeverForwardsAnIDTwice`, `TestRequestLog_ModeTestReadsTheSandbox`, `TestDevelopers_TestModeShowsTheSandboxsRequests`, `TestSandbox_LookupErrorFailsClosed`, `TestEdgeMirror_SettleIsTheAdmittingHostsOnly`, `TestBootstrapTenant_LocalOnly` |
| **#1056** PR A, the platform and its children | Migration 117: `tenants.parent_tenant_id`, `platform`, `external_id`, `label`, `deletion_scheduled_at`; unique `(parent_tenant_id, external_id)`.<br>`PUT /api/v1/admin/tenants/{id}/platform` (admin; audited `admin.platform_granted` / `_revoked`; never a child or a sandbox; no revoke while children remain).<br>`/api/v1/platform/tenants` (POST, GET, GET {id}, DELETE {id}) behind requireJWT, the request log and a gate that answers 404 to any tenant that is not a platform.<br>`auth.CreateChildTenant` in one transaction (the parent `FOR SHARE`, the tenants row, the quota row at limit 0 with the parent's tier, the audit row); `account.ScheduleTenant`; a child is suspended with its platform; `tenant.created` events; the export's and the produce page's tenant section name the columns. | `TestPlatform_ANonPlatformTenantGets404`, `TestPlatform_CreatesATenantWithoutAUser`, `TestPlatform_ListAndReadChildren`, `TestPlatform_ParentSuspendedSuspendsTheChildren`, `TestPlatformRoutes_EveryRouteLoggedAndGated`, `TestExportTenantSection_NamesThePlatformColumns` |
| **#1057** PR B, the pool | `usage.ReserveTx` (every write path: PUT, parts, copy, direct upload, the edge admit) locks the pool owner's row, then sums the pool against the owner's limit; a guarded `ApplyLedgerTx` change the same; `usage.PoolBilledBytes`. | `TestPlatform_ChildQuotaDrawsFromTheParentsPool`, `TestPlatform_ASandboxIsNeverInThePool`, `TestPool_ReserveChecksThePoolNotTheChildsRow`, `TestPool_GuardChecksThePool` |
| **#1058** PR C, keys and the cascade | `POST/GET/DELETE /api/v1/platform/tenants/{id}/keys[/{key}]` (`auth.CapScope`, `GenerateChildKey`, 50 live per child, never in the platform user's in-memory list or plan cap), `POST …/sts`.<br>The cascade: the runner's stage b5, `EraseRows` refuses a platform while a child row remains and erases a child by its tenants row; a due child is erased alone. | `TestPlatform_ChildKeysNeverExceedTheParentsScope`, `TestPlatform_ChildSTSBoundedByTheChildsKey`, `TestPlatform_EraseParentCascades`, `TestPlatform_ADueChildIsErasedAlone` |

## Decisions (D-STRIPE-18…)

- **D-STRIPE-18 (deviation):** migration 117 adds a **fifth** column, `tenants.deletion_scheduled_at`, and two indexes (`idx_tenants_parent`, `idx_tenants_deletion_due`). The prompt names four columns, but a child has no `users` row to hold its 30-day date (`account.ScheduleTenant`).
- **D-STRIPE-19:** the platform routes take the **JWT only** (the Opus edition). The chain edition's "a key with the new `PlatformAdmin` permission" is not built: a SigV4-signed request belongs to the S3 API before any route (Architecture Decision 8), so a key-authenticated platform call needs an exemption to the gate (as `/api/v1/whoami` has) or a token exchange. That is open question 1.
  - The scope cap reads the calling credential's scope from the context (`auth.KeyScopeFromContext`). A JWT carries none: it is the account's own authority. `TestPlatform_ChildKeysNeverExceedTheParentsScope` sets a scoped credential on the context, which is the seam a key-authenticated caller would use.
- **D-STRIPE-20:** **a tenant has no residency of its own** (each bucket picks its region). The prompt's "the parent's region in metadata" names a column `tenants` does not have. A child's `residency` answer is the deployment's default bucket region's (`drivers.DataResidency(IDriveDefaultRegion)`), the same as the parent's, and nothing is stored. Open question 2.
- **D-STRIPE-21:** a child's quota row has limit 0 and the **parent's tier**. The free-tier bucket and key caps read the tier, and a child is the platform's customer, not a free account of its own.
- **D-STRIPE-22: the pool's walk is two statements.** The first resolves `COALESCE(parent_tenant_id, id)` and locks the owner's row (`FOR UPDATE OF o`). The second sums the pool after the lock.
  - A sum inside the locking statement keeps the snapshot from before the lock wait, so it would miss a reservation committed while it waited.
  - Every pool reservation takes the owner's lock first, so they serialise on it.
  - A guarded `ApplyLedgerTx` change takes the owner's lock **before** its own row, the reservation's order. That keeps a settle and a reservation of two children from waiting on each other in turn.
- **D-STRIPE-23:** a child's key row has `user_id` = the platform user who minted it (as a sandbox's test keys are the parent user's). It is kept out of that user's in-memory key list (`LoadFromDB`) and the free-tier key cap (`notChildKeySQL`). A child holds at most 50 live keys.
  - What a mint request leaves unset (permissions, buckets, IP allowlist, expiry) is the calling credential's, so a child key is never wider by omission.
- **D-STRIPE-24:** a child's STS token needs `parent_key_id` (a child has no primary to default to). The intersection is `GenerateSTSToken`'s, unchanged.
- **D-STRIPE-25: the cascade is stage b5,** after the sandbox's b4 and before the row erase.
  - `ScheduleChildrenNow` makes every child due now (an earlier date of its own is kept). Each child then runs through `eraseTenant` itself: objects, sweep, keys, rows, and its own `account.erased` row.
  - A child that defers defers the platform.
  - `EraseRows` refuses a platform while a child row remains (`ErrChildrenRemain`).
- **D-STRIPE-26:** a child's request-log, bandwidth and event rows carry the **child's** id, as the prompt says. The plan row's "metering rows carry the parent id at write time" is not followed. The pool is a read-time sum (`PoolBilledBytes`), and the parent's view is the list endpoint's per-child usage.
- **D-STRIPE-27 (Part 0):** the test-key presign cap is decided by the **tenant** the credential resolved to (`isSandbox`), after the signature. An STS token of a test key is held to it too. The answer is AWS's `AuthorizationQueryParametersError` with its own sentence.
- **D-STRIPE-28 (Part 0):** the long-poll semaphore is **per process**. During a slot switch a tenant can hold up to 8 waits across the two slots.

## Residuals (RB9-n)

- **RB9-1:** a child has no floor rows. A platform with a house has its floors charged by its own writes only; its children draw on the pool's total.
- **RB9-2: a child's egress has no allowance.** Its quota row's limit is 0, so `EgressAllowance` answers 0: never "over" and never paced. Its downloads are recorded under its id, but nothing applies the pool's allowance to children. This is the obvious follow-up (the pool's allowance against the pool's month).
- **RB9-3:** the parent dashboard's "Children" card (chain edition item 5) is not built. The list endpoint answers the per-child usage.
- **RB9-4 (RB8-3 widened):** admin views count children (and sandboxes) as tenants.
- **RB9-5:** no `PlatformAdmin` key permission (D-STRIPE-19).
- **RB9-6 (fixed, #1059):** the Worker's JS test `putTwo: a mirrored PUT the isolate budget cannot cover is sent unmirrored` failed CI on #1054 and #1057. On a loaded runner the first PUT's mirror fell 16 MiB behind and was cut inside the second's 200 ms wait, giving the buffer back. The test now holds the budget itself.
- **RB9-7:** a child whose erasure settles on a lagging backend records `user_id = ""` in its settle row. The walk is tenant-keyed, so nothing else reads it.

## The prod proof

All on prod, 2026-10-29 (UTC), after #1057 (the pool) went live (build `3ba1e2e0`, 18:03).

**How (owner-authorised, no TOTP).** The admin route needs an admin JWT, which needs the owner's TOTP. The owner authorised the operator to do it on the box instead, as Prompt 50's session did for the test key. One script (`psql` with the app's credentials, never printed) ran each step in **the code's own transaction**:
- `auth.SetPlatform`: tenant zero's row `FOR UPDATE`, `platform = true`, and an `admin.platform_granted` audit row whose metadata says "operator SQL, owner-authorised".
- `auth.CreateChildTenant`: the parent `FOR SHARE`, the child `tenant-9f168de3e4ef8be3` (label "Prompt 51 proof", `external_id` `proof-1`, `platform+…@platform.invalid`), its quota row at limit 0 with tenant zero's tier `starter`, and `platform.tenant_created`.
- `auth.GenerateChildKey`: a scoped key `VK0453DCEDAB45B29674DC` (CreateBucket, ListObjects, PutObject, GetObject, HeadObject, DeleteObject) whose secret was generated on the box into a 0600 file and bcrypt-hashed, and `platform.key_created`.

**Through `s3.stored.cloud`, with the child's key:**
- CreateBucket `plat-proof-2a1605` 200;
- a 3,000,000-byte PutObject 200 (ETag `8fb62310…`), the head row on `idrive`;
- GetObject: bytes identical;
- a ListObjectsV2 of tenant zero's `lightroom-backups` is **404**: a child's key sees only its own tenant.

**The pool:**

| | Before | After |
|---|---|---|
| child's `storage_used_bytes` | — | **3,000,000** |
| tenant zero's own counter | 106,527,633,904 | unchanged |
| the pool's sum (tenant zero + children) | 106,527,633,904 | **106,530,633,904** (+3,000,000) |
| the pool's limit (tenant zero's) | 1 TiB | 1 TiB |

**The rows:**
- the request log holds four rows **under the child's id** (CreateBucket, PutObject, GetObject, the 404 ListObjects), each `host = origin`;
- `edge_admits`: 0 (the child has no `edge_ingest` flag, so the Worker passed it to the origin);
- the audit trail under tenant zero: `admin.platform_granted`, `platform.tenant_created`, `platform.key_created`, `platform.key_revoked`.

**Left in place:** tenant zero is a platform. The child, its bucket and its 3 MB object stay as the proof. The key was **revoked** at the end, and the secret file was removed with the script (no live credential outlives it).

## Open questions

1. **A platform key (D-STRIPE-19).** Should a platform's server call `/api/v1/platform/*` with an access key instead of a JWT? Either an exemption to the S3 gate for these routes (SigV4-verified by the handler, as `/whoami` is), or an exchange of a `PlatformAdmin` key for a short JWT.
2. **Residency (D-STRIPE-20).** Should a tenant have a residency of its own (a column), inherited by its children and enforced at bucket create? Today every bucket picks its own region.
3. **RB9-2.** A child's downloads: the pool's allowance, or a per-child allowance the platform sets?
