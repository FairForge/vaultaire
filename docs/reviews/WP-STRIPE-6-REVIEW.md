# RP88 — the review of WP-STRIPE-6 (WP-KEY-1: the management key, then the budgets)

## Part 1 — the pre-merge pass on Part 0 and PR A (2026-10-31)

*Read adversarially, before PR A merges:*
- *#1091 (Part 0, merged, on prod as `62269271` since 2026-10-30 21:48 UTC): RP86-1…5;*
- *#1093 (PR A, open, head `37d4e356`): migration 121, `internal/auth/management_keys.go`, `internal/api/management_keys.go`, `requireCaller` / `requirePerson`, the audit principal, the erasure rules, the export section, `KeysMadeBy`;*
- *against D-STRIPE-29, RP86's three corrections, `docs/ACCOUNTS.md` § Principal and the plan's WP-KEY-1 row;*
- *with a private migrated database (`vaultaire_test_rp88`, through 120).*

*Prod state 2026-10-31: no management key exists (the table is not on prod yet); PR A's CI is red on `internal/payments` only (RP88-0, below), the api and auth packages pass.*

### Verdict so far

**Part 0 holds.** The five fixes are the review's, each by its test:
- RP86-1: `ensureSystemBucket` reads the residency and takes `firstRegionInside` (none = `no_region_inside`, nothing written); `moveSystemBucketsInside` runs inside `setTenantResidency`'s transaction under `FOR UPDATE OF b`, moves an empty `_exports` row inside (or drops it when no region is inside), and a row with a live head row is still `errBucketsOutside`; `treeBucketRegions` leaves system buckets out.
- RP86-2: the label is `drivers.DataResidency(region)` only; another label is 409 `residency_is_the_region`; `null` re-asserts the region's.
- RP86-3: `BackfillBuckets` takes `idrive-<region>`'s region, else the default, and skips (Error line) a bucket outside its tenant's residency.
- RP86-4: `ReleaseIfMember` under the user's `FOR UPDATE` before stage a, and `lockedReleaseIfMember` again inside `EraseRows`' own lock (`ErrBecameMember` is an `ErrNotPending`).
- RP86-5: `deploy/monitoring/promtool-test.sh` (pinned, checksum-verified) in `ci.yml` after Lint.

**PR A holds at P4.** All ten claimed tests exist by name. The three RP86 corrections are in: `audit_logs.principal` written by `insert` (a key's row has the principal and no `performed_by`; a person's row has their id in both), `created_by` with no foreign key and the ByTenant delete + ByUser null, `requirePerson` on every member route, the accept, the export, the erasure and the management-key mint itself. What was checked and holds:

| Area | Checked | Holds |
|---|---|---|
| The gate | a Bearer `sk_…` is no AWS scheme (`auth.IsAWSAuthorization`), so the S3 gate never sees it; `requireCaller` falls to `requireJWT` for anything else; `/api/v1/user`, `/webhooks`, `/events`, `/sts` keep `requireJWT` (a key there is 401) | Yes |
| The role | `manage:write` → developer, `manage:read` → viewer on `/manage`; `platform` → admin on `/platform` of a root; `apps` → admin as the app; a key with no scope for the surface is 403 `scope_forbidden` at `requireMember`; `auth.Can` stays the one table (owner-only actions — export, erasure, billing — are out of a key's reach by the table alone, `requirePerson` is the second wall) | Yes |
| The actor | `LookupManagementKey`'s `COALESCE` of scalar subqueries: one owner member per tenant (`SetMemberRole` makes no second owner, `RemoveMember` refuses the owner), so no subquery can answer two rows | Yes |
| An app's key | `requirePlatform` passes `actsAsApp`; every platform handler reads its parent from `tenantIDKey` = the app, and `DescendantT1/T2` with the app as `$1` is the app's `managed` children only; `handlePlatformCreateTenant` as an app forces `kind = managed`, `parent_id` = the app, and calls `CreateChildTenant(platform, …, ParentID: app)` — depth 2 stays the deepest, the pool root stays the platform; an app's key cannot mint a management key for its children (`platformAppOrRefused` wants an `app`) nor for itself | Yes |
| The ceiling | `S3Ceiling.KeyScope` → `auth.WithKeyScope`; `handleMgmtCreateKey`, `handlePlatformCreateKey` (platform_keys.go:85) and `mintManagementKey` all pass through `CapScope`, which handles `*`; a nil ceiling = the account's authority (D-STRIPE-19's seam) | Yes |
| The erasure | ByTenant rule present (the schema walk demands one); ByUser nulls `created_by`; `operatorTrailScrub` nulls `principal`; `KeysMadeBy` UNION lists management keys with `kind`; `revoke_keys` revokes them | Yes |
| The secret | 32 random bytes in base62 (43 chars), sha256 hex stored, shown once, hint = 4 chars; lookup on every request, `last_used_at` once a minute | Yes |

### Findings

