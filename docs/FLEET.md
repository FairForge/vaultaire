# The OneDrive fleet (`permafrost`) — operator page

*Written 2026-11-06 from the measurements of 2026-11-04/05 (`bench-results/VAULT-PARITY-LEGS-CF-2026-11-04.md`, `bench-results/PERMAFROST-FLEET-2026-11-05.md`). The driver's internals are `internal/drivers/onedrive_README.md`; the work packages are `docs/IMPLEMENTATION_PLAN.md` § P7. This page is the rules an operator needs, with the number behind each one.*

## What the fleet is

A set of Microsoft 365 OneDrive for Business drives, one per licensed user, in tenants Stored administers, reached by the `permafrost` driver through Microsoft Graph with one app registration per tenant (certificate or secret, app-only). Each drive is **5 TB** when the tenant's `OneDriveStorageQuota` default says so (a tenant left at the 1 TB default provisions 1 TB drives — two of three tenants were, until 2026-11-05). A plan whose only storage plan is OneDrive Lite gives 5 GB: the 5 TB seats are the ones carrying `SHAREPOINTENTERPRISE` (SharePoint Plan 2 — today the Dynamics 365 bundle; renewal mid-2028).

**What it is for:** the Vault parity leg (four Reed-Solomon shards per object, any four of eight rebuild it), the cold half of Standard, operator backups. **What it is never:** the only copy of anything (one company controls every tenant), a store for small customer files (shards and packs only), a hot tier.

## The limits, as measured (not as published)

