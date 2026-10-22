# K3 probe — who writes a bucket's mode today, who prints it, and what Standard is on prod

*Stage 1a Prompt 26, keystone K3 PR 1 (docs only), 2026-10-22. Feeds PR 2 (migration 102, `modes.json`, the one write door) and PR 3 (the readers). Contract: `docs/ARCHITECTURE_CONTRACT.md` line 3 ("one mode table, machine-read") and line 8 ("can't read is only ever client-side"); plan Phase 41.0.*

## The mapping K3 encodes

A bucket's mode is not stored today; it follows from two columns:

| `visibility` | `ssec_required` (096) | Mode |
|---|---|---|
| `public-read` | false | **Public** |
| `public-read` | true | refused: a public bucket cannot require a customer key (both writers below check it) |
| `private` | true | **Private** (every write carries an SSE-C key, WP-SSEC-1) |
| `private` | false | **Standard** |

**Name trap.** `visibility = 'private'` means *not public-read*, which is **Standard** mode, not Private. Every place below that prints "Private" for that value is printing the Standard bucket under the Private name. PR 3 replaces those labels with the `modes.json` names (behind `modes_copy`); until then nothing new may print "Private" for `visibility = 'private'`.

## Writers of `buckets.visibility` / `buckets.ssec_required`

| # | Where | What it writes | Notes |
|---|---|---|---|
| W1 | `internal/api/bucket_create.go:162` `createBucketRegistryWith` | INSERT `visibility = 'private'`, `ssec_required` default false | The one create path for S3 `CreateBucket`, the management API `POST /api/v1/buckets` and the dashboard (`dashboardBucketCreator`). No `x-amz-acl` honoured: every bucket is born Standard |
| W2 | `internal/auth/backfill.go:16` `BackfillBuckets` | INSERT `visibility = 'private'` for head-cache buckets with no row | Boot backfill; born Standard |
| W3 | `internal/api/account_export.go:456` `ensureSystemBucket` | INSERT `visibility = 'private'` for the `_exports` system bucket | Never listed; born Standard |
| W4 | `internal/dashboard/handlers/bucket_settings.go:193` (POST `/dashboard/buckets/{name}/settings`) | UPDATE `visibility` (`private` / `public-read`) **and** `ssec_required` with CORS, cache, tier | The only writer of `visibility = 'public-read'` (the "Visibility" toggle). Refuses public + ssec at `:172` |
| W5 | `internal/api/management_routes.go:327` (PATCH `/api/v1/buckets/{name}`) | UPDATE `ssec_required` (with `metadata`) | Refuses ssec on a public bucket at `:319` (409 `bucket_public`) |

No S3 path writes either column: `PUT ?acl` (`internal/api/s3_acl.go:175`) accepts only owner-equivalent ACLs and answers a `public-read` canned ACL or grant with 400 `AccessControlListNotSupported` (BucketOwnerEnforced); `?policy` is not implemented; no S3 call sets `ssec_required`. A bucket becomes Public only through the dashboard toggle (W4). Not counted as writers: the migrations (025 `DEFAULT 'private'`, 096 `DEFAULT FALSE`) and `account.EraseRows` (deletes rows).

**What PR 2 does with them:** W4 and W5 (the two UPDATEs, the only mode changes) collapse onto one function `bucketmode.Set(ctx, db, tenant, bucket, Change)`, which writes `visibility` / `ssec_required` and enforces the 096 rule (public + ssec refused) in one place, in one statement. W1–W3 are creates: they all write Standard and stay where they are, and a test pins that the only SQL writing `visibility` or `ssec_required` outside the door is these three INSERTs.

## Readers that print a mode, visibility or encryption label