| ID | Sev | Where | What | Fix |
|---|---|---|---|---|
| **RP88-0** | **P4 (fixed in this PR)** | `internal/payments/admin.go` `addPeriod`; `pay3_test.go` | **Go's month arithmetic beside Postgres's.** `addPeriod` used `AddDate`, which normalises an overflow forward (Oct 31 + 1 month = Dec 1; Feb 29 + 1 year = Mar 1), while `Extend` and `PaidThroughAfter` add `interval '1 month'` in SQL, which clamps to the month's last day. A manual grant with no entitlement row made on the 29th–31st ran up to three days past the extension that followed it; and `TestManualPayment_GrantsWithoutRow_ThenExtends` asserted the Go rule against the SQL result, so every CI run on those days of the month was red (#1093's today, in a package it never touched). | `addPeriod` clamps the day to the target month (the Postgres rule); `TestAddPeriod_IsPostgresRule` compares eight dates against the live database; the flaky assertion uses `addPeriod`. |
| RP88-1 | P4 | `mintRequest`, `MintManagementKey` | **`sk_test_` cannot be minted.** The prefix follows the tenant's `sandbox_of`, but every mint route names the live tenant (`activeTenant`, or an app child), and no route mints on a sandbox; the PR body's "sk_live_/sk_test_" is half built. | `"test": true` on the mint body (as `handleMgmtCreateKey`'s): the key is minted ON the tenant's sandbox (`auth.SandboxOf`, made on first use as a test S3 key's is) and acts on it; a sandbox reset or purge deletes data, never keys (`purgeSandbox` touches objects, versions, locks and buckets only — keep it so, Stripe's test keys survive "delete all test data"). `TestManagementKey_TestKeyActsOnTheSandbox`. |
| RP88-2 | P4 | migration 121, `LookupManagementKey` | **No expiry.** `docs/ACCOUNTS.md` § Principal: every principal has "a scope, optional caps and an expiry", and WP-AGENT-1's agent grant is "a management key with … a lifetime (30 days by default, renewable)". The table has no `expires_at`, and 121 is the WP's one migration (D-STRIPE-43) — a later ALTER is a second migration on this table. | `expires_at TIMESTAMPTZ` in 121 (NULL = none, a server key's default); the lookup adds `AND (expires_at IS NULL OR expires_at > NOW())`; the mint body takes an optional `expires_at`; the list shows it. `TestManagementKey_ExpiredIsRefused`. |
| RP88-3 | P4 | `MintManagementKey` | The 25-live-keys count runs under the tenant row's `FOR SHARE`: two concurrent mints both read 24 and both insert. | `FOR UPDATE` on the tenant row (the mint is rare; the lock is cheap). |

### For PR B — two decisions the prompt left to the review

**D-RP88-1: the key's identity reaches the request.** The S3 verifier answers `(tenantID, *KeyScope)` and nothing else (`auth.ValidateRequest`, `auth.ValidateDescribed`); the S3 handlers, the edge admit and the egress meter never learn WHICH key acts — only what it may do. A per-key cap cannot be enforced, metered or named (`x-stored-cap: key`) without it. So `KeyScope` gains `KeyID` (the `api_keys.id`; an STS token: its parent key's), filled by the one lookup (`auth.Auth.LookupCredential`) on every scope it answers, the primary pair's included; a management key's ceiling (`S3Ceiling.KeyScope`) carries none (it is no S3 credential). F-39 (c) then falls out: the primary is never capped, so a nil cap on its row is the whole rule — no special case.

**D-RP88-2: the storage cap is bytes HELD, and 121 gets the columns now.** The prompt's step 6 says STOP unless `object_head_cache.written_by_key` exists — it does not (nothing in the schema names the writing key). The plan row says "storage cap" and "egress cap per UTC month": the storage cap is what the key's writes still hold, not an ingest budget, and that needs attribution. PR A is open, so migration 121 takes PR B's columns before it merges (D-STRIPE-43, one file per WP):
- `object_head_cache.written_by_key UUID`, `object_versions.written_by_key UUID`, `multipart_uploads.written_by_key UUID` — the `api_keys.id` of the credential that wrote the row (an STS token: its parent's; NULL = written before 121, or by the primary pair under F-39 (c), counted to no key). Written where the row is written: `commitBlobWrite`, the version write, `CreateMultipartUpload`; the edge admit's `edge_admits.attrs` carries it to the commit;
- `api_keys.storage_used_bytes BIGINT NOT NULL DEFAULT 0` — the bytes the key's rows hold, moved by the ONE ledger (`internal/usage`, RP82-3's rule: every writer of `storage_used_bytes` is there) in the same statement that moves the tenant's: + on a write that lands, − when a row the key wrote is displaced or deleted BY ANYONE (`displacedRow` knows the old row's key), the reservation of an UploadPart on the upload's key, released with it;
- `buckets.storage_used_bytes BIGINT` — NULL = not yet counted; seeded by SUM over the bucket's head rows and retained versions under the bucket's lock at the first cap set and by the daily reconcile, moved by the same ledger afterwards; the reconcile re-asserts both new counters and exports their drift beside `vaultaire_pool_used_drift_bytes`.

The check is then `fits` extended: min(key cap, bucket cap, tenant, pool) on the reservation, in `internal/usage`, and the refusal names the first cap that failed (`x-stored-cap: key|bucket|tenant|pool`). A bucket cap above the tenant's is accepted and ignored with a note (F-39 (b) as recommended). The egress cap per key reuses `egress_member.go`'s per-member bucket keyed by `KeyID` (the prompt's step 7); F-39 (a) — pace at the key, or refuse — is still the owner's; build the prompt's shape (pace, refuse past 2×) and keep the switch one constant.

### What stays for Part 2 (post-merge)

The budgets' enforcement on the three surfaces (S3, edge admit, STS narrowing), the dashboard pages, the prod proof, and the closeout note `docs/reviews/WP-STRIPE-6.md`. Part 2 is written after PR D merges.