| Dimension | Microsoft's page | What the fleet actually did (2026-11-05) | Rule |
|---|---|---|---|
| Streams per drive | 3,000 requests / 5 min per user | ~8 parallel streams or ~24 parallel upload sessions before 429s | **8 streams per drive**; 16 sessions per drive from the edge is the knee (171 MB/s), 24 collapses |
| Write volume per drive | 50 GB/h | 666 GB in 2 h (6.7×) with 9 of 111 rounds hit by a 429 burst | pace at the published rate; writes are not the problem |
| Read volume per drive | 100 GB/h | 666 GB in 2 h (3.3×); **a one-minute 429 burst about every five minutes**, 19 of 111 rounds lost most objects, the next round always clean | pace at the published rate as the good-tenant posture; the DRIVER must ride a burst (4+ attempts, full `Retry-After`, a hedge) — WP-FLEET-3 |
| Small writes per tenant | 1,250 RU/min (2 RU per upload) | 1,200 × 1 MiB PUTs on one drive at 33/s, zero 429s, no `RateLimit-*` header | small objects are latency-bound (~1 s each), not budget-bound; pack them anyway |
| Per app per tenant | 400 GB/h in and out | **two 5 TB drives in ONE tenant, 30 min, nothing else running: 12 of 21 rounds lost ≥ 20 of 48 reads and 7 of 21 rounds lost writes, against 5 of 30 and 0 of 30 for one drive per tenant at the same per-drive rate** (each drive ≈ 420 GB/h, so ≈ 840 GB/h on the tenant) | **the tenant budget is the one that bites first in a dense tenant**: pace per TENANT at 400 GB/h each way, spread a job's drives across tenants, never saturate one tenant's drives together |
| `RateLimit-*` headers | sent at ≥ 80 % of a budget | never seen in any run | back-pressure on them is defensive, not the primary control |
| Download URL | "may expire within minutes" | 206 at +60 min, 401 at +70 | fetch per read, never cache; a long stream re-fetches and resumes by `Range` |
| Request body at the edge | — | Cloudflare 413 at 256 MiB (the plan's 100 MB limit) | edge writes are whole bodies ≤ 100 MB; larger = one upload session per piece + a manifest |
| Upload session | fragments < 60 MiB, multiples of 320 KiB, sequential | the largest fragment (180 × 320 KiB) is fastest from a local body (~25 MB/s per session vs ~13 at 20 MiB); pipelining helps only to overlap a client's upload | one session = one object (or piece); parallelism comes from sessions, not fragments |
| Hashes | `quickXorHash` only on Business | returned on every simple PUT, on the final fragment, on every later GET; Go port with live vectors in `internal/drivers/quickxor.go` (#1169) | verify every write and every migration copy by it |
| Recycle bin | 93 days, counts against quota | 68 / 23 / 21 GiB of deleted shards on the fleet drives after one bench day | every driver delete is `permanentDelete` since #1168; the bin is `vaultaire_permafrost_quota_bytes{kind="deleted"}`; a one-time purge is the SharePoint `RecycleBin/DeleteAll` (operator script, admin app) |
| Drive selection | `/users/{upn}/drives` lists several; order arbitrary | two of three fleet users list a hidden `PersonalCacheLibrary` first | the driver picks the drive named `OneDrive`; `TENANT_N_DRIVE_ID` pins it; the marker records it (#1167) |

## Throughput to plan with

- **From the origin (SLC, the driver):** 6 drives × 48 streams = 826 MB/s PUT / 659 GET, 0 errors — the 10 Gbps box is the next ceiling. One object ≈ 20–25 MB/s (one session); aggregate comes from concurrency across objects.
- **From the edge (a Worker, no origin bytes):** reads 171 MB/s per object whole, ~300 MB/s as 8 ranges; writes ~25 MB/s per session, 171 MB/s per drive with 8 sessions.
- **As the Vault parity leg through Vaultaire** (lab, same binary as prod): protect 64 MiB 10 s / 256 MiB 15 s / 1 GiB 54 s — bound by Geyser's read-back (~20 MB/s per object), not by the fleet; parity-serve reads of 1 GiB 78–137 MB/s single stream (one straggler run in ~10); **four concurrent 1 GiB rebuilds: 62–77 MB/s aggregate on three drives, 298 MB/s on six** — rebuild capacity scales with drive count.
- **Driver-level, the fleet alone as the parity leg (erasure-bench, six drives):** protect 1 GiB in 12–15 s at 138–166 MB/s on the wire; a parity-only read of 1 GiB 97 MB/s, **124–131 MB/s hedged** (a slow shard range re-requested on a fresh connection — the straggler cure).
- **Beside the other legs (erasure-bench, six drives):** a full parity set on each of Geyser, Lyve and the fleet reads a 1 GiB object at 140–160 MB/s and still 115–204 with any one leg gone; a spread of shards across legs reads at 73–129 and needs every leg. Stack, never spread (the 2026-11-04 verdict, confirmed).
- **Rebuild at the edge:** a Worker decodes RS(4,4) at ~110 MB/s in plain JavaScript, stripe by stripe (a whole-object decode exceeds the 128 MiB isolate); shard reads are not the bottleneck.

## Provisioning a drive (the recipe that works)

The SharePoint bulk pre-provisioning request (`CreatePersonalSiteEnqueueBulk`, what `Request-SPOPersonalSite` does) answered 200 and created nothing in 8 hours. **A drive exists about one minute after the user's own first sign-in.** The unattended recipe (`fleet-admin` scripts on SLC; the tool is WP-FLEET-7):

1. Create the user (Graph, the tenant's admin app), `usageLocation` set, a random password nobody keeps.
2. Assign the 5 TB SKU with every plan but `SHAREPOINTENTERPRISE` + `SHAREPOINTWAC` disabled.
3. Mint a **Temporary Access Pass** (Graph, a Global Admin's DELEGATED token — the one step needing the admin's browser, once per tenant per session; the TAP policy must be enabled for all users).
4. Sign in once as the user with the pass (headless browser) and open `https://<initial>-my.sharepoint.com/`: the site is created; a password sign-in instead hits the Authenticator-registration wall and never gets there.
5. Confirm with SharePoint, not Graph: `PeopleManager/GetPropertiesFor` → `PersonalUrl` is `/personal/<alias>/`, and `/personal/<alias>/_api/site/usage` answers 200 (`StoragePercentageUsed` is a ratio: quota = `Storage` ÷ ratio). Graph `/users/{id}/drive` from an app without a Files role answers 404 for existing drives.
6. `accountEnabled = false` — the app's access is unaffected; the user has no sign-in surface.
7. Append the slot (`TENANT_N_*`, `TENANT_N_DRIVE_ID`); never reorder slots (placement is hash % fleet size; `PERMAFROST_FLEET_ACCEPT` blesses a new order).

Before any of it, per tenant: `OneDriveStorageQuota` = 5242880 (MB) via the tenant admin REST (`PATCH /_api/SPO.Tenant`, Int64 as a string), sharing internal-only, TAP policy on.

## Good-tenant posture

Consent to this use is the owner's premise and decision; the operator's part is to behave like a well-made client: one app registration per tenant and never more for the same job; pace at the published per-user and per-tenant rates even though the soaks took several times more; honour `Retry-After` fully and treat a 429 burst as the budget's signal, not a fault; decorate the `User-Agent`; keep ≥ 5 licensed users per tenant (the 5 TB entitlement) and every drive licensed (an unlicensed drive goes read-only at 60 days and archived at 93); no per-file customer keys (path ≤ 400, ~50k items per folder); shards, packs and generated names only.

## What a Vault write and read look like on the fleet, end to end

1. PUT of a GLACIER object → Geyser (tape) takes the bytes; the row records `geyser`.
2. The `vault_parity` job reads the object back from Geyser and writes four shards to the fleet (`<tenant>__parity/<digest>/<etag>/p0..3`), each on the account its name hashes to; the row is `complete` when all four landed. 1 GiB: ~54 s after the write.
3. A GET while the object is on tape, or while Geyser refuses, reads the four shards (any four of the eight data+parity shards rebuild) and serves the object with `x-vaultaire-served-from: parity`; with `vault_parity_serve` the parity copy is read FIRST. 1 GiB: 10–13 s at ~100 MB/s, first byte ~1–3 s.
4. A DELETE erases the shards (`permanentDelete`) and the row. Nothing waits in a bin.
5. Loss of a drive (unlicensed, suspended, deleted): the object is still on tape and on three other shards' drives; the job re-protects. Loss of every drive: the object is on tape, a recall away.

## Open at the time of writing

- WP-FLEET-1 built and on prod; slot 3 migrated; **owed:** slot 3 `-retire` after 24 h green, slot 1's migration, the owner's one-time bin purge of the fleet drives.
- WP-FLEET-3 (ride the bursts; a refused range must fail the body — the bench saw clean 0-byte bodies under throttling) is the next prompt: `docs/reviews/WP-FLEET-3-PROMPT.md`.
- The driver sees 15 slots and fans out to every account (WP-FLEET-11) — before any expansion past 15 drives.
- Unsealed Vault objects' shards are a readable copy under the app's credential: F-14 or a sealed-only leg rule before customer plaintext goes on the fleet.