| # | Where | Prints | Trap? |
|---|---|---|---|
| R1 | `internal/dashboard/templates/customer/buckets.html:48,60` | column "Visibility": badge **Public** for `public-read`, else **Private** | **yes** — a Standard bucket shows "Private" |
| R2 | `internal/dashboard/templates/customer/bucket_settings.html:68-88` | card "Visibility", toggle **Private** / **Public** | **yes** — the Standard choice is labelled "Private" |
| R3 | `internal/dashboard/templates/customer/bucket_settings.html:100-101` | "Require a customer-provided key (SSE-C) on every write" | no (describes the column; does not name a mode) |
| R4 | `internal/dashboard/templates/customer/compliance.html:52,62` | "Encryption" ✓/✗ from `buckets.sse_enabled` | not a mode, but **wrong on prod**: `sse_enabled` asks for SSE-S3, and with no master key a write that needs it is refused (`placement.go:133`); every prod bucket shows ✗ today, which is the truth |
| R5 | `internal/dashboard/templates/customer/billing.html:113` | "TLS in transit; SSE-C and client-side encryption" | no (true on prod) |
| R6 | `internal/api/management_routes.go` `mgmtBucket` (GET/PATCH `/api/v1/buckets[/{name}]`) | `ssec_required` only; no visibility, no mode | PR 3 adds `mode` (additive) |
| R7 | `internal/docs/openapi.go:910-923, 2005` | the `ssec_required` text of the bucket PATCH and schema | no mode words; PR 3 adds the generated `mode` enum + text. The spec documents the management API only: there is no `x-amz-server-side-encryption*` header text in it today, so PR 3 adds that text generated from `modes.json` |
| R8 | `internal/api/account_export_sections.go:398` | `visibility` in the GDPR export JSON | raw column value, not a label; PR 2 adds `mode` to the section (`TestExportSectionsCoverSchema`) |
| R9 | `internal/dashboard/templates/legal/privacy.html:37` | "We never read, analyze or profile the content … encrypted in transit (TLS) and treated as opaque data" | no mode table yet; PR 3 adds the generated per-mode table behind `modes_copy` (owner flips after reading — a legal page) |
| R10 | `legal/dpa.html:54-55`, `legal/gdpr.html:50,70`, `legal/baa.html:45-46` | "server-managed encryption at rest (SSE-S3) is implemented but not yet enabled in production"; SSE-C / client-side on request; sub-processors' own at-rest encryption | no — already the honest pre-key wording; PR 3 does not touch them (they are not generated; a change of them goes with the privacy policy, CLAUDE.md "change both together") |
| R11 | `internal/api/landing/landing.src.html:282,439-440,641` | restic "client-side encrypted … can't read a byte of it"; "TLS everywhere, SSE-C and client-side encryption" | no — client-side claims, true (contract line 8) |
| R12 | `internal/api/llms_txt.go:59` | "Sub-resources: … ?acl (public-read)" | **misleading** — `PUT ?acl` refuses `public-read` (R-row above); a bucket is made Public only from the dashboard. Fixed in PR 3 (the line names the modes from `modes.json` and says how a bucket becomes Public) |

No reader in the repository prints "zero-knowledge" or "zero knowledge" (grep of `*.go *.html *.md *.txt *.json *.js` under `internal/` and `cmd/`, 2026-10-22). "Vault" appears only as the tier (attic / tape), never as a mode name.

## The truth of Standard on prod today (read 2026-10-22 ~03:40 UTC, slot 8001, build `70a72d7`)

- **No master key is loaded.** `/opt/vaultaire/configs/.env` has no `ENCRYPTION_MASTER_KEY*` line, `/etc/credstore.encrypted/` is empty, and the slot's boot journal has no `master key: credential|file|env` line. So **SSE-S3 is off** and **"encrypted at rest with keys we hold" is FALSE on prod right now.**
- `tenant_key_versions`: **0 rows.** `object_head_cache`: **503 rows, every one with no `encryption_algorithm`** (501 on `idrive`, 2 on `blomp`; no chunked head rows). `global_content_index`: **34,234 chunk rows, none with an `encryption_key_version`** (stored unencrypted by us).
- `buckets`: **7 rows, all `visibility = 'private'`, `ssec_required = false`, `sse_enabled = false`** → all 7 are **Standard**; 0 Public, 0 Private.

So a Standard object on prod today is: **TLS in transit; at rest, only whatever the storage vendor applies on its own disks** (iDrive e2, Lyve, Blomp — the vendor's statement, not verified by us); **readable by us and by the vendor.** SSE-S3 under our keys applies only where a master key is loaded, which is nowhere yet. SSE-C (Private, per bucket or per request) works today: the key travels with each request and is never stored.

**Consequence for PR 2/3:** Standard's "keys we hold" sentence in `modes.json` carries `requires_master_key: true`, and every generated surface renders it only when the process has a master key loaded; otherwise it renders the pre-key sentence above. `TestPrivacyModesTable_NoMasterKey_NeverSaysKeysWeHold` pins it.

## Counts for the convergence row

`docs/STATUS.md` "Features without a declared mode": PR 3 replaces "not yet counted" with the number its test counts (the bucket-scoped features that render a mode, measured against `modes.json`).
